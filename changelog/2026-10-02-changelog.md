# Release Notes (2026-10-02)

The timezone refactor landed in bulk: servers and storage are pinned to UTC, the hub becomes the only source of an agent's TZ, and users and admins get display and default timezone controls. Project access now comes only from role bindings, and multi-role project membership arrived end to end. On Kubernetes, NFS workspaces now give each agent its own worktree or clone, along with many start and cleanup fixes. Mobile chat and GitHub skill resolution under rate limits also improved.

## ⚠️ BREAKING CHANGES
* **`Project.OwnerID` no longer grants access** (#2297, #2274): Project authority comes only from project-scoped role bindings. A user whose only claim was `OwnerID`, such as a removed creator, is now denied. At startup the hub logs a warning, and grants nothing, for legacy projects whose owner has no owner binding. Reading a project no longer re-grants project-owner to its creator, and transferring ownership now also moves `OwnerID`.
* **Cron schedules are UTC-only** (#2319): `CRON_TZ=` and `TZ=` prefixes return 400. Existing prefixed schedules are paused once at upgrade and logged. Edit them to UTC, then resume them.
* **The hub is the only TZ source for hub-dispatched agents** (#2282, #2312, #2344): Create, start and restart take `TZ` from the hub's resolver: the agent's explicit timezone, then the hub env store (user, project, hub, broker), then `agent_defaults.default_timezone`. The broker ignores `TZ` from local templates, broker settings and persisted agent config, and logs a warning for each value it drops. Runtime-profile timezone injection is removed. Set a broker-scope `TZ` env var on the hub instead.
* **gemini-cli default model changes** (#2327): The image no longer pins `gemini-3.5-flash`. Agents started without a model resolve the `medium` alias, so existing default agents switch models on restart.

## 🚀 Features
* **Timezone controls and UTC everywhere** (#2285, #2252, #2308, #2313, #2303, #2257, #2267): Hub and broker processes are pinned to UTC, and tzdata is embedded in the binaries and agent images. The SQLite store boundary is UTC, which fixes scan errors on hosts in non-UTC zones. agent.log is written in UTC. A profile "Display timezone" card and a 24h clock are added across the web UI. Admins get a "Default Timezone" control under Agent Defaults. Agent PATCH returns `resolvedTimezone` and `timezoneSource`. Access-constraint windows and `exposedAt` are stored and returned in UTC.
* **Multi-role project membership** (#2273, #2320, #2330): An atomic `PUT`/`DELETE /api/v1/projects/{id}/members/principals/{type}/{id}` sets a member's full role set: one built-in role plus any custom roles. It has last-owner and escalation guards. A grouped members GET and an `assignable-roles` endpoint back a new Members editor with one row per principal. *(Credit: miller79)*
* **Per-agent workspaces on Kubernetes NFS** (#2314, #2333): Worktree-per-agent projects give each agent its own git worktree, matching the Docker layout (needs git 2.48 or later). Clone-per-agent projects give each agent its own `agents/<name>/workspace`. Deleting an agent with its files removes its workspace.
* **Kubernetes scheduling controls** (#2276, #2324): Agent pods can use a `priority_class_name` (set on the runtime or in a template). Preempted and evicted pods are reported as such instead of as a crash. `shared_dir_storage_class` and `shared_dir_size` can be set on a runtime or profile, which fixes Pending pods on GKE Autopilot.
* **Mobile web, phases 2 and 3** (#2287, #2322, #2283): 16px inputs, so iOS no longer zooms on focus. 44px touch targets and a larger rail. Long-press bottom action sheets for every chat menu. An app logo, PWA icons and a manifest.
* **Faster, sorted agent lists** (#2336, #2341, #2277, #2255): `GET /api/v1/agents` gains `sort=created|updated` with keyset paging, and the project list adds `sort=created`. Authorization inputs are reused across a list request (input loads for a 500-agent list went from 4,005 to 1). SSE updates are merged in place.
* **Jump to agent in the terminal view** (#2306): Cmd+K opens an agents-only palette that fills or replaces a terminal pane.
* **User access token scope eligibility** (#2300): `GET /api/v1/auth/scopes` and `scion hub token scopes` show which selectors you can mint and why. The token form shows the same information.
* **gs:// link images** (#2271): PNG, JPEG, GIF and WebP objects render inline, and other types offer a download. This is still behind `web.gcs_links`.
* **Access-boundary audit history** (#2292): Structured audit events with a transactional history of access-boundary changes, shown in the admin timeline.
* **Chat: new-thread row at the top** (#2260): Choosing New thread on a space opens the name row directly under the space header.
* **Async agent-create groundwork** (#2264, #2359): The broker side of non-blocking create, plus runtime checkpoint and created-resource hooks. Nothing uses them yet: no hub sends async launches.

## 🔒 Security
* **User access tokens checked at mint and use** (#2299): Each requested selector is checked against the caller's live authority before the token is written. A project-bound token is admitted only while its holder still has access to the project. Denials return 403 `scope_violation`.
* **Port-forward proxy responses sandboxed** (#2340): Responses from an agent's exposed port always carry the hub's sandbox CSP and `nosniff`, and the agent's `Set-Cookie` and CSP headers are never relayed.
* **Transport credential delivered via Secret on Kubernetes** (#2272): `SCION_TRANSPORT_TOKEN` moves out of the pod env into the per-agent Secret, referenced with `secretKeyRef`.
* **Credentials revoked on failed create** (#2278): The agent credential minted for a create or launch is revoked when the hub can confirm the broker never used it.
* **Project stop-all authorized from role bindings** (#2334): Project owners and admins get scope "all", members get "own", and everyone else gets 403.
* **Opt-in enforced privilege drop** (#2298): A new sciontool init mode fails closed if the drop to the workload identity doesn't happen. No runtime enables it yet.
* **Authorization hardening** (#2265, #2290): A typed-nil broker identity is treated as missing. Admission scaffolding for an internal delivery credential is added; it denies everything for now.

## 🐛 Fixes
* **Agents whose container vanished are reconciled** (#2293): A running agent missing from its broker's complete inventory for `missing_agent_grace` (default 3m) moves to `error` with exit reason `container_missing`, instead of staying `running` forever while messages to it are buffered.
* **Chat slash commands** (#2253, #2329, #2331): `/stop` deleted the agent instead of stopping it. It now calls the stop endpoint. `/status` always reported no agents, and `/spawn` could never create one. Both are fixed, and in DMs the commands use the peer agent's project.
* **GitHub skill resolution under rate limits** (#2294, #2316, #2309, #2304): Concurrent creates share one resolution per ref and credential, and stale branch refs are served while they refresh. Rate-limit responses start a per-credential cooldown. Resolution has its own 20s budget and fails with a named cause instead of "context canceled". Credential-scoped results now persist across broker restarts.
* **Kubernetes start and cleanup** (#2318, #2354, #2352, #2281, #2302, #2288, #2263): A start that never completes has its pod and Secrets removed. Secrets are cleaned up even when the pod is already gone. Secret and auth files are staged outside the agent home, which fixes "Permission denied" for non-root pods. NFS workspace directories are created before the pod starts, chowned to the pod uid and gid, and tolerate EPERM under squashed exports. The hub-native worktree path rejected by #2244 is allowed again.
* **Multi-runtime brokers** (#2254, #2305, #2323): Each distinct Kubernetes runtime (by cluster, context or namespace) is registered separately. The hub endpoint rewrite, extra hosts, create response, start and restart all follow the agent's runtime instead of the broker default.
* **Create failure robustness** (#2321, #2301, #2284): Create-failure cleanup and quota release run even after the client disconnects. A late start failure no longer removes files belonging to a new same-named agent. Stop and restart no longer act on a bare slug after lookup errors.
* **Wake and restart failures** (#2258, #2291): Waking an agent no longer fails with 502 on resume, and no-auth agents restart correctly.
* **Send with interruption in native chat** (#2310): The interrupt flag is now honored, and a failed mention dispatch is reported as an error.
* **Chat sidebar loading** (#2266, #2296, #2339): Hub members are fully paginated and loads are coalesced. Stale responses no longer overwrite the current view. Viewers without attach no longer see a spurious access-denied toast. *(Credit: miller79)*
* **Smaller web fixes** (#2269, #2279, #2332, #2249): Deleting an agent stays within the SPA. The graph button moved away from Stop and Delete. Cleared string settings now stick in file mode. `block` is no longer offered as a new GCP identity default for Kubernetes targets.
* **sciontool logging race** (#2317), and provision credential lookup failures are now logged (#2315).

## 📖 Docs
* **terraform-ha NFS endpoint change** (#2326): Changing `nfs_server` or `nfs_export` is documented as a manual migration (agent-runbook-terraform-ha.md §11).
