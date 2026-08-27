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

thick-prep         Patches Cloud Workstations base for scion compatibility (amd64 only)
  └── scion-base   Same Dockerfile, different foundation
        ├── harness images
        └── hub
```

`core-base/`, `scion-base/`, and `hub/` live under `image-build/`. Harness images
build from self-contained bundles under `harnesses/<name>/` when that bundle has
a `Dockerfile` and `cloudbuild.yaml`. See
[`harnesses/README.md`](../harnesses/README.md).

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
| `.github/workflows/build-images.yml` | GitHub Actions workflow for building and pushing images. |

### Builders

`build-images.sh` selects an execution backend with `--builder <name>`. Three are bundled:

| Builder | Backend | Multi-arch | Push behavior |
|---|---|---|---|
| `local-docker` (default) | `docker buildx` | yes (auto-promotes to `--push`) | honors `--push`; `--load` otherwise |
| `local-podman` | `podman build` | single-arch by default; multi-arch errors out (manual QEMU setup required) | honors `--push`; built images live in the local store automatically |
| `cloud-build` | `gcloud builds submit` against a static `cloudbuild-*.yaml` | always amd64+arm64 (server-side) | always pushes |

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

### Quick Start: Build Your Own Images

```bash
# Build locally without ever pushing — bare tags (scion-base:latest,
# scion-claude:latest, etc.)
# land in your local engine's image store. Default builder: local-docker.
image-build/scripts/build-images.sh --target all

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

`--registry` is optional for local builds without `--push`; it's required when `--push` is set or when using `--builder cloud-build`.

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

The `cloud-build` builder maps each `--target` to a static YAML file:

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

These YAMLs reference `$_TAG`, `$_SHORT_SHA`, `$_COMMIT_SHA`, and `$_REGISTRY` substitutions, all forwarded by the orchestrator.

The aggregate `cloudbuild-harnesses.yaml`, `cloudbuild-common.yaml`, and
`cloudbuild.yaml` files are static snapshots of the current catalog. When adding
or removing a harness Dockerfile under `harnesses/<name>/`, update those
aggregate YAMLs too. Individual harness bundles can also carry their own
`harnesses/<name>/cloudbuild.yaml` for one-off builds.

## Authentication

The orchestrator and builders assume the caller is already authenticated to the target registry (via `docker login`, `podman login`, `gcloud auth configure-docker`, etc.) and to any required cloud APIs. No login steps are performed inside the script.
