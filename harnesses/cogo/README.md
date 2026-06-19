# Cogo Harness Bundle

Scion harness configuration for [Cogo](https://github.com/go-steer/cogo), a terminal-native agentic CLI for Go developers using Gemini 3.x models.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/cogo
```

## Auth Modes

| Mode | Env / Secret | Notes |
|------|-------------|-------|
| `api-key` (default) | `GOOGLE_API_KEY` | Public Gemini API via API key authentication |
| `vertex-ai` | `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_LOCATION` | Vertex AI mode using Google Application Default Credentials (ADC) |

## Bundle Layout

```
cogo/
  config.yaml       # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  Dockerfile         # Image build (FROM scion-base)
  cloudbuild.yaml    # Cloud Build configuration
```

## Build and Run

To build the `cogo` binary and pack it into the container locally:

```sh
# 1. Compile static cogo binary for linux-amd64
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o harnesses/cogo/bin/cogo /Users/ptone/src/cogo/cmd/cogo

# 2. Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-cogo:latest harnesses/cogo/
```
