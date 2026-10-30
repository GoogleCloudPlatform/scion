# Release Process

Scion publishes releases through three channels. Each channel has an immutable tag for a
specific build and a long-lived branch that moves to the latest release in that channel.

| Channel | Intended use | Version example | Tracking branch |
|---------|--------------|-----------------|-----------------|
| Nightly | Bleeding-edge builds from `main` | `nightly-20260916` | `nightly` |
| Preview | Pre-release validation before stable promotion | `v0.3.0-preview.1` | `preview` |
| Stable | Production-ready releases | `v1.0.0` | `stable` |

Users select a channel by installing a release artifact whose tag belongs to that channel.
Automated consumers can follow the corresponding long-lived branch or read the channel entry
in [`LATEST.json`](../LATEST.json). Update checks remain within the installed version's channel:
stable versions check stable, preview and release-candidate versions check preview, and nightly
versions check nightly.

## Release channels

### Nightly

Nightly releases expose the latest changes on `main` for early testing.

- The nightly workflow runs daily at 02:00 UTC and can also be started manually.
- Tags use `nightly-YYYYMMDD`, such as `nightly-20260916`.
- A tag is created only when `main` has commits newer than the most recent nightly tag. If the
  current day's tag already exists, the workflow does not create another one.
- The `nightly` branch is force-updated by CI to the commit for the latest nightly build.
- Nightly GitHub releases and their tags are deleted after 14 days.

Nightly releases receive the same multi-architecture build outputs as tagged preview and stable
releases, but they have not passed the preview stabilization process.

### Preview

Preview releases are pre-release builds used to validate a prospective stable release. They are
built from a `release/vX.Y` branch rather than directly from a moving `main` branch.

- Tags use `vX.Y.Z-preview.N`, such as `v0.3.0-preview.1`.
- Every Friday, CI fast-forwards the release branch to the most recent nightly that passed CI and
  tags the next preview there (see [Weekly preview workflow](#weekly-preview-workflow)). `N` goes
  up each week until the line is promoted to stable.
- During a freeze before stable, fixes are cherry-picked to the release branch and previews are
  tagged by hand with `bump-preview.sh`.
- Release-candidate tags use `vX.Y.Z-rc.N` and are treated as preview-channel releases.
- GitHub marks preview and release-candidate releases as pre-releases.
- The `preview` branch is force-updated by CI to the commit for the latest preview or release
  candidate.

### Stable

Stable releases are production-ready builds promoted from a tested preview.

- Tags use `vX.Y.Z`, such as `v1.0.0`.
- Promotion removes the `-preview.N` suffix from the latest preview tag and creates the stable
  tag on the same commit.
- The `stable` branch is force-updated by CI to the commit for the latest stable release.

## Branch taxonomy

| Branch | Purpose | Lifetime |
|--------|---------|----------|
| `main` | Development branch | Permanent |
| `release/vX.Y` | Release line for version X.Y; rolled forward to a nightly each week | Until the release is end-of-life |
| `preview` | Points to the latest preview-channel release | Permanent; auto-updated by CI |
| `stable` | Points to the latest stable release | Permanent; auto-updated by CI |
| `nightly` | Points to the latest nightly build | Permanent; auto-updated by CI |

The channel branches are moving references for consumers and automation. Release tags are the
immutable identifiers for published builds.

## Release lifecycle

1. Develop and merge changes on `main`. Nightlies are tagged from `main` every day.
2. Cut a new release line from `main` with `cut-preview.sh`. This creates the next
   `release/vX.Y` branch and its first `vX.Y.0-preview.1` tag.
3. Every Friday, the weekly preview workflow fast-forwards `release/vX.Y` to the most recent
   nightly that passed CI and tags the next `vX.Y.0-preview.N`. The release branch is not frozen
   when it is cut; it follows `main` one tested nightly at a time.
4. To stabilize before stable, freeze the line: land fixes on `main` and cherry-pick them to the
   release branch. A cherry-pick makes the branch diverge from `main`, so the weekly workflow
   stops fast-forwarding it and fails loudly instead. During the freeze, tag each candidate with
   `bump-preview.sh`.
5. Once the latest preview is approved, promote it to stable with `promote-stable.sh`. Promotion
   tags the exact tested release-branch commit without a pre-release suffix. Then cut the next
   release line.

## Operator runbook

Run release scripts from the repository root with an `origin` remote that points to the release
repository. Each script fetches the latest remote branches and tags, prints the operation it will
perform, and requests confirmation before creating or pushing a tag. Use `--dry-run` to inspect
the selected branch, version, tag, and commit without creating or pushing release refs.

### Cut a new preview

```bash
./scripts/release/cut-preview.sh [--dry-run]
```

[`cut-preview.sh`](../scripts/release/cut-preview.sh) must be run while `main` is checked out. It:

1. Fetches branches and tags from `origin`.
2. Finds the highest `release/vX.Y` remote branch and selects the next minor version. If no
   release branch exists, the initial version is `v0.3`.
3. Creates `release/vX.Y` from the current `main` HEAD.
4. Creates `vX.Y.0-preview.1` at that commit.
5. Pushes the branch and tag to `origin`. The tag push starts the release build workflow.

### Weekly preview roll-forward

The [weekly preview workflow](#weekly-preview-workflow) normally runs on its own. To run it by
hand, for example to release a specific nightly:

```bash
gh workflow run build-preview-weekly.yml -R GoogleCloudPlatform/scion
gh workflow run build-preview-weekly.yml -R GoogleCloudPlatform/scion -f nightly_tag=nightly-20261022
```

A `nightly_tag` must match `nightly-YYYYMMDD` and exist. It skips the CI guard, but the
fast-forward and tag rules still apply. The run summary records which nightly was picked and why.

To preview the selection locally without changing anything, fetch first and then run the
read-only selection script:

```bash
git fetch origin --tags '+refs/heads/release/*:refs/remotes/origin/release/*'
./scripts/release/select-weekly-preview.sh [--nightly-tag nightly-YYYYMMDD] [--skip-ci-check]
```

The script uses `gh api` for the CI lookup. Its tests run with
`./scripts/release/test-select-weekly-preview.sh`.

### Bump an existing preview

```bash
./scripts/release/bump-preview.sh [--dry-run] [release-branch]
```

[`bump-preview.sh`](../scripts/release/bump-preview.sh) accepts a branch such as
`release/v0.3`. If the branch is omitted, it selects the highest versioned release branch on
`origin`. It:

1. Fetches branches and tags, then checks out and pulls the selected release branch.
2. Finds the highest `vX.Y.Z-preview.N` tag for that release line.
3. Increments `N` and tags the release branch HEAD.
4. Pushes the new tag to `origin`, starting the release build workflow.

The script requires an existing preview tag. Use `cut-preview.sh` to create the first preview for
a release line. Use this script to tag previews during a freeze, after cherry-picks have stopped the
weekly fast-forward.

### Promote a preview to stable

```bash
./scripts/release/promote-stable.sh [--dry-run] [release-branch]
```

[`promote-stable.sh`](../scripts/release/promote-stable.sh) also selects the highest versioned
release branch when no branch is provided. It:

1. Fetches branches and tags from `origin`.
2. Finds the highest preview tag for the selected release line.
3. Verifies that the preview tag and the remote release branch HEAD point to the same commit.
4. Removes the `-preview.N` suffix to derive the stable `vX.Y.Z` tag.
5. Verifies that the stable tag does not already exist, then creates and pushes it.

The commit equality check prevents promotion when fixes on the release branch have not yet been
published and tested as a preview. If it fails, publish another preview before promoting.

## Version manifest

[`LATEST.json`](../LATEST.json) at the repository root records the latest release in each channel.
The release workflow updates the relevant entry after publishing a release and commits the change
to `main`. Other channel entries are left unchanged.

The canonical raw manifest URL is:

```text
https://raw.githubusercontent.com/GoogleCloudPlatform/scion/main/LATEST.json
```

The manifest has this structure:

```json
{
  "channels": {
    "stable":  { "version": "v1.0.0", "date": "...", "url": "..." },
    "preview": { "version": "v0.3.0-preview.2", "date": "...", "url": "..." },
    "nightly": { "version": "nightly-20260916", "date": "...", "url": "..." }
  }
}
```

Each channel entry contains:

| Field | Meaning |
|-------|---------|
| `version` | Latest tag in the channel |
| `date` | Release time as a UTC timestamp |
| `url` | GitHub release page for the tag |

Release-candidate tags update the `preview` entry. Before a channel has a published release, its
fields may be empty strings.

## Version checking

`scion version --check` fetches the manifest, detects the channel from the installed version, and
reports whether that channel contains a newer release. It does not move an installation from one
channel to another. Development builds that do not identify a release channel do not perform an
update comparison.

The Go package [`pkg/version/update`](../pkg/version/update/) provides `CheckForUpdate()` for
programmatic use. It supplies the same manifest retrieval, channel detection, and version
comparison used by the CLI. The hub server can use this package to provide automated update
notifications without implementing a separate release-discovery path.

## CI/CD architecture

### Tagged release workflow

[`build-release.yml`](../.github/workflows/build-release.yml) runs when a stable, preview,
release-candidate, or nightly tag is pushed. It:

1. Builds the web UI and cross-compiles Scion for all supported platforms.
2. Builds the Linux plugin binaries.
3. Packages and uploads the build artifacts.
4. Generates a `SHA256SUMS` checksums file covering every published asset.
5. Creates a GitHub release and marks non-stable channels as pre-releases.
6. Force-updates the matching `stable`, `preview`, or `nightly` channel branch.
7. Updates the corresponding channel in `LATEST.json` on `main`.

Release-candidate tags route to the preview branch and preview manifest entry.

### Nightly scheduling workflow

[`build-nightly.yml`](../.github/workflows/build-nightly.yml) runs on a daily 02:00 UTC schedule
or by manual dispatch. It compares `main` with the latest nightly tag and pushes a new dated tag
only when new commits exist. That tag starts `build-release.yml`, which builds and publishes the
nightly release. After creating a new nightly tag, the workflow deletes nightly GitHub releases
and tags older than 14 days.

### Weekly preview workflow

[`build-preview-weekly.yml`](../.github/workflows/build-preview-weekly.yml) runs every Friday at
14:00 UTC, after that day's nightly, or by manual dispatch with an optional `nightly_tag` input.
The selection logic lives in
[`select-weekly-preview.sh`](../scripts/release/select-weekly-preview.sh). The workflow:

1. Picks the highest `release/vX.Y` branch, comparing versions numerically. It fails if none
   exists.
2. Picks a nightly. Without `nightly_tag`, it starts at the newest nightly tag that the release
   branch does not already contain. **CI guard:** it reads the `Build & Test` check run on that
   nightly's commit and walks back through older nightlies until it finds one whose conclusion is
   `success`. Failed, pending and missing checks are skipped. It never picks a nightly the branch
   already contains. If none of the newer nightlies passed, it logs a warning and tags nothing.
3. Skips, with "no new nightly since last preview", when the chosen commit is already on the
   release branch.
4. Fails when the release branch head is not an ancestor of the chosen commit, i.e. the branch
   has commits that are not on `main`. It never force-pushes. This is the normal state during a
   freeze; use `bump-preview.sh` until the line is promoted.
5. Computes the next tag from the highest `vX.Y.Z-preview.N` for the line: the highest patch `Z`
   first, then the highest `N` within it, both compared numerically, or `vX.Y.0-preview.1` if
   there is none. It fails if that tag exists or if that `vX.Y.Z` has already been promoted to
   stable.
6. Pushes the fast-forward of `release/vX.Y` and the new tag in one atomic push (a plain push,
   which the remote rejects unless the branch update is a fast-forward).
7. Calls `build-release.yml` with the tag, because tags pushed with `GITHUB_TOKEN` do not start
   other workflows. The build publishes the pre-release, moves the `preview` branch and updates
   `LATEST.json`.

## Build matrix

The main `scion` binary is published for:

| Platform | Artifact base name |
|----------|--------------------|
| `linux/amd64` | `scion-linux-amd64` |
| `linux/arm64` | `scion-linux-arm64` |
| `darwin/amd64` | `scion-darwin-amd64` |
| `darwin/arm64` | `scion-darwin-arm64` |
| `windows/amd64` | `scion-windows-amd64` |
| `windows/arm64` | `scion-windows-arm64` |

Linux and macOS artifacts are gzip-compressed tar archives (`.tar.gz`) containing `scion`. Windows
artifacts are zip archives (`.zip`) containing `scion.exe`. The Telegram, Discord, Slack, and Teams
plugins are built for Linux on both amd64 and arm64; plugin builds are not published for macOS.

## Checksums

Every release publishes a `SHA256SUMS` asset alongside its binaries and plugin tarballs,
covering every other asset in that release. It is generated from the final set of build
artifacts with `sha256sum -- * | sort -k2`, so it always matches exactly what was
published, including for nightly and preview builds.

`scripts/single-node-vm/deploy.sh` downloads and verifies this file automatically before
installing anything (see [Single-Node VM Deployment Guide](deploy/single-node-vm.md#quick-start)),
as does the Hub's own binary auto-updater. To verify a manual download yourself:

```bash
curl -fsSLO https://github.com/GoogleCloudPlatform/scion/releases/download/<TAG>/SHA256SUMS
curl -fsSLO https://github.com/GoogleCloudPlatform/scion/releases/download/<TAG>/scion-linux-amd64.tar.gz
sha256sum -c --ignore-missing SHA256SUMS
```

A release published before this feature shipped has no `SHA256SUMS` asset.
