package go_ci_action

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	releaseWorkflowPath           = ".github/workflows/release.yml"
	publishedArtifactWorkflowPath = ".github/workflows/validate-published-artifact.yml"
)

func TestReleaseWorkflowExtractsAnAbsoluteRunnableBinaryPath(t *testing.T) {
	extractScript := releaseWorkflowRunBlock(t, "Extract published archive")

	for _, tc := range []struct {
		name        string
		archivePath string
	}{
		{name: "bare filename", archivePath: "specscore"},
		{name: "nested filename", archivePath: "bin/specscore"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := t.TempDir()
			payloadDir := filepath.Join(workDir, "payload")
			binaryInArchive := filepath.Join(payloadDir, tc.archivePath)
			if err := os.MkdirAll(filepath.Dir(binaryInArchive), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(binaryInArchive, []byte("#!/usr/bin/env bash\nprintf 'specscore 0.33.0\\n'\n"), 0o755); err != nil {
				t.Fatal(err)
			}

			archive := filepath.Join(workDir, "specscore_linux_amd64.tar.gz")
			tar := exec.Command("tar", "-C", payloadDir, "-czf", archive, tc.archivePath)
			if output, err := tar.CombinedOutput(); err != nil {
				t.Fatalf("create archive: %v\n%s", err, output)
			}

			extractDir := filepath.Join(workDir, "extract")
			if err := os.MkdirAll(extractDir, 0o755); err != nil {
				t.Fatal(err)
			}
			archiveDestination := filepath.Join(extractDir, filepath.Base(archive))
			if err := os.Rename(archive, archiveDestination); err != nil {
				t.Fatal(err)
			}
			outputFile := filepath.Join(workDir, "github-output")
			output, err := runBash(extractDir, extractScript, map[string]string{
				"BINARY":        "specscore",
				"GITHUB_OUTPUT": outputFile,
			})
			if err != nil {
				t.Fatalf("extract script: %v\n%s", err, output)
			}

			binPath := githubOutput(t, outputFile, "bin_path")
			if !filepath.IsAbs(binPath) {
				t.Fatalf("bin_path must be absolute, got %q", binPath)
			}
			wantPath, err := filepath.EvalSymlinks(filepath.Join(extractDir, tc.archivePath))
			if err != nil {
				t.Fatal(err)
			}
			if binPath != wantPath {
				t.Fatalf("bin_path = %q, want %q", binPath, wantPath)
			}
			version := exec.Command(binPath, "--version")
			if output, err := version.CombinedOutput(); err != nil {
				t.Fatalf("execute extracted binary: %v\n%s", err, output)
			} else if string(output) != "specscore 0.33.0\n" {
				t.Fatalf("binary output = %q", output)
			}
		})
	}
}

func TestReleaseWorkflowResolvesSmokeMetadataInParallelButGatesPublishedArtifacts(t *testing.T) {
	workflow := readReleaseWorkflow(t)

	metadataStart := strings.Index(workflow, "\n  resolve_artifact_smoke_metadata:\n")
	artifactStart := strings.Index(workflow, "\n  artifact_smoke_test:\n")
	if metadataStart < 0 || artifactStart < 0 || metadataStart >= artifactStart {
		t.Fatal("release workflow must declare an inspectable pre-build metadata job before artifact smoke tests")
	}
	metadata := workflow[metadataStart:artifactStart]
	for _, required := range []string{
		"name: Resolve release test configuration",
		"ref: ${{ github.sha }}",
		"id: metadata",
		"steps.metadata.outputs.binary",
	} {
		if !strings.Contains(metadata, required) {
			t.Fatalf("pre-build metadata job is missing %q", required)
		}
	}
	if strings.Contains(metadata, "needs:") {
		t.Fatal("source-bound metadata must not wait for release or promotion; it should run in parallel with the build")
	}

	artifactEnd := strings.Index(workflow, "\n  artifact_smoke_test_homebrew_cask:\n")
	if artifactEnd < 0 {
		t.Fatal("release workflow must retain the Homebrew smoke job")
	}
	artifact := workflow[artifactStart:artifactEnd]
	for _, required := range []string{
		"needs: [ release, macos_verify_and_promote, resolve_artifact_smoke_metadata ]",
		"!cancelled()",
		"needs.release.result == 'success'",
		"needs.resolve_artifact_smoke_metadata.result == 'success'",
		"needs.macos_verify_and_promote.result == 'success' || needs.macos_verify_and_promote.result == 'skipped'",
		"TAG: ${{ needs.release.outputs.tag || github.ref_name }}",
		"BINARY: ${{ needs.resolve_artifact_smoke_metadata.outputs.binary }}",
	} {
		if !strings.Contains(artifact, required) {
			t.Fatalf("artifact smoke job is missing exact publication-gate contract %q", required)
		}
	}
	if strings.Contains(workflow, "resolve_artifact_smoke_target:") {
		t.Fatal("do not add a runner-only tag resolver; smoke jobs can bind the released tag directly after their publication gates")
	}

	homebrewStart := artifactEnd
	homebrew := workflow[homebrewStart:]
	for _, required := range []string{
		"needs: [ release, macos_verify_and_promote, resolve_artifact_smoke_metadata ]",
		"!cancelled()",
		"needs.release.result == 'success'",
		"needs.resolve_artifact_smoke_metadata.result == 'success'",
		"needs.macos_verify_and_promote.result == 'success' || needs.macos_verify_and_promote.result == 'skipped'",
		"needs.resolve_artifact_smoke_metadata.outputs.has_homebrew_cask == 'true'",
		"BINARY: ${{ needs.resolve_artifact_smoke_metadata.outputs.binary }}",
	} {
		if !strings.Contains(homebrew, required) {
			t.Fatalf("Homebrew smoke job is missing exact publication-gate contract %q", required)
		}
	}
}

// TestReleaseWorkflowVerifiesRealPublishedArtifactBeforePromotion supersedes
// the removed TestReleaseWorkflowUsesConsumerGoReleaserSnapshotPreflight:
// strongo/cicd#70 replaced macos_signing_preflight (a full binary matrix
// SNAPSHOT built and verified on macos-latest, discarded, while `release`
// separately rebuilt and published the real matrix on ubuntu-latest -- two
// full matrix builds per release, verifying bytes that were never shipped)
// with a single build. `release` (ubuntu-latest) now builds the matrix
// once and, when require_notarized_macos, publishes a DRAFT; a new
// macos_verify_and_promote job downloads THAT EXACT published darwin/arm64
// asset, runs the same codesign/spctl/execute checks the old preflight ran
// against a throwaway snapshot, and promotes the draft to public only on
// success. This test asserts the new contract with the same rigor the old
// one asserted the removed one, plus the one-matrix-build property the
// whole change exists to deliver.
func TestReleaseWorkflowVerifiesRealPublishedArtifactBeforePromotion(t *testing.T) {
	workflow := readReleaseWorkflow(t)

	// Still true regardless of implementation shape: this job must verify
	// the CONSUMER's own artifact, never a custom quill-based helper. This
	// guards the same incident the removed test guarded, now against a
	// stronger implementation that doesn't even invoke GoReleaser at all in
	// this job (it downloads a published asset instead of building one).
	if strings.Contains(workflow, "anchore/quill") {
		t.Fatal("macOS verification must not download the unrelated anchore/quill tool")
	}
	for _, forbidden := range []string{
		"go mod init cicd-macos-signing-preflight",
		"github.com/goreleaser/quill/quill",
		"NewSigningConfigFromP12",
		"load.P12(",
		"PRECHECK_BINARY",
		"cicd-macos-signing-preflight",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("macOS verification must check the consumer's real published artifact, not a custom helper containing %q", forbidden)
		}
	}

	// The removed job's exact job-declaration line must be gone -- not just
	// unmentioned in prose (this file's comments deliberately still name
	// macos_signing_preflight when explaining what it replaced).
	if strings.Contains(workflow, "  macos_signing_preflight:\n") {
		t.Fatal("release workflow must not redeclare the removed macos_signing_preflight job")
	}
	if strings.Contains(workflow, "needs: macos_signing_preflight") {
		t.Fatal("release job must not depend on the removed macos_signing_preflight job")
	}

	// The core deliverable of strongo/cicd#70: exactly ONE matrix build per
	// release, not two. macos_verify_and_promote downloads a published
	// asset; it must never invoke goreleaser-action itself.
	if got := strings.Count(workflow, "uses: goreleaser/goreleaser-action@v7"); got != 1 {
		t.Fatalf("goreleaser-action must run exactly once (one matrix build), found %d", got)
	}

	for _, required := range []string{
		"  macos_verify_and_promote:\n",
		"if: ${{ inputs.require_notarized_macos && !failure() && !cancelled() && (needs.release.outputs.tag != '' || github.ref_type == 'tag') }}",
		"runs-on: macos-latest",
		"BINARY_OVERRIDE: ${{ inputs.artifact_smoke_test_binary }}",
		"TAG_PREFIX: ${{ inputs.tag_prefix }}",
		"TIMEOUT_SECONDS: ${{ inputs.artifact_smoke_test_timeout_seconds }}",
		"run_with_timeout \"$TIMEOUT_SECONDS\"",
		"process_group_exists",
		"os.killpg",
		".macos_verify_timed_out",
		"codesign --verify --deep --strict --verbose=4",
		"spctl -a -t install -vv",
		"source=Notarized Developer ID",
		`gh release edit "$TAG" --repo "$GITHUB_REPOSITORY" --draft=false`,
		// --draft is the mechanism that gates public visibility on
		// verification: GoReleaser OSS has no build/publish split
		// (confirmed: no publish/continue subcommand at the pinned
		// v2.18.0), so the release is published as a draft and only
		// promoted after this job verifies it.
		"${{ inputs.require_notarized_macos && '--draft' || '' }}",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("release workflow is missing exact macOS verification contract %q", required)
		}
	}

	// Signing secret guard must still run before any tag is cut -- this
	// invariant is unaffected by strongo/cicd#70 and lives entirely inside
	// the release job.
	tagGuardIndex := strings.Index(workflow, "Guard signing secret policy before tagging")
	tagIndex := strings.Index(workflow, "Determine and push guarded tag")
	if tagGuardIndex < 0 || tagIndex < 0 || tagGuardIndex > tagIndex {
		t.Fatal("signing secret guard must run before the guarded tag step")
	}

	// The REAL sequencing guarantee now comes from the needs:/if: graph,
	// not file position (unlike the removed job, which had to be declared
	// before the tag step to visibly gate it). Assert the graph edge
	// directly: macos_verify_and_promote must declare release as its
	// needs -- checked above via its `if:` referencing needs.release.
	verifyStart := strings.Index(workflow, "\n  macos_verify_and_promote:\n")
	verifyEnd := strings.Index(workflow, "\n  finalize_release:\n")
	if verifyStart < 0 || verifyEnd < 0 || verifyStart >= verifyEnd {
		t.Fatal("release workflow must contain an inspectable macos_verify_and_promote job")
	}
	verify := workflow[verifyStart:verifyEnd]
	if !strings.Contains(verify, "needs: release") {
		t.Fatal("macos_verify_and_promote must declare release as its needs")
	}

	// This job must never mutate anything by tagging directly, nor take
	// the Homebrew-cask-install path (that is layer 3's job, further
	// below, against the PROMOTED release). It legitimately DOES mutate
	// things now -- promote or roll back the release, and (only on the
	// AUR rollback path) a scoped `git push --force` to restore a
	// captured pre-release SHA -- unlike the removed preflight, which was
	// read-only by design, so only the specific unrelated/unscoped
	// mutations remain forbidden.
	for _, forbidden := range []string{"git tag ", "brew install", "brew tap"} {
		if strings.Contains(verify, forbidden) {
			t.Fatalf("macos_verify_and_promote must not mutate release tags or install via Homebrew, found %q", forbidden)
		}
	}
	// The only git push in this job must be the deliberate, guarded AUR
	// rollback restore -- never a bare/unscoped push.
	if strings.Contains(verify, "git push ") && !strings.Contains(verify, `"git", "push", "--force", git_url, pre_sha`) {
		t.Fatal("macos_verify_and_promote must not contain an unscoped git push outside the guarded AUR rollback restore")
	}
	if strings.Contains(verify, "output=\\\"( \\\"$BIN_PATH\\\" $SMOKE_CMD") {
		t.Fatal("macOS verification execution must use the bounded process-group watchdog")
	}
	for _, required := range []string{`printf '%s\n' "$output"`, `if [ "$status" -ne 0 ]; then`, `if [ -z "$(printf '%s' "$output"`} {
		if !strings.Contains(verify, required) {
			t.Fatalf("macOS verification must preserve execution diagnostics %q", required)
		}
	}

	// Deliberate simplification, not an oversight: this job downloads a
	// published asset rather than building from source, so it needs none
	// of the private-module (GOPRIVATE) build setup the removed preflight
	// needed. Assert that setup step appears exactly once in the whole
	// file now (in the release job's own build), not duplicated here.
	if got := strings.Count(workflow, "Set GitHub access token for GOPRIVATE"); got != 1 {
		t.Fatalf("GOPRIVATE setup should appear exactly once (release job only, macos_verify_and_promote does not build), found %d", got)
	}

	if !strings.Contains(workflow, `if [ -n "$SIGN_P12" ] && [ "${{ inputs.require_notarized_macos }}" != "true" ]; then`) {
		t.Fatal("release workflow must reject a signing secret when notarization proof is disabled")
	}
}

func TestReleaseWorkflowReleasesTheLocallyCreatedTagWithoutRefetching(t *testing.T) {
	workflow := readReleaseWorkflow(t)
	if !strings.Contains(workflow, "uses: actions/checkout@v7\n        with:\n          fetch-depth: 0") {
		t.Fatal("release workflow must retain its full-history checkout before computing or releasing tags")
	}
	if strings.Contains(workflow, "git fetch --tags") {
		t.Fatal("release workflow must not refetch after pushing its locally created immutable tag")
	}
	tagIndex := strings.Index(workflow, "git tag \"$tag\"")
	releaseIndex := strings.Index(workflow, "- name: Run GoReleaser")
	if tagIndex == -1 || releaseIndex == -1 || tagIndex > releaseIndex {
		t.Fatal("release workflow must create its local immutable tag before running GoReleaser")
	}
	if !strings.Contains(workflow, "if: ${{ github.ref_type == 'tag' || steps.tag.outputs.new_tag != '' }}") {
		t.Fatal("release workflow must retain the exact existing-tag GoReleaser path")
	}

	tagScript := releaseWorkflowRunBlock(t, "Determine and push guarded tag")
	workspace := t.TempDir()
	fakeBinDir := filepath.Join(workspace, "bin")
	if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	callsFile := filepath.Join(workspace, "git-calls")
	fakeGit := filepath.Join(fakeBinDir, "git")
	if err := os.WriteFile(fakeGit, []byte(`#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CICD_GIT_CALLS"
case "$1" in
  tag|push) exit 0 ;;
  fetch)
    echo 'server certificate verification failed. CAfile: none CRLfile: none' >&2
    exit 128
    ;;
  *) exit 2 ;;
esac
`), 0o755); err != nil {
		t.Fatal(err)
	}

	outputFile := filepath.Join(workspace, "github-output")
	output, err := runBash(workspace, tagScript, map[string]string{
		"PREV":           "0.33.2",
		"NEXT":           "0.33.3",
		"NEXT_TAG":       "v0.33.3",
		"PREV_TAG":       "v0.33.2",
		"ALLOW_MAJOR":    "false",
		"PREFIX":         "v",
		"GITHUB_OUTPUT":  outputFile,
		"CICD_GIT_CALLS": callsFile,
		"PATH":           fakeBinDir + ":" + os.Getenv("PATH"),
	})
	if err != nil {
		t.Fatalf("create and push local tag: %v\n%s", err, output)
	}
	if got := githubOutput(t, outputFile, "new_tag"); got != "v0.33.3" {
		t.Fatalf("new_tag = %q, want v0.33.3", got)
	}
	calls, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(calls), "tag v0.33.3\npush origin refs/tags/v0.33.3\n"; got != want {
		t.Fatalf("git calls = %q, want %q", got, want)
	}
}

// TestReleaseWorkflowResolvesPreviousReleaseTag exercises the "Resolve
// previous release tag" step in isolation. It runs before "Compute next
// version from Git history" so that step can pass git-cliff an EXPLICIT
// <previous-tag>..HEAD range: git-cliff's own auto-detected range for
// --bumped-version silently drops conventional commits that reach HEAD only
// through a merge commit's second parent (sneat-dev/wb#160, empirically
// reproduced against sneat-dev/wb commit
// 298b9067c7686a4258b0e59d44d1db5a0e82d50e), even though the same commits
// are correctly found and classified by `git-cliff --unreleased`. An
// explicit range closes that gap.
func TestReleaseWorkflowResolvesPreviousReleaseTag(t *testing.T) {
	script := releaseWorkflowRunBlock(t, "Resolve previous release tag")

	// COLD START (strongo/cicd defect #2): a repo with no matching tag used
	// to leave `range` empty, which made git-cliff --bumped-version fall
	// back to reporting [bump].initial_tag verbatim regardless of the
	// commits in history -- a repo with only feat:/fix: commits and no tags
	// could never bootstrap its first release (empirically reproduced
	// against openvaultdb/ovdb: six feat: commits, still proposed v0.0.0).
	// The fix: create a synthetic, LOCAL-ONLY, never-pushed baseline tag on
	// an orphan commit (NOT an ancestor of HEAD) and pass an explicit
	// <synthetic-tag>..HEAD range, so git-cliff computes a real bump from
	// the actual parsed commit types instead of parroting initial_tag back.
	// These subtests assert the mechanism directly: the tag this step
	// creates really exists, really is NOT reachable from HEAD (so no real
	// commit is excluded from the diff the way tagging the true first
	// commit would exclude it -- git-cliff's range syntax is exclusive of
	// the left-hand tag), and is named to match [bump].initial_tag
	// ("${PREFIX}0.0.0") in the "Prepare git-cliff configuration" step
	// above. Whether git-cliff then actually computes the correct bump from
	// that range is exercised end-to-end, with the real git-cliff binary,
	// in TestReleaseWorkflowResolvesVersionAgainstRealGitCliff below; this
	// step has no git-cliff dependency of its own.
	t.Run("no previous tag yet", func(t *testing.T) {
		repository := initGitRepo(t)
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(repository, script, map[string]string{
			"PREFIX":        "v",
			"GITHUB_OUTPUT": outputFile,
		})
		if err != nil {
			t.Fatalf("resolve previous release tag: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "latest_tag"); got != "" {
			t.Fatalf("latest_tag = %q, want empty (no REAL previous release exists)", got)
		}
		if got := githubOutput(t, outputFile, "previous_version"); got != "0.0.0" {
			t.Fatalf("previous_version = %q, want 0.0.0", got)
		}
		if got := githubOutput(t, outputFile, "cold_start"); got != "true" {
			t.Fatalf("cold_start = %q, want true", got)
		}
		// The range must now point at a synthetic "v0.0.0..HEAD" baseline
		// (not empty) so git-cliff computes a real bump instead of falling
		// back to initial_tag.
		if got := githubOutput(t, outputFile, "range"); got != "v0.0.0..HEAD" {
			t.Fatalf("range = %q, want v0.0.0..HEAD", got)
		}
		// The synthetic tag must actually exist...
		tagCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0.0.0")
		tagCmd.Dir = repository
		if output, err := tagCmd.CombinedOutput(); err != nil {
			t.Fatalf("synthetic baseline tag v0.0.0 was not created: %v\n%s", err, output)
		}
		// ...and must NOT be an ancestor of HEAD, or git-cliff's exclusive
		// <tag>..HEAD range would silently drop a real first commit that
		// happens to be a feat: (the exact bug tagging the true first
		// commit would reintroduce).
		ancestorCmd := exec.Command("git", "merge-base", "--is-ancestor", "v0.0.0", "HEAD")
		ancestorCmd.Dir = repository
		if err := ancestorCmd.Run(); err == nil {
			t.Fatal("synthetic baseline tag v0.0.0 must not be an ancestor of HEAD")
		}
	})

	t.Run("cold-start synthetic tag is scoped to the exact configured prefix", func(t *testing.T) {
		repository := initGitRepo(t)
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(repository, script, map[string]string{
			"PREFIX":        "backend/v",
			"GITHUB_OUTPUT": outputFile,
		})
		if err != nil {
			t.Fatalf("resolve previous release tag: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "range"); got != "backend/v0.0.0..HEAD" {
			t.Fatalf("range = %q, want backend/v0.0.0..HEAD", got)
		}
		tagCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/backend/v0.0.0")
		tagCmd.Dir = repository
		if output, err := tagCmd.CombinedOutput(); err != nil {
			t.Fatalf("synthetic baseline tag backend/v0.0.0 was not created: %v\n%s", err, output)
		}
	})

	t.Run("picks the highest matching tag and builds an explicit range", func(t *testing.T) {
		repository := initGitRepo(t, "v0.1.0", "v0.2.0", "v0.10.0")
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(repository, script, map[string]string{
			"PREFIX":        "v",
			"GITHUB_OUTPUT": outputFile,
		})
		if err != nil {
			t.Fatalf("resolve previous release tag: %v\n%s", err, output)
		}
		// --sort=-v:refname is numeric-aware: v0.10.0 outranks v0.2.0.
		if got := githubOutput(t, outputFile, "latest_tag"); got != "v0.10.0" {
			t.Fatalf("latest_tag = %q, want v0.10.0", got)
		}
		if got := githubOutput(t, outputFile, "previous_version"); got != "0.10.0" {
			t.Fatalf("previous_version = %q, want 0.10.0", got)
		}
		if got := githubOutput(t, outputFile, "range"); got != "v0.10.0..HEAD" {
			t.Fatalf("range = %q, want v0.10.0..HEAD", got)
		}
		// An already-tagged repo must never take the cold-start path.
		if got := githubOutput(t, outputFile, "cold_start"); got != "false" {
			t.Fatalf("cold_start = %q, want false (a real tag exists)", got)
		}
	})

	t.Run("ignores tags outside the configured prefix", func(t *testing.T) {
		repository := initGitRepo(t, "backend/v0.9.0")
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(repository, script, map[string]string{
			"PREFIX":        "v",
			"GITHUB_OUTPUT": outputFile,
		})
		if err != nil {
			t.Fatalf("resolve previous release tag: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "latest_tag"); got != "" {
			t.Fatalf("latest_tag = %q, want empty (backend/v0.9.0 does not match prefix %q)", got, "v")
		}
	})

	t.Run("scoped prefix with a slash", func(t *testing.T) {
		repository := initGitRepo(t, "backend/v0.5.0")
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(repository, script, map[string]string{
			"PREFIX":        "backend/v",
			"GITHUB_OUTPUT": outputFile,
		})
		if err != nil {
			t.Fatalf("resolve previous release tag: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "latest_tag"); got != "backend/v0.5.0" {
			t.Fatalf("latest_tag = %q, want backend/v0.5.0", got)
		}
		if got := githubOutput(t, outputFile, "range"); got != "backend/v0.5.0..HEAD" {
			t.Fatalf("range = %q, want backend/v0.5.0..HEAD", got)
		}
	})
}

// TestReleaseWorkflowRemovesSyntheticColdStartBaselineTag exercises "Remove
// synthetic cold-start baseline tag" in isolation: the cleanup counterpart of
// the synthetic tag "Resolve previous release tag" creates for a cold start
// (see TestReleaseWorkflowResolvesPreviousReleaseTag above). It must run
// before "Determine and push guarded tag" and GoReleaser ever see the
// checkout, so the phantom local tag never reaches either.
func TestReleaseWorkflowRemovesSyntheticColdStartBaselineTag(t *testing.T) {
	script := releaseWorkflowRunBlock(t, "Remove synthetic cold-start baseline tag")

	t.Run("deletes the synthetic tag", func(t *testing.T) {
		repository := initGitRepo(t)
		tagCmd := exec.Command("git", "tag", "v0.0.0")
		tagCmd.Dir = repository
		if output, err := tagCmd.CombinedOutput(); err != nil {
			t.Fatalf("seed synthetic tag: %v\n%s", err, output)
		}
		output, err := runBash(repository, script, map[string]string{
			"PREFIX": "v",
		})
		if err != nil {
			t.Fatalf("remove synthetic cold-start baseline tag: %v\n%s", err, output)
		}
		verifyCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0.0.0")
		verifyCmd.Dir = repository
		if err := verifyCmd.Run(); err == nil {
			t.Fatal("synthetic baseline tag v0.0.0 must be deleted")
		}
	})

	t.Run("respects the configured prefix and never touches a real release tag", func(t *testing.T) {
		repository := initGitRepo(t, "v0.0.0", "backend/v0.0.0")
		output, err := runBash(repository, script, map[string]string{
			"PREFIX": "backend/v",
		})
		if err != nil {
			t.Fatalf("remove synthetic cold-start baseline tag: %v\n%s", err, output)
		}
		verifyCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/backend/v0.0.0")
		verifyCmd.Dir = repository
		if err := verifyCmd.Run(); err == nil {
			t.Fatal("backend/v0.0.0 must be deleted")
		}
		// A real "v0.0.0" tag under a DIFFERENT prefix than the one
		// configured must survive -- this step only ever deletes exactly
		// "${PREFIX}0.0.0".
		keepCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0.0.0")
		keepCmd.Dir = repository
		if err := keepCmd.Run(); err != nil {
			t.Fatal("v0.0.0 (a different prefix's tag) must not be touched")
		}
	})

	t.Run("is a no-op, not a failure, when no synthetic tag exists", func(t *testing.T) {
		// The already-tagged path never creates a synthetic tag at all; the
		// step's own `if:` condition (cold_start == 'true') is expected to
		// skip it, but the script itself must also tolerate being run
		// against a repository where the tag simply isn't there.
		repository := initGitRepo(t, "v0.33.2")
		output, err := runBash(repository, script, map[string]string{
			"PREFIX": "v",
		})
		if err != nil {
			t.Fatalf("remove synthetic cold-start baseline tag: %v\n%s", err, output)
		}
		keepCmd := exec.Command("git", "rev-parse", "-q", "--verify", "refs/tags/v0.33.2")
		keepCmd.Dir = repository
		if err := keepCmd.Run(); err != nil {
			t.Fatal("v0.33.2 must not be touched")
		}
	})
}

// TestReleaseWorkflowResolvesConfiguredDefaultBumpFromSharedPreviousTag locks
// in the "Resolve configured default bump" step's post-refactor contract: it
// now consumes LATEST_TAG/PREVIOUS_VERSION from the "Resolve previous release
// tag" step's outputs instead of rescanning `git tag` itself, so the range fed
// to git-cliff and the baseline this step compares against can never
// disagree. A useful side effect is that the step is now a pure function of
// its environment: these subtests run with no git repository at all.
func TestReleaseWorkflowResolvesConfiguredDefaultBumpFromSharedPreviousTag(t *testing.T) {
	script := releaseWorkflowRunBlock(t, "Resolve configured default bump")
	workspace := t.TempDir() // deliberately not a git repository

	t.Run("accepts a valid greater proposed version", func(t *testing.T) {
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(workspace, script, map[string]string{
			"PROPOSED_VERSION": "v0.34.0",
			"PREFIX":           "v",
			"DEFAULT_BUMP":     "false",
			"LATEST_TAG":       "v0.33.2",
			"PREVIOUS_VERSION": "0.33.2",
			"GITHUB_OUTPUT":    outputFile,
		})
		if err != nil {
			t.Fatalf("resolve configured default bump: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "new_version"); got != "0.34.0" {
			t.Fatalf("new_version = %q, want 0.34.0", got)
		}
		if got := githubOutput(t, outputFile, "new_tag"); got != "v0.34.0" {
			t.Fatalf("new_tag = %q, want v0.34.0", got)
		}
		if got := githubOutput(t, outputFile, "previous_tag"); got != "v0.33.2" {
			t.Fatalf("previous_tag = %q, want v0.33.2", got)
		}
	})

	t.Run("falls back to default_bump when git-cliff proposed nothing", func(t *testing.T) {
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(workspace, script, map[string]string{
			"PROPOSED_VERSION": "v0.33.2", // unchanged: git-cliff found nothing to bump
			"PREFIX":           "v",
			"DEFAULT_BUMP":     "patch",
			"LATEST_TAG":       "v0.33.2",
			"PREVIOUS_VERSION": "0.33.2",
			"GITHUB_OUTPUT":    outputFile,
		})
		if err != nil {
			t.Fatalf("resolve configured default bump: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "new_version"); got != "0.33.3" {
			t.Fatalf("new_version = %q, want 0.33.3", got)
		}
	})

	// SILENT-NOOP INCIDENT fix: declining still sets no GITHUB_OUTPUT (a
	// chained "Determine and push guarded tag" step must still see an empty
	// new_version and skip), but the run is no longer silent about it -- an
	// `::notice::` naming the proposed version, the baseline, default_bump,
	// and which of those caused the decline must appear on stdout, where
	// GitHub Actions surfaces it on the run's Annotations panel regardless
	// of which step emitted it. This is the exact scenario behind the
	// sneat-cli and datatug-cli incidents: an already-tagged repo where no
	// commit since the last tag implies a bump.
	t.Run("declines without setting outputs, but emits a loud notice, when default_bump is disabled and nothing was proposed", func(t *testing.T) {
		outputFile := filepath.Join(t.TempDir(), "github-output")
		if err := os.WriteFile(outputFile, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		output, err := runBash(workspace, script, map[string]string{
			"PROPOSED_VERSION": "v0.33.2",
			"PREFIX":           "v",
			"DEFAULT_BUMP":     "false",
			"LATEST_TAG":       "v0.33.2",
			"PREVIOUS_VERSION": "0.33.2",
			"COLD_START":       "false",
			"GITHUB_OUTPUT":    outputFile,
		})
		if err != nil {
			t.Fatalf("resolve configured default bump: %v\n%s", err, output)
		}
		content, err := os.ReadFile(outputFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) != 0 {
			t.Fatalf("GITHUB_OUTPUT = %q, want no output written", content)
		}
		assertDeclineNotice(t, string(output), "v0.33.2", "v0.33.2", "false")
	})

	t.Run("handles the very first release with no previous tag", func(t *testing.T) {
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(workspace, script, map[string]string{
			"PROPOSED_VERSION": "v0.1.0",
			"PREFIX":           "v",
			"DEFAULT_BUMP":     "false",
			"LATEST_TAG":       "",
			"PREVIOUS_VERSION": "0.0.0",
			"COLD_START":       "true",
			"GITHUB_OUTPUT":    outputFile,
		})
		if err != nil {
			t.Fatalf("resolve configured default bump: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "new_version"); got != "0.1.0" {
			t.Fatalf("new_version = %q, want 0.1.0", got)
		}
		if got := githubOutput(t, outputFile, "previous_tag"); got != "" {
			t.Fatalf("previous_tag = %q, want empty", got)
		}
		// A successful bootstrap is not a decline: it must NOT emit the
		// decline notice (the run summary would be misleading otherwise).
		if strings.Contains(string(output), "::notice title=No release published this run::") {
			t.Fatalf("a successful cold-start bootstrap must not emit the decline notice, got:\n%s", output)
		}
	})

	// Defect 2 (cold start): a never-tagged repo whose entire history is
	// chore:/ci:/docs:/refactor: (or has no commits at all) must still
	// publish nothing -- the cold-start bootstrap must be gated on the same
	// releasing-commit-type rule as every other release, never leak into
	// "any never-tagged repo auto-releases". git-cliff proposing back the
	// same "0.0.0" baseline (see TestReleaseWorkflowResolvesVersionAgainst
	// RealGitCliff for the real git-cliff invocation that produces this)
	// is exactly the signal this step already treats as "nothing to bump".
	t.Run("declines a cold start with no releasing commits and explains the bootstrap failed", func(t *testing.T) {
		outputFile := filepath.Join(t.TempDir(), "github-output")
		if err := os.WriteFile(outputFile, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		output, err := runBash(workspace, script, map[string]string{
			"PROPOSED_VERSION": "v0.0.0",
			"PREFIX":           "v",
			"DEFAULT_BUMP":     "false",
			"LATEST_TAG":       "",
			"PREVIOUS_VERSION": "0.0.0",
			"COLD_START":       "true",
			"GITHUB_OUTPUT":    outputFile,
		})
		if err != nil {
			t.Fatalf("resolve configured default bump: %v\n%s", err, output)
		}
		content, err := os.ReadFile(outputFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(content) != 0 {
			t.Fatalf("GITHUB_OUTPUT = %q, want no output written (no releasing commit exists to bootstrap from)", content)
		}
		if !strings.Contains(string(output), "::notice title=No release published this run::") {
			t.Fatalf("declining a cold start must emit a decline notice, got:\n%s", output)
		}
		if !strings.Contains(string(output), "first release") {
			t.Fatalf("decline notice for a cold start must say this would have been the first release, got:\n%s", output)
		}
	})
}

// assertDeclineNotice asserts that output contains the "Resolve configured
// default bump" step's decline `::notice::`, and that it names the proposed
// version, the previous/baseline tag, and the default_bump setting -- the
// three pieces of context that used to require opening logs to reconstruct
// (see the three same-day incidents: sneat-cli, datatug-cli,
// openvaultdb/ovdb). It is deliberately loose about exact wording so the
// message can keep improving without brittle string-locking every rewrite,
// while still failing if any required piece of context goes missing.
func assertDeclineNotice(t *testing.T, output, proposedVersion, previousTag, defaultBump string) {
	t.Helper()
	if !strings.Contains(output, "::notice title=No release published this run::") {
		t.Fatalf("expected a decline ::notice::, got:\n%s", output)
	}
	for _, want := range []string{proposedVersion, previousTag, defaultBump} {
		if !strings.Contains(output, want) {
			t.Fatalf("decline notice must mention %q, got:\n%s", want, output)
		}
	}
}

// initGitRepo creates a git repository with a single commit and the given
// tags, all pointed at that commit, and returns its path.
func initGitRepo(t *testing.T, tags ...string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	run("init", "--initial-branch=main", "-q")
	run("config", "user.email", "release-test@example.invalid")
	run("config", "user.name", "Release Test")
	run("commit", "--allow-empty", "-q", "-m", "chore: bootstrap")
	for _, tag := range tags {
		run("tag", tag)
	}
	return dir
}

func TestPublishedArtifactWorkflowValidatesTheExactRequestedTag(t *testing.T) {
	validateScript := publishedArtifactWorkflowRunBlock(t, "Validate exact release tag and platforms")

	for _, tc := range []struct {
		name       string
		releaseTag string
		tagPrefix  string
		wantStatus int
		wantTag    string
	}{
		{
			name:       "valid plain semver tag",
			releaseTag: "v0.33.0",
			tagPrefix:  "v",
			wantStatus: 0,
			wantTag:    "v0.33.0",
		},
		{
			name:       "scoped prefix",
			releaseTag: "ingitdb/v0.33.0",
			tagPrefix:  "ingitdb/v",
			wantStatus: 0,
			wantTag:    "ingitdb/v0.33.0",
		},
		{
			name:       "rejects invalid semver",
			releaseTag: "v0.33",
			tagPrefix:  "v",
			wantStatus: 1,
		},
		{
			name:       "rejects semver component with leading zero",
			releaseTag: "v01.2.3",
			tagPrefix:  "v",
			wantStatus: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outputFile := filepath.Join(t.TempDir(), "github-output")
			env := map[string]string{
				"RELEASE_TAG":        tc.releaseTag,
				"TAG_PREFIX":         tc.tagPrefix,
				"ARTIFACT_BINARY":    "specscore",
				"ARTIFACT_PLATFORMS": `[{"runner":"ubuntu-latest","goos":"linux","goarch":"amd64"}]`,
				"GITHUB_OUTPUT":      outputFile,
			}
			output, err := runBash(t.TempDir(), validateScript, env)
			if tc.wantStatus == 0 {
				if err != nil {
					t.Fatalf("validate published artifact request: %v\n%s", err, output)
				}
				if got := githubOutput(t, outputFile, "release_tag"); got != tc.wantTag {
					t.Fatalf("validated tag = %q, want %q", got, tc.wantTag)
				}
				return
			}
			if err == nil {
				t.Fatalf("invalid published-artifact request succeeded:\n%s", output)
			}
		})
	}
}

func TestPublishedArtifactWorkflowCannotPassWithoutExecutingAnArtifact(t *testing.T) {
	downloadScript := publishedArtifactWorkflowRunBlock(t, "Download exact published release archive")
	extractScript := publishedArtifactWorkflowRunBlock(t, "Extract exact published release archive")

	t.Run("missing GitHub release or archive", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBin := filepath.Join(workspace, "fake-bin")
		if err := os.MkdirAll(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(fakeBin, "gh"), "#!/usr/bin/env bash\nexit 1\n")
		preparePublishedArtifactRunner(t, workspace)

		baseEnv := map[string]string{
			"PATH":              fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":       workspace,
			"GITHUB_REPOSITORY": "specscore/specscore-cli",
			"RELEASE_TAG":       "v0.33.0",
			"GOOS":              "linux",
			"GOARCH":            "amd64",
		}
		assertWorkflowFailure(t, workspace, downloadScript, baseEnv)
	})

	t.Run("download command returns success without an archive", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBin := filepath.Join(workspace, "fake-bin")
		if err := os.MkdirAll(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(fakeBin, "gh"), "#!/usr/bin/env bash\nexit 0\n")
		preparePublishedArtifactRunner(t, workspace)
		assertWorkflowFailure(t, workspace, downloadScript, map[string]string{
			"PATH":              fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":       workspace,
			"GITHUB_REPOSITORY": "specscore/specscore-cli",
			"RELEASE_TAG":       "v0.33.0",
			"GOOS":              "linux",
			"GOARCH":            "amd64",
		})
	})

	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "archive extraction fails",
			prepare: func(t *testing.T, workspace string) {
				if err := os.WriteFile(filepath.Join(workspace, "specscore_linux_amd64.tar.gz"), []byte("not an archive"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "archive has no resolved binary",
			prepare: func(t *testing.T, workspace string) {
				readme := filepath.Join(workspace, "README.md")
				if err := os.WriteFile(readme, []byte("no executable here"), 0o644); err != nil {
					t.Fatal(err)
				}
				archive := exec.Command("tar", "-C", workspace, "-czf", filepath.Join(workspace, "specscore_linux_amd64.tar.gz"), "README.md")
				if output, err := archive.CombinedOutput(); err != nil {
					t.Fatalf("create archive without binary: %v\n%s", err, output)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			tc.prepare(t, workspace)
			assertWorkflowFailure(t, workspace, extractScript, map[string]string{
				"ARCHIVE":         filepath.Join(workspace, "specscore_linux_amd64.tar.gz"),
				"ARTIFACT_BINARY": "specscore",
				"RELEASE_TAG":     "v0.33.0",
				"RUNNER_TEMP":     workspace,
			})
		})
	}
}

func TestPublishedArtifactWorkflowSuccessInvokesExecutable(t *testing.T) {
	workspace := t.TempDir()
	payloadDir := filepath.Join(workspace, "payload")
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	invokedFile := filepath.Join(workspace, "invoked")
	writeExecutable(t, filepath.Join(payloadDir, "specscore"), "#!/usr/bin/env bash\nprintf invoked > \"$INVOKED_FILE\"\nprintf '0.33.0\\n'\n")
	archivePath := filepath.Join(workspace, "specscore_linux_amd64.tar.gz")
	archive := exec.Command("tar", "-C", payloadDir, "-czf", archivePath, "specscore")
	if output, err := archive.CombinedOutput(); err != nil {
		t.Fatalf("create valid archive: %v\n%s", err, output)
	}

	extractOutput := filepath.Join(workspace, "extract-output")
	extractScript := publishedArtifactWorkflowRunBlock(t, "Extract exact published release archive")
	if output, err := runBash(workspace, extractScript, map[string]string{
		"ARCHIVE":         archivePath,
		"ARTIFACT_BINARY": "specscore",
		"RELEASE_TAG":     "v0.33.0",
		"RUNNER_TEMP":     workspace,
		"GITHUB_OUTPUT":   extractOutput,
	}); err != nil {
		t.Fatalf("extract valid published artifact: %v\n%s", err, output)
	}

	preparePublishedArtifactRunner(t, workspace)
	runScript := publishedArtifactWorkflowRunBlock(t, "Run exact published executable")
	output, err := runBash(workspace, runScript, map[string]string{
		"BIN_PATH":                 githubOutput(t, extractOutput, "bin_path"),
		"ARTIFACT_BINARY":          "specscore",
		"ARTIFACT_COMMAND":         "--version",
		"ARTIFACT_TIMEOUT_SECONDS": "2",
		"RELEASE_TAG":              "v0.33.0",
		"RUNNER_TEMP":              workspace,
		"INVOKED_FILE":             invokedFile,
	})
	if err != nil {
		t.Fatalf("run valid published artifact: %v\n%s", err, output)
	}
	if _, err := os.Stat(invokedFile); err != nil {
		t.Fatalf("successful published-artifact validation did not invoke the executable: %v\n%s", err, output)
	}
}

func TestReleaseWorkflowKeepsWarningSkipBehaviorForUntestableArtifacts(t *testing.T) {
	downloadScript := releaseWorkflowRunBlock(t, "Download published release asset")
	downloadScript = strings.ReplaceAll(downloadScript, "${{ matrix.goos }}", "linux")
	downloadScript = strings.ReplaceAll(downloadScript, "${{ matrix.goarch }}", "amd64")

	t.Run("missing release archive", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBin := filepath.Join(workspace, "fake-bin")
		if err := os.MkdirAll(fakeBin, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutable(t, filepath.Join(fakeBin, "gh"), "#!/usr/bin/env bash\nexit 1\n")
		outputFile := filepath.Join(workspace, "github-output")
		output, err := runBash(workspace, downloadScript, map[string]string{
			"PATH":              fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":       workspace,
			"GITHUB_OUTPUT":     outputFile,
			"GITHUB_REPOSITORY": "specscore/specscore-cli",
			"TAG":               "v0.33.0",
		})
		if err != nil {
			t.Fatalf("normal release download must warn and skip: %v\n%s", err, output)
		}
		if got := githubOutput(t, outputFile, "asset_found"); got != "false" {
			t.Fatalf("asset_found = %q, want false\n%s", got, output)
		}
		if !strings.Contains(string(output), "::warning::Could not test linux/amd64") {
			t.Fatalf("normal release download did not retain warning taxonomy:\n%s", output)
		}
	})

	extractScript := releaseWorkflowRunBlock(t, "Extract published archive")
	extractScript = strings.ReplaceAll(extractScript, "${{ matrix.goos }}", "linux")
	extractScript = strings.ReplaceAll(extractScript, "${{ matrix.goarch }}", "amd64")
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, string)
	}{
		{
			name: "download has no recognized archive",
			prepare: func(*testing.T, string) {
			},
		},
		{
			name: "archive extraction fails",
			prepare: func(t *testing.T, workspace string) {
				if err := os.WriteFile(filepath.Join(workspace, "specscore_linux_amd64.tar.gz"), []byte("not an archive"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "archive has no resolved binary",
			prepare: func(t *testing.T, workspace string) {
				if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("no executable here"), 0o644); err != nil {
					t.Fatal(err)
				}
				archive := exec.Command("tar", "-C", workspace, "-czf", filepath.Join(workspace, "specscore_linux_amd64.tar.gz"), "README.md")
				if output, err := archive.CombinedOutput(); err != nil {
					t.Fatalf("create archive without binary: %v\n%s", err, output)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			tc.prepare(t, workspace)
			outputFile := filepath.Join(workspace, "github-output")
			output, err := runBash(workspace, extractScript, map[string]string{
				"BINARY":        "specscore",
				"RUNNER_TEMP":   workspace,
				"GITHUB_OUTPUT": outputFile,
			})
			if err != nil {
				t.Fatalf("normal release extraction must warn and skip: %v\n%s", err, output)
			}
			if got := githubOutput(t, outputFile, "extracted"); got != "false" {
				t.Fatalf("extracted = %q, want false\n%s", got, output)
			}
			if !strings.Contains(string(output), "::warning::Could not test linux/amd64") {
				t.Fatalf("normal release extraction did not retain warning taxonomy:\n%s", output)
			}
		})
	}
}

func TestReleaseWorkflowReportsInvalidDarwinSignatureAfterExecutionFailure(t *testing.T) {
	script := releaseWorkflowRunBlock(t, "'Layer 1: run the published binary (must exit within timeout)'")
	script = strings.ReplaceAll(script, "${{ matrix.goos }}", "darwin")
	script = strings.ReplaceAll(script, "${{ matrix.goarch }}", "arm64")
	workspace := t.TempDir()
	fakeBin := filepath.Join(workspace, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "broken"), "#!/usr/bin/env bash\nexit 137\n")
	writeExecutable(t, filepath.Join(fakeBin, "codesign"), "#!/usr/bin/env bash\nprintf '%s\\n' invoked > \"$CODESIGN_MARKER\"\nprintf '%s\\n' 'code object is not signed at all'\nexit 1\n")
	marker := filepath.Join(workspace, "codesign-invoked")

	darwinOutput, err := runBash(workspace, script, map[string]string{
		"BIN_PATH":        filepath.Join(fakeBin, "broken"),
		"SMOKE_CMD":       "--version",
		"TIMEOUT_SECONDS": "2",
		"PATH":            fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CODESIGN_MARKER": marker,
	})
	if err == nil {
		t.Fatalf("broken darwin artifact unexpectedly passed:\n%s", darwinOutput)
	}
	if !strings.Contains(string(darwinOutput), "invalid macOS code signature") || !strings.Contains(string(darwinOutput), "exited 137") {
		t.Fatalf("Darwin execution failure did not preserve the original status and add code-signature diagnostics:\n%s", darwinOutput)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Darwin execution failure did not invoke deep strict codesign: %v\n%s", err, darwinOutput)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	linuxScript := releaseWorkflowRunBlock(t, "'Layer 1: run the published binary (must exit within timeout)'")
	linuxScript = strings.ReplaceAll(linuxScript, "${{ matrix.goos }}", "linux")
	linuxScript = strings.ReplaceAll(linuxScript, "${{ matrix.goarch }}", "arm64")
	linuxOutput, err := runBash(workspace, linuxScript, map[string]string{
		"BIN_PATH":        filepath.Join(fakeBin, "broken"),
		"SMOKE_CMD":       "--version",
		"TIMEOUT_SECONDS": "2",
		"PATH":            fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CODESIGN_MARKER": marker,
	})
	if err == nil {
		t.Fatalf("broken Linux artifact unexpectedly passed:\n%s", linuxOutput)
	}
	if strings.Contains(string(linuxOutput), "invalid macOS code signature") {
		t.Fatalf("Linux execution failure ran the Darwin-only diagnostic:\n%s", linuxOutput)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Linux execution failure unexpectedly invoked codesign, stat error=%v\n%s", err, linuxOutput)
	}
}

// TestReleaseWorkflowRollbackRestoresPublisherRepoOnlyWhenSafe exercises the
// compare-and-swap safety guard in "Roll back publisher repos and delete the
// draft release" (added alongside macos_verify_and_promote to restore the
// old macos_signing_preflight's all-or-nothing guarantee: a failed
// verification must not leave a Homebrew/Scoop/Nix formula push live
// pointing at a release that will never be promoted). The guard must reset
// a publisher repo to its pre-release SHA when nothing has touched it since
// GoReleaser's own push, and must REFUSE -- never force-reset -- when a
// concurrent push has moved that repo's HEAD since, naming both SHAs
// instead of silently clobbering someone else's commit.
// TestReleaseWorkflowCapturePreReleaseSHAsDerivesTargetsFromConsumerConfig
// exercises "Capture pre-release publisher repo SHAs" against the common
// case named explicitly when this rollback mechanism was requested: a
// consumer configuring only a Homebrew tap. Targets must come from the
// consumer's own .goreleaser.yaml (never hardcoded), and a WinGet
// publisher alongside it -- unrecoverable by this rollback -- must produce
// an explicit warning rather than being silently ignored.
func TestReleaseWorkflowCapturePreReleaseSHAsDerivesTargetsFromConsumerConfig(t *testing.T) {
	if _, err := exec.LookPath("yq"); err != nil {
		t.Skip("yq not installed; the real release runner image guarantees it, this dev machine may not")
	}
	script := releaseWorkflowRunBlock(t, "Capture pre-release publisher repo SHAs")

	workspace := t.TempDir()
	goreleaserConfig := "project_name: widget\n" +
		"homebrew_casks:\n" +
		"  - name: widget\n" +
		"    repository:\n" +
		"      owner: acme\n" +
		"      name: homebrew-tap\n" +
		"      branch: main\n" +
		"aurs:\n" +
		"  - name: widget-bin\n" +
		"    git_url: \"ssh://aur@aur.archlinux.org/widget-bin.git\"\n" +
		"    private_key: \"{{ .Env.AUR_SSH_PRIVATE_KEY }}\"\n" +
		"winget:\n" +
		"  - repository:\n" +
		"      owner: acme\n" +
		"      name: winget-pkgs\n"
	if err := os.WriteFile(filepath.Join(workspace, ".goreleaser.yaml"), []byte(goreleaserConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	fakeBinDir := filepath.Join(workspace, "fake-bin")
	if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const headSHA = "dddddddddddddddddddddddddddddddddddddddd"
	writeExecutable(t, filepath.Join(fakeBinDir, "gh"), "#!/usr/bin/env bash\n"+
		"set -euo pipefail\n"+
		"if [ \"$1\" = \"api\" ]; then\n"+
		"  printf '%s\\n' \"$CICD_HEAD_SHA\"\n"+
		"  exit 0\n"+
		"fi\n"+
		"exit 1\n")

	const aurHeadSHA = "5555555555555555555555555555555555555555"
	writeExecutable(t, filepath.Join(fakeBinDir, "git"), "#!/usr/bin/env bash\n"+
		"set -euo pipefail\n"+
		"if [ \"$1\" = \"ls-remote\" ]; then\n"+
		"  printf 'ref: refs/heads/master\\tHEAD\\n'\n"+
		"  printf '%s\\tHEAD\\n' \"$CICD_AUR_HEAD_SHA\"\n"+
		"  exit 0\n"+
		"fi\n"+
		"exit 1\n")

	outputFile := filepath.Join(workspace, "github-output")
	output, err := runBash(workspace, script, map[string]string{
		"PATH":                fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"GH_TOKEN":            "tap-token",
		"AUR_SSH_PRIVATE_KEY": "fake-private-key-contents",
		"GITHUB_OUTPUT":       outputFile,
		"RUNNER_TEMP":         workspace,
		"CICD_HEAD_SHA":       headSHA,
		"CICD_AUR_HEAD_SHA":   aurHeadSHA,
	})
	if err != nil {
		t.Fatalf("capture pre-release SHAs: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "::warning::") || !strings.Contains(string(output), "winget") {
		t.Fatalf("capture must warn by name about the unrecoverable WinGet publisher:\n%s", output)
	}
	targets := githubOutput(t, outputFile, "targets")
	for _, want := range []string{`"owner": "acme"`, `"name": "homebrew-tap"`, `"branch": "main"`, `"pre_sha": "` + headSHA + `"`} {
		if !strings.Contains(targets, want) {
			t.Fatalf("targets = %q, missing %q", targets, want)
		}
	}
	for _, want := range []string{`"kind": "aur"`, `"git_url": "ssh://aur@aur.archlinux.org/widget-bin.git"`, `"branch": "master"`, `"pre_sha": "` + aurHeadSHA + `"`} {
		if !strings.Contains(targets, want) {
			t.Fatalf("targets = %q, missing AUR target %q", targets, want)
		}
	}
	if strings.Contains(targets, "winget-pkgs") {
		t.Fatalf("targets must not include the unrecoverable WinGet repo: %q", targets)
	}
}

func TestReleaseWorkflowRollbackRestoresPublisherRepoOnlyWhenSafe(t *testing.T) {
	script := releaseWorkflowRunBlock(t, "Roll back publisher repos and delete the draft release")

	const owner, repo, branch = "acme", "homebrew-tap", "main"
	const preSHA, postSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rollbackTargets := `[{"owner":"` + owner + `","name":"` + repo + `","branch":"` + branch + `","pre_sha":"` + preSHA + `","post_sha":"` + postSHA + `"}]`

	newFakeGH := func(t *testing.T, workspace, currentSHA string) (fakeBinDir, callsFile string) {
		t.Helper()
		fakeBinDir = filepath.Join(workspace, "fake-bin")
		if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
			t.Fatal(err)
		}
		callsFile = filepath.Join(workspace, "gh-calls")
		writeExecutable(t, filepath.Join(fakeBinDir, "gh"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$CICD_GH_CALLS"
if [ "$1" = "api" ] && [ "$2" = "-X" ] && [ "$3" = "PATCH" ]; then
  printf '%s\n' "$*" >> "$CICD_PATCH_CALLS"
  exit 0
fi
if [ "$1" = "api" ]; then
  printf '%s\n' "`+"`"+`echo $CICD_CURRENT_SHA`+"`"+`"
  exit 0
fi
if [ "$1" = "release" ] && [ "$2" = "delete" ]; then
  exit 0
fi
exit 1
`)
		return fakeBinDir, callsFile
	}

	t.Run("safe: current HEAD matches GoReleaser's push, resets to pre-release SHA", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBinDir, callsFile := newFakeGH(t, workspace, postSHA)
		patchCallsFile := filepath.Join(workspace, "patch-calls")
		output, err := runBash(workspace, script, map[string]string{
			"PATH":             fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":      workspace,
			"RELEASE_GH_TOKEN": "release-token",
			"TAP_GH_TOKEN":     "tap-token",
			"ROLLBACK_TARGETS": rollbackTargets,
			"TAG":              "v1.2.3",
			"REPO":             "acme/widget",
			"CICD_GH_CALLS":    callsFile,
			"CICD_PATCH_CALLS": patchCallsFile,
			"CICD_CURRENT_SHA": postSHA,
		})
		if err != nil {
			t.Fatalf("rollback with a safe compare-and-swap must succeed: %v\n%s", err, output)
		}
		patchCalls, err := os.ReadFile(patchCallsFile)
		if err != nil {
			t.Fatalf("expected the PATCH ref-update call to run: %v\n%s", err, output)
		}
		if !strings.Contains(string(patchCalls), "repos/"+owner+"/"+repo+"/git/refs/heads/"+branch) {
			t.Fatalf("PATCH call did not target the expected ref: %s", patchCalls)
		}
		if !strings.Contains(string(patchCalls), "sha="+preSHA) {
			t.Fatalf("PATCH call did not reset to the pre-release SHA: %s", patchCalls)
		}
		if !strings.Contains(string(patchCalls), "force=true") {
			t.Fatalf("PATCH call was not forced: %s", patchCalls)
		}
		if !strings.Contains(string(output), "Restored "+owner+"/"+repo+"@"+branch) {
			t.Fatalf("rollback did not report the restored repo:\n%s", output)
		}
	})

	t.Run("unsafe: concurrent push since GoReleaser's push, refuses to reset", func(t *testing.T) {
		workspace := t.TempDir()
		const concurrentSHA = "cccccccccccccccccccccccccccccccccccccccc"
		fakeBinDir, callsFile := newFakeGH(t, workspace, concurrentSHA)
		patchCallsFile := filepath.Join(workspace, "patch-calls")
		output, err := runBash(workspace, script, map[string]string{
			"PATH":             fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":      workspace,
			"RELEASE_GH_TOKEN": "release-token",
			"TAP_GH_TOKEN":     "tap-token",
			"ROLLBACK_TARGETS": rollbackTargets,
			"TAG":              "v1.2.3",
			"REPO":             "acme/widget",
			"CICD_GH_CALLS":    callsFile,
			"CICD_PATCH_CALLS": patchCallsFile,
			"CICD_CURRENT_SHA": concurrentSHA,
		})
		if err == nil {
			t.Fatalf("rollback must exit non-zero when it refuses a repo, to stay visibly red:\n%s", output)
		}
		if _, statErr := os.Stat(patchCallsFile); statErr == nil {
			patchCalls, _ := os.ReadFile(patchCallsFile)
			t.Fatalf("rollback must NOT force-reset a repo with a concurrent push, but called PATCH: %s", patchCalls)
		}
		if !strings.Contains(string(output), "::error::") {
			t.Fatalf("rollback must report a concurrent-push refusal as an error:\n%s", output)
		}
		if !strings.Contains(string(output), postSHA) || !strings.Contains(string(output), concurrentSHA) {
			t.Fatalf("rollback refusal must name both the expected and actual SHA so a human can reconcile it:\n%s", output)
		}
	})

	t.Run("no targets: still deletes the unpromoted draft release", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBinDir, callsFile := newFakeGH(t, workspace, "")
		output, err := runBash(workspace, script, map[string]string{
			"PATH":             fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":      workspace,
			"RELEASE_GH_TOKEN": "release-token",
			"TAP_GH_TOKEN":     "tap-token",
			"ROLLBACK_TARGETS": "[]",
			"TAG":              "v1.2.3",
			"REPO":             "acme/widget",
			"CICD_GH_CALLS":    callsFile,
			"CICD_PATCH_CALLS": filepath.Join(workspace, "patch-calls"),
			"CICD_CURRENT_SHA": "",
		})
		if err != nil {
			t.Fatalf("rollback with no publisher targets must still succeed and delete the draft: %v\n%s", err, output)
		}
		calls, err := os.ReadFile(callsFile)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(calls), "release delete v1.2.3 --repo acme/widget --yes") {
			t.Fatalf("rollback did not delete the unpromoted draft release:\n%s", calls)
		}
	})

	// AUR is reachable only via SSH to a non-GitHub host
	// (ssh://aur@aur.archlinux.org/<pkg>.git), so it needs its own fake
	// `git` (intercepting ls-remote/clone/push) alongside the fake `gh`
	// used for the draft-release delete that always runs. No real SSH is
	// ever invoked: the fake `git` handles every subcommand the rollback
	// script calls directly, so GIT_SSH_COMMAND is set but never exercised
	// here -- this test is about the orchestration logic (compare-and-swap,
	// which calls run, in what order), not git/SSH plumbing itself.
	newFakeGitAndGH := func(t *testing.T, workspace, ghCurrentSHA, aurBranch, aurCurrentSHA string) (fakeBinDir string, ghCallsFile, gitCallsFile, gitPushCallsFile string) {
		t.Helper()
		fakeBinDir = filepath.Join(workspace, "fake-bin")
		if err := os.MkdirAll(fakeBinDir, 0o755); err != nil {
			t.Fatal(err)
		}
		ghCallsFile = filepath.Join(workspace, "gh-calls")
		writeExecutable(t, filepath.Join(fakeBinDir, "gh"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s
' "$*" >> "$CICD_GH_CALLS"
if [ "$1" = "release" ] && [ "$2" = "delete" ]; then
  exit 0
fi
exit 1
`)
		gitCallsFile = filepath.Join(workspace, "git-calls")
		gitPushCallsFile = filepath.Join(workspace, "git-push-calls")
		writeExecutable(t, filepath.Join(fakeBinDir, "git"), `#!/usr/bin/env bash
set -euo pipefail
printf '%s
' "$*" >> "${CICD_GIT_CALLS:-/dev/null}"
if [ "$1" = "ls-remote" ]; then
  printf 'ref: refs/heads/%s	HEAD
' "$CICD_AUR_BRANCH"
  printf '%s	HEAD
' "$CICD_AUR_CURRENT_SHA"
  exit 0
fi
if [ "$1" = "clone" ]; then
  mkdir -p "${!#}"
  exit 0
fi
if [ "$1" = "push" ]; then
  printf '%s
' "$*" >> "$CICD_GIT_PUSH_CALLS"
  exit 0
fi
exit 1
`)
		return fakeBinDir, ghCallsFile, gitCallsFile, gitPushCallsFile
	}

	const aurGitURL = "ssh://aur@aur.archlinux.org/widget-bin.git"
	const aurBranch = "master"
	const aurPreSHA, aurPostSHA = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	aurRollbackTargets := `[{"kind":"aur","git_url":"` + aurGitURL + `","branch":"` + aurBranch + `","pre_sha":"` + aurPreSHA + `","post_sha":"` + aurPostSHA + `"}]`

	t.Run("aur safe: current HEAD matches GoReleaser's push, force-pushes the pre-release SHA", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBinDir, ghCallsFile, _, gitPushCallsFile := newFakeGitAndGH(t, workspace, "", aurBranch, aurPostSHA)
		output, err := runBash(workspace, script, map[string]string{
			"PATH":                 fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":          workspace,
			"RELEASE_GH_TOKEN":     "release-token",
			"TAP_GH_TOKEN":         "tap-token",
			"AUR_SSH_PRIVATE_KEY":  "fake-private-key-contents",
			"ROLLBACK_TARGETS":     aurRollbackTargets,
			"TAG":                  "v1.2.3",
			"REPO":                 "acme/widget",
			"CICD_GH_CALLS":        ghCallsFile,
			"CICD_GIT_PUSH_CALLS":  gitPushCallsFile,
			"CICD_AUR_BRANCH":      aurBranch,
			"CICD_AUR_CURRENT_SHA": aurPostSHA,
		})
		if err != nil {
			t.Fatalf("AUR rollback with a safe compare-and-swap must succeed: %v\n%s", err, output)
		}
		pushCalls, err := os.ReadFile(gitPushCallsFile)
		if err != nil {
			t.Fatalf("expected the force-push restore call to run: %v\n%s", err, output)
		}
		if !strings.Contains(string(pushCalls), "push --force "+aurGitURL+" "+aurPreSHA+":refs/heads/"+aurBranch) {
			t.Fatalf("force-push did not restore the expected AUR ref: %s", pushCalls)
		}
		if !strings.Contains(string(output), "Restored "+aurGitURL) {
			t.Fatalf("rollback did not report the restored AUR repo:\n%s", output)
		}
	})

	t.Run("aur unsafe: concurrent push since GoReleaser's push, refuses to force-push", func(t *testing.T) {
		workspace := t.TempDir()
		const aurConcurrentSHA = "3333333333333333333333333333333333333333"
		fakeBinDir, ghCallsFile, _, gitPushCallsFile := newFakeGitAndGH(t, workspace, "", aurBranch, aurConcurrentSHA)
		output, err := runBash(workspace, script, map[string]string{
			"PATH":                 fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":          workspace,
			"RELEASE_GH_TOKEN":     "release-token",
			"TAP_GH_TOKEN":         "tap-token",
			"AUR_SSH_PRIVATE_KEY":  "fake-private-key-contents",
			"ROLLBACK_TARGETS":     aurRollbackTargets,
			"TAG":                  "v1.2.3",
			"REPO":                 "acme/widget",
			"CICD_GH_CALLS":        ghCallsFile,
			"CICD_GIT_PUSH_CALLS":  gitPushCallsFile,
			"CICD_AUR_BRANCH":      aurBranch,
			"CICD_AUR_CURRENT_SHA": aurConcurrentSHA,
		})
		if err == nil {
			t.Fatalf("AUR rollback must exit non-zero when it refuses a repo, to stay visibly red:\n%s", output)
		}
		if _, statErr := os.Stat(gitPushCallsFile); statErr == nil {
			pushCalls, _ := os.ReadFile(gitPushCallsFile)
			t.Fatalf("AUR rollback must NOT force-push a repo with a concurrent push, but called git push: %s", pushCalls)
		}
		if !strings.Contains(string(output), "::error::") {
			t.Fatalf("AUR rollback must report a concurrent-push refusal as an error:\n%s", output)
		}
		if !strings.Contains(string(output), aurPostSHA) || !strings.Contains(string(output), aurConcurrentSHA) {
			t.Fatalf("AUR rollback refusal must name both the expected and actual SHA so a human can reconcile it:\n%s", output)
		}
	})

	t.Run("aur: missing AUR_SSH_PRIVATE_KEY warns and errors without attempting SSH", func(t *testing.T) {
		workspace := t.TempDir()
		fakeBinDir, ghCallsFile, gitCallsFile, gitPushCallsFile := newFakeGitAndGH(t, workspace, "", aurBranch, aurPostSHA)
		output, err := runBash(workspace, script, map[string]string{
			"PATH":                 fakeBinDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"RUNNER_TEMP":          workspace,
			"RELEASE_GH_TOKEN":     "release-token",
			"TAP_GH_TOKEN":         "tap-token",
			"AUR_SSH_PRIVATE_KEY":  "",
			"ROLLBACK_TARGETS":     aurRollbackTargets,
			"TAG":                  "v1.2.3",
			"REPO":                 "acme/widget",
			"CICD_GH_CALLS":        ghCallsFile,
			"CICD_GIT_PUSH_CALLS":  gitPushCallsFile,
			"CICD_AUR_BRANCH":      aurBranch,
			"CICD_AUR_CURRENT_SHA": aurPostSHA,
		})
		if err == nil {
			t.Fatalf("AUR rollback without a key must exit non-zero (nothing was restored):\n%s", output)
		}
		if _, statErr := os.Stat(gitCallsFile); statErr == nil {
			calls, _ := os.ReadFile(gitCallsFile)
			t.Fatalf("AUR rollback must not invoke git at all without a key, but got: %s", calls)
		}
		if !strings.Contains(string(output), "::error::") || !strings.Contains(string(output), aurGitURL) {
			t.Fatalf("AUR rollback must name the repo it could not restore for lack of a key:\n%s", output)
		}
	})
}

// TestReleaseWorkflowResolvesVersionAgainstRealGitCliff runs the ACTUAL
// version-resolution pipeline release.yml drives -- "Resolve previous
// release tag" (real git), then the real `git-cliff` binary with the exact
// config and args "Compute next version from Git history" and "Prepare
// git-cliff configuration" build, then "Remove synthetic cold-start baseline
// tag", then "Resolve configured default bump" -- against four scratch
// repositories covering the cross product this PR must get right:
//
//	no tags + feat:/fix: commits      -> bootstrap the first release (Defect 2)
//	no tags + only chore:/ci: commits -> still decline (no auto-release leak)
//	tags + feat: commits since        -> bump exactly as today (must not regress)
//	tags + only ci: commits since     -> still decline, now with a notice (Defect 1)
//
// This is the Go-test encoding of the four-case table verified by hand
// against the same real git-cliff 2.13.1 this workflow pins (see the PR
// description for that manual run's pasted output). It is skipped, not
// failed, when git-cliff isn't on PATH: git-cliff is normally supplied by
// orhun/git-cliff-action inside the actual GitHub Actions job, not
// installed as a prerequisite for `go test`, so a runner without it must
// not fail the build over a tool this suite does not otherwise depend on --
// the other tests in this file cover the same shell contracts without it.
func TestReleaseWorkflowResolvesVersionAgainstRealGitCliff(t *testing.T) {
	if _, err := exec.LookPath("git-cliff"); err != nil {
		t.Skip("git-cliff not on PATH; skipping real-binary version-resolution test")
	}

	previousTagScript := releaseWorkflowRunBlock(t, "Resolve previous release tag")
	removeSyntheticTagScript := releaseWorkflowRunBlock(t, "Remove synthetic cold-start baseline tag")
	resolvedBumpScript := releaseWorkflowRunBlock(t, "Resolve configured default bump")

	cliffConfig := filepath.Join(t.TempDir(), "cicd-git-cliff.toml")
	if err := os.WriteFile(cliffConfig, []byte(`[bump]
initial_tag = "v0.0.0"
features_always_bump_minor = true
breaking_always_bump_major = false
[git]
commit_parsers = [{ message = "^(chore|ci|docs|refactor)(\\(.+\\))?:", skip = true }]
`), 0o644); err != nil {
		t.Fatal(err)
	}

	repoWithCommits := func(t *testing.T, messages []string, tagAfterFirstCommit string) string {
		t.Helper()
		dir := t.TempDir()
		run := func(args ...string) {
			t.Helper()
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
			}
		}
		run("init", "--initial-branch=main", "-q")
		run("config", "user.email", "release-test@example.invalid")
		run("config", "user.name", "Release Test")
		for i, message := range messages {
			run("commit", "--allow-empty", "-q", "-m", message)
			if i == 0 && tagAfterFirstCommit != "" {
				run("tag", tagAfterFirstCommit)
			}
		}
		return dir
	}

	// bumpedVersion reproduces "Compute next version from Git history"
	// exactly: same git-cliff version's binary, same --tag-pattern built
	// from PREFIX, same explicit range this step now always supplies.
	bumpedVersion := func(t *testing.T, repo, prefix, rng string) string {
		t.Helper()
		args := []string{"--config", cliffConfig, "--bumped-version", "--tag-pattern", "^" + prefix + `[0-9]+\.[0-9]+\.[0-9]+$`}
		if rng != "" {
			args = append(args, rng)
		}
		cmd := exec.Command("git-cliff", args...)
		cmd.Dir = repo
		output, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if exitErr, ok := err.(*exec.ExitError); ok {
				stderr = exitErr.Stderr
			}
			t.Fatalf("git-cliff: %v\n%s", err, stderr)
		}
		return strings.TrimSpace(string(output))
	}

	for _, tc := range []struct {
		name          string
		messages      []string
		tag           string // seeded on the first commit; "" means no tags at all
		wantNewTag    string // "" means declined -- no new_tag output
		wantNotice    bool
		wantColdStart string
	}{
		{
			name:          "no tags, feat/fix commits: bootstraps the first release",
			messages:      []string{"chore: bootstrap", "feat: add thing", "fix: fix thing"},
			tag:           "",
			wantNewTag:    "v0.1.0",
			wantNotice:    false,
			wantColdStart: "true",
		},
		{
			name:          "no tags, only chore/ci commits: still declines",
			messages:      []string{"chore: bootstrap", "ci: tweak pipeline"},
			tag:           "",
			wantNewTag:    "",
			wantNotice:    true,
			wantColdStart: "true",
		},
		{
			name:          "tagged, feat commit since: bumps exactly as today",
			messages:      []string{"chore: bootstrap", "feat: add thing"},
			tag:           "v0.5.0",
			wantNewTag:    "v0.6.0",
			wantNotice:    false,
			wantColdStart: "false",
		},
		{
			name:          "tagged, only ci commit since: still declines",
			messages:      []string{"chore: bootstrap", "ci: tweak pipeline"},
			tag:           "v0.5.0",
			wantNewTag:    "",
			wantNotice:    true,
			wantColdStart: "false",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := repoWithCommits(t, tc.messages, tc.tag)

			previousTagOutput := filepath.Join(t.TempDir(), "github-output")
			output, err := runBash(repo, previousTagScript, map[string]string{
				"PREFIX":        "v",
				"GITHUB_OUTPUT": previousTagOutput,
			})
			if err != nil {
				t.Fatalf("resolve previous release tag: %v\n%s", err, output)
			}
			latestTag := githubOutput(t, previousTagOutput, "latest_tag")
			previousVersion := githubOutput(t, previousTagOutput, "previous_version")
			rng := githubOutput(t, previousTagOutput, "range")
			coldStart := githubOutput(t, previousTagOutput, "cold_start")
			if coldStart != tc.wantColdStart {
				t.Fatalf("cold_start = %q, want %q", coldStart, tc.wantColdStart)
			}

			proposed := bumpedVersion(t, repo, "v", rng)
			t.Logf("resolved version for %q: %s (range=%q)", tc.name, proposed, rng)

			if output, err := runBash(repo, removeSyntheticTagScript, map[string]string{"PREFIX": "v"}); err != nil {
				t.Fatalf("remove synthetic cold-start baseline tag: %v\n%s", err, output)
			}

			resolvedBumpOutput := filepath.Join(t.TempDir(), "github-output")
			if err := os.WriteFile(resolvedBumpOutput, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			output, err = runBash(repo, resolvedBumpScript, map[string]string{
				"PROPOSED_VERSION": proposed,
				"PREFIX":           "v",
				"DEFAULT_BUMP":     "false",
				"LATEST_TAG":       latestTag,
				"PREVIOUS_VERSION": previousVersion,
				"COLD_START":       coldStart,
				"GITHUB_OUTPUT":    resolvedBumpOutput,
			})
			if err != nil {
				t.Fatalf("resolve configured default bump: %v\n%s", err, output)
			}

			if tc.wantNewTag == "" {
				content, readErr := os.ReadFile(resolvedBumpOutput)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(content) != 0 {
					t.Fatalf("new_tag output = %q, want no output written (decline)", content)
				}
			} else if got := githubOutput(t, resolvedBumpOutput, "new_tag"); got != tc.wantNewTag {
				t.Fatalf("new_tag = %q, want %q", got, tc.wantNewTag)
			}

			hasNotice := strings.Contains(string(output), "::notice title=No release published this run::")
			if hasNotice != tc.wantNotice {
				t.Fatalf("decline notice present = %v, want %v; output:\n%s", hasNotice, tc.wantNotice, output)
			}
		})
	}
}

func TestReadmeKeepsQuillSigningIncidentAndOwnershipVisible(t *testing.T) {
	contents, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(contents)
	for _, required := range []string{
		"https://github.com/strongo/cicd/issues/66",
		"quill is still the intended",
		"pre-publication",
		"codesign --verify --deep --strict --verbose=4",
		"consumer repository owns",
		"strongo/cicd owns",
	} {
		if !strings.Contains(readme, required) {
			t.Errorf("README.md does not keep quill-signing contract %q visible", required)
		}
	}
	if strings.Contains(readme, "Keep the release on Go 1.26, or override the Darwin build's linker flags") {
		t.Error("README.md still recommends the falsified Darwin linker workaround")
	}
}

func assertWorkflowFailure(t *testing.T, workspace, script string, variables map[string]string) {
	t.Helper()
	output, err := runBash(workspace, script, variables)
	if err == nil {
		t.Fatalf("published-artifact validation passed without executing an artifact:\n%s", output)
	}
}

func preparePublishedArtifactRunner(t *testing.T, workspace string) {
	t.Helper()
	script := publishedArtifactWorkflowRunBlock(t, "Prepare bounded process runner")
	if output, err := runBash(workspace, script, map[string]string{"RUNNER_TEMP": workspace}); err != nil {
		t.Fatalf("prepare bounded process runner: %v\n%s", err, output)
	}
}

func TestPublishedArtifactBoundedRunnerReapsForkedDescendants(t *testing.T) {
	workspace := t.TempDir()
	preparePublishedArtifactRunner(t, workspace)
	childPIDFile := filepath.Join(workspace, "child-pid")
	program := `python3 "$RUNNER_TEMP/cicd-run-with-timeout.py" 2 "$RUNNER_TEMP/timeout" bash -c '(while :; do :; done) & printf "%s\n" "$!" > "$CHILD_PID_FILE"; printf complete'`
	started := time.Now()
	output, err := runBash(workspace, program, map[string]string{
		"RUNNER_TEMP":    workspace,
		"CHILD_PID_FILE": childPIDFile,
	})
	if err != nil {
		t.Fatalf("bounded runner: %v\n%s", err, output)
	}
	if elapsed := time.Since(started); elapsed >= 1500*time.Millisecond {
		t.Fatalf("bounded runner waited %s after command leader exited\n%s", elapsed, output)
	}
	childPID := readPID(t, childPIDFile)
	t.Cleanup(func() { killProcess(childPID) })
	if processIsAlive(childPID) {
		t.Fatalf("bounded runner left child process %d alive", childPID)
	}
}

func TestPublishedArtifactWorkflowIsReadOnlyAndNonPublishing(t *testing.T) {
	workflow := readPublishedArtifactWorkflow(t)
	if !strings.Contains(workflow, "permissions:\n  contents: read\n") {
		t.Fatal("published-artifact reusable workflow must declare contents: read")
	}
	for _, forbidden := range []string{
		"contents: write",
		"goreleaser-action",
		"git tag",
		"git push",
		"brew install",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Fatalf("read-only published-artifact workflow contains publishing capability %q", forbidden)
		}
	}

	releaseWorkflow := readReleaseWorkflow(t)
	if strings.Contains(releaseWorkflow, "existing_artifact_tag") {
		t.Fatal("write-capable release workflow must not expose historical validation mode")
	}
	if !strings.Contains(releaseWorkflow, "contents: write   # required for GoReleaser to push GitHub releases") {
		t.Fatal("normal release workflow lost its required write permission")
	}
}

func TestReleaseWorkflowWatchdogsReturnPromptlyAndKillTimeouts(t *testing.T) {
	for i, watchdog := range releaseWorkflowTimeoutHelpers(t) {
		t.Run(fmt.Sprintf("watchdog_%d", i+1), func(t *testing.T) {
			workspace := t.TempDir()
			watchdogPIDs := watchdogProcessHarness(t, workspace)
			for attempt := 1; attempt <= 3; attempt++ {
				runImmediateCommand(t, workspace, watchdog, watchdogPIDs, "printf ready", 0, "ready")
				runImmediateCommand(t, workspace, watchdog, watchdogPIDs, "printf broken; exit 23", 23, "broken")
			}
			runParentExitWithBackgroundChild(t, workspace, watchdog, watchdogPIDs)

			timeoutProgram := strings.Join([]string{
				"set +e",
				watchdog,
				`run_with_timeout 1 bash -c 'trap "" TERM; (trap "" TERM; while :; do :; done) & printf '%s\n' "$!" > "$CHILD_PID_FILE"; while :; do :; done'`,
				"status=$?",
				`printf 'status=%s\n' "$status"`,
			}, "\n")
			started := time.Now()
			output, err := runBash(workspace, timeoutProgram, watchdogPIDs.environment())
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("timed command: %v\n%s", err, output)
			}
			if elapsed < time.Second || elapsed >= 5*time.Second {
				t.Fatalf("timed command completed in %s, want timeout plus TERM/KILL grace\n%s", elapsed, output)
			}
			if !strings.HasPrefix(string(output), "status=") || string(output) == "status=0\n" {
				t.Fatalf("timed command must be killed, got %q", output)
			}
			childPID := readPID(t, watchdogPIDs.childPIDFile)
			t.Cleanup(func() { killProcess(childPID) })
			if processIsAlive(childPID) {
				t.Fatalf("timed command's child process %d survived the process-group kill", childPID)
			}
			matches, err := filepath.Glob(filepath.Join(workspace, ".*_timed_out"))
			if err != nil || len(matches) != 1 {
				t.Fatalf("timeout marker = %v, %v; want one marker", matches, err)
			}
		})
	}
}

func runParentExitWithBackgroundChild(t *testing.T, workspace, watchdog string, harness watchdogHarness) {
	t.Helper()
	if err := os.Remove(harness.readyFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Remove(harness.childPIDFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	program := strings.Join([]string{
		"set +e",
		watchdog,
		`output="$(run_with_timeout 2 bash -c '(while :; do :; done) >/dev/null 2>&1 & printf "%s\n" "$!" > "$CHILD_PID_FILE"; while [ ! -f "$CICD_WATCHDOG_READY_FILE" ]; do :; done; printf leader-exited')"`,
		"status=$?",
		`printf 'status=%s output=%s\n' "$status" "$output"`,
	}, "\n")
	started := time.Now()
	output, err := runBash(workspace, program, harness.environment())
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("leader-exit command: %v\n%s", err, output)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("leader-exit command waited %s\n%s", elapsed, output)
	}
	if string(output) != "status=0 output=leader-exited\n" {
		t.Fatalf("leader-exit result = %q", output)
	}
	childPID := readPID(t, harness.childPIDFile)
	t.Cleanup(func() { killProcess(childPID) })
	if processIsAlive(childPID) {
		t.Fatalf("child process %d survived after its leader exited", childPID)
	}
	harness.assertAllExited(t)
}

func runImmediateCommand(t *testing.T, workspace, watchdog string, harness watchdogHarness, command string, wantStatus int, wantOutput string) {
	t.Helper()
	if err := os.Remove(harness.readyFile); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	program := strings.Join([]string{
		"set +e",
		watchdog,
		`output="$(run_with_timeout 2 bash -c 'while [ ! -f "$CICD_WATCHDOG_READY_FILE" ]; do :; done; exec bash -c "$CICD_IMMEDIATE_COMMAND"')"`,
		"status=$?",
		`printf 'status=%s output=%s\n' "$status" "$output"`,
	}, "\n")
	env := harness.environment()
	env["CICD_IMMEDIATE_COMMAND"] = command
	started := time.Now()
	output, err := runBash(workspace, program, env)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("immediate command: %v\n%s", err, output)
	}
	if elapsed >= 1500*time.Millisecond {
		t.Fatalf("immediate command waited %s for its 2s watchdog\n%s", elapsed, output)
	}
	want := fmt.Sprintf("status=%d output=%s\n", wantStatus, wantOutput)
	if string(output) != want {
		t.Fatalf("immediate command result = %q, want %q", output, want)
	}
	harness.assertAllExited(t)
}

type watchdogHarness struct {
	path          string
	pythonPIDFile string
	sleepPIDFile  string
	childPIDFile  string
	readyFile     string
	realPython    string
}

func watchdogProcessHarness(t *testing.T, workspace string) watchdogHarness {
	t.Helper()
	realPython, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	harness := watchdogHarness{
		path:          filepath.Join(workspace, "fake-bin"),
		pythonPIDFile: filepath.Join(workspace, "python-pids"),
		sleepPIDFile:  filepath.Join(workspace, "sleep-pids"),
		childPIDFile:  filepath.Join(workspace, "child-pid"),
		readyFile:     filepath.Join(workspace, "watchdog-ready"),
		realPython:    realPython,
	}
	if err := os.MkdirAll(harness.path, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(harness.path, "python3"), "#!/usr/bin/env bash\nprintf '%s\\n' \"$$\" >> \"$CICD_PYTHON_PID_FILE\"\ncase \"$*\" in *time.sleep*) touch \"$CICD_WATCHDOG_READY_FILE\" ;; esac\nexec \"$CICD_REAL_PYTHON\" \"$@\"\n")
	writeExecutable(t, filepath.Join(harness.path, "sleep"), "#!/usr/bin/env bash\nprintf '%s\\n' \"$$\" >> \"$CICD_SLEEP_PID_FILE\"\ntouch \"$CICD_WATCHDOG_READY_FILE\"\nexec /bin/sleep \"$@\"\n")
	return harness
}

func (h watchdogHarness) environment() map[string]string {
	return map[string]string{
		"RUNNER_TEMP":              filepath.Dir(h.path),
		"PATH":                     h.path + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CICD_REAL_PYTHON":         h.realPython,
		"CICD_PYTHON_PID_FILE":     h.pythonPIDFile,
		"CICD_SLEEP_PID_FILE":      h.sleepPIDFile,
		"CICD_WATCHDOG_READY_FILE": h.readyFile,
		"CHILD_PID_FILE":           h.childPIDFile,
	}
}

func (h watchdogHarness) assertAllExited(t *testing.T) {
	t.Helper()
	for _, pidFile := range []string{h.pythonPIDFile, h.sleepPIDFile} {
		for _, pid := range readPIDs(t, pidFile) {
			t.Cleanup(func() { killProcess(pid) })
			if processIsAlive(pid) {
				t.Fatalf("watchdog process %d from %s survived a completed command", pid, filepath.Base(pidFile))
			}
		}
	}
}

func writeExecutable(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, text := range strings.Fields(string(content)) {
		var pid int
		if _, err := fmt.Sscanf(text, "%d", &pid); err != nil {
			t.Fatalf("parse PID %q from %s: %v", text, path, err)
		}
		pids = append(pids, pid)
	}
	return pids
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	pids := readPIDs(t, path)
	if len(pids) != 1 {
		t.Fatalf("PIDs in %s = %v, want one", path, pids)
	}
	return pids[0]
}

func processIsAlive(pid int) bool {
	if output, err := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(pid)).Output(); err == nil && strings.HasPrefix(strings.TrimSpace(string(output)), "Z") {
		return false
	}
	return exec.Command("kill", "-0", fmt.Sprint(pid)).Run() == nil
}

func killProcess(pid int) {
	_ = exec.Command("kill", "-KILL", fmt.Sprint(pid)).Run()
}

func releaseWorkflowRunBlock(t *testing.T, stepName string) string {
	t.Helper()
	return namedWorkflowRunBlock(t, readReleaseWorkflow(t), stepName)
}

func publishedArtifactWorkflowRunBlock(t *testing.T, stepName string) string {
	t.Helper()
	return namedWorkflowRunBlock(t, readPublishedArtifactWorkflow(t), stepName)
}

func namedWorkflowRunBlock(t *testing.T, workflow, stepName string) string {
	t.Helper()
	needle := "      - name: " + stepName
	start := strings.Index(workflow, needle)
	if start == -1 {
		t.Fatalf("release workflow has no %q step", stepName)
	}
	return yamlRunBlock(t, workflow[start:])
}

func releaseWorkflowTimeoutHelpers(t *testing.T) []string {
	t.Helper()
	workflow := readReleaseWorkflow(t)
	const startMarker = "          run_with_timeout() {\n"
	const endMarker = "\n          }\n"
	var helpers []string
	for remaining := workflow; ; {
		start := strings.Index(remaining, startMarker)
		if start == -1 {
			break
		}
		remaining = remaining[start:]
		end := strings.Index(remaining, endMarker)
		if end == -1 {
			t.Fatal("unterminated run_with_timeout helper")
		}
		helper := remaining[:end+len(endMarker)]
		helper = strings.ReplaceAll(helper, "          ", "")
		helpers = append(helpers, helper)
		remaining = remaining[end+len(endMarker):]
	}
	if len(helpers) != 5 {
		t.Fatalf("found %d run_with_timeout helpers, want 5", len(helpers))
	}
	return helpers
}

func yamlRunBlock(t *testing.T, text string) string {
	t.Helper()
	const marker = "        run: |\n"
	start := strings.Index(text, marker)
	if start == -1 {
		t.Fatal("step has no bash run block")
	}
	text = text[start+len(marker):]
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, "          ") {
			break
		}
		lines = append(lines, strings.TrimPrefix(line, "          "))
	}
	return strings.Join(lines, "\n")
}

func readReleaseWorkflow(t *testing.T) string {
	t.Helper()
	workflow, err := os.ReadFile(releaseWorkflowPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(workflow)
}

func readPublishedArtifactWorkflow(t *testing.T) string {
	t.Helper()
	workflow, err := os.ReadFile(publishedArtifactWorkflowPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(workflow)
}

func runBash(dir, program string, variables map[string]string) ([]byte, error) {
	command := exec.Command("bash", "-c", program)
	command.Dir = dir
	command.Env = os.Environ()
	for key, value := range variables {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.CombinedOutput()
}

func githubOutput(t *testing.T, outputFile, key string) string {
	t.Helper()
	output, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if value, ok := strings.CutPrefix(line, key+"="); ok {
			return value
		}
	}
	t.Fatalf("%s missing from GITHUB_OUTPUT:\n%s", key, output)
	return ""
}
