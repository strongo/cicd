package go_ci_action

import (
	"os"
	"os/exec"
	"path/filepath"
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
	resolver := workflowJob(t, workflow, "validation_reuse", "go_lint")
	if strings.Contains(resolver, "\n    permissions:\n") {
		t.Fatal("resolver must inherit caller permissions so default-off callers do not fail workflow startup")
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

func TestGoCIWorkflowMintsRepositoryScopedGitHubAppTokenPerJob(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	for _, jobName := range []string{"go_lint", "go_test_build"} {
		next := "go_test_build"
		if jobName == "go_test_build" {
			next = "publish_validation_receipt"
		}
		job := workflowJob(t, workflow, jobName, next)
		for _, required := range []string{
			"name: Validate scoped GitHub App configuration",
			"PRIVATE_GIT_APP_PRIVATE_KEY: ${{ secrets.GOPRIVATE_GITHUB_APP_PRIVATE_KEY }}",
			"Incomplete GitHub App private-module configuration",
			"At least one syntactically valid repository is required before a token is minted",
			"uses: actions/create-github-app-token@v3",
			"client-id: ${{ inputs.goprivate_github_app_client_id }}",
			"private-key: ${{ secrets.GOPRIVATE_GITHUB_APP_PRIVATE_KEY }}",
			"owner: ${{ inputs.goprivate_github_app_owner }}",
			"repositories: ${{ steps.goprivate_app_config.outputs.repositories }}",
			"permission-contents: read",
			"PRIVATE_GIT_APP_TOKEN: ${{ steps.goprivate_app_token.outputs.token }}",
			"PRIVATE_GIT_APP_REPOSITORIES: ${{ steps.goprivate_app_config.outputs.repositories }}",
			"must be an unqualified repository name",
			"\"$PRIVATE_GIT_APP_OWNER/$repository\" \"$PRIVATE_GIT_APP_OWNER/$repository.git\"",
			"git credential-store --file \"$credential_file\" store",
			"credential.https://github.com.useHttpPath true",
			"if: ${{ inputs.GOPRIVATE }}",
			"PRIVATE_GIT_APP_ENABLED: ${{ inputs.goprivate_github_app_client_id }}",
			"git config --global --add credential.helper \"$helper_file\"",
		} {
			if !strings.Contains(job, required) {
				t.Errorf("%s is missing scoped GitHub App dependency contract %q", jobName, required)
			}
		}
		preflight := workflowStep(t, job, "Validate scoped GitHub App configuration", "Mint scoped GitHub App token for GOPRIVATE")
		if !strings.Contains(preflight, "repositories+=(\"$repository\")") || !strings.Contains(preflight, ">> \"$GITHUB_OUTPUT\"") {
			t.Errorf("%s does not normalize and validate repositories before token minting", jobName)
		}
		if strings.Contains(job, "insteadOf") {
			t.Errorf("%s must not use prefix URL rewriting for private-module credentials", jobName)
		}
	}
	if strings.Count(workflow, "uses: actions/create-github-app-token@v3") != 2 {
		t.Fatal("each parallel dependency-consuming job must mint its own short-lived token")
	}
}

func TestGoCIWorkflowRejectsEmptyNormalizedGitHubAppRepositoriesBeforeMint(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	job := workflowJob(t, workflow, "go_lint", "go_test_build")
	preflight := workflowStepRun(t, workflowStep(t, job, "Validate scoped GitHub App configuration", "Mint scoped GitHub App token for GOPRIVATE"))

	for _, repositories := range []string{",", "  , \n\t,  "} {
		t.Run(strings.ReplaceAll(repositories, "\n", "newline"), func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "github-output")
			_, err := runWorkflowBash(preflight, map[string]string{
				"GITHUB_OUTPUT":                output,
				"PRIVATE_GIT_APP_CLIENT_ID":    "client-id",
				"PRIVATE_GIT_APP_OWNER":        "acme",
				"PRIVATE_GIT_APP_PRIVATE_KEY":  "private-key",
				"PRIVATE_GIT_APP_REPOSITORIES": repositories,
			})
			if err == nil {
				t.Fatal("repository list with no normalized entries passed preflight")
			}
			if data, readErr := os.ReadFile(output); readErr == nil && len(data) != 0 {
				t.Fatalf("failed preflight published repositories output: %q", data)
			}
		})
	}
}

func TestGoCIWorkflowNormalizesGitHubAppRepositoriesToSingleLine(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	job := workflowJob(t, workflow, "go_lint", "go_test_build")
	preflight := workflowStepRun(t, workflowStep(t, job, "Validate scoped GitHub App configuration", "Mint scoped GitHub App token for GOPRIVATE"))
	output := filepath.Join(t.TempDir(), "github-output")
	if combined, err := runWorkflowBash(preflight, map[string]string{
		"GITHUB_OUTPUT":                output,
		"PRIVATE_GIT_APP_CLIENT_ID":    "client-id",
		"PRIVATE_GIT_APP_OWNER":        "acme",
		"PRIVATE_GIT_APP_PRIVATE_KEY":  "private-key",
		"PRIVATE_GIT_APP_REPOSITORIES": " repo-one,\n WB_REPOSITORIES ",
	}); err != nil {
		t.Fatalf("valid repository preflight failed: %v\n%s", err, combined)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), "repositories=repo-one,WB_REPOSITORIES\n"; got != want {
		t.Fatalf("normalized repositories output = %q, want %q", got, want)
	}
}

func TestGoCIWorkflowRoutesExactAppAndLegacyCredentials(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	job := workflowJob(t, workflow, "go_lint", "go_test_build")
	appConfig := workflowStepRun(t, workflowStep(t, job, "Set scoped GitHub App access for GOPRIVATE", "Set GitHub access token for GOPRIVATE"))
	legacyConfig := workflowStepRun(t, workflowStep(t, job, "Set GitHub access token for GOPRIVATE", "Download dependencies"))
	tempDir := t.TempDir()
	githubEnv := filepath.Join(tempDir, "github-env")
	common := map[string]string{
		"GITHUB_ENV":  githubEnv,
		"GITHUB_JOB":  "credential-test",
		"RUNNER_TEMP": tempDir,
	}
	appEnv := cloneEnvironment(common)
	appEnv["PRIVATE_GIT_APP_TOKEN"] = "APP_TOKEN"
	appEnv["PRIVATE_GIT_APP_OWNER"] = "acme"
	appEnv["PRIVATE_GIT_APP_REPOSITORIES"] = "repo"
	if combined, err := runWorkflowBash(appConfig, appEnv); err != nil {
		t.Fatalf("App credential setup failed: %v\n%s", err, combined)
	}

	assertCredentialPassword(t, tempDir, "credential-test", "acme/repo", "APP_TOKEN")
	assertCredentialPassword(t, tempDir, "credential-test", "acme/repo.git", "APP_TOKEN")
	assertNoCredential(t, tempDir, "credential-test", "acme/repo-tools")

	legacyEnv := cloneEnvironment(common)
	legacyEnv["PRIVATE_GIT_TOKEN"] = "LEGACY_TOKEN"
	legacyEnv["PRIVATE_GIT_HOST"] = "github.com"
	legacyEnv["PRIVATE_GIT_HOSTS"] = "github.com/acme"
	legacyEnv["PRIVATE_GIT_APP_ENABLED"] = "client-id"
	if combined, err := runWorkflowBash(legacyConfig, legacyEnv); err != nil {
		t.Fatalf("legacy credential setup failed: %v\n%s", err, combined)
	}

	assertCredentialPassword(t, tempDir, "credential-test", "acme/repo", "APP_TOKEN")
	assertCredentialPassword(t, tempDir, "credential-test", "acme/repo.git", "APP_TOKEN")
	assertCredentialPassword(t, tempDir, "credential-test", "acme/repo-tools", "LEGACY_TOKEN")
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

func workflowStepRun(t *testing.T, step string) string {
	t.Helper()
	const marker = "        run: |\n"
	start := strings.Index(step, marker)
	if start == -1 {
		t.Fatal("workflow step has no multiline run script")
	}
	var lines []string
	for _, line := range strings.Split(step[start+len(marker):], "\n") {
		if line == "" {
			lines = append(lines, line)
			continue
		}
		if !strings.HasPrefix(line, "          ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "          "))
	}
	return strings.Join(lines, "\n")
}

func runWorkflowBash(script string, environment map[string]string) ([]byte, error) {
	command := exec.Command("bash", "-c", script)
	command.Env = []string{"PATH=" + os.Getenv("PATH")}
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.CombinedOutput()
}

func cloneEnvironment(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source))
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func assertCredentialPassword(t *testing.T, tempDir, job, path, want string) {
	t.Helper()
	password, ok := credentialPassword(tempDir, job, path)
	if !ok {
		t.Fatalf("no credential returned for %s", path)
	}
	if password != want {
		t.Fatalf("credential password for %s = %q, want %q", path, password, want)
	}
}

func assertNoCredential(t *testing.T, tempDir, job, path string) {
	t.Helper()
	if password, ok := credentialPassword(tempDir, job, path); ok {
		t.Fatalf("unexpected credential for %s: %q", path, password)
	}
}

func credentialPassword(tempDir, job, path string) (string, bool) {
	command := exec.Command("git", "credential", "fill")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(tempDir, "goprivate-"+job+".gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GITHUB_JOB=" + job,
		"RUNNER_TEMP=" + tempDir,
	}
	command.Stdin = strings.NewReader("protocol=https\nhost=github.com\npath=" + path + "\n\n")
	output, err := command.CombinedOutput()
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "password=") {
			return strings.TrimPrefix(line, "password="), true
		}
	}
	return "", false
}
