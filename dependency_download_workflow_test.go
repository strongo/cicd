package go_ci_action

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func dependencyDownloadScript(t *testing.T, workflow, job, next string) string {
	t.Helper()
	section := workflowJob(t, workflow, job, next)
	start := strings.Index(section, "      - name: Download dependencies\n")
	if start < 0 {
		t.Fatal("download step missing")
	}
	step := section[start:]
	if end := strings.Index(step[1:], "\n      - "); end >= 0 {
		step = step[:end+1]
	}
	if strings.Contains(step, "        run: |\n") {
		return workflowStepRun(t, step)
	}
	for _, line := range strings.Split(step, "\n") {
		if command, ok := strings.CutPrefix(line, "        run: "); ok {
			return command
		}
	}
	t.Fatal("download command missing")
	return ""
}

func TestDependencyDownloadScopesMaintenanceWithoutChangingCredentials(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	for _, job := range []struct{ name, next string }{{"go_lint", "go_test_build"}, {"go_test_build", "publish_validation_receipt"}} {
		t.Run(job.name, func(t *testing.T) {
			for _, inheritedCount := range []string{"", "2", "0002"} {
				inherited := inheritedCount != ""
				for _, failure := range []bool{false, true} {
					t.Run(strings.Join([]string{map[bool]string{false: "empty", true: "inherited"}[inherited], map[bool]string{false: "success", true: "failure"}[failure]}, "/"), func(t *testing.T) {
						dir := t.TempDir()
						config := filepath.Join(dir, "credentials.gitconfig")
						original := "[credential]\n\thelper = exact-global-helper\n[maintenance]\n\tauto = true\n[gc]\n\tauto = 6700\n"
						if err := os.WriteFile(config, []byte(original), 0o600); err != nil {
							t.Fatal(err)
						}
						fakeGo := `#!/bin/bash
set -euo pipefail
cd "$ISOLATED_DIRECTORY"
[[ "$*" == 'mod download all' ]] || exit 90
[[ "$(git config --get maintenance.auto)" == false ]] || exit 91
[[ "$(git config --get gc.auto)" == 0 ]] || exit 92
[[ "$(git config --global --get credential.helper)" == exact-global-helper ]] || exit 93
[[ "$(git config --get credential.helper)" == "$EXPECTED_HELPER" ]] || exit 94
[[ "$GIT_CONFIG_COUNT" == "$EXPECTED_COUNT" ]] || exit 95
printf 'download invoked\n'
exit "$DOWNLOAD_EXIT"
`
						if err := os.WriteFile(filepath.Join(dir, "go"), []byte(fakeGo), 0o700); err != nil {
							t.Fatal(err)
						}
						env := map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "ISOLATED_DIRECTORY": dir, "GIT_CONFIG_GLOBAL": config, "EXPECTED_HELPER": "exact-global-helper", "EXPECTED_COUNT": "2", "DOWNLOAD_EXIT": "0"}
						if inherited {
							env["GIT_CONFIG_COUNT"] = inheritedCount
							env["GIT_CONFIG_KEY_0"], env["GIT_CONFIG_VALUE_0"] = "credential.helper", "exact-inherited-helper"
							env["GIT_CONFIG_KEY_1"], env["GIT_CONFIG_VALUE_1"] = "maintenance.auto", "true"
							env["EXPECTED_HELPER"], env["EXPECTED_COUNT"] = "exact-inherited-helper", "4"
						}
						if failure {
							env["DOWNLOAD_EXIT"] = "37"
						}
						script := dependencyDownloadScript(t, workflow, job.name, job.next)
						if !failure {
							// The command's appended pairs must not escape into its shell.
							script += "\n[[ ${GIT_CONFIG_COUNT:-} == '" + env["GIT_CONFIG_COUNT"] + "' ]]\n"
						}
						output, err := runWorkflowBash(script, env)
						if failure {
							var exit *exec.ExitError
							if !errors.As(err, &exit) || exit.ExitCode() != 37 {
								t.Fatalf("download error not preserved: %v\n%s", err, output)
							}
						} else if err != nil {
							t.Fatalf("download failed: %v\n%s", err, output)
						}
						if strings.Count(string(output), "download invoked") != 1 {
							t.Fatalf("download not invoked exactly once: %s", output)
						}
						after, err := os.ReadFile(config)
						if err != nil || string(after) != original {
							t.Fatalf("credential configuration changed: %v", err)
						}
					})
				}
			}
		})
	}
}

func TestDependencyDownloadRejectsMalformedInheritedConfigCount(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	for _, job := range []struct{ name, next string }{{"go_lint", "go_test_build"}, {"go_test_build", "publish_validation_receipt"}} {
		t.Run(job.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "go"), []byte("#!/bin/bash\nprintf 'unexpected download'\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			for _, count := range []string{"not-a-count", "18446744073709551616", "2147483646", "2147483647", strings.Repeat("9", 100)} {
				env := map[string]string{"PATH": dir + string(os.PathListSeparator) + os.Getenv("PATH"), "GIT_CONFIG_COUNT": count}
				output, err := runWorkflowBash(dependencyDownloadScript(t, workflow, job.name, job.next), env)
				if err == nil || strings.Contains(string(output), "unexpected download") || !strings.Contains(string(output), "Invalid inherited Git runtime configuration count") {
					t.Fatalf("invalid configuration %q reached download: %v\n%s", count, err, output)
				}
			}
		})
	}
}
