# Project Log: Enforced-mode uid/gid parsing and staged-secrets symlink containment

**Date:** 2026-09-30

## Overview

Closes a fail-open uid/gid parsing gap that only applied outside enforced
mode, and a family of symlink-following writes and chowns in root-context
code (the staged-secrets writer, and the direct `/etc/passwd`/`/etc/group`
fallback's home-directory chown) that could be redirected by a workload-
plantable symlink.

## ValidWorkloadID

`ValidWorkloadID` takes a `refuseZero` parameter: `setupHostUser` passes
`requirePrivilegeDrop`, so a `SCION_HOST_UID`/`SCION_HOST_GID` of `0` is
refused only when privilege drop is required, and otherwise proceeds as
root — the behavior every non-enforced runtime already depends on. An
out-of-range or non-numeric value is refused in both modes, closing the
same uint32-overflow fail-open this parser already guarded against for the
enforced case.

`RunInit`'s own call onto `newLifecycleManager` and
`hub.EnforceTokenFileOwnerChecks` goes through two package-var seams,
`runNewLifecycleManager` and `runEnforceTokenFileOwnerChecks`, so a test can
assert the exact arguments crossing that call site — including that
`EnforceTokenFileOwnerChecks` receives `newLifecycleManager`'s own second
return value, not a value re-derived from `RequirePrivilegeDrop` directly.

`stagedsecrets.Write` parses `SCION_HOST_UID`/`SCION_HOST_GID` through
`rootexec.ValidWorkloadID` instead of its own bare `strconv.Atoi` call,
removing the second, less strict parse path for the same two values.

## Staged-secrets symlink containment

`stagedsecrets.Write`'s writes to a file secret's own `Target` and to
`secrets.json` go through `dirfd.WriteFileNoFollow`: a symlink planted at
either leaf path — left over from a previous run on a persisted home, or
planted ahead of a restart — is refused outright rather than written or
chowned through as root. `WriteFileNoFollow`'s own parent-directory walk is
symlink-safe component-by-component regardless of mode, so this applies
whether or not privilege drop is enforced.

The directory each leaf is about to be written into is created and (when it
resolves under the agent home) chowned the same symlink-safe way, via
`dirfd.EnsureDirNoFollowUnderRoot`: every component from the home directory
down is opened `O_DIRECTORY|O_NOFOLLOW`, a missing component is created with
`mkdirat` and only that newly created component is ever chowned (by fd,
never a path-based `os.Chown`), and a symlink or any non-directory at any
component below the home directory is refused before anything is touched.
A file secret whose target lives directly inside the agent home (its parent
directory IS the home directory itself, e.g. `~/.netrc`) is chown-eligible
exactly like one nested deeper.

The home directory's OWN chain — its ancestors, and its own final
component — is resolved differently, via `dirfd.EnsureDirTrustedAncestorFollow`:
a symlink there is followed, not refused, when it and its containing
directory are both owned by uid 0 with no group/other-write bit (a
system-configured alias such as `/home` -> `/var/home`, which no workload
can plant or redirect), and refused otherwise. The same walk resolves a
file secret's directory when it does NOT resolve under the agent home at
all (a legitimate operator-configured absolute target, e.g.
`/var/run/secrets/sa.json`): missing components are still created
(`mkdirat`, 0755) exactly as a plain `os.MkdirAll` would, but a symlink is
followed only under that same root-owned, non-group/other-writable rule —
an ordinary `EvalSymlinks` is deliberately not used here, since it would
also follow a workload-plantable ancestor (e.g. a target under a
workload-owned directory with a redirected component) and let root write
into or truncate a directory the workload chose instead of the operator.
Containment (whether a target resolves under the agent home) is decided by
walking the chain, not by a string-prefix check on the path, and is
unaffected by which ancestors turn out to be trusted symlinks.

An untrusted symlink anywhere in either chain — a link not owned by uid 0,
or sitting in a group/other-writable directory — is refused (fatal) in
both enforced and non-enforced mode, and nothing is created past the
refusal. Writing a secret to an unintended, workload-influenceable
location is worse than not starting.

A file-secret target may itself be a path the runtime bind-mounts into the
container (for example gcloud's `application_default_credentials.json`).
Because replacing a bind-mounted regular file's directory entry with a
freshly created one via `rename(2)` fails `EBUSY` — the mount cannot follow
the entry to a new inode — a file secret whose target already exists as a
regular file is rewritten in place (truncate and overwrite through the same
already-open parent directory descriptor) instead of being replaced
atomically. This trades away crash-atomicity for that one write in exchange
for working correctly against a bind-mounted target; every other writer
built on `dirfd.WriteFileNoFollow` keeps its existing atomic create-and-
rename behavior. The in-place path opens the leaf once, without truncating
it, fstats that same descriptor, and refuses — naming the link count —
anything that isn't a single-link regular file before truncating it: a
workload can plant a hard link to an unrelated file it does not own at the
leaf's own name, and a hard link has no symlink for `O_NOFOLLOW` to stop
at, so the link count is what closes that gap.

## Direct passwd/group fallback: home-directory chown

On runtimes where `usermod`'s recursive chown is too slow or unavailable
(Podman's fuse-overlayfs, or `SCION_ALT_USERMOD`), the direct `/etc/passwd`
and `/etc/group` edit path also chowns the scion user's home directory and
its immediate entries. That chown now happens entirely through an already-
open, already-verified `O_DIRECTORY|O_NOFOLLOW` descriptor for the home
directory itself, with each entry chowned by name via `AT_SYMLINK_NOFOLLOW`
— so a symlinked entry has its own directory-entry ownership changed, never
whatever it points at. If the home directory itself is not a plain,
non-symlink directory, enforced mode refuses the chown pass outright before
touching anything; unenforced mode logs and skips it, leaving the rest of
the direct-edit fallback (the `/etc/passwd`/`/etc/group` rewrite) unaffected.
`AT_SYMLINK_NOFOLLOW` alone does not stop a hard-linked entry, which names
the same inode as an unrelated file the workload need not own at all: under
enforcement, each entry is fstat'd first and skipped — logged, non-fatal —
when it has more than one link.
