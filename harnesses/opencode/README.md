# OpenCode Harness Bundle

Scion harness configuration for [OpenCode](https://opencode.ai), an open-source
AI coding assistant.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/opencode
```

Or directly from GitHub:

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/opencode
```

## Auth Modes

| Mode | Env / File | Notes |
|------|-----------|-------|
| `api-key` (default) | `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` | Anthropic key takes precedence |
| `auth-file` | `~/.local/share/opencode/auth.json` | OpenCode native auth file |

## Bundle Layout

```
opencode/
  config.yaml       # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  Dockerfile         # Image build (FROM scion-base)
  cloudbuild.yaml    # Cloud Build configuration
  home/
    .config/opencode/opencode.json   # OpenCode client settings
```

## Telemetry

OpenCode has no usable native OTel usage signal (it emits `ai.streamText.doStream`
spans only when `experimental.openTelemetry` is explicitly enabled and only to
an externally configured OTLP endpoint), so model calls and tokens are
published from hooks instead. `provision.py` declares
`SCION_USAGE_SOURCE=hooks` (see `.design/hosted/usage-telemetry.md` §3.7 and
[the metrics docs](../../docs-site/src/content/docs/hosted/single-node/metrics.md)
for the full contract).

`home/.config/opencode/plugins/scion-bridge.js` subscribes to OpenCode's
generic `event` bus hook — not same-named keyed hooks, which never fire for
session, permission or model-usage events — and emits one `model-end` per
completed LLM step (a `step-finish` bus part), deduped on
`(sessionID, messageID, part.id)` with fork replays excluded. `dialect.yaml`
maps that event, plus `session.created`/`session.idle`/`session.error`/
`permission.asked`/`permission.replied`, onto the normalized Scion event
grammar. `tool.execute.before`/`tool.execute.after` remain real, directly
subscribed keyed hooks.

Known undercount: a model call that produces no `step-finish` part (a failed
or retried attempt, an abort, title generation, or agent generation) is not
counted.

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-opencode:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```
