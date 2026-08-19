package go_ci_action

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goCIWorkflowPath = ".github/workflows/workflow.yml"

// The gate's total-coverage extraction used to be `go tool cover -func=profile.cov
// | grep total | awk '{print $3}'`. Any function whose name merely contains
// "total" (e.g. a helper named totalsByParticipant) produces a second line
// matching `grep total`, so `$3` reads the wrong value and the build fails, or
// passes, for the wrong reason — this repository hit it once already. The
// extraction is now anchored on column 1 (`awk '$1 == "total:"'`), which only
// `go tool cover -func`'s single summary row satisfies. This test exercises the
// exact pipeline embedded in the workflow's "Check test coverage" step against a
// synthetic `go tool cover -func` transcript, shadowing `go` so no real
// coverage profile is required.
func TestGoCIWorkflowCoverageGateIgnoresFunctionsNamedLikeTotal(t *testing.T) {
	script := goCIWorkflowRunBlock(t, "Check test coverage")
	extraction := scriptAssignmentLine(t, script, "total_coverage=")

	for _, tc := range []struct {
		name       string
		transcript string
		want       string
	}{
		{
			name: "ordinary profile with no naming collision",
			transcript: strings.Join([]string{
				"github.com/x/y/foo.go:10:\tDoThing\t\t100.0%",
				"total:\t\t\t\t\t(statements)\t\t78.5%",
			}, "\n") + "\n",
			want: "78.5",
		},
		{
			name: "a function named like total no longer confuses the extraction",
			transcript: strings.Join([]string{
				"github.com/x/y/foo.go:10:\tDoThing\t\t100.0%",
				"github.com/x/y/foo.go:20:\ttotalsByParticipant\t\t0.0%",
				"total:\t\t\t\t\t(statements)\t\t78.5%",
			}, "\n") + "\n",
			want: "78.5",
		},
		{
			name: "a file path containing the substring total no longer confuses the extraction",
			transcript: strings.Join([]string{
				"github.com/x/y/totalizer.go:10:\tHelper\t\t50.0%",
				"total:\t\t\t\t\t(statements)\t\t78.5%",
			}, "\n") + "\n",
			want: "78.5",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			transcriptPath := filepath.Join(dir, "cover-func.txt")
			if err := os.WriteFile(transcriptPath, []byte(tc.transcript), 0o644); err != nil {
				t.Fatal(err)
			}

			// Shadow `go` so `go tool cover -func=profile.cov` returns the
			// synthetic transcript instead of requiring a real coverage profile.
			program := "go() {\n" +
				"  if [ \"$1\" = tool ] && [ \"$2\" = cover ]; then cat \"$COVER_FUNC_TRANSCRIPT\"; fi\n" +
				"}\n" +
				extraction + "\n" +
				"printf '%s' \"$total_coverage\"\n"

			output, err := runBash(dir, program, map[string]string{"COVER_FUNC_TRANSCRIPT": transcriptPath})
			if err != nil {
				t.Fatalf("extraction script: %v\n%s", err, output)
			}
			if string(output) != tc.want {
				t.Fatalf("total_coverage = %q, want %q", output, tc.want)
			}
		})
	}
}

func scriptAssignmentLine(t *testing.T, script, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return line
		}
	}
	t.Fatalf("script has no line starting with %q:\n%s", prefix, script)
	return ""
}

func readGoCIWorkflow(t *testing.T) string {
	t.Helper()
	workflow, err := os.ReadFile(goCIWorkflowPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(workflow)
}

// goCIWorkflowRunBlock finds a step by its `name:` field rather than by a
// leading `- name: ` sequence: unlike release.yml's steps, workflow.yml's
// coverage steps lead with `- if: ...` and carry `name:` as a later field at
// 8-space indent, not as the first token of the step.
func goCIWorkflowRunBlock(t *testing.T, stepName string) string {
	t.Helper()
	workflow := readGoCIWorkflow(t)
	needle := "\n        name: " + stepName + "\n"
	start := strings.Index(workflow, needle)
	if start == -1 {
		t.Fatalf("go CI workflow has no %q step", stepName)
	}
	return yamlRunBlock(t, workflow[start+1:])
}
