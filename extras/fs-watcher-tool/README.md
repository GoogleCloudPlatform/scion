# scion-fs-watcher

A standalone Linux utility that monitors filesystem activity across scion agent worktrees and attributes each event (create, modify, delete, rename) to the originating agent. It uses Linux `fanotify` for filesystem monitoring and correlates process PIDs to Docker container labels for agent attribution.

## Requirements

- **Linux** kernel 5.9 or later (for `FAN_REPORT_DFID_NAME` support)
- **Go** 1.25+ (to build from source)
- **Docker** daemon running on the host
- **Root access** or `CAP_SYS_ADMIN` capability (required by fanotify)

## Building

```bash
cd extras/fs-watcher-tool
make build
```

This produces a `scion-fs-watcher` binary in the current directory.

To run the full local CI (format, vet, test, build):

```bash
make ci
```

## Usage

The watcher must run as root (or with `CAP_SYS_ADMIN`) on the host machine, not inside a container.

### Watch a project

The simplest way to use the watcher is with `--project`, which automatically discovers all agent worktree directories by inspecting Docker containers with the matching `scion.grove` label:

```bash
sudo ./scion-fs-watcher --project my-project
```

This will:
1. Query Docker for all containers labeled `scion.grove=my-project`
2. Inspect their bind mounts to find workspace directories
3. Start monitoring those directories with fanotify
4. Output NDJSON events to stdout as agents create, modify, or delete files
5. Dynamically add/remove watched directories as agent containers start and stop

### Watch explicit directories

You can also watch specific directories directly:

```bash
sudo ./scion-fs-watcher --watch /path/to/worktree-a --watch /path/to/worktree-b
```

Both `--project` and `--watch` can be combined:

```bash
sudo ./scion-fs-watcher --project my-project --watch /extra/shared/dir
```

### Write to a log file

By default, events are written to stdout. To write to a file instead:

```bash
sudo ./scion-fs-watcher --project my-project --log /var/log/scion/fs-events.ndjson
```

### Filter noise

Exclude paths with `--ignore` glob patterns (repeatable):

```bash
sudo ./scion-fs-watcher --project my-project \
  --ignore '.git/**' \
  --ignore 'node_modules/**' \
  --ignore '*.swp'
```

For more complex filtering, use a `.gitignore`-style filter file:

```bash
sudo ./scion-fs-watcher --project my-project --filter-file /path/to/fs-filter.txt
```

The filter file supports `#` comments, `!` negation for re-inclusion, and glob patterns. Send `SIGHUP` to reload the filter file without restarting:

```bash
kill -HUP $(pidof scion-fs-watcher)
```

### Debug mode

Use `--debug` for verbose output on stderr showing Docker interactions, PID resolution, fanotify setup, and event processing:

```bash
sudo ./scion-fs-watcher --project my-project --debug
```

Example debug output:

```
[docker] connected to docker daemon at unix:///var/run/docker.sock
[docker] server version: 27.1.0, containers: 5 running / 12 total
[docker] cgroup driver: systemd, cgroup version: 2
[resolver] warming up container cache (label filter: scion.name)
[resolver]   cached container a1b2c3d4e5f6 → agent "frontend-refactor"
[resolver]   cached container f6e5d4c3b2a1 → agent "backend-api"
[resolver] warmed up with 2 scion containers
[project] discovered watch dir: /home/user/.scion_worktrees/my-project/frontend-refactor (agent: frontend-refactor, dest: /workspace)
[project] discovered watch dir: /home/user/.scion_worktrees/my-project/backend-api (agent: backend-api, dest: /workspace)
[project] discovered 2 directories for project "my-project"
[config] project="my-project", label-key="scion.name", debounce=300ms, cache-ttl=5m0s
[config] ignore patterns: [.git/**]
[config] log output: -
[config] watch root [0]: /home/user/.scion_worktrees/my-project/frontend-refactor
[config] watch root [1]: /home/user/.scion_worktrees/my-project/backend-api
[resolver] subscribing to docker events (type=container, actions=start,die)
[watcher] subscribed to docker container lifecycle events (start/die)
scion-fs-watcher started, watching 2 directories
[watcher] marking filesystem for dir: /home/user/.scion_worktrees/my-project/frontend-refactor
[watcher] marking filesystem for dir: /home/user/.scion_worktrees/my-project/backend-api
[watcher] fanotify fd=3, flags=FAN_CLASS_NOTIF|FAN_REPORT_DFID_NAME|FAN_CLOEXEC
[watcher] mark flags=FAN_MARK_ADD|FAN_MARK_FILESYSTEM, mask=ACCESS|CREATE|DELETE|CLOSE_WRITE|MOVED_FROM|MOVED_TO
[watcher] watching 2 directories, debounce=300ms
[watcher] entering event loop (poll timeout=500ms)
[resolver] pid 12345 → container a1b2c3d4e5f6 → agent "frontend-refactor" (resolved)
```

## Output Format

Events are written as line-delimited JSON (NDJSON), one object per line:

```json
{"ts":"2026-03-24T14:32:01.003Z","agent_id":"frontend-refactor","action":"modify","path":"web/src/client/App.tsx","size":4096}
{"ts":"2026-03-24T14:32:01.150Z","agent_id":"backend-api","action":"create","path":"pkg/hub/handlers.go","size":1523}
{"ts":"2026-03-24T14:32:02.400Z","agent_id":"frontend-refactor","action":"delete","path":"web/src/client/old-util.ts"}
```

| Field      | Type   | Description |
|------------|--------|-------------|
| `ts`       | string | RFC 3339 timestamp with millisecond precision |
| `agent_id` | string | Value of the `scion.name` Docker label, or `""` if unresolvable |
| `action`   | string | One of: `create`, `modify`, `delete`, `rename_from`, `rename_to` |
| `path`     | string | Relative path from the watched root directory |
| `size`     | int    | File size in bytes (omitted for `delete` and `rename_from`) |

## CLI Reference

```
sudo scion-fs-watcher [flags]
```

| Flag            | Description | Default |
|-----------------|-------------|---------|
| `--project`     | Project ID — auto-discover agent directories via Docker labels | (none) |
| `--watch`       | Directory to watch explicitly (repeatable) | (none) |
| `--log`         | Output log file path (`-` for stdout) | `-` (stdout) |
| `--label-key`   | Docker label key to use as agent ID | `scion.name` |
| `--ignore`      | Glob patterns to exclude (repeatable) | `.git/**` |
| `--filter-file` | Path to `.gitignore`-style filter file | (none) |
| `--debounce`    | Duration to collapse rapid edits to the same file | `300ms` |
| `--cache-ttl`   | Duration to cache PID-to-container mappings | `5m` |
| `--debug`       | Enable verbose debug logging to stderr | `false` |

At least one of `--project` or `--watch` is required.

## How It Works

1. **fanotify** monitors the filesystem for create, modify (`FAN_CLOSE_WRITE`), delete, and rename events. Using `FAN_MARK_FILESYSTEM` provides coverage across all paths on the filesystem without needing per-directory watches.

2. **PID resolution** maps each event's process ID to a Docker container by reading `/proc/<pid>/cgroup`, then looks up the container's `scion.name` label via the Docker API. Results are cached to avoid repeated API calls.

3. **Debouncing** collapses rapid successive events on the same file (keyed by agent + path) within a configurable window (default 300ms), so a burst of writes produces a single `modify` event.

4. **Rename coalescing** detects the common editor save pattern (write to temp file, then rename over the target) and emits a single `modify` event instead of a `rename_from` + `rename_to` pair.

5. **Dynamic discovery** (with `--project`) subscribes to Docker container lifecycle events to automatically start/stop watching directories as agent containers come and go.
