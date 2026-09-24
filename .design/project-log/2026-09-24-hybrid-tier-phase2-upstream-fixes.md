# Hybrid Deployment Tier — Phase 2: readDirNames simplification and export-layout guidance

Branch `scion/hybrid-tier-p2`.

## Overview

A small batch: a `readDirNames` simplification, and a further pass on the export-layout
guidance added in the prior Phase 2 entry.

## `readDirNames`: drop a redundant intermediate slice

`unix.ParseDirent` already accumulates matched entry names into whichever slice is
passed as its third argument. `readDirNames` (`pkg/shareddirs/delete_unix.go`) was
collecting into a separate `newNames` slice each iteration and then appending it onto
`names`; assigning directly (`_, _, names = unix.ParseDirent(buf[:n], -1, names)`)
drops the redundant allocation and append with no behavior change. Covered by the
existing tests (including the ENOENT-race path) plus a new multi-buffer test (~1000
entries, forcing more than one `ReadDirent` call) that exercises the cross-iteration
accumulation this change touches.

Project-deletion's NFS shared-dir cleanup remains synchronous, after the delete
transaction commits and before the request returns: best-effort (logged, never rolling
back or blocking the delete), depth-capped, and only runs when the NFS shared-dir
backend is configured.

## Export-layout guidance: a further pass

Continues the paragraph explaining why the NFS export should be the root of a
filesystem rather than a subdirectory:

- Fixes an antecedent and a caveat that inverted its own meaning (a sub-mount exposed
  with `crossmnt`/`nohide` extends the gap, not the guarantee).
- Qualifies the Boot-disk trade-offs table's "no mount boundary" row to the default
  (subdirectory) layout specifically, since the recommended layout adds a mount on
  purpose.
- Replaces the export-layout recipe with a concrete, runnable command block: create and
  format the backing image once (refusing to re-create one that already exists), an
  fstab entry combining `x-systemd.before=nfs-server.service` (ordering) with
  `x-systemd.required-by=nfs-server.service` (an actual dependency, so nfs-server does
  not start if the mount fails) plus a `mountpoint -q` check, and the exports(5) `mp`
  option on the export line itself (the export-side half of the same fail-closed
  guarantee, since it also covers a mount that fails on a later reboot, not just at
  initial provisioning).
- Adds the ownership/mode step the recipe was missing: the freshly formatted
  filesystem's root needs the broker's own uid, group `scion`, mode `2755`, matching
  the E2 hardening section below it.
- Adds a short cutover procedure for an existing deployment (stop the hub and agents,
  copy the existing tree with ACLs preserved, apply the same ownership/mode, re-export,
  restart, remount clients) rather than presenting the recipe as a live, in-place
  change.

## Verification

- `go build ./...`, `GOOS=darwin go build ./...`, `go vet ./...` — clean.
- `gofmt -l` on the changed file — clean.
- `golangci-lint run ./pkg/shareddirs/...` — 0 issues.
- `hack/check-project-compat-literals.sh` — rc 0.
- `go test ./pkg/shareddirs/...`, including the new multi-buffer test — all pass.
- `go test ./...` — all pass except 4 pre-existing `pkg/hub` failures, confirmed
  identical at the base commit and unrelated to this change.
