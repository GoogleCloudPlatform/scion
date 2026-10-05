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
                   including git >= 2.47 (amd64 only)
  └── scion-base   Same Dockerfile, different foundation
        ├── harness images
        └── hub
```

`core-base/`, `scion-base/`, and `hub/` live under `image-build/`. Harness images
build from self-contained bundles under `harnesses/<name>/` when that bundle has
a `Dockerfile` and `cloudbuild.yaml`. See
[`harnesses/README.md`](../harnesses/README.md).

### Where git comes from

Scion hard-requires **git >= 2.47.0** (`pkg/util/git.go` `CheckGitVersion`, for
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
| `scripts/builders/*.sh` | Backend adapters (local-docker, local-podman, cloud-build). |
| `scripts/lib/targets.sh` | Target → step list resolution. Single source of truth for the build DAG. |
| `scripts/trigger-cloudbuild.sh` | Deprecation shim. Forwards to `build-images.sh --builder cloud-build`. |
| `scripts/pull-containers.sh` | Pull pre-built images (auto-detects runtime). |
| `scripts/setup-cloud-build.sh` | One-time GCP setup (APIs, Artifact Registry, permissions). |
| `scripts/check-harness-coverage.sh` | Fails if a `harnesses/<name>/Dockerfile` is missing from an aggregate `cloudbuild-*.yaml`. |
| `.github/workflows/build-images.yml` | GitHub Actions workflow for building and pushing images. |

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

### Build provenance and stale sciontool

`scion-base` is where the `sciontool` binary is compiled; every harness image and `scion-hub` just `FROM` it without rebuilding Go code. That means **a fix that lands in `sciontool` (or `pkg/version`) does not reach a running agent until `scion-base` is rebuilt, and then the harness/hub images on top of it are rebuilt too.** A harness-only build (`--target harnesses`, `--target hub`, or an individual harness step) reuses whatever `scion-base:<tag>` already exists and will silently keep an old `sciontool` if you skip the base rebuild.

To make that visible:

- `sciontool version` (inside any built image) prints the embedded git commit, and, on an exact release tag, the version.
- `docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' <image>` prints the same commit as an OCI label on `scion-base` and everything built from it, without starting the container.
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
  --ignore-file=image-build/gcloudignore-hub-gke \
  --substitutions=_REGISTRY=<registry>,_SHORT_SHA=$(git rev-parse --short HEAD) \
  .
```

`--ignore-file` is required because the default `.gcloudignore` drops the web
source the Dockerfile builds. Use `gcloudignore-hub-gke`, not
`gcloudignore-omni`: both keep the web source, but omni's unanchored
`agents.md` and `.gemini/` patterns also drop the embedded default-template
files under `pkg/config/embeds/` and `resources/` (gitignore semantics match a
slash-less pattern at any depth), so the build succeeds with those files
missing from the binary. `gcloudignore-hub-gke` anchors those patterns to the
repo root, as the root `.dockerignore` does.

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
