---
title: Approved Commands
description: Let agents propose commands for a named human to review, approve, and run once in that human's execution container.
---

**Approved commands** let an LLM agent propose a shell command or script to a named human **approver**, who reviews the exact bytes and either approves or dismisses it. Approved commands run **at most once** inside the approver's **execution container** over Conduit, keeping human credentials inside the execution container's `$HOME` and out of reach of LLM agents.

:::note[Experimental]
Approved commands are gated by the `hub.command_requests` experiment (alpha, off by default) and require `hub.exec_containers` and `hub.conduit`. See [Experiments — `hub.command_requests`](/scion/reference/experiments/#server-experiments-with-notes).
:::

## Prerequisites

1. A Hub administrator enables `hub.conduit`, `hub.exec_containers`, and `hub.command_requests`.
2. The human approver starts an execution container (`shell` harness-config) and registers it as their default execution container:

```bash
scion exec-container set <execution-container-name>
```

Execution containers started before `hub.command_requests` was enabled do not advertise the Conduit exec routes until restarted.

## Logging in inside your execution container

Inside an execution container, the `scion` CLI runs in **human mode** rather than agent mode. At start, `sciontool` isolates PID 1's agent identity from the workload and seeds `~/.scion/context.yaml` (if absent) with the container's Hub endpoint, so you can authenticate as yourself with zero flags:

```bash
scion login
```

Execution containers started before the Hub, Runtime Broker, and image were upgraded keep the previous behavior (agent mode with no `scion login`, `SCION_*` variables visible, and `~/.scion/scion-token` in `$HOME`) until restarted on an upgraded Runtime Broker and image.

- **Zero-flag device flow:** `scion login` (a top-level alias for `scion hub auth login`) detects the headless container environment and runs the OAuth device authorization flow against the seeded Hub endpoint. Your browser completes verification directly with the OAuth provider (Google or GitHub) while the CLI polls the Hub, so your browser does not need direct access to an internal Hub URL.
- **Separate credentials:** Your login is stored in `~/.scion/credentials.json` keyed by the container's Hub URL (typically the internal route configured by the Hub operator). It is separate from your workstation login to the public Hub URL.
- **Registering from inside the container:** Because the workload runs in human mode, you can also run `scion exec-container set <execution-container-name>` directly from the container's shell (as shown in the login MOTD).
- **Switching Hubs:** Because `SCION_HUB_ENDPOINT` and `SCION_HUB_URL` are not set in the workload environment, `scion use --hub <hub>` works normally and updates `~/.scion/context.yaml`. On runtimes with a persistent `$HOME`, `sciontool` never overwrites an existing `~/.scion/context.yaml` on restart.

### What approved commands can and cannot use

- **Cannot use:** Approved commands (as well as the interactive tmux shell, Conduit SSH sessions, and sidecar services) have no `SCION_*` control environment variables — except `SCION_TRIGGER_CONTEXT`, which is injected per-run only for trigger-routed commands. `~/.scion/scion-token` is removed before the workload starts: on root-init runtimes approved commands cannot read PID 1's runtime agent token (stored root-only), while on shared-UID or no-privilege-drop runtimes there is no OS boundary, so isolation removes the CLI-discoverable token path and relies on the Hub's 3-scope runtime token cap. Approved commands also cannot use the agent delegation helper or auto-exposed ports, and receive no project GitHub App token or project default GCP identity. Before you run `scion login`, `scion hub status` reports `Not authenticated` rather than falling back to the agent token (and `scion whoami` prints the container's OS user rather than an agent identity).
- **Can use:** Approved commands run as the container user (`scion`, where the runtime allows a privilege drop) and can use any CLI credentials you sign in to inside `$HOME` (such as `gh`, `gcloud`, `kubectl`, and your `scion` login once you run `scion login`), along with user-scope environment variables and secrets (non-`SCION_*` names).

:::caution[Approval card is the security boundary]
After you run `scion login`, your user OAuth credential sits in `~/.scion/credentials.json` under the same OS user as approved commands. Any command you approve can therefore run `scion` as you, just as it can use `gh` or `gcloud` credentials in `$HOME`. Always review the exact script, working directory, and SHA-256 hash on the approval card before approving, and keep credentials in `$HOME` — never in `/workspace`, which may be shared with LLM agents.
:::

### Known limitations

- **Cloud Run in-place restarts (`ptone/scion#9821`):** On Cloud Run, an in-place platform restart replays the creation-time agent and transport tokens because refreshed tokens are not persisted back to `$HOME`. Once the original creation-time token expires, a platform-restarted container can fail Hub authentication until it is restarted or `scion reset-auth <agent>` is run.
- **Proxy-auth Hubs (`ptone/scion#9808`):** When the Hub sits behind IAP or Cloud Run invoker proxy authentication, `sciontool` seeds `transport: {mode, audience}` into `~/.scion/context.yaml` (without copying PID 1's transport token) and prints a MOTD reminder, but the `scion` CLI does not yet apply the `transport` block from the current-hub context entry.
- **Plain HTTP internal Hub URL:** The seeded Hub endpoint comes from PID 1's `SCION_HUB_ENDPOINT` (the container-reachable agent route), which may be plain `http://` on an internal cluster or VPC network; in that case, device-flow polling and subsequent user OAuth tokens travel unencrypted across that internal network segment.
- **Linked project precedence:** If you run `scion login` or `scion hub auth login` from inside a directory linked to a Hub project (for example, bound to a public Hub URL via `scion hub link`), that linked project's Hub URL takes precedence over the seeded `current_hub` in `~/.scion/context.yaml` (see [Hub resolution order](/scion/reference/cli/#global-flags)). Run `scion login` from `$HOME`, or pass `--hub <nickname>` (run `scion hub list` to see the seeded nickname, derived from the endpoint host) to target the seeded Hub explicitly.
- **`sciontool doctor` inside the shell:** Running `sciontool doctor` from the interactive shell inside an execution container reports that no agent token is found. This is expected because PID 1's runtime token is isolated from the workload user.
- **Planted directory or symlink on a persistent `$HOME`:** PID 1 fails closed and refuses to start the workload if it cannot unlink `~/.scion/scion-token` (or `~/.scion/transport-token`) — for example, if a directory is planted at `~/.scion/scion-token`, `~/.scion` itself is a symlink, or `~/.scion` is not writable. Fix the persistent home directory on the host and restart the container. (A leaf symlink at `~/.scion/scion-token` is refused and unlinked rather than followed.)
- **Pre-cap child agents:** Any pre-existing child agents created by an execution container before the runtime scope cap shipped keep their stored-role authority at delegation checks. Review and delete such child agents, or have a Hub super-admin narrow them with `scion reset-auth <child> --reissue-scopes`.
- **Legacy tokens on running execution containers:** After a Hub upgrade, already-running execution containers keep their previous broader token until the next automatic refresh (up to roughly 8 hours under the default 10-hour token TTL). Restart the execution container to mint a capped token immediately, or have a Hub super-admin run `scion reset-auth <agent> --reissue-scopes` without restarting.

### Verifying isolation (operators)

`sciontool` selects the runtime token store by privilege boundary rather than runtime name (check the `sciontool` init log if unsure — the in-memory fallback logs a warning):

- **Root-init runtimes (PID 1 starts as root and drops to the workload UID — for example Docker, rootful Podman, Apple Container, and Kubernetes with privilege drop):** Inspect the runtime token store as root (for example, `docker exec -u 0 <container> stat /run/sciontool/auth/scion-token`). `/run/sciontool/auth` should be `0700 root:root` and `/run/sciontool/auth/scion-token` (plus `/run/sciontool/auth/transport-token` when proxy transport is active) should be `0600 root:root`. From the workload user (`scion`), `~/.scion/scion-token` must not exist and reading `/run/sciontool/auth/scion-token` must fail with `Permission denied`.
- **Shared-UID / no-privilege-drop runtimes or read-only `/run` (PID 1 already runs as the workload UID, lacks `CAP_SETUID`, or cannot write `/run` — for example rootless Podman with `keep-id`, non-root Kubernetes, and Cloud Run):** PID 1 keeps the runtime token in memory only (nothing is written under `/run/sciontool/auth/` or `$HOME`) and logs a warning naming the in-memory fallback. There is no OS boundary on these runtimes; `~/.scion/scion-token` is still removed before the workload starts (leaving no CLI-discoverable token path), and the Hub's runtime scope cap (`agent:status:update`, `agent:token:refresh`, `agent:port:forward`) bounds token authority.

## Proposing a command (agents)

Inside an agent container (agent mode), propose a command with `scion message --command-request`:

```bash
# Direct message — the DM recipient is the approver
scion message @alice@example.com --command-request deploy-preview \
  --command 'gcloud run deploy preview --region us-central1' \
  --description "Deploy PR 42 preview" \
  --timeout 15m

# Multi-line script from a file in a group conversation or thread (--approver required)
scion message conv:<conversation-id> --command-request db-migrate \
  --command-file ./scripts/migrate.sh \
  --approver @alice@example.com \
  --workdir /home/scion/repo
```

Key proposal rules:

- **Targets & single approver:** Command requests can be sent in a direct message to a user (who is automatically the approver) or to a conversation/thread with `--approver <@email>` (email address or user ID; display names are not accepted) naming one active, non-visitor human participant. Agents and message groups cannot be recipients or approvers.
- **Working directory:** Defaults to `$HOME` (`~`). An explicit `--workdir` must be an absolute, clean path; paths under `/workspace` or shared directories are warned on because LLM agents can write to them.
- **Timeout:** Defaults to 10 minutes (`600s`), with a range of `1s` to `60m` (`3600s`).
- **Script bounds:** Up to 4,096 bytes of valid UTF-8, with no NUL, control characters (other than `\n` and `\t`), bidirectional controls, or invisible/non-ASCII whitespace.
- **Versioning (`--update`):** Reusing a pending key without `--update` returns `409 command_request_key_pending`. Pass `--update` to create the next version and mark the previous pending version `replaced`.
- **Pending cap:** Each agent may have at most **10 pending** command requests at a time (`429 command_request_limit`). Updating an existing pending key keeps the net count unchanged.
- **Inspect & withdraw:** Agents can check status and run outcomes with `scion command request list [--key <key>] [--json]` and retire a pending request with `scion command request withdraw <key> [--reason <text>]`.

## Reviewing, approving, and dismissing (humans)

Only the single named **approver**, authenticated with an interactive human session (browser session or interactive CLI login — User Access Tokens, agent tokens, visitors, and federated credentials are refused with `403`), can approve or dismiss a command request. Other conversation participants can read the request and run status, but cannot approve, dismiss, or view the approver's execution container or command output.

### Approve from the CLI

```bash
scion command request approve <request-id>
```

On an interactive terminal, the CLI prints the exact script (with line numbers, visible tabs/trailing spaces, and escaped non-ASCII characters), working directory, timeout, target execution container, and canonical SHA-256 hash, then prompts `[y/N]`. Non-interactively (or with `--json`), pass `--confirm-sha256 <64-char-hex>` matching the request's `command_sha256`.

A `stopped` execution container is never auto-started (`409 exec_container_stopped`); start it manually and approve again.

### Dismiss from the CLI

```bash
scion command request dismiss <request-id> --reason "Not needed right now"
```

Dismissing transitions a `pending` request to `dismissed`, notifies the requesting agent, and is idempotent if repeated. Reasons are capped at 500 characters.

## Lifecycle and execution guarantees

- **At-most-once execution:** Approval atomically transitions `pending -> approved` (checking `command_sha256` and expiry) and inserts a single `command_runs` row with a `UNIQUE(request_id)` constraint. Concurrent approve, dismiss, and withdraw calls race on the same `pending` compare-and-set so at most one succeeds.
- **24-hour expiry:** Unresolved `pending` requests expire automatically after 24 hours.
- **Run outcomes:** Each run finishes as `succeeded`, `failed`, `not_started`, or `unknown` (killed mid-flight or lost after dispatch — never treated as "did not run"). The Hub sends one structured `command-response` message back to the requesting agent and retries transient delivery failures for up to 1 hour.
- **Agent deletion:** When a requesting agent is deleted (soft-deleted, hard-deleted, or purged), its `pending` command requests are automatically withdrawn (`withdrawn_by: system`). Already-approved requests and their `command_runs` rows remain intact.
- **Audit trail:** Every request, replacement, approval, dismissal, withdrawal, refusal, expiry, and run transition emits a `MessageActionEvent` with allow-listed metadata (`command_sha256`, `exec_agent_id`, `run_id`, etc.). Script text and command output are never written to audit logs or server logs.
