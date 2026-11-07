# Claude Harness Bundle

Scion harness configuration for [Claude Code](https://claude.ai/code),
Anthropic's coding agent CLI.

## Install

From a repository checkout:

```sh
scion harness-config install harnesses/claude
```

Or directly from GitHub:

```sh
scion harness-config install github.com/GoogleCloudPlatform/scion/tree/main/harnesses/claude
```

## Auth Modes

| Mode | Env / File | Notes |
|------|-----------|-------|
| `api-key` (default) | `ANTHROPIC_API_KEY` | Direct API access |
| `oauth-token` | `CLAUDE_CODE_OAUTH_TOKEN` | Generate with `claude setup-token` |
| `auth-file` | `~/.claude/.credentials.json` | Claude native credentials file |
| `vertex-ai` | `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_REGION` | Vertex AI with ADC or service account |

## Bundle Layout

```
claude/
  config.yaml        # Harness configuration (provisioner, capabilities, auth)
  provision.py        # Container-side provisioner (pre-start hook)
  capture_auth.py     # Interactive auth capture script
  Dockerfile          # Image build (FROM scion-base)
  init-firewall.sh    # Optional egress firewall, run by sciontool init (see below)
  cloudbuild.yaml     # Cloud Build configuration
  home/
    .bashrc                     # Shell config with scion env sourcing
    .claude.json                # Claude Code settings template
    .claude/settings.json       # Claude Code settings (hooks, permissions)
```

## Egress Firewall

`init-firewall.sh` restricts outbound traffic to GitHub, the npm registry,
Anthropic endpoints and the host network. It runs only at container start:
set `SCION_INIT_FIREWALL=1` in the agent or template environment and
`sciontool init` runs the script as root before anything else starts. If the
script fails, the agent does not start. Unset, empty or a false value
(`0`, `false`) means no firewall; any other value is an error.

The script changes iptables, so it needs the network administration
capability, which the broker grants only when metadata interception is on
(`assign` or `block` mode). With `passthrough` the container does not have
it, the script fails, and the agent does not start. The script cannot be run
later from inside the agent: in `assign` and `block` modes, init removes the
network administration capability once network setup is done.

The allow-list covers the host's /24 network but no other Hub address, so an
agent whose Hub is elsewhere cannot reach it with the firewall on.

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-claude:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```
