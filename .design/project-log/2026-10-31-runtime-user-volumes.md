# `volumes:` entries realized through Layout.Realize

**Date**: 2026-10-31
**Design**: storage design §P5 and OQ-R2 (a `volumes:` entry is always `Scope=agent`, `Owner=user`)

## Summary

A `volumes:` entry is now a `projectfs.Mount` (`KindVolume`, `Scope=agent`,
`Owner=user`, `Provision=none`, named by its target, `UserSource` = the host
path or, for gcs, the bucket). It goes through `Layout.Resolve` and
`Layout.Realize` like the workspace and shared dirs, via one runtime helper,
`realizeUserVolume` (`pkg/runtime/user_volumes.go`).

- Docker, Podman and Apple: the `-v src:tgt[:ro]` of a non-gcs entry comes
  from `Realize(RuntimeBind)`. The step sits after the configured-volume
  source checks (`checkConfiguredVolumeSource`, `checkVolumeSource`) and
  before `registerMount`; every bind is still checked when it is emitted
  (`checkBindSource`).
- Kubernetes: a gcs entry's `MountPath` and `ReadOnly` come from
  `Realize(RuntimePod)`, which returns no claim. The CSI volume stays the
  runtime's.

This merges code paths only. Behaviour does not change.

## Decisions

- **Resolve and Realize never refuse a volume.** Before this change the
  runtimes passed an entry through as written: an empty source or target, or
  any type other than `gcs` treated as a bind. A refusal in projectfs would
  therefore be new behaviour. Entry checks stay in `api.VolumeMount.Validate`
  (template load) and in the runtime's source checks. The guard projectfs
  does add is that a `KindVolume` Mount must be user-owned and agent-scoped.
- **gcsfuse on bind runtimes is left as it was.** It is an in-container
  mount command, not a bind mount, and replacing GCS-FUSE is a design
  non-goal. On k8s, local and nfs entries are still skipped with a warning.
- **The golden testdata is captured at the base commit** (a detached
  worktree at the base, with only the new test files copied in and the
  `-update-*` flags run there). It then passes unchanged at the new head.
  That is the proof of no behaviour change.

## Gotchas

- `TestResolveRejects/unknown_kind` used `"volume"` as its example of an
  unknown kind. Adding a kind can break that sort of test. It now uses
  `"other"`.
- The run-argv golden test pins `HOME`, `projectConfigsDirFunc`,
  `SCION_DEV_BINARIES` and the force-host-network env, and replaces the
  host uid and gid values with a placeholder, so the testdata is the same on
  every machine. `-update-volume-args-golden` rewrites it.
- The argv includes the `scion.bind_sources` label (a base64 record of the
  emitted binds), so the goldens change whenever a bind source does.
- A source of `~` is the host home, which holds the Scion global
  directory; the bind-source checks refuse it. The `refused` case pins that
  refusal's text before and after.

## Follow-ups (not done)

- Docker gcs volumes and the k8s `/workspace` volume fallback in `Run` read
  `api.VolumeMount` directly. They could take the Mount too if GCS-FUSE ever
  becomes a Realize runtime kind.
