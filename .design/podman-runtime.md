# Podman Runtime Support

## Overview
This document outlines the design for adding Podman as a supported container runtime in Scion. Podman provides a daemonless, rootless alternative to Docker, sharing a largely compatible CLI but offering different architectural benefits, particularly regarding security and process isolation.

## Motivation
- **Security**: Podman's rootless mode is a significant advantage for users who cannot or do not want to run a root-level daemon.
- **Compatibility**: Many developers use Podman as a drop-in replacement for Docker (often aliased). Native support ensures Scion works correctly without relying on aliases.
- **Ecosystem**: Podman is the default container tool in several Linux distributions (e.g., RHEL, Fedora) and is gaining popularity on macOS via Podman Machine.

## Implementation Design

### 1. `PodmanRuntime` Implementation
A new `PodmanRuntime` struct will be added to `pkg/runtime/podman.go`, implementing the `Runtime` interface defined in `pkg/runtime/interface.go`.

```go
type PodmanRuntime struct {
    Command string // defaults to "podman"
    Host    string // optional remote Podman socket
}
```

The struct mirrors `DockerRuntime` with a `Host` field for remote Podman connections (e.g., `podman machine` socket or a remote Podman service).

### 2. Command Compatibility & Code Reuse
Most Podman commands are identical to Docker, allowing `PodmanRuntime` to follow the same structure as `DockerRuntime`. The shared logic in `buildCommonRunArgs()` (`pkg/runtime/common.go`) handles volume construction, environment variables, harness integration, and secret staging — all of which are runtime-agnostic and will work for Podman without modification.

Podman-compatible commands:
- `podman run`: Supports `--memory`, `--cpus`, `-v` (bind mounts), `-l` (labels), `-t` (TTY).
- `podman stop`, `podman rm`, `podman logs`, `podman exec`, `podman attach`, `podman pull`.
- `podman inspect --format '{{json .Mounts}}'`: Same output structure for `GetWorkspacePath()`.

### 3. Key Differences & Challenges

#### JSON Output Format (`podman ps`)
Docker's `docker ps --format '{{json .}}'` returns a stream of newline-separated JSON objects.
Podman's `podman ps --format json` returns a single JSON array of objects.

Furthermore, field names and types differ:
- **Docker**: `ID`, `Names`, `Status`, `Image`, `Labels` (all strings; Labels is CSV).
- **Podman**: `Id`, `Names` (array), `Status`, `Image`, `Labels` (map).

`PodmanRuntime.List()` must implement a custom parser:

```go
type podmanListOutput struct {
    Id     string            `json:"Id"`
    Names  []string          `json:"Names"`
    Status string            `json:"Status"`
    Image  string            `json:"Image"`
    Labels map[string]string `json:"Labels"`
}
```

Parsing is simpler than Docker's since Labels are already a map (no CSV splitting needed) and Names are already an array.

#### Rootless Mode & UID/GID Handling
Scion's existing UID/GID synchronization mechanism in `sciontool init` (`cmd/sciontool/commands/init.go`) handles bind mount permissions as follows:

1. The host passes `SCION_HOST_UID` and `SCION_HOST_GID` as environment variables (set in `pkg/runtime/common.go`).
2. The container starts as root via the `sciontool init` entrypoint.
3. `setupHostUser()` uses `usermod`/`groupmod` to adjust the `scion` user's UID/GID to match the host user.
4. The supervisor drops privileges to the adjusted `scion` user via `syscall.Credential`.

**Rootful Podman**: This flow works identically to Docker — the container starts as real root and `sciontool init` can freely modify users and drop privileges.

**Rootless Podman**: This is where the key difference lies. In rootless mode:
- The container runs inside a user namespace where the invoking user is mapped to UID 0 (root) inside the container by default.
- `usermod`/`groupmod` operate on `/etc/passwd` and `/etc/group` inside the container's filesystem layer, which still works within the user namespace.
- Bind-mounted files appear owned by the mapped UID. By default, the host user's UID maps to root (UID 0) inside the container.

The default mapping (host UID → container UID 0) allows `sciontool init` to run privileged operations (usermod, chown) and then drop to the remapped scion user. However, there is a fundamental flaw: after remapping the scion user to the host UID (e.g., 1000), container UID 1000 maps to a **subordinate host UID** (e.g., 100999), not to the actual host user. This makes bind-mounted files unreadable by the host user after the container exits.

**Decision**: Use `--userns=keep-id:uid=1000,gid=1000` to map the host user's UID directly to container UID 1000 (the scion user). This ensures:
1. Bind-mounted files have correct host-user ownership on both sides.
2. The container process runs as the scion user (UID 1000), so harness config in `/home/scion` is accessible without remapping.
3. Works for any host UID (e.g., 501 on macOS, 1000 on Linux) because the `uid=1000` parameter explicitly maps the host user to container UID 1000.

The trade-off is that `sciontool init` cannot perform privileged operations (usermod, groupmod, chown) since PID 1 is no longer root. This is acceptable because with keep-id mapping, the process is already running as the correct container user — no remapping or privilege drop is needed. `setupHostUser()` detects this case (not root, but running as the scion user) and signals `rootless=true` to the supervisor, which sets HOME/USER/LOGNAME without attempting a credential drop.

**Decision**: `PodmanRuntime` should detect rootless mode (via `podman info`) and log the mode at **debug** level. This aids troubleshooting without adding noise at normal log levels.

#### Podman Machine (macOS/Windows)
On macOS and Windows, Podman runs inside a Linux virtual machine managed by `podman machine`. The `podman` CLI communicates with the VM.

Key considerations:
- **Volume mounts**: The host path must be accessible inside the VM. Podman Machine exposes the user's home directory (and configurable additional paths) via virtiofs. Paths outside these mounts will silently fail or error.
- **`GetWorkspacePath()`**: Returns the host path to the workspace. Since Scion worktrees are created relative to the project directory (typically under `$HOME`), they should be within the default VM mount. However, this should be validated.
- **Socket path**: On macOS, the Podman socket is typically at `$XDG_RUNTIME_DIR/podman/podman.sock` or managed by `podman machine`. The `Host` field on `PodmanRuntime` can point to a custom socket if needed.

**Decision**: Validate that the workspace path is within the user's home directory on macOS when using Podman Machine. Since Podman Machine exposes `$HOME` via virtiofs by default, this covers the common case. If the workspace is outside `$HOME`, emit a clear error explaining the limitation and suggesting the user configure additional Podman Machine mounts.

### 4. Integration Plan

#### Factory Registration
Update `pkg/runtime/factory.go` to register "podman" in the runtime type switch:

```go
case "podman":
    pr := NewPodmanRuntime()
    if rtConfig.Host != "" {
        pr.Host = rtConfig.Host
    }
    return pr
```

Also add "podman" to the known-type fallback check (the condition around line 48 that recognizes bare type names like "docker", "kubernetes", etc.).

`NewPodmanRuntime()` should run `podman --version` and parse the major version. If the version is below 4.x, return an `ErrorRuntime` with a message indicating the minimum supported version.

#### Auto-Detection
The runtime should be specified explicitly in the user's profile configuration. However, during `scion init` bootstrapping (when writing the initial `settings.yaml`), auto-detection determines the default:

- **macOS**: Prefer `container` (Apple Virtualization), fall back to `docker`, then `podman`.
- **Linux**: If both `docker` and `podman` are on `$PATH`, **prefer `podman`**. If only one is available, use it.

This auto-detection only runs at bootstrap time. After that, the profile's `runtime` field is authoritative.

#### Default Settings
Add a `podman` entry to the embedded default settings at `pkg/config/embeds/default_settings.yaml` for discoverability:

```yaml
runtimes:
  docker:
    type: docker
    host: ""
  podman:
    type: podman
    host: ""
  container:
    type: container
    tmux: true
  kubernetes:
    type: kubernetes
    context: ""
    namespace: ""
```

Users can then reference it from a profile:
```yaml
profiles:
  local:
    runtime: podman
```

The OS-specific default adjustment logic in `pkg/config/koanf.go` (`GetDefaultSettingsDataYAML()`) and `pkg/config/init.go` (`GetDefaultSettingsData()`) should remain as-is — the `local` profile defaults to `docker` on Linux and `container` on macOS. Users who want Podman configure it explicitly.

#### API Updates
Add `"podman"` to any runtime type enumerations or documentation in `pkg/api/types.go`.

### 5. Method-by-Method Implementation Notes

| Method | Approach |
|---|---|
| `Name()` | Return `"podman"` |
| `Run()` | Same as Docker: call `buildCommonRunArgs()`, prepend `podman run -t`, add resource flags, execute. |
| `Stop()` | `podman stop <id>` — identical to Docker. |
| `Delete()` | `podman rm -f <id>` — identical to Docker. Podman supports `-f` unlike Apple container. |
| `List()` | Custom parser for Podman's JSON array format (see section 3). |
| `GetLogs()` | `podman logs <id>` — identical to Docker. |
| `Attach()` | Same logic as Docker: find container, check status, use `podman attach` or `podman exec -it` for tmux. |
| `ImageExists()` | `podman image inspect <image>` — identical to Docker. |
| `PullImage()` | `podman pull <image>` — identical to Docker. |
| `Sync()` | Same as Docker: bind mounts are automatic, GCS sync handled by existing `gcp` package. |
| `Exec()` | `podman exec <id> <cmd>` — identical to Docker. |
| `GetWorkspacePath()` | `podman inspect --format '{{json .Mounts}}'` — identical to Docker. |

### 6. Shared Code Consideration

Given that most methods are identical to Docker, consider one of these approaches:

**Decision: Independent implementation.** Duplicate the `DockerRuntime` structure in `PodmanRuntime`, changing the command to "podman" and implementing `List()` with the Podman-specific JSON parser. This provides clean separation and allows the two implementations to drift independently as Podman and Docker diverge over time. The duplication is minimal since most shared logic already lives in `buildCommonRunArgs()`.

## Testing Strategy

### Unit Tests
Create `pkg/runtime/podman_test.go` with:
- **JSON parsing tests**: Verify `List()` correctly parses Podman's JSON array format, including edge cases (empty array, containers with no labels, containers with multiple names).
- **Argument construction tests**: Verify `Run()` produces correct `podman run` arguments using a mockable command runner (same pattern as Docker tests).
- **Label filtering tests**: Ensure label-based filtering works with Podman's native map format.

### Integration Tests
- Verify end-to-end lifecycle: create, start, attach, exec, logs, stop, delete.
- Verify bind mount permissions work correctly (UID/GID sync via `sciontool init`).
- Test with both rootful and rootless Podman.

### Cross-Platform
- **Linux (native rootful)**: Standard Podman installation.
- **Linux (native rootless)**: Rootless Podman with user namespace — validate UID/GID handling.
- **macOS (Podman Machine)**: Verify volume mounts pass through the VM correctly and `GetWorkspacePath()` returns usable host paths.

### CI Considerations
If CI runs in a container or restricted environment, rootless Podman may not be available (requires `newuidmap`/`newgidmap` and `/etc/subuid`/`/etc/subgid` configuration). Tests may need to be conditional or run in a dedicated environment.

## Decisions Summary

| # | Topic | Decision |
|---|---|---|
| 1 | **Auto-detection priority** | Specified in profile. At bootstrap, prefer Podman over Docker on Linux when both are available. |
| 2 | **Rootless diagnostics** | Detect rootless mode via `podman info` and log at debug level. |
| 3 | **Podman Machine mount validation** | Validate workspace is within `$HOME` on macOS; error with guidance if not. |
| 4 | **Code structure** | Independent implementation (duplicate from Docker) for clean separation. |
| 5 | **Rootless UID/GID strategy** | Use `--userns=keep-id:uid=1000,gid=1000` to map host user to container scion user (UID 1000). `sciontool init` detects the non-root scion user and skips privilege operations. |
| 6 | **Image registry compatibility** | Not an issue — harness configs provide fully-qualified image names. |
| 7 | **Podman version floor** | Podman 4.x+ minimum; check version during detection and error on older versions. |

## Additional Decisions

| # | Topic | Decision |
|---|---|---|
| 6 | **Image registry compatibility** | Not an issue — harness configs already provide fully-qualified image names (e.g., `docker.io/scion/...`), so Podman's multi-registry search behavior is not a concern. |
| 7 | **Podman version floor** | Minimum supported version is **Podman 4.x+**. Check the version during runtime detection and emit a clear error if an older version is found. Podman 4.x provides stable rootless mode, machine support, and consistent JSON output formats. |
