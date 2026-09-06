package civalidation

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type ResolveConfig struct {
	APIURL           string
	Token            string
	Repository       string
	LandedSHA        string
	LandedTree       string
	TargetBranch     string
	CurrentRunID     int64
	WorkflowRevision string
	PolicyDigest     string
}

type Decision struct {
	Reuse        bool
	Reason       string
	ReceiptRunID int64
}

type resolver struct {
	client *http.Client
	cfg    ResolveConfig
}

func LookupWorkflowID(ctx context.Context, client *http.Client, apiURL, token, repository string, runID int64) (int64, error) {
	if client == nil {
		client = http.DefaultClient
	}
	r := resolver{client: client, cfg: ResolveConfig{APIURL: apiURL, Token: token, Repository: repository}}
	if r.cfg.APIURL == "" {
		r.cfg.APIURL = "https://api.github.com"
	}
	var run apiRun
	if err := r.getJSON(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d", repository, runID), &run); err != nil {
		return 0, err
	}
	if run.WorkflowID == 0 {
		return 0, fmt.Errorf("workflow run has no workflow ID")
	}
	return run.WorkflowID, nil
}

func Resolve(ctx context.Context, client *http.Client, cfg ResolveConfig) (Decision, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if cfg.APIURL == "" {
		cfg.APIURL = "https://api.github.com"
	}
	if cfg.Repository == "" || cfg.Token == "" || cfg.LandedSHA == "" || cfg.LandedTree == "" || cfg.CurrentRunID == 0 || cfg.WorkflowRevision == "" || cfg.PolicyDigest == "" {
		return Decision{}, fmt.Errorf("incomplete resolver configuration")
	}
	r := resolver{client: client, cfg: cfg}
	return r.resolve(ctx)
}

func (r resolver) resolve(ctx context.Context) (Decision, error) {
	var pulls []apiPullRequest
	if err := r.getJSON(ctx, fmt.Sprintf("/repos/%s/commits/%s/pulls?per_page=100", r.cfg.Repository, r.cfg.LandedSHA), &pulls); err != nil {
		return Decision{}, fmt.Errorf("list associated pull requests: %w", err)
	}
	matchingPulls := make([]apiPullRequest, 0, 1)
	for _, pr := range pulls {
		if pr.MergedAt != "" && pr.MergeCommitSHA == r.cfg.LandedSHA && pr.Base.Ref == r.cfg.TargetBranch && pr.Head.Repository.FullName == r.cfg.Repository {
			matchingPulls = append(matchingPulls, pr)
		}
	}
	if len(matchingPulls) != 1 {
		return Decision{Reason: fmt.Sprintf("expected one merged same-repository pull request for landed SHA, found %d", len(matchingPulls))}, nil
	}
	pr := matchingPulls[0]

	var currentRun apiRun
	if err := r.getJSON(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d", r.cfg.Repository, r.cfg.CurrentRunID), &currentRun); err != nil {
		return Decision{}, fmt.Errorf("read current workflow run: %w", err)
	}
	if currentRun.WorkflowID == 0 {
		return Decision{Reason: "current workflow run has no workflow ID"}, nil
	}

	query := url.Values{}
	query.Set("event", "pull_request")
	query.Set("head_sha", pr.Head.SHA)
	query.Set("status", "success")
	query.Set("per_page", "100")
	var runs apiRuns
	if err := r.getJSON(ctx, fmt.Sprintf("/repos/%s/actions/workflows/%d/runs?%s", r.cfg.Repository, currentRun.WorkflowID, query.Encode()), &runs); err != nil {
		return Decision{}, fmt.Errorf("list pull request workflow runs: %w", err)
	}
	if runs.TotalCount > len(runs.Runs) {
		return Decision{Reason: "pull request run result is paginated and therefore ambiguous"}, nil
	}

	type validationCandidate struct {
		run      apiRun
		artifact apiArtifact
	}
	candidates := make([]validationCandidate, 0, 1)
	for _, run := range runs.Runs {
		if run.Event != "pull_request" || run.Conclusion != "success" || run.HeadBranch != pr.Head.Ref || run.HeadSHA != pr.Head.SHA || run.HeadRepository.FullName != r.cfg.Repository {
			continue
		}
		var artifacts apiArtifacts
		if err := r.getJSON(ctx, fmt.Sprintf("/repos/%s/actions/runs/%d/artifacts?name=%s&per_page=100", r.cfg.Repository, run.ID, ReceiptName), &artifacts); err != nil {
			return Decision{}, fmt.Errorf("list receipt artifacts for run %d: %w", run.ID, err)
		}
		if artifacts.TotalCount > len(artifacts.Artifacts) {
			return Decision{Reason: "receipt artifact result is paginated and therefore ambiguous"}, nil
		}
		for _, artifact := range artifacts.Artifacts {
			if artifact.Name == ReceiptName && !artifact.Expired {
				candidates = append(candidates, validationCandidate{run: run, artifact: artifact})
			}
		}
	}
	if len(candidates) != 1 {
		return Decision{Reason: fmt.Sprintf("expected one successful pull request validation receipt, found %d", len(candidates))}, nil
	}
	candidate := candidates[0]
	archive, err := r.getBytes(ctx, fmt.Sprintf("/repos/%s/actions/artifacts/%d/zip", r.cfg.Repository, candidate.artifact.ID))
	if err != nil {
		return Decision{}, fmt.Errorf("download validation receipt: %w", err)
	}
	if err := verifyArtifactDigest(archive, candidate.artifact.Digest); err != nil {
		return Decision{Reason: err.Error()}, nil
	}
	receipt, err := receiptFromArchive(archive)
	if err != nil {
		return Decision{Reason: err.Error()}, nil
	}

	expected := Receipt{
		Repository:  r.cfg.Repository,
		PullRequest: PullRequest{Number: pr.Number, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA},
		Checkout:    Checkout{Tree: r.cfg.LandedTree},
		Workflow:    Workflow{Revision: r.cfg.WorkflowRevision, RunID: candidate.run.ID, RunAttempt: candidate.run.RunAttempt, WorkflowID: currentRun.WorkflowID},
		Policy:      PolicyBinding{Digest: r.cfg.PolicyDigest},
	}
	if err := ValidateReceipt(receipt, expected); err != nil {
		return Decision{Reason: err.Error()}, nil
	}
	return Decision{Reuse: true, Reason: "exact tree and validation policy match", ReceiptRunID: candidate.run.ID}, nil
}

func (r resolver) getJSON(ctx context.Context, path string, target any) error {
	body, err := r.getBytes(ctx, path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

func (r resolver) getBytes(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.cfg.APIURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+r.cfg.Token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	if err := response.Body.Close(); err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub API returned %s", response.Status)
	}
	return body, nil
}

func verifyArtifactDigest(archive []byte, expected string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(expected, prefix) || len(expected) != len(prefix)+64 {
		return fmt.Errorf("receipt artifact has no valid SHA-256 digest")
	}
	digest := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), strings.TrimPrefix(expected, prefix)) {
		return fmt.Errorf("receipt artifact digest mismatch")
	}
	return nil
}

func receiptFromArchive(archive []byte) (Receipt, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return Receipt{}, fmt.Errorf("open receipt artifact: %w", err)
	}
	if len(reader.File) != 1 || reader.File[0].Name != ReceiptFile || reader.File[0].FileInfo().IsDir() || reader.File[0].UncompressedSize64 > 64<<10 {
		return Receipt{}, fmt.Errorf("receipt artifact must contain exactly one small %s", ReceiptFile)
	}
	file, err := reader.File[0].Open()
	if err != nil {
		return Receipt{}, err
	}
	var receipt Receipt
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&receipt); err != nil {
		_ = file.Close()
		return Receipt{}, fmt.Errorf("decode receipt: %w", err)
	}
	if err := file.Close(); err != nil {
		return Receipt{}, fmt.Errorf("close receipt: %w", err)
	}
	return receipt, nil
}

type apiPullRequest struct {
	Number         int    `json:"number"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Base           struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"base"`
	Head struct {
		Ref        string `json:"ref"`
		SHA        string `json:"sha"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

type apiRun struct {
	ID             int64  `json:"id"`
	WorkflowID     int64  `json:"workflow_id"`
	RunAttempt     int    `json:"run_attempt"`
	Event          string `json:"event"`
	Conclusion     string `json:"conclusion"`
	HeadBranch     string `json:"head_branch"`
	HeadSHA        string `json:"head_sha"`
	HeadRepository struct {
		FullName string `json:"full_name"`
	} `json:"head_repository"`
}

type apiRuns struct {
	TotalCount int      `json:"total_count"`
	Runs       []apiRun `json:"workflow_runs"`
}
type apiArtifact struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Expired bool   `json:"expired"`
	Digest  string `json:"digest"`
}
type apiArtifacts struct {
	TotalCount int           `json:"total_count"`
	Artifacts  []apiArtifact `json:"artifacts"`
}

func ParseInt64(name, value string) (int64, error) {
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}
