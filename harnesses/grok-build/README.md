# Grok Build Harness Bundle

Scion harness bundle for [xAI's Grok Build CLI](https://x.ai/)
(`grok` from xAI).

## Bundle Layout

```
harnesses/grok-build/
  config.yaml           # Harness configuration
  provision.py          # Container-side provisioner (pre-start hook)
  provision_test.py     # Unit tests for provision.py
  capture_auth.py       # Post-login credential capture
  dialect.yaml          # Grok hook event → sciontool normalized events
  scion_harness.py      # Vendored shared library
  Dockerfile            # Image build (FROM scion-base)
  cloudbuild.yaml       # Cloud Build configuration
  README.md             # This file
  home/
    .bashrc             # Shell initialization
    .grok/
      config.toml       # Seed config (auto-update off, telemetry off)
```

## Installation

```bash
scion harness-config install harnesses/grok-build
```

## Authentication

The Grok Build CLI requires an xAI account with API access.

### API Key (Recommended)

Set `XAI_API_KEY` with a valid xAI API key:

```bash
scion start --harness grok-build --env XAI_API_KEY=xai-...
```

### Auth File

Use `~/.grok/auth.json` produced by `grok login --device-auth`:

### Vertex AI

Use GCP Vertex AI authentication by providing `GOOGLE_CLOUD_PROJECT` and a
region (`GOOGLE_CLOUD_REGION` / `CLOUD_ML_REGION` / `GOOGLE_CLOUD_LOCATION`),
plus Application Default Credentials (ADC).

| Mode | Credential | Setup |
|---|---|---|
| API Key | XAI_API_KEY | Set env var with xAI API key |
| Auth File | ~/.grok/auth.json | `grok login --device-auth` + capture |
| Vertex AI | GOOGLE_CLOUD_PROJECT + region | GCP project and region env vars + ADC |

### Interactive Login (No-Auth Fallback)

If no credentials are provided, the agent drops to a shell. Run
`grok login --device-auth` to authenticate, then capture credentials:

```bash
python3 /home/scion/.scion/harness/capture_auth.py
```

## Configuration

- **Config directory**: `~/.grok/` (settings in `config.toml`).
- **Instructions**: `agent_instructions` and `system_prompt` are projected
  into `AGENTS.md`. Grok has no native system-prompt flag, so the system
  prompt is *prepended to the instructions file*.
- **MCP**: `~/.grok/config.toml` under `[mcp_servers.*]` sections.
  Project-scoped MCP servers are not supported (demoted to global).
- **Model aliases**: `small` → `grok-3-mini`, `medium` → `grok-3`,
  `large` → `grok-4`, `extra-large` → `grok-4`.
- **Hooks**: Grok events are wired to sciontool via
  `~/.grok/hooks/scion.json` using the `grok-build` dialect.

## Known Limitations

- **No max_model_calls support** — Grok hooks do not expose model-call
  start/end events. Only `max_turns` and `max_duration` are supported.
- **System prompt is approximate** — system prompt content is prepended to
  `AGENTS.md`; there is no native `--system-prompt` flag.
- **No project-scoped MCP** — project-scoped MCP server entries are
  demoted to global scope.
- **xAI API key, auth file, or Vertex AI required** — an xAI account
  with API access (or GCP Vertex AI credentials) is required. The harness
  provisions successfully without one, but the CLI will fail with an auth
  error after launch.
