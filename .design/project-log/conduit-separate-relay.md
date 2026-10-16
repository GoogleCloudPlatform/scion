# Conduit: separate relay role (Phase 6.2)

**Date:** 2026-10-14
**Issue:** ptone/scion#2815

## What was built

- **`scion server --role relay`**: a policy-free conduit relay process with no hub, Runtime Broker or web. It reaches the database only through the registry store (`pkg/store/relaydb`): `relay_instances`, `conduit_sessions`, the experiments settings row, and LISTEN on the conduit channel. An import check (`pkg/conduit/relayrole/relayserver/imports_test.go`) parses the relay's packages and refuses imports of `pkg/store`, `pkg/ent`, `pkg/hub` or database drivers.
- **Hub internal API** under `/internal/v1/hub/`, served on `server.hub.conduit.internal_listen`. The relay calls it for admission, re-checks and refreshes (`pkg/conduit/relay/hubclient.go`).
- **Relay-peer authentication** keyed from the dedicated `server.hub.conduit.peer_secret`: raw octets, one trailing newline trimmed, at least 32 octets with at least 16 distinct. Requests are HMAC-signed over timestamp and nonce. Hub-to-relay requests are bound to the receiving relay's instance and generation (`409 relay_target_mismatch`), and relay-to-hub nonces go to a replay store shared by the hub replicas (503 when it is unreachable).
- **Hub external relay mode** (`server.hub.conduit.relay_mode: external`): the hub runs no relay of its own. It needs the peer secret (different from the session secret), the internal listener, the hub.conduit experiment and a shared replay store, and checks each at startup.
- **Experiment follow-through**: the relay reads hub.conduit at startup (with retries until a deadline), on a poll timer and on settings events. Off stops admission and marks the relay draining.
- **Env hygiene**: both peer secret spellings are on the server secret scrub list. `--role relay` removes the server secret names from its env without reading them. A test checks that the peer secret never reaches an agent's environment.
- **Renewal across relays**: a renewal for a target on another relay goes over the bridged stream (`conduit.StreamRenewer`), and the owner relay forwards it to the target stream.

## Key files

| Area | Files |
|------|-------|
| Relay process | `cmd/server_relay.go`, `pkg/conduit/relayrole/relayserver/` |
| Relay database access | `pkg/store/relaydb/` |
| Peer auth | `pkg/conduit/relay/conduit_peerauth.go`, `pkg/hub/conduit_hub_internal.go` |
| Hub external mode | `cmd/server_conduit.go`, `pkg/hub/conduit_relay.go` |
| Settings | `pkg/config/hub_conduit_config.go`, `pkg/config/relay_server_config.go`, `pkg/config/settings_v1.go` (env alias table) |
| Env scrub | `cmd/server_secret_env.go` |

## Testing

- Package windows on a shared build host ran the light relay packages, the relay-role cmd tests, every pkg/hub shard holding the new tests, and mutation checks. Each mutation is applied in a private clone, and the named test must fail.
- The Postgres-backed tests (cross-replica replay, the conduit-channel listener, the registry epoch upserts) skip without a Postgres DSN. They run in a separate Postgres pass.
- Fake-clock tests target a timer by its own deadline (`clock.Fake.WaitForTimer`), and every channel wait in the relay server tests is bounded.

## Notes and follow-ups

- The relay does not persist Hello capability features in its registry rows; renewal-capability handling for targets stays with the hub (later phase).
- Rotation with a current and a previous peer secret is a production gate, not part of this phase.
- In production a separate relay needs a dedicated database role with table rights only on the registry tables and the settings it reads.
