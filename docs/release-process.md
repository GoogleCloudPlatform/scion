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
- Incrementing `N` publishes another build after fixes have been cherry-picked to the release
  branch.
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
| `release/vX.Y` | Stabilization branch for version X.Y | Until the release is end-of-life |
| `preview` | Points to the latest preview-channel release | Permanent; auto-updated by CI |
| `stable` | Points to the latest stable release | Permanent; auto-updated by CI |
| `nightly` | Points to the latest nightly build | Permanent; auto-updated by CI |

The channel branches are moving references for consumers and automation. Release tags are the
immutable identifiers for published builds.

## Release lifecycle

1. Develop and merge changes on `main`.
2. When the changes are ready for release testing, cut a preview from `main`. This creates the
   next `release/vX.Y` branch and its first `vX.Y.0-preview.1` tag.
3. Test the preview. Land fixes and cherry-pick the required commits to the release branch.
4. Bump the preview number to build and test each updated release candidate.
5. Once the latest preview is approved, promote it to stable. Promotion tags the exact tested
   release-branch commit without a pre-release suffix.

Development can continue on `main` while a release branch is stabilized.

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
a release line.

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
4. Creates a GitHub release and marks non-stable channels as pre-releases.
5. Force-updates the matching `stable`, `preview`, or `nightly` channel branch.
6. Updates the corresponding channel in `LATEST.json` on `main`.

Release-candidate tags route to the preview branch and preview manifest entry.

### Nightly scheduling workflow

[`build-nightly.yml`](../.github/workflows/build-nightly.yml) runs on a daily 02:00 UTC schedule
or by manual dispatch. It compares `main` with the latest nightly tag and pushes a new dated tag
only when new commits exist. That tag starts `build-release.yml`, which builds and publishes the
nightly release. After creating a new nightly tag, the workflow deletes nightly GitHub releases
and tags older than 14 days.

## Build matrix

The main `scion` binary is published for:

| Platform | Artifact base name |
|----------|--------------------|
| `linux/amd64` | `scion-linux-amd64` |
| `linux/arm64` | `scion-linux-arm64` |
| `darwin/amd64` | `scion-darwin-amd64` |
| `darwin/arm64` | `scion-darwin-arm64` |

Release artifacts are gzip-compressed tar archives. The Telegram, Discord, Slack, and Teams
plugins are built for Linux on both amd64 and arm64; plugin builds are not published for macOS.
