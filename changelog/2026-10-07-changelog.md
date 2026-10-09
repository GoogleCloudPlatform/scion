# Release Notes (2026-10-07)

The redesigned health dashboard gained agent, dispatch, integration and Runtime Broker self-health sections. Artifacts (experimental) gained versions, bundles and message references. Shared dirs can switch between `local` and `nfs` through `scion reincarnate`. Token scoping covered inbox, conversation, project and Hub configuration operations, and the remaining create, start and delete paths that lose to a delete or a different run now answer 409.

## ⚠️ BREAKING CHANGES
* **Scheduled `dispatch_agent` schedules need re-authoring** (#2736): Scheduled dispatches now run under the authority recorded at the schedule's last revision. Existing `dispatch_agent` schedules fire as failed until they are paused and resumed, or recreated.
* **Start, restart and managed create answer 409 after a lost delete** (#2783, #2815): A start or restart that loses to a delete before the Hub's final read now answers 409 `delete_in_progress` instead of 200 (and a no-Runtime Broker start whose row was hard-deleted answers 409 instead of 404). A managed create whose post-create write fails now rolls back with 500 instead of answering 201.
* **Delete refused when the Runtime Broker holds a different run** (#2729, #2752): The Hub keeps the row and answers 409 `conflict`, also under `force=true` and best-effort deletes. A refused create-failure cleanup leaves the agent in phase error with its quota held.
* **Hub telemetry dashboard and Helm `POD_NAME`** (#2780): `scion.db.pool.connections.waiting` is replaced by the `wait_count` counter, so re-import the dashboard. The Helm chart now sets `POD_NAME`. A value already set via `hub.extraEnv` is kept but must be unique per pod.
* **Authorization fails closed** (#2755): The Hub refuses to start if session-only route metadata is incomplete. Hub-admin routes that declare a permission answer 500 "authorization unavailable" when no authorization service is configured, instead of falling back to the admin role check.
* **Postgres index on `broker_dispatch`** (#2805): Auto-migrate creates a new `(state, updated_at)` index with a plain `CREATE INDEX`, which blocks writes while it builds. On large deployments, pre-create it with `CREATE INDEX CONCURRENTLY` before upgrading.
* **Kubernetes per-run secret names** (#2804): Hub starts now name agent Secrets and SecretProviderClasses per run (`scion-run-secret-…`, `scion-run-auth-…`). Older Runtime Brokers ignore per-run objects, so upgrade Runtime Brokers with the Hub.

## 🚀 Features
* **Health dashboard redesign** (#2772, #2779, #2803, #2805, #2723, #2724, #2722): New cards for agents (errored, crashed and offline, with links) and dispatch (stuck messages, stuck and failed Runtime Broker dispatch), and an integrations list for chat plugins. Runtime Brokers send an optional self-health report on the heartbeat, shown in a new compact Runtime Broker table with a Health column. Plugin records no longer appear as Runtime Brokers. An importable Cloud Monitoring dashboard covers Hub metrics, with per-replica series.
* **Artifacts, experimental** (#2742, #2746, #2787, #2791, #2792, #2808): Behind `hub.artifacts`: bundles and versions, HTML bundle views, Markdown with remote images, a hub-level Artifacts list page, and a project Files area with New artifact and Edit. Messages can carry up to 10 `scion://artifact/` references (`scion message --artifact`), shown in web chat as chips with an in-place preview and an attach picker.
* **Switch shared dir backends** (#2661, #2777, #2769): `scion reincarnate --shared-dir-backend NAME=nfs` (or `=local`) changes an agent's recorded shared dir backend without copying data. The next start refuses an empty target dir unless `--allow-empty-shared-dir` is given. The change applies once and is not replayed on later re-renders. Docs give the copy procedure.
* **Admin user provisioning** (#2735, #2778): `POST /api/v1/users` and `scion hub users provision` pre-register a user as invited, with an optional display name. The Admin > Users invite dialog has a Display name field.
* **Scheduled send for chat topics** (#2807): Behind `web.chat_scheduled_send` (default off), right-click Send offers Schedule send. Pending messages are visible to the sender only and can be cancelled. The Hub delivers each one at most once, re-checking access at send time.
* **Web** (#2795, #2725, #2708, #2699): A Reincarnate action on the agent Configuration tab, for callers with the lifecycle permission. Jumping to an agent in the graph zooms so its name is readable. Threads with an unread mention show the mention dot. Terminal sidebar rows show the agent's project name.
* **Attachment warnings** (#2737): Message, DM and outbound-message responses return `attachment_warnings` for attachments the Hub could not read, and `scion message` prints one warning per undelivered attachment.
* **Conversation list names** (#2811): `scion conversation list` shows `DM:<name>` or the thread name for native DMs and threads.
* **Hub request performance tracing** (#2745, #2775): `server.hub.perf_trace` (default off) logs per-request phase timings, authorization store reads and DB pool waits for agent lists and SSE. A new contributor guide covers it and the `perf/bench` harness.
* **Delegation edge adoption** (#2648): A boot migration gives delegation edges written before provenance was recorded a bounded ceiling, so agents on those chains can assign service accounts again. An admin API offers status, preview, commit and revert.
* **Conduit PTY** (#2818, #2828, #2730): `sciontool` serves the conduit `pty` stream kind through tmux, and the Hub resolves Runtime Broker targets through its existing control channel. The router re-resolves past an unreachable or refusing owner relay. PTY attach is unchanged with the conduit experiment on or off.
* **Faster web loads** (#2793, #2794, #2810, #2733, #2758): Project detail reuses the server-prefetched project and shows its header before agents load. Shoelace loads only from the bundle (no CDN). Startup shares one admin-status request. The terminal Jump to agent palette and chat's members sidebar read from the shared agent store.
* **Groundwork** (#2714, #2774): Store support for agent holds, a membership-check outbox and a delegation descendant query, and a default-off decision audit admission path. Runtime behaviour is unchanged.

## 🔒 Security
* **User access token scoping** (#2798, #2757, #2739): Inbox, conversation and notification operations check inbox selectors and the token boundary. Project messaging policy, template and project configuration operations each require their own permission (owners hold `project.set_messaging_policy`). Hub configuration operations admit only tokens with the matching Hub-boundary selector, and pre-start hook scripts are redacted for non-session credentials.
* **Agent tokens bound to the agent run** (#2756): Agent tokens carry a `run_id` claim. A restart-only setting (`off` | `observe`) compares it with the agent's current run and logs mismatches. Enforcement is not selectable yet.
* **Principal ID validation** (#2659): Role-binding and member create reject malformed user and agent IDs with 400 and store UUIDs in canonical lower case.
* **Less detail in logs** (#2707): Project delete and path resolution warnings log `project_id` and an error class instead of paths, slugs and raw errors.

## 🐛 Fixes
* **Lifecycle races with deletes and runs** (#2705, #2797, #2669, #2719): Every create, create-completion and start path, including cross-node, DM wake and a2a-bridge, answers 409 once a delete has won. Cloud Run, Cloud Run Sandbox and Apple Stop/Delete leave another run's instance alone, and rollback removes only the run's own containers. DM wake resume no longer follows the sender's request. `scion start` reports an accepted launch whose status it cannot read instead of claiming the agent was deleted.
* **Exec on a removed container** (#2759, #2718): Docker, Podman and Apple report a container removed after lookup as not found, and the Hub then moves the agent to phase error (`container_missing`). A command's own non-zero exit is never misreported.
* **Hub-native reincarnate** (#2786): Reincarnating a clone-per-agent agent in a hub-native project no longer fails with an empty workspace path.
* **Shared dirs on NFS in chat** (#2743, #2776): Discord, Telegram and the chat app resolve shared-dir attachment paths through the configured storage backend.
* **Colocated agents behind IAP** (#2727): The local Hub endpoint is chosen per runtime (Docker, Podman), whatever the Runtime Broker's default runtime.
* **Server config save** (#2789): Saving from the admin page keeps server fields the form left out instead of dropping them.
* **Empty Hub responses** (#2754, #2781, #2817): Hub client calls that need a body return an error on a 204 or empty 200 instead of nil (removing 14 panics). The CLI reports an empty response instead of suggesting `scion hub disable`.
* **CLI** (#2814, #2812, #2710, #2672): `stop --all` and `suspend --all` cap concurrency at 6. Remote template import bounds `git ls-remote` at 30 s. Secret and env list/get honour `--format json`. `scion message` prints mention notes before the send confirmation.
* **sciontool hooks** (#2721): The harness response is written before telemetry, and hook telemetry export is bounded at 250 ms.
* **Hub leaks** (#2716, #2773): `MessageBrokerProxy` unsubscribes on Stop, and OIDC key loops stop on shutdown.
* **Agent list paging** (#2802): Web agent lists page over a frozen order, so agents updated between pages are no longer skipped or repeated.
* **Web** (#2731, #2732, #2788, #2784, #2664, #2698, #2717, #2644): Chat image previews render (CSP allows `blob:` images) and show an error state when an image cannot be decoded. Terminal bulk actions work for restored terminals. Opening a palette from a tap brings up the on-screen keyboard. On macOS, Ctrl+K in text fields stays with the field. The quick switcher's columns stay balanced. The None project role is hidden when no custom roles exist. Chat toasts render as plain text.
* **"Waiting on Parent" wording** (#2738, #2790): `waiting_for_input` now reads "Waiting on Parent" in the web UI, browser notifications, the chat app and `sciontool`'s default status message.

## 🔧 CI & Infrastructure
* **Cloud Build timeouts** (#2660): Raised to 7200 s for the common and omni builds and 3600 s for `scion-base`.
* **Paged browser benchmarks** (#2796): `perf/bench` measures the paged project grid and list.

## 📖 Docs
* **Lifecycle action responses** (#2806): The API reference now describes the real start, stop, suspend and restart response shape.
