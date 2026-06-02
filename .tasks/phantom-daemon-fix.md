# Fix: Phantom Daemon After Reinstall

**Branch:** workstation-improvements  
**Key files:** `pkg/daemon/daemon.go`, `cmd/server_daemon.go`  
**Commit all changes to the current branch.**

## Fix 1 — Port conflict detection in `scion server start`

In `cmd/server_daemon.go` `runServerStartOrDaemon()` (around line 34), after the existing `StatusComponent()` check, add a port probe:

```go
// Check for phantom processes holding server ports even without a PID file
if phantomPorts := detectOccupiedPorts(cfg); len(phantomPorts) > 0 {
    fmt.Fprintf(os.Stderr, "Error: the following ports are already in use: %v\n", phantomPorts)
    fmt.Fprintf(os.Stderr, "A previous server process may be running without a PID file.\n")
    fmt.Fprintf(os.Stderr, "Run 'scion server stop --force' to kill any process on these ports.\n")
    return fmt.Errorf("port conflict: ports %v are occupied", phantomPorts)
}
```

Implement `detectOccupiedPorts(cfg)` in `pkg/daemon/daemon.go` or a new `pkg/daemon/ports.go`:
- Try to bind (and immediately release) each server port (default: 8080 for web/hub, 9800 for broker, 9810 for broker gRPC or whatever ports the config uses)
- If binding fails → port is occupied
- Return the list of occupied ports

Look at how the server resolves its ports from `cfg` (search for `cfg.Hub.Port`, `cfg.RuntimeBroker.Port` etc.) to know which ports to probe.

Use `net.Listen("tcp", fmt.Sprintf(":%d", port))` — if it succeeds, close it immediately and mark as free; if it fails with EADDRINUSE, mark as occupied.

## Fix 2 — `scion server stop --force`

In `cmd/server_daemon.go` `runServerStop()` (around line 165), add a `--force` flag:

```
--force    Kill any process listening on the server ports, even without a PID file
```

When `--force` is set:
1. Probe the server ports (reuse `detectOccupiedPorts`)
2. For each occupied port, find the PID of the process holding it using `lsof -ti :<port>` (macOS/Linux) or `ss -tlnp` (Linux fallback)
3. Kill the PID with SIGTERM, wait up to 3s, then SIGKILL if still running
4. Report what was killed

Simple implementation using `exec.Command("lsof", "-ti", fmt.Sprintf(":%d", port))` — parse the output as a PID, then `syscall.Kill(pid, syscall.SIGTERM)`.

If no PID file and no occupied ports, print "No running server found."

Add `--force` to the `stop` command's flags in `cmd/server_daemon.go`.

## Commit Instructions

- `feat: detect port conflicts on server start to catch phantom daemons`
- `feat: add scion server stop --force to kill phantom processes by port`
- Run `go build ./...` and `go vet ./...` before committing
- Do not open PRs — commit directly to `workstation-improvements`
