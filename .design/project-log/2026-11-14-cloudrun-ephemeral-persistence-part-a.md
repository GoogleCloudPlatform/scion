# Project Log: Ephemeral persistence, part A (contract and control plane)

**Date:** 2026-11-14
**Task:** ptone/scion#7953 part A (phase P1 of ptone/scion#7780).

## Changes
- `api.Persistence` (`persistent` | `ephemeral`, empty means persistent), `ScionConfig.persistence`, `StartOptions.Persistence`, `RunConfig.Persistence`.
- Experiment `hub.cloudrun_ephemeral_agents` (server layer, alpha, off by default), forwarded in dispatch.
- Hub: `resolveAgentPersistence` at create. It takes the agent value, then the template value (not wired until P4), else persistent. It refuses ephemeral with `412 experiment_disabled` while the gate is off, and with `422 persistence_unsupported` on a broker without `ephemeralPersistence`. Both messages name the source. The stamp is written to the annotations `scion.dev/persistence` and `scion.dev/persistence-source`, kept fixed across PATCH, and sent on create, start and restart.
- Broker: wire field `persistence`, refused with 412 unless the forwarded gate is on; non-resume harness launch; workspace placement `local`.
- Capability `ephemeralPersistence` and the runtime interface `EphemeralPersistenceRuntime`. No runtime implements it yet, so `CheckPersistenceSupported` refuses ephemeral everywhere, including Cloud Run, until part B.

## Findings
- Q3: the hub sees a hub-stored template's config at create (`templateJobConfig`, the same path `message_mode` uses). It does not see a template that only the broker resolves from its local path.
- Agent annotations are replaced as a whole by PATCH, so the stamp needs explicit protection (`keepPersistenceStamp`).
- A golden of the persistent Cloud Run instance, generated from the base commit, pins byte-identical behaviour.
