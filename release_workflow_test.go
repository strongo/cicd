package go_ci_action

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const releaseWorkflowPath = ".github/workflows/release.yml"

func TestReleaseWorkflowExtractsAnAbsoluteRunnableBinaryPath(t *testing.T) {
	extractScript := releaseWorkflowRunBlock(t, "Extract published archive")

	for _, tc := range []struct {
		name        string
		archivePath string
	}{
		{name: "bare filename", archivePath: "specscore"},
		{name: "nested filename", archivePath: "bin/specscore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			payloadDir := filepath.Join(workDir, "payload")
			binaryInArchive := filepath.Join(payloadDir, tc.archivePath)
			if err := os.MkdirAll(filepath.Dir(binaryInArchive), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binaryInArchive, []byte("#!/usr/bin/env bash\nprintf 'specscore 0.33.0\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			archive := filepath.Join(workDir, "specscore_linux_amd64.tar.gz")
			tar := exec.Command("tar", "-C", payloadDir, "-czf", archive, tc.archivePath)
			if output, err := tar.CombinedOutput(); err != nil {
				t.Fatalf("create archive: %v\n%s", err, output)
			}

			extractDir := filepath.Join(workDir, "extract")
			if err := os.MkdirAll(extractDir, 0o755); err != nil {
				t.Fatal(err)
			}
			archiveDestination := filepath.Join(extractDir, filepath.Base(archive))
			if err := os.Rename(archive, archiveDestination); err != nil {
				t.Fatal(err)
			}
			outputFile := filepath.Join(workDir, "github-output")
			output, err := runBash(extractDir, extractScript, map[string]string{
				"BINARY":        "specscore",
				"GITHUB_OUTPUT": outputFile,
			})
			if err != nil {
				t.Fatalf("extract script: %v\n%s", err, output)
			}

			binPath := githubOutput(t, outputFile, "bin_path")
			if !filepath.IsAbs(binPath) {
				t.Fatalf("bin_path must be absolute, got %q", binPath)
			}
			wantPath, err := filepath.EvalSymlinks(filepath.Join(extractDir, tc.archivePath))
			if err != nil {
				t.Fatal(err)
			}
			if binPath != wantPath {
				t.Fatalf("bin_path = %q, want %q", binPath, wantPath)
			}
			version := exec.Command(binPath, "--version")
			if output, err := version.CombinedOutput(); err != nil {
				t.Fatalf("execute extracted binary: %v\n%s", err, output)
			} else if string(output) != "specscore 0.33.0\n" {
				t.Fatalf("binary output = %q", output)
			}
		})
	}
}

func TestReleaseWorkflowHistoricalArtifactModeSelectsOnlyTheExactRequestedTag(t *testing.T) {
	validateScript := releaseWorkflowRunBlock(t, "Validate existing artifact tag and release-mode inputs")
	resolveTagScript := releaseWorkflowRunBlock(t, "Determine the artifact tag to verify")

	for _, tc := range []struct {
		name       string
		env        map[string]string
		wantStatus int
		wantTag    string
	}{
		{
			name: "valid plain semver tag",
			env: map[string]string{
				"EXISTING_ARTIFACT_TAG":    "v0.33.0",
				"TAG_PREFIX":               "v",
				"DEFAULT_BUMP":             "false",
				"ALLOW_MAJOR_VERSION_BUMP": "false",
				"GORELEASER_EXTRA_ARGS":    "",
				"GITHUB_REF_TYPE":          "branch",
			},
			wantStatus: 0,
			wantTag:    "v0.33.0",
		},
		{
			name: "scoped prefix",
			env: map[string]string{
				"EXISTING_ARTIFACT_TAG":    "ingitdb/v0.33.0",
				"TAG_PREFIX":               "ingitdb/v",
				"DEFAULT_BUMP":             "false",
				"ALLOW_MAJOR_VERSION_BUMP": "false",
				"GORELEASER_EXTRA_ARGS":    "",
				"GITHUB_REF_TYPE":          "branch",
			},
			wantStatus: 0,
			wantTag:    "ingitdb/v0.33.0",
		},
		{
			name: "rejects invalid semver",
			env: map[string]string{
				"EXISTING_ARTIFACT_TAG":    "v0.33",
				"TAG_PREFIX":               "v",
				"DEFAULT_BUMP":             "false",
				"ALLOW_MAJOR_VERSION_BUMP": "false",
				"GORELEASER_EXTRA_ARGS":    "",
				"GITHUB_REF_TYPE":          "branch",
			},
			wantStatus: 1,
		},
		{
			name: "rejects semver component with leading zero",
			env: map[string]string{
				"EXISTING_ARTIFACT_TAG":    "v01.2.3",
				"TAG_PREFIX":               "v",
				"DEFAULT_BUMP":             "false",
				"ALLOW_MAJOR_VERSION_BUMP": "false",
				"GORELEASER_EXTRA_ARGS":    "",
				"GITHUB_REF_TYPE":          "branch",
			},
			wantStatus: 1,
		},
		{
			name: "rejects release mode conflict",
			env: map[string]string{
				"EXISTING_ARTIFACT_TAG":    "v0.33.0",
				"TAG_PREFIX":               "v",
				"DEFAULT_BUMP":             "patch",
				"ALLOW_MAJOR_VERSION_BUMP": "false",
				"GORELEASER_EXTRA_ARGS":    "",
				"GITHUB_REF_TYPE":          "branch",
			},
			wantStatus: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "github-output")
			env := maps.Clone(tc.env)
			env["GITHUB_OUTPUT"] = outputFile
			env["ARTIFACT_SMOKE_TEST"] = "true"
			env["ARTIFACT_SMOKE_TEST_PLATFORMS"] = `[{"runner":"ubuntu-latest","goos":"linux","goarch":"amd64"}]`
			output, err := runBash(t.TempDir(), validateScript, env)
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("validate historical mode: %v\n%s", err, output)
				}
				if got := githubOutput(t, outputFile, "existing_artifact_tag"); got != tc.wantTag {
					t.Fatalf("validated tag = %q, want %q", got, tc.wantTag)
				}
				return
			}
			if err == nil {
				t.Fatalf("invalid historical mode succeeded:\n%s", output)
			}
		})
	}

	outputFile := filepath.Join(t.TempDir(), "github-output")
	output, err := runBash(t.TempDir(), resolveTagScript, map[string]string{
		"EXISTING_ARTIFACT_TAG": "v0.33.0",
		"RELEASE_TAG":           "v99.99.99",
		"GITHUB_REF_NAME":       "main",
		"GITHUB_OUTPUT":         outputFile,
	})
	if err != nil {
		t.Fatalf("resolve exact historical tag: %v\n%s", err, output)
	}
	if got := githubOutput(t, outputFile, "tag"); got != "v0.33.0" {
		t.Fatalf("resolver fell back from requested historical tag: got %q, want %q", got, "v0.33.0")
	}
}

func TestReleaseWorkflowHistoricalArtifactCannotPassWithoutExecutingAnArtifact(t *testing.T) {
	downloadScript := localWorkflowScript(releaseWorkflowRunBlock(t, "Download published release asset"))
	extractScript := localWorkflowScript(releaseWorkflowRunBlock(t, "Extract published archive"))

	t.Run("missing GitHub release or archive", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBin := filepath.Join(workspace, "fake-bin")
		if err := os.MkdirAll(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(fakeBin, "gh"), "#!/usr/bin/env bash\nexit 1\n")

		baseEnv := map[string]string{
			"PATH":              fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":       workspace,
			"GITHUB_REPOSITORY": "specscore/specscore-cli",
			"TAG":               "v0.33.0",
		}
		assertHistoricalFailureAndNormalSkip(t, workspace, downloadScript, baseEnv, "asset_found")
	})

	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "downloaded set has no recognized archive",
			prepare: func(t *testing.T, workspace string) {
				if err := os.WriteFile(filepath.Join(workspace, "checksums.txt"), []byte("sidecar"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "archive extraction fails",
			prepare: func(t *testing.T, workspace string) {
				if err := os.WriteFile(filepath.Join(workspace, "specscore_linux_amd64.tar.gz"), []byte("not an archive"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "archive has no resolved binary",
			prepare: func(t *testing.T, workspace string) {
				readme := filepath.Join(workspace, "README.md")
				if err := os.WriteFile(readme, []byte("no executable here"), 0o644); err != nil {
					t.Fatal(err)
				}
				archive := exec.Command("tar", "-C", workspace, "-czf", filepath.Join(workspace, "specscore_linux_amd64.tar.gz"), "README.md")
				if output, err := archive.CombinedOutput(); err != nil {
					t.Fatalf("create archive without binary: %v\n%s", err, output)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			tc.prepare(t, workspace)
			assertHistoricalFailureAndNormalSkip(t, workspace, extractScript, map[string]string{
				"BINARY": "specscore",
			}, "extracted")
		})
	}
}

func TestReleaseWorkflowSuccessfulHistoricalValidationInvokesExecutable(t *testing.T) {
	workspace := t.TempDir()
	payloadDir := filepath.Join(workspace, "payload")
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invokedFile := filepath.Join(workspace, "invoked")
	writeExecutable(t, filepath.Join(payloadDir, "specscore"), "#!/usr/bin/env bash\nprintf invoked > \"$INVOKED_FILE\"\nprintf '0.33.0\\n'\n")
	archivePath := filepath.Join(workspace, "specscore_linux_amd64.tar.gz")
	archive := exec.Command("tar", "-C", payloadDir, "-czf", archivePath, "specscore")
	if output, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("create valid archive: %v\n%s", err, output)
	}

	extractOutput := filepath.Join(workspace, "extract-output")
	extractScript := localWorkflowScript(releaseWorkflowRunBlock(t, "Extract published archive"))
	if output, err := runBash(workspace, extractScript, map[string]string{
		"BINARY":                         "specscore",
		"GITHUB_OUTPUT":                  extractOutput,
		"HISTORICAL_ARTIFACT_VALIDATION": "true",
	}); err != nil {
		t.Fatalf("extract valid historical artifact: %v\n%s", err, output)
	}

	runScript := localWorkflowScript(releaseWorkflowRunBlock(t, "'Layer 1: run the published binary (must exit within timeout)'"))
	output, err := runBash(workspace, runScript, map[string]string{
		"BIN_PATH":        githubOutput(t, extractOutput, "bin_path"),
		"SMOKE_CMD":       "--version",
		"TIMEOUT_SECONDS": "2",
		"INVOKED_FILE":    invokedFile,
	})
	if err != nil {
		t.Fatalf("run valid historical artifact: %v\n%s", err, output)
	}
	if _, err := os.Stat(invokedFile); err != nil {
		t.Fatalf("successful historical validation did not invoke the executable: %v\n%s", err, output)
	}
}

func assertHistoricalFailureAndNormalSkip(t *testing.T, workspace, script string, baseEnv map[string]string, outputKey string) {
	t.Helper()
	for _, tc := range []struct {
		name       string
		historical string
		wantError  bool
	}{
		{name: "historical mode fails", historical: "true", wantError: true},
		{name: "normal release mode keeps warning skip", historical: "false", wantError: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputFile := filepath.Join(workspace, strings.ReplaceAll(tc.name, " ", "-")+"-output")
			env := maps.Clone(baseEnv)
			env["GITHUB_OUTPUT"] = outputFile
			env["HISTORICAL_ARTIFACT_VALIDATION"] = tc.historical
			output, err := runBash(workspace, script, env)
			if tc.wantError {
				if err == nil {
					t.Fatalf("historical could-not-test path passed:\n%s", output)
				}
				return
			}
			if err != nil {
				t.Fatalf("normal release could-not-test path no longer soft-skips: %v\n%s", err, output)
			}
			if got := githubOutput(t, outputFile, outputKey); got != "false" {
				t.Fatalf("normal release %s = %q, want false", outputKey, got)
			}
		})
	}
}

func localWorkflowScript(script string) string {
	replacer := strings.NewReplacer(
		"${{ matrix.goos }}", "linux",
		"${{ matrix.goarch }}", "amd64",
	)
	return replacer.Replace(script)
}

func TestReleaseWorkflowHistoricalArtifactModeSkipsPublishingAndHomebrew(t *testing.T) {
	workflow := readReleaseWorkflow(t)
	for _, want := range []string{
		"if: ${{ needs.validate_release_mode.result == 'success' && inputs.existing_artifact_tag == '' }}",
		"${{ always() && !cancelled() &&",
		"inputs.existing_artifact_tag != '' ||",
		"inputs.existing_artifact_tag == '' &&",
		"ref: ${{ steps.release_tag.outputs.tag }}",
	} {
		if !strings.Contains(workflow, want) {
			t.Fatalf("workflow does not preserve historical artifact safety contract %q", want)
		}
	}
}

func TestReleaseWorkflowWatchdogsReturnPromptlyAndKillTimeouts(t *testing.T) {
	for i, watchdog := range releaseWorkflowTimeoutHelpers(t) {
		t.Run(fmt.Sprintf("watchdog_%d", i+1), func(t *testing.T) {
			workspace := t.TempDir()
			watchdogPIDs := watchdogProcessHarness(t, workspace)
			for attempt := 1; attempt <= 3; attempt++ {
				runImmediateCommand(t, workspace, watchdog, watchdogPIDs, "printf ready", 0, "ready")
				runImmediateCommand(t, workspace, watchdog, watchdogPIDs, "printf broken; exit 23", 23, "broken")
			}
			runParentExitWithBackgroundChild(t, workspace, watchdog, watchdogPIDs)

			timeoutProgram := strings.Join([]string{
				"set +e",
				watchdog,
				`run_with_timeout 1 bash -c 'trap "" TERM; (trap "" TERM; while :; do :; done) & printf '%s\n' "$!" > "$CHILD_PID_FILE"; while :; do :; done'`,
				"status=$?",
				`printf 'status=%s\n' "$status"`,
			}, "\n")
			started := time.Now()
			output, err := runBash(workspace, timeoutProgram, watchdogPIDs.environment())
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("timed command: %v\n%s", err, output)
			}
			if elapsed < time.Second || elapsed >= 5*time.Second {
				t.Fatalf("timed command completed in %s, want timeout plus TERM/KILL grace\n%s", elapsed, output)
			}
			if !strings.HasPrefix(string(output), "status=") || string(output) == "status=0\n" {
				t.Fatalf("timed command must be killed, got %q", output)
			}
			childPID := readPID(t, watchdogPIDs.childPIDFile)
			t.Cleanup(func() { killProcess(childPID) })
			if processIsAlive(childPID) {
				t.Fatalf("timed command's child process %d survived the process-group kill", childPID)
			}
			matches, err := filepath.Glob(filepath.Join(workspace, ".*_timed_out"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("timeout marker = %v, %v; want one marker", matches, err)
			}
		})
	}
}

func runParentExitWithBackgroundChild(t *testing.T, workspace, watchdog string, harness watchdogHarness) {
	t.Helper()
	if err := os.Remove(harness.readyFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(harness.childPIDFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	program := strings.Join([]string{
		"set +e",
		watchdog,
		`output="$(run_with_timeout 2 bash -c '(while :; do :; done) >/dev/null 2>&1 & printf "%s\n" "$!" > "$CHILD_PID_FILE"; while [ ! -f "$CICD_WATCHDOG_READY_FILE" ]; do :; done; printf leader-exited')"`,
		"status=$?",
		`printf 'status=%s output=%s\n' "$status" "$output"`,
	}, "\n")
	started := time.Now()
	output, err := runBash(workspace, program, harness.environment())
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("leader-exit command: %v\n%s", err, output)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("leader-exit command waited %s\n%s", elapsed, output)
	}
	if string(output) != "status=0 output=leader-exited\n" {
		t.Fatalf("leader-exit result = %q", output)
	}
	childPID := readPID(t, harness.childPIDFile)
	t.Cleanup(func() { killProcess(childPID) })
	if processIsAlive(childPID) {
		t.Fatalf("child process %d survived after its leader exited", childPID)
	}
	harness.assertAllExited(t)
}

func runImmediateCommand(t *testing.T, workspace, watchdog string, harness watchdogHarness, command string, wantStatus int, wantOutput string) {
	t.Helper()
	if err := os.Remove(harness.readyFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	program := strings.Join([]string{
		"set +e",
		watchdog,
		`output="$(run_with_timeout 2 bash -c 'while [ ! -f "$CICD_WATCHDOG_READY_FILE" ]; do :; done; exec bash -c "$CICD_IMMEDIATE_COMMAND"')"`,
		"status=$?",
		`printf 'status=%s output=%s\n' "$status" "$output"`,
	}, "\n")
	env := harness.environment()
	env["CICD_IMMEDIATE_COMMAND"] = command
	started := time.Now()
	output, err := runBash(workspace, program, env)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("immediate command: %v\n%s", err, output)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("immediate command waited %s for its 2s watchdog\n%s", elapsed, output)
	}
	want := fmt.Sprintf("status=%d output=%s\n", wantStatus, wantOutput)
	if string(output) != want {
		t.Fatalf("immediate command result = %q, want %q", output, want)
	}
	harness.assertAllExited(t)
}

type watchdogHarness struct {
	path          string
	pythonPIDFile string
	sleepPIDFile  string
	childPIDFile  string
	readyFile     string
	realPython    string
}

func watchdogProcessHarness(t *testing.T, workspace string) watchdogHarness {
	t.Helper()
	realPython, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	harness := watchdogHarness{
		path:          filepath.Join(workspace, "fake-bin"),
		pythonPIDFile: filepath.Join(workspace, "python-pids"),
		sleepPIDFile:  filepath.Join(workspace, "sleep-pids"),
		childPIDFile:  filepath.Join(workspace, "child-pid"),
		readyFile:     filepath.Join(workspace, "watchdog-ready"),
		realPython:    realPython,
	}
	if err := os.MkdirAll(harness.path, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(harness.path, "python3"), "#!/usr/bin/env bash\nprintf '%s\\n' \"$$\" >> \"$CICD_PYTHON_PID_FILE\"\ncase \"$*\" in *time.sleep*) touch \"$CICD_WATCHDOG_READY_FILE\" ;; esac\nexec \"$CICD_REAL_PYTHON\" \"$@\"\n")
	writeExecutable(t, filepath.Join(harness.path, "sleep"), "#!/usr/bin/env bash\nprintf '%s\\n' \"$$\" >> \"$CICD_SLEEP_PID_FILE\"\ntouch \"$CICD_WATCHDOG_READY_FILE\"\nexec /bin/sleep \"$@\"\n")
	return harness
}

func (h watchdogHarness) environment() map[string]string {
	return map[string]string{
		"RUNNER_TEMP":              filepath.Dir(h.path),
		"PATH":                     h.path + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CICD_REAL_PYTHON":         h.realPython,
		"CICD_PYTHON_PID_FILE":     h.pythonPIDFile,
		"CICD_SLEEP_PID_FILE":      h.sleepPIDFile,
		"CICD_WATCHDOG_READY_FILE": h.readyFile,
		"CHILD_PID_FILE":           h.childPIDFile,
	}
}

func (h watchdogHarness) assertAllExited(t *testing.T) {
	t.Helper()
	for _, pidFile := range []string{h.pythonPIDFile, h.sleepPIDFile} {
		for _, pid := range readPIDs(t, pidFile) {
			t.Cleanup(func() { killProcess(pid) })
			if processIsAlive(pid) {
				t.Fatalf("watchdog process %d from %s survived a completed command", pid, filepath.Base(pidFile))
			}
		}
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, text := range strings.Fields(string(content)) {
		var pid int
		if _, err := fmt.Sscanf(text, "%d", &pid); err != nil {
			t.Fatalf("parse PID %q from %s: %v", text, path, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	pids := readPIDs(t, path)
	if len(pids) != 1 {
		t.Fatalf("PIDs in %s = %v, want one", path, pids)
	}
	return pids[0]
}

func processIsAlive(pid int) bool {
	if output, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output(); err == nil && strings.HasPrefix(strings.TrimSpace(string(output)), "Z") {
		return false
	}
	return exec.Command("kill", "-0", fmt.Sprint(pid)).Run() == nil
}

func killProcess(pid int) {
	_ = exec.Command("kill", "-KILL", fmt.Sprint(pid)).Run()
}

func releaseWorkflowRunBlock(t *testing.T, stepName string) string {
	t.Helper()
	workflow := readReleaseWorkflow(t)
	needle := "      - name: " + stepName
	start := strings.Index(workflow, needle)
	if start == -1 {
		t.Fatalf("release workflow has no %q step", stepName)
	}
	return yamlRunBlock(t, workflow[start:])
}

func releaseWorkflowTimeoutHelpers(t *testing.T) []string {
	t.Helper()
	workflow := readReleaseWorkflow(t)
	const startMarker = "          run_with_timeout() {\n"
	const endMarker = "\n          }\n"
	var helpers []string
	for remaining := workflow; ; {
		start := strings.Index(remaining, startMarker)
		if start == -1 {
			break
		}
		remaining = remaining[start:]
		end := strings.Index(remaining, endMarker)
		if end == -1 {
			t.Fatal("unterminated run_with_timeout helper")
		}
		helper := remaining[:end+len(endMarker)]
		helper = strings.ReplaceAll(helper, "          ", "")
		helpers = append(helpers, helper)
		remaining = remaining[end+len(endMarker):]
	}
	if len(helpers) != 4 {
		t.Fatalf("found %d run_with_timeout helpers, want 4", len(helpers))
	}
	return helpers
}

func yamlRunBlock(t *testing.T, text string) string {
	t.Helper()
	const marker = "        run: |\n"
	start := strings.Index(text, marker)
	if start == -1 {
		t.Fatal("step has no bash run block")
	}
	text = text[start+len(marker):]
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "          "))
	}
	return strings.Join(lines, "\n")
}

func readReleaseWorkflow(t *testing.T) string {
	t.Helper()
	workflow, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(workflow)
}

func runBash(dir, program string, variables map[string]string) ([]byte, error) {
	command := exec.Command("bash", "-c", program)
	command.Dir = dir
	command.Env = os.Environ()
	for key, value := range variables {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.CombinedOutput()
}

func githubOutput(t *testing.T, outputFile, key string) string {
	t.Helper()
	output, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return value
		}
	}
	t.Fatalf("%s missing from GITHUB_OUTPUT:\n%s", key, output)
	return ""
}
