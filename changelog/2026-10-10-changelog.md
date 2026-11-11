# Release Notes (2026-10-10)

Chat notifications gave way to an unread-conversation badge, and the CLI assistant mode was removed. Templates moved to content-addressed blobs with compare-and-swap commits. The Hub-instance registry brought per-replica health to the dashboard. Kubernetes GCP identity gained per-profile mapping reports, refusal before dispatch and a per-account status view. Stop, suspend and stop-all now finish even when the client disconnects.

## ⚠️ BREAKING CHANGES
* **Chat notifications replaced by an unread badge** (#3106, #3109): The Hub no longer creates `MENTION`/`DM_RECEIVED` notification rows or the chat notification event. The web Messages envelope and inbox tray are removed. The chat mode selector and tab title show the unread conversation count from `GET /api/v1/chat/unread-count`, and chat popups come from chat message events.
* **CLI assistant mode removed** (#3131): `SCION_CLI_MODE=assistant` and `cli.mode: assistant` are ignored with one stderr warning. Agent mode and the default mode are unchanged.
* **Federated callers refused on non-agent writes** (#3178): Federated callers keep visitor rights on resources they are granted. Profile and other non-agent resource writes (user-scope templates, skills, harness configs, imports, env and secrets, chat preferences, presence, attachments outside a project) and invite redeem are refused.
* **Token and permission changes** (#3099, #3127, #3187, #3156): User access tokens need `user_skill_injection:update` to change their own injected skills. The bulk agent auth-reset route requires a user session. `hub.federation.read`/`update`, `hub.teams_manifest.update`, `user.list` and `agent.log_append` are reserved and leave built-in roles and scope pickers. The GCP service account list endpoints require project read (agents: own project only) or Hub membership.
* **Template commits are compare-and-swap** (#3174, #3197, #3132): A lost race on template content returns 409 `template_conflict`, and the CLI retries. Templates move to content-addressed blobs. Legacy rows keep working and migrate on their next commit, and unreferenced blobs are garbage-collected after a grace period.
* **Unmapped GCP service accounts refused before Kubernetes dispatch** (#3177): When a Runtime Broker's latest profile report says no Kubernetes service account maps the assigned account, create, start, restart and provision answer 400 `identity_not_mapped` before any token or run is created.

## 🚀 Features
* **Hub-instance registry and fleet health** (#3144, #3100, #3160, #3191): Each Hub replica registers itself, records clean stops and prunes old rows. The health summary reports per-replica DB pool and checks, counts integrations across replicas and shows fleet Hub status, all read from the database.
* **Kubernetes GCP identity** (#3152, #3193, #3117, #3095, #3156): Runtime Brokers report, per Kubernetes profile, which Google service accounts are usable and through which Kubernetes service account. The Hub shows whether each account can be assigned. `scion project service-accounts show <id|email|name>` gives a per-account status view. Without a mapping, the Runtime Broker finds a ServiceAccount by its `iam.gke.io/gcp-service-account` annotation. Identity failures carry stable codes such as `identity_not_verified` and `identity_assign_denied`.
* **Conduit** (#3154, #3188, #3186, #3149, #3164, #3147, #3143): Behind the conduit experiment, Runtime Brokers hold a conduit session alongside the control channel and can tunnel Runtime Broker requests over it. The Hub can request agent token rotation over the agent's session. Managed-runtime agents attach through their session or get 503 `runtime_attach_unsupported`. Agent TCP targets fall back to `[::1]`.
* **Artifacts, experimental** (#3166, #3138, #3208, #3129, #3102): Behind `hub.artifacts`: live chat chips with owner and project names, Markdown preview and New artifact from chat. Reads report `canPublish`, and unloadable references are listed instead of hidden. CriticMarkup inside code is literal text.
* **Agent edits report when they apply** (#3172, #3146): The agent PATCH response groups keys as applied (next container), held, or held for the next reincarnation. The create page embeds the shared agent config form, showing inherited values with their source and sending only fields the user set.
* **Upgrade safety** (#3158, #3159): `server.hub.membership_sweep_report_only` logs and audits the agents the membership sweep would hold without holding them, for the first boot after an upgrade. The Hub warns at startup when stored built-in role revisions are newer than the running build, and a new rollback reference page explains what to do.
* **Kubernetes stop reasons** (#3126): OOMKilled pods are reported as a crash with a memory-limit message, and preempted or evicted pods that vanish are classified from their Events.
* **Port proxy timeout** (#3194): `server.hub.port_proxy.response_header_timeout` (default 60 s) bounds the wait for an agent port's response headers, answering 504 on timeout.
* **Web** (#3213, #3148, #3098, #3108): New terminal windows explain when terminals are open elsewhere. Rail clicks fill the lowest free slot, and multi-pane layouts restore in parallel. Detail pages share a back link. The delete banner and Start button follow the Hub's `blocksStart` answer.
* **Groundwork** (#3083): Bounded async authorization decision logging, default off.

## 🔒 Security
* **Runtime Broker data races** (#3199, #3173, #3151, #3150): Hub connection and control-channel fields are read and written under their locks, and control-channel goroutines can no longer outlive Close.
* **Provisioner validation on import** (#3110): Harness-config import and reimport refuse builtin or empty-command provisioner blocks with 422 `harness_config_unusable`, writing nothing.
* **Test sign-in hardening** (#3168): `POST /api/v1/auth/test-login` adds a `createOnly` mode, audit rows and rate limiting.

## 🐛 Fixes
* **Stop and suspend survive disconnects** (#3165, #3104, #3207): Single-agent stop and suspend, stop-all and sync-from run detached from the client and extend their write deadlines, so a slow but successful operation no longer surfaces as a 502. Stop-time workspace sync-back is bounded and warns on timeout. Sync-from answers 502 on a reply with no manifest instead of panicking.
* **Delete and create races** (#3084, #3119, #3116, #3123): Every create rollback a delete holds answers 409 `delete_in_progress`, and env-gather recreate leaves such rows to the delete. A queued start of a soft-deleted agent answers like the start gate. A reincarnation whose worker never ran fails after 2 minutes instead of blocking for 30.
* **Server Config saves keep fields** (#3121): Every remaining DB-backed section merges onto the stored row. Omitted keys are kept, explicit null clears, and concurrent writes get 409.
* **Session metrics** (#3113, #3170): Metrics are no longer lost or recorded twice around shutdown, and `scion server migrate` copies agent session metrics.
* **Postgres schemas** (#3180, #3209, #3176): Pre-migration and web chat table checks are scoped to the current schema, and every migrate entry point runs the shared pre-migration steps.
* **Projects and workspaces** (#3161, #3092, #3198): Deleting a project removes its NFS workspace tree. Project sync excludes a bare root `.git` file. A starting agent is no longer counted as in use against itself.
* **Runtime Brokers** (#3088, #3086): Each profile registers its resolved runtime type. Template and harness-config downloads set `Content-Length` from the storage object.
* **CLI and web** (#3201, #3216, #3217): Skills commands no longer crash without a Hub. A cleared agent status message disappears from the chat members tooltip. Mobile chat headers and touch targets are larger.

## 🔧 CI & Infrastructure
* **CI** (#3153, #3128, #3183, #3120, #3112): Web checks run only when web inputs change, and golangci-lint memory is bounded. The SQLite-to-Postgres migrate test runs in CI. Cloud Build fetches a fresh registry token at push time. New perf regression budgets cover list, graph and agent views.

## 📖 Docs
* **Docs catch-up** (#3215, #3145, #3079, #3182): Agent lifecycle, API, hosted and permissions gaps are filled. New pages cover a Kubernetes GCP identity setup checklist and `reset-auth` scope re-issue. A release note covers session reports during a mixed-version rollout.
