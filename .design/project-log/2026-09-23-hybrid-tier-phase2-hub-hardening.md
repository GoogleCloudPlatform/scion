# Hybrid Deployment Tier — Phase 2: hub-side + hardening

Branch `scion/hybrid-tier-p2`, based on `main` at `cb7eca30`. Fork PR on
`ptone/scion`. Design: `design.md` v1.4 (APPROVED), §3.2.5, §3.5–3.7, §5.1, §7 Phase 2.

## Overview

When `server.shared_dir_storage` is configured with `backend: nfs`, a project's shared
directories live on an NFS export instead of the local project-configs layout, addressed by
hub project ID under `<mount_root>/<share_id>/<subpath_root>/<project_id>/shared-dirs/<name>`.
This is opt-in and additive: when the setting is unset or `local`, behavior is unchanged.

Both the runtime broker (creating shared dirs for agents it starts) and the hub's own file
browser (project pages in the web UI, and the shared-dir REST API) resolve this same layout
through a shared package, `pkg/shareddirs`, so a shared dir either of them causes to exist gets
identical treatment.

## `pkg/shareddirs`: leaf creation, safe opening, and deletion

**`EnsureLeaf(hostBase, rel)`** is the single entry point used to create a shared dir's
directory chain (`<subpath_root>/<project_id>/shared-dirs/<name>`). It walks every path
component with `O_NOFOLLOW`, refusing a symlink at any point — leaf or intermediate — before
creating or touching anything past it. Every intermediate component is created at mode `2755`
(setgid, no group/other write); a leaf this call creates gets mode `2775` plus a minimal POSIX
default ACL (`u::rwx,g::rwx,o::r-x`) on its own owning group, so a file later created with a
permissive mode comes out group-writable regardless of the creating process's umask. A
pre-existing leaf is left untouched — its mode and ACL are never modified. If the leaf's chmod
or ACL step fails after `EnsureLeaf` itself created it, the leaf is removed before returning the
error, so a retry starts clean instead of getting stuck half-finalized. A pre-existing
intermediate that is group/other-writable, or not owned by the calling process, logs one
process-wide warning pointing at the manual fix-up recipe in `docs/deploy/hybrid-tier.md`; it is
never fixed automatically.

**`OpenAnchoredRoot(hostBase, rel)`** opens an `*os.Root` on a shared dir's leaf, anchored to the
directory identity (device + inode) an independent, symlink-safe, open-only walk just observed
there. This closes the gap between an earlier path resolution and the later open: a structural
component (the project-id directory, or `shared-dirs/` itself) replaced with a symlink between
those two points is detected — the freshly observed identity won't match — and refused, never
silently followed. A missing leaf reports an `fs.ErrNotExist`-compatible error and creates
nothing; a symlinked component reports a different, non-`ErrNotExist` error, so a caller can't
mistake a refusal for "nothing here yet".

**`DeleteProjectTree(hostBase, subPathRoot, projectID)`** removes a whole project's
`shared-dirs` tree via a symlink-safe, fd-based `openat`/`unlinkat` walk. It tolerates
concurrent or repeated deletion (another actor racing the same
cleanup) and caps recursion depth; a subtree past the cap is logged and left in place rather
than aborting the whole call, so its siblings are still removed. **`DeleteSharedDir(hostBase,
subPathRoot, projectID, name)`** removes one named shared dir's leaf the same way, without
touching its siblings. Both refuse outright, naming the offending component, if any structural
component — `subPathRoot` (including a multi-component value's own intermediates), the project
id, `shared-dirs`, or the target itself — is not a plain directory; a symlink found as leaf
*content* is unlinked directly, never followed. A missing host base is an error (an unmounted
export must not look like a successful delete); a missing `subPathRoot`, project id, or leaf
under an existing host base is a no-op.

## `pkg/agent`: broker provisioning

`resolveSharedDirs` resolves the NFS layout for a project's declared shared dirs, confines every
resolved path to the project's own subtree, and creates the chain via `EnsureLeaf` when the NFS
host base exists locally. A missing host base fails closed, naming the export path, for every
runtime this tier supports — it is never auto-created, and a Kubernetes `PersistentVolume`'s own
`subPath` is never allowed to create it either.

## `pkg/hub`: file browser and shared-dir API

Shared-dir list, download, archive, upload, write and delete all resolve the NFS layout first
when configured, independent of whether the project has a co-located broker, and open the leaf
via `OpenAnchoredRoot`. A missing leaf is created on first write via `EnsureLeaf`; a read or
delete on one that doesn't exist yet reports empty/not-found and creates nothing.

Removing a shared directory from a project's configuration (`DELETE
/projects/{id}/shared-dirs/{name}`) removes that directory, and all of its contents, from the
export via `DeleteSharedDir`. Deleting a project removes its whole `shared-dirs` tree from the
export via `DeleteProjectTree`, best-effort, after the database transaction commits — this is
part of `ptone/scion#1802`; local-backend and workspace-directory cleanup are unaffected and
remain tracked on that issue.

Chat attachment ingest and staging are not available for NFS-backed shared dirs in this phase:
resolution returns unavailable immediately, with no fallback to a directory that might exist
from before a project moved to NFS.

A global settings file that fails to load, or loads in the legacy pre-`schema_version` format,
is treated as "`shared_dir_storage` may be configured" whenever its raw bytes mention the key,
rather than falling back to the local layout. The hub and broker log a one-time startup warning
for any workspace-storage-only NFS field (`uid`, `gid`, `mount_options`, `storage_class`) set
alongside `shared_dir_storage`, and one summary line of the resolved layout, once per process. A
409 response for a resolution failure never includes the resolved host path or the underlying
settings error; that detail is logged server-side, and the client gets a fixed message.

## Tests

- `pkg/shareddirs`: `EnsureLeaf`, `OpenAnchoredRoot`, `DeleteProjectTree` and `DeleteSharedDir`
  each have direct unit coverage for the happy path, a missing host base/leaf, a symlinked
  structural component (single- and multi-component `subPathRoot`, and the leaf itself), a
  symlink as leaf content, concurrent/repeated deletion, the depth cap, and finalization
  failure/rollback (via package-level seams that let a test inject a chmod/ACL error without a
  real filesystem condition producing one).
- `pkg/agent`: `resolveSharedDirs` is covered for the happy path, a missing host base on every
  supported runtime, path-confinement against traversal-shaped names/IDs, symlinked structural
  components, and leaf modes/ACL assertions (a new leaf vs. a pre-existing one, including when the
  same call also requests an already-existing sibling).
- `pkg/hub`: the shared-dir HTTP handlers (list, download, archive, upload, write, delete, and
  the config-level add/remove endpoints) are covered end to end for the NFS layout, including: a
  symlink planted inside a leaf (escaping the export, or reaching a sibling project); a
  statically symlinked structural component with an existing leaf on the victim side, across
  every verb; a genuine `RENAME_EXCHANGE` race swapping a real structural directory with a
  symlink to a matching victim tree, verifying both that the victim is never read/written/removed
  and that the real leaf is still reachable when the race allows it; a missing global-settings
  file (and a legacy-format one) that mentions `shared_dir_storage`, verifying the local layout
  is never served as a fallback; attachment ingest/staging refusing an NFS-backed shared dir with
  no fallback to a stale local directory; and an `EnsureLeaf` finalization failure surfacing as a
  server error rather than being masked into a generic not-found response.

## Verifying this locally

1. `env -i PATH=$PATH HOME=$(mktemp -d) GOCACHE=$(go env GOCACHE) GOMODCACHE=$(go env GOMODCACHE) GOPATH=$(go env GOPATH) go build ./...`
2. `go test ./pkg/shareddirs/... ./pkg/agent/... ./pkg/hub/... ./pkg/config/... ./cmd/...`
3. `bash hack/check-project-compat-literals.sh`
4. Manually: configure `server.shared_dir_storage: nfs` per `docs/deploy/hybrid-tier.md`, declare
   a shared dir on a project, and confirm it appears under the configured export at
   `<subpath_root>/<project-id>/shared-dirs/<name>` with mode `2775` and the default ACL once an
   agent or the hub file browser first uses it.
