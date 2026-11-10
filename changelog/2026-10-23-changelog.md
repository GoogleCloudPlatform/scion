# Release Notes (2026-10-23)

Project storage now has a single layout authority: the Files tab and chat attachments resolve through it, and the old Hub project copy, GCS workspace sync and linked-project cache are gone. Posting in project chat needs a new permission, non-interactive starts no longer send local environment values without opt-in, and host-side git runs hardened. Dev tokens no longer reach dispatched agents.

## ⚠️ BREAKING CHANGES
* **`project.chat_post` permission** (#4023): Posting in a project chat thread, or promoting an agent DM into one, needs `project.chat_post`; reading still needs `project.read`. Built-in roles from member up get it on upgrade, but custom roles are not backfilled, so add it to any custom role that should post. Sends without it get 403 `chat_post_denied`, and the web thread shows a read-only notice.
* **Non-interactive starts need `--send-env`** (#4111): When the Hub asks for environment values during `scion start` or `scion resume` and no prompt can be shown (no terminal, `--non-interactive`, `cli.interactive_disabled` or agent mode), the CLI sends local values only with `--send-env`. Without it the command fails, naming the keys and the flag.
* **Hardened host-side git** (#4072): Provisioning clones, shared-workspace pulls and worktree operations run git with hooks, pagers, driver programs and the `ext::` transport disabled and a scrubbed environment. Repository config is checked against a deny-by-default allow-list, and a repository outside it refuses to start.
* **Hub project copy and GCS workspace sync retired** (#4091): The Files tab, archive, pull and resource import resolve the workspace through the project storage layout, so web uploads land in the tree agents mount. The Hub project copy, GCS workspace upload and sync-back, and the linked-project cache are removed.
* **REST API response fixes** (#4082): Integration update by ID accepts only GET (405 otherwise). Every malformed or unknown list cursor or page token returns 400 `invalid_cursor`. Agent GET includes the time zone, and the default Runtime Broker marker is corrected.

## 🚀 Features
* **Chat attachments through project storage** (#4092): With object storage configured, chat attachments use artifact transport by default and reach each recipient's inbox. A copy is placed in the project scratchpad only for project-wide topics.
* **Agents follow a re-linked git project** (#4100): A shared-workspace agent of a git-backed project can be resumed after the project is re-linked under a new ID.
* **Content hashes cover paths and modes** (#4118): A v2 content hash for templates and harness configs covers file paths and modes, so rename-only and chmod-only changes are detected and Runtime Broker caches no longer serve an old layout.
* **Base-path-aware web navigation** (#4093): Full-page navigations go through one base-path-aware helper, so reloads work when the app is served under a proxy prefix, and only the newest navigation wins.
* **Usage analytics** (#4086, #4095): `sciontool` emits normalized `agent.usage` events with durable, deduplicated delivery and a `telemetry.health` event, and its usage diagnostics gain delivery and completeness counters.
* **Conduit singleton relays, experimental** (#4096): `server.relay.singleton: true` runs a single non-HA relay, which Cloud Run requires, with relay process metrics and a features column on relay instances.
* **Groundwork** (#4128): For safe server-secret rotation, a versioned value cipher for stored secrets and a database encryption policy with a write fence.

## 🔒 Security
* **No dev tokens in dispatched agents** (#4083, #4077): Create, provision and finalize-env dispatches give an agent only the token the Hub mints for its run. A Runtime Broker offers its dev token only to an agent bound to the co-located Hub at an operator-derived endpoint.
* **Agents mask fails closed** (#4073): On Docker and Podman, an agent start fails if the `.scion/agents` mask mount point cannot be created or is not a real directory.
* **URL display** (#4115): Config, Hub profile and API error text show a placeholder when URL user info cannot be isolated.

## 🐛 Fixes
* **Reincarnation moves** (#4105, #4113): A move whose start the Hub refused before sending it now rolls back definitively instead of staying ambiguous, and a failed move no longer briefly frees the source reservation slot.
* **Flat Runtime Broker creates** (#4099): A create that fails before its agent exists releases its slug reservation, and a create refused because another agent holds the slug returns 409.
* **Notifications to suspended agents** (#4080): Notifications for suspended subscriber agents are parked, retried with backoff and expired with a recorded reason, instead of failing on every sweep.
* **Agent DM dispatch** (#3704): A persisted agent DM dispatch keeps running when the caller goes away, capped at 30 seconds, instead of sitting pending until its 24-hour TTL.
* **Tunnels and Telegram** (#4137, #3503): A concurrent session replacement no longer resets a tunnel connection. Telegram v2 handlers finish on their own configuration's client after a reconfigure, and the poll offset is kept per bot.
* **Diagnostics and logs** (#4075, #4103): `scion doctor` reports the development credential source only when that credential is sent. The Hub logs far fewer DEBUG lines for agent status events and per-request auth.
* **Windows builds** (#4134): `pkg/util` compiles for Windows again.
* **API client** (#4057, #4060, #4053): Non-2xx JSON responses are rejected, request bodies are closed when prepare fails, and the initial request always runs. *(Contributor: Ravshanbek Nazarov)*

## 🔧 CI & Infrastructure
* **Release and extras images** (#4120, #4076): Release `SHA256SUMS` no longer lists itself with an empty hash. The chat app builds on `golang:1.26.1`, and a new check keeps extras Go base images in line with their modules.
