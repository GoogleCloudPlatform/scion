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
