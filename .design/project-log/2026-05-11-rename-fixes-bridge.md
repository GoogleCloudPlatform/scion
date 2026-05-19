# Project Log - 2026-05-11 - Rename Fixes & A2A Bridge

Implemented fixes for A2A bridge messaging and minor rename issues identified in Code Review v5.

## Changes

### 1. A2A Bridge Messaging (Critical)
- Updated `extras/scion-a2a-bridge/internal/bridge/bridge.go`:
    - `SendMessage` now requests subscriptions for both `scion.project.*` and legacy `scion.grove.*` topics.
    - `parseTopic` and `extractProjectIDFromTopic` updated to handle both `scion.project` and `scion.grove` prefixes in broker topics.
- This ensures the bridge continues to receive messages even if the Hub still publishes to `scion.grove` topics during the transition.

### 2. ResolvedSecret.MarshalJSON (Low)
- Updated `pkg/api/types.go`:
    - Modified `ResolvedSecret.MarshalJSON` to set the legacy `grove` field to the value `"grove"` when `Source` is `"project"`.
    - This maintains compatibility for old clients expecting the category name "grove".

### 3. Receiver Renaming (Low)
- Updated `pkg/store/models.go`:
    - Renamed receiver for `IsSharedWorkspace` from `g *Project` to `p *Project` for consistency with other methods on the `Project` struct.

## Verification Results
- Main repository build: `go build ./...` passed.
- A2A bridge build: `cd extras/scion-a2a-bridge && go build ./...` passed.
- Topic mismatch confirmed via grep before implementation.
- All changes committed to `scion/rename-strategy` branch.
