# Agent-keys raw inventory (post-removal)

Companion to the frozen contract `.design/agent-keys-contract.md` (see its §0). Raw message
delivery was removed in Keys 4.2 (ptone/scion#2201). This inventory replaces the pre-removal
caller/image inventory and rollout checklist (tasks 3.2/4.1), which are preserved in git history
and are no longer accurate: there is no raw caller left to migrate, no bridge to observe, and no
image can deliver keystrokes through the message path whatever its `scion` binary version, because
every server-side ingress now rejects the field.

**Rule:** every remaining reference to message raw in the tree is exactly one of:

- **Rejection adapter**: code that detects the retired field and rejects it with 422
  `raw_input_removed` (or, in the CLI, fails `--raw` before any wire call);
- **Test**: a test pinning that rejection, its zero side effects, or that Plain/normal/interrupt
  messaging is unaffected;
- **Migration documentation**: text telling a reader that raw is removed and to use `scion keys`
  / `POST .../keys` instead, or historical design records explicitly marked as such.

Anything else is a regression. Unrelated uses of the word "raw" (secret `Encoding: "raw"`,
`ref.Raw` URI fields, `json.RawMessage`, `entsql.Raw`, koanf `Raw()`, template `?raw=` downloads,
GitHub raw-content URLs, web log-viewer `raw` entries) are not message raw and are out of scope.

## 1. Rejection adapters

| Ingress / surface | Spelling probed | Where | Order guarantee |
| --- | --- | --- | --- |
| Shared probe | top level + one named nested object, case-insensitive, any value, malformed treated as present | `pkg/messages/raw_tombstone.go` (`HasRetiredRawField`, `RawInputRemovedCode`, `RawInputRemovedMessage`) | n/a |
| Hub shared rejector | per caller | `pkg/hub/raw_tombstone.go` (`rejectRetiredRawMessageBody`; 2 MiB cap on every buffered ingress read (the `/message` routes before authorization, broadcast, broker inbound/routed), 413 `payload_too_large`; generic `replacement` that never names the resolved target; content-free audit `route=message_raw_removed`; broken read containing raw fails closed) | probe runs before decode |
| `POST /api/v1/agents/{id}/message` | `raw`, `structured_message.raw` | `pkg/hub/handlers_agents_core.go` (`rawIngressAgentMessage`) | after target lookup, before `authorizeAgentMessage`, persistence, dispatch |
| `POST /api/v1/projects/{project}/agents/{id}/message` | `raw`, `structured_message.raw` | `pkg/hub/handlers_projects_core.go` (`rawIngressProjectAgentMessage`) | same as above |
| `POST /api/v1/projects/{project}/broadcast` | `raw`, `structured_message.raw` | `pkg/hub/handlers_agent_messaging.go` (`rawIngressBroadcast`) | after project read authorization, before decode, fan-out, persistence |
| `POST /api/v1/broker/inbound` (plugin/broker inbound) | `raw`, `message.raw` | `pkg/hub/handlers_broker_inbound.go` (`rawIngressBrokerInbound`) | before decode, topic validation, and sender synthesis |
| `POST /api/v1/broker/inbound/routed` | `raw`, `message.raw` | `pkg/hub/handlers_broker_inbound_routed.go` (`rawIngressBrokerInboundRouted`) | same as above |
| Scheduled-event and recurring-schedule advanced `payload` JSON | `raw` key in the payload | `pkg/hub/raw_tombstone.go` (scheduled payload check), called from `pkg/hub/handlers_scheduled_events.go` and `pkg/hub/handlers_schedules.go` | payload shape check (400) → raw probe (422) → decode; before storage |
| Runtime broker `POST /api/v1/agents/{id}/message` | `raw`, `structured_message.raw` | `pkg/runtimebroker/handlers.go` (`sendMessage`; 2 MiB body cap, 413) | before decode and before any manager call |
| Plugin gRPC wire | field 11 | `proto/broker/v1/broker.proto` (`reserved 11; reserved "raw";`) | field number and name can never be reused |
| CLI `scion message --raw` | flag, any value | `cmd/message.go` (`errRawFlagRemoved`, rejected in cobra `Args`; flag kept hidden only to produce the guidance) | before `PersistentPreRunE` and before any client is built: zero wire calls |
| Outcome constant | n/a | `pkg/agentkeys/types.go` (`OutcomeRawInputRemoved`, 422), `pkg/agentkeys/doc.go` | n/a |

Not an ingress: the agent outbound message request (`pkg/hub/handlers_agent_messaging.go`) never
had a raw field; a `raw` member there is an unknown JSON field and is ignored. Decoding of
historical rows and payloads that still contain `"raw"` ignores the member (no Raw field exists in
`StructuredMessage`), so history is readable and never rewritten.

## 2. Tests

| Test | Covers |
| --- | --- |
| `pkg/messages/raw_tombstone_test.go` (`TestHasRetiredRawField`) | both spellings, every value shape, case-insensitivity, malformed input, raw-free controls |
| `pkg/messages/format_test.go` (`TestFormatForDelivery_IgnoresRetiredRawMember`) | historical rows with `raw` decode as normal messages |
| `pkg/hub/raw_tombstone_test.go` | both `/message` routes (user and agent callers, same/cross project), broadcast, broker inbound and routed (before sender synthesis and topic validation), content-free audit, pre-auth body cap, broken read; zero persistence/dispatch/event/user/conversation side effects; Plain/normal/interrupt controls |
| `pkg/hub/scheduled_payload_raw_tombstone_test.go` | scheduled-event and schedule payloads, including malformed values and positive controls |
| `pkg/hub/agent_dm_observer_test.go` | Normal and Plain DMs still publish the observer copy |
| `pkg/runtimebroker/message_raw_tombstone_test.go` | broker `/message` rejection with zero manager calls; Plain/normal/interrupt unaffected |
| `pkg/agentkeys/types_test.go`, `pkg/agentkeys/broker_test.go` | `OutcomeRawInputRemoved` status mapping and allowlist membership |
| `cmd/message_test.go` (`TestMessageCmd_RawFlag_ZeroWireCalls`) | `--raw` on every target form and mode makes zero HTTP calls |
| `cmd/message_deprecation_test.go` (`TestRemovedFlag_Raw`) | removal guidance names `scion keys` |

## 3. Migration documentation

- `docs-site/src/content/docs/reference/cli.md` (`scion message` / `scion keys`)
- `docs-site/src/content/docs/reference/api.md` (message endpoints, `raw_input_removed`)
- `docs-site/src/content/docs/reference/messaging-authorization.md` (retired raw field)
- `docs-site/src/content/docs/release-notes.md` (migration notice)
- `cmd/keys.go` help text ("replaces the removed 'scion message --raw' flag")
- `.design/agent-keys-contract.md` §0 (current state) and its bridge-era sections, retained as
  the historical record and marked post-removal in §6 and §11
- `.design/messaging-conversation-model*.md`, `.design/managed-agents.md`: historical design
  references to raw delivery

## 4. External callers

Callers outside this repository that still send `raw` receive 422 `raw_input_removed` with
guidance naming `scion keys` and `details.replacement` naming the `/keys` route; nothing is
delivered and nothing is persisted. The content-free `route=message_raw_removed` audit line (with
an `ingress` field) is the signal for finding such callers.
