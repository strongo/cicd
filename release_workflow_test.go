package go_ci_action

import (
	"fmt"
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

func TestReleaseWorkflowWatchdogsReturnPromptlyAndKillTimeouts(t *testing.T) {
	for i, watchdog := range releaseWorkflowTimeoutHelpers(t) {
		t.Run(fmt.Sprintf("watchdog_%d", i+1), func(t *testing.T) {
			workspace := t.TempDir()
			program := strings.Join([]string{
				"set +e",
				watchdog,
				`success_output="$(run_with_timeout 3 bash -c 'printf ready')"`,
				"success_status=$?",
				`failure_output="$(run_with_timeout 3 bash -c 'printf broken; exit 23')"`,
				"failure_status=$?",
				`printf 'success=%s:%s failure=%s:%s\n' "$success_status" "$success_output" "$failure_status" "$failure_output"`,
			}, "\n")
			started := time.Now()
			output, err := runBash(workspace, program, map[string]string{"RUNNER_TEMP": workspace})
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("immediate commands: %v\n%s", err, output)
			}
			if elapsed >= 1500*time.Millisecond {
				t.Fatalf("immediate commands waited %s for their 3s watchdogs\n%s", elapsed, output)
			}
			if string(output) != "success=0:ready failure=23:broken\n" {
				t.Fatalf("unexpected immediate command result: %q", output)
			}

			timeoutProgram := strings.Join([]string{
				"set +e",
				watchdog,
				`run_with_timeout 1 bash -c 'trap "" TERM; while :; do sleep 0.05; done'`,
				"status=$?",
				`printf 'status=%s\n' "$status"`,
			}, "\n")
			started = time.Now()
			output, err = runBash(workspace, timeoutProgram, map[string]string{"RUNNER_TEMP": workspace})
			elapsed = time.Since(started)
			if err != nil {
				t.Fatalf("timed command: %v\n%s", err, output)
			}
			if elapsed < time.Second || elapsed >= 5*time.Second {
				t.Fatalf("timed command completed in %s, want timeout plus TERM/KILL grace\n%s", elapsed, output)
			}
			if !strings.HasPrefix(string(output), "status=") || string(output) == "status=0\n" {
				t.Fatalf("timed command must be killed, got %q", output)
			}
			matches, err := filepath.Glob(filepath.Join(workspace, ".*_timed_out"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("timeout marker = %v, %v; want one marker", matches, err)
			}
		})
	}
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
	const endMarker = "          }\n"
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
