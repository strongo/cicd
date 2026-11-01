package go_ci_action

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoCIWorkflowCoverageCommandDispatch(t *testing.T) {
	const coverpkgExpression = "${{ inputs.coverpkg && format('-coverpkg={0}', inputs.coverpkg) || '' }}"
	const profile = "mode: set\nexample.com/module/file.go:1.1,2.2 1 1\n"
	script := goCIWorkflowRunBlock(t, "Run tests with coverage")
	if !strings.Contains(script, coverpkgExpression) {
		t.Fatal("default coverage invocation lost its coverpkg input")
	}
	for _, tc := range []struct {
		name, command, coverpkg, wantArgs string
		wantCode                          int
		wantProfile                       bool
	}{
		{name: "empty preserves default", wantArgs: "test\n./...\n-coverprofile=profile.cov\n-count=1\n", wantProfile: true},
		{name: "empty preserves coverpkg", coverpkg: "-coverpkg=./...", wantArgs: "test\n./...\n-coverprofile=profile.cov\n-count=1\n-coverpkg=./...\n", wantProfile: true},
		{name: "multiline custom owns flags and working directory", command: "test -f caller-module\nprintf '%s' \"$EXPECTED_PROFILE\" > profile.cov\nprintf '%s' 'literal $() and quotes' > custom-marker", coverpkg: "-coverpkg=./...", wantProfile: true},
		{name: "custom exit status", command: "exit 23", wantCode: 23},
		{name: "custom errexit", command: "false\nprintf should-not-run > custom-marker", wantCode: 1},
		{name: "custom pipefail", command: "false | true\nprintf should-not-run > custom-marker", wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, item := range map[string]struct {
				body string
				mode os.FileMode
			}{
				"caller-module": {body: "module fixture", mode: 0o600},
				"go":            {body: "#!/bin/bash\nprintf '%s\\n' \"$@\" > go-args\nprintf '%s' \"$EXPECTED_PROFILE\" > profile.cov\n", mode: 0o700},
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(item.body), item.mode); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "bash", "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", strings.ReplaceAll(script, coverpkgExpression, tc.coverpkg))
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "COVERAGE_COMMAND="+tc.command, "EXPECTED_PROFILE="+profile)
			output, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("command did not exit normally: %v\n%s", err, output)
				}
				code = exit.ExitCode()
			}
			if ctx.Err() != nil || code != tc.wantCode {
				t.Fatalf("exit=%d, want=%d, deadline=%v\n%s", code, tc.wantCode, ctx.Err(), output)
			}
			args, argsErr := os.ReadFile(filepath.Join(dir, "go-args"))
			if tc.wantArgs != "" {
				if argsErr != nil || string(args) != tc.wantArgs {
					t.Fatalf("default args=%q err=%v, want=%q", args, argsErr, tc.wantArgs)
				}
			} else if !os.IsNotExist(argsErr) {
				t.Fatalf("custom command invoked default go: %q, err=%v", args, argsErr)
			}
			gotProfile, profileErr := os.ReadFile(filepath.Join(dir, "profile.cov"))
			if tc.wantProfile {
				if profileErr != nil || string(gotProfile) != profile {
					t.Fatalf("profile=%q err=%v", gotProfile, profileErr)
				}
			} else if !os.IsNotExist(profileErr) {
				t.Fatalf("failed command wrote a profile: %q err=%v", gotProfile, profileErr)
			}
			marker, markerErr := os.ReadFile(filepath.Join(dir, "custom-marker"))
			if tc.wantCode != 0 && !os.IsNotExist(markerErr) {
				t.Fatalf("failed custom command continued: %q err=%v", marker, markerErr)
			}
			if tc.wantCode == 0 && tc.command != "" && (markerErr != nil || string(marker) != "literal $() and quotes") {
				t.Fatalf("multiline command corrupted: %q err=%v", marker, markerErr)
			}
		})
	}
}

func TestGoCIWorkflowCoverageCommandWiring(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	for _, required := range []string{
		"coverage_command:\n        type: string\n        required: false\n        default: ''",
		"CI_POLICY_COVERAGE_COMMAND: ${{ inputs.coverage_command }}",
		"COVERAGE_COMMAND: ${{ inputs.coverage_command }}",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("missing coverage command contract: %s", required)
		}
	}
	if strings.Count(workflow, "inputs.coverage_command") != 2 {
		t.Fatal("coverage command must be limited to policy binding and the existing coverage step")
	}
}
