# Windows CLI platform seams

`./cmd/scion` also builds for Windows as a client-only CLI (hub, messaging, conversation, config, ssh/vscode). The server, Runtime Broker and agent provisioning are unix-only. Any code `./cmd/scion` compiles must keep these seams.

## Unix-only code

- Put it in `*_unix.go` starting with `//go:build unix`; the filename alone is not a build constraint.
- Pair it with `*_other.go` (`//go:build !unix`), same signatures, failing closed: return `fmt.Errorf("...: %w on this platform", errors.ErrUnsupported)`. Never return nil success or fall back to a weaker path-based version.
- Unix-behaviour tests also get `//go:build unix`.
- Examples: `pkg/runtime/git_bind_source_{unix,other}.go`, `pkg/agent/git_base_config_{unix,other}.go`, `cmd/ssh_links_unix_test.go`.

## Private files

Tokens, credentials, settings and per-user state go only through `pkg/util/privatefs`:

- Open: `privatefs.OpenTree(dir, privatefs.TreeOptions{CreateAnchor: true})`.
- Write: `tree.WriteFile`, `tree.MkdirAll`, `tree.Create` with `privatefs.Private` (or `privatefs.ForMode(mode)`).
- Read: `tree.ReadPrivate`; check an open handle with `privatefs.CheckPrivate`.
- A file the user may have symlinked: `privatefs.ReplaceFollowing`.

On Windows these set an owner-only DACL. `os.WriteFile(p, b, 0o600)`, `os.MkdirAll(d, 0o700)` and `os.Chmod` protect nothing there. Use `pathcanon.Within` (`pkg/util/pathcanon`) for path containment and `filelock.Acquire` (`pkg/util/filelock`) for lock files. Models: `pkg/credentials/store.go`, `pkg/config/cli_context.go`.

## Command gate

New cobra commands are hidden and refused on Windows until added to `windowsAllowed` in `cmd/platform_gate.go` (dot-separated paths such as `hub.env.set`; list each parent too). Add one only if it runs entirely client-side (hub API calls; no server, broker or provisioning work) and every local write goes through `privatefs`, `pathcanon` or `filelock`. That is a product decision: say so in the PR. `cmd/platform_gate_test.go` guards the gate (stale entries fail `TestWindowsGate_AllowlistEntriesExist`). This is separate from `agentAllowed` (`cmd/cli_mode.go`).

## `hack/winsec-guard.sh` (CI)

Scans non-test Go files in the `GOOS=windows` build of `./cmd/scion` and fails on:

- (a) `Chmod`/`Fchmod` used as protection;
- (b) `filepath.Rel` followed by `HasPrefix(rel, "..")`;
- (c) `O_NOFOLLOW` or `openNoFollow` set to 0 in a non-unix file;
- (d) `return nil` in a `check*Private*` function in a non-unix file.

Fix the code first. If a line is genuinely not about protection, mark it `//winsec:not-protection <reason>` or `//winsec:not-containment <reason>`. A file reachable only from gated commands goes in `hack/winsec-unprotected.txt` (sorted) as `<path> not protected on Windows: gated (<why>)`. After editing the script, run `bash hack/winsec-guard.sh --self-test`.

## Windows tests

The `windows-latest` job runs only `.github/windows-test-packages.txt`: one package per line, no `./...`, optionally an anchored `-run=^(TestA|TestB)$` whose tests must all run and pass. A skipped test fails the job; link tests use `internal/testutil/wintest`. Windows-only tests go in `*_windows_test.go` as `TestWindows*`. Never list `./pkg/hub`, `./pkg/hub/...`, or any package whose test binary links pkg/hub (about 12 GiB to compile); `./pkg/hub/<sub>` packages that don't link it are fine.

## Local check

```bash
GOOS=windows GOARCH=amd64 go build -buildvcs=false -o /dev/null ./cmd/scion
GOOS=windows GOARCH=amd64 go vet ./pkg/util/privatefs/... ./pkg/util/pathcanon/... ./pkg/util/filelock/... ./pkg/tunnelclient/... ./pkg/<yours>/...
bash hack/winsec-guard.sh
GOOS=windows go test -c -o /dev/null ./pkg/<yours>   # compiles Windows-only tests
```
