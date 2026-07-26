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
| `action.yml` | Composite action | Single-job CI for callers that want CI steps inline in their own job. |
| `default.json` | Renovate preset | Shareable config consumers `extends` to auto-track this repo's tag (see below). |

## Recommended usage: pin to a tag, not `@main`

Pinning `@main` means one bad commit to the shared workflow breaks **every**
consumer's CI at the same time — there is no blast-radius firebreak. Pin to a
version tag instead:

```yaml
jobs:
  ci:
    uses: strongo/cicd/.github/workflows/workflow.yml@v1   # moving major tag
    secrets:
      GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Two pinning styles are supported:

- **`@v1` — moving major tag (recommended default).** A lightweight tag that the
  maintainer advances deliberately to the latest backward-compatible release.
  You automatically get fixes without opening a PR, but a bad release only
  reaches you when `v1` is advanced (not on every push to `main`).
- **`@v1.x.y` — exact release (maximum control).** Pin an immutable release and
  let **Renovate** open a PR to bump it (see below). Each bump runs through your
  own CI before merging, giving you a full per-repo firebreak and an audit trail.

`@main` still works and stays supported for backward compatibility, but is
discouraged for the reasons above.

> Existing `@main` consumers are **not** being mass-migrated. Adopt `@v1` (or the
> Renovate preset) gradually, per repo, on your own schedule.

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
    uses: strongo/cicd/.github/workflows/release.yml@v1
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
   `spctl -a -vv`) against the same darwin binary — static inspection, no
   execution, no Gatekeeper risk in the check itself. This is what actually
   would have caught v0.24.0: an ad-hoc signature with no Developer ID.
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

**Layers 2 and 3 currently only warn, by design.** As of this writing,
*nothing this org publishes is notarized* — Developer ID signing and App
Store Connect notarization aren't wired into the shared pipeline yet. A
default hard-fail here would red every CLI's release on day one and just get
disabled fleet-wide instead of fixed. They log a `::warning::` naming
exactly what's missing (e.g. ad-hoc signature, `TeamIdentifier: not set`,
`spctl` rejection) on every release so the gap stays visible. Set
`require_notarized_macos: true` per repo the moment its macOS build is
genuinely signed + notarized, to make that guarantee binding. This is the
intended end state, not an optional extra.

```yaml
jobs:
  release:
    uses: strongo/cicd/.github/workflows/release.yml@v1
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

Existing `@v1` callers get layer 1 automatically (hard-fail) and layers 2/3
automatically (warn-only) the next time the maintainer advances the `v1`
tag — no config changes required. A repo whose binary name the inference
guesses wrong (multi-binary repos only check the first `builds[]` entry;
repos where the executable doesn't match `project_name` or a `-cli`-stripped
repo name) should set `artifact_smoke_test_binary` explicitly. A repo that
doesn't publish a runnable CLI binary at all should set
`artifact_smoke_test: false`.

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

### macOS notarization: opt-in, dormant by default

Ship unsigned macOS binaries by default. Wire notarization as a `notarize.macos`
block whose `enabled` is gated on the signing secret
(`{{ isEnvSet "MACOS_SIGN_P12" }}`), so with the secret unset it is skipped and
can never break a release. `release.yml` forwards five optional secrets into
GoReleaser's env for exactly this: `MACOS_SIGN_P12`, `MACOS_SIGN_PASSWORD`
(Developer ID Application cert + password), and `NOTARIZE_ISSUER_ID`,
`NOTARIZE_KEY_ID`, `NOTARIZE_KEY` (App Store Connect API key). GoReleaser's
notarize publisher is pure Go (quill), so it runs on this same ubuntu job —
no macOS runner needed. Enabling it is a deliberate per-repo step: verify the
Apple credentials are current, wire the five secrets above into the calling
workflow's `secrets:` block (see `ingitdb-cli`'s `release.yml` for a worked
example), then flip that repo's `require_notarized_macos` input (see
"Post-release artifact smoke test" above) to true once its darwin artifacts
actually pass `spctl -a -vv`.

## Keep the pin fresh with Renovate

Add the shared preset to a consumer repo's `renovate.json` so Renovate keeps the
`strongo/cicd` reference current — and rewrites any lingering
`strongo/go-ci-action` reference onto `strongo/cicd@v1` — automatically:

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
  advancing `@v1.x.y` pins (and following the moving `@v1`) as releases are cut.
- Auto-merges those bumps **through a PR gated by your CI**, so a broken release
  fails your build and blocks the merge — the firebreak — instead of landing
  silently.
- Replaces legacy `strongo/go-ci-action` references with `strongo/cicd@v1`,
  automating the rename find-and-replace.

Override anything you like (e.g. disable `automerge`) in your own `renovate.json`
after the `extends`.

## Automatic version tagging

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
