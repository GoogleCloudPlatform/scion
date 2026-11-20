# Release Notes (2026-11-18)

This day added an idle auto-suspend sweep, let agents withdraw decisions they no longer need, and made credential revocation part of an agent's hard delete.

## 🚀 Features
* **Idle auto-suspend (experimental)** (#5012): With both the `lifecycle.idle_suspend` experiment and `server.hub.auto_suspend_idle` on (each off by default), the Hub suspends an agent that has been idle, completed or in a harness error for `server.hub.idle_suspend_after` (default 15 minutes), records the suspend reason `idle`, and wakes the agent automatically when new work arrives for it. Blocked agents are never auto-suspended. The docs advise against using it in production until recovery from a crash or failed stop during an idle suspend is in place (ptone/scion#8204).
* **Agents withdraw obsolete decisions (experimental)** (#5067): Under `web.decisions`, the asking agent can retire open decisions it no longer needs with `scion decisions withdraw <key>... [--set] [--reason <text>]` (`POST /api/v1/agent/decisions/withdraw`). `--set` also withdraws every other pending decision in the same prompt. Only the agent's own pending decisions with no answers can be withdrawn.
* **GitHub trigger definitions and presets (experimental)** (#5004, #5010): Behind `hub.github_triggers`, the Hub defines triggers and webhook endpoints, and adds a registry of GitHub trigger presets with routes to list and compile them.
* **Header mode switcher says Terminals** (#5007): The switcher now reads Console, Chat and Terminals, and the Terminals entry shows how many terminal sessions are open.

## 🔒 Security
* **Agent credentials revoked atomically on hard delete** (#5002): A hard-deleted agent's remaining active credentials are revoked in the same transaction that deletes its row. The compensation for a failed create also runs as one transaction and writes an `agent_create_dispatch_failed` audit record. Unused agent auth middleware was removed.

## 🐛 Fixes
* **Web create checks the creator's secrets like start does** (#5013): When the web create form checks for missing credentials, it now counts the creating user's secrets only where a start would actually deliver them, so the check and the start agree.
* **`scion hub brokers info` lists projects** (#5040): The command now fetches and shows the projects a broker provides, where it always printed `Projects: none`. If the fetch fails, it reports the error instead.
* **Missing Secret Manager payloads are errors** (#5033, #5036): A Secret Manager response without a payload now fails with an error naming the version, both for normal and pending-secret reads, instead of returning an empty value.
