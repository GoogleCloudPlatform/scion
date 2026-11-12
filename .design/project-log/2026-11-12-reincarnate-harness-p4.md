# Reincarnate --harness, Phase 4 (Kubernetes home reset)

**Date:** 2026-11-12
**Branch:** scion/reincarnate-harness-p4
**Issues:** ptone/scion#7761 (phase), ptone/scion#7757 (tracking)
**Updated:** 2026-11-13; P1-P3 are upstream.
**Upstream base:** bfdf9f1.

The landed start path already validates the persistent broker-side reset
record with bounded `privatefs.ReadPrivate` and supplies `RunConfig.HomeResetID`.
This phase contains only the sciontool reset and Kubernetes integration changes.

## Changes

- Kubernetes NFS-home pods pass `home prepare --reset-id` on every start
  after a reset. A different reset ID chooses seed with `CleanAll`, clears
  everything except the sentinel, then runs the existing skeleton, broker
  transfer and mark-seeded path. After marking seeded, repeated starts
  with the same reset ID use seed-over.
- Keep the agent identity check. Only a full reset skips the old UID check;
  removal failures return `home_seed_failed` and refuse the start. A failed
  cleanup retains the previous sentinel so a retry still resets the home.
- Sentinel version remains 1; optional `reset_id` is ignored by old readers.
  Images advertise an additional `home-reset-v1` feature; pending resets
  require it through the existing init-container feature probe. No release
  floor exists yet, so the manager approved replacing the design's `<ver>`
  placeholder with guidance to use a release with `home-reset-v1` support.
- Kubernetes local home needs no additional cleanup: `KubernetesRuntime.Run`
  creates a new pod and copies `RunConfig.HomeDir` into its home before
  releasing the startup gate. Phase 1 rebuilds that broker directory.
  NFS and local paths are now covered, so the broker advertises `kubernetes`
  in `ReprovisionResetHomeRuntimes` and the capability contract tests.

## Verification

The phase ran `make ci -o lint -o test-fast` successfully: formatting, custom
guards and Linux build passed. Full `make ci` uses unsharded Hub vet/test targets,
which AGENTS.md forbids; the manager approved substituting touched-package
vet/tests. `go vet -tags no_sqlite` passed for agent, runtime, runtimebroker,
homeprep and sciontool commands. The Windows `cmd/scion` build and
`hack/winsec-guard.sh` passed. Scoped `golangci-lint --new-from-rev=upstream-main
--concurrency=1` reported `0 issues.` across all touched packages. Final package
results and exact commands are recorded in the phase PR.

The Hub registration-consumer contract, `TestResetHomeRuntimeRefusal`, is
SQLite-only. It accepts advertised Kubernetes support and still refuses nil
or older capability lists. The full SQLite Hub shard 2 is included in final
verification; `no_sqlite` checks alone do not exercise that contract.

Unit coverage includes the
reset decision table, both filesystem resolvers, full cleanup and new skeleton,
symlink containment, UID change, agent mismatch, removal denial and retry,
sentinel compatibility, command flag, persistent ID propagation, exact feature
probing, and local/NFS pod construction.

Live Kubernetes NFS/local UAT is explicitly deferred by the task brief to the
whole-feature end-to-end phase. The issue's original live-UAT text is superseded
by that instruction. No live cluster was used.
