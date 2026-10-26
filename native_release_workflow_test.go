package go_ci_action

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the actual workflow shell, including the run-identity selector and
// dispatch/download failures. A green unrelated run must never unlock publishing.
func TestNativeReleaseBuildGate(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const request = "native-release-123-1"
	run := func(title, commit, event, status, conclusion string) string {
		return fmt.Sprintf(`{"id":42,"display_title":%q,"head_sha":%q,"event":%q,"status":%q,"conclusion":%q}`,
			title, commit, event, status, conclusion)
	}
	green := run(request, sha, "workflow_dispatch", "completed", "success")
	for _, tc := range []struct {
		name       string
		runs       string
		gitSHA     string
		fail       string
		workflow   string
		timeout    string
		want       string
		wantOK     bool
		wantLaunch bool
	}{
		{name: "exact successful run", runs: green, want: "succeeded at exact commit", wantOK: true, wantLaunch: true},
		{name: "failed native matrix", runs: run(request, sha, "workflow_dispatch", "completed", "failure"), want: "concluded failure", wantLaunch: true},
		{name: "cancelled native matrix", runs: run(request, sha, "workflow_dispatch", "completed", "cancelled"), want: "concluded cancelled", wantLaunch: true},
		{name: "unrelated successful run", runs: run("other-request", sha, "workflow_dispatch", "completed", "success"), want: "timed out", wantLaunch: true},
		{name: "wrong commit", runs: run(request, "other-commit", "workflow_dispatch", "completed", "success"), want: "identity mismatch", wantLaunch: true},
		{name: "wrong event", runs: run(request, sha, "push", "completed", "success"), want: "identity mismatch", wantLaunch: true},
		{name: "ambiguous request", runs: green + "," + green, want: "Ambiguous", wantLaunch: true},
		{name: "queued run times out", runs: run(request, sha, "workflow_dispatch", "queued", ""), want: "timed out", wantLaunch: true},
		{name: "missing run times out", want: "timed out", wantLaunch: true},
		{name: "tag points elsewhere", gitSHA: "different", want: "tag does not point"},
		{name: "dispatch permission denied", runs: green, fail: "dispatch", want: "dispatch denied", wantLaunch: true},
		{name: "API error fails closed", runs: green, fail: "api", want: "api denied", wantLaunch: true},
		{name: "artifact missing fails closed", runs: green, fail: "download", want: "download denied", wantLaunch: true},
		{name: "invalid workflow", workflow: "../../unsafe.yml", want: "workflow filename"},
		{name: "invalid timeout", timeout: "0", want: "positive integer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeExecutable(t, filepath.Join(dir, "gh"), `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$LOG"
case "$1 $2" in
  'workflow run') if [ "$FAIL" = dispatch ]; then echo 'dispatch denied'; exit 1; fi ;;
  'api '*) if [ "$FAIL" = api ]; then echo 'api denied' >&2; exit 1; fi; cat "$FIXTURE" ;;
  'run download') if [ "$FAIL" = download ]; then echo 'download denied'; exit 1; fi ;;
  *) echo "unexpected gh invocation $*"; exit 2 ;;
esac
`)
			writeExecutable(t, filepath.Join(dir, "git"), "#!/usr/bin/env bash\nprintf '%s\\n' \"$GIT_SHA\"\n")
			fixture := filepath.Join(dir, "runs.json")
			if err := os.WriteFile(fixture, []byte(`{"workflow_runs":[`+tc.runs+`]}`), 0o600); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "gh.log")
			gitSHA := tc.gitSHA
			if gitSHA == "" {
				gitSHA = sha
			}
			workflow := tc.workflow
			if workflow == "" {
				workflow = "native-release.yml"
			}
			timeout := tc.timeout
			if timeout == "" {
				timeout = "1"
			}
			script := releaseWorkflowRunBlock(t, "Require native release builds at the exact tag")
			script = strings.Replace(script, "POLL_SECONDS=10", "POLL_SECONDS=0.01", 1)
			out, err := runBash(dir, script, map[string]string{
				"PATH": dir + ":" + os.Getenv("PATH"), "GH_TOKEN": "test-token", "REPO": "acme/widget",
				"SHA": sha, "TAG": "v1.2.3", "REQUEST": request, "WORKFLOW": workflow, "TIMEOUT": timeout,
				"GIT_SHA": gitSHA, "FAIL": tc.fail, "LOG": log, "FIXTURE": fixture,
			})
			if (err == nil) != tc.wantOK || !strings.Contains(string(out), tc.want) {
				t.Fatalf("error = %v, output = %s; want success %v and %q", err, out, tc.wantOK, tc.want)
			}
			calls, readErr := os.ReadFile(log)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatal(readErr)
			}
			launched := strings.Contains(string(calls), "workflow run native-release.yml --repo acme/widget --ref v1.2.3 -f release_request="+request)
			if launched != tc.wantLaunch {
				t.Fatalf("launched %v, want %v; calls: %s", launched, tc.wantLaunch, calls)
			}
			downloaded := strings.Contains(string(calls), "run download")
			if downloaded != (tc.wantOK || tc.fail == "download") {
				t.Fatalf("unexpected download state %v: %s", downloaded, calls)
			}
			if tc.wantOK && !strings.Contains(string(calls), "run download 42 --repo acme/widget --pattern native-release-"+request+"-* --dir native-release") {
				t.Fatalf("download must bind artifact pattern and exact run: %s", calls)
			}
		})
	}
}

func TestNativeReleaseBuildRunsBeforePublishers(t *testing.T) {
	workflow := readReleaseWorkflow(t)
	gate := strings.Index(workflow, "      - name: Require native release builds at the exact tag")
	for _, name := range []string{"Capture pre-release publisher repo SHAs", "Run GoReleaser"} {
		if gate < 0 || gate >= strings.Index(workflow, "      - name: "+name) {
			t.Fatalf("native build gate must precede %s", name)
		}
	}
	if !strings.Contains(workflow, "if: ${{ inputs.native_build_workflow != '' && (github.ref_type == 'tag' || steps.tag.outputs.new_tag != '') }}") {
		t.Fatal("native builds must be opt-in and only dispatch when a release tag exists")
	}
}
