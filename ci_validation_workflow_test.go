package go_ci_action

import (
	"strings"
	"testing"
)

func TestGoCIWorkflowExactTreeReuseIsOptInAndFailClosed(t *testing.T) {
	workflow := readGoCIWorkflow(t)

	for _, required := range []string{
		"reuse_exact_tree_validation:\n        type: boolean",
		"or ambiguous receipts fall back to the existing full validation path",
		"CI_POLICY_EXACT_TREE_VALIDATION_REUSE: ${{ inputs.reuse_exact_tree_validation }}",
		"if: ${{ inputs.reuse_exact_tree_validation && github.event_name == 'push' && github.ref == 'refs/heads/main' }}",
		"actions: read\n      contents: read\n      pull-requests: read",
		"timeout-minutes: 3\n    # The optimization itself is never a new gate",
		"continue-on-error: true",
		"repository: ${{ job.workflow_repository }}",
		"ref: ${{ job.workflow_sha }}",
		"CI_RECEIPT_WORKFLOW_REVISION: ${{ job.workflow_sha }}",
		"CI_RECEIPT_EXPECTED_CHECKOUT_SHA: ${{ github.sha }}",
		"github.event.pull_request.head.repo.full_name == github.repository",
		"needs.go_test_build.result == 'success' && needs.go_lint.result == 'success'",
		"name: ci-validation-receipt",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("workflow is missing exact-tree reuse contract %q", required)
		}
	}

	inputStart := strings.Index(workflow, "\n      reuse_exact_tree_validation:\n")
	if inputStart == -1 {
		t.Fatal("reuse input is missing")
	}
	inputEnd := strings.Index(workflow[inputStart:], "\n      validation_receipt_retention_days:")
	if inputEnd == -1 || !strings.Contains(workflow[inputStart:inputStart+inputEnd], "default: false") {
		t.Fatal("exact-tree validation reuse must remain disabled by default")
	}

	lint := workflowJob(t, workflow, "go_lint", "go_test_build")
	if !strings.Contains(lint, "needs: [ validation_reuse ]") || !strings.Contains(lint, "if: ${{ !cancelled() && needs.validation_reuse.outputs.reuse_valid != 'true' }}") {
		t.Fatal("lint must run unless the preflight explicitly verifies reuse")
	}
	if strings.Contains(lint, "needs.validation_reuse.result == 'success'") {
		t.Fatal("a failed or skipped receipt resolver must not suppress full lint")
	}
	build := workflowJob(t, workflow, "go_test_build", "publish_validation_receipt")
	if !strings.Contains(build, "if: ${{ !cancelled() }}") {
		t.Fatal("build job must still run after exact-tree validation reuse")
	}
	if strings.Count(build, "needs.validation_reuse.outputs.reuse_valid != 'true'") != 6 {
		t.Fatal("every test and coverage step must be guarded by the verified reuse output")
	}
	buildStep := workflowStep(t, build, "Build", "Set up Java JRE 24")
	if strings.Contains(buildStep, "reuse_valid") {
		t.Fatal("exact-tree reuse must never skip the caller's main-SHA build command")
	}
	uploadStep := workflowStep(t, build, "Upload build artifact", "Save Go dependencies and build cache")
	if strings.Contains(uploadStep, "reuse_valid") {
		t.Fatal("exact-tree reuse must never skip the deployable artifact upload")
	}

	bump := workflowJob(t, workflow, "go_bump", "")
	for _, required := range []string{
		"needs: [ validation_reuse, go_test_build, go_lint ]",
		"needs.go_test_build.result == 'success'",
		"needs.go_lint.result == 'skipped' && needs.validation_reuse.outputs.reuse_valid == 'true'",
	} {
		if !strings.Contains(bump, required) {
			t.Fatalf("version bump continuity is missing %q", required)
		}
	}
}

func TestGoCIWorkflowPolicyDigestCoversEveryWorkflowCallInput(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	policyStart := strings.Index(workflow, "\nenv:\n  CI_POLICY_")
	jobsStart := strings.Index(workflow, "\njobs:\n")
	if policyStart == -1 || jobsStart == -1 || policyStart >= jobsStart {
		t.Fatal("normalized validation-policy environment is missing")
	}
	policy := workflow[policyStart:jobsStart]
	inputsStart := strings.Index(workflow, "    inputs:\n")
	if inputsStart == -1 {
		t.Fatal("workflow_call inputs are missing")
	}
	inputsBlock := workflow[inputsStart+len("    inputs:\n") : policyStart]
	var names []string
	for _, line := range strings.Split(inputsBlock, "\n") {
		if strings.HasPrefix(line, "      ") && !strings.HasPrefix(line, "        ") && strings.HasSuffix(line, ":") {
			names = append(names, strings.TrimSuffix(strings.TrimSpace(line), ":"))
		}
	}
	for _, name := range names {
		if !strings.Contains(policy, "${{ inputs."+name+" }}") {
			t.Errorf("workflow input %q is absent from the normalized policy digest", name)
		}
	}
}

func workflowJob(t *testing.T, workflow, name, next string) string {
	t.Helper()
	startToken := "\n  " + name + ":\n"
	start := strings.Index(workflow, startToken)
	if start == -1 {
		t.Fatalf("workflow job %q is missing", name)
	}
	if next == "" {
		return workflow[start:]
	}
	end := strings.Index(workflow[start+len(startToken):], "\n  "+next+":\n")
	if end == -1 {
		t.Fatalf("workflow job %q has no following job %q", name, next)
	}
	return workflow[start : start+len(startToken)+end]
}

func workflowStep(t *testing.T, job, name, next string) string {
	t.Helper()
	start := strings.Index(job, "      - name: "+name+"\n")
	if start == -1 {
		t.Fatalf("job has no step %q", name)
	}
	end := strings.Index(job[start+1:], "name: "+next+"\n")
	if end == -1 {
		t.Fatalf("step %q has no following step %q", name, next)
	}
	return job[start : start+1+end]
}
