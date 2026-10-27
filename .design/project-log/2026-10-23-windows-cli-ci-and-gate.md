# Windows CLI: CI package list, gated-command hardening, follow-ups

**Date:** 2026-10-23

## Summary

The windows-latest job now runs every package whose tests have no
unix-specific test code, the Windows command gate also covers persistent
hooks and shell completion, and a set of small code-quality follow-ups to the
Windows CLI change landed.

## Windows job package list

`.github/windows-test-packages.txt` lists, besides the anchored packages
(`./cmd`, `./pkg/config`, `./pkg/agent`, `./pkg/brokercredentials`,
`./pkg/artifacts`), the packages without unix-specific test code (no
unix-only syscalls, symlinks, mode assertions, Chmod, hard-coded unix paths
or shell execs in the tests the Windows build compiles). Each runs in full
with zero skips; each package left off has a one-line reason in the file.

- Rule stated in the header: never `./pkg/hub` (or a `./pkg/hub/...`
  wildcard) and never a package whose test binary links pkg/hub. `./cmd` is
  the one exception: it links pkg/hub production code only, as on Linux, and
  runs anchored tests only. Check a candidate with
  `GOOS=windows go list -test -deps <pkg>`.
- `pkg/util/otlptls`'s test accepts the `%q`-quoted path in error messages
  (a quoted Windows path doubles its backslashes).
- Left off after triage on windows-latest, each with its reason in the file:
  `pkg/projectfs/migrate` (every file is unix-only), `pkg/k8s` (its exec
  credential plugin test runs a shell script) and `pkg/sciontool/log` (the
  agent log is not opened on Windows by design).
- `pkg/util/urlredact`'s source-copy test normalises CRLF before locating its
  span, so it passes on the CRLF checkout of windows-latest.
- `pkg/preflight` stays off; its line names the two failing tests and what
  they report on windows-latest (taken from a run's test.json artifact).
- The job's artifact `windows-test-json` holds the full go test -json
  streams (test.json and each flagged line's flagged-N.json) for 7 days, so
  a failure or a timeout's goroutine dump can be read without a rerun (the
  console shows only the last 200 output lines). The job has no secrets, so
  the files hold only test names, results and test output.

## Gated commands: persistent hooks and completion

- `gateCommands` (cmd/platform_gate.go) also clears `PersistentPreRun(E)` and
  `PersistentPostRun(E)` on every gated command. Cobra runs only the nearest
  persistent pre-run hook, so a hook left on a gated command would replace
  root's, which returns the refusal. An allowed child of a gated parent
  (`project use` under `project`) falls back to root's hook and keeps
  working.
- `completeGated` answers cobra's shell completion request (`__complete`,
  `__completeNoDesc`) for a gated command, or for anything under a gated
  parent, with only the `ShellCompDirectiveNoFileComp` directive line, before
  cobra runs, so for any request cobra would treat as a completion request
  the command's `ValidArgsFunction` and the completion functions registered
  for its flags do not run. The request is recognised as cobra recognises
  it: with a temporary hidden placeholder named `__complete` on root,
  `root.Find` must resolve the arguments to a command of that name (matched
  by name, so an existing `__complete` command counts too), and global flags
  before `__complete` (`--debug`, `--project <name>`, `-g <name>`,
  `--project=<name>`) are covered. `Execute` calls it right after the
  platform restrictions; off Windows nothing is gated, so it never answers.
  The directive line is printed (and nothing else) because without it the
  completion scripts fall back to file completion.
- Known limitation: on Windows, `scion project <TAB>` offers nothing, so the
  allowed `project use` is not suggested by completion (it still runs, and
  `scion use` is the same command).
- PowerShell completion and stderr: cobra's PowerShell script (v1.10.2) runs
  the request as `Invoke-Expression -OutVariable out "$RequestComp" 2>&1 |
  Out-Null`, so stderr from `__complete` (for example the credentials
  refusal notice) is discarded, never shown and never parsed. Cobra itself
  writes "Completion ended with directive" to stderr on every request. No
  suppression is needed.

## Follow-ups

- The Windows ssh link test file is now `cmd/ssh_links_windows_test.go`;
  test and message names describe what they check.
- The free-floating note on `isUnderHubProjectsDir` in pkg/config/templates.go
  is part of `isHubWorkspaceRootWithoutScion`'s doc comment.
- The Windows `isUnderHubProjectsDir` logs at debug level when it cannot
  resolve a path (or the global directory) and therefore skips the project
  layer; the outcome is unchanged.
- `writeVSCodeSettings` had no production caller; its test writes through
  `writeVSCodeSettingsData` directly.
- The unix ssh link tests use one tree snapshot helper.

## Open items

- `pkg/plugin/grpcbroker` hit the 10-minute go test timeout once on
  windows-latest; the next runs passed. The goroutine dump (from that run's
  test.json artifact) shows
  `TestGoogleIDTokenValidator_AllCallersCanceledFetchStillStops` blocked on
  its unbounded `<-requestStarted` receive (tokenvalidator_test.go): no
  handler goroutine and no fetch goroutine were left, so the first JWKS fetch
  returned without reaching the test server and the test waited forever. The
  hang is in that test's wait, outside the Windows file-protection code; a
  bounded wait in the test is the follow-up.
- Local gate tree checks for this branch run git with every GIT_* variable
  unset and no global or system config, from `/`, and stop on any
  repository-local config key outside a short allowlist and on
  skip-worktree or assume-unchanged index entries; a dirty-tree,
  config-key, skip-worktree and deleted-cwd plant is checked before each
  run.

## Guard: no persistent hooks on allowed ancestors of gated commands

The gate clears persistent hooks only on gated commands. An allowed command
that has gated descendants (hub above hub secret migrate, for example) keeps
its hooks, and cobra runs the nearest persistent pre-run hook only, so a hook
there would run instead of root's refusing hook for the gated descendant.
TestWindowsGate_NoPersistentHooksOnAllowedAncestorsOfGatedCommands walks the
real command tree and, for every gated command, checks each allowed ancestor
below root (root's hook is the refusing one) for PersistentPreRun(E) and
PersistentPostRun(E). It asserts that it checked a non-zero number of pairs
and names the command and hook on failure. Its control plants a
PersistentPreRunE on hub in a copied tree and shows the check reports it.
The test is part of the Windows job's ./cmd anchor. No allowed ancestor in
today's tree has such a hook.
