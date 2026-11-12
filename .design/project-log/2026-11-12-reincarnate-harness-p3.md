# Reincarnate --harness, Phase 3 (Cloud Run home leaf)

**Date:** 2026-11-12
**Branch:** scion/reincarnate-harness-p3
**Issues:** ptone/scion#7760 (phase), ptone/scion#7757 (tracking)

Cloud Run home provisioning compares `RunConfig.HomeResetID` with the leaf's
reserved `.scion-home-reset-id`. A different pending ID clears every top-level
entry through `agenthome.Home.RemoveAll` before seeding. Symlinks are removed
without following their targets. The marker is written only after seeding and
harness-material cleanup succeed, so a partial clear or failed seed is retried
on the next start. Matching IDs preserve new-harness state; no pending ID keeps
the previous seed-over behavior. Invalid marker content or type triggers an
extra reset without following links, while genuine I/O errors refuse the start.
The reserved marker is excluded from the
broker-home copy and is not part of the harness-material removal list.

The broker advertises `cloudrun` and `cloudrun-instances` in its reset-home
runtime capability, including registration names and heartbeat payloads.
Contract tests cover both names and the hub's refusal for old brokers.

## Stack and agreed implementation adjustment

This phase stacks on P1, ptone/scion PR 7770, at `d01b08b`. It must land after
P1. The first commit above P1 is the unchanged shared reader commit authored
by P4, `607c63be0113b71fe549bab562647c27162598b2` (ptone/scion#7761).
Whichever phase lands second drops that shared commit on rebase.

The manager approved reading and validating `home-reset.json` in `pkg/agent`
and passing `HomeResetID` through `RunConfig`, instead of reading the record
inside `provisionCloudRunHome` as design section 3.5.2 originally described.
`RunConfig` has no broker agent-directory path, split homes cannot reliably
supply it through `HomeDir`, and the runtime cannot import `pkg/agent`.

## Verification

The initial reproduction tests failed because old files survived and the
broker-home marker was copied onto the leaf. The heartbeat test initially
failed because both Cloud Run capability names were missing. The new tests
exercise real Cloud Run start/provisioning with a fake instance client and a
local leaf, plus a fake filesystem failure after a real partial clear.

Live Cloud Run UAT is deferred to the whole-feature end-to-end phase by ptone.
