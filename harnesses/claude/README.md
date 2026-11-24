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
Anthropic endpoints, Visual Studio update/marketplace endpoints, and the
container's default gateway host (`/32`) plus any distinct broker-written host
alias (`host.docker.internal` / `host.containers.internal`). It runs only at
container start: when the agent's environment has `SCION_INIT_FIREWALL=1`,
`sciontool` runs the script as root before anything else starts. If the script
fails, the agent does not start. Unset, empty or a false value (`0`, `false`)
means no firewall; any other value is an error.

Key rules enforced by `init-firewall.sh`:

- **IPv6 default drop:** When `ip6tables` is available, IPv6 `INPUT`, `FORWARD`,
  and `OUTPUT` default policies are set to `DROP` (allowing loopback,
  established/related traffic, and IPv6 DNS nameservers from `/etc/resolv.conf`)
  with an explicit `OUTPUT` `REJECT` (`icmp6-adm-prohibited`) so dual-stack
  clients fall back to IPv4 immediately. If IPv6 is enabled on any non-loopback
  interface and `ip6tables` is unavailable, the script fails closed and refuses
  startup.
- **Gateway `/32` only:** Only the default gateway's `/32` address (and any
  distinct `/32` address resolved for `host.docker.internal` or
  `host.containers.internal`) is permitted. Sibling containers on the same
  Docker bridge `/24` are not reachable.
- **Outbound `tcp/22` off by default:** Arbitrary outbound SSH (`tcp/22`) is no
  longer unconditionally allowed. SSH to allowlisted hosts (such as GitHub)
  continues to work because the `allowed-domains` ipset rule permits all ports
  to allowlisted IPs. Operators can restore outbound `tcp/22` to any IPv4 host
  by setting `server.broker.init_firewall.allow_ssh: true` in the broker's
  `settings.yaml`.
- **Operator domain allowlist:** Operators can add extra allowed hostnames
  without rebuilding the image via `server.broker.init_firewall.allow_domains`
  in the broker's `settings.yaml` (up to 64 unique lowercase DNS hostnames;
  wildcards, IP literals, CIDRs, `localhost`, and metacharacters are rejected
  at broker startup). The allowlist is operator-authoritative: the runtime
  passes `SCION_INIT_FIREWALL_ALLOW_DOMAINS` and `SCION_INIT_FIREWALL_ALLOW_SSH`
  only into the short-lived `sciontool netns-setup` container's argv from
  broker configuration, strips all `SCION_INIT_FIREWALL_*` variables from the
  agent container environment, and ignores them on in-container `sciontool init`
  fallback paths. Users, templates, harness-configs, secrets, and parent agents
  cannot widen the allowlist.
- **IP-set limitation and CDN warning:** Domain names are resolved at container
  startup via `getent ahostsv4` into an IPv4 ipset (IPv6 AAAA records are not
  allowlisted). Adding a domain hosted behind a shared CDN or multi-tenant
  front-end (for example Cloudflare, Fastly, or CloudFront) permits traffic to
  any other service sharing those resolved edge IPs, and DNS rotation after
  startup is not refreshed until the container restarts.

Set the variable in one of these places:

- the agent's own config: the `env` of an inline `--config` file, or
  `config.env` in the agent-create request;
- harness-config env: the `env:` block of this harness-config's
  `config.yaml`, `harness_configs.<name>.env` in `settings.yaml`, or
  `profiles.<profile>.harness_overrides.<name>.env`.

A template's `env:` block cannot set it: templates may not set `SCION_*`
keys, so the key is dropped with a warning and the firewall stays off. Hub
env (`scion hub env set`) and environment-type secrets cannot set it either,
because the Hub refuses and drops every `SCION_*` name.

With native identity (the agent uses its runtime platform's own GCP
identity, for example a Cloud Run service account) the metadata mode is
passthrough. A requested init firewall then either fails, because the
container lacks the network administration capability, and the agent does
not start; or it installs and blocks the metadata server and Google APIs, so
the native identity cannot be used. Nothing refuses this combination.

The script changes iptables, so it needs the network administration
capability, which the broker grants only when metadata interception is on
(`assign` or `block` mode). With `passthrough` the broker does not grant it,
so the script normally fails and the agent does not start; if the runtime
grants it anyway, the firewall installs and init then removes the
capability. After a successful removal, the script cannot be run later from
inside the agent: init removes the network administration capability once
network setup is done, in `assign` and `block` modes and whenever the
firewall was installed, before any workload runs. If the firewall or
metadata interception was installed and that removal fails, the agent does
not start; otherwise a failed removal is only logged.

The allow-list covers only the default gateway's `/32` address (and distinct
host-alias `/32` addresses), not the wider `/24` subnet or any other Hub
address, so sibling containers on the same bridge and a Hub elsewhere cannot
be reached with the firewall on.

## Build the Image

```sh
# Local Docker build
docker build --build-arg BASE_IMAGE=scion-base:latest -t scion-claude:latest -f Dockerfile .

# Cloud Build
gcloud builds submit --config cloudbuild.yaml .
```
