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
  `COPILOT_OTEL_ENABLED=true`, `COPILOT_OTEL_EXPORTER_TYPE=otlp-http` and
  `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta`. No cloud
  endpoint, headers, or CA are copied into the harness env. `SCION_COPILOT_OTEL_ENDPOINT`
  is a **local-debugging escape hatch** only: setting it bypasses sciontool's
  redaction and identity stamping entirely, so it must never point anywhere
  but a local collector. `SCION_COPILOT_OTEL_PROTOCOL` does not bypass
  anything by itself — it only changes the wire format used to reach
  whichever endpoint is in effect, and exists so that a debug endpoint
  pointed at a non-protobuf collector can still be reached (the sciontool
  receiver itself accepts only `http/protobuf`).
- **Usage (`SCION_USAGE_SOURCE=native`)** — sciontool derives
  `gen_ai.api.calls`/`scion.usage.tokens` from Copilot's own metrics: calls
  from `gen_ai.client.inference.operation.input_tokens`'s per-export
  observation count (confirmed by a real capture: exactly one observation
  per model call), and tokens from the `gen_ai.client.inference.usage.*`
  counters (the pre-CLI-1.0.45 `gen_ai.client.token.usage` histogram is not
  supported). A real capture also shows every Copilot metric point is
  cumulative regardless of `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE`
  (Copilot documents no temporality override), and that `input_tokens` is not
  exclusive of cached tokens — sciontool's deriver converts cumulative points
  to per-export deltas and subtracts `cache_read`/`cache_write` from `input`
  itself; see `.design/hosted/usage-telemetry.md` §5 for the full contract.
  On the GCP provider, the raw metrics behind this derivation are consumed
  (removed from the request once derived) instead of being rejected for
  carrying attributes outside the Cloud allowlist; on generic OTLP they are
  forwarded unchanged alongside the derived counters. Every other native
  metric (for example `gen_ai.client.operation.duration`) is unaffected:
  rejected on GCP, forwarded on generic OTLP, exactly as before this rule
  existed.
- **System prompt is approximate** — system prompt content is prepended to
  `~/.copilot/copilot-instructions.md`; there is no native `--system-prompt` flag.
- **No project-scoped MCP** — project-scoped MCP server entries are demoted to
  global scope.
- **Subscription required** — an active GitHub Copilot subscription is required.
  The harness provisions successfully without one, but the CLI will fail with an
  auth error after launch.
