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
		"if: ${{ (inputs.reuse_exact_tree_validation || !inputs.validate_on_main) && github.event_name == 'push' && github.ref == 'refs/heads/main' }}",
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

func TestGoCIWorkflowKeepsMainValidationOnByDefault(t *testing.T) {
	workflow := readGoCIWorkflow(t)

	inputStart := strings.Index(workflow, "\n      validate_on_main:\n")
	if inputStart == -1 {
		t.Fatal("validate_on_main input is missing")
	}
	inputEnd := strings.Index(workflow[inputStart:], "\n      reuse_exact_tree_validation:")
	if inputEnd == -1 {
		t.Fatal("validate_on_main must be declared before the exact-tree reuse input")
	}
	input := workflow[inputStart : inputStart+inputEnd]
	if !strings.Contains(input, "type: boolean") || !strings.Contains(input, "default: true") {
		t.Fatal("main validation must stay enabled unless a caller opts out")
	}
	if !strings.Contains(input, "A direct push to main") || !strings.Contains(input, "semantic conflict") {
		t.Fatal("the weaker guarantee and its boundaries must stay documented on the input")
	}
	if !strings.Contains(workflow, "CI_POLICY_VALIDATE_ON_MAIN: ${{ inputs.validate_on_main }}") {
		t.Fatal("main-validation policy must bind into the receipt policy digest")
	}

	// Opting out changes which validation runs, never whether the deployable is
	// produced: both the caller's build command and the artifact upload stay
	// outside every reuse guard, exactly as under exact-tree reuse.
	build := workflowJob(t, workflow, "go_test_build", "publish_validation_receipt")
	for _, step := range []struct{ name, next string }{
		{"Build", "Set up Java JRE 24"},
		{"Upload build artifact", "Save Go dependencies and build cache"},
	} {
		if strings.Contains(workflowStep(t, build, step.name, step.next), "reuse_valid") {
			t.Fatalf("%q must never be skipped by validation reuse", step.name)
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
	if strings.Count(workflow, "uses: actions/create-github-app-token@v3") != 4 {
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

	customHostEnv := cloneEnvironment(common)
	customHostEnv["GITHUB_JOB"] = "custom-host-test"
	customHostEnv["PRIVATE_GIT_TOKEN"] = "CUSTOM_HOST_TOKEN"
	customHostEnv["PRIVATE_GIT_HOST"] = "ghe.example.com"
	customHostEnv["PRIVATE_GIT_HOSTS"] = "ghe.example.com/acme"
	if combined, err := runWorkflowBash(legacyConfig, customHostEnv); err != nil {
		t.Fatalf("custom-host credential setup failed: %v\n%s", err, combined)
	}
	assertCredentialPasswordAtHost(t, tempDir, "custom-host-test", "ghe.example.com", "acme/repo", "CUSTOM_HOST_TOKEN")
	assertNoCredentialAtHost(t, tempDir, "custom-host-test", "ghe.example.com", "another/repo")
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
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
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
	assertCredentialPasswordAtHost(t, tempDir, job, "github.com", path, want)
}

func assertCredentialPasswordAtHost(t *testing.T, tempDir, job, host, path, want string) {
	t.Helper()
	password, ok := credentialPassword(tempDir, job, host, path)
	if !ok {
		t.Fatalf("no credential returned for %s", path)
	}
	if password != want {
		t.Fatalf("credential password for %s = %q, want %q", path, password, want)
	}
}

func assertNoCredential(t *testing.T, tempDir, job, path string) {
	t.Helper()
	assertNoCredentialAtHost(t, tempDir, job, "github.com", path)
}

func assertNoCredentialAtHost(t *testing.T, tempDir, job, host, path string) {
	t.Helper()
	if password, ok := credentialPassword(tempDir, job, host, path); ok {
		t.Fatalf("unexpected credential for %s: %q", path, password)
	}
}

func credentialPassword(tempDir, job, host, path string) (string, bool) {
	command := exec.Command("git", "credential", "fill")
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_CONFIG_GLOBAL=" + filepath.Join(tempDir, "goprivate-"+job+".gitconfig"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GITHUB_JOB=" + job,
		"RUNNER_TEMP=" + tempDir,
	}
	command.Stdin = strings.NewReader("protocol=https\nhost=" + host + "\npath=" + path + "\n\n")
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

func TestGoCISecondaryInstallationPreservesPrimaryExactCredentials(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	for _, jobName := range []string{"go_lint", "go_test_build"} {
		t.Run(jobName, func(t *testing.T) {
			next := "go_test_build"
			if jobName == next {
				next = ""
			}
			job := workflowJob(t, workflow, jobName, next)
			temp := t.TempDir()
			env := map[string]string{"RUNNER_TEMP": temp, "GITHUB_ENV": filepath.Join(temp, "env"), "GITHUB_JOB": jobName, "PRIVATE_GIT_APP_TOKEN": "PRIMARY_TOKEN", "PRIVATE_GIT_APP_OWNER": "sneat-co", "PRIVATE_GIT_APP_REPOSITORIES": "private-one"}
			primary := workflowStepRun(t, workflowStep(t, job, "Set scoped GitHub App access for GOPRIVATE", "Set secondary scoped GitHub App access for GOPRIVATE"))
			secondary := workflowStepRun(t, workflowStep(t, job, "Set secondary scoped GitHub App access for GOPRIVATE", "Set GitHub access token for GOPRIVATE"))
			if out, err := runWorkflowBash(primary, env); err != nil {
				t.Fatalf("primary setup: %v %s", err, out)
			}
			env["PRIVATE_GIT_APP_TOKEN"] = "SECONDARY_TOKEN"
			env["PRIVATE_GIT_APP_OWNER"] = "datatug"
			env["PRIVATE_GIT_APP_REPOSITORIES"] = "datatug-core"
			if out, err := runWorkflowBash(secondary, env); err != nil {
				t.Fatalf("secondary setup: %v %s", err, out)
			}
			assertCredentialPassword(t, temp, jobName, "sneat-co/private-one", "PRIMARY_TOKEN")
			assertCredentialPassword(t, temp, jobName, "datatug/datatug-core", "SECONDARY_TOKEN")
			assertCredentialPassword(t, temp, jobName, "datatug/datatug-core.git", "SECONDARY_TOKEN")
			assertNoCredential(t, temp, jobName, "datatug/datatug-core-tools")
			assertNoCredential(t, temp, jobName, "other/datatug-core")
			validate := workflowStepRun(t, workflowStep(t, job, "Validate secondary scoped GitHub App configuration", "Mint scoped GitHub App token for GOPRIVATE"))
			for _, bad := range []string{"", ",", "other/repo", "repo\nrepositories=injected"} {
				env["PRIVATE_GIT_APP_CLIENT_ID"] = "client"
				env["PRIVATE_GIT_APP_PRIVATE_KEY"] = "test-key"
				env["PRIVATE_GIT_APP_REPOSITORIES"] = bad
				env["GITHUB_OUTPUT"] = filepath.Join(temp, "output")
				if _, err := runWorkflowBash(validate, env); err == nil {
					t.Fatalf("invalid secondary repos %q passed", bad)
				}
			}
		})
	}
}

func TestReleaseAppDependencyCredentialsRemainSeparateFromPublication(t *testing.T) {
	workflow := readReleaseWorkflow(t)
	validate := workflowStepRun(t, workflowStep(t, workflow, "Validate scoped GitHub App configuration", "Mint scoped GitHub App token for GOPRIVATE"))
	temp := t.TempDir()
	env := map[string]string{"PRIVATE_GIT_APP_CLIENT_ID": "client", "PRIVATE_GIT_APP_PRIVATE_KEY": "test-key", "PRIVATE_GIT_APP_OWNER": "datatug", "PRIVATE_GIT_APP_REPOSITORIES": " datatug-core,\n models ", "GITHUB_OUTPUT": filepath.Join(temp, "output")}
	if out, err := runWorkflowBash(validate, env); err != nil {
		t.Fatalf("valid release config: %v %s", err, out)
	}
	data, _ := os.ReadFile(env["GITHUB_OUTPUT"])
	if string(data) != "repositories=datatug-core,models\n" {
		t.Fatalf("normalized output %q", data)
	}
	env["PRIVATE_GIT_APP_OWNER"] = "datatug/other"
	if _, err := runWorkflowBash(validate, env); err == nil {
		t.Fatal("qualified owner accepted")
	}
	setup := workflowStepRun(t, workflowStep(t, workflow, "Set scoped GitHub App access for GOPRIVATE", "Set GitHub access token for GOPRIVATE"))
	env = map[string]string{"RUNNER_TEMP": temp, "GITHUB_ENV": filepath.Join(temp, "env"), "GITHUB_JOB": "release", "PRIVATE_GIT_APP_TOKEN": "READ_TOKEN", "PRIVATE_GIT_APP_OWNER": "datatug", "PRIVATE_GIT_APP_REPOSITORIES": "datatug-core"}
	if out, err := runWorkflowBash(setup, env); err != nil {
		t.Fatalf("release dependency setup: %v %s", err, out)
	}
	assertCredentialPassword(t, temp, "release", "datatug/datatug-core", "READ_TOKEN")
	assertNoCredential(t, temp, "release", "datatug/datatug")
	if !strings.Contains(workflow, "GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}") {
		t.Fatal("release publication token changed")
	}
	if !strings.Contains(workflow, "if: ${{ inputs.GOPRIVATE && inputs.goprivate_github_app_client_id == '' }}") {
		t.Fatal("legacy/default auth is not preserved")
	}
}

func TestGoCIWorkflowDedicatedExactRepositoryCredentials(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	for _, jobName := range []string{"go_lint", "go_test_build"} {
		t.Run(jobName, func(t *testing.T) {
			next := "go_test_build"
			if jobName == next {
				next = ""
			}
			job := workflowJob(t, workflow, jobName, next)
			exact := workflowStepRun(t, workflowStep(t, job, "Set exact repository access for GOPRIVATE", "Set GitHub access token for GOPRIVATE"))
			legacy := workflowStepRun(t, workflowStep(t, job, "Set GitHub access token for GOPRIVATE", "Download dependencies"))
			temp := t.TempDir()
			env := map[string]string{"RUNNER_TEMP": temp, "GITHUB_ENV": filepath.Join(temp, "env"), "GITHUB_JOB": jobName, "PRIVATE_GIT_REPOSITORIES": " github.com/improveit-now/improveit-go,\n ghe.example.com/team/module ", "PRIVATE_GIT_REPOSITORY_TOKEN": "EXACT_READ_ONLY", "PRIVATE_GIT_HOSTS": "github.com/sneat-co", "PRIVATE_GIT_HOST": "github.com"}
			app := workflowStepRun(t, workflowStep(t, job, "Set scoped GitHub App access for GOPRIVATE", "Set secondary scoped GitHub App access for GOPRIVATE"))
			appEnv := cloneEnvironment(env)
			appEnv["PRIVATE_GIT_APP_TOKEN"] = "DATATUG_APP"
			appEnv["PRIVATE_GIT_APP_OWNER"] = "datatug"
			appEnv["PRIVATE_GIT_APP_REPOSITORIES"] = "datatug-core"
			if out, err := runWorkflowBash(app, appEnv); err != nil {
				t.Fatalf("App setup: %v %s", err, out)
			}
			if out, err := runWorkflowBash(exact, env); err != nil || strings.Contains(string(out), "EXACT_READ_ONLY") {
				t.Fatalf("exact setup failed or leaked token: %v %s", err, out)
			}
			legacyEnv := cloneEnvironment(env)
			legacyEnv["PRIVATE_GIT_TOKEN"] = "LEGACY_NAMESPACE"
			legacyEnv["PRIVATE_GIT_APP_ENABLED"] = ""
			legacyEnv["PRIVATE_GIT_EXACT_ENABLED"] = env["PRIVATE_GIT_REPOSITORIES"]
			if out, err := runWorkflowBash(legacy, legacyEnv); err != nil {
				t.Fatalf("legacy setup: %v %s", err, out)
			}
			assertCredentialPassword(t, temp, jobName, "improveit-now/improveit-go", "EXACT_READ_ONLY")
			assertCredentialPassword(t, temp, jobName, "improveit-now/improveit-go.git", "EXACT_READ_ONLY")
			assertCredentialPassword(t, temp, jobName, "sneat-co/existing-module", "LEGACY_NAMESPACE")
			assertCredentialPassword(t, temp, jobName, "datatug/datatug-core", "DATATUG_APP")
			assertNoCredential(t, temp, jobName, "datatug/datatug-core-child")
			assertCredentialPasswordAtHost(t, temp, jobName, "ghe.example.com", "team/module.git", "EXACT_READ_ONLY")
			for _, path := range []string{"improveit-now/improveit-go/child", "improveit-now/improveit-go.git/child", "improveit-now/improveit-go-suffix", "improveit-now/improveit-go.git.evil", "improveit-now/sibling", "improveit-now/improveit-go/../sibling", "improveit-now/improveit-go%2fchild"} {
				assertNoCredential(t, temp, jobName, path)
			}
			assertNoCredentialAtHost(t, temp, jobName, "github.com.evil", "improveit-now/improveit-go")
			assertNoCredentialAtHost(t, temp, jobName, "other.example", "improveit-now/improveit-go")
			helper := filepath.Join(temp, "goprivate-exact-"+jobName+"-credential-helper")
			for _, protocol := range []string{"http", "ssh"} {
				cmd := exec.Command(helper, "get")
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "RUNNER_TEMP=" + temp, "GITHUB_JOB=" + jobName}
				cmd.Stdin = strings.NewReader("protocol=" + protocol + "\nhost=github.com\npath=improveit-now/improveit-go\n\n")
				if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
					t.Fatalf("foreign protocol returned a credential: %v %s", err, out)
				}
			}
			for _, name := range []string{"goprivate-exact-" + jobName + ".token", "goprivate-exact-" + jobName + ".repositories"} {
				info, err := os.Stat(filepath.Join(temp, name))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("credential file permissions: %v %v", info, err)
				}
			}
			cleanupStart := strings.Index(job, "      - name: Remove exact repository dependency credentials\n")
			if cleanupStart < 0 {
				t.Fatal("no always cleanup")
			}
			cleanup := job[cleanupStart:]
			if !strings.Contains(cleanup, "if: ${{ always() }}") {
				t.Fatal("cleanup is not unconditional")
			}
			if out, err := runWorkflowBash(workflowStepRun(t, cleanup), env); err != nil {
				t.Fatalf("cleanup: %v %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(temp, "goprivate-exact-"+jobName+".token")); !os.IsNotExist(err) {
				t.Fatal("exact token retained after cleanup", err)
			}
			assertNoCredential(t, temp, jobName, "improveit-now/improveit-go")
			assertCredentialPassword(t, temp, jobName, "sneat-co/existing-module", "LEGACY_NAMESPACE")
			only := cloneEnvironment(env)
			only["GITHUB_JOB"] = jobName + "-exact-only"
			only["PRIVATE_GIT_HOSTS"] = ""
			if out, err := runWorkflowBash(exact, only); err != nil {
				t.Fatalf("exact-only setup: %v %s", err, out)
			}
			old := cloneEnvironment(only)
			old["PRIVATE_GIT_TOKEN"] = "LEGACY_NAMESPACE"
			old["PRIVATE_GIT_APP_ENABLED"] = ""
			old["PRIVATE_GIT_EXACT_ENABLED"] = only["PRIVATE_GIT_REPOSITORIES"]
			if out, err := runWorkflowBash(legacy, old); err != nil {
				t.Fatalf("exact-only legacy suppression: %v %s", err, out)
			}
			assertCredentialPassword(t, temp, only["GITHUB_JOB"], "improveit-now/improveit-go", "EXACT_READ_ONLY")
			assertNoCredential(t, temp, only["GITHUB_JOB"], "unrelated/repository")
			assertNoCredential(t, temp, only["GITHUB_JOB"], "improveit-now/improveit-go/child")

		})
	}
}

func TestGoCIWorkflowExactRepositoryConfigurationFailsClosed(t *testing.T) {
	workflow := readGoCIWorkflow(t)
	for _, jobName := range []string{"go_lint", "go_test_build"} {
		t.Run(jobName, func(t *testing.T) {
			next := "go_test_build"
			if jobName == next {
				next = ""
			}
			job := workflowJob(t, workflow, jobName, next)
			script := workflowStepRun(t, workflowStep(t, job, "Set exact repository access for GOPRIVATE", "Set GitHub access token for GOPRIVATE"))
			invalid := []string{"github.com", "github.com/owner", "https://github.com/owner/repo", "user@github.com/owner/repo", "github.com:443/owner/repo", "github.com/owner/repo/child", "github.com/owner/repo/", "github.com/owner/repo.git", "github.com/owner/repo.GIT", "github.com/./repo", "github.com/owner/..", "github.com/owner/repo?x", "github.com/owner/repo#x", "github.com/owner/*", "github.com/owner/repo\nhelper=bad", ".github.com/owner/repo", "github..com/owner/repo", "github.com./owner/repo", "github.com/owner/repo\rinside"}
			for _, list := range invalid {
				env := map[string]string{"RUNNER_TEMP": t.TempDir(), "GITHUB_ENV": filepath.Join(t.TempDir(), "env"), "GITHUB_JOB": jobName, "PRIVATE_GIT_REPOSITORIES": list, "PRIVATE_GIT_REPOSITORY_TOKEN": "EXACT_SYNTHETIC_SECRET", "PRIVATE_GIT_HOSTS": "", "PRIVATE_GIT_APP_ENABLED": "client-id", "PRIVATE_GIT_TOKEN": "LEGACY_MUST_NOT_BE_BORROWED"}
				if out, err := runWorkflowBash(script, env); err == nil || strings.Contains(string(out), "EXACT_SYNTHETIC_SECRET") {
					t.Fatalf("invalid list admitted or token leaked: %q %v %s", list, err, out)
				}
			}
			for _, pair := range [][2]string{{"github.com/owner/repo", ""}, {"", "EXACT_SYNTHETIC_SECRET"}, {" , \n ", "EXACT_SYNTHETIC_SECRET"}} {
				env := map[string]string{"RUNNER_TEMP": t.TempDir(), "GITHUB_ENV": filepath.Join(t.TempDir(), "env"), "GITHUB_JOB": jobName, "PRIVATE_GIT_REPOSITORIES": pair[0], "PRIVATE_GIT_REPOSITORY_TOKEN": pair[1], "PRIVATE_GIT_HOSTS": "", "PRIVATE_GIT_APP_ENABLED": "client-id", "PRIVATE_GIT_TOKEN": "LEGACY_MUST_NOT_BE_BORROWED"}
				if out, err := runWorkflowBash(script, env); err == nil || strings.Contains(string(out), "EXACT_SYNTHETIC_SECRET") {
					t.Fatalf("unpaired exact config accepted: %v %s", err, out)
				}
			}
			for _, prefix := range []string{"github.com", "github.com/improveit-now", "github.com/improveit-now/improveit-go", "github.com/improveit-now/improveit-go/child", "github.com/improveit-now/improveit-go.git", "github.com/improveit-now/improveit-go.git/child", "github.com/other/../improveit-now", "github.com/IMPROVEIT-NOW", "github.com/improveit-now/IMPROVEIT-GO", "github.com/IMPROVEIT-NOW/IMPROVEIT-GO/child", "github.com/improveit-now/improveit-go.GIT/child", "GITHUB.COM/improveit-now"} {
				env := map[string]string{"RUNNER_TEMP": t.TempDir(), "GITHUB_ENV": filepath.Join(t.TempDir(), "env"), "GITHUB_JOB": jobName, "PRIVATE_GIT_REPOSITORIES": "github.com/improveit-now/improveit-go", "PRIVATE_GIT_REPOSITORY_TOKEN": "EXACT_SYNTHETIC_SECRET", "PRIVATE_GIT_HOSTS": prefix}
				if out, err := runWorkflowBash(script, env); err == nil {
					t.Fatalf("unsafe mixed legacy prefix accepted: %q %s", prefix, out)
				}
			}
			for _, exactList := range []string{"github.com/IMPROVEIT-NOW/improveit-go", "github.com/improveit-now/IMPROVEIT-GO"} {
				env := map[string]string{"RUNNER_TEMP": t.TempDir(), "GITHUB_ENV": filepath.Join(t.TempDir(), "env"), "GITHUB_JOB": jobName, "PRIVATE_GIT_REPOSITORIES": exactList, "PRIVATE_GIT_REPOSITORY_TOKEN": "EXACT_SYNTHETIC_SECRET", "PRIVATE_GIT_HOSTS": "github.com/improveit-now/improveit-go.git/child"}
				if out, err := runWorkflowBash(script, env); err == nil {
					t.Fatalf("case alias exact recipient overlapped legacy path: %q %s", exactList, out)
				}
			}

		})
	}
}
