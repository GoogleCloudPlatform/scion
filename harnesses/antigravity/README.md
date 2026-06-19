# Antigravity Harness Bundle

Scion harness configuration for
[Antigravity CLI](https://antigravity.google/product/antigravity-cli), a
Gemini-based coding agent CLI using OAuth via gnome-keyring.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/antigravity
```

Or directly from GitHub:

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/antigravity
```

## Auth Modes

| Mode | Env / Secret | Notes |
|------|-------------|-------|
| `oauth-token` (default) | `AGY_KEYRING_TOKEN` | OAuth refresh token JSON stored in gnome-keyring |
| `vertex-ai` | `AGY_KEYRING_TOKEN` + `GOOGLE_CLOUD_PROJECT` | Enterprise/GCP mode via keyring + Vertex AI |

Both auth modes require a JSON object containing a `refresh_token` field,
injected via the `AGY_KEYRING_TOKEN` secret. The provisioner initializes
gnome-keyring and stores the token at container startup.

## Bundle Layout

```
antigravity/
  config.yaml       # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  dialect.yaml       # Hook dialect mapping (antigravity events -> scion events)
  Dockerfile         # Image build (FROM scion-base)
  cloudbuild.yaml    # Cloud Build configuration
  skills/.gitkeep    # Skills directory placeholder
  home/.gitkeep      # Home files generated at provision time
```

## Image Build Chain

```
core-base -> scion-base -> scion-antigravity
```

The keyring packages (`gnome-keyring`, `libsecret`, `dbus-x11`) are
provided by `core-base`. The antigravity Dockerfile adds the Antigravity
CLI binary on top of `scion-base`.

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-antigravity:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```
