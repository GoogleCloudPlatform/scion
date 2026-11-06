# Provisioning: a refused chown of an entry that already has the wanted owner is not a failure

**Date:** 2026-11-06
**Branch:** scion/provision-chown-owner-match-91e913b0
**Addresses:** ptone/scion#7447

## Problem

The Kubernetes provisioning init container runs as root and chowns to uid
1000 (the pod RunAsUser) and `nfs.gid`, with `RequireChownSuccess` set. On an
NFS export that maps root to an anonymous user (`root_squash` with
`anonuid=1000,anongid=<nfs.gid>`, or `all_squash`), root cannot chown, but
every entry is already owned by 1000:`nfs.gid`. The recursive tree walk
(`chownProjectTree`) and `PrepareStateDir` already accepted that case through
`checkChownEPERM`. Three single `lchown` calls did not, so the pod failed:

- the `chown` closure in `ProvisionAgentDir` (clone-per-agent and
  empty-per-agent agent directories);
- the non-recursive branch of the `chown` closure in `ensureMountedWorktree`
  (worktree-per-agent: `.git/config`, sharer markers, branch refs);
- `recreateEmptyDir` (recreating the agent's empty worktree directory after
  removing a stale git entry).

## Change

Each of the three now passes an `lchown` error through `checkChownEPERM`:
EPERM with a matching owner is success; EPERM with another owner, and every
other error, fails as before (the message gains the "owner is X:Y, want U:G"
detail). `recreateEmptyDir` checks against the owner it was given (the owner
observed on the removed directory) and calls the stubbable `lchownFile`
instead of `os.Lchown`; its mode step still runs.

Log level: with `RequireChownSuccess=false`, an EPERM on an entry that already
has the wanted owner used to log the Warn "chown failed (continuing)"; it is
now success and logs only `checkChownEPERM`'s Debug line. EPERM with another
owner and other errors still log the Warn.

No pod-spec change: the init container still chowns to uid 1000; `nfs.uid`
unset or 1000 is unchanged.

## Tests

`pkg/provision/chown_owner_match_test.go`: EPERM with owner match succeeds,
EPERM with owner mismatch fails, non-EPERM fails, for `ProvisionAgentDir`
(sentinel written only on success), the mounted-worktree restart (only the
non-recursive chowns run) and `recreateEmptyDir`. The tests skip when run
with uid or gid 0, because 0 selects the default owner 1000.

## Docs

`reference/server-config.md` (Kubernetes init container paragraph) and
`hosted/ha/kubernetes.md` (NFS workspace export requirements): a root-squash
export must map root to `anonuid=1000` and `anongid=<nfs.gid>`; the default
`anonuid=65534` does not work.
