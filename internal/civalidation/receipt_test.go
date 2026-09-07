package civalidation

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testRepository   = "owner/repository"
	testLandedSHA    = "1111111111111111111111111111111111111111"
	testLandedTree   = "2222222222222222222222222222222222222222"
	testHeadSHA      = "3333333333333333333333333333333333333333"
	testCheckoutSHA  = "5555555555555555555555555555555555555555"
	testWorkflowSHA  = "6666666666666666666666666666666666666666"
	testPolicyDigest = "sha256:7777777777777777777777777777777777777777777777777777777777777777"
)

func TestPolicyDigestNormalizesTypedInputs(t *testing.T) {
	setMinimumPolicyEnvironment(t)
	t.Setenv("CI_POLICY_MIN_TEST_COVERAGE_PERCENT", "75.50")
	first, err := PolicyFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	firstDigest, err := PolicyDigest(first)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CI_POLICY_MIN_TEST_COVERAGE_PERCENT", "151/2")
	second, err := PolicyFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := PolicyDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("equivalent numeric policies differ: %s != %s", firstDigest, secondDigest)
	}

	second.GoVersion = "1.28.0"
	changedDigest, err := PolicyDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if changedDigest == firstDigest {
		t.Fatal("changed validation input retained the same policy digest")
	}
}

func TestResolveReusesOneAuthenticatedExactReceipt(t *testing.T) {
	fixture := newResolveFixture(t)
	decision, err := resolveFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Reuse || decision.ReceiptRunID != 123 {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestResolveReusesReceiptAfterPostMergeBaseDrift(t *testing.T) {
	fixture := newResolveFixture(t)
	// GitHub's pull-request API reports the current base ref. Once the PR has
	// landed, that SHA is the landed main commit (or a newer main commit), not
	// the pre-merge SHA observed by the pull-request validation run. The target
	// branch and exact landed tree provide the stable post-merge binding.
	fixture.pulls[0]["base"].(map[string]any)["sha"] = testLandedSHA

	decision, err := resolveFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Reuse || decision.ReceiptRunID != 123 {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestResolveRefusesEveryUnprovedBoundary(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*resolveFixtureData)
		want   string
	}{
		{name: "missing associated pull request", mutate: func(f *resolveFixtureData) { f.pulls = []map[string]any{} }, want: "found 0"},
		{name: "ambiguous associated pull requests", mutate: func(f *resolveFixtureData) { f.pulls = append(f.pulls, cloneMap(f.pulls[0])) }, want: "found 2"},
		{name: "fork pull request", mutate: func(f *resolveFixtureData) {
			f.pulls[0]["head"].(map[string]any)["repo"] = map[string]any{"full_name": "fork/repository"}
		}, want: "found 0"},
		{name: "wrong target branch", mutate: func(f *resolveFixtureData) { f.pulls[0]["base"].(map[string]any)["ref"] = "release" }, want: "found 0"},
		{name: "no successful workflow run", mutate: func(f *resolveFixtureData) { f.runs["workflow_runs"] = []map[string]any{}; f.runs["total_count"] = 0 }, want: "receipt, found 0"},
		{name: "run head branch mismatch", mutate: func(f *resolveFixtureData) {
			f.runs["workflow_runs"].([]map[string]any)[0]["head_branch"] = "other"
		}, want: "receipt, found 0"},
		{name: "expired receipt", mutate: func(f *resolveFixtureData) { f.artifacts[123]["artifacts"].([]map[string]any)[0]["expired"] = true }, want: "receipt, found 0"},
		{name: "ambiguous receipts", mutate: addSecondReceiptCandidate, want: "receipt, found 2"},
		{name: "artifact digest mismatch", mutate: func(f *resolveFixtureData) {
			f.artifacts[123]["artifacts"].([]map[string]any)[0]["digest"] = "sha256:" + strings.Repeat("0", 64)
		}, want: "digest mismatch"},
		{name: "repository mismatch", mutate: func(f *resolveFixtureData) { f.receipt.Repository = "other/repository"; f.rebuildArchive(t) }, want: "repository mismatch"},
		{name: "head mismatch", mutate: func(f *resolveFixtureData) {
			f.receipt.PullRequest.HeadSHA = strings.Repeat("8", 40)
			f.rebuildArchive(t)
		}, want: "pull request identity mismatch"},
		{name: "landed tree mismatch", mutate: func(f *resolveFixtureData) { f.receipt.Checkout.Tree = strings.Repeat("8", 40); f.rebuildArchive(t) }, want: "landed tree mismatch"},
		{name: "workflow revision mismatch", mutate: func(f *resolveFixtureData) {
			f.receipt.Workflow.Revision = strings.Repeat("8", 40)
			f.rebuildArchive(t)
		}, want: "workflow identity mismatch"},
		{name: "workflow run mismatch", mutate: func(f *resolveFixtureData) { f.receipt.Workflow.RunID = 999; f.rebuildArchive(t) }, want: "workflow identity mismatch"},
		{name: "validation policy mismatch", mutate: func(f *resolveFixtureData) {
			f.receipt.Policy.Digest = "sha256:" + strings.Repeat("8", 64)
			f.rebuildArchive(t)
		}, want: "validation policy mismatch"},
		{name: "lint was not successful", mutate: func(f *resolveFixtureData) { f.receipt.RequiredJobs["go_lint"] = "failure"; f.rebuildArchive(t) }, want: "job conclusions"},
		{name: "extra unrecognized conclusion", mutate: func(f *resolveFixtureData) { f.receipt.RequiredJobs["other"] = "success"; f.rebuildArchive(t) }, want: "job conclusions"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			tc.mutate(fixture)
			decision, err := resolveFixture(t, fixture)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Reuse {
				t.Fatalf("invalid fixture was reused: %+v", decision)
			}
			if !strings.Contains(decision.Reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", decision.Reason, tc.want)
			}
		})
	}
}

func TestResolveReturnsAPIFailureForFailClosedWrapper(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		http.Error(response, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	_, err := Resolve(context.Background(), server.Client(), ResolveConfig{APIURL: server.URL, Token: "token", Repository: testRepository, LandedSHA: testLandedSHA, LandedTree: testLandedTree, TargetBranch: "main", CurrentRunID: 456, WorkflowRevision: testWorkflowSHA, PolicyDigest: testPolicyDigest, ExactTreeReuse: true})
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("error = %v", err)
	}
}

type resolveFixtureData struct {
	t                    *testing.T
	exactTreeReuse       bool
	skipValidationOnMain bool
	pulls                []map[string]any
	currentRun           map[string]any
	runs                 map[string]any
	artifacts            map[int64]map[string]any
	archives             map[int64][]byte
	receipt              Receipt
}

func newResolveFixture(t *testing.T) *resolveFixtureData {
	t.Helper()
	receipt := Receipt{Schema: ReceiptSchema, Repository: testRepository, PullRequest: PullRequest{Number: 9, HeadRef: "feature", HeadSHA: testHeadSHA}, Checkout: Checkout{SHA: testCheckoutSHA, Tree: testLandedTree}, Workflow: Workflow{Revision: testWorkflowSHA, RunID: 123, RunAttempt: 1, WorkflowID: 77}, Policy: PolicyBinding{Digest: testPolicyDigest}, RequiredJobs: map[string]string{"go_lint": "success", "go_test_build": "success"}}
	f := &resolveFixtureData{
		t:              t,
		exactTreeReuse: true,
		pulls:          []map[string]any{{"number": 9, "merged_at": "2026-09-06T00:00:00Z", "merge_commit_sha": testLandedSHA, "base": map[string]any{"ref": "main", "sha": "4444444444444444444444444444444444444444"}, "head": map[string]any{"ref": "feature", "sha": testHeadSHA, "repo": map[string]any{"full_name": testRepository}}}},
		currentRun:     map[string]any{"id": 456, "workflow_id": 77},
		runs:           map[string]any{"total_count": 1, "workflow_runs": []map[string]any{{"id": int64(123), "workflow_id": 77, "run_attempt": 1, "event": "pull_request", "conclusion": "success", "head_branch": "feature", "head_sha": testHeadSHA, "head_repository": map[string]any{"full_name": testRepository}}}},
		artifacts:      map[int64]map[string]any{123: {"total_count": 1, "artifacts": []map[string]any{{"id": int64(321), "name": ReceiptName, "expired": false}}}},
		archives:       map[int64][]byte{}, receipt: receipt,
	}
	f.rebuildArchive(t)
	return f
}

func (f *resolveFixtureData) rebuildArchive(t *testing.T) {
	t.Helper()
	archive := receiptArchive(t, f.receipt)
	f.archives[321] = archive
	digest := sha256.Sum256(archive)
	f.artifacts[123]["artifacts"].([]map[string]any)[0]["digest"] = "sha256:" + hex.EncodeToString(digest[:])
}

func addSecondReceiptCandidate(f *resolveFixtureData) {
	second := cloneMap(f.runs["workflow_runs"].([]map[string]any)[0])
	second["id"] = int64(124)
	f.runs["workflow_runs"] = append(f.runs["workflow_runs"].([]map[string]any), second)
	f.runs["total_count"] = 2
	f.artifacts[124] = map[string]any{"total_count": 1, "artifacts": []map[string]any{{"id": int64(322), "name": ReceiptName, "expired": false, "digest": "sha256:" + strings.Repeat("9", 64)}}}
	f.archives[322] = f.archives[321]
}

func resolveFixture(t *testing.T, fixture *resolveFixtureData) (Decision, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		switch {
		case strings.Contains(path, "/commits/") && strings.HasSuffix(path, "/pulls"):
			writeFixtureJSON(t, response, fixture.pulls)
		case strings.HasSuffix(path, "/actions/runs/456"):
			writeFixtureJSON(t, response, fixture.currentRun)
		case strings.Contains(path, "/actions/workflows/77/runs"):
			writeFixtureJSON(t, response, fixture.runs)
		case strings.Contains(path, "/actions/runs/") && strings.HasSuffix(path, "/artifacts"):
			parts := strings.Split(path, "/")
			var runID int64
			for index, part := range parts {
				if part == "runs" && index+1 < len(parts) {
					runID, _ = ParseInt64("run", parts[index+1])
				}
			}
			writeFixtureJSON(t, response, fixture.artifacts[runID])
		case strings.Contains(path, "/actions/artifacts/") && strings.HasSuffix(path, "/zip"):
			parts := strings.Split(path, "/")
			var artifactID int64
			for index, part := range parts {
				if part == "artifacts" && index+1 < len(parts) {
					artifactID, _ = ParseInt64("artifact", parts[index+1])
				}
			}
			response.Header().Set("Content-Type", "application/zip")
			if _, err := response.Write(fixture.archives[artifactID]); err != nil {
				t.Errorf("write artifact fixture: %v", err)
			}
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	return Resolve(context.Background(), server.Client(), ResolveConfig{APIURL: server.URL, Token: "token", Repository: testRepository, LandedSHA: testLandedSHA, LandedTree: testLandedTree, TargetBranch: "main", CurrentRunID: 456, WorkflowRevision: testWorkflowSHA, PolicyDigest: testPolicyDigest, ExactTreeReuse: fixture.exactTreeReuse, SkipValidationOnMain: fixture.skipValidationOnMain})
}

func receiptArchive(t *testing.T, receipt Receipt) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create(ReceiptFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(file).Encode(receipt); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func cloneMap(value map[string]any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var clone map[string]any
	if err := json.Unmarshal(encoded, &clone); err != nil {
		panic(err)
	}
	return clone
}

func writeFixtureJSON(t *testing.T, response http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Errorf("write JSON fixture: %v", err)
	}
}

func setMinimumPolicyEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range []string{"CI_POLICY_CODE_COVERAGE", "CI_POLICY_DISABLE_VERSION_BUMPING", "CI_POLICY_INSTALL_FIREBASE_TOOLS", "CI_POLICY_GOLANGCI_LINT_CACHE", "CI_POLICY_RUN_GORELEASER", "CI_POLICY_CGO_ENABLED", "CI_POLICY_ALLOW_MAJOR_VERSION_BUMP", "CI_POLICY_ARTIFACT_ON_MAIN_ONLY", "CI_POLICY_EXACT_TREE_VALIDATION_REUSE"} {
		t.Setenv(name, "false")
	}
	for _, name := range []string{"CI_POLICY_GOLANGCI_LINT_CACHE_INVALIDATION_INTERVAL", "CI_POLICY_ARTIFACT_RETENTION_DAYS", "CI_POLICY_VALIDATION_RECEIPT_RETENTION_DAYS"} {
		t.Setenv(name, "7")
	}
}

// A merge commit's tree legitimately differs from the validated pull-request
// tree whenever main moved, which is the common case. A caller that protects
// main behind required pull-request checks may delegate validation to the
// green pull-request run instead of repeating it after the merge.
func TestResolveDelegatesMainValidationToTheGreenPullRequestRun(t *testing.T) {
	fixture := newResolveFixture(t)
	fixture.exactTreeReuse = false
	fixture.skipValidationOnMain = true
	fixture.artifacts = map[int64]map[string]any{}

	decision, err := resolveFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Reuse || decision.ReceiptRunID != 123 {
		t.Fatalf("decision = %+v", decision)
	}
	if !strings.Contains(decision.Reason, "#9") {
		t.Fatalf("reason %q does not name the validated pull request", decision.Reason)
	}
}

func TestResolveFallsBackFromTreeDriftToDelegatedValidation(t *testing.T) {
	fixture := newResolveFixture(t)
	fixture.skipValidationOnMain = true
	fixture.receipt.Checkout.Tree = strings.Repeat("8", 40)
	fixture.rebuildArchive(t)

	decision, err := resolveFixture(t, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.Reuse || !strings.Contains(decision.Reason, "validated green before merge") {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestResolveRefusesDelegationWithoutExactlyOneGreenPullRequestRun(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*resolveFixtureData)
		want   string
	}{
		{name: "direct push to main", mutate: func(f *resolveFixtureData) { f.pulls = []map[string]any{} }, want: "pull request for landed SHA, found 0"},
		{name: "no green run for the head", mutate: func(f *resolveFixtureData) {
			f.runs["workflow_runs"] = []map[string]any{}
			f.runs["total_count"] = 0
		}, want: "validation run, found 0"},
		{name: "ambiguous green runs", mutate: addSecondReceiptCandidate, want: "validation run, found 2"},
		{name: "no reuse mode enabled", mutate: func(f *resolveFixtureData) { f.skipValidationOnMain = false }, want: "no validation reuse mode is enabled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newResolveFixture(t)
			fixture.exactTreeReuse = false
			fixture.skipValidationOnMain = true
			tc.mutate(fixture)
			decision, err := resolveFixture(t, fixture)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Reuse {
				t.Fatalf("unproved boundary was reused: %+v", decision)
			}
			if !strings.Contains(decision.Reason, tc.want) {
				t.Fatalf("reason %q does not contain %q", decision.Reason, tc.want)
			}
		})
	}
}

// An absent or mis-plumbed CI_POLICY_VALIDATE_ON_MAIN must read as the safe
// workflow default, never as "skip validation".
func TestPolicyValidatesOnMainUnlessExplicitlyDisabled(t *testing.T) {
	setMinimumPolicyEnvironment(t)
	unset, err := PolicyFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if !unset.ValidateOnMain {
		t.Fatal("unset validate_on_main must keep main validation enabled")
	}
	unsetDigest, err := PolicyDigest(unset)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CI_POLICY_VALIDATE_ON_MAIN", "false")
	disabled, err := PolicyFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if disabled.ValidateOnMain {
		t.Fatal("explicit false must disable main validation")
	}
	disabledDigest, err := PolicyDigest(disabled)
	if err != nil {
		t.Fatal(err)
	}
	if disabledDigest == unsetDigest {
		t.Fatal("changing the main-validation policy must change the policy digest")
	}
}
