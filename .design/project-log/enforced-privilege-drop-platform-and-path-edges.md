# Project Log: Enforced privilege drop — host-path validation, platform and path edges

**Date:** 2026-10-02

## Overview

Behaviour of the opt-in enforced privilege-drop mode (`RequirePrivilegeDrop`)
alongside host-path validation for recursive chowns, plus three edge cases in
the enforced hook runner and the trusted-ancestor directory walk.

## Recursive chown: validation, then the no-follow walk

`chownTreeRootOwned` (sciontool init's post-pre-start ownership fixup) and
`supervisor.chownRecursive` run both checks in enforced mode, in this order:

1. `fsutil.CheckRoot` refuses a root that is a known critical system path or
   looks like a filesystem root.
2. `checkMountSource` (`fsutil.CheckMountSource` behind a test seam) refuses
   a root that is a mount point whose bind source is a critical system
   directory.
3. `dirfd.ChownTreeNoFollow` walks the tree fd-relative with
   `openat(O_NOFOLLOW)` and chowns each entry through its own fd, with the
   hard-link guard on. It never follows a symlink and never re-resolves a
   full path. Per-entry failures are returned joined, not only logged.

Without enforcement, `chownTreeRootOwned` keeps the historical path-based
`filepath.WalkDir` + `os.Lchown` walk.

The direct `/etc/passwd` fallback's home-directory chown does not get the
host-path checks. It changes one level only, anchored on the opened home
directory fd, and does not follow symlinks. Because that chown is
single-level and fd-anchored, it cannot be redirected or escape the opened
directory. The broader exposure — a home that is itself a bind mount onto a
sensitive host path — is not specific to this fallback: the earlier
usermod-based path chowns the same home recursively and is equally unchecked
here, and this change neither introduces nor widens it. A home that resolves
to a disallowed source is refused on the host before it ever becomes a
mount, so that precondition never reaches this code.

## Enforced hooks run by fd only on Linux

`execViaFd` runs a verified hook script through `/proc/self/fd/3`, which only
Linux provides. On any other platform it returns an
`*UnsupportedPlatformError` instead of a command, and the enforced hook fails.
It does not fall back to `/dev/fd` or to running the script by path. Enforced
hooks only run inside Linux agent containers, so a fallback would be exec code
that is never run in production or tested, in the one place that must run
exactly the file that was checked. Tests that run a script through `execViaFd`
skip on non-Linux. `TestExecViaFd_RefusesNonLinux` overrides the platform seam
(`execViaFdGOOS`) to check the refusal on a Linux host.

## Enforced hooks directory compared by cleaned path

In enforced mode, a refused hook entry (a symlink or non-regular file) is
skipped so it cannot block the event's other hooks. The exception is the
enforced hooks directory, where a refused entry still fails the event.
`skipRefusedEntry` now applies `filepath.Clean` to both directory paths before
comparing them. Otherwise a trailing slash on either path made that directory
look like a different one, and its entries were skipped instead of failing.
The hard-fail test is a table with trailing-slash rows for both paths.

## Trusted-ancestor walk resolves "/"

`dirfd.EnsureDirTrustedAncestorFollow("/")` returns an fd for the root
directory instead of refusing. This lets a staged file secret target directly
under the root (for example `/token`) be written again, as it could be before
`os.MkdirAll` was replaced. `"/"` cannot be a symlink, so there is nothing to
check. A trusted symlink whose target is `"/"` is a separate case and is still
refused as an empty target. A test checks that the returned fd's device and
inode match `"/"`.
