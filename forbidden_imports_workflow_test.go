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
	if download < 0 || checkout < 0 || check < 0 || lint < 0 || !(download < checkout && checkout < check && check < lint) {
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
}
