# sciontool rootexec guard

When adding or changing process execution (`exec.Command`, `exec.CommandContext`, or package-level aliases of them) under `cmd/sciontool/` or `pkg/sciontool/`, run the rootexec guard tests:

```bash
go test -buildvcs=false ./pkg/sciontool/rootexec/...
```

This suite runs in under a second inside a container and needs no special build tags. It is included in `make test-fast-ci` and upstream CI, so it is easy to miss when running scoped package tests (for example `go test -buildvcs=false ./pkg/sciontool/<pkg>/...`).

## What it protects

On substrate, `sciontool` runs root-context operations with an inherited `PATH` that includes workload-owned directories (such as `/usr/local/share/npm-global/bin`) ahead of `/usr/bin`. Resolving a bare command name against `PATH`—or a relative path against cwd—can execute a workload-planted binary as root.

`TestNoRootContextExecUsesABareUnresolvedCommandName` (`pkg/sciontool/rootexec/guard_test.go`) statically sweeps `guardedDirs` (`cmd/sciontool`, `cmd/sciontool/commands`, and `pkg/sciontool/{substrate,hooks,hooks/dialects,hooks/handlers,metadata,supervisor,services,telemetry}`) and enforces:

- Every `exec.Command` / `exec.CommandContext` call (direct or via a package-level alias) uses an argument that is provably safe—an inline `rootexec.Resolve(...)` call, an absolute string literal starting with `/`, or a local identifier whose nearest preceding assignment in the smallest enclosing function/closure is one of those—or matches an entry in `execSiteAllowlist`.
- Guarded files import `os/exec` plainly (no renamed or dot imports) and do not pass exec constructors as values the static check cannot trace.
- `execSiteAllowlist` contains no stale entries.

## What a failure means

- **Unresolved command argument:** resolve bare command names with `rootexec.Resolve("<cmd>")` in the same function before calling `exec.Command`/`exec.CommandContext` (pair with `rootexec.Env(...)` so ambient `PATH`/`LD_*` variables are not inherited; use `rootexec.SelfExe()` for `/proc/self/exe` self-re-exec), or pass an absolute string literal.
- **Legitimate site the static check cannot prove** (for example, a privilege-dropped workload command or a helper-resolved path across an intervening call): add an `execSite` entry with a one-line reason to `execSiteAllowlist` in `pkg/sciontool/rootexec/guard_test.go`.
- **Stale allowlist entry:** a listed call was removed, moved to another function, or rewritten—update or delete its entry in `execSiteAllowlist`.
- **Renamed/dot `os/exec` import or escaping constructor value:** switch to plain `import "os/exec"` and call `exec.Command`/`exec.CommandContext` (or a package-level alias var) directly.
- **New root-context package under `pkg/sciontool/`:** add the directory to `guardedDirs` in `pkg/sciontool/rootexec/guard_test.go`.
