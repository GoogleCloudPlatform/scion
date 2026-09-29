# Release Notes (2026-09-28)

Two security fixes led the day: agent-written files could run scripts with a viewer's hub session, and the hub logged its database password on every boot. Harness usage telemetry moved to a single canonical contract with correct Claude attribution, `scion reincarnate` now handles shared-workspace agents, and the grove→project rename is complete.

## ⚠️ BREAKING CHANGES
* **Hook token metrics retired** (#2057): Hook-side usage now emits `scion.usage.tokens{token_type}` instead of the three `scion.hook.tokens.*` counters, and the receiver rejects the old names. Hook usage is only recorded when `SCION_USAGE_SOURCE=hooks`, so codex, gemini-cli, and muse-code stop publishing hook usage until they declare a source. Dashboards and alerts on `scion.hook.tokens.*` must switch.
* **hubclient request types ignore `groveId`** (#2040): Create agent, notification subscription, subscription template, template, clone template, and token requests honor only `projectId`.

## 🚀 Features
* **Canonical usage telemetry** (#2051, #2057): A shared telemetry contract (`gen_ai.api.calls` and `scion.usage.tokens{token_type}`) is used by sciontool and the hub. Exporters stamp project and agent identity on every point from authoritative sources. sciontool derives Claude calls and tokens from Claude's native events. The Hub dashboard now counts cumulative increases per series instead of summing every flushed point, so Claude usage is attributed correctly. This requires a scion-base rebuild.
* **Reincarnate for shared-workspace and hub-managed agents** (#2037): `scion reincarnate` now works for these agents while preserving the agent row, identity, and shared checkout, without restarting sibling agents. Worktree-per-agent agents are refused with 400. Reincarnate is now authorized by `agent.lifecycle` instead of `agent.update`.
* **`scion keys` in agent mode** (#2050): `scion keys` is hub-aware and usable inside agent containers.

## 🔒 Security
* **Agent-written files ran with the viewer's session** (#2009): Workspace and shared-dir files opened with `?view=true` were served inline on the hub origin, so agent-written HTML or SVG could run scripts with the viewer's hub session. Workspace, shared-dir, WebDAV, and chat attachment responses now send a sandbox CSP and `nosniff`, and WebDAV reads download as attachments. The file browser's HTML preview still works. *(Credit: miller79)*
* **Database password in logs** (#2035): The hub logged its full database DSN, including the password, on every boot, and `server recover-authz` and `migrate-storage` printed it too. On Cloud Run this reached Cloud Logging. Passwords are now masked in URL and keyword DSNs, including Cloud SQL socket hosts and query-string credentials.
* **Raw keystrokes limited to the sender's project** (#2050): Agents can only send raw keystrokes to agents in their own project.

## 🐛 Fixes
* **"Default runtime broker is unavailable" on multi-instance hubs** (#2046): When the hub instance holding broker affinity disconnected (scale-in or revision retirement), project providers were marked offline and nothing restored them. Each instance now periodically re-marks providers online for brokers it holds a live connection to.
* **Terminal behavior for stopped agents** (#2044, #2049, #2045): Attaching to a stopped agent now ends immediately with a terminal `agent_stopped` close, instead of holding the connection for 30 seconds and then asking clients to retry. After a stopped or crashed agent is restarted, the web terminal reconnects on its own, and agents in the error phase are treated as stopped. `scion attach` no longer prints the full usage block when a session ends abnormally.
* **Raw and plain message delivery** (#2043, #2053): `scion message --raw` and `--plain` from an agent delivered a rendered envelope instead of the bare text or keys; the flags now reach the dispatcher. The message endpoint also honors top-level `raw` and `plain` flags. *(#2053 contributor: G. Hussain Chinoy)*
* **Claude harness version guard** (#2052): Guards Opus 5.5 on Claude Code versions older than 2.1.280 and disables the auto-updater inside containers. *(Contributor: G. Hussain Chinoy)*

## 📖 Docs
* **Nightly and weekly notes** (#2039): Nightly update for 2026-09-27, plus weekly release notes for Sep 21–27 with a Breaking Changes section covering the grove removals.
* **Grove rename wrap-up** (#2036): Rewords the remaining "grove" mentions in docs and glossaries as history and points to the migration guide.
