# Fix: Set[] Broadcasts Don't Propagate Attachments

**Date:** 2026-05-14
**Issue:** I5 — Attachments missing from Set[] broadcast messages

## Problem

In `cmd/message.go`, the `sendSetMessageViaHub` function constructs an
`OutboundMessageRequest` for user recipients without the `Attachments` field.
This means files attached via `--attach` are silently dropped when using
`set[]` broadcast addressing, even though the single-recipient path in
`sendOutboundMessageViaHub` correctly includes `Attachments: msgAttach`.

## Fix

Added `Attachments: msgAttach` to the `OutboundMessageRequest` struct literal
in `sendSetMessageViaHub` (~line 599), matching the pattern already used in
`sendOutboundMessageViaHub` (~line 521).

## Verification

- `go vet ./cmd/scion/` passes (pre-existing unrelated error in
  `pkg/hub/telegram_link.go` is not affected by this change).
- Single-line, low-risk change — no behavioral side effects beyond restoring
  the intended attachment propagation.
