# Agent-keys raw-caller and image inventory, and rollout checklist

Task **3.2**, part of the master agent-keys design, frozen contract
`.design/agent-keys-contract.md`. This is the repository-committed inventory and migration
checklist this task's own scope calls for ("Create the concrete rollout inventory and migration
checklist with broker/Hub/client/image versions and removal evidence requirements... Commit the
inventory and the rollout checklist as a repo doc"). It does not authorize removal: Phase 4 (tasks
4.1/4.2/4.3) still owns exercising the replacement and cutting over.

Tested/inventoried revision: `scion/agent-keys-2-2` at `c1af0cbc3` (includes 0.1/0.2/1.1/1.2/2.1/2.2
and the task 2.2 follow-up adding the agent-caller attach gate). Recheck before Phase 4 acts on
this: 2.3 and 3.1 are parallel, not-yet-available work at the time this inventory was written, and
will change some "Pending" rows below to "Migrated" once they land — see "Known pending
integration" at the end.

## 1. What "raw-dependent" means here

A caller or image is raw-dependent if it builds or transports a `Raw`/`raw`-flagged
`StructuredMessage`/`MessageRequest` (the pre-cutover keystroke-injection path), or if it embeds a
`scion` CLI binary whose `cmd/keys.go`/`cmd/message.go` still do so. It is not raw-dependent merely
for using `scion message`, `scion keys`, or the Hub messaging API without `raw`.

## 2. Repository CLI/SDK callers

| Caller | Owner | Migration status | Evidence |
| --- | --- | --- | --- |
| `cmd/keys.go` (`scion keys`, Hub mode) | Client owner (3.1) | **Pending.** Still builds a `Raw=true` `StructuredMessage` via `buildStructuredMessage` and calls `SendStructuredMessage` (`sendKeysViaHub`, `cmd/keys.go:96`-128) — the exact pre-cutover path the master design's finding #1 and contract §8 describe. Not yet switched to the dedicated `/keys` endpoint. | `cmd/keys.go:116`,`120` |
| `cmd/keys.go`'s own `--help` text (`Long`/`Examples`) | Client owner (3.1 scope item "Correct multi-key example") | **Pending — explicitly owned by 3.1, not this task.** The `Examples:` block still advertises `scion keys my-agent "Up Up Enter"` with no caveat, and the `Short`/`Long` text still says "Send raw keystrokes"/"Supports control keys like arrows, Escape, etc." without the one-argument/no-sequence correction the contract (§2.3, "Deviation from the master design recorded here") requires. This is the exact misleading example this task's own scope ("remove misleading sequence examples") describes, but it lives in Go source (`cmd/keys.go`), not a docs-site/skill file this task's scope covers, and the lead has ruled it is 3.1's to fix alongside the rest of the CLI migration, not 3.2's to edit — 3.2 does not touch `cmd/`. The 3.1 CLI migration's fork PR (open at the time of writing) does not yet touch these lines (checked via its current diff); this row stays Pending until it does. | `cmd/keys.go:33`-35,`45`-48 |
| `cmd/keys.go` (local mode) | Client owner (3.1) | **Pending (3.1) — this caller is migrated, not unaffected.** 3.1's own scope says "local mode uses manager SendKeys"; today it still calls `agent.Manager.MessageRaw` directly (`cmd/keys.go:86`), which 1.1 already added `SendKeys` alongside without removing (§2a's `MessageRaw` row). Switching this one call site from `MessageRaw` to `SendKeys` is what 3.1 must do for local mode: it adds the `agent_id` container-label identity check `MessageRaw` lacks (`pkg/agent/manager.go:774`-792) and, per contract §2.2, rejects an empty string — `MessageRaw` does not, so local-mode semantics do **not** already fully match the frozen contract today (see §8's empty-string note). Only the one-tmux-argument/no-auto-Enter delivery shape is already identical between the two primitives. **Narrower gap found in 3.1's current fork PR head (`6fa2e72cd`):** its `resolveLocalKeysTarget` requires a Hub-linked project ID to resolve `SendKeys`'s `projectID` parameter at all — a local project with no `scion hub enable` link gets a hard error ("requires this project to have a Hub-linked project ID (none found)"), not a working local-only keys call; its own `TestMessageCmd_RunE_Raw_LocalMode_UsesSendKeysLocal` asserts exactly that error text for the unlinked case. Per the lead, 3.1 is expected to add a further primitive, `Manager.SendKeysLocal`, for unlinked local projects — that primitive is not on 3.1's current head and is not claimed as shipped anywhere in this task's docs. | `cmd/keys.go:86`; `pkg/agent/manager.go:774`-792; 3.1's fork PR (`gh pr diff`, `resolveLocalKeysTarget` and `TestMessageCmd_RunE_Raw_LocalMode_UsesSendKeysLocal`) |
| `cmd/message.go` `--raw` flag | Client owner (3.1) | **Deprecated, hidden, not yet rerouted.** Hidden (`MarkHidden("raw")`, `cmd/message.go:1470`) and emits a deprecation warning naming `scion keys` as the replacement (`cmd/message.go:63`), satisfying the "no replacement the command cannot yet parse" rule (AC-15a) since `scion keys` already exists and works end-to-end for local mode. Still delivers via `mgr.MessageRaw(ctx, agentName, "", message)` directly in local mode (`cmd/message.go:482`-485, no `StructuredMessage` involved) or, in Hub mode, via `buildStructuredMessage(..., msgRaw, ...)` (`:532` sets `msg.Raw = raw`) followed by the Hub `SendStructuredMessage` path (same as `cmd/keys.go`'s Hub path) — not yet the hidden keys-primitive alias 3.1's scope calls for. | `cmd/message.go:63`,`482`-485,`1458`,`1470` |
| `pkg/hubclient` (`AgentService`) | Client owner (3.1) | **Pending.** No `SendKeys` method exists yet; `SendStructuredMessage`/`SendStructuredMessageWithOptions` (`pkg/hubclient/agents.go:579`,`589`) are the only way a caller can set `Raw` today, and nothing in this package rejects it. 3.1 adds the typed `SendKeys` call and the no-retry-override described in the contract. | `pkg/hubclient/agents.go:579`,`589` |
| `pkg/apiclient/transport.go` / `pkg/hubclient.WithRetry` | Client owner (3.1) | **Pending (3.1) — already in scope, not undecided.** `WithRetry` (`pkg/apiclient/transport.go:59`-62, exposed to callers as `hubclient.WithRetry`, `pkg/hubclient/client.go:473`) is a transport-wide, per-client setting; 3.1's own scope ("bypasses generic transport retries even when WithRetry is configured") and its AC4 ("WithRetry(>0) … cannot replay keys") already require 3.1 to make the keys call bypass it even when a caller configured retries for everything else (contract §4.3, AK-34). | `pkg/apiclient/transport.go:59`-89; `pkg/hubclient/client.go:472`-477 |

**Default retry posture of in-repo callers**, recorded because the contract requires tracking which
legacy raw clients enable message retries (the master design's "Inventory whether old callers enable
message retries"):

- The first-party `scion` CLI never calls `hubclient.WithRetry` when building its Hub client
  (`cmd/hub.go:531`-592, the only options appended are auth and a 30s timeout) — `MaxRetries`
  therefore defaults to `0` (`pkg/apiclient/transport.go:89`). **No *client-side* retry exposure for
  the CLI's own raw path today.**
- **Server-side retry exists today on the legacy raw path and is not yet closed.** The Hub's own
  dispatch of a single-agent raw request (and the agent-to-agent DM raw path) goes through
  `dispatchWithBrokerRetry`, which retries with exponential backoff (capped at 5s, overall deadline
  30s) whenever the dispatcher returns `ErrMessageDeferred` — i.e. a temporarily unreachable broker
  can still receive a raw/keystroke delivery up to ~30s after the original request, with no client
  involvement or awareness. This is the opposite of `/keys`'s immediate, single-attempt,
  never-queued semantics (contract §4.2's execute-before deadline, §4.3's no-replay rule) and is
  exactly what 2.3's cutover removes for the raw path specifically — recorded here as **pending
  removal by task 2.3**, not a client-retry gap this task's CLI/SDK scope (3.1) would close.
  Evidence: `pkg/hub/broker_routing.go:140`-167 (`dispatchWithBrokerRetry`, `brokerRetryMaxBackoff`),
  `pkg/hub/handlers_agent_messaging.go:2581`-2585, `pkg/hub/agent_dm_operation.go:627`-629.
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
  obligation on this task, this is recorded as **unknown**, not assumed safe — see §5.

## 2a. Hub/broker/shared-type consumers

Per r1 review finding 5: §1's own definition ("builds or transports a `Raw`-flagged
`StructuredMessage`/`MessageRequest`") also covers the server-side legacy raw path, not only the
client-side callers in §2. These are the components task 4.2 ("Complete `MessageRaw` → `SendKeys`
cleanup") removes or replaces, distinct from the dedicated `/keys` route
§7 step 1 already covers:

| Component | Owner | Migration status | Evidence |
| --- | --- | --- | --- |
| Runtime Broker's legacy raw consumer (`isRaw` → `mgr.MessageRaw`) | Broker owner (4.2) | **Pending removal.** The broker's own `/message` action handler branches on `req.StructuredMessage.Raw` and calls `mgr.MessageRaw` directly, bypassing the paste buffer/debounce and logging `"message delivered (raw, unbuffered)"` — this is the broker-side half of the legacy path the dedicated `/keys` broker route (§7 step 1, already merged) does not replace or remove. | `pkg/runtimebroker/handlers.go:2162`-2166,`2211`-2213 |
| **Hub message-path raw handling** (`agent_dm_operation.go`, `raw_guard.go`) — the agent-to-agent DM half | Hub ingress owner (0.2/2.3/4.2) | **Pending 2.3/4.2 — describing the current base state, not task 2.3's unmerged changes.** `AgentDMInput.Raw` (deprecated client flag, "still functional", `:74`-77); step 4b, the foreign-raw cross-project denial (`MessageDenialCrossProjectRawUnsupported`/`cross_project_raw_unsupported`, `:358`-372); step 4c, the managed-backend raw denial (`MessageDenialRawManagedUnsupported`/`raw_managed_backend_unsupported`, `:376`-390). `raw_guard.go`'s Phase-0.2 containment guards — `crossProjectRawUnsupported` (`:44`), `evaluateRawMessageGuard` (`:116`, the combination-rejection switch), `isRawConversationAddressed` (`:184`), `rejectRawScheduledPayload` (`:206`) — and the `MessageDenialRaw*`/`MessageDenialCrossProjectRawUnsupported` code constants (`pkg/hub/authorize_message.go:46`,`59`-75) are explicitly "containment guards only, not a new permanent raw policy engine" pending the 2.3 bridge. **Not yet merged into this base, but already in flight:** task 2.3's fork PR (open) removes step 4b entirely — raw agent-to-agent DMs stop reaching `ExecuteAgentDM` at all once the bridge classifies them first — and relies on a new dispatch-layer backstop (`httpdispatcher.go`) instead; step 4c and the `raw_guard.go` functions are unchanged by it. Recheck this row once that work (or whatever its successor is) actually merges into `scion/agent-keys-2-2`. | `pkg/hub/agent_dm_operation.go:74`-77,`358`-372,`376`-390; `pkg/hub/raw_guard.go:44`,`116`,`184`,`206`; `pkg/hub/authorize_message.go:46`,`59`-75 |
| **Single-agent `POST /:id/message` raw path** (`handlers_agent_messaging.go`) | Hub ingress owner (2.3/4.2) | **Pending 2.3/4.2 — this is the main raw ingress, not a secondary one.** Top-level `raw` request field and OR-merge onto `StructuredMessage.Raw`; the managed-backend rejection and the pre-conversation cross-project raw guard on this HTTP path (separate code from, but mirroring, `agent_dm_operation.go`'s step 4b/4c above); raw-aware mention-fanout skip, offload `Qualifies()` call, and observer-publish skip. This is the exact surface task 2.3 bridges onto `ExecuteAgentKeys` and task 4.2 removes; §2a's `agent_dm_operation.go` row above covers the agent-to-agent DM half, this row covers the single-agent HTTP-handler half — the two are siblings, not duplicates. | `pkg/hub/handlers_agent_messaging.go:1482`,`1528`-1529,`1600`,`1698`,`1715`,`2345`,`2400`,`2487`,`2613` |
| Message-broker plugin gRPC protocol `raw` field | 4.2 (in-repo); **plugin owner — unknown** for out-of-repo plugins, same as §5 | **Pending removal, with an external compatibility concern §5's other rows don't have.** `StructuredMessage.raw` is wire field 11 in the plugin gRPC protocol itself, not just an internal Go struct field — it round-trips to and from every message-broker plugin, in-repo or not. Removing the Go field without reserving the wire number would let a stale out-of-repo plugin binary silently reuse field 11 for something else; 4.2 must add `reserved 11;` to the `.proto` (and bump/document the protocol version) rather than merely deleting the field, and the migration note should say so. Whether any out-of-repo plugin actually reads/sets this field is **unknown**, consistent with §5. | `proto/broker/v1/broker.proto:44` (`bool raw = 11;`); `proto/broker/v1/broker.pb.go:51`,`168`; `pkg/plugin/grpcbroker/convert.go:43`,`82` |
| `pkg/messaging` raw-aware rendering/offload, and fan-out forwarding | Message-domain owner (4.2) | **Pending removal.** `DeliveryOptions.Raw`/`Qualifies()`'s raw parameter govern whether rendering returns the bare body instead of the normal envelope and whether a message qualifies for offload (called from both `agent_dm_operation.go:585` and `handlers_agent_messaging.go:2487`); `messagebroker.go`'s `fanOutToProject`/`fanOutGlobal` forward `msg.Raw` unchanged today, documented as safe only because 0.2 already ensures no Hub publisher places a raw message on that bus — a guarantee that becomes moot, not merely safe, once the field is gone. | `pkg/messaging/delivery.go:77`; `pkg/messaging/delivery_compat.go:40`-44; `pkg/messaging/render_delivery.go:86`,`129`; `pkg/messaging/offload.go:189`; `pkg/hub/messagebroker.go:742`-746 |
| `messages.StructuredMessage.Raw` wire field, incl. its log-attribute handling | Message-domain owner (4.2) | **Pending removal**, with a decode-time tombstone per contract §6.1/AK-36 rather than silent deletion. `FormatForDelivery` already special-cases `Raw` identically to `Plain` (return the bare `Msg`, no envelope); `LogAttrs()` already redacts raw message content in structured logs (`redactedRawContent`) but still logs the `raw` boolean itself — both are call sites 4.2 must account for when the field goes. | `pkg/messages/types.go:133`,`304`-315; `pkg/messages/format.go:71`-73 |
| `agent.Manager.MessageRaw` primitive | Runtime owner (1.1 added `SendKeys` alongside it; 4.2 removes it) | **Deprecated, still live.** Doc-commented "pending Phase 4 removal... performs no `agent_id` identity binding. New callers must use SendKeys" — this is the exact primitive §2's `cmd/keys.go`/`cmd/message.go` rows and this table's broker-handler row both still call into. Four test doubles also implement this interface method and will need updating when it goes. | `pkg/agent/manager.go:83`-85,`429`; mock implementations at `pkg/runtimebroker/handlers_test.go:130`, `heartbeat_test.go:135`, `protocol_mismatch_test.go:117`, `workspace_handlers_test.go:70` |
| `extras/scion-broker-log` (`msg.Raw` field read/display) | Extras owner | **Consumer only; affected by field removal, not itself raw-dependent in the §1 sense** — it reads and summarizes an inbound `messages.StructuredMessage.Raw` value (`flagsSummary` appends `"raw"` to a flag list) for log/audit display; it never constructs or transports a raw request. When 4.2 removes the `Raw` field, this tool's event struct and summary function need a matching update or they silently stop compiling/displaying it. | `extras/scion-broker-log/main.go:457`,`487`,`522` |

## 3. Skills and templates

| Source | Owner | Migration status | Evidence |
| --- | --- | --- | --- |
| `resources/platform_skills/scion-agent-manage/references/troubleshooting.md` | Docs/inventory owner (3.2, this task) | **Migrated in this change.** Previously told operators to run `scion message <agent> --raw "ENTER"` / three separate `--raw` calls — correct in effect (verified live: tmux resolves named keys like `Enter` case-insensitively, so `"ENTER"` did submit, same as `"Enter"`), but it used the deprecated `message --raw` flag and a non-canonical spelling. Rewritten to `scion keys <agent> "Enter"`, the current command and the canonical spelling, keeping the existing one-key-per-call shape (never presented as a sequence). | This task's diff to that file |
| `resources/platform_skills/scion-messaging/SKILL.md` | Docs/inventory owner (3.2, this task) | **Already used `scion keys`, not `--raw`; amended in this change** to state the attach-vs-message authority distinction and the no-sequence rule explicitly, since neither was previously called out and an agent reading only this skill could reasonably assume message authority was sufficient. | This task's diff to that file |
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
- Any fork or private deployment of the harness images in §6 that has not rebased onto a
  `scion`-binary release carrying the 3.1 CLI migration.
- Any operator script, cron job, or external automation outside this repository that shells out to
  `scion message --raw` or `scion keys` directly.
- These callers' retry behavior (whether they wrap the Hub API in their own retry loop, independent
  of `hubclient.WithRetry`) is likewise unknown.

**Removal-gate consequence:** 2.3/4.1 must use the Hub's content-free raw-bridge usage counters
(contract §5's audit field list, `route: "raw"` — added by task 2.3; **no such audit emission
exists yet at the tested revision**, verified by `git grep`ing `pkg/` for a `"raw"` route value in an
audit event) to detect whether *any* caller — known or unknown — is still exercising the bridge,
rather than relying on this static inventory alone. Low observed usage is explicitly **not** proof
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
deployment step, after the Hub bridge, before removal). Completeness is checked against
`git ls-files '*Dockerfile*'` run repo-wide (26 tracked Dockerfiles at the tested revision), not
only under `extras/*` — an r5 review finding caught five Dockerfiles outside `image-build/`,
`harnesses/` and `extras/` that an `extras/`-scoped sweep had missed.

| Image | Base chain | Owner | Migration status | Evidence |
| --- | --- | --- | --- | --- |
| `image-build/scion-base` | `core-base` or `thick-prep` | Image-build owner | **Pending** — builds the `scion` binary from this repo's current `cmd/`; migration status always matches whatever commit it's built from. Not itself raw-dependent code, but the sole source of the binary every image below inherits. | `image-build/scion-base/Dockerfile:36`-64 |
| `image-build/hub` (Hub server image, GKE-oriented) | `scion-base` | Image-build owner | **Pending CLI migration** (embeds the same `scion` binary for in-container CLI use); the Hub *server* code itself (not the CLI) is already on the real `/keys` implementation — see §8, "Everything else this task documents... is already real and merged." Per `docs/deploy/agent-runbook-terraform-ha.md:263`-266, this is explicitly **not** the image used for the HA Cloud Run deployment pattern (`scripts/cloudrun/Dockerfile` below is) — it runs as root and is called out as "the wrong hub image for this Cloud Run pattern," so 4.1's version-recording step should not assume this row alone covers every deployed Hub image. | `image-build/hub/Dockerfile` |
| `image-build/omni` (Hub + all harnesses, Cloud Run Instances) | harness chain → `scion-base` | Image-build owner | **Pending** — rebuilds `scion` without `no_embed_web` but from the same `cmd/keys.go` source; inherits every harness image's migration status below. | `image-build/omni/Dockerfile:16`-30 |
| `harnesses/claude` (default-installed) | `scion-base` | Harness owner | **Pending** — default-install harness; highest-priority rebuild once 3.1 lands. | `harnesses/claude/Dockerfile` |
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
| `docs-site/Dockerfile` | `node:20-slim` → `nginxinc/nginx-unprivileged` | Docs-site owner | **Not raw-dependent.** Builds and serves the static Astro docs site only; no `go build` of any kind, no `scion` binary anywhere in the image. The 26th and last tracked Dockerfile in the repo at the tested revision (`git ls-files '*Dockerfile*'`), listed here so its exclusion from every other row is a checked fact. | `docs-site/Dockerfile:16` (`FROM node:20-slim`), `:42` (`FROM nginxinc/nginx-unprivileged:stable-alpine`) — no `cmd/scion` reference |
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
   (1.1/1.2, already merged on this branch). An old broker without it answers `422
   keys_unsupported` by design (AK-28/AK-47) — record which deployed brokers, if any, are still on
   a pre-1.1 build.
2. **Hub with the dedicated operation and the temporary bridge.** The dedicated `/keys` operation
   (2.1/2.2) is merged on this branch; the temporary `message`/`raw` bridge (task 2.3)
   is not yet. Do not treat the bridge as shipped until task 2.3 merges.
3. **CLI/SDK binaries, agent images, and operator scripts.** Depends on §2's CLI/SDK migration
   (task 3.1) landing, then rebuilding and releasing every image in §6, in priority
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

- "Migrate known scripts/docs/images" → §2 (CLI/SDK), §3 (skills — done in this change), §6 (images,
  pending 3.1 + rebuild).
- "No known callers still depend on raw... plus explicit owner confirmation" → every row in §2 marked
  **Pending** needs its owner's explicit confirmation once migrated, not just a code diff; every row
  in §5 is unknown and cannot be confirmed from this repository alone — the content-free bridge
  usage counters (contract §5) are the only signal available for those.
- "Upgrade [retry-enabled callers] or disable those retries before bridge use" → §2's retry-posture
  list found no in-repo caller with retries enabled against the raw path; this gate is satisfied
  for known callers today and must be re-checked if that changes.
- "Record the versions deployed" → §6 names the images; actual version numbers are 4.1's to fill in.

## 8. Known pending integration (for the final integration check)

The following are specified by the frozen contract and by tasks 2.3/3.1, but are not yet
implemented on this branch. They are called out here, and in this task's PR body, precisely so the
integration check after 2.3/3.1 land can confirm each one against the docs this task ships:

- `cmd/keys.go`'s Hub-mode path and `cmd/message.go`'s `--raw` alias still use the legacy
  `SendStructuredMessage` transport, not the dedicated `/keys` API (§2 above) — task 3.1.
- The `message`/`raw` compatibility bridge (classification before `authorizeAgentMessage`,
  `ExecuteAgentKeys` reuse, legacy field table enforcement) is not yet wired into
  `handleAgentMessage` — task 2.3. Until it lands, `message --raw` denies/authorizes using
  the pre-existing message-mode path described in contract §6.3, not the attach-based decision
  `/keys` and this task's docs describe for the bridge.
- `pkg/hubclient.AgentService.SendKeys` does not exist yet (§2) — task 3.1.
- Local-mode `scion keys` for an **unlinked** local project (no `scion hub enable`) depends on a
  further primitive, `Manager.SendKeysLocal`, that is not on task 3.1's current fork PR head
  (`6fa2e72cd`) — that head's `resolveLocalKeysTarget` requires a Hub-linked
  project ID to call `SendKeys` at all, and errors clearly for a project with none (§2's local-mode
  row). The integration check should confirm `SendKeysLocal` (or whatever it's eventually named)
  actually lands before treating unlinked-local-project `scion keys` as supported.
- Consequently, today's `scion keys`/`message --raw` in Hub mode does not print an `operation_id`,
  and a managed-runtime target gets the pre-existing `422 unsupported_capability`/
  `raw_managed_backend_unsupported` denial rather than the dedicated operation's `422
  keys_unsupported` — the integration check should confirm both flip once task 3.1 lands. `scion keys`
  itself goes through the single-agent `/message` route, so its denial is written at
  `pkg/hub/handlers_agent_messaging.go:1698`-1701, before `ExecuteAgentDM` is ever reached; the
  agent-to-agent DM route's equivalent check (`pkg/hub/agent_dm_operation.go:376`-390, §2a above)
  governs a *different* caller (another agent messaging this one), not `scion keys`'s own call —
  cited together here since both produce the same code today and both are replaced by
  `keys_unsupported` post-bridge.

  An empty `keys` string is **not** one of these: in Hub mode it is already refused client-side
  (`messaging.ValidateLegacyMessage`, `pkg/messaging/validate_compat.go:74`-76, requires a non-empty
  `Msg`), just with a generic `msg field is required` error instead of the dedicated operation's
  `400 invalid_request`; only in **local** mode does an empty string currently reach tmux unchanged
  (`cmd/keys.go:86` → `MessageRaw`, no validation). The CLI/docs text in this task's diff says
  exactly this.
- **Migration note for contract §6.2/§6.3/§6.4** (the size-ceiling reduction, the authorization
  tightening, and the new 2 MiB pre-authorization body cap the contract requires 3.2 to document):
  added as a "Migrating from `raw`" callout under `reference/api.md`'s `/:id/keys` entry, explicitly
  opened as planned follow-up work since none of it is in effect at the tested revision.
  The integration check should confirm the callout's three behavior changes actually occur once 2.3
  ships, and remove the "none of this is in effect yet" qualifier at that point.
- The un-upgraded-Hub case (a 3.1-migrated client calling a pre-2.1 Hub that has no `/keys` route at
  all, answering with the route's own generic unknown-action `404` rather than any `agentkeys.Outcome`
  value) only arises once 3.1 ships a client that calls `/keys` directly; it is not yet reachable
  through today's CLI. Recorded here as a 3.1 integration item rather than documented as a present
  behavior with no real caller to exhibit it yet.

Everything else this task documents (the `/keys` HTTP API itself, its authorization table, the
agent-caller attach gate added as a task 2.2 follow-up, rate limits, audit, and outcome codes) is
already real and merged on `scion/agent-keys-2-2` at the tested revision above — not a
forward-looking claim.
