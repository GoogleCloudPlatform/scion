# Release Notes (2026-11-24)

This day built out the experimental Inbox, with a CLI command and categories for decisions, waiting agents, reviews and chat. It also added daily automatic Hub updates, broker-confirmed message delivery, a "view as member" mode for admins, and a shared filter query language. Claude agents no longer stop for critical-path delete confirmations.

## ⚠️ BREAKING CHANGES
* **Claude agents skip critical-path delete confirmations** (#5519): The Claude harness now sets `CLAUDE_CODE_EVEN_MORE_DANGEROUSLY_SKIP_PERMISSIONS=1` by default, alongside `--dangerously-skip-permissions`. This suppresses Claude Code's remaining `rm`/`rmdir` confirmation prompts, such as for globbed `/tmp` cleanups, which stalled unattended agents in waiting-for-input. Be aware of the larger blast radius on shared volumes and worktrees.

## 🚀 Features
* **Inbox (experimental)** (#5476, #5463, #5467, #5478, #5465, #5481, #5518, #5534): Behind `web.inbox_dashboard`, the Inbox gathers what is waiting on you across projects: decisions (answerable from the Inbox), agents waiting for input, approvals, artifact reviews and unread chats. The web UI has an Inbox sidebar item and a bell link. `scion inbox` prints a read-only summary.
* **Daily automatic Hub updates** (#5461, #5486, #5525, #5526): Admins can turn on daily automatic updates from the Maintenance page and pick a UTC start hour (default 04:00, just after the nightly release). The Hub tries at most once per day within a 2-hour window, never downgrades, and skips a release whose update failed or rolled back. An explicit `server.maintenance.update_policy: disabled` still wins. Settings changes and runs are audited.
* **Delivered status from the broker** (#5440, #5508): Chat shows Delivered once the agent's broker confirms delivery, and shows the progress stages while a suspended agent is resumed for a message.
* **View as member (experimental)** (#5480): Behind `hub.view_as_member`, a Hub admin can temporarily drop Hub-level admin grants in their session to see what a member sees. It's a preview and a safety rail, not a security boundary.
* **Filter query language (experimental)** (#5436, #5496, #5493): Behind `filters.query_dialect`, a shared filter syntax works on the global agents list (`GET /api/v1/agents?q=`), artifacts and files.
* **Execution containers in the web UI (experimental)** (#5510, #5524): Execution containers can be set up from profile settings and created from the web UI, and are badged. They signal whether their home persists and show a first-login message.
* **Scheduling views** (#5453, #5506, #5507): The project scheduler has pending and past event tabs, pagination and a wake column. Agent pages have a Schedules tab, and schedule detail names the creator.
* **Template drift warning (experimental)** (#5536): Behind `hub.template_drift_warning`, creating an agent from a GitHub-sourced template from the project's own repository warns when the template folder has changed upstream since it was imported.
* **Agent secret requests in chat (experimental)** (#5470): Agent secret requests can be filled in a native chat form.
* **Broker init firewall policy** (#5459): `server.broker.init_firewall` sets the egress allowlist and SSH opt-in that apply when an agent turns on `SCION_INIT_FIREWALL`. It is passed only to the network-setup container, and agents can't override it through reserved environment variables. Invalid entries stop the broker from starting.
* **GitHub triggers (experimental)** (#5458, #5469): Triggers can run an exec action. GitHub triggers acknowledge with a comment, release claims when a repository is removed, and guard against bot loops.
* **CLI lists report truncation** (#5466, #5492): `scion conversation list` and similar list commands take `--limit` and `--all` and say when output was truncated. JSON output includes totals.
* **Web UI updates** (#5444, #5485, #5441, #5511, #5520, #5521, #5498, #5442): Pages across the app use a shared breadcrumb, and the file editor warns about unsaved changes. The admin Experiments tab filters by All, On, Off and Overridden. The Cross-Project Messaging card is hidden when admins have turned it off.

## 🐛 Fixes
* **Suspend is idempotent** (#5462): Suspending an already-suspended agent succeeds, and an explicit suspend of an agent that the Hub suspended as idle or stalled records the reason as `user`.
* **Custom default user role races** (#5456, #5502): Concurrent first sign-ins can no longer leave a user in hub-members with the wrong custom default role, admins stay in hub-members, and startup admin reconciliation waits for operational settings.
* **Antigravity default model** (#5495): With no model set anywhere, Antigravity uses the `medium` alias from its harness config.
* **Harness-config hardening** (#5455, #5513, #5522): Harness-config hooks, lifecycle environment and services staging are hardened when container-script harnesses are disallowed. Non-force harness-config upgrades skip seed-skip files, including glob patterns and `__init__.py`.
* **Smaller fixes** (#5439, #5477, #5509, #4244, #4255): Podman's version is checked before passing `--add-host host-gateway`. Template errors, sciontool test logs and `sync --project` are clearer. Image builds lock `pi` npm dependencies, and a failing harness image no longer fails the `scion-hub` image build. Attachment delivery tells an unexpected status apart from a refusal, and transfer hash checks allocate less. *(Contributor for #4244, #4255: Ravshan04)*
