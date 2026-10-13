# Codex Harness Bundle

Scion harness configuration for [Codex](https://github.com/openai/codex),
OpenAI's coding agent CLI.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/codex
```

Or directly from GitHub:

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/codex
```

## Auth Modes

| Mode | Env / File | Notes |
|------|-----------|-------|
| `api-key` (default) | `CODEX_API_KEY` or `OPENAI_API_KEY` | Codex key takes precedence |
| `auth-file` | `~/.codex/auth.json` | Codex native auth file |

## Bundle Layout

```
codex/
  config.yaml       # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  Dockerfile         # Image build (FROM scion-base)
  cloudbuild.yaml    # Cloud Build configuration
  home/
    .bashrc                    # Shell config with scion env sourcing
    .codex/config.toml         # Codex client settings (model, otel, etc.)
    .codex/scion_notify.sh     # Notification hook script
```

## Hooks

`home/.codex/config.toml` enables codex hooks (`[features] hooks = true`) and
registers `SessionStart`, `SessionEnd`, `UserPromptSubmit`, `PreToolUse`,
`PostToolUse`, `Stop` and `SubagentStop`. Each one runs `scion_notify.sh`,
which forwards the payload to `sciontool hook --dialect=codex`, mapped by
`dialect.yaml`. The legacy `notify` callback (`agent-turn-complete`) is wired
too.

`SubagentStart`, `PermissionRequest`, `PreCompact`, `PostCompact` and
`Interrupt` are left unregistered: codex supports them, but the dialect has no
mapping for them. This was verified against `@openai/codex` 0.162.1. The image
installs `@openai/codex@latest`, so re-check the list when codex changes its
hook events.

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-codex:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```
