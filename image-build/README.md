# Image Build

Dockerfiles and build configurations for Scion container images.

## Image Hierarchy

```
core-base          System dependencies (Go, Node, Python)
  └── scion-base   Adds sciontool binary and scion user
        ├── claude     Claude Code harness
        ├── gemini     Gemini CLI harness
        ├── opencode   OpenCode harness
        └── codex      Codex harness
```

Each harness directory contains a `Dockerfile` that extends `scion-base` with harness-specific tooling.

`quack` (the duckflux workflow runner) is installed at `/usr/local/bin/quack` in `core-base`, so it is available in every Scion agent container. The pinned version is `QUACK_VERSION=0.7.3` and can be overridden at build time with `--build-arg QUACK_VERSION=...`.

## Scripts

All image-related scripts live under `scripts/`. GitHub Actions workflows remain in `.github/workflows/` per GitHub convention.

| Script | Purpose |
|--------|---------|
| `scripts/build-images.sh` | Build images locally using `docker buildx` |
| `scripts/trigger-cloudbuild.sh` | Submit a build to Google Cloud Build |
| `scripts/pull-containers.sh` | Pull pre-built images (auto-detects runtime) |
| `scripts/setup-cloud-build.sh` | One-time GCP setup (APIs, Artifact Registry, permissions) |
| `.github/workflows/build-images.yml` | GitHub Actions workflow for building and pushing images |

### Quick Start: Build Your Own Images

```bash
# Build and push to your registry
image-build/scripts/build-images.sh --registry ghcr.io/myorg --push

# Configure scion to use them
scion config set image_registry ghcr.io/myorg
```

### Quick Start: Google Cloud Build

```bash
# One-time setup
image-build/scripts/setup-cloud-build.sh --project my-project

# Trigger a build
image-build/scripts/trigger-cloudbuild.sh --project my-project
```

### Quick Start: GitHub Actions (GHCR)

1. Fork the repo.
2. Go to **Actions** > **Build Scion Images** > **Run workflow**.
3. Enter `ghcr.io/<your-username>` as the registry.
4. Run `scion config set image_registry ghcr.io/<your-username>`.

The workflow is also available as a reusable workflow via `workflow_call` for use in downstream repos.

## Cloud Build Configs

- `cloudbuild.yaml` - Full rebuild of all layers.
- `cloudbuild-common.yaml` - Rebuild scion-base + harnesses (most common).
- `cloudbuild-core-base.yaml` - Rebuild `core-base` only.
- `cloudbuild-scion-base.yaml` - Rebuild `scion-base` only.
- `cloudbuild-harnesses.yaml` - Rebuild all harness images only.
