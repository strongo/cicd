package go_ci_action

import (
	"strings"
	"testing"
)

// The go_lint job checks forbidden imports once dependencies (and private
// module credentials) are in place and before golangci-lint; nothing runs
// when nothing is forbidden; the tool is built on its own terms and run
// against the caller's module.
func TestGoLintChecksForbiddenImports(t *testing.T) {
	job := workflowJob(t, readGoCIWorkflow(t), "go_lint", "go_test_build")
	download := strings.Index(job, "      - name: Download dependencies\n")
	checkout := strings.Index(job, "      - name: Checkout exact forbidden-imports tool\n")
	check := strings.Index(job, "      - name: Check forbidden imports\n")
	lint := strings.Index(job, "      - name: golangci-lint\n")
	if download < 0 || checkout < 0 || check < 0 || lint < 0 || download >= checkout || checkout >= check || check >= lint {
		t.Fatalf("steps out of order: download=%d checkout=%d check=%d lint=%d", download, checkout, check, lint)
	}
	steps := job[checkout:lint]
	for _, want := range []string{
		"ref: ${{ job.workflow_sha }}",
		"persist-credentials: false",
		"CI_FORBIDDEN_IMPORTS: ${{ inputs.forbidden_imports }}",
		"CI_ALLOWED_IMPORTS: ${{ inputs.allowed_imports }}",
		"CI_MODULE_DIR: ${{ github.workspace }}/${{ inputs.working_directory }}",
		`GOWORK=off GOTOOLCHAIN=auto go build -o "$tool" ./cmd/forbidden-imports`,
	} {
		if !strings.Contains(steps, want) {
			t.Errorf("forbidden-imports steps lack %q", want)
		}
	}
	if got := strings.Count(steps, "if: ${{ inputs.forbidden_imports != '' }}"); got != 2 {
		t.Errorf("%d of 2 forbidden-imports steps are skipped when nothing is forbidden", got)
	}
	// The build's environment is its own, and the built tool is what runs.
	script := workflowStepRun(t, steps[check-checkout:])
	var lines []string
	for _, line := range strings.Split(script, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if strings.Contains(script, "export GOWORK") || strings.Contains(script, "export GOTOOLCHAIN") {
		t.Error("GOWORK or GOTOOLCHAIN is exported: it would reach the caller's go list")
	}
	buildIdx := strings.Index(script, `GOWORK=off GOTOOLCHAIN=auto go build -o "$tool" ./cmd/forbidden-imports`)
	cdIdx := strings.Index(script, `cd "$GITHUB_WORKSPACE"`)
	rmIdx := strings.Index(script, `rm -rf "${GITHUB_WORKSPACE}/.strongo-cicd-forbidden-imports-tool"`)
	runIdx := strings.LastIndex(script, `"$tool"`)
	if buildIdx < 0 || cdIdx < 0 || rmIdx < 0 || runIdx < 0 || buildIdx >= cdIdx || cdIdx >= rmIdx || rmIdx >= runIdx {
		t.Errorf("tool checkout is not cleaned up after build and before running tool: %q", script)
	}
	if len(lines) == 0 || lines[len(lines)-1] != `"$tool"` {
		t.Errorf("the built tool is not what the step runs last: %q", lines)
	}
}

// The fleet-wide default forbids the in-memory DALgo test database.
func TestForbiddenImportsDefaultsToDalgo2memory(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	start := strings.Index(workflow, "      forbidden_imports:\n")
	end := strings.Index(workflow, "      allowed_imports:\n")
	if start < 0 || end < start {
		t.Fatal("forbidden_imports input missing")
	}
	if !strings.Contains(workflow[start:end], "        default: 'github.com/dal-go/dalgo/adapters/dalgo2memory'\n") {
		t.Error("forbidden_imports no longer defaults to dalgo2memory")
	}
}
