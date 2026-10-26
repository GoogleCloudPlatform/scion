# Windows CLI support

**Date:** 2026-10-23

## Summary

The scion CLI builds and runs natively on Windows (amd64 and arm64) as a hub
client. The server, the Runtime Broker and agent-side commands stay unix-only.

## Scope: a client-only command allowlist

`cmd/platform_gate.go` lists the commands the Windows build runs: list, ssh,
ssh-config, tunnel, vscode, messages and conversations, use / project use,
config, and the hub client commands. Every other command, including any added
later, is hidden on Windows and refused from the root pre-run hook with a
"not supported on Windows" error, before it does any work of its own. The gate
is a no-op on other platforms. Adding a command to the Windows set means
checking that every local file it writes goes through the private-file
primitives below.

## Private files

`pkg/util/privatefs` is the one primitive for files that must be private to
the user.

- **Created with their ACL.** On Windows a private file or directory is
  created with an owner-only security descriptor: a protected DACL granting
  only the user and SYSTEM, with the owner set explicitly to the user. It is
  never created first and tightened afterwards.
- **Verified on the handle.** Existing files are checked on the open handle:
  owner, DACL entries (Administrators are accepted on verify) and link count.
  A file opened for writing must have exactly one name.
- **Trees walked by handle.** A private tree is opened from an anchor, one
  component at a time relative to the parent's handle. By default any
  reparse point (symlink or junction) at any component is refused.
- **Verify or repair.** An owned file that is too open and is opened for
  writing gets the private DACL on the same handle before anything is
  written; a too-open file owned by someone else is refused.
- **~/.ssh.** Dotfile managers link ~/.ssh, so that tree follows links
  (`TreeOptions.FollowLinks`) and decides trust on handles after following:
  the target's parent and an existing target must not be writable or owned
  by another principal, and the old descriptor is never copied, because a
  writable ssh config is code execution via ProxyCommand.
- **On unix** the same API keeps the mode-based behaviour (0600/0700,
  O_NOFOLLOW leaves), so callers carry no per-OS security code.

The client-reachable private files use it: hub credentials and hub profiles,
CLI settings and context files, the broker credentials read, the tunnel status
records, the GitHub resolution cache, the ssh include, config and known-hosts
files, the VS Code files, and the hub test-identity token output. On Windows a
credentials file another principal can access is refused on read, with a
notice on stderr that says how to resolve it; logging in over such a file warns
that other hubs' logins in it are not kept.

Files that stay unprotected on Windows are listed, with the reason, in
`hack/winsec-unprotected.txt`; all of them are server, broker or
agent-provisioning paths that the command gate refuses.

## Paths and locks

- `pkg/util/pathcanon` gives one canonical form of a path and the only
  containment check (`Within`). On Windows it resolves 8.3 short names, case,
  junctions, symlinks, `\\?\` and UNC prefixes, and trailing dots and spaces,
  and refuses alternate data streams and device names. Hub workspace
  detection in `pkg/config` uses it on Windows and treats any path it cannot
  resolve as a hub workspace.
- `pkg/util/filelock` has a Windows `Acquire` (LockFileEx on a handle that
  refuses links and directories and does not share delete access) and a
  handle-level `LockFile` for the tunnel store.

## ssh and VS Code

- The Windows ProxyCommand form accepts only words from a conservative
  character allowlist (double-quoted when they contain a space or `( ) ~ [ ]`);
  anything else is an error naming the word. Hub endpoints written into ssh
  and VS Code host entries are held to a plain URL character set.
- A VS Code CLI that is a `.cmd` or `.bat` file is started only when every
  argument passes a similar allowlist, because cmd.exe interprets its
  arguments.

## Portability

Unix-only code is split into `_unix` files with `_other` counterparts. Where an
operation needs a unix facility, the counterpart fails with
`errors.ErrUnsupported`; where a platform has no such concept (numeric owners,
process groups, reaping), it reports none or does nothing.

## CI

- `ci.yml` cross-builds `./cmd/scion` for windows/amd64 and windows/arm64 in
  its own job (plus a GOOS=windows vet of the core packages). Build & Test
  runs `hack/winsec-guard.sh`, which flags Chmod used as protection,
  string-based containment and stubbed privacy checks in Windows-reachable
  files.
- `windows.yml` runs on windows-latest. It first proves the runner can create
  symlinks and junctions, generates 8.3 names and has a FAT32 volume, then
  builds, vets and tests the packages in `.github/windows-test-packages.txt`
  with zero skips allowed. pkg/hub is never on that list.

## Open items

- Windows coverage of the full `cmd` and `pkg/config` test suites (they are
  POSIX-bound today; the Windows job runs anchored subsets).
- Release packaging of the Windows binaries.
