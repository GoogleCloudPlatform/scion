# Copilot Harness Bundle

Scion harness bundle for the [GitHub Copilot CLI](https://github.com/github/copilot-cli)
(`copilot` from `github/copilot-cli`).

## Bundle Layout

```
harnesses/copilot/
  config.yaml           # Harness configuration
  provision.py          # Container-side provisioner (pre-start hook)
  capture_auth.py       # Post-login credential capture
  Dockerfile            # Image build (FROM scion-base)
  cloudbuild.yaml       # Cloud Build configuration
  README.md             # This file
  home/
    .bashrc             # Shell initialization
    .copilot/
      settings.json     # Default settings (auto-update off)
```

## Installation

```bash
scion harness-config install harnesses/copilot
```

## Authentication

The Copilot CLI requires a GitHub account with an active Copilot subscription.

### Fine-Grained PAT (Recommended)

Create a [fine-grained personal access token](https://github.com/settings/personal-access-tokens)
with the **"Copilot Requests"** permission enabled. The token must be user-owned
(not organization-owned).

```bash
scion start --harness copilot --env COPILOT_GITHUB_TOKEN=github_pat_...
```

Token precedence: `COPILOT_GITHUB_TOKEN` > `GH_TOKEN` > `GITHUB_TOKEN`.

**Note:** Classic PATs (`ghp_...`) are not supported by the Copilot CLI.

### Interactive Login (No-Auth Fallback)

If no token is provided, the agent drops to a shell. Run `copilot login` to
authenticate via browser-based OAuth device flow, then capture credentials:

```bash
python3 /home/scion/.scion/harness/capture_auth.py
```

## Known Limitations

- **No turn/model-call limits** — Copilot CLI has no hook dialect for individual
  turn or model call events. Only `max_duration` (via Scion's external timeout)
  is supported.
- **Native telemetry routes to the local receiver only** — when telemetry is
  enabled, `provision.py` always points Copilot's native OTel exporter at
  sciontool's local OTLP/HTTP receiver (`http://127.0.0.1:${SCION_OTEL_HTTP_PORT:-4318}`,
  `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`, the only exporter type and wire
  format Copilot CLI v1.0.88 and the receiver both support), setting
  `COPILOT_OTEL_ENABLED=true` and `COPILOT_OTEL_EXPORTER_TYPE=otlp-http`. No
  cloud endpoint, headers, or CA are copied into the harness env. Copilot's raw
  native metrics (`gen_ai.client.token.usage` and friends) are still rejected
  on the GCP provider until sciontool's usage deriver ships (a later phase);
  logs are forwarded and redacted like any other harness's. `SCION_COPILOT_OTEL_ENDPOINT`
  / `SCION_COPILOT_OTEL_PROTOCOL` remain as a **local-debugging escape hatch**
  only — setting either bypasses sciontool's redaction and identity stamping
  entirely, so they must never point anywhere but a local collector.
- **System prompt is approximate** — system prompt content is prepended to
  `~/.copilot/copilot-instructions.md`; there is no native `--system-prompt` flag.
- **No project-scoped MCP** — project-scoped MCP server entries are demoted to
  global scope.
- **Subscription required** — an active GitHub Copilot subscription is required.
  The harness provisions successfully without one, but the CLI will fail with an
  auth error after launch.
