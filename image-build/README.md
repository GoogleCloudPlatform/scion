# Image Build

Dockerfiles and build configurations for Scion container images.

`image-build/` is focused on Scion-owned base and server images. Harness-specific
images are recipes attached to Harness-config bundles under `harnesses/<name>/`.
The build scripts can still build those harness images for a workstation or a
private registry, but the catalog is the source of truth for their Dockerfiles.

## Image Hierarchy

```
core-base          System dependencies (Go, Node, Python)
  └── scion-base   Adds sciontool binary and scion user
        ├── harness images  Optional recipes from harnesses/<name>/
        └── hub             Scion hub server

thick-prep         Patches Cloud Workstations base for scion compatibility,
                   including git >= 2.48 (amd64 only)
  └── scion-base   Same Dockerfile, different foundation
        ├── harness images
        └── hub
```

`core-base/`, `scion-base/`, and `hub/` live under `image-build/`. Harness images
build from self-contained bundles under `harnesses/<name>/` when that bundle has
a `Dockerfile` and `cloudbuild.yaml`. See
[`harnesses/README.md`](../harnesses/README.md).

### Where git comes from

Scion hard-requires **git >= 2.48.0** (`pkg/util/git.go` `CheckGitVersion`, for
`git worktree add --relative-paths`). Below that, worktree-per-agent mode is
disabled.

`scion-base` does **not** install git — it only builds the Go binaries — so git
is always inherited from whichever foundation is underneath it. Both foundations
therefore have to provide it independently:

| Foundation | Base OS | glibc | git |
|---|---|---|---|
| `core-base` | `node:24-trixie-slim` (Debian 13) | 2.41 | vendored from `chainguard/git` |
| `thick-prep` | Cloud Workstations base (Ubuntu 24.04) | 2.39 | vendored from `chainguard/git` |

Both copy the same four artifacts out of the Chainguard image. Two constraints
apply to that copy and are enforced by a build-time assertion in each Dockerfile:

- helpers **must** land in `/usr/libexec/git-core` — that path is compiled into
  the binary as `GIT_EXEC_PATH`, and getting it wrong produces a misleading
  `'remote-https' is not a git command`;
- the binary **must** land at `/usr/bin/git`, overwriting any distro git.
  Chainguard ships `/usr/libexec/git-core/git` as a relative symlink to
  `../../bin/git`, and git re-execs itself through that path for internal
  subcommands. On a base that already has its own `/usr/bin/git` (the thick
  base has 2.43), installing ours elsewhere leaves that symlink resolving to
  the **old** binary — `git --version` reports 2.55.0 while subcommands die
  with `fatal: unknown repository extension found: relativeworktrees`;
- the base image needs **glibc >= 2.38**. Debian bookworm (2.36) fails at exec
  with `GLIBC_2.38 not found`, so this gates any future base-image change.

`GIT_IMAGE` defaults to the floating `chainguard/git:latest`. Pin it to a digest
in CI (`--build-arg GIT_IMAGE=chainguard/git@sha256:...`) and refresh on a
schedule — a vendored binary stops receiving Chainguard's rebuild cadence the
moment it is copied out.

## Scripts

All image-related scripts live under `scripts/`. GitHub Actions workflows remain in `.github/workflows/` per GitHub convention.

| Script | Purpose |
|--------|---------|
| `scripts/build-images.sh` | Orchestrator. Build images via a pluggable backend (`--builder`). |
| `scripts/build-release-images.sh` | Build the image set from a release tag's specs. Standalone; see [Build images for a release](#build-images-for-a-release). |
| `scripts/builders/*.sh` | Backend adapters (local-docker, local-podman, cloud-build). |
| `scripts/lib/targets.sh` | Target → step list resolution. Single source of truth for the build DAG. |
| `scripts/trigger-cloudbuild.sh` | Deprecation shim. Forwards to `build-images.sh --builder cloud-build`. |
| `scripts/pull-containers.sh` | Pull pre-built images (auto-detects runtime). |
| `scripts/setup-cloud-build.sh` | One-time GCP setup (APIs, Artifact Registry, permissions). |
| `scripts/check-harness-coverage.sh` | Fails if a `harnesses/<name>/Dockerfile` is missing from an aggregate `cloudbuild-*.yaml`. |
| `scripts/check-image-size.sh` | Reports the size growth of a candidate image over a baseline; fails above a MiB budget. See [Image size budget](#image-size-budget). |
| `.github/workflows/build-images.yml` | GitHub Actions workflow for building and pushing images. |

### Image size budget

`scripts/check-image-size.sh <baseline> <candidate>` compares the
uncompressed sizes of two images in the local store, prints both and the delta,
and exits 1 when the candidate grew by more than the budget. With docker it
sums the layer sizes from `docker history`; it does not use `image inspect`'s
`.Size`, which with docker's containerd image store depends on how the image
entered the store (larger than the layer sum for a local build, about the
compressed size for a pull). With podman (used
when docker is absent, or with `IMAGE_TOOL=podman`) it reads
`podman image inspect`'s `.Size`, because `podman history` prints sizes only
in human-readable form. If `docker` is podman's docker shim (podman-docker),
set `IMAGE_TOOL=podman`.

```bash
# Build core-base before and after a change, then:
image-build/scripts/check-image-size.sh core-base:before core-base:after
image-build/scripts/check-image-size.sh --budget-mib 300 \
  core-base:before core-base:after
```

The default budget (`DEFAULT_BUDGET_MIB` in the script, overridable with
`IMAGE_SIZE_BUDGET_MIB` or `--budget-mib`, at most 6 digits) is 550 MiB: the
measured 471.8 MiB that core-base's Playwright WebKit and Firefox system
libraries add (docker layer sum, core-base on Debian 13, amd64, built
2026-10-31, playwright-core 1.62.1's list), plus about 15%. The script is not
wired into Cloud Build: doing so would mean pulling a baseline image inside the
pipeline.

### Playwright browser libraries

core-base installs the system libraries Playwright's WebKit and Firefox builds
need (`install-core-toolchain.sh playwright-deps`), because agents run as
non-root and cannot `apt-get install` them. The browsers themselves are not
baked in; `npx playwright install webkit firefox` fetches the revision that
matches each project's Playwright version. The package list is Playwright's
own for the base's distro release; the script names the Playwright version it
came from and how to regenerate it. Only Debian 13 (core-base) has a list, so
thick-prep's Ubuntu 24.04 base does not get these libraries.

### Builders

`build-images.sh` selects an execution backend with `--builder <name>`. Three are bundled:

| Builder | Backend | Multi-arch | Push behavior |
|---|---|---|---|
| `local-docker` (default) | `docker buildx` | yes (auto-promotes to `--push`) | honors `--push`; `--load` otherwise |
| `local-podman` | `podman build` | single-arch by default; multi-arch errors out (manual QEMU setup required) | honors `--push`; built images live in the local store automatically |
| `cloud-build` | `gcloud builds submit` against a static `cloudbuild-*.yaml` (group targets) or a config generated on the fly (individual harness targets) | always amd64+arm64 (server-side) | always pushes |

The orchestrator owns target sequencing, tag computation, and BASE_IMAGE threading. Each builder only knows how to execute one image build (per-image mode) or one target submission (target mode).

### Targets

| Target | What gets built | Notes |
|---|---|---|
| `core-base` | `core-base` | Foundation tools layer. |
| `scion-base` | `scion-base` | Adds sciontool. Uses existing `core-base:<tag>`. |
| `harnesses` | All catalog harness images with `harnesses/<name>/Dockerfile` | Uses existing `scion-base:<tag>`. Builds recipes from the root Harness-config catalog. |
| `hub` | `scion-hub` | Hub server image. Uses existing `scion-base:<tag>`. |
| `omni` | `scion-omni` | Single-node deployment image: selected harnesses (claude, codex, opencode, antigravity, grok-build) chained into one image with embedded web UI. amd64 only. |
| `common` (default) | `scion-base` + catalog harnesses + hub | Skips `core-base`. Most common rebuild. |
| `all` | Full DAG | Rebuilds everything from `core-base`. |
| `thick-prep` | `thick-prep` | Prep layer for thick base. amd64 only. |
| `thick` | `thick-prep` + `scion-base` + catalog harnesses + hub | Full thick rebuild using Cloud Workstations base instead of `core-base`. amd64 only. |

### Tagging

Every image is tagged with both `:<tag>` (controlled by `--tag`, defaults to `latest`) and `:<short-sha>` (computed once from `git rev-parse --short HEAD`). When no SHA is available (e.g. running outside a git working tree), only the mutable tag is emitted.

When two steps in the same run depend on each other, the orchestrator threads `BASE_IMAGE=...:<short-sha>` so chained builds are immune to concurrent overwrites of `:latest`. Standalone targets (e.g. `--target harnesses` on its own) reference the parent image as `:<tag>`.

**Release tag warning.** When HEAD is exactly on a git tag (`git describe --tags --exact-match`) and `--tag` is not given, `build-images.sh` prints a warning that the images will be tagged `:latest`, not `:<git tag>`, and suggests `--tag <git tag>`. The default stays `latest`; passing `--tag latest` explicitly silences the warning.

### Base guard, `--allow-stale-base` and `--strict-base`

Targets that stack on a base without building it check that the base exists before anything is built:

| Target | Required base |
|---|---|
| `scion-base`, `common` | `core-base:<tag>` (`thick-prep:<tag>` when `THICK_BUILD=true`) |
| `harnesses`, `hub`, `omni`, single images (`scion-claude`, `scion-hub`, ...) | `scion-base:<tag>`; a single image needs its own parent (e.g. `scion-grok-build:<tag>` for `scion-omni` when `OMNI_BUILD=true`) |
| `all`, `core-base`, `thick-prep`, `thick` | none (they build their own base) |

The base counts as present when any lookup finds it. A tag-list line must equal the tag exactly, and an empty `--tag` never matches:

- **Local store**, only for local (non-push) `local-docker` and `local-podman` builds: `docker image inspect` or `podman image inspect`. A `--push` build, including a multi-platform build that turns `--push` on, pulls the base from the registry, so a local copy does not count there.
- **Registry**, when a registry is set: `docker buildx imagetools inspect` (`local-docker`), `podman manifest inspect` (`local-podman`), or for `cloud-build` first `gcloud artifacts docker tags list <registry>/<image>` (needs only Artifact Registry read; the tag must match a listed tag exactly; run with `--user-output-enabled` so `core/user_output_enabled=false` cannot hide the list) and then, if docker is on PATH, `docker buildx imagetools inspect`. `gcloud artifacts docker images describe` is not used: it also lists Container Analysis occurrences, which needs `containeranalysis.occurrences.list`.

With `--builder cloud-build`, only the registry counts. The required base follows the Cloud Build configs (`scion-base` and `common` always check `core-base`), and a target whose config builds the base itself (`omni`) is not checked. This is the release driver's (`build-release-images.sh`) pre-flight mapping, made exact for each builder.

**Definitely missing stops the build.** Only each tool's own "not found" answer counts: `No such image` (docker store), `image not known` (podman store), `manifest unknown` or a trailing `: not found` (imagetools, podman manifest), and `NOT_FOUND` or an exact-match miss in the tag list (gcloud). When a registry is set, the registry must answer "not found": a local miss alone proves nothing about the registry. The build then stops with an error naming the image and how to build it, instead of building on whatever base the builder happens to resolve (ptone/scion#2250).

**A lookup that fails never stops a build that worked before.** Any other failure is a lookup error, not "not found": a permission or API error, no registry credentials (including docker's `executable file not found` for a configured credential helper), an old gcloud, no buildx, or the network. It prints a warning, "could not be checked (<tool>: <error>)", and the build goes ahead on whatever the builder resolves, as it did before this check existed. Lookups can fail where the build would succeed. For example, cloud-build lookups run as the submitting identity while the build runs as the Cloud Build service account, and imagetools uses client-side config while a non-push docker build pulls through the daemon.

**Precedence: error markers beat not-found markers, for every backend** (gcloud, docker store and registry, podman store and registry). If a lookup's output contains any error marker, the lookup is a lookup error, even when a not-found marker also appears. The markers cover:

- credentials and permissions (`denied`, `permission`, `unauthorized`, `forbidden`, a missing credential helper);
- timeouts and network, DNS and TLS failures;
- HTTP 429 and 5xx;
- a crashed tool or an unreachable daemon;
- multi-source messages such as podman's registries.conf mirror aggregation (`(Mirrors also failed: [...]): <primary>: <error>`).

A mirror message is a lookup error even when every source says not found, because which source is authoritative cannot be told safely. Only output with a not-found marker and no error marker counts as definitely missing. The looked-up reference (image path and tag) is masked before the check, so a tag such as `timeout-fix` or a path such as `credentials-prod` is not taken for an error marker. The full list is `PROBE_ERROR_MARKERS` in `build-images.sh`.

The gcloud lookup runs with `CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard` and `--verbosity=error`. Structured logs, a timestamped console format or a quiet verbosity in the user's gcloud config therefore cannot hide a real `NOT_FOUND`.

- `--strict-base` (opt-in, for CI that wants a hard gate) also stops on a lookup error.
- `--allow-stale-base` skips the check entirely.
- Under `--dry-run`, a stop is reported as a warning and the dry run continues.

`build-release-images.sh` does not forward `--allow-stale-base` or `--strict-base`, because they are outside its base flag contract. That is fine: it runs its own base pre-flight, and the default here never blocks on a lookup error.

### Build time and version label

`BUILD_TIME` (the `BuildTime` embedded in `scion` and `sciontool`) is the commit time of HEAD (`git log -1 --format=%ct`), not the build host clock, so two builds of one commit embed the same timestamp. The same value is exported as `SOURCE_DATE_EPOCH`. With `local-docker`, buildx uses it for the image timestamps, so `docker images` shows the commit's age under CREATED, not the build time. Cloud Build does not get `SOURCE_DATE_EPOCH` (only `BUILD_TIME`), so its image timestamps are still the build clock. Podman honours it depending on the version. An explicit `SOURCE_DATE_EPOCH` in the environment takes precedence, and a non-integer value there is an error. The commit time is read with `git log -1 --no-show-signature --no-color --format=%ct`, so `log.showSignature` or `color.ui=always` in the user's git config cannot corrupt it. If git still prints something other than an integer, the script warns and falls back to the build clock. Outside a git checkout the Dockerfiles fall back to the build clock. Cloud Build gets the value as the `_BUILD_TIME` substitution.

**Move `scion-base:latest` together with the harness tags.** Agents that use the `generic` harness run `scion-base:latest` itself (under your `image_registry`, if set): the built-in generic harness-config (`pkg/config/harness_config.go`) names no harness-specific image. If a deployment repoints only the harness `:latest` tags (`scion-claude:latest`, ...) after a rebuild, generic agents stay on the old base, and with it the old `sciontool`. Repoint `scion-base:latest` in the same step, or give a `generic` harness-config its own `image`.

### Build provenance and stale sciontool

`scion-base` is where the `sciontool` binary is compiled; every harness image and `scion-hub` just `FROM` it without rebuilding Go code. That means **a fix that lands in `sciontool` (or `pkg/version`) does not reach a running agent until `scion-base` is rebuilt, and then the harness/hub images on top of it are rebuilt too.** A harness-only build (`--target harnesses`, `--target hub`, or an individual harness step) reuses whatever `scion-base:<tag>` already exists and will silently keep an old `sciontool` if you skip the base rebuild.

To make that visible:

- `sciontool version` (inside any built image) prints the embedded git commit, and, on an exact release tag, the version.
- `docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' <image>` prints the same commit as an OCI label on `scion-base` and everything built from it, without starting the container.
- `docker inspect --format '{{index .Config.Labels "org.opencontainers.image.version"}}' <image>` prints the release (the git tag HEAD was exactly on; empty for off-tag builds), from the same images.
- Running `build-images.sh` with a target that needs `scion-base` but doesn't build it in the same invocation prints a warning naming the `scion-base` image it will inherit (and that image's revision label, when it can be read locally without a pull).

**Upgrade note:** after pulling a `sciontool`-side fix (for example, a usage-telemetry fix), rebuild in this order: `--target scion-base` first, then `--target harnesses` (and `--target hub` if needed). Rebuilding only harnesses on top of an old `scion-base` does not pick up the fix.

### Quick Start: Build Your Own Images

```bash
# Build locally without ever pushing — bare tags (scion-base:latest,
# scion-claude:latest, etc.)
# land in your local engine's image store. Default builder: local-docker.
image-build/scripts/build-images.sh --target all

# Build locally, auto-detecting the registry from SCION_IMAGE_REGISTRY so
# images are tagged with the prefix the hub expects (e.g., scion-local/scion-claude:latest).
# No --registry flag needed when the env var is already set.
SCION_IMAGE_REGISTRY=scion-local image-build/scripts/build-images.sh --target all

# Same, with Podman
image-build/scripts/build-images.sh --builder local-podman --target all

# Build and push to your registry (default builder: local-docker)
image-build/scripts/build-images.sh --registry ghcr.io/myorg --push

# Submit to Cloud Build (--registry is required here)
image-build/scripts/build-images.sh --builder cloud-build \
  --registry us-central1-docker.pkg.dev/myproj/scion --target all

# Preview what would run, without executing
image-build/scripts/build-images.sh --target all --platform all --dry-run

# Configure scion to use the images you built (only when pushing to a registry)
scion config set image_registry ghcr.io/myorg
```

`--registry` is optional for local builds without `--push`; it's required when
`--push` is set or when using `--builder cloud-build`. When omitted, the script
falls back to the `SCION_IMAGE_REGISTRY` environment variable so that locally-built
images automatically match the hub's configured registry prefix.

### Quick Start: Google Cloud Build

```bash
# One-time setup
image-build/scripts/setup-cloud-build.sh --project my-project

# Trigger a build
image-build/scripts/build-images.sh --builder cloud-build \
  --registry us-central1-docker.pkg.dev/my-project/public-docker
```

The legacy `trigger-cloudbuild.sh` script still works as a deprecation shim and forwards to the orchestrator.

### Quick Start: GitHub Actions (GHCR)

1. Fork the repo.
2. Go to **Actions** > **Build Scion Images** > **Run workflow**.
3. Enter `ghcr.io/<your-username>` as the registry.
4. Run `scion config set image_registry ghcr.io/<your-username>`.

The workflow shells out to `build-images.sh --builder local-docker`. It is also available as a reusable workflow via `workflow_call` for use in downstream repos.

## Build images for a release

`scripts/build-release-images.sh` builds the full image set (core-base,
scion-base, harnesses, hub) from the image specs of a release tag. The images
are tagged `:<version>` (plus `:<short-sha>`). For v0.3.0-preview.4 and
later, the `scion` and `sciontool` binaries inside them also report that
version. Older releases (v0.3.0-preview.1 to .3) build and are tagged the same
way, but they predate version stamping, so their binaries report `dev`; the
driver warns and records `"version_stamped": false` in the manifest.

The recipe is the tag's tree. The driver clones the tag and runs **that
tree's** `build-images.sh --target all --tag <version>`, so a driver from
`main` can build an older release. It forwards only the flags every release's
`build-images.sh` understands (`--target`, `--registry`, `--platform`,
`--push`, `--builder`, `--dry-run`) and rejects the rest. The driver is
standalone: it needs bash (3.2 or later), git, and docker or podman, and it can
be downloaded and run as a single file. See `--help` for every option.

**Maintainer: build the images for the `scion` binary you run** (local docker,
no registry). With no version argument the driver uses the `version` field of
`scion version --format json`, and it fails if that is `dev`:

```bash
scion version --format json | grep '"version"'   # "v0.3.0-preview.5"
image-build/scripts/build-release-images.sh       # clones v0.3.0-preview.5, builds all, tags :v0.3.0-preview.5
```

**External user, no checkout, pushing multi-arch to their own registry:**

```bash
curl -fsSLO https://raw.githubusercontent.com/GoogleCloudPlatform/scion/main/image-build/scripts/build-release-images.sh
docker login ghcr.io
bash build-release-images.sh v0.3.0-preview.5 -- --registry ghcr.io/myorg --platform all --push
```

**Maintainer: rebuild an older release on Cloud Build:**

```bash
image-build/scripts/build-release-images.sh v0.3.0-preview.4 -- \
  --builder cloud-build --registry us-central1-docker.pkg.dev/<project>/<repo>
```

Cloud Build runs asynchronously: the tag's `build-images.sh` submits the build
(`gcloud builds submit --async`) and returns at once. The driver therefore
records no digests: `build-manifest.json` gets `"submitted": true`,
`"pushed": false` and empty digests, and the summary says the build was only
submitted. `--also-tag latest` is refused with `--builder cloud-build`. Follow
the build with `gcloud builds list` / `gcloud builds describe <id>`. Once it
has finished, move `:latest` with
`docker buildx imagetools create -t <registry>/<image>:latest <registry>/<image>:<version>`,
or rebuild locally without `--builder cloud-build`.

Other behaviour:

- **Source.** The tag is cloned (`--depth 1`) into
  `${XDG_CACHE_HOME:-~/.cache}/scion/image-src/<version>/`, or `--src-dir`.
  The clone carries only the requested tag, because several tags can point at
  one commit (a preview tag and a nightly tag, for example) and
  `build-images.sh` stamps whichever name `git describe` prints. An existing
  clone is reused when its HEAD is the tag's commit and `git describe` gives
  the tag and it has no modified or untracked files (other than
  `build-manifest.json`); any other non-empty directory is an error, and the
  driver never deletes it. Forks pass `--repo <url>`.
- **`--ref <branch|tag|full 40-hex sha>`** builds an arbitrary commit (short
  SHAs are rejected). The images are tagged with its short SHA, no tag is
  fetched, and the binaries report `dev`.
- **Single-image targets** such as `-- --target scion-claude` need
  v0.3.0-preview.4 or later; older releases only accept the group targets
  (`core-base`, `scion-base`, `harnesses`, `hub`, `omni`, `common`, `all`,
  `thick-prep`, `thick`). The driver checks the cloned tree and stops with
  that message.
- **Stacking on an existing base.** `-- --target common` (or `scion-base`,
  `harnesses`, `hub`, `omni`, or a single image such as `scion-claude`) skips the
  core-base rebuild. It is allowed only when `core-base:<version>` (for
  `scion-base` and `common`) or `scion-base:<version>` exists locally or in
  the registry; otherwise the driver stops before building. That avoids
  stacking release images on a stale base. The check needs docker (with buildx
  for registry images) or podman on PATH, also with `--builder cloud-build`;
  a `--dry-run` only warns.
- **Output.** A summary of image refs with registry digests (pushed images) or
  local image IDs, and the source commit. The same record is written to
  `<src-dir>/build-manifest.json`:
  `{version, commit, built_at, builder, pushed, submitted, version_stamped, images: {name: {ref, digest}}}`.
  A staged build in one `--src-dir` (core-base, then scion-base, then a
  harness, with the same builder and push flags) merges into the manifest of
  the same version, commit, builder and push mode, so the file ends up listing
  every image built from that tree, and every `digest` in it is of one kind
  (local image IDs, or registry digests when `pushed` is true; empty for a
  submitted `--builder cloud-build` run, which has `"submitted": true` and is
  never merged). Podman pushes record
  the registry digest (via `skopeo` when installed, else `podman push
  --digestfile`).
- **`--channel-tag preview|stable`** also points the channel tag at exactly
  the images this run pushed: each `<image>:preview` or `<image>:stable` gets
  the digest of that image's `:<version>`. The flag never touches `:latest`
  and is refused together with `--also-tag latest`.
  - It needs a registry path of the form `<host>[:<port>]/<path>` and pushed
    images. Image names from the build's step list must be plain image names;
    anything else stops the run, with nothing moved.
  - The tag must match the channel: `preview` follows the preview channel
    and needs `vX.Y.Z-preview.N` or `vX.Y.Z-rc.N`, and `stable` needs
    `vX.Y.Z`. Nightly tags and `--ref` builds are refused before anything is
    cloned.
  - Every digest is verified, and each channel tag's current digest is read
    and recorded, before the first channel tag moves. Only a lookup that
    reports "not found" (or "manifest unknown") records "none"; any other
    lookup failure stops the run with nothing moved. Docker uses `imagetools
    create` from `<image>@<digest>`. Podman uses `tag` and `push`, and needs
    `skopeo`, which reads every registry digest of a channel run (never a
    push). Each moved tag is read back. Podman pushes the channel tags one
    image at a time, so a failure at a later image leaves the earlier channel
    tags moved: the recovery file below applies.
  - A registry cannot move several tags at once. If a tag move or a
    read-back fails part-way, the run stops and lists the tags already
    moved. It writes the commands that point each of them back to its
    recorded digest (for a tag that did not exist: a `tags delete` with
    cloud-build, and a note for docker and podman) to
    `<src-dir>/channel-recovery.XXXXXX`, which the driver never runs. No
    `build-manifest.json` is written then.
  - With `--builder cloud-build`, the registry must be an Artifact Registry
    path, `<location>-docker.pkg.dev/<project>/<repository>`, where
    `<location>` is `us`, `europe`, `asia` or a region such as `us-central1`.
    The driver waits for the submitted builds with `gcloud builds describe`
    (`--build-timeout`, default 14400s per build; `--wait-timeout`, default
    21600s in total; both count 30s poll intervals only, so a `gcloud` call
    that hangs is not bounded by them).
  - It takes each digest from the builds' own results: `results.images`, or a
    `<ref> sha256:<hex>` line per image in `results.buildStepOutputs`. It
    requires `:<version>` to point to that digest, then runs `gcloud
    artifacts docker tags add <image>@<digest> <image>:<channel>`.
  - A failed build, a timeout or a missing result stops the run before any
    channel tag moves. A release tree whose `cloudbuild*.yaml` files report
    neither result stops at "no build-result digest".
  - `build-manifest.json` then also records, per image, `tag_digest` (what
    `:<version>` pointed to when checked) and `channel_ref`.

### Making agents use release images: `--also-tag latest`

A release build of the Hub (`vX.Y.Z`, `vX.Y.Z-preview.N` or `vX.Y.Z-rc.N`)
runs `scion-<harness>:<its version>` when `image_tag` is unset (the
`hub.image_tag_version_default` experiment, on by default). Development and
nightly builds run `scion-<harness>:latest`, unless the `image_tag` setting
(behind the `hub.image_tag_setting` experiment) selects another tag. For
those, or with `image_tag: latest`, add `--also-tag latest` so the release
images are also tagged `:latest`:

```bash
image-build/scripts/build-release-images.sh v0.3.0-preview.5 --also-tag latest
```

With a registry, this moves that registry's shared `:latest`, so it also
requires `--force-latest`:

```bash
image-build/scripts/build-release-images.sh v0.3.0-preview.5 --also-tag latest --force-latest -- --registry localhost/scion
```

### Single-node VM (`deploy.sh`)

With `container_images.source: build`, `scripts/single-node-vm/deploy.sh`
builds the images on the VM with this driver. It copies the driver from the
checkout it runs from to `/opt/scion-image-build/` on the VM (installed
root-owned and checked against the local copy's SHA-256), then runs it as
root once per image, in order:

```bash
build-release-images.sh <VERSION> --src-dir /opt/scion-image-src/<VERSION> \
  --also-tag latest --force-latest -- --builder local-docker --registry localhost/scion --target <image>
```

for `core-base`, `scion-base` and `scion-antigravity`, the minimal set the
hub needs. Notes for adopters:

- **Nothing is pushed.** `--registry localhost/scion` without `--push` only
  names the images in the VM's local Docker store
  (`localhost/scion/<image>:<VERSION>`, `:<short-sha>` and `:latest`).
  `--force-latest` is there because the driver requires it whenever a
  registry is named; it moves only the local `:latest`, which is what the hub
  runs.
- **Release tags only.** `VERSION` must be `vX.Y.Z`, `vX.Y.Z-preview.N`,
  `vX.Y.Z-rc.N` or `nightly-YYYYMMDD`; anything else is refused before the
  driver runs. An expired nightly fails with `tag not found`.
- **Version floor.** `container_images.source: build` needs
  v0.3.0-preview.4 or later, because the single-image target
  `scion-antigravity` exists only from that release on.
  - An older release given with `--version`, or on a new install, is
    refused before `deploy.sh` creates anything, even if the VM already
    has its images: that is only known from a VM call after Phase 2.
  - A re-run without `--version` that keeps the older release a VM
    already runs is refused only if its images must be rebuilt (the
    marker or an image is missing, or a rebuild is forced); when they
    are built, the build is skipped and the deploy goes on.
  - Workaround: re-run without `--version` to keep the installed
    release, or use `container_images.source: registry`.
- **Reuse.** A successful build writes
  `/opt/scion-image-build/images-built.version`. A re-run with the same
  `VERSION` skips the build when that marker (or the
  `/opt/scion-source/.images-built.version` marker an older `deploy.sh`
  wrote) matches and the three `localhost/scion/*:latest` images exist.
  `--rebuild-images` or `container_images.force_rebuild: true` forces a
  rebuild. Each version's clone stays in `/opt/scion-image-src/<VERSION>/`
  and is reused by a rebuild; remove old ones yourself to free disk.
- **The IAP proxy** is still built from `/opt/scion-source`, which `deploy.sh`
  checks out at `VERSION`.

### Same recipe, not same bits

A release build uses exactly the release's Dockerfiles, install scripts and
scion source. Inputs that float still resolve at build time: harness CLIs
installed at `@latest`, apt packages, base images by tag, kubectl
`stable.txt` and similar. Two builds of the same version a week apart can
differ. `build-manifest.json` records what a given build produced; it does not
promise that a rebuild produces the same bits.

### Nightly tags expire

`nightly-YYYYMMDD` tags are deleted after 14 days. Building an expired nightly
fails with `tag not found`. Use a preview or stable release, or `--ref` with a
commit SHA.

## Cloud Build Configs

The `cloud-build` builder maps each group `--target` to a static YAML file:

| Target | Config file |
|---|---|
| `all` | `cloudbuild.yaml` |
| `common` | `cloudbuild-common.yaml` |
| `core-base` | `cloudbuild-core-base.yaml` |
| `scion-base` | `cloudbuild-scion-base.yaml` |
| `harnesses` | `cloudbuild-harnesses.yaml` |
| `hub` | `cloudbuild-hub.yaml` |
| `omni` | `cloudbuild-omni.yaml` |
| `thick-prep` | `cloudbuild-thick.yaml` (builds the full thick chain — see note below) |
| `thick` | `cloudbuild-thick.yaml` |

An individual harness step ID (`scion-claude`, `scion-muse-code`, etc.) is also
a valid `--target` under `cloud-build`: instead of a static file, the builder
generates a one-step config on the fly — the same `verify-registry` /
`setup-buildx` / `bootstrap-buildx` / `buildx build` shape as the matching step
in `cloudbuild-harnesses.yaml`, for that harness alone — submits it, and
removes it when the run ends. This is how to rebuild one harness on Cloud
Build (ptone/scion#2354) without resubmitting the whole `harnesses` group.

**Note:** `--target thick-prep` with `--builder cloud-build` builds the full thick
chain (thick-prep + scion-base + harnesses + hub), since both `thick-prep` and
`thick` map to the same `cloudbuild-thick.yaml`. With per-image builders
(`local-docker`, `local-podman`), `--target thick-prep` builds only thick-prep.

**Builder divergence for `omni`:** Under `cloud-build`, the omni target builds the
full chain from `thick-prep` (amd64 only, no buildx cache); under `local-docker`,
it chains from whatever `scion-base:<tag>` already exists. Same target name, two
lineages. The `cloud-build` path uses `gcloudignore-omni` to include web source
files that the default `.gcloudignore` excludes (the omni Dockerfile runs
`npm install && npm run build` to embed the web frontend).

These YAMLs reference `$_TAG`, `$_SHORT_SHA`, `$_COMMIT_SHA`, `$_REGISTRY`, and (in the five that build `scion-base`: `all`, `common`, `scion-base`, `thick`/`thick-prep`, `omni`) `$_VERSION`, all forwarded by the orchestrator. `_TAG` defaults to `latest` in every YAML's `substitutions:` block; `_VERSION` defaults to `''` in the YAMLs that declare it, so a manual `gcloud builds submit` that omits either still works. The orchestrator itself only forwards a non-empty `_VERSION` when `HEAD` is on an exact git tag (see "Build provenance and stale sciontool" above) — off-tag, it relies on that yaml default.

The aggregate `cloudbuild-harnesses.yaml`, `cloudbuild-common.yaml`,
`cloudbuild.yaml`, and `cloudbuild-thick.yaml` files are static snapshots of
the current catalog. When adding or removing a harness Dockerfile under
`harnesses/<name>/`, update those four aggregate YAMLs too (`cloudbuild-omni.yaml`
is a deliberate subset — see its own header — and is not part of this set).
`scripts/check-harness-coverage.sh` compares those four files against the
`harnesses/` tree and fails if one falls out of sync (ptone/scion#2357).
Individual harness bundles can also carry their own
`harnesses/<name>/cloudbuild.yaml` for one-off builds.

## GKE Hub Image (`cloudbuild-hub-gke.yaml`)

The `deploy/helm/scion-hub` chart runs the hub with `runAsNonRoot` as uid 1000,
so it needs the non-root `hub-gke` stage of the repo-root `Dockerfile`, not the
root-running `scion-hub` image above. `cloudbuild-hub-gke.yaml` builds that
stage (linux/amd64, web UI embedded) and pushes only
`$_REGISTRY/scion-hub-gke:$_SHORT_SHA`. It is not one of the `build-images.sh`
targets; submit it directly from the repo root:

```bash
gcloud builds submit \
  --config=image-build/cloudbuild-hub-gke.yaml \
  --ignore-file=image-build/gcloudignore-omni \
  --substitutions=_REGISTRY=<registry>,_SHORT_SHA=$(git rev-parse --short HEAD) \
  .
```

`--ignore-file` is required because the default `.gcloudignore` drops the web
source the Dockerfile builds. hub-gke uses the omni ignore file,
`gcloudignore-omni`, which keeps it.

All Cloud Build ignore files (`.gcloudignore` and `image-build/gcloudignore-*`)
anchor every pattern that names a repo-root path with a leading `/`, as the
root `.dockerignore` does. Gitignore semantics match a slash-less pattern at
any depth, so an unanchored `agents.md`, `.gemini/`, `.claude/` or `README.md`
would also drop files under `//go:embed` roots (`pkg/config/embeds/`,
`resources/templates/`, `harnesses/`): the build would succeed with those files
missing from the binary. Patterns left unanchored on purpose (`*_test.go`,
`.git/`, `testdata/`, `downloads/`) say why in a comment.
`cmd/gcloudignore_contract_test.go` fails if any ignore file matches a tracked
file under a `//go:embed` root. Note that gcloud resolves `#!include:.gitignore`
relative to the ignore file, so the `image-build/` files include the empty
`image-build/.gitignore`, and only the root `.gcloudignore` pulls in the root
`.gitignore`.

No moving tag is pushed: repointing one needs an explicit ACK, and the chart
prefers pinning the image by digest (`image.digest` over `image.tag`).

The image is **linux/amd64 only** (the frontend and builder stages do not
cross-compile; see the file's header). On a cluster with arm64 nodes, pin the
hub pod to amd64 nodes, e.g. chart value
`hub.nodeSelector: {kubernetes.io/arch: amd64}`.

Locally, from a clean checkout, `docker build --platform linux/amd64 --target
hub-gke .` builds from the same source files as the Cloud Build upload above,
so the binary embeds the same templates and web UI. It is not byte-identical:
base images are pulled at build time and layer timestamps differ.
`docker build .` with no `--target` still builds the root-running runtime
image. With BuildKit (the default `docker build`, and `buildx`), a default
build skips the unused `hub-gke` stage; the legacy builder
(`DOCKER_BUILDKIT=0`) runs that stage too but still outputs the runtime image,
so it only costs build time.

## Package Registries

By default the images install packages from the public npm registry and PyPI. On
a network where `registry.npmjs.org` or `pypi.org` is unreachable, point the
build at internal mirrors (Artifactory, Nexus, Verdaccio, devpi) with four
optional environment variables:

| Variable | Purpose |
|---|---|
| `NPM_REGISTRY` | npm registry URL. Passed to `core-base` as a build-arg and exported as `NPM_CONFIG_REGISTRY`, so `scion-base`, the harness images, and agents at runtime all inherit it. |
| `NPM_CONFIG_FILE` | Path to an `.npmrc` holding credentials for that registry. Mounted as a BuildKit secret, never written to an image layer. |
| `PIP_INDEX_URL` | Python package index URL. Same handling: a `core-base` build-arg, exported as `PIP_INDEX_URL` and inherited the same way. Used by the `hermes` image, and by anything an agent pip-installs at runtime. |
| `PIP_CONFIG_FILE` | Path to a `pip.conf` holding credentials for that index. Mounted as a BuildKit secret at `/etc/pip.conf`. |

```bash
export NPM_REGISTRY=https://artifactory.example.com/artifactory/api/npm/npm-repos/
export NPM_CONFIG_FILE="$HOME/.npmrc"
export PIP_INDEX_URL=https://artifactory.example.com/artifactory/api/pypi/pypi-repos/simple
export PIP_CONFIG_FILE="$HOME/.config/pip/pip.conf"
image-build/scripts/build-images.sh --target common
```

A minimal `pip.conf` for an index requiring basic auth:

```ini
[global]
index-url = https://USER:TOKEN@artifactory.example.com/artifactory/api/pypi/pypi-repos/simple
```

All four are optional and independent. Unset, the build uses the public
registries unauthenticated, exactly as before. `NPM_REGISTRY` or
`PIP_INDEX_URL` without its credentials file works for a mirror that allows
anonymous reads.

Put credentials in the secret file rather than in `PIP_INDEX_URL`: that variable
is baked into the image as `ENV`, so a token embedded in the URL would persist
in the image and show up in `docker inspect`.

Credentials go in as a BuildKit secret rather than a build-arg deliberately: a
build-arg is recoverable from `docker history` on the resulting image, whereas a
secret mount is available only during the `RUN` that requests it. The mounts are
declared `required=false`, so builds without a secret are unaffected.

Supported by the `local-docker` and `local-podman` builders.

**Still not covered:** apt. The `hermes` image installs Node.js from
`deb.nodesource.com` via an apt source, which no build-arg here redirects. A host
that blocks that domain will fail on that layer regardless of the index settings
above.

## Authentication

The orchestrator and builders assume the caller is already authenticated to the target registry (via `docker login`, `podman login`, `gcloud auth configure-docker`, etc.) and to any required cloud APIs. No login steps are performed inside the script.
