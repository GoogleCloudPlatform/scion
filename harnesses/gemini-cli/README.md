# Gemini CLI Harness Bundle

Scion harness configuration for [Gemini CLI](https://github.com/google-gemini/gemini-cli),
Google's coding agent CLI.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/gemini-cli
```

Or directly from GitHub:

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/gemini-cli
```

## Auth Modes

| Mode | Env / File | Notes |
|------|-----------|-------|
| `api-key` (default) | `GEMINI_API_KEY` or `GOOGLE_API_KEY` | Direct API access |
| `auth-file` | `~/.gemini/oauth_creds.json` | Personal OAuth credentials |
| `vertex-ai` | `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_REGION` | Vertex AI with ADC or service account |

## Bundle Layout

```
gemini-cli/
  config.yaml        # Harness configuration (provisioner, capabilities, auth)
  provision.py        # Container-side provisioner (pre-start hook)
  capture_auth.py     # Interactive auth capture script
  Dockerfile          # Image build (FROM scion-base)
  cloudbuild.yaml     # Cloud Build configuration
  home/
    .bashrc                     # Shell config with scion env sourcing
    .gemini/settings.json       # Gemini CLI settings (hooks, permissions)
    system_prompt.md            # System prompt placeholder
```

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-gemini:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```
