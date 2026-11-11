# Release Notes (2026-09-25)

The grove→project rename reached its breaking phase: legacy `/api/v1/groves` routes, `--grove` flags, and grove error codes were removed. Agents gained `scion reincarnate`, best-effort resume from error, and a hub-wide default GCP identity that gives single-node VMs working Vertex AI out of the box. Message delivery now reports failures honestly, and read scope for skills, templates, and harness configs was tightened.

## ⚠️ BREAKING CHANGES
* **`/api/v1/groves` routes removed** (#1944): The legacy route aliases and the hubclient `/groves` fallback are gone, and `/api/v1/groves[/…]` now returns 404. Callers must switch to `/api/v1/projects`.
* **`--grove` CLI flags removed** (#1938): Every hidden `--grove` flag (root, `broker provide/withdraw`, `hub env`, `hub secret`, `hub token`, `notifications`) now fails with `unknown flag: --grove`. Use `--project`.
* **GCP `--project` renamed to `--gcp-project`** (#1937): On `scion project service-accounts add` and `scion hub secret migrate`, the GCP project flag is now `--gcp-project`, because `--project` shadowed the scion project selector. The old flag has no alias, but old invocations get a targeted hint.
* **Broker error code renamed** (#1923): `global_grove_disabled` is now `global_project_disabled`. Anything matching on the old string must update.
* **Per-broker agent limit** (#1902, #1946): A new `max_agents_per_broker` limit (default 12, overridable per broker through the admin limits API) is checked before an agent is created. Before this, creating an agent past capacity returned 201 and then crashed the broker host. Only running agents count toward the limit: stop, suspend, and exit release the slot, and start, resume, and restart take it back. A startup and hourly reconcile cleans up stale reservations.
* **`visibility` field removed** (#1916, #1929): The unused `visibility` field is gone from agents, templates, harness configs, and skills, including API responses and the agent SSE payload. Access depends only on scope and grants. The DB column is kept, so upgraded databases keep working.

## 🚀 Features
* **`scion reincarnate` (Phase 1)** (#1918): Restarts an agent on the same row with a freshly derived config, image, and harness config. It runs through an async hub worker (202, CAS-guarded, with sweep and rollback) and supports a dry-run plan.
* **Hub-default GCP identity and out-of-box Vertex AI** (#1906, #1927, #1915, #1899, #1897): Agent Defaults gains a hub-wide GCP identity mode (block, passthrough, or assign). It applies after the explicit request and project default, including for scheduled dispatch. Single-node VM deploys default it to passthrough and seed `GOOGLE_CLOUD_PROJECT`/`GOOGLE_CLOUD_LOCATION` as hub-scoped env vars. Antigravity agents with a metadata-server service account select Vertex AI auth without an ADC file, so a fresh VM runs Vertex inference with no manual setup.
* **Best-effort resume for error-phase agents** (#1900): Hub `/start` accepts `forceResume` to continue the harness session of an agent in the error phase. The web UI adds a confirm-gated "Resume (best effort)" button.
* **`scion project status`** (#1782): New command (alias `health`) that shows per-project agent metrics. *(Contributor: G. Hussain Chinoy)*
* **Agent hub endpoint override** (#1925): The optional `server.hub.agent_endpoint` setting overrides only the `SCION_HUB_ENDPOINT` given to agents, leaving invite links, OIDC, and broker endpoints unchanged.
* **Chat navigation and layout** (#1932, #1911, #1910): Threads with unread messages open at the "New messages" divider. Inter-agent messages are split by day using the main date separator. The thread-default agent is listed first under a "Thread default" sub-heading.

## 🔒 Security
* **Scope boundary on resource reads** (#1912, #1936): User- and project-scoped skills, templates, and harness configs are readable only by their owner, project members, and hub admins. Hub-wide member and viewer grants now apply only to hub- and global-scoped records. Template resolution at agent create and template cloning are also scope-checked.
* **Project agent route authorization** (#1926): Tightens per-caller authorization on project-scoped agent list, get, and update, and on the resume/restart path. Agent response serialization now leaves applied-config fields out unless explicitly included, and a migration normalizes existing rows.
* **Reset-auth token off argv** (#1894): `reset-auth` passes the token over exec stdin instead of the command line, so it no longer shows in `/proc/<pid>/cmdline` on the host. Covers every runtime.

## 🐛 Fixes
* **Honest message delivery** (#1895, #1893, #1892, #1940, #1903): Chat shows "Agent unreachable" instead of a false "Delivered" when the target agent is not running. Broker flush failures are retried 3 times without retyping partially delivered text, and user senders see failures live. Failed messages are purged after 7 days, and pending messages to deleted agents fail early. Replies go to the original sender. The message audit log includes `conversation_id`.
* **Doctor revoking agent tokens** (#1924): `sciontool doctor` refreshed the agent's token to test auth. The hub revoked the old token, so the agent got 401s until restart. It now uses a read-only check.
* **Leaked sandboxes on dispatch timeout** (#1908): The hub now sends a cancel to the broker when dispatch times out. Docker, Podman, and Apple container runtimes roll back a container that may have started, instead of leaving it running.
* **Agent start and restart reliability** (#1941, #1891, #1890): Start and restart now set up the skill resolver the same way create does, fixing "no skill resolver available". Model aliases resolve on resume and restart through a built-in fallback table. `docker ps` is retried on transient failures, and PTY attach returns an actionable 503 instead of a misleading 404.
* **Default template and harness config errors** (#1898, #1905): Embedded agent defaults load at bootstrap, so creates without a harness config no longer return 502. Missing defaults are reported at startup, and missing resources return 404 naming the resource.
* **Grove→project rename follow-ups** (#1920, #1919, #1917): chat-app no longer drops every hub notification (it only accepted `scion.grove.*` topics). `SCION_PROJECT_ID` now takes precedence over `SCION_GROVE_ID`. Assistant mode correctly hides `project reconnect` and `config cd-project`.
* **CLI in agent containers** (#1914): `scion conversation` and `scion notifications` work inside hub-connected agent containers. This needs a rebuilt harness image.
* **Chat interaction fixes** (#1913, #1921, #1939): File-path links in DMs resolve against the sender's project. Backspace deletes an accepted @mention in one keystroke. Message jumps re-check their position after scrolling.

## 🔧 CI & Infrastructure
* **SQLite hub test job** (#1904): Adds a parallel CI job that runs the roughly 73% of `pkg/hub` tests the `no_sqlite` build never compiled.
* **Mergeability gate** (#1901): Conflicted PRs now get a visible failing check instead of showing no checks at all.

## 📖 Docs
* **Grove wording cleanup** (#1922): Replaces stale "grove" wording with "project" in READMEs, CLI help, and schema descriptions.
* **Nightly doc update** (#1909): Nightly update for 2026-09-24.
