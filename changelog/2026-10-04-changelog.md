# Release Notes (2026-10-04)

Raw message delivery is gone; agent keys replace it. Asynchronous agent create became usable end to end as an opt-in, and the web UI now renders the Hub's backend-driven agent delete. Large deployments get bounded agent lists. Runtime Broker routing and availability fixes, user access token boundary enforcement and the last mobile layout phase round out the day.

## ⚠️ BREAKING CHANGES
* **Raw message delivery removed** (#2434): The raw bridge, the CLI `--raw` alias and the `raw` field on messages are gone. Every message ingress (Hub, DMs, broadcasts, schedules, Runtime Brokers, plugins) rejects a raw field in any form with 422 `raw_input_removed`, before any side effect. Clients must switch to the agent keys API (`POST .../keys`, `scion keys`).
* **Runtime-profile timezone retired** (#2400): `profiles.<name>.timezone` is removed. A server-config PUT that includes it returns 422 and points to `agent_defaults.default_timezone`. At boot, DB-tier values are migrated or dropped with warnings. File-tier values only produce a warning, and `settings.yaml` is never rewritten.
* **`harness-config sync` and `push` default to project scope** (#2445): Use `--global` to target `~/.scion/harness-configs`.
* **Invalid workspace storage config stops the Hub at startup** (#2456): Workspace storage settings, including a single `subpath_root` default, are validated at boot.
* **Server config saves reject edited masked placeholders** (#2453): An API save that sends a masked secret placeholder now returns 400, unless the block it belongs to exactly matches the stored value. To edit a block that contains a secret, send every secret in that block in clear. The web UI is unaffected.
* **Antigravity thinking-level cut points changed** (#2384): Levels map 0-25 to low, 26-50 to medium and 51-100 to high. Codex output is unchanged. Both harnesses now read a declarative `thinking:` map from config.yaml.

## 🚀 Features
* **Asynchronous agent create, opt-in** (#2420, #2360, #2455): With `server.hub.async_agent_launch` on, the Hub acknowledges a create at once and the agent stays provisioning while the Runtime Broker launches it. `scion start` and `resume` opt in and wait, printing progress steps, with `--no-wait` and `--wait-timeout`. Start paths answer 409 `agent_launching` or `agent_create_incomplete` while a create is pending or has failed.
* **Agent delete in the web UI** (#2446, #2466, #2415): Agents show "Deleting…", "Delete interrupted" and "Delete failed" states. Pages wait for the SSE `deleted` event on a 202. A failure banner offers Retry and Force. Each agent run now has its own `run_id`, so a stale delete can't remove a recreated agent with the same name (enforced end to end on Docker for now).
* **Bounded agent lists on large deployments** (#2451, #2439): The agents page, project page and home dashboard page through server-sorted results instead of loading every agent. Tree and graph views load at most 4 pages of 500 and mark the list as capped beyond that. Above 2,000 agents the dashboard shows counts only. A shared frontend agent store now backs the chat palette.
* **Recorded run intent and queued stops** (#2422): Each agent records whether it should be running, separately from its observed phase. Stopping an agent on an offline Runtime Broker returns 202 and queues the stop. Start, stop, suspend and restart responses carry warnings and a queued flag.
* **`scion reincarnate --broker --dry-run`** (#2441): Reports whether an agent could move to another Runtime Broker on the same NFS export, after nine ordered checks. A real move isn't supported yet (501).
* **Empty-per-agent projects on Kubernetes NFS** (#2419, #2418): Each agent gets its own directory on the export, replacing the previous fail-closed behavior. Unsupported dispatches return a clear 412 that names the Runtime Brokers that work.
* **Auto-expose ports follow one precedence everywhere** (#2430): Config PATCH resolves `SCION_AUTO_EXPOSE_*` the same way create does. The Configure page shows the effective value and where it came from.
* **Mobile web, final phase** (#2461, #2358): Fits on 320px phones and in landscape. Sideways swipes no longer trigger history-back. Wide code blocks and tables scroll on their own. The chat view no longer bounces back to the conversation after a send or a swipe.
* **Harness telemetry** (#2463): Native gemini-cli usage, Codex cache writes and failed API calls, per-event model attribution for hook usage, and Antigravity and OpenCode tool and error mapping.
* **Project members editor follow-ups** (#2449): Readable reasons for blocked roles, and a Source column shown only when access comes from a non-direct source.
* **Groundwork** (#2462, #2417, #2457, #2410): Settings and an experiment (`hub.k8s_nfs_home`, off) for a persistent NFS agent home on Kubernetes, a start-claim store for pod recovery, and Conduit Ed25519 stream grants. All of it is inactive for now.

## 🔒 Security
* **User access token boundary applied to lists, delegation and messages** (#2436): Hub-bound tokens list only projects they can reach and need the exact list permission. Grants must fall inside the token's boundary. User-to-agent messages pass the bearer gate.
* **Last owners can't be deleted, and role bindings cascade** (#2414): Deleting a user who is the last direct owner of any project returns 409 `last_owner`. A successful delete removes all of the user's role bindings in the same transaction.
* **Ownership transfers can't be undone by a concurrent PATCH** (#2435): Only the ownership writer changes `Project.OwnerID`.

## 🐛 Fixes
* **Existing-agent operations go to the agent's recorded runtime** (#2423): Stop, restart, delete, exec, logs and similar operations could hit the wrong runtime and report "not found" or success while the agent kept running. When the recorded runtime isn't registered, the Runtime Broker now returns 503 `runtime_unavailable` instead of falling back.
* **Runtime Broker stays available when listing or pings stall** (#2450): Heartbeats no longer wait on agent listing, each runtime target has its own deadline, and a failed ping reconnects at once.
* **Skill resolution failures surfaced** (#2452): Required-skill failures on create, start, restart, resume and async launch return typed `skill_resolution_failed` errors instead of 201 or 502. Skills are no longer duplicated on restart.
* **Transport credential recovery** (#2454): An agent whose dispatch-time transport mint failed now picks up a credential delivered later, without a restart. `sciontool doctor` flags the missing credential.
* **Global directory kept out of provider paths** (#2465): `provide --project` run outside a project no longer registers the Runtime Broker's global dir, which broke creates and rewrote the global marker. The slug `global` is reserved. Repair steps are in the PR.
* **CLI Hub-mode fixes** (#2445): `start` resumes stopped agents. `--no-auth` works on create. `--body-file -` reads stdin. Not-found and hint messages are clearer.
* **Web UI fixes** (#2453, #2437): Saving the admin server config no longer stores the masked placeholder over real values. Exposed-port links show only with port access. The skills page loads every page. `blocked` reads "waiting on others". Backfilled project members groups are adopted again.

## 🔧 CI & Infrastructure
* **CI gates** (#2443, #2378, #2459): An ent codegen drift job and a fixture-coverage gate (neither required yet). The 405 `Allow` lint is now blocking. Harness provision tests run in Build & Test. The `pkg/hub` SQLite timeout is raised to 40m. D2 is installed from a pinned, checksummed release.

## 📖 Docs
* **Times and Timezones reference** (#2458): The UTC API contract, display zone, CLI `--tz` and `--utc`, the agent `TZ` chain, UTC-only cron and the timezone maintenance migrations.
* **Kubernetes GCP identity modes and async create settings** (#2455).
