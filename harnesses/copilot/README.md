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
- **No telemetry integration** — Copilot's OpenTelemetry configuration surface
  is undocumented.
- **System prompt is approximate** — system prompt content is prepended to
  `.github/copilot-instructions.md`; there is no native `--system-prompt` flag.
- **No project-scoped MCP** — project-scoped MCP server entries are demoted to
  global scope.
- **Subscription required** — an active GitHub Copilot subscription is required.
  The harness provisions successfully without one, but the CLI will fail with an
  auth error after launch.
