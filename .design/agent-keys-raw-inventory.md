# Agent-keys raw-caller and image inventory, and rollout checklist

Task **3.2**, part of the master agent-keys design, frozen contract
`.design/agent-keys-contract.md`. This is the repository-committed inventory and migration
checklist this task's own scope calls for ("Create the concrete rollout inventory and migration
checklist with broker/Hub/client/image versions and removal evidence requirements... Commit the
inventory and the rollout checklist as a repo doc"). It does not authorize removal: Phase 4 (tasks
4.1/4.2/4.3) still owns exercising the replacement and cutting over.

Originally inventoried at `scion/agent-keys-2-2` `c1af0cbc3` (0.1/0.2/1.1/1.2/2.1/2.2 and the task
2.2 follow-up adding the agent-caller attach gate). Updated against upstream `main` at `e761178`,
which also includes task 2.3 (the Hub message-raw bridge) and task 3.1 (the CLI/`hubclient` keys
migration); rows those tasks changed now read "Migrated", "Bridged", or "rerouted". Phase 4
removal has not happened: raw still works, through the bridge. Recheck before Phase 4 acts on
this — see "Integration status" at the end.

## 1. What "raw-dependent" means here

A caller or image is raw-dependent if it builds or transports a `Raw`/`raw`-flagged
`StructuredMessage`/`MessageRequest` (the pre-cutover keystroke-injection path), or if it embeds a
`scion` CLI binary whose `cmd/keys.go`/`cmd/message.go` still do so. It is not raw-dependent merely
for using `scion message`, `scion keys`, or the Hub messaging API without `raw`. Since task 3.1, the
first-party `scion` CLI built from this repository is not raw-dependent in this sense.

## 2. Repository CLI/SDK callers

| Caller | Owner | Migration status | Evidence |
| --- | --- | --- | --- |
| `cmd/keys.go` (`scion keys`, Hub mode) | Client owner (3.1) | **Migrated (3.1).** `sendKeysViaHub` validates locally with `agentkeys.ValidateKeys`, then calls `hubclient` `ProjectAgents(projectID).SendKeys`, which POSTs `{"keys": ...}` to the project-scoped `/keys` route. It never builds a `Raw` `StructuredMessage` and never falls back to `/message`, including against a Hub that predates `/keys` (reported as `hub_unsupported`). | `cmd/keys.go:289`-321 (`hub_unsupported` mapping: `cmd/keys.go:210`-221) |
| `cmd/keys.go`'s own `--help` text (`Long`/`Examples`) | Client owner (3.1) | **Migrated (3.1).** The `Long` text states the one-argument rule (each example is a separate call, a whole-argument tmux key name is sent as that key, anything else is typed literally, no trailing Enter), and the `Examples:` block no longer contains a multi-key sequence. The `Short` text still reads "Send raw keystrokes", which describes the input, not the deprecated `raw` flag. | `cmd/keys.go:36`-60 |
| `cmd/keys.go` (local mode) | Client owner (3.1) | **Migrated (3.1).** `sendKeysLocal` resolves the single target within the selected project (`resolveLocalKeysTarget`; ambiguous resolution fails) and calls `agent.Manager.SendKeys` for a Hub-linked project or the additive `agent.Manager.SendKeysLocal` for a purely local project. Both validate input (an empty string is rejected) and bind to the container's `agent_id` before delivery. `MessageRaw` is no longer called from the CLI. | `cmd/keys.go:338`-385,`409`; `pkg/agent/manager.go:876`-895,`914`-927 |
| `cmd/message.go` `--raw` flag | Client owner (3.1) | **Deprecated, hidden, rerouted (3.1).** Hidden (`MarkHidden("raw")`) with a deprecation warning naming `scion keys`. After client-side combination checks it is a plain alias: Hub mode calls `sendKeysViaHub` (also for same-project `@agent` addressing), local mode calls `sendKeysLocal` — the same paths as `scion keys`, never `MessageRaw` or a `Raw` `StructuredMessage`. Removal is Phase 4. | `cmd/message.go:63`,`475`-484,`499`-504,`520`-525,`1517` |
| `pkg/hubclient` (`AgentService`) | Client owner (3.1) | **Migrated (3.1).** Typed `AgentService.SendKeys` exists and POSTs to `{id}/keys`. `SendStructuredMessage`/`SendStructuredMessageWithOptions` still accept a `Raw` field for legacy callers; the Hub bridge handles such requests (§2a). | `pkg/hubclient/agents.go:129`-145,`634` |
| `pkg/apiclient/transport.go` / `pkg/hubclient.WithRetry` | Client owner (3.1) | **Migrated (3.1).** `SendKeys` uses `postNoRetry` → `Transport.DoNoRetry`, which sends once regardless of `WithRetry` and does not follow redirects (contract §4.3, AK-34). `WithRetry` still applies to every other call. | `pkg/hubclient/client.go:361`-368; `pkg/apiclient/transport.go:155`-164 |

**Default retry posture of in-repo callers**, recorded because the contract requires tracking which
legacy raw clients enable message retries (the master design's "Inventory whether old callers enable
message retries"):

- The first-party `scion` CLI never calls `hubclient.WithRetry` when building its Hub client
  (`cmd/hub.go:531`-592, the only options appended are auth and a 30s timeout) — `MaxRetries`
  therefore defaults to `0`. Since 3.1, the CLI no longer sends raw at all, and its keys call bypasses
  `WithRetry` regardless (§2).
- **Server-side retry on the legacy raw path is closed (2.3).** Before the bridge, the Hub's dispatch
  of a raw request went through `dispatchWithBrokerRetry` (exponential backoff, ~30s overall), so a
  temporarily unreachable broker could receive a keystroke delivery well after the request. A raw
  request on either single-agent `/message` route is now handled entirely by the bridge, which
  delegates to the keys admission/dispatch path (single attempt, execute-before deadline, no
  replay); and the HTTP dispatcher refuses any `Raw == true` message that still reaches it
  (`ErrRawDispatchRefused`, zero broker calls). `dispatchWithBrokerRetry` remains for non-raw
  messages. Evidence: `pkg/hub/agent_keys_message_bridge.go:95`,`285`-290;
  `pkg/hub/httpdispatcher.go:2645`-2686; `pkg/hub/broker_routing.go:148`.
- `extras/scion-a2a-bridge` builds several `hubclient.New` clients (`internal/bridge/bridge.go:1376`
  -1436); none pass `WithRetry`. It also never constructs a `Raw`-flagged message — `translate.go`'s
  `TranslateA2AToScion` only sets text/attachment fields — so it is not a raw-dependent caller at
  all, listed here only to close the question, not left unchecked.
- `extras/scion-chat-app` and `extras/scion-teams` likewise call `hubclient.New` without
  `WithRetry` (`extras/scion-chat-app/internal/identity/identity.go:239`,
  `extras/scion-teams/internal/teams/send.go` builds its own HTTP client, not `hubclient`, with a
  401-refresh retry only, not a general send retry); neither sends raw messages (see §4, plugin
  ingress, for why their outbound-only chat relay shape cannot construct one).
- **No external (non-repository) caller's retry configuration is known.** Per the binding
  obligation on this task, this is recorded as **unknown**, not assumed safe — see §5. The bridge
  cannot disable such retries; every bridged response carries migration headers saying so
  (`setAgentKeysBridgeMigrationHeaders`, `pkg/hub/agent_keys_message_bridge.go:494`).

## 2a. Hub/broker/shared-type consumers

Per r1 review finding 5: §1's own definition ("builds or transports a `Raw`-flagged
`StructuredMessage`/`MessageRequest`") also covers the server-side legacy raw path, not only the
client-side callers in §2. These are the components task 4.2 ("Complete `MessageRaw` → `SendKeys`
cleanup") removes or replaces, distinct from the dedicated `/keys` route
§7 step 1 already covers:

| Component | Owner | Migration status | Evidence |
| --- | --- | --- | --- |
| Runtime Broker's legacy raw consumer (`isRaw` → `mgr.MessageRaw`) | Broker owner (4.2) | **Pending removal.** The broker's own `/message` action handler branches on `req.StructuredMessage.Raw` and calls `mgr.MessageRaw` directly, bypassing the paste buffer/debounce and logging `"message delivered (raw, unbuffered)"`. A current Hub never sends it a raw message (bridge plus dispatch backstop, rows below), but an older Hub could; this is the broker-side half of the legacy path the dedicated `/keys` broker route (§7 step 1) does not remove. | `pkg/runtimebroker/handlers.go:2220`-2222,`2269` |
| **Hub message-path raw handling** (`agent_dm_operation.go`, `raw_guard.go`) — the agent-to-agent DM half | Hub ingress owner (0.2/2.3/4.2) | **Bridged (2.3); residual code pending removal (4.2).** Step 4b (the cross-project raw denial, `cross_project_raw_unsupported`) was removed by 2.3: raw requests no longer reach `ExecuteAgentDM` from the single-agent routes, and a cross-project raw DM now gets `cross_project_keys_unsupported` from the bridge (on the top-level route only when the agent holds `ScopeAgentLifecycle`; without it, `403 keys_denied` first, `pkg/hub/agent_keys_message_bridge.go:342`-348). Still present but unreachable from public single-agent routes: `AgentDMInput.Raw`, step 4c (managed-backend raw denial), and the `evaluateRawMessageGuard`/`isRawConversationAddressed` guards. Still live on other ingress: `rejectRawScheduledPayload` (§4). The dispatch-layer backstop (`ErrRawDispatchRefused`) is the fail-closed guarantee for any `Raw == true` call that reaches dispatch. | `pkg/hub/agent_dm_operation.go:345`-357; `pkg/hub/raw_guard.go`; `pkg/hub/httpdispatcher.go:2674`-2686 |
| **Single-agent `POST /:id/message` raw path** (`handlers_agent_messaging.go`) | Hub ingress owner (2.3/4.2) | **Bridged (2.3); legacy handler code pending removal (4.2).** Both routers call `tryAgentKeysMessageBridge` before `authorizeAgentMessage`; a raw-selected request is normalized into `authorizeAgentKeys` + `admitAndDispatchAgentKeys` and never reaches `handleAgentMessage`. The legacy raw branches inside `handleAgentMessage` (top-level `raw` OR-merge, managed-backend and cross-project raw guards, raw-aware mention/offload/observer handling) remain as unreachable code until 4.2 removes them. | `pkg/hub/handlers_agents_core.go:3201`; `pkg/hub/handlers_projects_core.go:2555`; `pkg/hub/agent_keys_message_bridge.go:95`-292; `pkg/hub/handlers_agent_messaging.go:1614`,`1699`,`1737` |
| Message-broker plugin gRPC protocol `raw` field | 4.2 (in-repo); **plugin owner — unknown** for out-of-repo plugins, same as §5 | **Pending removal, with an external compatibility concern §5's other rows don't have.** `StructuredMessage.raw` is wire field 11 in the plugin gRPC protocol itself, not just an internal Go struct field — it round-trips to and from every message-broker plugin, in-repo or not. Removing the Go field without reserving the wire number would let a stale out-of-repo plugin binary silently reuse field 11 for something else; 4.2 must add `reserved 11;` to the `.proto` (and bump/document the protocol version) rather than merely deleting the field, and the migration note should say so. Whether any out-of-repo plugin actually reads/sets this field is **unknown**, consistent with §5. | `proto/broker/v1/broker.proto:44` (`bool raw = 11;`); `proto/broker/v1/broker.pb.go:51`,`168`; `pkg/plugin/grpcbroker/convert.go:43`,`82` |
| `pkg/messaging` raw-aware rendering/offload, and fan-out forwarding | Message-domain owner (4.2) | **Pending removal.** `DeliveryOptions.Raw`/`Qualifies()`'s raw parameter govern whether rendering returns the bare body instead of the normal envelope and whether a message qualifies for offload (called from both `agent_dm_operation.go:585` and `handlers_agent_messaging.go:2487`); `messagebroker.go`'s `fanOutToProject`/`fanOutGlobal` forward `msg.Raw` unchanged today, documented as safe only because 0.2 already ensures no Hub publisher places a raw message on that bus — a guarantee that becomes moot, not merely safe, once the field is gone. | `pkg/messaging/delivery.go:77`; `pkg/messaging/delivery_compat.go:40`-44; `pkg/messaging/render_delivery.go:86`,`129`; `pkg/messaging/offload.go:189`; `pkg/hub/messagebroker.go:742`-746 |
| `messages.StructuredMessage.Raw` wire field, incl. its log-attribute handling | Message-domain owner (4.2) | **Pending removal**, with a decode-time tombstone per contract §6.1/AK-36 rather than silent deletion. `FormatForDelivery` already special-cases `Raw` identically to `Plain` (return the bare `Msg`, no envelope); `LogAttrs()` already redacts raw message content in structured logs (`redactedRawContent`) but still logs the `raw` boolean itself — both are call sites 4.2 must account for when the field goes. | `pkg/messages/types.go:133`,`304`-315; `pkg/messages/format.go:71`-73 |
| `agent.Manager.MessageRaw` primitive | Runtime owner (1.1 added `SendKeys` alongside it; 4.2 removes it) | **Deprecated, still live.** Doc-commented "pending Phase 4 removal... performs no `agent_id` identity binding. New callers must use SendKeys". Since 3.1 the CLI no longer calls it; the only remaining caller is this table's broker-handler row. Four test doubles also implement this interface method and will need updating when it goes. | `pkg/agent/manager.go:81`-85 (interface doc comment), `:444` (implementation); `pkg/runtimebroker/handlers.go:2220`-2222; mock implementations in `pkg/runtimebroker/handlers_test.go`, `heartbeat_test.go`, `protocol_mismatch_test.go`, `workspace_handlers_test.go` |
| `extras/scion-broker-log` (`msg.Raw` field read/display) | Extras owner | **Consumer only; affected by field removal, not itself raw-dependent in the §1 sense** — it reads and summarizes an inbound `messages.StructuredMessage.Raw` value (`flagsSummary` appends `"raw"` to a flag list) for log/audit display; it never constructs or transports a raw request. When 4.2 removes the `Raw` field, this tool's event struct and summary function need a matching update or they silently stop compiling/displaying it. | `extras/scion-broker-log/main.go:457`,`487`,`522` |

## 3. Skills and templates

| Source | Owner | Migration status | Evidence |
| --- | --- | --- | --- |
| `resources/platform_skills/scion-agent-manage/references/troubleshooting.md` | Docs/inventory owner (task 3.2) | **Migrated in task 3.2's change.** Previously told operators to run `scion message <agent> --raw "ENTER"` / three separate `--raw` calls — correct in effect (verified live: tmux resolves named keys like `Enter` case-insensitively, so `"ENTER"` did submit, same as `"Enter"`), but it used the deprecated `message --raw` flag and a non-canonical spelling. Rewritten to `scion keys <agent> "Enter"`, the current command and the canonical spelling, keeping the existing one-key-per-call shape (never presented as a sequence). | Task 3.2's diff to that file |
| `resources/platform_skills/scion-messaging/SKILL.md` | Docs/inventory owner (task 3.2) | **Already used `scion keys`, not `--raw`; amended in task 3.2's change** to state the attach-vs-message authority distinction and the no-sequence rule explicitly, since neither was previously called out and an agent reading only this skill could reasonably assume message authority was sufficient. | Task 3.2's diff to that file |
| `pkg/config/embeds/templates/default/*` (seeded agent home dir, `agents.md`, shell rc files) | Template owner | **Not applicable.** No `raw`/`keys` reference of any kind found in this tree; nothing to migrate. | `grep -r` over `pkg/config/embeds/` — zero matches |
| `examples/`, `scripts/`, `hack/`, git submodules | N/A | **Not applicable — checked, not merely unchecked.** No file under `examples/` or `scripts/` uses `--raw` or `scion keys` (both only ever call plain `scion message`); `hack/merge-work.sh` likewise. The repository has no git submodules (no `.gitmodules` file, no `160000`-mode gitlinks in `git ls-files -s`). The `examples/amp` and `examples/adk_scion_agent` *Dockerfiles* are a separate, image-level dependency (both inherit the `scion` CLI via `FROM scion-base`) and are inventoried under §6, not here — this row covers their non-Docker source files, which have no raw/keys reference either. | `git grep -l -- '--raw\|scion keys' examples/ scripts/ hack/` — zero matches; absence of `.gitmodules` and of any `160000` entry in `git ls-files -s` |
| Any user- or project-scoped skill stored in the Hub (not in this repository) | Unknown — not owned by this repository | **Unknown.** Cannot be inventoried from source; see §5. | — |

## 4. Plugin ingress raw payloads (inventoried separately — rejected, not adapted)

Per this task's own scope ("Inventory plugin ingress raw payloads separately because those forms are
rejected, not adapted"): these are not bridge-adapted the way the single-agent `/message` path is
(contract §6.1) — task 0.2 closes them unconditionally, before sender-identity synthesis, with no
`/keys`-equivalent replacement on that ingress at all.

| Ingress | Owner | Status | Evidence |
| --- | --- | --- | --- |
| `POST /api/v1/broker/inbound`, `/api/v1/broker/inbound/routed` (`message.raw`) | Hub ingress owner (0.2) | Unconditionally rejected before sender resolution (AK-57) | `pkg/hub/handlers_broker_inbound.go`, `pkg/hub/handlers_broker_inbound_routed.go` |
| `POST /api/v1/projects/:projectId/broadcast` (`structured_message.raw`) | Hub ingress owner (0.2) | Unconditionally rejected immediately after decode (AK-56) | `pkg/hub/handlers_agent_messaging.go` (broadcast handler) |
| Scheduled-event / recurring-schedule advanced Payload JSON (`"raw"` key, any value) | Hub ingress owner (0.2) | Tombstoned with 422, any value including `false` (AK-58) | `pkg/hub/raw_guard.go:206` (`rejectRawScheduledPayload`), called from `pkg/hub/handlers_scheduled_events.go:243` and `pkg/hub/handlers_schedules.go:226`,`414` |

**Chat-integration plugins that publish through this ingress** (`extras/scion-slack`,
`extras/scion-discord`, `extras/scion-telegram`, `extras/scion-teams`): checked directly — none of
their `broker.go`/`hubclient.go` call sites construct a `raw`/`Raw` field (`grep -rn "\"raw\"\|\.Raw\b"`
over each `extras/<name>/internal/` tree found no match outside an unrelated `Encoding: "raw"`
attachment-encoding string in `scion-discord`). They relay human-authored chat text, which is
rejected by the ingress guards above only if a user could inject a literal `raw` JSON key through
the plugin's own payload construction — confirmed not possible, since each plugin marshals a fixed
Go struct, not pass-through JSON. Recorded as **not raw-dependent today**, not merely unchecked.

## 5. External/unknown callers — recorded as unknown, per the binding obligation

This repository cannot enumerate callers outside it. The following are **explicitly recorded as
unknown**, not assumed migrated, per this task's own acceptance criterion and the master design's
removal gate ("demonstrate no known callers still depend on raw via content-free counters plus
explicit owner confirmation... inventory whether old callers enable message retries"):

- Any third-party script or service calling `POST /api/v1/agents/{id}/message` or the project-scoped
  equivalent directly with `raw`/`structured_message.raw`, bypassing the `scion` CLI/SDK entirely.
- Any fork or private deployment of the harness images in §6 still running a `scion` binary built
  before the 3.1 CLI migration.
- Any operator script, cron job, or external automation outside this repository that shells out to
  `scion message --raw` or `scion keys` directly.
- These callers' retry behavior (whether they wrap the Hub API in their own retry loop, independent
  of `hubclient.WithRetry`) is likewise unknown.

**Removal-gate consequence:** 4.1 must use the Hub's content-free raw-bridge audit records (contract
§5's audit field list; task 2.3 emits them with `route` = `message_raw_bridge`,
`pkg/hub/execute_agent_keys.go:249`,`585`) to detect whether *any* caller — known or unknown — is
still exercising the bridge, rather than relying on this static inventory alone. Low observed usage is explicitly **not** proof
per the master design's removal gate; an explicit owner confirmation is still required for every
*known* caller above before Phase 4 removes the bridge.

## 6. In-agent CLI images

Every image below carries the `scion` CLI binary one of two ways: (a) inherited `FROM scion-base`
(directly or through a harness chain), whose own Dockerfile builds it from this repository's
`./cmd/scion/` (`image-build/scion-base/Dockerfile:63`,
`go build ... -o /usr/local/bin/scion ./cmd/scion/`) and installs it at `/usr/local/bin/scion`; or
(b) a standalone Dockerfile elsewhere in the repo that runs its own `go build ... ./cmd/scion` and
`COPY`s or installs the resulting binary directly, independent of `scion-base`. **Every image in
this table therefore carries whatever `cmd/keys.go`/`cmd/message.go` behavior is compiled into the
`scion` binary at image-build time** — this is the mechanism, not an assumption, and is why the
CLI/SDK migration (3.1) and these images' rebuild-and-release are two separate, sequenced rollout
steps (the master design's "Cutover and deployment": CLI/SDK binaries and agent images are one
deployment step, after the Hub bridge, before removal). The CLI source is migrated as of 3.1; the
**Pending** rows below mean the image still needs a rebuild and release from a commit that includes
it. Completeness is checked against
`git ls-files '*Dockerfile*'` run repo-wide (26 tracked Dockerfiles at `e761178`), not
only under `extras/*` — an r5 review finding caught five Dockerfiles outside `image-build/`,
`harnesses/` and `extras/` that an `extras/`-scoped sweep had missed.

| Image | Base chain | Owner | Migration status | Evidence |
| --- | --- | --- | --- | --- |
| `image-build/scion-base` | `core-base` or `thick-prep` | Image-build owner | **Pending rebuild/release** — builds the `scion` binary from this repo's current `cmd/`, which carries the CLI migration as of `e761178`; migration status always matches whatever commit it's built from. Not itself raw-dependent code, but the sole source of the binary every image below inherits. | `image-build/scion-base/Dockerfile:36`-64 |
| `image-build/hub` (Hub server image, GKE-oriented) | `scion-base` | Image-build owner | **Pending rebuild/release** (embeds the `scion` binary for in-container CLI use; the CLI migration is in source at `e761178`); the Hub *server* code built from that source already carries `/keys` and the raw-to-keys bridge — see §8. Per `docs/deploy/agent-runbook-terraform-ha.md:263`-266, this is explicitly **not** the image used for the HA Cloud Run deployment pattern (`scripts/cloudrun/Dockerfile` below is) — it runs as root and is called out as "the wrong hub image for this Cloud Run pattern," so 4.1's version-recording step should not assume this row alone covers every deployed Hub image. | `image-build/hub/Dockerfile` |
| `image-build/omni` (Hub + all harnesses, Cloud Run Instances) | harness chain → `scion-base` | Image-build owner | **Pending** — rebuilds `scion` without `no_embed_web` but from the same `cmd/keys.go` source; inherits every harness image's migration status below. | `image-build/omni/Dockerfile:16`-30 |
| `harnesses/claude` (default-installed) | `scion-base` | Harness owner | **Pending** — default-install harness; highest-priority rebuild now that 3.1 has landed. | `harnesses/claude/Dockerfile` |
| `harnesses/gemini-cli` (default-installed as "gemini") | `scion-base` | Harness owner | **Pending** — default-install harness. | `harnesses/gemini-cli/Dockerfile` |
| `harnesses/opencode` | `scion-base` (chained after claude in `omni`) | Harness owner | **Pending**, opt-in | `harnesses/opencode/Dockerfile` |
| `harnesses/codex` | `scion-base` (chained after opencode in `omni`) | Harness owner | **Pending**, opt-in | `harnesses/codex/Dockerfile` |
| `harnesses/antigravity` | `scion-base` (chained in `omni`) | Harness owner | **Pending**, opt-in | `harnesses/antigravity/Dockerfile` |
| `harnesses/grok-build` | `scion-base` (chained in `omni`) | Harness owner | **Pending**, opt-in | `harnesses/grok-build/Dockerfile` |
| `harnesses/copilot` | `scion-base` | Harness owner | **Pending**, opt-in | `harnesses/copilot/Dockerfile` |
| `harnesses/muse-code` | `scion-base` | Harness owner | **Pending**, opt-in | `harnesses/muse-code/Dockerfile` |
| `harnesses/hermes` | `scion-base` | Harness owner | **Pending**, opt-in | `harnesses/hermes/Dockerfile` |
| `Dockerfile` (repo root, generic Hub image) | standalone — builds `./cmd/scion/` directly, not `FROM scion-base` | Image-build owner | **Pending** — multi-stage build (`node:22-alpine` frontend, `golang:1.26.1-alpine` builder, `debian:bookworm-slim` runtime); `go build -o /scion ./cmd/scion/`, copied to `/usr/local/bin/scion`, set as `ENTRYPOINT`. Byte-for-byte identical to `Dockerfile.hub` (verified: `diff Dockerfile Dockerfile.hub` produces no output). | `Dockerfile:1`-8 (frontend stage), `:10`-27 (builder stage, `go build ... ./cmd/scion/` at `:27`), `:37` (`COPY`), `:41` (`ENTRYPOINT`) |
| `Dockerfile.hub` (Hub server image, alternate/explicit name) | standalone — same build as the root `Dockerfile` | Image-build owner | **Pending** — identical file to the root `Dockerfile` above (same `diff`, confirmed byte-for-byte). Not the same file as `image-build/hub/Dockerfile` (that one is `FROM scion-base`, GKE-oriented per the note on the next row); this is a second, independent way the Hub image gets built. No *deployment* doc references it specifically (only release notes do: `docs-site/src/content/docs/release-notes-archive.md:238`, `changelog/2026-06-29-changelog.md:9`, both describing its original addition, not how/when to build it operationally). | `Dockerfile.hub:1`-8,`:10`-27,`:37`,`:41` |
| `scripts/cloudrun/Dockerfile` (Hub, Cloud Run target — the actual production image for the HA Cloud Run deployment pattern) | standalone — builds `./cmd/scion` directly | Image-build owner | **Pending** — a third independent build of the same binary, cross-compiled for Cloud Run (`GOARCH=amd64`, embeds a build-time version ldflag). `docs/deploy/agent-runbook-terraform-ha.md:262`-266 explicitly documents this file, **not** `image-build/hub/Dockerfile`, as the Hub image source for this deployment pattern ("the latter builds the GKE-oriented `scion-hub` image... it runs as root and is the wrong hub image for this Cloud Run pattern") — making this one of the two or three Dockerfiles most likely to actually be running in a real HA deployment, not merely a build artifact. | `scripts/cloudrun/Dockerfile:17`-30; `docs/deploy/agent-runbook-terraform-ha.md:262`-266 |
| `examples/amp/Dockerfile` | `FROM scion-base:latest` | Examples owner | **Pending** — inherits the CLI the same way harness images do; adds only the Amp CLI via npm. | `examples/amp/Dockerfile:15` |
| `examples/adk_scion_agent/Dockerfile` | `FROM ${BASE_IMAGE}` (its own header comment: "Builds on scion-base") | Examples owner | **Pending** — inherits the CLI; its own comment states runtime input delivery depends on `scion message`/`send-keys` working through tmux, so this example is itself keys-adjacent in intent, not just incidentally built on `scion-base`. | `examples/adk_scion_agent/Dockerfile:16`-27 (header comment), `:31` (`FROM ${BASE_IMAGE}`) |
| `image-build/core-base`, `image-build/thick-prep` | — (foundations under `scion-base`) | Image-build owner | **Not raw-dependent.** These provide only system dependencies (Go/Node/Python) or the Cloud Workstations compatibility patch; neither builds or embeds the `scion` binary — `scion-base` is the first layer in the chain that does (§6's intro paragraph). | `image-build/core-base/Dockerfile`, `image-build/thick-prep/Dockerfile` — no `go build ... ./cmd/scion/` in either |
| `docs-site/Dockerfile` | `node:20-slim` → `nginxinc/nginx-unprivileged` | Docs-site owner | **Not raw-dependent.** Builds and serves the static Astro docs site only; no `go build` of any kind, no `scion` binary anywhere in the image. The 26th and last tracked Dockerfile in the repo at `e761178` (`git ls-files '*Dockerfile*'`), listed here so its exclusion from every other row is a checked fact. | `docs-site/Dockerfile:16` (`FROM node:20-slim`), `:42` (`FROM nginxinc/nginx-unprivileged:stable-alpine`) — no `cmd/scion` reference |
| `extras/scion-a2a-bridge`, `extras/scion-chat-app`, `extras/scion-discord`, `extras/cloudrun-iap-proxy`, `extras/docs-agent`, `extras/scion-telegram` (have a `Dockerfile`, confirmed via `git ls-files 'extras/*/Dockerfile'`) | own Dockerfiles, not `scion-base` | Extras owner | **Not raw-dependent** — standalone services, built per-item: `scion-a2a-bridge` and `scion-chat-app` from their own `./cmd/scion-a2a-bridge`/`./cmd/scion-chat-app`; `scion-discord` and `scion-telegram` from their own `./cmd/scion-plugin-discord`/`./cmd/scion-plugin-telegram`; `cloudrun-iap-proxy` and `docs-agent` from their module root (`go build ... .`, no `cmd/` directory in either). None builds `./cmd/scion` or inherits `scion-base`, so none is a `scion`-CLI-embedding agent image, and none constructs `raw` (§2/§4). (Only `scion-a2a-bridge` and `scion-chat-app` actually import `pkg/hubclient` — verified via `grep -rl`; `scion-discord`, `cloudrun-iap-proxy`, `docs-agent` and `scion-telegram` do not, so "hubclient SDK consumers" does not describe all six.) | `extras/scion-a2a-bridge/Dockerfile:35`; `extras/scion-chat-app/Dockerfile:33`; `extras/scion-discord/Dockerfile:22`-24; `extras/scion-telegram/Dockerfile:37`-39; `extras/cloudrun-iap-proxy/Dockerfile:23`; `extras/docs-agent/Dockerfile:30`; no `FROM scion-base` in any of them; `grep -rl "pkg/hubclient" extras/<name>` — 7/5/0/0/0/0 hits respectively |
| `extras/scion-slack`, `extras/scion-teams`, `extras/agent-viz`, `extras/fs-watcher-tool`, `extras/scion-broker-log` (no `Dockerfile` at all, per the same `git ls-files` check) | Extras owner | **Not applicable** — not a buildable image in this repository at all, so there is no "every Dockerfile" question for them to close; listed so their absence is a checked fact, not a silent omission. `scion-broker-log` is additionally covered in §2a as a `Raw`-field consumer, which is a source-level dependency, not an image one. | `ls extras/`; absence of `extras/<name>/Dockerfile` for each |

No version pins are recorded here for "the versions deployed" (the master design's removal-gate
text) because this is a planning/inventory task, not a deployment one (this task's own scope guard:
"No code, deployment, or agent dispatch is authorized merely by creation of this planning issue"
applies equally to its parent design). **4.1 must fill in actual deployed broker/Hub/client/image version
identifiers** against this table before the removal gate can be considered satisfied — this table
fixes *which* images and callers need a version recorded, not what those versions are yet.

## 7. Rollout checklist (for task 4.1, restated against this inventory)

Restates the master design's "Cutover and deployment" and "Removal gate" sections (and
`.design/agent-keys-contract.md` §11's verbatim copy) as a concrete, inventory-grounded checklist.
This is a checklist to execute, not a claim that any step below is already done:

1. **Keys-capable brokers.** Confirm every Runtime Broker intended to support `/keys` has the route
   (1.1/1.2, already merged on `main`). An old broker without it answers `422
   keys_unsupported` by design (AK-28/AK-47) — record which deployed brokers, if any, are still on
   a pre-1.1 build.
2. **Hub with the dedicated operation and the temporary bridge.** The dedicated `/keys` operation
   (2.1/2.2) and the temporary `message`/`raw` bridge (2.3) are both merged on upstream `main`.
   Record which deployed Hubs carry the bridge before relying on it.
3. **CLI/SDK binaries, agent images, and operator scripts.** §2's CLI/SDK migration (task 3.1) is
   merged; what remains is rebuilding and releasing every image in §6, in priority
   order: default-installed (`claude`, `gemini-cli`) first, then opt-in harnesses, then `omni`/`hub`.
   Record the actual released version of each image that carries the migrated CLI — this inventory
   names *which* images, 4.1 names *which version*.
4. **Removal build.** Only after 1-3 above and the removal gate below.

**Removal gate** (verbatim restatement, per contract §11's rule that this text must never drift from
the contract's own copy):

> Removal gate: exercise the replacement for user and agent callers in local/Hub modes; record the
> versions deployed; migrate known scripts/docs/images; demonstrate no known callers still depend
> on raw via content-free counters plus explicit owner confirmation. Inventory whether old callers
> enable message retries, and upgrade them or disable those retries before bridge use. Low observed
> usage alone is not proof. Do not dual-send or shadow-execute keys. The temporary bridge has an
> owner and removal task from day one. If rollout fails, pause removal or roll back the coordinated
> component set; never restore message-based raw fallback inside new clients. No database rollback
> is required because there is no schema change.

Applied against this inventory specifically:

- "Migrate known scripts/docs/images" → §2 (CLI/SDK — migrated by 3.1), §3 (skills — done), §6
  (images — pending rebuild and release).
- "No known callers still depend on raw... plus explicit owner confirmation" → every row in §2 marked
  **Migrated** still needs its owner's explicit confirmation, not just a code diff; every row
  in §5 is unknown and cannot be confirmed from this repository alone — the content-free bridge
  audit records (`route` = `message_raw_bridge`, `pkg/hub/execute_agent_keys.go:249`) are the only
  signal available for those.
- "Upgrade [retry-enabled callers] or disable those retries before bridge use" → §2's retry-posture
  list found no in-repo caller with retries enabled against the raw path; this gate is satisfied
  for known callers today and must be re-checked if that changes.
- "Record the versions deployed" → §6 names the images; actual version numbers are 4.1's to fill in.

## 8. Integration status

Tasks 2.3 and 3.1 have merged (upstream `main` at `e761178`). Each item this section previously
listed as pending integration, checked against that revision:

- `scion keys` (Hub mode) and the `message --raw` alias call the dedicated `/keys` API via
  `hubclient.AgentService.SendKeys`, not `SendStructuredMessage` (§2). Done.
- The `message`/`raw` compatibility bridge is wired in front of `authorizeAgentMessage` on both
  single-agent routes, delegating to `authorizeAgentKeys` and `admitAndDispatchAgentKeys`
  (`pkg/hub/agent_keys_message_bridge.go:279`,`290`), with legacy field-table enforcement (§2a).
  `message --raw` and legacy `raw` API callers are authorized like attach. Done.
- `pkg/hubclient.AgentService.SendKeys` exists, sends once, and bypasses `WithRetry` (§2). Done.
- Local-mode `scion keys` for an unlinked local project uses `Manager.SendKeysLocal` (§2). Done.
- `scion keys` reports `dispatched`/`rejected`/`unknown`; JSON output includes the outcome code and
  `operation_id` when the Hub returns one (`cmd/keys.go:138`-190). A managed-runtime target gets
  `422 keys_unsupported` from `/keys` (`pkg/hub/execute_agent_keys.go:366`-370), whether called
  directly, by the CLI, or through the bridge. An empty `keys` string is rejected with
  `invalid_request` before any request is sent, in both local and Hub mode (`cmd/keys.go:270`-282).
  Done.
- The "Migrating from `raw`" callout in `reference/api.md` (size ceiling, authorization tightening,
  2 MiB pre-authorization body cap, changed codes) now describes current behaviour
  (`pkg/hub/agent_keys_message_bridge.go:71`,`101`-111,`179`-292). Done.
- A migrated client calling a Hub without the `/keys` route gets that Hub's generic `404`; the CLI
  reports it as `hub_unsupported` and never falls back to `/message` (`cmd/keys.go:210`-221,
  `310`-316). Done.

Still pending (Phase 4): removal of the bridge, the hidden `--raw` alias, the residual raw code in
§2a, `MessageRaw`, and the `Raw` wire fields; image rebuilds in §6; and the removal gate in §7. Two
leftover pieces of raw plumbing are also 4.2 cleanup items, though no first-party path reaches them
with raw set:

- `hubclient` `SendStructuredMessage`/`SendStructuredMessageWithOptions` still forward a caller's
  `Raw` field (`pkg/hubclient/agents.go:608`-626).
- `cmd/message.go`'s `buildStructuredMessage(..., raw, ...)` still takes and sets `Raw`
  (`cmd/message.go:576`-579; callers `:618`, `:694`, `:770`, `:884`, `:1035`), although `--raw` now
  returns through the keys path before any of those callers runs.
