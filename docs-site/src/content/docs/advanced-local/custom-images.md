---
title: Building Custom Images
description: Build and configure your own Scion container images using Docker, GitHub Actions, or Google Cloud Build.
---

Scion agents run inside container images that bundle an LLM harness (Claude, Gemini, etc.) with the Scion toolchain. By default, Scion uses pre-built images from the upstream registry. This guide shows how to build your own images and configure Scion to use them.

## Why Build Custom Images?

- **Self-hosted registries**: Push images to a registry you control (GHCR, Artifact Registry, ECR, etc.).
- **Pinned versions**: Tag and version images to match your deployment lifecycle.
- **Custom modifications**: Add tools, certificates, or configurations to the base images.

## Image Hierarchy

Scion images are built in layers:

```
core-base          System dependencies (Go, Node, Python, Git)
  └── scion-base   Scion CLI, sciontool binary, scion user, entrypoint
        ├── scion-claude     Claude Code harness
        ├── scion-gemini     Gemini CLI harness
        ├── scion-opencode   OpenCode harness
        └── scion-codex      Codex harness
```

The `core-base` layer changes infrequently, but needs to be built at least once as it is a prerequisite for all other layers. Most rebuilds only need `scion-base` and the harness layers (the `common` build target).

### Non-Root Requirement

For security and compatibility across runtimes (especially Kubernetes), Scion agents are required to run as a non-root user.

- **User**: The base images create a `scion` user.
- **UID**: The user must have UID `1000`.
- **Permissions**: Ensure your custom images do not require root privileges at runtime and that any added files or directories are accessible by the `scion` user. Home directory structure (`/home/scion`) and environmental variables (`HOME`, `USER`, `LOGNAME`) are automatically injected by the runtime.

## Quick Start

### Option 1: Local Docker Build

Build all images locally and push to your registry. Once `core-base` has been built, rebuilds can often use the default `common` build target.

```bash
# Build all layers (core-base, scion-base, and harnesses), then push
image-build/scripts/build-images.sh --registry ghcr.io/myorg --push --target all

# Configure Scion to use them
scion config set image_registry ghcr.io/myorg
```

### Option 2: GitHub Actions (GHCR)

If your project is hosted on GitHub:

1. Fork the repo (or use it as a template).
2. Go to **Actions** > **Build Scion Images** > **Run workflow**.
3. Enter `ghcr.io/<your-username>` as the registry.
4. Wait for the build to complete.
5. Configure Scion:
   ```bash
   scion config set image_registry ghcr.io/<your-username>
   ```

The workflow builds multi-platform images (`linux/amd64` and `linux/arm64`) and pushes them to GHCR using the repository's `GITHUB_TOKEN`.

### Option 3: Google Cloud Build

For GCP-based workflows:

```bash
# One-time setup: enable APIs, create Artifact Registry repo, grant permissions
image-build/scripts/setup-cloud-build.sh --project my-gcp-project

# Trigger a build
image-build/scripts/trigger-cloudbuild.sh --project my-gcp-project
```

Then configure Scion with the registry path printed by the setup script:

```bash
scion config set image_registry us-central1-docker.pkg.dev/my-gcp-project/scion
```

## Configuring Scion: `image_registry`

The `image_registry` setting tells Scion to pull images from your registry instead of the upstream default. It rewrites the registry prefix of all standard harness images (those named `scion-<harness>`) while preserving the image name and tag.

### How It Works

When `image_registry` is set, Scion transforms the default image reference:

| Default Image | `image_registry` | Resolved Image |
| :--- | :--- | :--- |
| `us-central1-docker.pkg.dev/.../scion-claude:latest` | `ghcr.io/myorg` | `ghcr.io/myorg/scion-claude:latest` |
| `us-central1-docker.pkg.dev/.../scion-gemini:latest` | `ghcr.io/myorg` | `ghcr.io/myorg/scion-gemini:latest` |

### Setting It

**Globally** (applies to all groves):

```bash
scion config set image_registry ghcr.io/myorg
```

Or edit `~/.scion/settings.yaml` directly:

```yaml
schema_version: "1"
image_registry: "ghcr.io/myorg"
```

**Per-profile** (different registries for different environments):

```yaml
profiles:
  local:
    runtime: docker
    image_registry: "ghcr.io/myorg"
  staging:
    runtime: kubernetes
    image_registry: "us-central1-docker.pkg.dev/myproject/staging"
```

Profile-level `image_registry` takes precedence over the top-level setting.

### Override Precedence

The `image_registry` setting is the lowest-priority way to configure images. Explicit overrides always win:

1. **CLI `--image` flag** (highest priority)
2. **Template `scion-agent.yaml`** image field
3. **Profile `harness_overrides`** image field
4. **`image_registry`** rewrite (lowest priority)

If any higher-priority override specifies a full image path, `image_registry` does not apply to that agent.

:::note
`image_registry` only rewrites images whose name starts with `scion-`. Fully custom images (e.g., `mycompany/custom-agent:v2`) are never rewritten.
:::

## Build Script Reference

The `image-build/scripts/build-images.sh` script supports the following options:

| Flag | Description | Default |
| :--- | :--- | :--- |
| `--registry <path>` | **Required.** Target registry path (e.g., `ghcr.io/myorg`). | — |
| `--target <target>` | Build target: `common`, `all`, `core-base`, or `harnesses`. | `common` |
| `--push` | Push images after building. | Build only |
| `--platform <plat>` | Target platform(s). Use `all` for `linux/amd64,linux/arm64`. | Current arch |
| `--tag <tag>` | Image tag. | `latest` |

### Build Targets

| Target | What It Builds |
| :--- | :--- |
| `common` | `scion-base` + all harness images (assumes `core-base` already exists). |
| `all` | Full rebuild: `core-base` + `scion-base` + all harnesses. |
| `core-base` | Only the `core-base` layer. |
| `harnesses` | Only the harness images (assumes `scion-base` already exists). |

### Examples

```bash
# Full rebuild for all platforms, pushed to GHCR
image-build/scripts/build-images.sh \
  --registry ghcr.io/myorg \
  --target all \
  --platform all \
  --push

# Build only harness images with a specific tag
image-build/scripts/build-images.sh \
  --registry ghcr.io/myorg \
  --target harnesses \
  --tag v1.2.0 \
  --push

# Local build for testing (no push, current architecture only)
image-build/scripts/build-images.sh \
  --registry local/test
```

## GitHub Actions Workflow

The workflow at `.github/workflows/build-images.yml` can be used in two ways:

### Manual Trigger (`workflow_dispatch`)

Run it from the GitHub Actions UI with inputs for registry, target, tag, and platform.

### Reusable Workflow (`workflow_call`)

Call it from your own workflows in downstream repos:

```yaml
jobs:
  build-images:
    uses: google/scion/.github/workflows/build-images.yml@main
    with:
      registry: ghcr.io/myorg
      target: common
      tag: latest
      platform: all
```

## Google Cloud Build

Scion includes Cloud Build configuration files for GCP-native builds:

| Config File | Purpose |
| :--- | :--- |
| `cloudbuild.yaml` | Full rebuild of all layers. |
| `cloudbuild-common.yaml` | Rebuild `scion-base` + harnesses (most common). |
| `cloudbuild-core-base.yaml` | Rebuild `core-base` only. |
| `cloudbuild-harnesses.yaml` | Rebuild all harness images only. |

### Initial Setup

Run the one-time setup script to configure your GCP project:

```bash
image-build/scripts/setup-cloud-build.sh --project my-gcp-project
```

This script:
- Enables the Cloud Build and Artifact Registry APIs.
- Creates an Artifact Registry repository named `scion`.
- Grants Cloud Build the necessary IAM permissions.

### Triggering Builds

```bash
# Build everything (default: cloudbuild-common.yaml)
image-build/scripts/trigger-cloudbuild.sh --project my-gcp-project

# Full rebuild including core-base
image-build/scripts/trigger-cloudbuild.sh --project my-gcp-project --config cloudbuild.yaml
```
