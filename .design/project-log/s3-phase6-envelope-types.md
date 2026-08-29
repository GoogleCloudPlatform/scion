# S3 Phase 6: New Envelope Types and Taxonomy Split

**Date:** 2026-08-27
**Author:** dev-envelope-types
**Branch:** scion/ca-msg-em3 (based on scion/messaging-v2)

## What was built

### New types (`pkg/messaging/envelope.go`)

Defined the new envelope types that replace the old `StructuredMessage` and its
8-value type enum. The key design decision is the **split taxonomy**:

| Layer | Type | Values |
|-------|------|--------|
| Kind | `MessageKind` | `text`, `event` |
| Intent (text only) | `TextIntent` | `inform`, `request`, `question` |
| Event type (event only) | `EventType` | `agent.state-changed`, `agent.input-needed`, `delivery.failed`, `schedule.fired`, `port.exposed` |

Supporting types:
- `PrincipalRef` — `kind:id` format (user/agent/system) with parsing helpers
- `AddressedVia` — how an addressee was selected (explicit, body-mention, default-agent, direct)
- `DeliveryState` — per-addressee tracking (pending, delivered, failed)
- `Visibility` — consumer filtering (normal, verbose, full)
- `Message` — the new envelope struct with kind/intent mutual exclusivity validation
- `Addressee` — per-recipient delivery record
- `EventBody` — structured event payload (type, subject, status, reason, url)
- `AttachmentRef` — file reference with path and optional name

All enum types have validation functions and registered value maps.

### Compatibility layer (`pkg/messaging/envelope_compat.go`)

Three mapping functions bridge old and new formats:

1. **`MapLegacyType(oldType, systemCategory, hasAddressee)`** — maps the old 8-value
   type to the new split taxonomy
2. **`MapLegacyEnvelope(old)`** — full `StructuredMessage` → `Message` + `[]Addressee`
3. **`NewEnvelopeToLegacy(msg, addrs)`** — reverse for backward compatibility

Plus `MapLegacyDeliveryArtifact(oldType)` for extracting `AddressedVia` from
delivery artifact types.

## Mapping table

| Old type | New kind | New intent/event | Notes |
|----------|----------|-----------------|-------|
| `instruction` | `text` | `intent: request` | Clean mapping |
| `chat` | `text` | `intent: inform` | Clean mapping |
| `assistant-reply` | `text` | `intent: inform` | Provenance now in `from` field |
| `input-needed` (addressed) | `text` | `intent: question` | Ambiguous — splits on hasAddressee |
| `input-needed` (broadcast) | `event` | `event.type: agent.input-needed` | Ambiguous — splits on hasAddressee |
| `state-change` | `event` | `event.type: agent.state-changed` | Clean mapping |
| `system` (scheduler) | `event` | `event.type: schedule.fired` | Maps system_category metadata |
| `system` (port-forward) | `event` | `event.type: port.exposed` | Maps system_category metadata |
| `system` (delivery-failed) | `event` | `event.type: delivery.failed` | Maps system_category metadata |
| `system` (unknown) | `event` | `event.type: agent.state-changed` | Fallback with log warning |
| `mention` | `text` | `intent: request` | Delivery artifact → addressee via=body-mention |
| `group-set` | `text` | `intent: request` | Delivery artifact → addressee via=explicit |

## Ambiguous cases

### `input-needed`
This old type conflates two concepts: (1) an agent asking a specific person a
question, and (2) a system notification that an agent needs input. The split
uses `hasAddressee` (recipient set and not broadcast) to distinguish. When
addressed → `text/question`; when broadcast → `event/agent.input-needed`.

### `system`
The old `system` type requires `system_category` metadata to determine the
specific event. Unknown categories default to `agent.state-changed` with a log
warning rather than failing, to be resilient to new categories added before the
mapping is updated.

### `mention` and `group-set`
These are delivery artifacts, not message kinds. The old format used them as
message types, but they really describe *how* a recipient was selected. In the
new format, this information moves to `Addressee.Via`. The mapping returns the
underlying message kind/intent (`text/request`) and a separate function
provides the `AddressedVia` value.

## Decisions made

1. **Synthesised message IDs** — The old format has no ID field. Legacy envelope
   conversion synthesises IDs as `legacy-{timestamp}`. Production callers
   should assign real IDs before persisting.

2. **Sender heuristic** — When `SenderID` is empty, `buildPrincipalRef` checks
   if the `Sender` string already has a colon prefix (e.g. `agent:builder`).
   If not, it defaults to `system:{name}`.

3. **Round-trip fidelity** — The old→new→old round-trip preserves essential
   semantics (type, body, sender, status) but not all fields. The old format
   cannot represent multiple addressees, delivery state, or the kind/intent
   split, so some information is lost in the reverse direction.

4. **Visibility mapping** — Empty visibility in the old format maps to
   `VisibilityNormal` in the new format, and `VisibilityNormal` maps back to
   empty (matching old convention of empty = normal).

## Test coverage

- 40+ tests in `envelope_test.go` covering all enum validations, `Message.Validate`
  mutual exclusivity (text-without-intent, text-with-event, event-without-body,
  event-with-intent), `Addressee.Validate`, `EventBody.Validate`, `PrincipalRef`
  parsing.
- 30+ tests in `envelope_compat_test.go` covering all 8 old type values
  (table-driven), both `input-needed` scenarios, all `system` category variants,
  delivery artifact via extraction, full envelope conversion, visibility mapping,
  attachment mapping, and round-trip tests in both directions.

All tests pass: `go test ./pkg/messaging/... -count=1` and `go build ./...`.
