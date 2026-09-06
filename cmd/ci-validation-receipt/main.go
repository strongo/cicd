package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/strongo/go-ci-action/internal/civalidation"
)

func main() {
	if len(os.Args) != 2 {
		fatalf("usage: ci-validation-receipt <policy-digest|create|resolve>")
	}
	switch os.Args[1] {
	case "policy-digest":
		policy, err := civalidation.PolicyFromEnvironment()
		if err != nil {
			fatalf("policy: %v", err)
		}
		digest, err := civalidation.PolicyDigest(policy)
		if err != nil {
			fatalf("policy digest: %v", err)
		}
		fmt.Println(digest)
	case "create":
		create()
	case "resolve":
		resolve()
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func create() {
	policy, err := civalidation.PolicyFromEnvironment()
	if err != nil {
		fatalf("policy: %v", err)
	}
	digest, err := civalidation.PolicyDigest(policy)
	if err != nil {
		fatalf("policy digest: %v", err)
	}
	prNumber, err := strconv.Atoi(required("CI_RECEIPT_PR_NUMBER"))
	if err != nil || prNumber <= 0 {
		fatalf("CI_RECEIPT_PR_NUMBER must be a positive integer")
	}
	runID, err := civalidation.ParseInt64("CI_RECEIPT_RUN_ID", required("CI_RECEIPT_RUN_ID"))
	if err != nil {
		fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	workflowID, err := civalidation.LookupWorkflowID(ctx, nil, os.Getenv("GITHUB_API_URL"), required("GITHUB_TOKEN"), required("GITHUB_REPOSITORY"), runID)
	if err != nil {
		fatalf("read current workflow identity: %v", err)
	}
	runAttempt, err := strconv.Atoi(required("CI_RECEIPT_RUN_ATTEMPT"))
	if err != nil || runAttempt <= 0 {
		fatalf("CI_RECEIPT_RUN_ATTEMPT must be a positive integer")
	}
	checkoutSHA, checkoutTree := gitIdentity(required("CI_RECEIPT_CHECKOUT_DIR"))
	if expectedCheckoutSHA := required("CI_RECEIPT_EXPECTED_CHECKOUT_SHA"); checkoutSHA != expectedCheckoutSHA {
		fatalf("checked-out SHA %s does not match trusted run SHA %s", checkoutSHA, expectedCheckoutSHA)
	}
	receipt := civalidation.Receipt{
		Schema:       civalidation.ReceiptSchema,
		Repository:   required("GITHUB_REPOSITORY"),
		PullRequest:  civalidation.PullRequest{Number: prNumber, HeadRef: required("CI_RECEIPT_PR_HEAD_REF"), HeadSHA: required("CI_RECEIPT_PR_HEAD_SHA"), BaseSHA: required("CI_RECEIPT_PR_BASE_SHA")},
		Checkout:     civalidation.Checkout{SHA: checkoutSHA, Tree: checkoutTree},
		Workflow:     civalidation.Workflow{Revision: required("CI_RECEIPT_WORKFLOW_REVISION"), RunID: runID, RunAttempt: runAttempt, WorkflowID: workflowID},
		Policy:       civalidation.PolicyBinding{Digest: digest},
		RequiredJobs: map[string]string{"go_lint": required("CI_RECEIPT_LINT_CONCLUSION"), "go_test_build": required("CI_RECEIPT_TEST_CONCLUSION")},
	}
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		fatalf("encode receipt: %v", err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(required("CI_RECEIPT_OUTPUT"), encoded, 0o600); err != nil {
		fatalf("write receipt: %v", err)
	}
}

func resolve() {
	output := required("GITHUB_OUTPUT")
	writeDecision := func(decision civalidation.Decision) {
		value := "false"
		if decision.Reuse {
			value = "true"
		}
		reason := strings.NewReplacer("\n", " ", "\r", " ").Replace(decision.Reason)
		file, err := os.OpenFile(output, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fatalf("open GITHUB_OUTPUT: %v", err)
		}
		defer file.Close()
		fmt.Fprintf(file, "reuse_valid=%s\nreceipt_run_id=%d\nreason=%s\n", value, decision.ReceiptRunID, reason)
	}
	policy, err := civalidation.PolicyFromEnvironment()
	if err != nil {
		writeDecision(civalidation.Decision{Reason: "invalid current policy: " + err.Error()})
		return
	}
	digest, err := civalidation.PolicyDigest(policy)
	if err != nil {
		writeDecision(civalidation.Decision{Reason: "cannot digest current policy: " + err.Error()})
		return
	}
	runID, err := civalidation.ParseInt64("GITHUB_RUN_ID", required("GITHUB_RUN_ID"))
	if err != nil {
		writeDecision(civalidation.Decision{Reason: err.Error()})
		return
	}
	_, tree := gitIdentity(required("CI_RECEIPT_CHECKOUT_DIR"))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	decision, err := civalidation.Resolve(ctx, nil, civalidation.ResolveConfig{
		APIURL: os.Getenv("GITHUB_API_URL"), Token: required("GITHUB_TOKEN"), Repository: required("GITHUB_REPOSITORY"),
		LandedSHA: required("GITHUB_SHA"), LandedTree: tree, TargetBranch: required("GITHUB_REF_NAME"), CurrentRunID: runID,
		WorkflowRevision: required("CI_RECEIPT_WORKFLOW_REVISION"), PolicyDigest: digest,
	})
	if err != nil {
		decision = civalidation.Decision{Reason: "receipt lookup failed: " + err.Error()}
	}
	if decision.Reuse {
		fmt.Printf("::notice title=Exact-tree validation reused::Validated by pull-request run %d.\n", decision.ReceiptRunID)
	} else {
		fmt.Printf("::notice title=Full validation selected::%s\n", decision.Reason)
	}
	writeDecision(decision)
}

func gitIdentity(directory string) (string, string) {
	resolve := func(revision string) string {
		command := exec.Command("git", "-C", filepath.Clean(directory), "rev-parse", revision)
		output, err := command.Output()
		if err != nil {
			fatalf("resolve git %s: %v", revision, err)
		}
		return strings.TrimSpace(string(output))
	}
	return resolve("HEAD"), resolve("HEAD^{tree}")
}

func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		fatalf("%s is required", name)
	}
	return value
}

func fatalf(format string, values ...any) {
	fmt.Fprintf(os.Stderr, "ci-validation-receipt: "+format+"\n", values...)
	os.Exit(1)
}
