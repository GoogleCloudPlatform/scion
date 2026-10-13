# Release Notes (2026-10-12)

The CLI gained a per-user current context: named Hubs, `scion use` across Hubs, and link entries instead of pinned endpoints. `scion ssh` reaches agents over conduit tunnels. Workstation agents in linked git repos get local worktrees again. Templates now ignore environment keys, and `sciontool expose` moved to `scion expose`.

## ⚠️ BREAKING CHANGES
* **`expose` and `unexpose` move to `scion`** (#3419): `sciontool expose` and `sciontool unexpose` now print where the command moved and exit 1. Use `scion expose` / `scion unexpose`, with the same flags and output, available in agent mode.
* **Templates ignore environment keys** (#3339): Telemetry, Hub, image, resources, Kubernetes, volumes and reserved env keys (`SCION_`/`OTEL_` prefixes, Hub-only and harness credential keys) in a template are ignored with a warning instead of applied. Uploads are not rejected, and `admin validate-resources` lists templates that set them. Move these values to settings, profiles or Hub defaults.
* **Vertex AI location precedence** (#3486, #3321, #3403): Every harness path now resolves the location as `GOOGLE_CLOUD_LOCATION`, then `GOOGLE_CLOUD_REGION`, then `CLOUD_ML_REGION`, warning when the first two differ. Claude's Vertex AI inference now requires a region as well as a GCP project.
* **Telegram webhook mode requires a secret** (#3315, #3434): With `inbound_mode` webhook, a missing `webhook_secret` fails startup. The standalone env fallbacks for the webhook URL and credential are removed.
* **Last-admin check on role-binding removal** (#3382): Removing a role binding or project member that would leave a scope with no active access-constraint admin answers 409 `constraint_admin_lockout`.
* **Legacy `grove-id` marker no longer read** (#3383): Reading a project ID uses only the `project-id` marker. The legacy project migration is removed.
* **Workstation `--host` and macOS runtime order** (#3410, #3440): In workstation mode, `--host` moves only the Hub and web listeners. The Runtime Broker stays on 127.0.0.1 unless `server.broker.host` is set. On macOS, auto-detection prefers Podman, then Docker, then Apple container.

## 🚀 Features
* **CLI current context and named Hubs** (#3384, #3428, #3482, #3461, #3401, #3430): `scion project use <project>` (alias `scion use`) sets a per-user current Hub and project, after flags, env and a linked repo. `scion hub add|list|rename|remove` manages Hubs with nicknames that `--hub` accepts. `scion use` searches every registered Hub, with a picker for duplicates. `hub link` records a link entry instead of pinning an endpoint. `scion server start` registers the local Hub. The server warns when its Hub endpoint comes from the CLI's `hub.endpoint`.
* **`scion ssh`** (#3371, #3399, #3411): `scion ssh <agent>` runs the system ssh through `scion tunnel` with the agent's host key from the Hub, and `scion ssh-config [--write]` writes per-Hub include and known-hosts files. Tunnel streams are re-checked with the same authorization as tunnel open.
* **Workstation** (#3481, #3324, #3336, #3441, #3374, #3491): New agents in git repos linked to the local workstation Hub get a git worktree of the user's checkout, as local mode did. `scion server start` waits until each listener answers `/healthz`. `server stop --force` kills a hung server that holds no port. `server status` probes the actual ports. The public URL saved in admin settings sets the Hub endpoint at startup, after env and flags.
* **Platform health in `scion doctor`** (#3456): A Platform Health section shows the health dashboard's needs-attention list, Hub fleet, Runtime Broker health, agent problem groups and integrations.
* **Project GCP identity defaults** (#3370, #3358, #3448): `scion project identity default show|set|unset [--profile]` shows and edits the resolution ladder. A `block` project default is accepted on Kubernetes-bound projects. An agent PATCH that records a service account assignment lists `gcp_identity` as applied.
* **Messaging** (#3476, #3372, #3357): An agent's group post with no @mention goes to the conversation's default agent, as a human post does. `scion whoami --full` and conversation views show message modes. Project Messages lists each message once.
* **Flat Runtime Brokers, phase 2, experimental** (#3417, #3463): Behind `hub.flat_runtime_brokers`: several flat instances per host, each with its own identity and target, on Docker or Kubernetes, co-located or remote, acting only on objects it owns.
* **Artifacts, experimental** (#3425, #3348, #3450, #3431): Behind `hub.artifacts`: a review grant level, with a web Grant review access action on agent-owned artifacts. Version lists explain gaps with discarded versions. Senders are warned when a direct recipient may not be able to read a referenced artifact.
* **Settings validation** (#3361, #3438, #3405): Duration settings, including nested ones, are validated on save with 422 naming the full field path. An unparseable `SCION_SERVER_*` or settings value falls back to its default per key with a warning, instead of turning off Hub features.
* **Kubernetes resume fallback** (#3356): Resuming or waking an agent whose pod-local home was lost falls back to a fresh start instead of exiting.
* **Cloud Run CPU allocation** (#3460): Docs and a conditional startup warning cover CPU-always-allocated for Runtime Brokers hosted on Cloud Run.
* **Palette search** (#3342): A space in a palette query matches hyphens and underscores.
* **Groundwork** (#3378, #3366, #3406, #3459, #3412, #3489, #3373, #3474): Test identities on test Hubs gain workload-bound issuer delegation, browser sessions via single-use codes and target-Hub profiles. Federated accounts behind `federation.project_visitors` and project thread groups behind `hub.chat_thread_groups` (both default off). A runtime-reported starting phase is kept in `scion list`.

## 🔒 Security
* **Only active bindings grant membership** (#3386): Project membership reads count only role bindings inside their `NotBefore`/`ExpiresAt` window.
* **Bounded agent-info reads** (#3367): The Hub handler reads `agent-info.json` with a bounded, no-follow reader.

## 🐛 Fixes
* **`agent-info.json` integrity** (#3446, #3375, #3389, #3469, #3385): Read-modify-writes take an advisory lock, so concurrent writers no longer drop updates. Writers keep unknown keys in `agent-info.json` and `agent-limits.json`. The host no longer writes the file while the container runs. Stale agent directory cleanup keeps directories whose workspace may hold work.
* **Harness status hooks** (#3483, #3432, #3369, #3360, #3343, #3267): Claude and Codex sub-agent events and questions no longer overwrite a parent's held status, and parent tool-start uses one rule for local and Hub status. Claude treats a failed tool call as tool end. Codex prompt, tool and stop events map without the staged dialect, and Codex registers session and sub-agent hooks.
* **Harness provisioning** (#3380, #3393): A Copilot config file that cannot be decoded is replaced instead of failing the start. Malformed staged config files fail with an error naming the file.
* **Runs and re-links** (#3392, #3294, #3337): The Hub adopts the run an async launch report names, so later deletes reach the right entry. Shared-workspace agents resume after a project re-link. Scope re-issue compares the edge guard as an instant.
* **Telegram reconfigure** (#3466, #3445): Changing the inbound mode or configuration retires the previous webhook, poll loop, store and send queue.
* **Configuration and init** (#3478, #3327, #3387): Settings- and profile-derived environment values are no longer persisted into agent config. `scion init` reports a git ownership refusal instead of silently using the external layout. External project names derive from the absolute `.scion` target.
* **Other** (#3365, #3162, #3218, #3354, #3439, #3377, #3458): SQLite web chat migrations wait for the messages table. Harness-config reimport selects only its target. The events stream heartbeat is a named SSE event the web can see. `--debug` works in the runtime subsystem. Port auto-expose and port-forward retries back off. Conduit `Dial` sends no Hello after cancellation. Cloud Run sandbox deletes treat an already-gone sandbox as success.

## 🔧 CI & Infrastructure
* **CI** (#3364, #3464, #3492, #3477, #3462): `pkg/hub` tests run as six shard jobs, and Postgres tests are split into parallel jobs on a non-durable tmpfs database. CI checks that `go.mod` is tidy, and the ent generator runs as a `go.mod` tool.
* **GKE Terraform** (#3309, #3311, #3133, #3345): The Cloud SQL proxy connects over private IP, and the Hub chart's NFS workspace storage is wired. GKE resources are created in order after shared infrastructure. Optional test-runner service accounts are available for test Hubs (off by default).

## 📖 Docs
* **Harness configs and reincarnate** (#3368, #3413, #3394, #3421, #3326): Harness-config changes apply to new agents, and existing agents need a reincarnate. Docs cover when config reaches an agent, reincarnate workspace modes and harness-config precedence, and the up-to-date Runtime Broker requirement for in-place reincarnation.
