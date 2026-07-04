# Chat-App (Google Chat) HA Assessment

**Status**: Assessment only. No code changes in Phase 5.

## Current State

The chat-app integration (`extras/scion-chat-app/`) already runs as a standalone
process using self-managed net/rpc on `localhost:9090`. It has a Dockerfile and
can be deployed independently. However, it has three gaps that prevent Mode 3
(HA) adoption.

## Gap Analysis

### 1. Postgres Store

**Current**: SQLite via `mattn/go-sqlite3` (cgo-dependent).

**Required**: A Postgres backend behind the existing `Store` interface, following
the same pattern as Discord's `store_postgres.go`. Additionally, the SQLite
dependency should be swapped to `modernc.org/sqlite` for static builds
(CGO_ENABLED=0), matching the Discord plugin.

**Effort**: Medium. The store interface is clean. Table schema translation is
mechanical (8 tables).

### 2. gRPC Transport

**Current**: net/rpc via go-plugin. Cannot traverse Cloud Run or L7 load
balancers.

**Required**: Adopt the 5A gRPC broker scaffolding. The integration wraps its
`MessageBrokerPluginInterface` implementation with the generic gRPC server from
`pkg/plugin/grpcbroker`. Add `--standalone` entry point using the 5C runtime
library (lock loop, signal listener, update hook).

**Effort**: Low. The scaffolding exists; wiring is templated from 5C/5D.

### 3. Webhook Coordination

**Current**: Google Chat uses webhook-based push delivery. The chat-app receives
messages at a configured endpoint.

**Required**: In HA mode with multiple instances, webhook delivery can land on
any instance. Unlike Telegram (which has `update_id` for dedup), Google Chat
webhooks are not documented as having a unique message ID for dedup. Options:

- **Single-instance with advisory lock**: same as Discord. One instance handles
  webhooks; the other is standby. Simple, avoids dedup complexity.
- **Multi-instance with message-ID dedup**: if Google Chat provides a unique
  event ID in the webhook payload, use a Postgres dedup table. More scalable but
  requires API investigation.

**Recommendation**: Start with advisory-lock single-instance (matches Discord
pattern). Investigate multi-instance as a follow-up if Google Chat webhook
payloads include unique event IDs.

## Recommendation

File a follow-up issue mirroring #362 (Telegram HA). All three gaps are
addressable with the existing 5A-5D machinery:

1. Postgres store (same pattern as Discord/Telegram)
2. gRPC transport (5A scaffolding)
3. Standalone entry point with advisory lock (5C runtime library)

The mattn/go-sqlite3 to modernc swap should be done first as a separate change,
since it affects the build for all modes (not just HA).
