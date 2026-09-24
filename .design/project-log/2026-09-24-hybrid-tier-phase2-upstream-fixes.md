# Hybrid Deployment Tier — Phase 2 upstream-feedback fixes

Branch `scion/hybrid-tier-p2` (base for the stacked Phase 3 PR).

## Overview

A small batch responding to upstream review feedback and a wording follow-up on the
Phase 2 hardening entry's export-layout guidance.

## `readDirNames`: drop a redundant intermediate slice

`unix.ParseDirent` already accumulates matched entry names into whichever slice is
passed as its third argument. `readDirNames` (`pkg/shareddirs/delete_unix.go`) was
collecting into a separate `newNames` slice each iteration and then appending it onto
`names`; assigning directly (`_, _, names = unix.ParseDirent(buf[:n], -1, names)`)
drops the redundant allocation and append with no behavior change. The existing tests,
including the ENOENT-race path, are unaffected.

## Background NFS cleanup: evaluated, declined

A suggestion to move project-deletion's best-effort NFS shared-dir cleanup into a
background goroutine (detached from the request context) was evaluated against this
codebase's own precedent for background work. What exists here is either a persistent
worker loop started once at server startup and drained once at shutdown via a
`sync.WaitGroup` (several subsystems), or a bare, untracked goroutine with no shutdown
draining at all (the closest per-mutation analogue). Neither is a managed-lifecycle
precedent for a per-request one-off background task, so introducing an unmanaged
goroutine here was declined rather than adding a new pattern: it would detach the
cleanup from the request without any shutdown-drain, letting it race with process exit
mid-walk. The cleanup call stays synchronous, after the deletion transaction commits
and before the request returns; it remains best-effort (logged, never rolling back or
blocking the delete), depth-capped, and only runs when the NFS shared-dir backend is
configured.

## Export-layout guidance: wording pass

A follow-up pass on the paragraph explaining why the NFS export should be the root of
a filesystem rather than a subdirectory (added in the prior Phase 2 entry):

- States plainly, in that paragraph, that this tier's *default* export is a
  subdirectory on the boot disk and is affected by the gap being described, rather
  than only describing the general knfsd mechanism in the abstract.
- Corrects the `no_subtree_check` rationale to the actual reason it's the nfs-utils
  default (stable file handles across a rename, not "server restart" specifically).
- Reworded the affected-client description to "any host the export admits" (matching
  the network-shape guidance already in this document, rather than a narrower
  "node-level access" framing).
- Clarified precisely what knfsd does and doesn't check (inode-in-subtree, not just
  fsid), and noted the "removes this gap by construction" claim assumes nothing else
  is bind- or sub-mounted beneath the export with `crossmnt`/`nohide`.
- Added a matching row to the Boot-disk trade-offs table and a bullet to Known limits,
  so the trade-offs table's own framing no longer implies the export layout is a fully
  accepted trade-off with no fix.
- Added a concise, runnable recipe for exporting a dedicated filesystem root instead
  (create the backing image/disk and format it once, mount it before the NFS server
  starts, fail closed if it isn't actually mounted before exporting), with a
  subdirectory export called out as not recommended.

No code changed as part of this wording pass; the settings.yaml schema, the local
backend, and everything else described in the prior Phase 2 entry are unaffected.

## Verification

- `go build ./...`, `GOOS=darwin go build ./...`, `go vet ./...` — clean.
- `gofmt -l` on the changed file — clean.
- `golangci-lint run ./pkg/shareddirs/...` — 0 issues.
- `hack/check-project-compat-literals.sh` — rc 0.
- `go test ./pkg/shareddirs/...` — all pass, including the ENOENT-race test.
- `go test ./...` — full suite, run for this batch.
