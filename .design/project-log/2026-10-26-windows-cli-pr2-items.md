# Windows CLI PR 2: tracked items

**Date:** 2026-10-26
**Branch:** win-cli/pr2-items (from win-cli/pr2-base c07584c)

PR 2 (Windows CLI phase 4) follow-up items, one commit each.

## Changes

1. **`RemoveAllSafe` fails closed without unlinkat** (`pkg/util/fs.go`,
   `fs_unlinkat_other.go`). On non-unix, `unlinkAt` returns
   `errUnlinkAtUnsupported` (wraps `errors.ErrUnsupported`) and
   `removeSymlinkSafe` returns it without trying rename-over or a path-based
   `os.Remove`; the entry stays. `removeSymlinkSafe` now returns an error and
   `RemoveAllSafe` records it. Unix behaviour is unchanged (it still returns
   nil there; a failed unix removal still surfaces when the parent directory
   cannot be removed). Tests: `pkg/util/fs_windows_test.go`.
   Callers compiled into the Windows `./cmd/scion` (all behind the Windows
   command gate): `util.RemoveWorktree`, `agent.deleteAgentFiles` (via
   `DeleteAgentFiles` from `cmd/delete.go` `cleanupAfterHubDelete`:
   `delete`, `stop --rm`), and hub server project-dir removal in
   `pkg/hub/handlers_projects_core.go`.
2. `TestHostGitRunsNoHooksWindows` anchored in
   `.github/windows-test-packages.txt` (new `./pkg/util` line).
3. Windows tests for the git-metadata stubs:
   `pkg/runtime/git_bind_source_windows_test.go`,
   `pkg/agent/git_base_config_windows_test.go`. Neither package's Windows
   test binary links pkg/hub. Only the pkg/agent test is anchored: see
   item 10.
4. `//go:build unix` on `pkg/runtime/git_bind_source_check_test.go`,
   `pkg/runtime/git_metadata_mounts_test.go`,
   `pkg/agent/git_private_config_test.go` (they exercise the unix
   implementations: symlinks, O_NOFOLLOW, link counts).
5. `seedPrivateGitConfig` reads the base config before creating
   `<agentDir>/git`, so a refused read creates nothing (no other effect).
6. AGENTS.md section "Platform seams (Windows CLI)".
7. `windows.yml` concurrency group (ci.yml's block; superseded PR runs
   cancelled, main never).
8. `hub shadow` dropped from the Windows supported-commands docs list.
9. `windows.yml` guard: a compile-free `GOOS=windows go list -test` over
   every listed package fails the job if any test binary other than
   `cmd.test` depends on `pkg/hub`. Planted `./pkg/integration/runtime`
   (links pkg/hub) and `./pkg/brokerregistration` (links it only through its
   tests) both fail it; the real list passes (91 test binaries).

10. `./pkg/runtime` left off the Windows list (EM decision): its test
    package does not build for Windows. Eight untagged test files use
    unix-only syscalls (`cloudrun_sandbox_delete_workaround`,
    `cloudrun_sandbox_diag`, `cloudrun_sandbox_runtime`,
    `cloudrun_sandbox_state_robust`, `home_ownership`, `resume_fallback`,
    `substrate_bootstrap_home`, `workspace_project_config_v2`) and define
    helpers other test files share (`mockHarness`, `writeMockBin`,
    `newWorkaroundTestRuntime`, `ran`, `fsSnapshot`,
    `assertNothingCreated`). `git_bind_source_windows_test.go` stays for the
    follow-up that splits those helpers out.

## Verification

- Dev-time b02 LIGHT run (not evidence): linux `go test -run` on the
  touched tests in pkg/util, pkg/runtime, pkg/agent pass;
  `GOOS=windows go test -c` of pkg/util and pkg/agent pass on amd64 and
  arm64; pkg/runtime fails as described in item 10.
- gofmt clean; `hack/winsec-guard.sh` self-test and run pass;
  `hack/check-runner-routing.sh` selftest and run pass.
- Not run here: the Windows `./cmd/scion` cross-build (left to the fork CI
  draft) and the tests on a real Windows host (the CI "Windows build and
  test (allowlist)" job).

## Learnings

- `go list -test -f '{{if and (not .ForTest) (eq .Name "main")}}...'` gives
  each `<pkg>.test` binary's full dependency set in one call (about 1.5 s for
  the whole Windows list), far cheaper than a `go list -deps -test` per
  package.
- `syscall.Errno.Is(errors.ErrUnsupported)` is true for ENOSYS/ENOTSUP/
  EOPNOTSUPP, so a fail-closed branch keyed on `errors.ErrUnsupported` alone
  could fire on unix too; a private sentinel keeps unix behaviour unchanged.

## Follow-ups (not done)

- Split the pkg/runtime test helpers (item 10) and list `./pkg/runtime`
  with the two `TestWindowsGitBind*` anchors.

- The symlink tests in `pkg/util/fs_test.go` are untagged; they would fail
  on Windows if the full `pkg/util` suite were ever listed (only anchored
  tests run today).
