---
title: "v0.4.0-preview.2"
---

Released on 2026-11-13 (`v0.4.0-preview.1..v0.4.0-preview.2`, changes since `v0.4.0-preview.1`).

`v0.4.0-preview.2` adds experimental `scion reincarnate --harness` (`hub.reincarnate_harness_switch`) to move an agent to another harness-config in place with an Agent home reset, web chat cards for multiple-choice decisions, and opt-in `--wake` for scheduled messages. Hub operators can turn off the agents' SSH server, deny agent-local ports to every tunnel and refuse SSH socket forwarding under `server.hub.conduit`. `sciontool init` checks out workspaces with parallel workers, Runtime Brokers honor `container_hub_endpoint` for any Hub URL, terminal attach to a not-yet-connected agent is retried, and the antigravity harness moves to `agy` 1.5.7 with new model aliases. There are no new destructive migrations; the downgrade floor stays at `nightly-20261112`.

---

## Highlights

### 1. Switch an Agent's Harness-Config in Place

Behind the server-layer `hub.reincarnate_harness_switch` experiment (alpha, off by default), `scion reincarnate <agent> --harness <harness-config>` moves an agent to another harness-config while keeping its identity, workspace, branch and Hub history, and has its Runtime Broker delete and rebuild the Agent home. The harness session, transcripts, memory edits, installed tools and dotfile edits are lost, and there is no rollback. The Hub runs every check, including a credential preflight for the target harness, before anything is persisted or stopped, and `--dry-run` runs the same checks. See [Switching harness-config](/scion/local/agent-lifecycle/#switching-harness-config).

### 2. Choice Cards in Web Chat and Opt-In Wake for Scheduled Messages

Behind `web.chat_choices`, a multiple-choice prompt from an agent shows as a card in web chat that records single answers on one click and submits multi-decision sets at once. `scion schedule create` and `create-recurring` take `--wake` to resume a suspended target agent before a scheduled message is delivered, with the same option in the API and both web schedule forms.

### 3. Restricting What Users Reach Inside Agents and Faster Checkouts

`server.hub.conduit.disable_ssh`, `deny_ports` and `disable_ssh_socket_forwarding` let a Hub operator turn off the agents' embedded SSH server, deny specific agent-local ports to every tunnel, the port proxy and SSH forwarding, and refuse SSH Unix socket forwarding. `sciontool init` runs `git checkout` with parallel workers, cutting cold-start time on NFS-backed runtimes such as Cloud Run with Filestore.

---

## ⚠️ Breaking Changes
- **Antigravity model aliases:** The antigravity harness-config's `model_aliases` now map `small`, `medium`, `large` and `extra-large` to `gemini-4-hydrogen-low`, `gemini-4-hydrogen-medium`, `gemini-4-carbon-medium` and `gemini-4-carbon-high`, and the harness image installs `agy` 1.5.7 (from 1.5.4).
- **Downgrading to `v0.4.0-preview.1`:** The downgrade floor is unchanged at `nightly-20261112`. Because `v0.4.0-preview.1` was published before that release, self-update refuses it as a downgrade target (`below_downgrade_floor`). See [Upgrading and Rolling Back a Hub](/scion/reference/hub-upgrade-rollback/).

## 🔐 Security & Access Control
- **Restrict what users reach inside agents (#3655):** Three `server.hub.conduit` settings turn off the agents' embedded SSH server (`disable_ssh`), deny agent-local ports to every TCP tunnel, the port proxy and SSH forwarding (`deny_ports`, which wins over exposed ports and `tcp_allowed_ports`), and refuse SSH Unix socket forwarding (`disable_ssh_socket_forwarding`). The Hub sends them to each agent at create, start and restart as `SCION_CONDUIT_POLICY`; a refused SSH tunnel exits with code 5. They suit agents that share a container with local services such as a credential proxy, but are not a boundary against users who can run commands in the agent. See [Conduit (`server.hub.conduit`)](/scion/reference/server-config/#conduit-serverhubconduit).
- **Block identity ignores the project service account (#4768):** An agent whose GCP identity is explicitly `block` no longer counts the project's verified service account as a credential, so creating it falls back to the no-auth path instead of failing on a missing `ANTHROPIC_API_KEY`.
- **Cloud Run native flag cleared on best-effort delete and restore (#4766):** The native Cloud Run identity decision is cleared when a best-effort delete finalizes without a confirmed broker delete, and after a restore commits, so a restored agent no longer has its GCP tokens withheld. A failed mandatory dispatch still keeps it.

## 🤖 Agent Lifecycle & Workspaces
- **Faster workspace checkout (#4781):** `sciontool init` runs `git checkout` with parallel workers (git `checkout.workers`, default 16; set `SCION_GIT_CHECKOUT_WORKERS` to a value from 0 to 64, other values use the default). On a Filestore mount, a checkout of about 13,500 files took 138 s with one worker and 31 s with 16.
- **Reprovision keeps sciontool's `agent-info.json` keys (#4786):** Rewriting `agent-info.json` on reprovision carries over keys only `sciontool` writes (such as `toolName` and `activitySource`), and writes through a temporary file and rename so readers never see a truncated file.
- **Terminal attach to a not-yet-connected agent is retried (#4782):** On the legacy Cloud Run runtime, where an agent can show as running for minutes before it connects, a terminal opened in that window gets a retryable `503` with reason `agent_not_connected` instead of the final `runtime_attach_unsupported`.

## 💬 Chat & Messaging
- **Opt-in wake for scheduled messages (#4783):** `scion schedule create` and `create-recurring` take `--wake`, which resumes a suspended target agent before the message is delivered; the scheduled-events and schedules APIs and both web schedule forms have the same option. The wake needs the `agent.lifecycle` permission and is refused, with nothing started, when it is not held. Without the flag a message to a suspended agent still fails, and chat scheduled sends never wake.
- **Web chat automatic links in `scion-messaging` (#4769):** The bundled `scion-messaging` skill lists the forms web chat links on its own (`owner/repo#N` references, container file paths, `scion://artifact` references, `gs://` objects, URLs and email addresses) and the limits that apply.

## ☁️ Runtime Brokers, Settings & Operations
- **`container_hub_endpoint` applies to any Hub URL (#4770):** A Runtime Broker's `server.broker.container_hub_endpoint` is given to its Docker and Podman agents in place of the agent Hub endpoint even when that endpoint is a public `https://` URL, not only a loopback one. Runtime Brokers without it keep giving agents the public URL.
- **Starter Hub keeps operator settings on `--full` updates (#3422):** `gce-start-hub.sh --full` merges the generated `settings.yaml` over the existing file, so operator-owned sections (`server.plugins`, `server.message_broker`, `plugins`) survive, and aborts without overwriting if the merge fails.
- **Next release branch created with the stable tag (#4764):** `promote-stable.sh` creates `release/vX.(Y+1)` atomically with the stable tag so the weekly preview workflow has a line to roll forward; `--no-next-branch` pushes only the tag.

## 🧪 Experiments & Groundwork
- **`reincarnate --harness` (#4784, #4788, #4787):** Behind `hub.reincarnate_harness_switch`, a reincarnate request that names a harness-config moves the agent to it in place and asks its Runtime Broker to reset the Agent home; the old home is moved aside and purged in the background. Runtime Brokers on Docker, Podman, Apple container, Cloud Run and Cloud Run Instances runtimes advertise and perform the reset. The Hub refuses the request with `400 experiment_disabled` while the experiment is off, checks the target harness's credentials before anything changes, and refuses agents in worktree-per-agent projects.
- **Choice cards in web chat (#4771):** Behind `web.chat_choices`, a multiple-choice prompt shows as a card. A single-decision card records the answer on one click, or on Enter for the recommended choice; a multi-decision card (2 to 10 decisions) stages selections, counts what is still open and submits all answers at once. Cards show who answered, link a replaced decision to its newer version, and are disabled with a stated reason when you cannot answer; the `choice-response` message shows as a compact line. See [Multiple-Choice Decisions](/scion/hosted/user/messaging/#multiple-choice-decisions-experimental).
