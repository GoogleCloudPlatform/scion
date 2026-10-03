// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package registry is the Conduit session registry (design conduit v2.1
// §3.4, §3.9): the single source of truth for which relay holds which
// principal's session.
//
// It owns the registry *logic* — eligibility filtering and ranking,
// admission fencing, generation-CAS, the stale-relay reaper body and the
// relay self-check — and defines a [Store] interface for persistence. The
// production Store lives in pkg/store/entadapter (ConduitRegistryStore),
// backed by three ent tables that are identical in shape on Postgres and
// SQLite: relay_instances, conduit_sessions and conduit_principal_epochs.
//
// The package deliberately imports nothing from pkg/conduit or
// proto/conduit: principal kinds, transports and stream kinds are the
// canonical lowercase strings of the Phase 1 shared contract (§2).
//
// # Load-bearing invariants
//
//   - Deletes are CAS on (session_id, relay_instance_id, relay_generation):
//     a relay only deletes rows it created in its current generation
//     ([Registry.DeleteSessionCAS]); on startup it sweeps rows of its own
//     older generations ([Registry.SweepOwnOlderGenerations]).
//   - Generations are assigned by one atomic upsert. Re-registering an
//     existing instance_id gets stored+1: database-derived and clock-immune,
//     strictly increasing even if the clock goes backwards. A new row (first
//     registration, or after [Registry.PruneRelayInstances]) is seeded with
//     the Registry clock's Unix milliseconds, so generations stay monotonic
//     across a prune provided clocks are not wrong by more than the prune
//     horizon. The reaper keeps relay_instances rows so that live
//     re-registrations never depend on the clock; only PruneRelayInstances
//     removes rows that have had no sessions and no heartbeat for a long
//     horizon (default 7 days).
//   - connection_epoch comes from a durable per-principal counter
//     (conduit_principal_epochs) bumped by upsert … RETURNING in the same
//     transaction as the session insert. It is never derived from existing
//     session rows and never regresses.
//   - Routing filters before it ranks ([Registry.Eligible]): live ∧ ¬draining
//     ∧ project ∧ exec_scope ∧ current incarnation ∧ capability ∧ current
//     epoch; only then freshest-first. exec_scope matching is exact; only
//     an explicit [Want.AnyExecScope] (stateless operations) disables it.
//     Sessions on a draining relay stay eligible but rank last; draining
//     sessions are excluded.
//   - Admission is fenced ([Registry.Admission], [Registry.IsAdmissible]) and
//     fails closed: if authoritative state cannot be read, the result is
//     "not admissible" together with the error.
//
// # Epoch currency by principal kind (§3.4)
//
//   - agent: single-session; current iff epoch == the principal's counter.
//   - broker: multi-session (G9); current iff no newer session exists for the
//     same (broker_id, relay_instance_id).
//   - user: never resolved by principal; currency does not apply (epoch is
//     audit-only) and user sessions are never returned by Eligible.
//   - relay-peer: never has a conduit_sessions row; inserts are rejected.
//
// # Fault injection
//
// [Config.Clock] makes time injectable and [FaultStore] wraps any Store with
// a per-operation hook, so tests (including the 1v multi-node validation)
// can simulate stale relays, clock skew and read/write failures.
package registry
