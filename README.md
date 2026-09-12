# strongo/cicd

Shared CI/CD for Go repositories across the `strongo`, `dal-go`, `sneat-co`,
`ingitdb`, and `bots-go-framework` orgs: reusable GitHub **workflows** and a
composite **action** that run `get`, `vet`, `build`, `test`, `lint`, optional
coverage, and automatic SemVer tagging.

> **Renamed:** this repo was `strongo/go-ci-action`. GitHub redirects the old
> path, so existing `uses: strongo/go-ci-action/...` references keep working.
> New references should use `strongo/cicd`; the Renovate preset below migrates
> the old name for you.

## What's here

| File | Kind | Purpose |
| --- | --- | --- |
| `.github/workflows/workflow.yml` | Reusable workflow (`workflow_call`) | Full Go CI: lint, test (+coverage), build, and version bump. The primary entry point. |
| `.github/workflows/release.yml` | Reusable workflow (`workflow_call`) | GoReleaser release flow (tag + `goreleaser release`). |
| `.github/workflows/validate-published-artifact.yml` | Reusable workflow (`workflow_call`) | Read-only, exact-tag validation of already published CLI archives. |
| `action.yml` | Composite action | Single-job CI for callers that want CI steps inline in their own job. |
| `default.json` | Renovate preset | Shareable config consumers `extends` to auto-track this repo's tag (see below). |

## Recommended usage: pin to a tag, not `@main`

Pinning `@main` means one bad commit to the shared workflow breaks **every**
consumer's CI at the same time — there is no blast-radius firebreak. Pin to a
version tag instead:

```yaml
jobs:
  ci:
    uses: strongo/cicd/.github/workflows/workflow.yml@v1.14.15   # exact release tag
    secrets:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

**Pin an exact release — `@vX.Y.Z`.** Pin an immutable tag and let
**Renovate** open a PR to bump it (see below). Each bump runs through your own CI
before merging, giving you a per-repo firebreak and an audit trail.

`@main` still works and stays supported for backward compatibility, but is
discouraged: it silently takes whatever last landed upstream.

> **The moving `@v1` tag has been retired.** It was a lightweight tag the
> maintainer was expected to advance to the latest backward-compatible release.
> In practice it went unadvanced and drifted 14 patch releases behind, while
> still *accepting* inputs added after it — so `require_notarized_macos: true`
> was accepted without error by a copy of `release.yml` that did not contain the
> `macos_signing_preflight` job implementing it. Consumers believed they had a
> release gate; nothing ran. Two CLIs shipped macOS binaries that the kernel
> killed on launch.
>
> A moving tag that is only as fresh as someone remembering to move it gives the
> appearance of a pin without the guarantee. Pin an exact tag instead.

## CI cache policy

The reusable Go workflow restores a single cache of Go modules and compiled Go
packages in both parallel jobs. Only **Build & test** may save a missing cache
key, and only after a successful build and test run. This avoids a cold-cache
race in which both jobs attempt to reserve and upload the same key.

The key is determined before Go commands run, from the runner OS/architecture,
Go 1.26, CGO setting, and the selected module's `go.sum`; the matching primary
key is also used for the sole save. The reusable workflow enables the separate
`golangci-lint-action` analysis cache by default. Callers can set
`golangci_lint_cache: false` when cache transfer is slower than linting for a
particular repository. `golangci_lint_cache_invalidation_interval` controls how
many days a lint-cache namespace remains reusable and defaults to the action's
seven-day policy.

The composite action keeps that linter cache disabled; it has a separate input
surface and is not part of this reusable-workflow timing policy. It uses `go
mod download all` rather than `go get ./...`, so CI never rewrites a consumer's
dependency requirements.

## Reuse exact pull-request validation after merge

`workflow.yml` can reuse successful same-repository pull-request validation
when the exact tested tree lands on `main`. This is disabled by default. An
opted-in pull-request run publishes a small GitHub Actions artifact only after
both **Lint** and **Build & test** succeed. A subsequent `main` run verifies:

- the landed commit names exactly one merged, same-repository pull request;
- the receipt belongs to exactly one successful run of the same caller workflow;
- the artifact's GitHub-reported SHA-256 digest matches its downloaded bytes;
- repository, pull-request identity, landed Git tree, reusable-workflow
  revision, every non-secret workflow-call input, and both required job
  conclusions match exactly.

The publisher also requires its checkout's `HEAD` to equal GitHub's trusted
`${{ github.sha }}` before recording the actual checkout SHA. GitHub's run and
job APIs expose the pull-request head SHA rather than the temporary synthetic
merge SHA after that ref expires, so the landed `HEAD^{tree}` comparison is the
reusable security binding. The recorded checkout SHA remains audit evidence;
it is not treated as independently recoverable evidence after GitHub deletes
the temporary merge commit.

The merge run starts the moment auto-merge fires -- when the required checks
pass -- while the receipt is published by a job that only runs *after* them.
The resolver therefore waits, up to 90 seconds, whenever the evidence is
visibly still arriving: a pull-request run for that head which has not
concluded yet, or a successful run whose receipt has not appeared. Nothing
else waits. A direct push, a failed run, a tree mismatch and every other
settled refusal is decided on the first look, so a genuine miss never costs
the merge run more than the resolver's own runtime.

(Measured on sneat-co/sneat-go before this existed: receipt artifact created
at 22:11:13, pull-request run concluded at 22:11:18, resolver asked at
22:11:21 and was told there were no successful runs. The optimization was
losing a race with the very run it depends on, on every merge.)

Missing permissions, API failures, forks, expired artifacts, changed policy,
tree drift, and ambiguous runs all select the existing full validation path.
Only an explicit verified receipt skips lint, vet, tests, and coverage. The
Build & test job still checks out `main`, installs Go, runs `build_command`, and
uploads the configured artifact, so binaries that embed `${GITHUB_SHA}` and
deploy workflows that require the main-run artifact keep their current contract.

Opt in only from a caller that runs this reusable workflow for both
`pull_request` and pushes to `main`:

```yaml
permissions:
  actions: read
  contents: write
  pull-requests: read

jobs:
  ci:
    uses: strongo/cicd/.github/workflows/workflow.yml@<exact-release-containing-this-feature>
    with:
      reuse_exact_tree_validation: true
    secrets:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

`contents: write` remains required by this reusable workflow's existing,
statically declared version-bump job even when a caller disables bumping.
`actions: read` and `pull-requests: read` let the main preflight authenticate
the prior run and its receipt. Receipt retention defaults to seven days and is
configurable with `validation_receipt_retention_days`; expiry safely causes a
full revalidation.

## Skip revalidation on main after a green pull request

Exact-tree reuse only fires when the landed tree is byte-identical to the
validated one. With merge commits that happens only while `main` has not moved
since the pull request was last updated, so in an active repository the merge
run almost always revalidates the same code for a second time before the deploy
can start.

`validate_on_main: false` addresses that directly. The push-to-`main` run then
skips **Lint** and the test and coverage steps, and only builds and uploads the
artifact, provided the resolver can prove that the landed commit is the merge
commit of exactly one same-repository pull request whose head this same workflow
already concluded successfully. A direct push to `main`, an unmatched or
unmerged pull request, an ambiguous set of runs, and any API or resolver failure
all keep the full validation.

```yaml
jobs:
  ci:
    uses: strongo/cicd/.github/workflows/workflow.yml@<exact-release-containing-this-feature>
    with:
      validate_on_main: false
    secrets:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

The main-run resolver authenticates the prior run through the API, so the
caller needs the same `actions: read` and `pull-requests: read` permissions
listed for exact-tree reuse above.

This is a weaker guarantee than exact-tree reuse, deliberately: the merged tree
itself was never linted or tested. `build_command` still compiles the merged
tree on `main`, so a merge that does not compile is still caught before the
artifact is published, but a *semantic* conflict — the pull request renames a
function while `main` adds a caller, and each side is green alone — reaches the
artifact and would have been caught by a second validation. Only enable it where
`main` is protected and this workflow is a required pull-request check, and
prefer requiring branches to be up to date before merging: that makes the merge
tree equal to the validated tree, and exact-tree reuse then applies with its
full guarantee.

The two inputs compose. With both set, a matching receipt wins and reports exact
reuse; ordinary tree drift after `main` moves falls through to the delegated
path instead of revalidating. Either input alone enables the main-run resolver;
`validate_on_main` participates in the policy digest, so changing it invalidates
existing receipts rather than silently reusing validation performed under a
different policy.

## Private Go modules from multiple owners

Set `GOPRIVATE` to the private module prefixes and `goprivate_git_hosts` to the
matching Git URL prefixes that may receive `GH_TOKEN`. The latter accepts
comma- or newline-separated `host/owner` entries:

```yaml
jobs:
  ci:
    uses: strongo/cicd/.github/workflows/workflow.yml@v1.14.15
    with:
      GOPRIVATE: github.com/sneat-co,github.com/sneat-games
      goprivate_git_hosts: |
        github.com/sneat-co
        github.com/sneat-games
    secrets:
      GH_TOKEN: ${{ secrets.PRIVATE_MODULES_TOKEN }}
```

Each plural entry must include an owner or narrower repository path. A
host-wide entry such as `github.com` is rejected, preventing the private-module
token from being attached to unrelated GitHub fetches. The same inputs are
available on `release.yml`.

`goprivate_git_host` remains available for existing single-prefix callers. Its
historical `github.com` default is preserved for backward compatibility, but
new and migrated callers should use the scoped plural input.

For private modules in another organization, prefer a short-lived GitHub App
installation token over extending a user token across organizations. The App
must have only `Contents: read` and must be installed only on the repositories
listed by the caller. Each parallel job mints and revokes its own token:

```yaml
jobs:
  ci:
    uses: strongo/cicd/.github/workflows/workflow.yml@<exact-release-containing-this-feature>
    with:
      GOPRIVATE: github.com/sneat-co,github.com/sneat-dev
      goprivate_git_hosts: github.com/sneat-co
      goprivate_github_app_client_id: ${{ vars.WORKBENCH_GITHUB_OAUTH_CLIENT_ID }}
      goprivate_github_app_owner: sneat-dev
      goprivate_github_app_repositories: workbench-gh-app
    secrets:
      GH_TOKEN: ${{ secrets.SNEAT_CI_READWRITE_TOKEN }}
      GOPRIVATE_GITHUB_APP_PRIVATE_KEY: ${{ secrets.WORKBENCH_GITHUB_APP_PRIVATE_KEY }}
```

The installation and minted token are both repository-scoped. The workflow
also fixes the token permission to `Contents: read`; it never requests write
access for dependency downloads. The private key stays a secret and is not
included in exact-tree validation receipts.

When GitHub App authentication is enabled, `GH_TOKEN` remains available only
for the explicit prefixes in `goprivate_git_hosts`; the legacy host-wide
`goprivate_git_host` default is not installed alongside the App token. Exact
App repository credentials take priority when an explicit legacy prefix also
covers that owner. Both the bare repository URL and its `.git` form match;
similarly named repositories do not receive the App token.

## Releasing with `release.yml`

`release.yml` runs the GoReleaser flow: checkout (full history) → setup-go →
optional auto-tag → `goreleaser release --clean` against **your repo's own**
`.goreleaser.yaml`. Two trigger styles are supported:

- **Push to `main`** — git-cliff calculates the next version from conventional
  commits and the shared workflow releases the new tag (continuous delivery).
- **Push a `vX.Y.Z` tag** — the auto-bump step is skipped (the tag already fixes
  the version) and GoReleaser releases that exact tag. Use this for an explicit,
  human-gated "cut a release by pushing a tag" flow.

Publishers that push to **other** repos (Homebrew, Scoop, WinGet, AUR) need
credentials the default `GITHUB_TOKEN` can't provide. Pass them as optional
secrets; GoReleaser reads only the ones your config references:

```yaml
on:
  push:
    tags: ['v*']
permissions:
  contents: write
jobs:
  release:
    uses: strongo/cicd/.github/workflows/release.yml@v1.14.15
    with:
      go_version: '1.26.5'                 # optional; defaults to '1.26'
      # goreleaser_extra_args: '--skip=chocolatey,snapcraft'  # optional
    secrets:
      GORELEASER_GITHUB_TOKEN: ${{ secrets.MY_GORELEASER_PAT }}   # brew/scoop/winget
      WINGET_GITHUB_TOKEN:     ${{ secrets.WINGET_GITHUB_TOKEN }}  # optional, if separate
      AUR_SSH_PRIVATE_KEY:     ${{ secrets.AUR_SSH_PRIVATE_KEY }}  # optional
```

Reference the forwarded credentials in `.goreleaser.yaml` as
`{{ .Env.GORELEASER_GITHUB_TOKEN }}`, `{{ .Env.WINGET_GITHUB_TOKEN }}`, and
`{{ .Env.AUR_SSH_PRIVATE_KEY }}`.

Publishers that need a **different runner or extra tooling** this ubuntu job
lacks — Chocolatey (`choco`, Windows-only), Snapcraft (`snapcraft`), or native
macOS signing (`xcrun`/`codesign`) — cannot run here. Keep them as a small
per-repo job (`needs:` this one) and add `--skip=chocolatey,snapcraft` via
`goreleaser_extra_args` so this job doesn't try to run them.

## Gate the release on your quality workflow (`require_workflow_success`)

**The incident this exists for (2026-09):** `datatug-cli` published `v0.13.2`
from a commit whose `go build ./...` was red. Two Renovate pull requests for
the same `go-github` v90→v91 bump interleaved, leaving `go.mod` requiring v91
while every source file still imported v90. `Go CI` went red and said so. The
release shipped six binaries and a Homebrew cask anyway, because
GoReleaser's `go mod tidy` before-hook silently re-added v90 in the runner. The
published binaries were built against **v90** while the tagged `go.mod`
declared **v91**.

`release.yml` is a **sibling** of your quality workflows, not a dependant. The
same push starts both, and GitHub has no cross-workflow `needs:`, so a red test
suite cannot stop a release by itself. Name the workflow that must be green:

```yaml
jobs:
  release:
    permissions:
      contents: write
      actions: read          # required: this reads another workflow's result
    uses: strongo/cicd/.github/workflows/release.yml@v1.18.0
    with:
      require_workflow_success: 'Go CI'   # the workflow's `name:`, not its filename
      # Several, comma-separated, are allowed and ALL must be green:
      #   require_workflow_success: 'Go CI, Integration tests'
      # require_workflow_success_timeout_seconds: 1800   # optional, default 30 min
```

Behaviour, all of it fail-closed:

| Situation | Result |
|---|---|
| Required workflow concluded `success` (or `skipped`) | release proceeds |
| Concluded `failure`, `cancelled`, `timed_out` | **refused before any tag is cut** |
| Still running | waited for, then refused on timeout |
| Name matches no workflow in the repo | **refused** — a typo must not quietly disable the gate |
| Several workflows named, any one not green | **refused**, and the error names every one that was not green |
| Workflow produced no run for this commit (`paths:` filtered it out) | proceeds, with a notice |

The guard waits, because your quality workflow usually starts on the same push
as the release. The newest run for the commit decides, so re-running a red
suite can clear the gate. Leaving the input unset keeps the previous
behaviour exactly.

Note the `actions: read` permission. A reusable workflow cannot grant itself
more than its caller allows, so it must be added on **your** calling job. Public
repositories already permit the read; private ones fail closed with a message
naming this line.

## Post-release artifact smoke test

**The incident this exists for (2026-07):** `specscore-cli` v0.24.0 released
clean — every CI check green — but the Homebrew-cask-installed binary was
silently blocked by macOS Gatekeeper on every invocation for every user. CI
had tested the source, built it, and even uploaded the release artifact; it
had never *run* the thing it published. A local `curl` + run of the exact
same release asset worked instantly, which is what made this so easy to
ship: the bug lived entirely in the install path, not the binary.

`release.yml` now downloads and exercises what it just published, in three
layers, all under `artifact_smoke_test` (default **on**):

1. **Run the published GitHub release asset.** Downloads the archive for
   each platform in `artifact_smoke_test_platforms` (default: linux/amd64 on
   `ubuntu-latest`, darwin/arm64 on `macos-latest`; matched by the
   `*_<os>_<arch>.{tar.gz,tgz,zip}` archive extension, never a
   same-named checksum/signature/SBOM sidecar), extracts it, and runs
   `<binary> --version` (configurable via `artifact_smoke_test_command`, for
   a CLI whose default invocation doesn't support a bare `--version`) under
   an explicit watchdog (`artifact_smoke_test_timeout_seconds`, default
   30s) — a hang is killed and reported as a clear failure, never an
   eventual multi-hour job timeout. Once the binary is found and executed,
   this is **always a hard failure** if it hangs, exits non-zero, or prints
   nothing. This proves the artifact is a working binary; it does **not**
   prove a Homebrew-cask user can run it — a plain download carries no
   `com.apple.quarantine` attribute, so this layer alone passed for
   v0.24.0.
2. **Assert macOS code signing / notarization** (`codesign -dv`,
   `spctl -a -vv -t install`) against the same darwin binary — static
   inspection, no execution, no Gatekeeper risk in the check itself. Uses
   assessment type `install`, not the default `execute`/`exec`: a bare CLI
   binary is not an app bundle, so the default type rejects even a
   genuinely notarized one (measured directly against a real notarized
   `ingitdb-cli` binary — see the "macOS notarization" section below). It
   also checks the output for `source=Notarized Developer ID`, since exit 0
   alone only proves Developer ID signing, not notarization. This is what
   actually would have caught v0.24.0: an ad-hoc signature with no
   Developer ID.
3. **`brew install --cask` and run the installed binary**
   (`artifact_smoke_test_homebrew_cask`, default on) — auto-detected from
   this repo's own `homebrew_casks:` config, no-op otherwise. The only layer
   that reproduces Homebrew's quarantine attribute, i.e. the only layer that
   actually reproduces the incident end-to-end.

**"Could not test" is never "tested and broken."** This workflow is reused
by repos not enumerated here, including libraries and Docker-only releases
with no per-arch archive, so a naming/shape mismatch must never be read as
"the release is broken." Every layer distinguishes the two explicitly: no
matching release asset, an unrecognized archive format, no binary found
inside it, an unresolvable binary name (see the `tag_prefix` note below), or
a failed `brew tap`/`brew install --cask` (transient Homebrew/network issue,
tap-visibility timing, a cask-not-found mismatch) all log `::warning::` and
are **skipped**, unconditionally — never a failure, regardless of
`require_notarized_macos`. Only "the binary was found and actually executed,
and it hung / exited non-zero / printed nothing" fails a release: always for
layer 1, and (once installed) gated by `require_notarized_macos` for layers
2/3. This split is what makes shipping `artifact_smoke_test: true` by
default safe as well as meaningful — a default-off check would have
prevented nothing, but a default-on check that can't tell "untestable" from
"broken" would have started failing releases it never should have.

Every network step that isn't already bounded by the watchdog above
(`gh release download`, `brew tap`/`brew install --cask`) has its own
timeout too (120s / 300s respectively), plus each smoke-test job has a
`timeout-minutes` well under GitHub's 6-hour default job timeout — the same
"a hang must fail fast and legibly, never eventually" rationale extended to
every step that talks to a network, not just the binary invocation itself.

**Layers 2 and 3 currently only warn, by design.** No org consumer currently
has a verified notarized release path. A default hard-fail would red every
unsigned CLI release on day one and just get disabled fleet-wide instead of
fixed. These layers log a `::warning::` naming exactly what's missing (for
example, an ad-hoc signature, `TeamIdentifier: not set`, or `spctl` rejection)
so the gap stays visible. Set `require_notarized_macos: true` per repository
only after the exact published path has passed the proof gate below. A valid
Developer ID signature and Apple notarization remain the intended end state,
not an optional extra.

```yaml
jobs:
  release:
    uses: strongo/cicd/.github/workflows/release.yml@v1.14.15
    with:
      # All of the below are optional; shown at their defaults.
      # artifact_smoke_test: true
      # artifact_smoke_test_binary: ''                 # '' infers from .goreleaser.y*ml / repo name
      # artifact_smoke_test_command: '--version'
      # artifact_smoke_test_timeout_seconds: 30
      # artifact_smoke_test_homebrew_cask: true
      # require_notarized_macos: false                 # flip once this repo is actually notarized
    secrets: { ... }
```

Callers get layer 1 automatically (hard-fail) and layers 2/3 automatically
(warn-only) the next time they bump their pinned `strongo/cicd` tag — no other
config changes required. A repo whose binary name the inference
guesses wrong (multi-binary repos only check the first `builds[]` entry;
repos where the executable doesn't match `project_name` or a `-cli`-stripped
repo name) should set `artifact_smoke_test_binary` explicitly. A repo that
doesn't publish a runnable CLI binary at all should set
`artifact_smoke_test: false`.

## Revalidating an existing published artifact

Use `validate-published-artifact.yml` when an already published CLI archive
needs to be executed again without creating a tag or release. This is separate
from `release.yml` because reusable-workflow permissions can only stay the same
or become more restrictive: a `contents: read` validation caller cannot call a
workflow containing a `contents: write` release job, even when that job would be
conditionally skipped.

The validation workflow has only `contents: read`, requires the exact release
tag and executable name, downloads archives only from that tag, and hard-fails
unless every configured platform reaches and successfully executes the
published binary. It has no GoReleaser, tag creation, publishing, or Homebrew
cask path.

```yaml
permissions:
  contents: read

jobs:
  validate:
    uses: strongo/cicd/.github/workflows/validate-published-artifact.yml@v1.14.15
    permissions:
      contents: read
    with:
      release_tag: v0.33.0
      artifact_binary: my-cli
      artifact_command: --version
```

Binary-name inference reads `.goreleaser.y*ml`/`goreleaser.y*ml` at the repo
**root**. A repo using `tag_prefix` for a subdirectory module (e.g.
`"ingitdb/v"`, see "Automatic version tagging" below) whose GoReleaser
config lives in that subdirectory instead won't be found there — inference
deliberately does **not** fall back to guessing from the repo name in that
specific case (a subdirectory module's name isn't the repo's name), so the
whole smoke test is skipped with a warning instead of testing, or failing
on, an unverified guess. Set `artifact_smoke_test_binary` explicitly to
enable it for that shape of repo. (No current consumer of this workflow
combines a subdirectory `tag_prefix` with this fallback path — checked
directly against every one at the time this was written — but the workflow
is reused by repos not enumerated here, so this stays defensive.)

**Known residual gap (confirmed empirically, not assumed):** GitHub-hosted
macOS runners have no interactive user session, so the real, *indefinite*
"Apple could not verify…" Gatekeeper dialog a logged-in user hits has
nowhere to render. Layer 2 (static `codesign`/`spctl` inspection) is
unaffected by this — it never executes the binary. But layer 3 (`brew
install --cask` + run) was tested directly against the actual quarantined,
ad-hoc-signed specscore-cli v0.24.0 Homebrew-cask binary on a real
GitHub-hosted `macos-latest` runner, and it did **not** hang: the process
completed after a ~30s Gatekeeper assessment delay and exited 0 with valid
output — right at the edge of the default `artifact_smoke_test_timeout_seconds`.
So **layer 3's pass/fail is not reliable evidence of what a real interactive
user experiences on this runner type** — a pass there does not mean
Homebrew-cask users aren't blocked. Layer 2 is the reliable, deterministic
signal; treat layer 3 as corroborating only. This is exactly why layer 2 is
positioned as the primary gate.

## Packaging conventions (apply to every product)

These are ecosystem-wide `.goreleaser.yaml` standards so all our CLIs package
and update identically. New repos MUST follow them; existing repos are migrated
as they're touched.

### Homebrew: cask, not formula

Use `homebrew_casks:` — **not** the deprecated `brews:` — in `.goreleaser.yaml`.
Decided 2026-07-17; applied to `ingitdb-cli` and `specscore-cli`.

- **Why.** We ship prebuilt binaries, not source; casks are Homebrew's home for
  prebuilt artifacts, and GoReleaser has deprecated `brews:` (it emits a warning
  and will be removed). `goreleaser check` fails on `brews:` in current versions.
- **Install command becomes** `brew install --cask <tap>/<name>` (the
  tap-qualified form also resolves without `--cask`, so it's a soft change).
- **Linux tradeoff — accepted.** Homebrew casks are macOS-only; Linux users
  install via our `curl … | sh` script (or `go install`), not `brew`, so
  dropping the Linux-brew path costs us nothing.
- **Cask fidelity limits.** GoReleaser's cask schema has no `install`/`test`
  hook, so a per-manifest `--version` smoke test can't be carried over. The tap
  gains a `Casks/` tree; a leftover `Formula/<name>.rb` stops updating and can be
  pruned once.
- **Self-update gotcha.** If the CLI has a self-update path that detects
  Homebrew installs, it MUST treat `/Caskroom/` as Homebrew-managed: Apple
  Silicon casks live under `/opt/homebrew/Caskroom/…` but Intel casks under
  `/usr/local/Caskroom/…`, which matches no other Homebrew marker.

### macOS signing and notarization ownership

strongo/cicd owns the reusable signing orchestration: the GoReleaser and
signer version pins, optional Apple-secret forwarding, the pre-publication
proof gate, retained signing evidence, and the rule that decides when public
release mutation may begin. The consumer repository owns its product-specific
`.goreleaser.yml`, binary/build IDs, cask metadata, repository secret mapping,
and the decision to opt into the proven signing mode.

The cross-platform path uses quill inside GoReleaser: it reads a Developer ID
Application `.p12`, embeds a signature in each Mach-O binary, and submits that
binary to Apple's notarization service while the main release job remains on
Ubuntu. This is the desired architecture; quill is still the intended
destination once its exact output is proven runnable. Native `codesign` and
`notarytool` on a macOS runner remain the fallback architecture if the
cross-platform signer cannot satisfy the same contract.

Until the proof exists, consumers ship the existing ad-hoc-signed cask path
and may retain GoReleaser's documented post-install quarantine-removal hook.
A dormant `notarize.macos` example is not release evidence and must not be
copied into a production consumer as though it were a verified reference.

### Go 1.27 quill incident and current containment

WB CLI v0.66.1 exposed the failure tracked by
[issue #66](https://github.com/strongo/cicd/issues/66). GoReleaser v2.18.0
reported both signing and successful notarization for its Go 1.27 darwin/arm64
binary, but the published executable failed
`codesign --verify --deep --strict --verbose=4` with an invalid embedded
signature and macOS killed every invocation with status 137.

Do not attribute that incident to Go 1.27's Darwin deployment metadata. The
working WB CLI v0.66.0 control was also built with Go 1.27 and carried the same
`LC_BUILD_VERSION` values (`minos 13.0`, SDK `26.2`); its linker-provided ad-hoc
signature verified and it executed normally. The discriminating change was
enabling the quill signing path. The defect has not yet been isolated to a
specific line inside quill, so linker overrides are not an approved remedy.

WB temporarily disables that signing path and retains its proven cask
quarantine-removal fallback. Re-enable it only after issue #66 closes with all
of this evidence against one exact candidate:

1. A non-public Go 1.27 pilot uses the same certificate, notary credentials,
   GoReleaser version, quill version, and workflow route as production.
2. Before any tag, GitHub Release, or cask mutation, a clean macOS runner runs
   deep strict `codesign`, `spctl -a -t install -vv` and checks for
   `source=Notarized Developer ID`, then executes `<binary> --version` and
   requires exit 0 with non-empty output.
3. The receipt retains the exact candidate digest, toolchain and signer
   versions, certificate identity metadata, notarization result, and macOS
   verification output.
4. Only after that gate passes may the consumer enable the signer and set
   `require_notarized_macos: true`.

The existing post-release layers remain valuable defense in depth, but they
cannot substitute for this gate: once publication has happened, they are an
alarm rather than a fence.

## Keep the pin fresh with Renovate

Add the shared preset to a consumer repo's `renovate.json` so Renovate keeps the
`strongo/cicd` reference current — and rewrites any lingering
`strongo/go-ci-action` reference onto a pinned `strongo/cicd@vX.Y.Z` — automatically:

```json
{
  "$schema": "https://docs.renovatebot.com/renovate-schema.json",
  "extends": [
    "config:recommended",
    "github>strongo/cicd"
  ]
}
```

The preset (`default.json` in this repo):

- Groups and auto-updates the `strongo/cicd` reusable-workflow / action ref,
  advancing `@vX.Y.Z` pins as releases are cut. This is what replaces the retired
  moving `@v1` tag: the bump arrives as a reviewable PR rather than silently.
- Auto-merges those bumps **through a PR gated by your CI**, so a broken release
  fails your build and blocks the merge — the firebreak — instead of landing
  silently.
- Replaces legacy `strongo/go-ci-action` references with `strongo/cicd@vX.Y.Z`,
  automating the rename find-and-replace.

Override anything you like (e.g. disable `automerge`) in your own `renovate.json`
after the `extends`.

## Automatic version tagging

### Conventional pull request title gate

The reusable workflow enforces a conventional pull request title in its
required `Lint` job by default:

```yaml
jobs:
  strongo_workflow:
    uses: strongo/cicd/.github/workflows/workflow.yml@v1.20.1
```

Pull request titles must use
`<type>(<optional-scope>)<optional-!>: <description>`. The failure names the
invalid title, accepted types and examples, explains the release consequence,
and prints an exact `gh pr edit` command. Pushes to `main` and tag workflows are
unchanged.

Callers with a different title policy can explicitly opt out:

```yaml
    with:
      require_conventional_pr_title: false
```

Use `fix:`, `feat:`, `perf:`, or a breaking `!` type when a runtime change must
produce a release. Syntactically valid `docs:`, `chore:`, `ci:`, `test:`, or
`refactor:` changes may intentionally produce no release when `default_bump` is
`false`.

On a push/merge to `main`, the `go_bump` job (workflow) / tag step (action) uses
[git-cliff] to calculate a new SemVer version from **conventional commits since
the last matching tag**. The shared workflow then applies its guard and pushes
the tag with Git:

We deliberately use git-cliff rather than semantic-release or an npm-oriented
tag action. It is a language-neutral Git-history tool: callers need neither
`package.json` nor any project-specific release metadata. It only proposes a
version; this repository's explicit guard remains the authority that creates a
tag, preserving our v0 policy and `default_bump` contract.

| Commit type since last tag | Result |
| --- | --- |
| `fix:` | patch bump (`v1.2.3 → v1.2.4`) |
| `feat:` | minor bump (`v1.2.3 → v1.3.0`) |
| `feat!:` / `BREAKING CHANGE:` | major **only if** `allow_major_version_bump: true`; otherwise **capped to a minor** bump (see guard below) |
| docs/chore/ci/refactor only | `default_bump` decides (see below) |

### Accidental-major guard (`allow_major_version_bump`)

git-cliff is configured to make a `feat!:` / `BREAKING CHANGE:` commit a
**minor** bump while the current major is zero. The shared guard independently
caps a proposed major bump when `allow_major_version_bump` is false. This keeps
a pre-1.0 module on v0 unless a maintainer deliberately cuts v1.

The reusable `workflow.yml` (`go_bump`), `release.yml` (CD bump path), and the
composite `action.yml` all guard against this: they compute a proposed version
without writing a tag, and
when `allow_major_version_bump` is `false` (the default) a bump that would raise
the **major** version is **capped to a minor** bump of the previous version
(`v0.64.2 → v0.65.0`; `v1.5.0 → v1.6.0`) with a `::warning::` in the log. Pre-1.0
this is exactly right — a breaking change is a minor bump until you deliberately
cut `v1.0.0`. To intend a real major, either set `allow_major_version_bump: true`
or push an explicit `vX.0.0` tag (a tag push bypasses the bump entirely).

### Release-history requirements

git-cliff derives the bump from the commit range `lastTag..HEAD`. Two
things must be true for it to see your `feat:`/`fix:` commits:

1. **Full history and tags.** The reusable workflows check out with
   **`fetch-depth: 0`**, and the composite action now fetches complete history
   and tags before calculating. With only the default shallow clone, git-cliff sees
   `HEAD`'s message —
   so a `Merge pull request #N …` merge commit (which is never
   conventional-commit-shaped) produced **no bump and no tag**, even when the
   merged branch was full of `feat:`/`fix:` commits. This was the cause of
   releases needing hand-cut tags. Composite-action callers no longer need to
   configure a special checkout depth themselves.

2. **Conventional commits reach `main`.** Choose one, and set it in your repo's
   **Settings → General → Pull Requests**:
   - **Squash merge + "Default to PR title" for the squash commit message**, and
     write conventional PR titles (`feat: …`, `fix: …`). The single squashed
     commit is then conventional. *(Recommended — simplest and most robust.)*
   - **Merge commits**, with conventional commits on your branches. `fetch-depth:
     0` lets the action read them behind the merge commit.

    Either way, enabling a linear-history / conventional-PR-title convention makes
    tagging deterministic.

### Cold start: a never-tagged repo's first release

A repo with **no matching tag anywhere in history** now bootstraps its first
release automatically, gated on the exact same rule as every other release:
only when the history actually contains a releasing commit type (`feat:`,
`fix:`, a breaking change) under the configured commit parsers. A repo whose
entire history is `chore:`/`ci:`/`docs:`/`refactor:` (or has no commits at
all) still publishes nothing — the bootstrap never overrides `default_bump`
or forces a release git-cliff itself found nothing to cut. Internally,
`release.yml`'s "Resolve previous release tag" step gives git-cliff a
synthetic, local-only baseline tag to diff against (never pushed, deleted
before GoReleaser runs) instead of the empty range it used to pass, which
made git-cliff silently report its configured `initial_tag` verbatim
regardless of what history actually contained.

### Declined releases are no longer silent

Every run that decides **not** to cut a tag — whatever the reason (no
releasing commit since the last tag, an already-tagged commit, or a
never-tagged repo with nothing to bootstrap) — now emits a `::notice::`
naming the proposed version, the baseline it was compared against, the
`default_bump` setting, and which of those caused the decline. GitHub
surfaces it on the run's Annotations panel regardless of which step emitted
it, so a no-op merge is legible from the run summary without opening logs.

### `default_bump` input

Controls what happens when **no** commit since the last tag implies a bump
(docs/chore/ci-only changes):

- Reusable `workflow.yml`: **`default_bump: 'patch'`** by default (a push/merge to
  `main` always cuts at least a patch tag — preserves prior behaviour). Set
  `default_bump: 'false'` to tag *only* on `feat:`/`fix:`/breaking commits.
- Composite `action.yml`: **`default_bump: 'false'`** by default (tags only on
  conventional commits). Set to `'patch'`/`'minor'`/`'major'` to always bump.

## Releasing this repo (maintainers)

Tags are cut automatically by this repo's own CI (`v1.x.y`). To publish or advance
the **moving `v1`** major tag after a release lands on `main`:

```bash
git fetch --tags origin
# point v1 at the newest v1.x.y release (or origin/main)
git tag -f v1 "$(git tag -l 'v1.*.*' | sort -V | tail -1)"
git push -f origin v1
```

Advance `v1` only to releases you've verified are backward-compatible.

<!-- dev-approach:v1 -->
## Our approach to development

We build with our own tooling:

- **[SpecScore](https://specscore.md)** — specify requirements as `SpecScore.md` artifacts
- **[SpecStudio](https://specscore.studio)** — author & manage specs across their lifecycle
- **[inGitDB](https://ingitdb.com)** — store structured data in Git where applicable
- **[DALgo](https://dalgo.io)** — data access layer for Go
- **[cover100.dev](https://cover100.dev)** — drive toward 100% test coverage
- **[DataTug](https://datatug.io)** — query & explore data
<!-- /dev-approach -->

[`git-cliff`]: https://git-cliff.org/
