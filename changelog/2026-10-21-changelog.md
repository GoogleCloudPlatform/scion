# Release Notes (2026-10-21)

Admin settings pages now save only what changed, and project chat thread groups graduated. Agents get one model channel with new `scion create --model` and `--thinking-level` flags, and `scion reincarnate --force` can now recreate a workspace on another Runtime Broker. Account status now gates every credential check, and a co-located Hub shuts down gracefully within one budget.

## ⚠️ BREAKING CHANGES
* **Settings saves send only changes; null deletes** (#3984): Admin settings pages and federation send only edited fields, and the Hub's file-mode save keeps omitted keys, as DB mode already did. Clearing a stored value now sends `null`, which deletes the key, an API change for settings clients. Integration plugin defaults show as placeholders and are never saved, an unrelated save no longer rewrites the default Runtime Broker, and new projects inherit the global active profile.
* **Runtime Broker host service accounts need actAs** (#3929): Setting a Runtime Broker host service account through the update API requires actAs on it and is a compare-and-set (409 on a concurrent change). Registration checks actAs only where it writes the pair, and a denial is a 403 before any write.

## 🚀 Features
* **Project chat thread groups graduate** (#3970): The `hub.chat_thread_groups` experiment is retired. Thread group routes and the personal-group migration always run, and the chat rail always shows project groups.
* **One model channel and `scion create --model`** (#3976, #3980, #3965): The agent's effective model is `SCION_MODEL` (Hub-resolved or explicit) else the Runtime Broker config, and the same value feeds the harness `--model` argument, so a harness-config default no longer overrides the Hub-resolved model. `scion create` gains `--model` and `--thinking-level`. The web create form sends a harness config only when the user picks one. Agent views expose the harness vendor and the requested model size for badges.
* **Recreate moves for `scion reincarnate`** (#3952, #3956, #3955): `scion reincarnate --broker <target> --force` moves clone-per-agent and empty-per-agent agents whose workspace cannot travel through a shared export by recreating it on the target. A failed reincarnation carries the new handoff forward instead of replaying the previous one, and the running-container check is scoped to the agent's project, so a same-named agent elsewhere no longer blocks it.
* **Graceful co-located shutdown** (#3744): One shutdown budget bounds every teardown step. `/readyz` reports 503 and new work is refused first, the HTTP listener closes before the Runtime Broker and control channel, and shutdown no longer runs to the service manager's stop timeout.
* **Agent credential refresh** (#3915, #3939): `sciontool init` checks its credential file every 5 seconds and adopts a valid new credential within about 6 seconds without a reset signal. `sciontool doctor` reports init's credential state against the token file and fails, with reload advice, when it is expired or refused.
* **Settings conflicts explained** (#3974): A conflicting Server Config save shows which keys were applied, saved for after a restart or skipped.
* **Chat and terminal UI** (#3944, #3949, #3682, #3945): The chat quick switcher is pinned to the bottom of the rails. The palette's People group loads recent users and searches the Hub as you type instead of paging through every user. On mobile, the terminal workspace gives the screen to the terminal while the on-screen keyboard is up. The agent create "notify me" hint lists every trigger.
* **User access tokens for projects** (#3909, #3928): Project create, register and clone need the new Hub-only `project:create` selector and live authority. Invited users' memberships and role bindings take effect at first sign-in.
* **Flat Runtime Brokers, experimental** (#3948, #3720, #3934): Behind `hub.flat_runtime_brokers`, a new Brokers area lists kind, runtime target, capacity and health, with detail tabs. Flat instances publish their runtime target configuration to the Hub, with credentials withheld. Capacity accounting handles refused starts, launch-end release and ambiguous restarts.
* **Conduit, experimental** (#3930, #3938, #3953, #3926, #3739, #3932): In external relay mode the Hub reverse-proxies each conduit dial to a relay it selects (`server.hub.conduit.ingress: proxy`), passes a trusted lifetime cap, and drains proxied sessions on SIGTERM. Stream logs show route and owning relay. Placement and startup repair read Runtime Broker presence through the shared helper.
* **Groundwork** (#3908, #3981, #3750, #3935): Project visitors can list and read agents and their own grants and DMs (behind `federation.project_visitors`). A hidden, read-only `scion server preflight` command and a `settings.profiles` check for the coming rebuild preflight. Kubernetes clients record runtime build records and connection mode.

## 🔒 Security
* **Active account required for credentials** (#3942): Credential checks, including the development user, now require an active account.
* **Thread-keyed agent message posts** (#3972): Posts that name only a `thread_id` decide the sender before any other answer or write; a refused post writes no conversation or message row.
* **Broader URL redaction** (#3979): Credential-named query, fragment and path parameters are masked, including one level of nested URL. Project ID env reads go through one key list.

## 🐛 Fixes
* **Per-agent state stays out of shared workspaces** (#3966): Kubernetes workspace syncs and NFS-shared checkouts keep `.scion/agents` out of the shared view, matching Docker and Podman.
* **Cloud Run Instances phases** (#3821): List maps instance state to agent phases, so failed or deleted instances no longer read as live in heartbeats and project-in-use checks.
* **Stale exit fields** (#3947, #3884): The first running report clears the previous run's exit code and reason, including after a failed started-status write. *(Contributor: MA)*
* **Chat** (#3954, #3946): Attachments on agent messages delivered live show their preview immediately, and suspended users are hidden from the members list.
* **Workstation logins** (#3790): Workstation logins are recorded, and unset login timestamps are omitted. *(Contributor: MA)*
* **Schedules on agent delete** (#3679): Deleting an agent stops its scheduled-event timers without a second cancel and the resulting error logs.
* **Transfers and API client** (#3814, #3914, #3913, #3925, #3917, #3919, #3747): Transfers avoid temporary file collisions, reject non-2xx responses with bounded error bodies and skip special files. The API client closes request bodies on auth errors, preserves redirect and cookie policies and forwards idle connection cleanup. Front matter delimiters match whole lines. *(Contributor: Ravshanbek Nazarov)*

## 🔧 CI & Infrastructure
* **Lint and extras** (#3923, #3451): The golangci-lint job runs with lower concurrency and GC pressure and records memory use. The chat app and A2A bridge move to the pure-Go SQLite driver, with unchanged timestamp text.

## 📖 Docs
* **Discord on Cloud Run** (#3548): Documents the gRPC health probe for scion-discord on Cloud Run.
