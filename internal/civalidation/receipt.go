package civalidation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"strconv"
	"strings"
)

const (
	ReceiptSchema = 1
	ReceiptName   = "ci-validation-receipt"
	ReceiptFile   = "ci-validation-receipt.json"
)

type Policy struct {
	Schema                                int    `json:"schema"`
	CodeCoverage                          bool   `json:"code_coverage"`
	DisableVersionBumping                 bool   `json:"disable_version_bumping"`
	InstallFirebaseTools                  bool   `json:"install_firebase_tools"`
	LintTimeout                           string `json:"lint_timeout"`
	GolangCILintCache                     bool   `json:"golangci_lint_cache"`
	GolangCILintCacheInvalidationInterval string `json:"golangci_lint_cache_invalidation_interval"`
	GoogleApplicationCredentials          string `json:"google_application_credentials"`
	GoPrivate                             string `json:"goprivate"`
	GoPrivateGitHost                      string `json:"goprivate_git_host"`
	GoPrivateGitHosts                     string `json:"goprivate_git_hosts"`
	GoPrivateGitHubAppClientID            string `json:"goprivate_github_app_client_id"`
	GoPrivateGitHubAppOwner               string `json:"goprivate_github_app_owner"`
	GoPrivateGitHubAppRepositories        string `json:"goprivate_github_app_repositories"`
	AdditionalGoTestPath                  string `json:"additional_go_test_path"`
	MinimumTestCoveragePercent            string `json:"min_test_coverage_percent"`
	CoverPackage                          string `json:"coverpkg"`
	RunGoReleaser                         bool   `json:"run_goreleaser"`
	CGOEnabled                            bool   `json:"cgo_enabled"`
	TagPrefix                             string `json:"tag_prefix"`
	WorkingDirectory                      string `json:"working_directory"`
	AllowMajorVersionBump                 bool   `json:"allow_major_version_bump"`
	GoVersion                             string `json:"go_version"`
	DefaultBump                           string `json:"default_bump"`
	RequireConventionalPullRequestTitle   bool   `json:"require_conventional_pr_title"`
	BuildCommand                          string `json:"build_command"`
	ArtifactName                          string `json:"artifact_name"`
	ArtifactPath                          string `json:"artifact_path"`
	ArtifactRetentionDays                 string `json:"artifact_retention_days"`
	ArtifactOnMainOnly                    bool   `json:"artifact_on_main_only"`
	ExactTreeValidationReuse              bool   `json:"exact_tree_validation_reuse"`
	ValidateOnMain                        bool   `json:"validate_on_main"`
	ValidationReceiptRetentionDays        string `json:"validation_receipt_retention_days"`
}

type Receipt struct {
	Schema       int               `json:"schema"`
	Repository   string            `json:"repository"`
	PullRequest  PullRequest       `json:"pull_request"`
	Checkout     Checkout          `json:"checkout"`
	Workflow     Workflow          `json:"workflow"`
	Policy       PolicyBinding     `json:"policy"`
	RequiredJobs map[string]string `json:"required_jobs"`
}

type PullRequest struct {
	Number  int    `json:"number"`
	HeadRef string `json:"head_ref"`
	HeadSHA string `json:"head_sha"`
}

type Checkout struct {
	SHA  string `json:"sha"`
	Tree string `json:"tree"`
}

type Workflow struct {
	Revision   string `json:"revision"`
	RunID      int64  `json:"run_id"`
	RunAttempt int    `json:"run_attempt"`
	WorkflowID int64  `json:"workflow_id"`
}

type PolicyBinding struct {
	Digest string `json:"digest"`
}

func PolicyFromEnvironment() (Policy, error) {
	boolean := func(name string) (bool, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return false, nil
		}
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return false, fmt.Errorf("%s must be true or false: %w", name, err)
		}
		return parsed, nil
	}
	// Unset means the workflow default, which for this one input is true:
	// an absent or mis-plumbed value must never silently disable validation.
	booleanDefaultTrue := func(name string) (bool, error) {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return true, nil
		}
		return boolean(name)
	}
	number := func(name string) (string, error) {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return "", nil
		}
		parsed := new(big.Rat)
		if _, ok := parsed.SetString(value); !ok {
			return "", fmt.Errorf("%s must be a number, got %q", name, value)
		}
		return parsed.RatString(), nil
	}

	codeCoverage, err := boolean("CI_POLICY_CODE_COVERAGE")
	if err != nil {
		return Policy{}, err
	}
	disableVersionBumping, err := boolean("CI_POLICY_DISABLE_VERSION_BUMPING")
	if err != nil {
		return Policy{}, err
	}
	installFirebaseTools, err := boolean("CI_POLICY_INSTALL_FIREBASE_TOOLS")
	if err != nil {
		return Policy{}, err
	}
	lintCache, err := boolean("CI_POLICY_GOLANGCI_LINT_CACHE")
	if err != nil {
		return Policy{}, err
	}
	runGoReleaser, err := boolean("CI_POLICY_RUN_GORELEASER")
	if err != nil {
		return Policy{}, err
	}
	cgoEnabled, err := boolean("CI_POLICY_CGO_ENABLED")
	if err != nil {
		return Policy{}, err
	}
	allowMajor, err := boolean("CI_POLICY_ALLOW_MAJOR_VERSION_BUMP")
	if err != nil {
		return Policy{}, err
	}
	artifactOnMainOnly, err := boolean("CI_POLICY_ARTIFACT_ON_MAIN_ONLY")
	if err != nil {
		return Policy{}, err
	}
	reuse, err := boolean("CI_POLICY_EXACT_TREE_VALIDATION_REUSE")
	if err != nil {
		return Policy{}, err
	}
	validateOnMain, err := booleanDefaultTrue("CI_POLICY_VALIDATE_ON_MAIN")
	if err != nil {
		return Policy{}, err
	}
	requireConventionalPullRequestTitle, err := boolean("CI_POLICY_REQUIRE_CONVENTIONAL_PR_TITLE")
	if err != nil {
		return Policy{}, err
	}
	lintCacheDays, err := number("CI_POLICY_GOLANGCI_LINT_CACHE_INVALIDATION_INTERVAL")
	if err != nil {
		return Policy{}, err
	}
	minimumCoverage, err := number("CI_POLICY_MIN_TEST_COVERAGE_PERCENT")
	if err != nil {
		return Policy{}, err
	}
	artifactRetention, err := number("CI_POLICY_ARTIFACT_RETENTION_DAYS")
	if err != nil {
		return Policy{}, err
	}
	receiptRetention, err := number("CI_POLICY_VALIDATION_RECEIPT_RETENTION_DAYS")
	if err != nil {
		return Policy{}, err
	}

	return Policy{
		Schema:                                ReceiptSchema,
		CodeCoverage:                          codeCoverage,
		DisableVersionBumping:                 disableVersionBumping,
		InstallFirebaseTools:                  installFirebaseTools,
		LintTimeout:                           os.Getenv("CI_POLICY_LINT_TIMEOUT"),
		GolangCILintCache:                     lintCache,
		GolangCILintCacheInvalidationInterval: lintCacheDays,
		GoogleApplicationCredentials:          os.Getenv("CI_POLICY_GOOGLE_APPLICATION_CREDENTIALS"),
		GoPrivate:                             os.Getenv("CI_POLICY_GOPRIVATE"),
		GoPrivateGitHost:                      os.Getenv("CI_POLICY_GOPRIVATE_GIT_HOST"),
		GoPrivateGitHosts:                     os.Getenv("CI_POLICY_GOPRIVATE_GIT_HOSTS"),
		GoPrivateGitHubAppClientID:            os.Getenv("CI_POLICY_GOPRIVATE_GITHUB_APP_CLIENT_ID"),
		GoPrivateGitHubAppOwner:               os.Getenv("CI_POLICY_GOPRIVATE_GITHUB_APP_OWNER"),
		GoPrivateGitHubAppRepositories:        os.Getenv("CI_POLICY_GOPRIVATE_GITHUB_APP_REPOSITORIES"),
		AdditionalGoTestPath:                  os.Getenv("CI_POLICY_ADDITIONAL_GO_TEST_PATH"),
		MinimumTestCoveragePercent:            minimumCoverage,
		CoverPackage:                          os.Getenv("CI_POLICY_COVERPKG"),
		RunGoReleaser:                         runGoReleaser,
		CGOEnabled:                            cgoEnabled,
		TagPrefix:                             os.Getenv("CI_POLICY_TAG_PREFIX"),
		WorkingDirectory:                      os.Getenv("CI_POLICY_WORKING_DIRECTORY"),
		AllowMajorVersionBump:                 allowMajor,
		GoVersion:                             os.Getenv("CI_POLICY_GO_VERSION"),
		DefaultBump:                           os.Getenv("CI_POLICY_DEFAULT_BUMP"),
		RequireConventionalPullRequestTitle:   requireConventionalPullRequestTitle,
		BuildCommand:                          os.Getenv("CI_POLICY_BUILD_COMMAND"),
		ArtifactName:                          os.Getenv("CI_POLICY_ARTIFACT_NAME"),
		ArtifactPath:                          os.Getenv("CI_POLICY_ARTIFACT_PATH"),
		ArtifactRetentionDays:                 artifactRetention,
		ArtifactOnMainOnly:                    artifactOnMainOnly,
		ExactTreeValidationReuse:              reuse,
		ValidateOnMain:                        validateOnMain,
		ValidationReceiptRetentionDays:        receiptRetention,
	}, nil
}

func PolicyDigest(policy Policy) (string, error) {
	canonical, err := json.Marshal(policy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ValidateReceipt(receipt Receipt, expected Receipt) error {
	if receipt.Schema != ReceiptSchema {
		return fmt.Errorf("unsupported receipt schema %d", receipt.Schema)
	}
	if receipt.Repository != expected.Repository {
		return fmt.Errorf("repository mismatch")
	}
	if receipt.PullRequest != expected.PullRequest {
		return fmt.Errorf("pull request identity mismatch")
	}
	if !validObjectID(receipt.Checkout.SHA) {
		return fmt.Errorf("invalid checkout SHA")
	}
	if receipt.Checkout.Tree != expected.Checkout.Tree {
		return fmt.Errorf("landed tree mismatch")
	}
	if receipt.Workflow != expected.Workflow {
		return fmt.Errorf("workflow identity mismatch")
	}
	if receipt.Policy.Digest != expected.Policy.Digest {
		return fmt.Errorf("validation policy mismatch")
	}
	if receipt.RequiredJobs["go_lint"] != "success" || receipt.RequiredJobs["go_test_build"] != "success" || len(receipt.RequiredJobs) != 2 {
		return fmt.Errorf("required job conclusions are not exactly successful")
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) < 40 || len(value) > 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
