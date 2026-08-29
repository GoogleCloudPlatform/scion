# Phase 9 — New Delivery Format

**Agent:** dev-delivery-format
**Branch:** `scion/ca-msg-em3` (base: `scion/messaging-v2`)
**Date:** 2026-08-27

## Summary

Replaced the agent-facing delivery JSON with a new conversation-based format that
delivers `status` and `visibility` as structured fields, removes the metadata
allowlist, and gives agents a clean envelope they can act on without parsing prose.

## Problem

The old `deliveryMessage` in `pkg/messages/format.go` dropped critical fields before
the agent saw them:

- **`status`** — the only machine-readable value on lifecycle events, forcing agents
  to parse English prose to determine completion state.
- **`visibility`** — agents had no way to know their message's visibility level.
- **Addressing** — `broadcasted` was a flat boolean; no structured addressee list.
- **Metadata allowlist** — 5 keys smuggled through a generic map instead of being
  first-class fields.

## Changes

### New files

| File | Purpose |
|---|---|
| `pkg/messaging/delivery.go` | `ConversationInfo`, `DeliveryEnvelope`, `DeliveryOptions`, `FormatNewDelivery()` |
| `pkg/messaging/delivery_compat.go` | `FormatLegacyAsNewDelivery()` — adapter from `StructuredMessage` to new format |
| `pkg/messaging/delivery_test.go` | 13 tests covering all delivery format behaviour |
| `pkg/messaging/delivery_compat_test.go` | 11 tests covering legacy adapter and round-trip |

### Key design decisions

1. **New format alongside old** — `FormatForDelivery()` in `pkg/messages/format.go`
   is untouched. The new `FormatNewDelivery()` lives in `pkg/messaging/delivery.go`.
   Callers switch when ready; no breaking change.

2. **Conversation as first-class context** — `conversation` replaces `channel` +
   `thread_id`. Agents get `conversation.id` to echo on replies (AC-11).

3. **Kind/intent/event taxonomy** — `kind` + `intent`/`event` replace the overloaded
   `type` enum. Event messages carry `event.status` as a structured field (AC-10).

4. **Closed field set** — No `metadata` map, no `broadcasted`, no `urgent`. The
   envelope has exactly the fields agents need (AC-12).

5. **Legacy adapter** — `FormatLegacyAsNewDelivery()` converts `StructuredMessage`
   via Phase 6's `MapLegacyEnvelope()`, synthesizing `ConversationInfo` when none is
   available. This enables the new format even before all callers have migrated.

6. **Delimiters unchanged** — `---BEGIN SCION MESSAGE---` / `---END SCION MESSAGE---`
   remain identical for backward-compatible parsing.

## Acceptance Criteria

- **AC-10** ✅ `status` on lifecycle events is a structured field (`event.status`) in the
  delivery JSON, verified by `TestFormatNewDelivery_EventWithStatus` and
  `TestFormatLegacyAsNewDelivery_EventStatusDelivered`.
- **AC-11** ✅ `conversation.id` is present in every delivery envelope.
- **AC-12** ✅ Closed, small set of fields — no metadata map, no broadcasted flag.
- ✅ `visibility` delivered to agents.
- ✅ Plain/raw delivery returns raw text.
- ✅ Delimiters identical to existing format.
- ✅ `go test ./pkg/messaging/...` passes (24 new tests, all existing tests unaffected).
- ✅ `go build ./...` passes.

## Boundaries

- Did NOT modify `FormatForDelivery()` in `pkg/messages/format.go`.
- Did NOT modify hub handlers, CLI, or broker code.
- Did NOT modify envelope types in `pkg/messaging/envelope.go` (Phase 6 owns those).
