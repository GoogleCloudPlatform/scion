# pi Harness Bundle

Scion harness configuration for [pi](https://pi.dev), a multi-provider terminal
coding agent (npm package `@earendil-works/pi-coding-agent`, pinned to 1.3.0).

> Status: the bundle is covered by unit tests for the provisioner and the
> bridge extension. It has not been exercised against a live pi under Scion;
> see [Known limits](#known-limits).

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/pi
```

## Auth Modes

| Mode | Env / File | Notes |
|------|-----------|-------|
| `api-key` (default) | `ANTHROPIC_API_KEY`, or any other pi provider key variable listed in `config.yaml` | Anthropic is listed first |
| `auth-file` | `~/.pi/agent/auth.json` (secret `PI_AUTH`) | Written 0600 and **never overwritten** if present, because pi refreshes OAuth tokens in place |

With no credentials the agent starts a shell with instructions (`no_auth`);
run `pi` and use `/login` interactively.

## Configuration

Everything has a safe default and is overridable per agent through env
(the `provision.py` step reads these) or by editing `settings.json`.

| Setting | Env var | Default | Notes |
|---------|---------|---------|-------|
| Project trust | `SCION_PI_PROJECT_TRUST` = `never` \| `ask` \| `always` | `never` | Highest precedence. Invalid values fail provisioning |
| Provider | `SCION_PI_PROVIDER` | unset | Sets `defaultProvider` |
| Model | `SCION_MODEL` | unset | A concrete `provider/id[:thinking]` sets `defaultProvider`/`defaultModel`. Size tiers (`small`/`medium`/`large`/`extra-large`) resolve via `model_aliases` in `config.yaml` and are passed with `--model` |
| Other pi settings | `SCION_PI_SETTINGS` (JSON object) | unset | Deep-merged into `settings.json`. Keys that look like credentials are rejected; it cannot change auth handling |
| Auto task completion | `SCION_PI_NO_AUTO_COMPLETE=1` | off | Bridge stops signalling `task_completed` on settle |
| Bridge debug | `SCION_HOOK_DEBUG=1` | off | Logs bridge failures to stderr |

Existing user values in `~/.pi/agent/settings.json` are preserved, and a
second provisioning run is a no-op.

### Project trust

pi can load extensions, skills and settings from a repository's `.pi/`
directory. Those run with the agent's privileges, so the default is
`never`: only the user-level agent directory is trusted. Set
`SCION_PI_PROJECT_TRUST=always` only for repositories you control.

## Lifecycle signals

`home/.pi/agent/extensions/scion-bridge.js` is a user-level pi extension.

| pi event | Scion event |
|----------|-------------|
| `session_start` | `session-start` |
| `input` (human/RPC) | `prompt-submit` |
| `tool_execution_start` / `_end` | `tool-start` / `tool-end` |
| `agent_end` | `agent-end` (carries a provider error) |
| `agent_settled`, not aborted, no error | `sciontool status task_completed` |

`ask_user` and `blocked` are not detectable from pi events. Instruct the
agent to run `sciontool status ask_user "<question>"` or
`sciontool status blocked "<reason>"` through its bash tool.

## Bundle Layout

```
pi/
  config.yaml        # Harness configuration (provisioner, capabilities, auth)
  provision.py       # Container-side provisioner (pre-start hook)
  scion_harness.py   # Generated copy of ../scion_harness.py (go generate ./harnesses/)
  capture_auth.py    # Captures ~/.pi/agent/auth.json as the PI_AUTH secret
  dialect.yaml       # Hook dialect for the bridge's events
  Dockerfile         # Image build (FROM scion-base), pinned pi version
  cloudbuild.yaml    # Cloud Build configuration
  scion-bridge.test.mjs, provision_test.py
  home/.pi/agent/
    settings.json                  # Safe defaults
    extensions/scion-bridge.js     # Scion lifecycle bridge
```

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-pi:1.3.0 -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```

The pi version is the `PI_VERSION` build arg (default `1.3.0`); never use a
floating tag. The install uses `--ignore-scripts`; transitive dependencies are
not pinned by npm's global install.

## Tests

```sh
cd harnesses/pi
python3 -m unittest provision_test
node --test scion-bridge.test.mjs
```

## Known limits

- **MCP:** pi supports stdio and streamable HTTP; SSE servers are skipped
  with a warning.
- **Usage/tokens:** no token mapping, so `SCION_USAGE_SOURCE` is not set.
- **Provider errors:** the bridge suppresses `task_completed` when the last
  assistant message has an error stop reason. This is untested against a live
  provider.
- **Not verified against a live pi:** terminal paste/Enter delivery, Escape
  interrupt, `agent_settled` fidelity, OAuth refresh.
  `ready` uses the repo-default `delay 8s` and `extra_submits: 2`; hook-based
  readiness and fewer extra Enters (pi steers on Enter while busy) are
  follow-ups once tested against a live pi.
