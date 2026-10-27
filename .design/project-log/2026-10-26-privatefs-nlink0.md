# privatefs: link-count check on a file replaced while it is checked

**Date:** 2026-10-26
**Branch:** win-cli/privatefs-nlink0

## Problem

`TestUpdateVSCodeFiles_ConcurrentWritersKeepAllEntries` (cmd/vscode_test.go)
failed intermittently with `refusing .../settings.json: it has 0 names: file
has more than one name (hard link)`.

`updateVSCodeFiles` runs `preflightSSHWrites` (`privatefs.CheckReplaceTarget`)
before it takes the vscode directory lock. A concurrent run that holds the
lock can rename a new `settings.json` over the path between the
`replaceTarget` open and its `fstat` (pkg/util/privatefs/replace_unix.go).
The open handle is then on the unlinked old inode, so `Nlink == 0`, and
`checkSingleLinkInfo` (which refused any `Nlink != 1`) reported it as a hard
link.

## Solution

- `checkSingleLinkInfo` (unix) and `checkSingleLink` (Windows) report a zero
  link count as the internal `errNoNames`, separately from `ErrHardLinked`.
- `retryReplaced` (privatefs.go) runs the open plus the check again while it
  fails with `errNoNames`, at most `replacedAttempts` (5) times in all. A file
  that keeps changing is refused with the new `ErrReplacedConcurrently`.
- A count above one is still refused with `ErrHardLinked` at once, with no
  retry.
- Sites: unix `replaceTarget` (re-resolves a symlinked leaf on each attempt);
  on Windows, `Tree.Create` (existing file), `Tree.OpenPrivate` and
  `Tree.checkReplaceTarget`. On Windows the handles share delete
  (`shareAll`), and the package's own rename and delete use POSIX semantics,
  so the name can leave an open handle there too (`NumberOfLinks == 0`).
- `testHookAfterLinkCheckOpen` (nil outside tests) runs between the open and
  the link-count read, so tests can replace or remove the file at that
  moment.

## Tests

- `replace_nlink_unix_test.go`: replaced once, then success; removed, then
  treated as a new file; replaced on every attempt, then
  `ErrReplacedConcurrently` after 5 attempts; a hard link is refused at once.
  The first three fail without the retry, with the CI error text.
- `replace_nlink_windows_test.go`: the same cases for `CheckReplaceTarget`,
  `Tree.Create` and `Tree.OpenPrivate`. Skipped where the volume has no
  POSIX-semantics rename.

## Out of scope

`TestInboxWriter_ConcurrentSameMessage` (pkg/runtimebroker/inbox_test.go)
fails differently: a `.tmp-*` directory ends up nested in the final message
directory. pkg/runtimebroker does not use privatefs. It is tracked
separately.

## Review round

- Windows tests: the replacing writer's error is kept. The test skips only
  when the classic rename refuses to replace an open file
  (`ERROR_ACCESS_DENIED`, `ERROR_NOT_SUPPORTED` or `ERROR_INVALID_PARAMETER`
  from the rename), and the skip message shows the error. Any other error
  fails the test. The Windows CI job fails on any skip, so these tests must
  PASS there.
- Added a Windows `Tree.Create(Private)` replaced-once test (the
  `verifyOrRepair` path inside `Create`).
- The unix replaced-once test passes a `newMode` (0644) that differs from
  the replacement's mode, so it shows the mode comes from the file that was
  checked again.
- `OpenPrivate` calls the test hook only for write opens, the only opens
  that check the link count.
