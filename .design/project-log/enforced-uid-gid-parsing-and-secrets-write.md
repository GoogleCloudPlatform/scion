# Project Log: Enforced-mode uid/gid parsing and staged-secrets symlink containment

**Date:** 2026-09-30

## Overview

Closes a fail-open uid/gid parsing gap that only applied outside enforced
mode, and a symlink-following write in the staged-secrets writer that ran
as root regardless of mode.

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
`secrets.json` go through `dirfd.WriteFileNoFollow` with the
`RefuseSymlink` leaf policy: a symlink planted at either path — left over
from a previous run on a persisted home, or planted ahead of a restart — is
refused outright rather than written or chowned through as root.
`WriteFileNoFollow`'s own parent-directory walk is symlink-safe
component-by-component regardless of mode, so this applies whether or not
privilege drop is enforced.
