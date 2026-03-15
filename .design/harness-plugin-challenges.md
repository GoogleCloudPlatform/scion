# Harness Plugin Extensibility Challenges

## Problem Statement

The harness plugin system (via `pkg/plugin/` and hashicorp/go-plugin RPC) successfully externalizes harness **logic** — provisioning, auth, env vars, commands. However, two coupled extension points remain closed to plugin authors:

1. **Sciontool hook dialects** — parsing harness-specific hook JSON into normalized events is compiled Go code inside `pkg/sciontool/hooks/dialects/`. A new harness with its own hook event schema requires modifying sciontool itself.
2. **Container images** — harness CLIs and runtime dependencies are baked into Dockerfiles in `image-build/`. A new harness needs its own container, but there's no mechanism for a plugin to declare which image to use.

This means a contributor who builds a harness plugin still needs to fork and modify the scion codebase for hooks and containers, undermining the plugin model.

## Current Architecture

### What's Already Pluggable

- Harness interface (`pkg/api/harness.go`): `Provision()`, `GetCommand()`, `GetEnv()`, `ResolveAuth()`, etc.
- Plugin discovery: binary naming convention (`scion-plugin-<name>`), filesystem scan, PATH lookup, explicit config
- Plugin RPC bridge: `pkg/plugin/harness_plugin.go` handles cross-process communication
- Reference implementation: `pkg/plugin/refharness/` provides a working template

### What's Not Pluggable

**Hook Dialects** (`pkg/sciontool/hooks/dialects/`)

Each dialect is a compiled Go struct implementing the `Dialect` interface:
```go
type Dialect interface {
    Name() string
    Parse(data map[string]interface{}) (*Event, error)
}
```

Dialects map harness-specific event names to normalized events (`tool-start`, `prompt-submit`, `session-start`, etc.) and extract fields from the hook JSON payload. The existing Claude, Gemini, and Codex dialects are mostly simple mapping tables with minimal logic.

**Container Images** (`image-build/`)

The image hierarchy is: `core-base` → `scion-base` → `<harness>` (claude, gemini, etc.). Harness images install the CLI tool and any harness-specific configuration. The runtime layer currently hardcodes the image-to-harness mapping.

## Proposed Approach

### Tier 1: Declarative Dialect Spec (High Priority)

Most dialects are simple mapping tables. Make them data-driven rather than compiled.

A plugin would ship a dialect definition (YAML/JSON), either bundled alongside its binary or returned via a new RPC method:

```yaml
dialect: cursor
event_name_field: hook_event_name
mappings:
  BeforeToolCall:
    event: tool-start
    fields:
      tool_name: .tool_name
      tool_input: .tool_input
  AfterToolCall:
    event: tool-end
    fields:
      tool_name: .tool_name
      success: .success
  UserPrompt:
    event: prompt-submit
    fields:
      prompt: .prompt
  SessionBegin:
    event: session-start
  SessionEnd:
    event: session-end
```

**Implementation:**

- Add a `MappingDialect` implementation in `pkg/sciontool/hooks/dialects/` that loads from a spec file at runtime
- Load specs from `~/.scion/plugins/harness/<name>/dialect.yaml` or via a `GetDialectSpec() ([]byte, error)` RPC method on the plugin interface
- The `HarnessProcessor` registers these alongside built-in dialects at startup
- All existing handlers (status, logging, hub, telemetry, limits) work unchanged since they operate on normalized events

**For complex parsing needs:** consider supporting CEL or Starlark expressions for field extraction, but defer until someone actually needs it.

**Why this is highest priority:** sciontool runs *inside the container*. Without this, every new harness requires rebuilding sciontool and the container image, which defeats the plugin model entirely.

### Tier 2: Plugin-Declared Container Image

Add an optional RPC method to the harness plugin interface:

```go
type ContainerImageProvider interface {
    ContainerImage() ContainerImageSpec
}

type ContainerImageSpec struct {
    // Pre-built image reference (e.g. "ghcr.io/someone/scion-harness-cursor:latest")
    Image string

    // Alternative: base image to layer on (e.g. "scion-base")
    BaseImage string

    // Alternative: inline Dockerfile content for local build
    Dockerfile string
}
```

This lets a plugin declare:
- **"Use this image"** — pre-built and published to a registry
- **"Layer on scion-base"** — provide a Dockerfile fragment that the runtime builds locally

The runtime layer already resolves images; it just needs to consult the plugin instead of maintaining a hardcoded mapping.

For contributors, the simplest path: publish a container image extending `scion-base`, reference it from plugin metadata.

### Tier 3: Contributor SDK / Scaffold

Once Tiers 1 and 2 are in place, formalize the contributor experience:

1. `scion plugin init my-harness` — scaffolds a Go module from refharness template
2. Contributor implements `Provision()`, `GetCommand()`, `ResolveAuth()`
3. Contributor writes a `dialect.yaml` for hook event mapping
4. Contributor writes a `Dockerfile` extending `scion-base` (or references an existing image)
5. `scion plugin build` — produces `scion-plugin-my-harness` binary + packages dialect spec
6. Drop into `~/.scion/plugins/harness/` and it works

## Priority

1. **Declarative dialect spec** — unblocks hook processing without sciontool changes; architecturally the most constraining problem
2. **Plugin-declared container image** — less urgent since contributors can build images from `scion-base` manually; mainly a config/settings wiring problem
3. **Scaffold tooling** — quality-of-life; depends on Tiers 1 and 2 being stable

## Related Files

| Area | Key Files |
|------|-----------|
| Harness interface | `pkg/api/harness.go` |
| Harness factory | `pkg/harness/harness.go` |
| Plugin system | `pkg/plugin/harness_plugin.go`, `manager.go`, `discovery.go`, `config.go` |
| Reference plugin | `pkg/plugin/refharness/refharness.go` |
| Hook processor | `pkg/sciontool/hooks/harness.go` |
| Hook types | `pkg/sciontool/hooks/types.go` |
| Existing dialects | `pkg/sciontool/hooks/dialects/claude.go`, `gemini.go`, `codex.go` |
| Handlers | `pkg/sciontool/hooks/handlers/status.go`, `logging.go`, `hub.go`, etc. |
| Container images | `image-build/scion-base/Dockerfile`, `image-build/claude/Dockerfile`, etc. |
