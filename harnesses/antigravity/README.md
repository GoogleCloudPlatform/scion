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

## Usage telemetry

`config.yaml`'s `capabilities.telemetry.native_emitter` is `no`: antigravity
has no native OTel integration (`enableTelemetry` in its own settings is
product telemetry, unrelated to OTLP, and stays disabled). `provision.py`
sets `SCION_USAGE_SOURCE=hooks` unconditionally, so `gen_ai.api.calls` comes
from the `PreInvocation`/`PostInvocation` hooks that `dialect.yaml` already
maps to `model-start`/`model-end`.

**Granularity.** `PostInvocation` fires once per real model **request**, not
once per agent turn. A single turn that makes a tool call and then a
follow-up call produces two full `PreInvocation`/`PostInvocation` pairs
(`invocationNum` 0 and 1) before its one `Stop`; `invocationNum` resets to 0
on the next turn. Confirmed by driving the real `agy` 1.2.12 binary against
a local, credential-free mock model backend — see
`pkg/sciontool/hooks/dialects/testdata/antigravity/README.md` in the scion
checkout for the captured fixture and how it was taken.

**Usage (calls-only).** `PreInvocation` and `PostInvocation` are identical
in shape — neither carries any usage or token field, regardless of whether
the underlying model response had one. This matches `agy`'s own embedded
hooks documentation, which states the `PostInvocation` input is "Same as
`PreInvocation` input." So `dialect.yaml` maps no token fields for either
event, and antigravity publishes calls only; a tokens follow-up would need
`agy` to add usage data to this hook payload, or a different capture
mechanism.
