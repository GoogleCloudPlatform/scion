# Project Log: Runtime broker error opacity, workspace git identity, and cross-platform build fixes

**Date:** 2026-09-30

## Overview

Closed a broker-side information-disclosure class, replaced the shared
workspace git-config path's private-staging-copy design with a direct,
identity-dropped write, fixed the CLI attach gate's handling of an
unreadable broker attach record, and fixed two remaining darwin build
breaks.

## Broker error opacity

- Added `runtimebroker.OpaqueError`: a typed error whose `Error()` returns a
  fixed, identity-free message regardless of what the wrapped runtime/
  bootstrap error carries (a container ID, node name, namespace, image
  reference), while `Unwrap()` keeps the original reachable for server-side
  telemetry and logging only.
- Every runtime-op handler in `pkg/runtimebroker/handlers.go` — start, stop,
  restart, delete, exec, message, logs, list — now builds its failure
  response through this type instead of concatenating the raw error's own
  text into the HTTP body.
- The runtime-level wrap points (`substrate_runtime.go`,
  `substrate_bootstrap.go`) are unaffected: they keep wrapping with
  sentinels for `errors.Is` callers; the redaction happens once, at the
  handler boundary that actually writes the HTTP response.

## Shared workspace git identity

- `configureSharedWorkspaceGit` runs its `git config --file` calls directly
  against the real gitconfig path under the workload's own uid/gid via
  `SysProcAttr.Credential`, the same mechanism `configureGitCommand` already
  used, instead of staging a private root-owned copy and installing it back
  over the real path. A symlinked `.gitconfig` (the layout a dotfile manager
  produces) is written through in place rather than replaced.
- Under `RequirePrivilegeDrop` with no usable uid/gid, it refuses outright
  with a sentinel error instead of running git as root.
- The one piece of the previous design kept unconditionally: a stat of the
  gitconfig leaf before ever touching it, so a planted FIFO with no writer
  is refused immediately rather than hanging `RunInit`.
- `resolvePrivateGitConfigDir`, only ever used by the staging copy this
  removes, is deleted, along with `dirfd.EnsureDirNoFollowRootOwned`,
  `dirfd.AmbientTempDirTrusted`, and `hooks.PrivateRootTmpDir`/
  `PrivateRootTmpDirMode`: none has any other caller. The substrate
  package's own private-tmp-dir constant is now its own, package-local
  copy rather than a reference to the deleted hooks-package export.
- `InitRunOptions.ForwardTermSignal` is renamed to
  `DisableTermSignalForwarding` with its polarity flipped, so a zero-value
  `InitRunOptions` matches `sciontool init`'s own CLI default instead of
  silently disabling signal forwarding for any other constructor.

## Attach gate

- An unreadable broker attach record (403, 404, absent from LIST, or a
  transient lookup error) now proceeds to dial rather than refusing
  up front — the broker's own pre-upgrade and post-upgrade gates are
  authoritative. A genuinely unsupported attach still surfaces the same
  fixed message and a non-zero exit on both the pre-upgrade 501 path and
  the post-upgrade 4501 close path, unified behind one shared wire
  constant so the two paths cannot drift to different wording.

## Cross-platform build

- `pkg/sciontool/dirfd` builds on darwin: the portable primitives use
  `golang.org/x/sys/unix` in place of Linux-only `syscall.Openat/Mkdirat/
  Renameat/Unlinkat`; the one genuinely Linux-only piece
  (`ChownTreeNoFollow`, which needs `O_PATH`/`AT_EMPTY_PATH`) is isolated
  behind a build tag with a typed, `errors.ErrUnsupported`-wrapped stub on
  every other platform.
- `cmd/sciontool/commands/substrate_rootfs.go` builds on darwin: normalizes
  a stat's mode field to `uint32` before combining it with other `uint32`
  values (the field's width is platform-dependent), and resolves the
  sudoers-grant cleanup through `golang.org/x/sys/unix.Unlinkat`, which is
  available on both platforms `sciontool` builds for.

## Exec-guard maintenance

`pkg/sciontool/rootexec/guard_test.go`'s `literalIsPathLike` now requires an
absolute path (`filepath.IsAbs`) rather than merely checking for a slash, so
a relative command name such as `bin/sh` is still flagged as unresolved
instead of being treated as already path-like — `os/exec` still resolves a
relative name against the process's current working directory, which a
workload can influence exactly as it could an inherited `PATH` entry.
