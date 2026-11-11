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

**Granularity.** `PostInvocation` fires once per main-loop model request, not
once per agent turn. A single turn that makes a tool call and then a
follow-up call produces two full `PreInvocation`/`PostInvocation` pairs
(`invocationNum` 0 and 1) before its one `Stop`; `invocationNum` resets to 0
on the next turn. Confirmed by driving the real `agy` 1.2.12 binary against
a local, credential-free mock model backend — see
`pkg/sciontool/hooks/dialects/testdata/antigravity/README.md` in the scion
checkout for the captured fixture and how it was taken.

**Known undercount.** `agy`'s own auxiliary calls — for example conversation
title generation — run against the model without firing any Invocation hook
at all, so they never show up as a call. Failed or retried main-loop
attempts were not captured either, so their behavior (whether `PostInvocation`
fires at all, and with what `status`) is uncharacterized; today `dialect.yaml`
maps no `error` field on `PostInvocation`, so every recorded call reads
`status=success`.

**Usage (calls-only).** `PreInvocation` and `PostInvocation` are identical
in shape — neither carries any usage or token field, regardless of whether
the underlying model response had one. This matches `agy`'s own embedded
hooks documentation, which states the `PostInvocation` input is "Same as
`PreInvocation` input." So `dialect.yaml` maps no token fields for either
event, and antigravity publishes calls only; a tokens follow-up would need
`agy` to add usage data to this hook payload, or a different capture
mechanism.

**Model label.** The `model` label on `gen_ai.api.calls` comes from the
hook payload's per-invocation `modelName` (`dialect.yaml` maps it on
`PreInvocation`/`PostInvocation`), following design §3.2's precedence: the
payload's own value, then the agent's configured `SCION_MODEL`, then
`unknown`, truncated to 128 bytes. `SCION_MODEL` is only set when the
agent's config names a model, so without the payload value many antigravity
calls would carry no model at all. Note that `modelName` is `agy`'s display alias
for the configured model, not necessarily the concrete API model: this
project's own capture sent `gemini-3.1-pro-low` to `agy` and saw it call the
API as `gemini-3.1-pro-preview`. The alias is still the closest per-call
value the hook offers.

**Tool name.** `PostToolUse` repeats the `toolCall` object (`{name, args}`)
from `PreToolUse` in `agy` 1.2.12, so `dialect.yaml` maps `tool_name` from
`.toolCall.name` on both, and `agent.tool.calls` is labelled with the real
tool name.

## Implementation notes

`harnesses/antigravity/__init__.py` exists so this directory is a real
Python package: Python's standard library ships its own `antigravity`
module (the xkcd-353 easter egg, `Lib/antigravity.py`), which otherwise
shadows this directory and breaks
`python3 -m unittest antigravity.provision_test`.
