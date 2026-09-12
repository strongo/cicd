package go_ci_action

import (
	"strings"
	"testing"
)

func TestGoCIWorkflowConventionalPullRequestTitleIsOptIn(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	inputStart := strings.Index(workflow, "\n      require_conventional_pr_title:\n")
	if inputStart == -1 {
		t.Fatal("require_conventional_pr_title input is missing")
	}
	inputEnd := strings.Index(workflow[inputStart:], "\n      build_command:")
	if inputEnd == -1 {
		t.Fatal("title-validation input must be declared before build_command")
	}
	input := workflow[inputStart : inputStart+inputEnd]
	if !strings.Contains(input, "type: boolean") || !strings.Contains(input, "default: false") {
		t.Fatal("conventional title validation must remain opt-in for migration safety")
	}

	for _, required := range []string{
		"CI_POLICY_REQUIRE_CONVENTIONAL_PR_TITLE: ${{ inputs.require_conventional_pr_title }}",
		"if: ${{ inputs.require_conventional_pr_title && github.event_name == 'pull_request' }}",
		"PR_TITLE: ${{ github.event.pull_request.title }}",
		"PR_NUMBER: ${{ github.event.pull_request.number }}",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("workflow is missing title-validation contract %q", required)
		}
	}

	lint := workflowJob(t, workflow, "go_lint", "go_test_build")
	checkout := strings.Index(lint, "name: Checkout code")
	validation := strings.Index(lint, "name: Validate conventional pull request title")
	install := strings.Index(lint, "name: Install Go")
	if checkout == -1 || validation <= checkout || install <= validation {
		t.Fatal("title validation must run in the required Lint job before toolchain setup")
	}
}

func TestGoCIWorkflowAcceptsConventionalPullRequestTitles(t *testing.T) {
	script := namedWorkflowRunBlock(t, readGoCIWorkflow(t), "Validate conventional pull request title")
	for _, title := range []string{
		"fix: preserve ACL blockers",
		"feat(auth): add principal bindings",
		"feat(auth)!: replace legacy identifiers",
		"perf(api/v2): reduce allocation count",
		"docs: x",
		"chore(deps): update module",
	} {
		t.Run(title, func(t *testing.T) {
			output, err := runBash(t.TempDir(), script, titleValidationEnvironment(title))
			if err != nil {
				t.Fatalf("valid title failed: %v\n%s", err, output)
			}
		})
	}
}

func TestGoCIWorkflowRejectsInvalidPullRequestTitlesWithActionableHelp(t *testing.T) {
	script := namedWorkflowRunBlock(t, readGoCIWorkflow(t), "Validate conventional pull request title")
	for _, title := range []string{
		"Integrate layered ACL queries",
		"Fix: preserve ACL blockers",
		"fix:no separating space",
		"fix:  leading description space",
		"fix(scope..child): malformed scope",
		"unknown: unsupported type",
		"fix: trailing space ",
		"fix: ",
	} {
		t.Run(title, func(t *testing.T) {
			output, err := runBash(t.TempDir(), script, titleValidationEnvironment(title))
			if err == nil {
				t.Fatalf("invalid title passed:\n%s", output)
			}
			message := string(output)
			for _, required := range []string{
				"Invalid pull request title",
				"Expected format:",
				"<type>(<optional-scope>)<optional-!>: <description>",
				"Allowed types:",
				"fix(cli): preserve ACL blockers",
				"gh pr edit 42 --repo example/project --title",
				"default_bump=false",
				"A squash merge uses the pull request title as the commit subject",
			} {
				if !strings.Contains(message, required) {
					t.Errorf("failure output is missing %q:\n%s", required, message)
				}
			}
		})
	}
}

func titleValidationEnvironment(title string) map[string]string {
	return map[string]string{
		"PR_TITLE":          title,
		"PR_NUMBER":         "42",
		"GITHUB_REPOSITORY": "example/project",
		"DEFAULT_BUMP":      "false",
	}
}
