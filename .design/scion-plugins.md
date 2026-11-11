# Scion Plugin System Design

> **Note (local-only mode removed):** this document describes the CLI-only mode with no Hub ("solo" or "local" mode) as current. That mode was removed; every agent now runs through a Hub (the workstation server on a single machine). See `docs-site/src/content/docs/reference/local-mode-removal.md`.

## Motivation

Scion currently hard-codes all message broker implementations (in-process only) and harness implementations (claude, gemini, opencode, codex, generic) directly into the binary. As we add external message brokers (NATS, Redis, etc.) and potentially new harnesses, this approach does not scale:

- Every new implementation increases binary size and dependency surface
- Users cannot add custom integrations without forking the project
- The hub/broker server carries code for harnesses it may never use

We want a **plugin system** that allows scion to load additional message broker and harness implementations at runtime from external binaries.

## Technology: hashicorp/go-plugin

[hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) provides the foundation:

- **Subprocess model**: Each plugin runs as a separate OS process, communicating via go-plugin's RPC layer (net/rpc or gRPC)
- **Crash isolation**: A plugin crash does not bring down the host
- **Language agnostic**: gRPC plugins can be written in any language
- **Versioning**: Protocol version negotiation between host and plugin
- **Security**: Magic cookie handshake prevents accidental plugin execution
- **Health checking**: Built-in gRPC health service

### Key go-plugin Lifecycle

1. Host calls `plugin.NewClient()` with the path to a plugin binary
2. Host calls `client.Client()` then `raw.Dispense("pluginName")` to get a typed interface
3. The plugin subprocess starts and stays running for the lifetime of the `Client`
4. Host calls methods on the dispensed interface; these become RPC calls
5. `client.Kill()` terminates the subprocess (graceful then force after 2s)

### Long-Running vs Per-Use

go-plugin is designed for **long-lived subprocesses**. The client starts the process once and reuses it for all calls. Per-invocation usage (start, call, kill) is technically possible but adds process-spawn overhead on every call.

**Implications for scion:**

| Plugin Type | Lifecycle | Rationale |
|---|---|---|
| Message Broker | Long-running | Brokers maintain connections, subscriptions, state. Must persist for the hub/broker server lifetime. |
| Harness | Per-agent-lifecycle | Harness methods are called during agent create/start/provision. Could be long-running (shared across agents) or per-use. |

**Recommendation**: Use long-running plugin processes for both types. For harnesses, one plugin process serves all agents using that harness — the overhead of keeping it alive is negligible vs. respawning per agent operation.

### RPC Layer: net/rpc vs gRPC

go-plugin supports two RPC transports: Go's built-in `net/rpc` and gRPC. Since we have zero external implementations today, we have freedom to choose the simplest option.

**Decision: Use go-plugin's `net/rpc` for Go plugins; support gRPC only for non-Go plugin authors.**

Rationale:
- `net/rpc` is simpler for Go-to-Go communication — no protobuf code generation, no `.proto` files to maintain
- Plugin interfaces are small and map directly to Go RPC
- If a plugin needs to talk to a gRPC-based backend (e.g., an external NATS or OpenClaw gateway), **that is internal to the plugin** — the plugin's external protocol does not dictate the host-plugin protocol
- gRPC support can be added later for polyglot plugins without breaking existing Go plugins

## Plugin Types

### Type 1: Message Broker (`broker`)

Implements the `broker.MessageBroker` interface across the plugin boundary. The plugin maintains the external connection (NATS, Redis, etc.) internally, handles reconnection, and delivers inbound messages via the hub API.

For detailed broker plugin interface design, message delivery patterns, authentication, and operational concerns, see [broker-plugins.md](broker-plugins.md).

### Type 2: Harness (`harness`)

Implements the `api.Harness` interface over RPC. The current interface has ~15 methods, most of which are simple getters or file operations.

**Key considerations:**
- `GetHarnessEmbedsFS()` returns an `embed.FS` — cannot cross process boundaries. Plugin harnesses write their embedded files directly during `Provision()`, since the plugin has filesystem access to the same paths. `GetHarnessEmbedsFS()` returns nil for plugin harnesses.
- `Provision()` operates on the local filesystem (agent home dir). The plugin process must have filesystem access to the same paths.
- Some methods are pure data (`Name()`, `GetEnv()`, `GetCommand()`) and could be batched into a single `GetMetadata()` call to reduce round-trips.
- Optional interfaces (`AuthSettingsApplier`, `TelemetrySettingsApplier`) need capability advertisement via a `GetCapabilities()` RPC call.

**Harness RPC interface considerations:**

| Method | Challenge | Solution |
|---|---|---|
| `GetHarnessEmbedsFS()` | Returns `embed.FS` | Plugin writes its own embedded files during `Provision()` directly to the agent home directory. Returns nil for plugin harnesses. |
| `Provision()` | Writes to local filesystem | Plugin has filesystem access to the same paths; pass paths and let plugin write |
| `InjectAgentInstructions()` | Writes to local filesystem | Same as Provision |
| `ResolveAuth()` | Complex types | Serialize as JSON over RPC (Go's `encoding/gob` handles this natively for `net/rpc`) |

## Plugin Discovery and Loading

### Filesystem Layout

```
~/.scion/plugins/
  broker/
    scion-plugin-nats        # Message broker plugin
    scion-plugin-redis       # Message broker plugin
  harness/
    scion-plugin-cursor      # Harness plugin
    scion-plugin-aider       # Harness plugin
```

Plugin binaries follow a naming convention: `scion-plugin-<name>`.

### Settings Configuration

Add a `plugins` section to settings:

```yaml
plugins:
  broker:
    nats:
      path: ~/.scion/plugins/broker/scion-plugin-nats  # optional, auto-discovered if omitted
      config:
        url: "nats://localhost:4222"
        credentials_file: "/path/to/creds"
  harness:
    cursor:
      path: ~/.scion/plugins/harness/scion-plugin-cursor
      config:
        image: "cursor-agent:latest"
        user: "cursor"
```

**Discovery order:**
1. Explicit `path` in settings
2. Scan `~/.scion/plugins/<type>/` directory
3. Search `$PATH` for `scion-plugin-<name>` (lower priority, optional)

### Harness Plugin Selection

Plugin harnesses are available alongside built-in ones. The harness factory (`harness.New()`) checks plugins after built-in types:

```go
func New(harnessName string) api.Harness {
    switch harnessName {
    case "claude": return &ClaudeCode{}
    // ... built-in harnesses
    default:
        if plugin, ok := pluginRegistry.GetHarness(harnessName); ok {
            return plugin
        }
        return &Generic{}
    }
}
```

## Plugin Registration

### Static Registration (Settings-based)

Plugins are declared in settings and loaded at startup. This is sufficient for the initial implementation:

- CLI reads settings, loads relevant plugins when needed
- Hub/broker server loads all configured plugins at startup
- No runtime registration needed

Dynamic self-registration via a hub API endpoint is deferred as a future enhancement. The static approach is simpler, debuggable, and covers the primary use cases.

## Local Mode Support

**Should plugins work in local (non-hub) mode?**

| Plugin Type | Local Mode? | Rationale |
|---|---|---|
| Message Broker | No (initially) | Messaging is a hub/broker feature. Local mode uses the CLI directly - no pub/sub needed. |
| Harness | Yes | A user may want to use a custom harness (e.g., Cursor, Aider) in local mode. The harness interface is used for agent create/start regardless of hub vs local. |

For harness plugins in local mode:
- Plugin process is started on-demand when an agent using that harness is created/started
- Plugin process is kept alive for the duration of the CLI command
- Cleaned up on CLI exit (go-plugin handles this via `CleanupClients()`)

## Implementation Architecture

### Core Package: `pkg/plugin`

```
pkg/plugin/
  manager.go          # Plugin lifecycle management (load, start, stop, health)
  registry.go         # Type-safe plugin registry
  discovery.go        # Filesystem scanning and settings-based discovery
  config.go           # Plugin configuration types
  broker_plugin.go    # RPC client/server wrapper for MessageBroker plugins
  harness_plugin.go   # RPC client/server wrapper for Harness plugins
```

Note: With `net/rpc`, no `.proto` files are needed. The RPC interface is defined in Go code using go-plugin's `plugin.Plugin` interface pattern. If gRPC support is added later for polyglot plugins, proto files would be added at that time.

### Plugin Manager

Central component that owns plugin lifecycle:

```go
type Manager struct {
    clients  map[string]*plugin.Client  // "type:name" -> client
    mu       sync.RWMutex
}

func (m *Manager) LoadAll(cfg PluginsConfig) error     // Load from settings
func (m *Manager) Get(pluginType, name string) (interface{}, error)
func (m *Manager) GetBroker(name string) (broker.MessageBroker, error)
func (m *Manager) GetHarness(name string) (api.Harness, error)
func (m *Manager) Shutdown()                            // Kill all plugins
```

### Plugin Lifecycle Tied to Server Lifecycle

Plugin processes are started when the hub/broker server starts and stopped when it stops. The plugin manager's `Shutdown()` is called as part of the server's graceful shutdown sequence. On `scion server restart` or `scion broker restart`, all plugin processes are killed and restarted with the new server instance.

### Integration Points

**Hub Server** (`pkg/hub/server.go`):
- `Server` receives a `*plugin.Manager` at construction
- If `message_broker` setting names a plugin, dispense broker from manager
- Plugin broker replaces the in-process broker in `MessageBrokerProxy`

**Runtime Broker** (`pkg/runtimebroker/server.go`):
- Similar to hub — receives plugin manager for harness plugins
- When creating agents with a plugin harness, dispense from manager

**CLI** (`cmd/`):
- For local harness plugins: create a temporary manager, load needed plugin, use, cleanup
- No broker plugins in local mode

**Harness Factory** (`pkg/harness/harness.go`):
- Accept optional `*plugin.Manager` parameter
- Fall through to plugin lookup before defaulting to `Generic`

## Decisions

| Topic | Decision | Rationale |
|---|---|---|
| Host-Plugin RPC | Use `net/rpc` for Go plugins | Simpler than gRPC; no proto files. Plugin handles external protocols internally. gRPC option deferred for polyglot support. |
| Harness embed files | Plugin writes files during `Provision()` | Closest to built-in behavior. Plugin has filesystem access, so it can write directly. `GetHarnessEmbedsFS()` returns nil for plugin harnesses. |
| Plugin config schema | Opaque `map[string]string` validated by plugin | Keep it simple for v1. Plugin returns clear errors for invalid config. |
| Security model | Simple trust — user-installed binaries, magic cookie handshake | No signature verification or mTLS for now. Same trust model as any user-installed binary. |
| Dynamic registration | Deferred | Static settings-based registration covers primary use cases. |
| Hot reload | Deferred | Plugin lifecycle tied to server start/stop/restart. No watch-and-reload. |
| Plugin distribution | Deferred | Manual install to `~/.scion/plugins/<type>/`. Future `scion plugin install` command possible. |
| Plugin versioning | Strict version check; reject incompatible | go-plugin protocol version negotiation with hard rejection on mismatch. No graceful degradation. |

### Plugin Versioning

Scion uses go-plugin's protocol version negotiation with **strict matching**:
- Each plugin type (broker, harness) has a protocol version number (starting at 1)
- On `plugin.NewClient()`, scion specifies the expected protocol version
- go-plugin rejects plugins that report a different version — the plugin process is killed and an error is returned
- Any change to the RPC method signatures, argument types, or semantics constitutes a breaking change requiring a version bump
- Plugins can report their minimum compatible scion version via a `GetInfo()` RPC call; scion logs a warning if the plugin targets a newer scion version

## Phased Implementation Plan

### Phase 1: Plugin Infrastructure ✓
- `pkg/plugin/` package with Manager, Registry, Discovery
- `net/rpc` interface definitions for broker and harness plugin types
- Settings schema additions for `plugins` section
- Integration with hub/broker server lifecycle (start/stop/restart)

### Phase 2: Message Broker Plugins ✓
- See [broker-plugins.md](broker-plugins.md) for detailed broker plugin implementation plan

### Phase 3: Harness Plugins ✓
- `net/rpc` interface definitions for harness plugin (`pkg/plugin/harness_plugin.go`)
- `GetHarnessEmbedsFS()` returns empty `embed.FS` for plugin harnesses; callers are nil-safe
- Integration with harness factory (`pkg/harness/harness.go`) and local mode
- Reference harness plugin (`pkg/plugin/refharness/`) with full RPC integration tests
- Optional capability tests for `AuthSettingsApplier` and `TelemetrySettingsApplier`

### Phase 4: Polish
- `scion plugin list` command showing discovered/loaded plugins
- Health status reporting
- Documentation and plugin authoring guide
- Optional: gRPC plugin support for non-Go plugin authors

## Related Design Documents

- [Broker Plugins](broker-plugins.md) - Broker-specific plugin design (message delivery, auth, routing)
- [Message Broker](hosted/hub-messaging.md) - Current messaging architecture
- [Hosted Architecture](hosted/hosted-architecture.md) - Hub/broker separation
- [Server Implementation](hosted/server-implementation-design.md) - Unified server command
- [Settings Schema](settings-schema.md) - Settings configuration format
