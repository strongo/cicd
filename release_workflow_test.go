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

func TestReleaseWorkflowUsesExactGoReleaserQuillPreflight(t *testing.T) {
	workflow := readReleaseWorkflow(t)
	const quillVersion = "v0.0.0-20260630015114-8310f3e9a321"
	if strings.Contains(workflow, "anchore/quill") {
		t.Fatal("macOS signing preflight must not download the unrelated anchore/quill tool")
	}
	for _, required := range []string{
		"  macos_signing_preflight:\n",
		"if: ${{ inputs.require_notarized_macos }}",
		"runs-on: macos-latest",
		"go-version: '1.27'",
		"version: v2.18.0",
		"go mod edit -go=1.27 -require=github.com/goreleaser/quill@\"${QUILL_VERSION}\"",
		"GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build",
		"github.com/goreleaser/quill/quill",
		"github.com/goreleaser/quill/quill/pki/load",
		"QUILL_VERSION: " + quillVersion,
		`load.P12("env:MACOS_SIGN_P12", os.Getenv("MACOS_SIGN_PASSWORD"))`,
		"NewSigningConfigFromP12",
		"WithTimestampServer(\"http://timestamp.apple.com/ts01\")",
		"quill.Notarize",
		"codesign --verify --deep --strict --verbose=4",
		"spctl -a -t install -vv",
		"source=Notarized Developer ID",
		"needs: macos_signing_preflight",
	} {
		if !strings.Contains(workflow, required) {
			t.Fatalf("release workflow is missing exact GoReleaser/quill preflight contract %q", required)
		}
	}
	preflightIndex := strings.Index(workflow, "  macos_signing_preflight:\n")
	tagGuardIndex := strings.Index(workflow, "Guard signing secret policy before tagging")
	tagIndex := strings.Index(workflow, "Determine and push guarded tag")
	if tagGuardIndex < 0 || tagIndex < 0 || tagGuardIndex > tagIndex {
		t.Fatal("signing secret guard must run before the guarded tag step")
	}
	if preflightIndex > tagIndex {
		t.Fatal("macOS signing preflight must be declared before the guarded tag step")
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
			t.Fatalf("latest_tag = %q, want empty", got)
		}
		if got := githubOutput(t, outputFile, "previous_version"); got != "0.0.0" {
			t.Fatalf("previous_version = %q, want 0.0.0", got)
		}
		// No previous tag means the first release: git-cliff must fall back
		// to processing full history, same as before this change, so the
		// range passed to it must be empty rather than a bogus "..HEAD".
		if got := githubOutput(t, outputFile, "range"); got != "" {
			t.Fatalf("range = %q, want empty for the initial release", got)
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

	t.Run("stays silent when default_bump is disabled and nothing was proposed", func(t *testing.T) {
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
	})

	t.Run("handles the very first release with no previous tag", func(t *testing.T) {
		outputFile := filepath.Join(t.TempDir(), "github-output")
		output, err := runBash(workspace, script, map[string]string{
			"PROPOSED_VERSION": "v0.1.0",
			"PREFIX":           "v",
			"DEFAULT_BUMP":     "false",
			"LATEST_TAG":       "",
			"PREVIOUS_VERSION": "0.0.0",
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
	})
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

func TestREADMEGo127DarwinGuidanceUsesMacOS13Floor(t *testing.T) {
	contents, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(contents)
	start := strings.Index(readme, "### Go 1.27 and Darwin signing")
	if start < 0 {
		t.Fatal("README has no Go 1.27 Darwin signing guidance")
	}
	section := readme[start:]
	if end := strings.Index(section, "## Keep the pin fresh with Renovate"); end >= 0 {
		section = section[:end]
	}
	if !strings.Contains(section, "macOS 13 or newer") {
		t.Fatal("Go 1.27 Darwin guidance must state the macOS 13 minimum")
	}
	if !strings.Contains(section, "-macos=13.0 -macsdk=13.0") {
		t.Fatal("Go 1.27 Darwin example must use macOS 13 deployment target and SDK")
	}
	for _, forbidden := range []string{"-macos=12.0", "-macsdk=12.1", "macOS 12"} {
		if strings.Contains(section, forbidden) {
			t.Fatalf("Go 1.27 Darwin guidance contains unsupported compatibility value %q", forbidden)
		}
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
	if len(helpers) != 4 {
		t.Fatalf("found %d run_with_timeout helpers, want 4", len(helpers))
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
