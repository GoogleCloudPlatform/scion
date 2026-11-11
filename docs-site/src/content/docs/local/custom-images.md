---
title: Building Custom Images
description: Build and configure your own Scion container images using Docker, Podman, GitHub Actions, or Google Cloud Build.
---

Scion agents run inside container images that bundle an LLM harness (Claude, Gemini, etc.) with the Scion toolchain. You can build them yourself: from a release tag, to match the `scion` you run (see [Images that match a release](#images-that-match-a-release)), or from your own checkout. This guide shows how to build images and configure Scion to use them.

## Why Build Custom Images?

- **Self-hosted registries**: Push images to a registry you control (GHCR, Artifact Registry, ECR, etc.).
- **Pinned versions**: Tag and version images to match your deployment lifecycle.
- **Custom modifications**: Add tools, certificates, or configurations to the base images.

## Image Hierarchy

Scion images are built in layers:

```
core-base          System dependencies on node:24-trixie-slim (Go, Node, Python, Git, CLI tools)
  └── scion-base   Scion CLI, sciontool binary, scion user, entrypoint
        ├── scion-claude     Claude Code harness
        ├── scion-gemini-cli Gemini CLI harness
        ├── scion-opencode   OpenCode harness
        ├── scion-codex      Codex harness
        └── scion-hub        Scion hub server
```

The `core-base` layer changes infrequently, but needs to be built at least once as it is a prerequisite for all other layers. Most rebuilds only need `scion-base`, the harness layers, and the hub layer (the `common` build target).

Besides the language runtimes, `core-base` installs the command-line tools agents commonly use, through `image-build/lib/install-core-toolchain.sh`: `gh`, `gcloud`, `gcsfuse`, `kubectl`, `golangci-lint`, Chromium, and `helm`, plus the npm tools `chrome-devtools-mcp` and `@playwright/cli`. `helm` is pinned to an exact version, v3.17.3, the version used by the chart's CI, so agents that work on the `deploy/helm/scion-hub` chart render the same output as CI without downloading `helm` at run time. To change the version, set the `HELM_VERSION`, `HELM_SHA256_AMD64`, and `HELM_SHA256_ARM64` build args together; the build fails if the downloaded archive does not match the checksum for the build architecture. The `thick-prep` layer installs the same pinned `helm`.

### Non-Root Requirement

For security and compatibility across runtimes (especially Kubernetes), Scion agents are required to run as a non-root user.

- **User**: The base images create a `scion` user.
- **UID**: The user must have UID `1000`.
- **Permissions**: Ensure your custom images do not require root privileges at runtime and that any added files or directories are accessible by the `scion` user. Home directory structure (`/home/scion`) and environmental variables (`HOME`, `USER`, `LOGNAME`) are automatically injected by the runtime.

## Building Behind a Corporate Proxy (npm Registry)

Corporate networks that block `registry.npmjs.org` can point the build at an internal npm mirror (Artifactory, Nexus, Verdaccio, etc.) using two environment variables:

| Variable | Purpose | Default |
| :--- | :--- | :--- |
| `NPM_REGISTRY` | Registry URL passed as a Docker build arg. Sets `NPM_CONFIG_REGISTRY` inside the image so that all `npm install` commands during the build — and agents at runtime — use the mirror. | `https://registry.npmjs.org/` |
| `NPM_CONFIG_FILE` | Host path to an `.npmrc` file. Mounted as a BuildKit secret (`id=npmrc`) so authentication tokens are available during `RUN` steps but never written to an image layer. | — (unauthenticated) |

```bash
# Unauthenticated mirror
NPM_REGISTRY=https://npm.corp.example.com/ \
  image-build/scripts/build-images.sh --target all

# Authenticated mirror (token stays out of the image)
NPM_REGISTRY=https://npm.corp.example.com/ \
NPM_CONFIG_FILE=~/.npmrc \
  image-build/scripts/build-images.sh --target all --push --registry ghcr.io/myorg
```

`NPM_REGISTRY` is threaded as a build arg into the `core-base` layer and inherited by every downstream image. `NPM_CONFIG_FILE` is forwarded by the `local-docker` and `local-podman` builders; `cloud-build` does not support BuildKit secrets — use a private Artifact Registry npm repository with IAM-based auth instead.

:::note[Backward compatible]
When neither variable is set, the build uses the public npm registry with no credentials — existing workflows are unaffected.
:::

## Building Behind a Corporate Proxy (Python Package Index)

The same pattern applies to Python packages. Corporate networks that block `pypi.org` can point the build at an internal PyPI mirror (Artifactory, Nexus, devpi, etc.) using two environment variables:

| Variable | Purpose | Default |
| :--- | :--- | :--- |
| `PIP_INDEX_URL` | Registry URL passed as a Docker build arg. Sets `PIP_INDEX_URL` inside the image so that all `pip install` commands during the build — and agents at runtime — use the mirror. | `https://pypi.org/simple` |
| `PIP_CONFIG_FILE` | Host path to a `pip.conf` file. Mounted as a BuildKit secret (`id=pipconf`) so authentication tokens are available during `RUN` steps but never written to an image layer. | — (unauthenticated) |

```bash
# Unauthenticated mirror
PIP_INDEX_URL=https://pypi.corp.example.com/simple/ \
  image-build/scripts/build-images.sh --target all

# Authenticated mirror (credentials stay out of the image)
PIP_INDEX_URL=https://pypi.corp.example.com/simple/ \
PIP_CONFIG_FILE=~/.pip/pip.conf \
  image-build/scripts/build-images.sh --target all --push --registry ghcr.io/myorg
```

`PIP_INDEX_URL` is threaded as a build arg into the `core-base` layer and inherited by every downstream image. `PIP_CONFIG_FILE` is forwarded by the `local-docker` and `local-podman` builders; `cloud-build` does not support BuildKit secrets — use a private Artifact Registry PyPI repository with IAM-based auth instead.

:::note[Backward compatible]
When neither variable is set, the build uses the public PyPI index with no credentials — existing workflows are unaffected.
:::

:::tip[Combining npm and PyPI mirrors]
Both sets of proxy variables can be used together:
```bash
NPM_REGISTRY=https://npm.corp.example.com/ \
NPM_CONFIG_FILE=~/.npmrc \
PIP_INDEX_URL=https://pypi.corp.example.com/simple/ \
PIP_CONFIG_FILE=~/.pip/pip.conf \
  image-build/scripts/build-images.sh --target all --push --registry ghcr.io/myorg
```
:::

## Images that match a release

To build the images for a release, use the release image driver, `image-build/scripts/build-release-images.sh`. It clones the release tag and runs that tag's own `build-images.sh` with `--target all --tag <version>`, so the images use exactly the release's Dockerfiles and source, and the `scion` and `sciontool` binaries inside report the release version (v0.3.0-preview.4 and later). It runs from a checkout of `main`, or as a single downloaded file, and needs only bash, git, and Docker or Podman:

```bash
# Build the release your scion binary reports (scion version), in the local image store
image-build/scripts/build-release-images.sh

# Build a given release and push it to your registry (multi-arch)
image-build/scripts/build-release-images.sh v0.3.0-preview.5 -- \
  --registry ghcr.io/myorg --platform all --push
scion config set image_registry ghcr.io/myorg
```

The images are tagged `:<version>` and `:<short-sha>`. A release build of `scion` (`vX.Y.Z`, `vX.Y.Z-preview.N` or `vX.Y.Z-rc.N`) uses its own version as the image tag when `image_tag` is unset (the `hub.image_tag_version_default` [experiment](/scion/reference/experiments/), on by default), so it runs these `:<version>` images directly. An explicit `--image scion-<harness>:latest`, or a bare `scion-<harness>`, is retagged the same way. Development and nightly builds use `scion-<harness>:latest`. For those, or to keep `:latest` on a release build:

- add `--also-tag latest` to the driver so the release images are also tagged `:latest` (with a registry this moves that registry's `:latest`, so the driver also requires `--force-latest`), and set `image_tag: latest` on a release build to opt out of the version default (or disable the experiment: `scion hub experiments disable hub.image_tag_version_default`, or **Admin → Server Config → Experiments**); or
- with the `hub.image_tag_setting` experiment enabled, set `image_tag: version` or a literal tag (see [Image tag](/scion/reference/settings-precedence/#image-tag-experimental)).

A release build uses the same recipe, not the same bits: inputs such as harness CLIs installed at `@latest` and apt packages resolve at build time. The driver's `--help` and the "Build images for a release" section of `image-build/README.md` cover every option, including staged builds and Cloud Build.

The rest of this guide covers `build-images.sh`, which builds from the checkout it runs in (for example `main`, or your own changes) and tags `:latest` by default. With a release build of `scion`, which pulls `:<version>` by default, also set `image_tag: latest` (or disable `hub.image_tag_version_default`), or build with `--tag <your scion version>`, so agents use these images (see [Image tag](/scion/reference/settings-precedence/#image-tag-experimental)).

## How the Build Tooling Is Organized

A single orchestrator script — `image-build/scripts/build-images.sh` — owns the build DAG (which images depend on which, in what order, with which tags). The execution backend is selected with `--builder`. Three backends ship today:

| Builder | Backend | Multi-arch | Push behavior |
| :--- | :--- | :--- | :--- |
| `local-docker` (default) | `docker buildx` | yes (auto-promotes to `--push`) | honors `--push`; `--load` otherwise |
| `local-podman` | `podman build` | single-arch by default; multi-arch errors out (manual QEMU setup required) | honors `--push`; built images live in the local store automatically |
| `cloud-build` | `gcloud builds submit` against a static `cloudbuild-*.yaml` (group targets) or a config generated on the fly (individual harness targets) | always `linux/amd64` + `linux/arm64` (server-side) | always pushes |

The orchestrator computes tags, threads `BASE_IMAGE` between layers, and dispatches to the selected builder. Switching backends is purely a `--builder` flag change — target names and other flags are uniform.

## Quick Start

### Option 1: Local Docker Build

Build all images locally. Once `core-base` has been built, rebuilds can often use the default `common` build target.

```bash
# Build all layers locally without pushing — bare tags
# (scion-claude:latest, etc.) land in your local docker engine.
image-build/scripts/build-images.sh --target all

# Or build and push to your registry
image-build/scripts/build-images.sh --registry ghcr.io/myorg --push --target all

# When pushing, configure Scion to use them
scion config set image_registry ghcr.io/myorg
```

`--registry` is optional for local-only builds. Omit it and the orchestrator tags images with bare names that stay in your local image store. Supply it (without `--push`) if you'd rather have fully-qualified tags locally — nothing leaves the machine until you `docker push` separately. `--registry` becomes required as soon as you pass `--push` or `--builder cloud-build`.

### Option 2: Local Podman Build

```bash
# Single-arch local build, no registry needed
image-build/scripts/build-images.sh --builder local-podman --target all

# Or push to a registry
image-build/scripts/build-images.sh \
  --builder local-podman \
  --registry quay.io/myorg \
  --push
```

Multi-arch Podman builds require manual QEMU `binfmt` setup. Until that is in place, passing `--platform linux/amd64,linux/arm64` to `local-podman` exits with an actionable error.

### Option 3: GitHub Actions (GHCR)

If your project is hosted on GitHub:

1. Fork the repo (or use it as a template).
2. Go to **Actions** > **Build Scion Images** > **Run workflow**.
3. Enter `ghcr.io/<your-username>` as the registry.
4. Wait for the build to complete.
5. Configure Scion:
   ```bash
   scion config set image_registry ghcr.io/<your-username>
   ```

The workflow shells out to `build-images.sh --builder local-docker` after `docker/setup-buildx-action`, so it shares all the orchestration logic with local builds. It is also available as a reusable workflow via `workflow_call` for downstream repos.

### Option 4: Google Cloud Build

For GCP-based workflows:

```bash
# One-time setup: enable APIs, create Artifact Registry repo, grant permissions
image-build/scripts/setup-cloud-build.sh --project my-gcp-project

# Submit a build
image-build/scripts/build-images.sh \
  --builder cloud-build \
  --registry us-central1-docker.pkg.dev/my-gcp-project/scion
```

Then point Scion at the registry:

```bash
scion config set image_registry us-central1-docker.pkg.dev/my-gcp-project/scion
```

:::note[Legacy `trigger-cloudbuild.sh`]
The old `trigger-cloudbuild.sh` script is now a thin deprecation shim that forwards to `build-images.sh --builder cloud-build`. New workflows should call the orchestrator directly.
:::

## Configuring Scion: `image_registry`

The `image_registry` setting tells Scion which registry to pull images from. It rewrites the registry prefix of all standard harness images (those named `scion-<harness>`) while preserving the image name and tag.

### How It Works

When `image_registry` is set, Scion transforms the default image reference, which the harness-config names without a registry:

| Default Image | `image_registry` | Resolved Image |
| :--- | :--- | :--- |
| `scion-claude:latest` | `ghcr.io/myorg` | `ghcr.io/myorg/scion-claude:latest` |
| `scion-gemini-cli:latest` | `ghcr.io/myorg` | `ghcr.io/myorg/scion-gemini-cli:latest` |

These examples show a development build or `image_tag: latest`. With a release build of `scion` and `image_tag` unset, the tag becomes the version, for example `ghcr.io/myorg/scion-claude:v0.3.0` (see [Image tag](/scion/reference/settings-precedence/#image-tag-experimental)).

### Setting It

**Globally** (applies to all projects):

```bash
scion config set image_registry ghcr.io/myorg
```

Or edit `~/.scion/settings.yaml` directly:

```yaml
schema_version: "1"
image_registry: "ghcr.io/myorg"
```

:::note[Failing Fast]
The Runtime Broker validates the configured `image_registry` at startup. If the setting is missing or invalid, the broker will fail fast with an explicit, actionable error rather than starting silently and causing opaque image-pull 404 errors during subsequent agent creation.
:::

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

Profile-level `image_registry` takes precedence over the top-level setting of the same source.
Across sources the order is: the Runtime Broker's `SCION_IMAGE_REGISTRY`, then values saved in the
Hub (profile, then top level), then the settings file (profile, then top level). So a registry
saved in the Hub, for example through the setup wizard, beats a settings-file profile value; save
the profile value in the Hub to keep it. See [Image registry order](/scion/reference/settings-precedence/#image-registry-order). The profile used is
the one the agent was provisioned with, recorded broker-side, on every start and restart; see
[Settings precedence](/scion/reference/settings-precedence/#container-image-and-kubernetes-image-pull-policy--a-separate-chain-from-b1).

### Override Precedence

The `image_registry` setting is the lowest-priority way to configure images. Explicit overrides always win (summary; see the full chain linked below, which also covers inline config and settings `harness_configs.<name>.image`):

1. **CLI `--image` flag** (highest priority)
2. **Profile `harness_overrides`** image field, when explicitly set
3. **Agent inline config** (`--config`) image field
4. **Settings `harness_configs.<name>.image`**, then the harness-config's own `config.yaml` image
5. **`image_registry`** rewrite (lowest priority)

A template's `image` field is ignored with a warning: a template describes the agent's job, and the image comes from the harness-config and settings.

Each time a higher-priority source replaces another source's image, the agent start logs it at
Info. See [Settings precedence](/scion/reference/settings-precedence/#container-image-and-kubernetes-image-pull-policy--a-separate-chain-from-b1)
for the full chain.

If any higher-priority override specifies a full image path, `image_registry` does not apply to that agent.

:::note
`image_registry` only rewrites images whose name starts with `scion-`. Fully custom images (e.g., `mycompany/custom-agent:v2`) are never rewritten.
:::

## Build Script Reference

The `image-build/scripts/build-images.sh` orchestrator supports the following options:

| Flag | Description | Default |
| :--- | :--- | :--- |
| `--registry <path>` | Target registry path (e.g., `ghcr.io/myorg`). Required when `--push` is set or with `--builder cloud-build`. When omitted for a local-only build, images are tagged with bare names (e.g., `scion-claude:latest`) and stay in the local store. | — |
| `--builder <name>` | Backend: `local-docker`, `local-podman`, or `cloud-build`. | `local-docker` |
| `--target <target>` | Build target (see below). | `common` |
| `--tag <tag>` | Mutable image tag. The `:<short-sha>` tag is always added when in a git repo. | `latest` |
| `--platform <plat>` | Target platform(s). Use `all` for `linux/amd64,linux/arm64`. Ignored by `cloud-build`. | builder's native arch |
| `--push` | Push images after building. Auto-enabled for multi-arch local builds. Ignored by `cloud-build` (always pushes). | build only |
| `--dry-run` | Print the resolved steps and the exact builder commands without executing. | off |

### Build Targets

Targets resolve to an ordered list of step IDs (one step per image):

| Target | What It Builds | Notes |
| :--- | :--- | :--- |
| `core-base` | `core-base` | Foundation tools layer. |
| `scion-base` | `scion-base` | Adds sciontool. Reuses existing `core-base:<tag>`. |
| `harnesses` | All catalog harness images with a `harnesses/<name>/Dockerfile` | Reuses existing `scion-base:<tag>`. |
| `hub` | `scion-hub` | Hub server image. Reuses existing `scion-base:<tag>`. |
| `common` (default) | `scion-base` + harnesses + hub | Skips `core-base`. Most common rebuild. |
| `all` | Full DAG | Rebuilds everything from `core-base`. |

### Tagging

Every image is tagged with both `:<tag>` (controlled by `--tag`, defaults to `latest`) and `:<short-sha>` (computed once from `git rev-parse --short HEAD`). When no SHA is available (e.g. running outside a git working tree), only the mutable tag is emitted.

When two steps in the same run depend on each other, the orchestrator threads `BASE_IMAGE=...:<short-sha>` so chained builds are immune to concurrent overwrites of `:latest`. Standalone targets (e.g. `--target harnesses` on its own) reference the parent image as `:<tag>`.

### Build Provenance

When building `scion-base`, the orchestrator stamps `scion` and `sciontool` with the git commit and, when `HEAD` is on an exact git tag, the version. Inside an agent container, `sciontool version` reports both. `scion-base` also carries the OCI `org.opencontainers.image.revision` label, which every image built `FROM scion-base` (harnesses, hub) inherits. To check which commit an image was built from without running it:

```bash
docker inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' scion-claude:latest
```

If a build uses `scion-base` but does not rebuild it in the same run (for example, `--target harnesses`), the orchestrator prints a warning. The resulting images inherit whatever `sciontool` is already baked into the existing `scion-base:<tag>`. The warning also shows that image's revision when a local copy exists. Include `scion-base` in the build (for example, `--target common`) so `sciontool` fixes reach your agents.

### Authentication

The orchestrator and builders assume the caller is already authenticated to the target registry (via `docker login`, `podman login`, `gcloud auth configure-docker`, etc.) and to any required cloud APIs. No login steps are performed inside the script.

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

# Local build for testing (no push, current architecture only, bare tags)
image-build/scripts/build-images.sh --target all

# Preview what would run, without executing anything
image-build/scripts/build-images.sh \
  --registry ghcr.io/myorg \
  --target all \
  --platform all \
  --dry-run

# Submit the same target DAG to Cloud Build
image-build/scripts/build-images.sh \
  --builder cloud-build \
  --registry us-central1-docker.pkg.dev/myproject/scion \
  --target all
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

The workflow is a runner, not a builder — it shells out to `build-images.sh --builder local-docker` and shares the same Dockerfiles and orchestration as a local build.

## Google Cloud Build Configs

The `cloud-build` builder maps each group `--target` to a static YAML file in `image-build/`:

| Target | Config file |
| :--- | :--- |
| `all` | `cloudbuild.yaml` |
| `common` | `cloudbuild-common.yaml` |
| `core-base` | `cloudbuild-core-base.yaml` |
| `scion-base` | `cloudbuild-scion-base.yaml` |
| `harnesses` | `cloudbuild-harnesses.yaml` |
| `hub` | `cloudbuild-hub.yaml` |

An individual harness step ID (`scion-claude`, `scion-muse-code`, etc.) is also
a valid `--target` under `cloud-build`. Rather than a static file, the builder
generates a one-step config for just that harness — mirroring the matching
step in `cloudbuild-harnesses.yaml` — submits it, and removes it afterward.
This rebuilds one harness on Cloud Build without resubmitting the whole
`harnesses` group.

These YAMLs reference `$_TAG`, `$_SHORT_SHA`, `$_COMMIT_SHA`, `$_REGISTRY`, and (in the five that build `scion-base`: `all`, `common`, `scion-base`, `thick`/`thick-prep`, `omni`) `$_VERSION`, all forwarded by the orchestrator. `_TAG` defaults to `latest` in every YAML's `substitutions:` block; `_VERSION` defaults to `''` in the YAMLs that declare it, preserving the prior behavior when `--tag` or a version tag is omitted. The orchestrator only forwards a non-empty `_VERSION` when `HEAD` is on an exact git tag; off-tag, it relies on that default.

### Initial Setup

Run the one-time setup script to configure your GCP project:

```bash
image-build/scripts/setup-cloud-build.sh --project my-gcp-project
```

This script:
- Enables the Cloud Build and Artifact Registry APIs.
- Creates an Artifact Registry repository named `scion`.
- Grants Cloud Build the necessary IAM permissions.
