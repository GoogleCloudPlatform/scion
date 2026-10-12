# Universal MCP Server Configuration in Scion Templates

## Motivation

Today, configuring MCP servers for a scion template requires duplicating harness-specific configuration across every harness-config variant. Take a web development template, here called `my-web-template`, that gives agents access to a Chrome DevTools MCP server. The template author must:

1. Define a `chromium` service in `scion-agent.yaml` (harness-agnostic)
2. Add `mcpServers.chrome-devtools` to `harness-configs/claude-web/home/.claude.json` (Claude-specific JSON)
3. Add `mcpServers.chrome-devtools` to `harness-configs/gemini-web/home/.gemini/settings.json` (Gemini-specific JSON)
4. Know that OpenCode and Codex have different MCP configuration mechanisms

The MCP server definition itself is identical across harnesses — the same `command`, `args`, and `env` — but must be expressed in each harness's native config format and written to the correct file path. This creates several problems:

- **Duplication**: The same logical MCP server is defined N times for N harness variants.
- **Drift**: A template author updates the Claude config but forgets the Gemini one. The harness variants silently diverge.
- **Expertise barrier**: Template authors need to understand each harness's native config format, file locations, and JSON/YAML structure to add an MCP server.
- **Incomplete coverage**: Harnesses added later (or community harness-configs) don't automatically inherit MCP servers defined in the template.
- **No validation**: MCP server definitions embedded in raw JSON home files bypass scion's config validation and schema enforcement.

### What We Want

A single, harness-agnostic `mcp_servers` block in `scion-agent.yaml` that:

1. Captures the full MCP server specification once, at the template level
2. Is validated against a schema at config load time
3. Is provisioned into each harness's native configuration format during agent creation
4. Works with the existing `services` block for MCP servers that need sidecar processes
5. Supports all common MCP transport types (stdio, SSE, streamable HTTP)

## Proposed Schema

### `scion-agent.yaml` Extension

```yaml
# Existing fields
default_harness_config: claude-web

# Existing: sidecar process definitions
services:
  - name: chromium
    command: ["chromium", "--headless", "--no-sandbox", "--remote-debugging-port=9222"]
    restart: always
    ready_check:
      type: tcp
      target: "localhost:9222"
      timeout: "10s"

# NEW: Universal MCP server configuration
mcp_servers:
  chrome-devtools:
    transport: stdio
    command: chrome-devtools-mcp
    args: ["--headless", "--browser-url", "http://localhost:9222"]
    env:
      DEBUG: "false"

  filesystem:
    transport: stdio
    command: npx
    args: ["-y", "@anthropic/mcp-filesystem", "/workspace"]

  remote-api:
    transport: sse
    url: "http://localhost:8080/mcp/sse"
    headers:
      Authorization: "Bearer ${MCP_API_TOKEN}"

  streaming-service:
    transport: streamable-http
    url: "http://localhost:9090/mcp"
    headers:
      X-API-Key: "${SERVICE_API_KEY}"
```

### MCP Server Configuration Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `transport` | enum | Yes | Transport protocol: `stdio`, `sse`, or `streamable-http` |
| `command` | string | stdio only | Executable to launch |
| `args` | []string | No | Command-line arguments |
| `env` | map[string]string | No | Environment variables passed to the MCP process |
| `url` | string | sse/http only | Server endpoint URL |
| `headers` | map[string]string | No | HTTP headers (sse/http only) |
| `scope` | enum | No | Where to register: `global` (default) or `project` |

#### Transport Types

**`stdio`** — The MCP server runs as a child process of the harness. The harness launches the command and communicates via stdin/stdout JSON-RPC.

- Requires: `command`
- Optional: `args`, `env`
- This is the most common transport for locally-installed MCP servers (e.g., filesystem, git, browser tools).

**`sse`** — The MCP server is an HTTP service that uses Server-Sent Events for server-to-client messages and HTTP POST for client-to-server messages.

- Requires: `url`
- Optional: `headers`
- Suitable for MCP servers running as sidecar services (defined in `services`) or external endpoints.

**`streamable-http`** — The newer HTTP-based transport where all communication uses HTTP POST with optional SSE streaming for server responses.

- Requires: `url`
- Optional: `headers`
- The emerging standard for HTTP-based MCP servers.

#### Scope

- **`global`** (default): The MCP server is registered at the harness's global/user-level configuration. It is available to all projects within the agent session.
- **`project`**: The MCP server is registered only for the agent's workspace project. Useful when the server is workspace-specific (e.g., a project-scoped database tool).

Not all harnesses distinguish between global and project scope. For harnesses that do not, `project`-scoped servers are treated as `global`.

#### Environment Variable Interpolation

String values in `url`, `headers`, `args`, and `env` support `${VAR_NAME}` interpolation from the agent's resolved environment. This allows MCP server configs to reference:

- Secrets injected via `scion-agent.yaml` `secrets` definitions
- Environment variables set via `env` in `scion-agent.yaml` or harness-config
- Runtime-provided variables (e.g., `${AGENT_WORKSPACE}`)

Unresolvable variables are left as literal strings (not an error), allowing harness-native variable expansion to handle them at runtime.

### Go Type Definitions

```go
// pkg/api/types.go

type MCPTransport string

const (
    MCPTransportStdio          MCPTransport = "stdio"
    MCPTransportSSE            MCPTransport = "sse"
    MCPTransportStreamableHTTP MCPTransport = "streamable-http"
)

type MCPScope string

const (
    MCPScopeGlobal  MCPScope = "global"
    MCPScopeProject MCPScope = "project"
)

type MCPServerConfig struct {
    Transport MCPTransport      `json:"transport" yaml:"transport"`
    Command   string            `json:"command,omitempty" yaml:"command,omitempty"`
    Args      []string          `json:"args,omitempty" yaml:"args,omitempty"`
    Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
    URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
    Headers   map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
    Scope     MCPScope          `json:"scope,omitempty" yaml:"scope,omitempty"`
}
```

Add to `ScionConfig`:

```go
type ScionConfig struct {
    // ... existing fields ...
    Services   []ServiceSpec              `json:"services,omitempty" yaml:"services,omitempty"`
    MCPServers map[string]MCPServerConfig `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"` // NEW
    // ... existing fields ...
}
```

### Validation Rules

Added to `ValidateScionConfig()` in `pkg/config/validate.go`:

1. `transport` is required and must be one of `stdio`, `sse`, `streamable-http`
2. `command` is required when `transport` is `stdio`; disallowed otherwise
3. `args` is only valid when `transport` is `stdio`
4. `url` is required when `transport` is `sse` or `streamable-http`; disallowed for `stdio`
5. `headers` is only valid when `transport` is `sse` or `streamable-http`
6. `scope` defaults to `global` if omitted; must be one of `global`, `project`
7. MCP server names follow the same slug rules as service names (alphanumeric, hyphens, underscores)
8. MCP server names must not conflict with service names (warning, not error — they are different namespaces but confusion is likely)

### Config Merging

`MCPServers` follows the same merge semantics as other map fields in `MergeScionConfig()`: entries from higher-priority layers override entries from lower-priority layers by key. An entry can be explicitly removed by setting it to a zero-value marker (TBD: `null` in YAML, or an `enabled: false` field).

Harness-config `config.yaml` overrides use the **same universal `MCPServerConfig` format**, not the harness's native config format. Merging happens entirely at the scion-schema level before any translation to native format. The provisioning layer translates the final merged map once. This keeps translation logic in one place and means harness-config scripts only receive an already-merged universal config.

## Relationship to Services

MCP servers and services are related but distinct concepts:

| Concern | `services` | `mcp_servers` |
|---|---|---|
| **What it defines** | A sidecar process to run inside the container | An MCP server the harness should connect to |
| **Lifecycle** | Managed by sciontool (start, health check, restart) | Managed by the harness (or sciontool for stdio) |
| **Where it runs** | Inside the container, supervised by sciontool | Inside the container (stdio) or external (sse/http) |
| **Config target** | `scion-services.yaml` consumed by sciontool | Harness-native config files (`.claude.json`, `.gemini/settings.json`, etc.) |

A common pattern combines both: a `service` runs a dependency (e.g., Chromium), and an `mcp_server` configures the harness to connect to it via an MCP bridge:

```yaml
services:
  - name: chromium
    command: ["chromium", "--headless", "--remote-debugging-port=9222"]
    restart: always
    ready_check:
      type: tcp
      target: "localhost:9222"
      timeout: "10s"

mcp_servers:
  chrome-devtools:
    transport: stdio
    command: chrome-devtools-mcp
    args: ["--headless", "--browser-url", "http://localhost:9222"]
```

There is no automatic linkage between a service and an MCP server — they are independently defined and provisioned. A future enhancement could add a `depends_on` field to `MCPServerConfig` referencing a service name, ensuring the service is healthy before the MCP server is started. This is deferred.

## Harness Provisioning: How MCP Configs Reach Native Formats

Each harness has its own native configuration format and file path for MCP servers. The provisioning layer must translate the universal `MCPServerConfig` into the harness's expected structure.

### Current Native Formats

**Claude Code** (`.claude.json` or `.claude.json` projects section):
```json
{
  "mcpServers": {
    "server-name": {
      "type": "stdio",
      "command": "executable",
      "args": ["--flag", "value"],
      "env": {"KEY": "value"}
    }
  }
}
```
- Scope `global` → top-level `mcpServers`
- Scope `project` → `projects[workspace_path].mcpServers`
- Supports: `stdio` natively. SSE/HTTP support varies by Claude Code version.

**Gemini CLI** (`.gemini/settings.json`):
```json
{
  "mcpServers": {
    "server-name": {
      "type": "stdio",
      "command": "executable",
      "args": ["--flag", "value"],
      "env": {"KEY": "value"}
    }
  }
}
```
- Scope: `global` only (Gemini does not distinguish project-scoped MCP)
- Supports: `stdio` natively. SSE/HTTP support varies.

**Codex CLI** (`~/.codex/mcp_servers.json` or `~/.codex/config.toml`):
- Scope: `global` (`~/.codex/mcp_servers.json` or under `[mcp_servers.<name>]` in `~/.codex/config.toml`) and `project` (`.codex/mcp_servers.json` in workspace root).
- Supports: `stdio` natively and `http` for remote servers.
- Command-line equivalent: `codex mcp add <name> --env KEY=value -- <command> <args>`

**OpenCode** (`opencode.json` or equivalent):
- MCP support: TBD. Config format differs from Claude/Gemini.

### Provisioning Flow

```
scion-agent.yaml              (template author defines mcp_servers)
       │
       ▼
ScionConfig.MCPServers         (parsed and validated at config load)
       │
       ▼
ContainerScriptHarness.Provision()   (host-side staging only — no file writes)
       │
       ├──► writes inputs/mcp-servers.json to agent_home/.scion/harness/inputs/
       │
       └──► (existing) writes scion-services.yaml for sidecar services
               │
               ▼ [container starts]
       sciontool harness provision (pre-start lifecycle hook, inside container)
               │
               ▼
       provision.py (inside container)
               │
               ├── Claude:  reads inputs/mcp-servers.json, merges into .claude.json mcpServers
               ├── Gemini:  reads inputs/mcp-servers.json, merges into .gemini/settings.json mcpServers
               ├── Codex:   reads inputs/mcp-servers.json, writes to ~/.codex/mcp_servers.json
               └── Generic: reads inputs/mcp-servers.json, no-op or logs warning
```

The critical point: **the host stages data; the container applies it.** There is no host-side Go code that writes harness-native MCP config files. The `MCPServers` map from `ScionConfig` is serialized to JSON and placed in the harness bundle as an input file, just as `telemetry.json` and `auth-candidates.json` are staged for other concerns.

### No Separate Go Interface Needed

The original proposal included an `MCPProvisioner` Go interface on `Harness`. This is not needed in the container-script model. The host's responsibility is only to stage `inputs/mcp-servers.json` when `ScionConfig.MCPServers` is non-empty — a straightforward addition to `ContainerScriptHarness.Provision()`. The harness-specific translation logic lives entirely in `provision.py`.

For built-in Go harnesses (Claude, Gemini) that are not yet migrated to container-script, a temporary `MCPProvisioner` optional interface could be added during the transition period. However, given that Phases 1–5 of the decoupled harness work are already complete (OpenCode and Codex have `provision.py`), the preferred path is to implement MCP support directly in the container-script model for any harness that already has a `provision.py`.

### Capability Advertisement

MCP transport support is best declared in the harness `config.yaml` alongside other capability metadata, consistent with how Phases 1–5 moved capabilities out of Go and into declarative config:

```yaml
# In harness-config config.yaml
capabilities:
  # ... existing limits/telemetry/prompts/auth blocks ...
  mcp:
    stdio: { support: yes }
    sse: { support: partial, reason: "Depends on Claude Code version" }
    streamable_http: { support: no }
    project_scope: { support: yes }
```

This mirrors the existing `capabilities` shape in `HarnessAdvancedCapabilities` (already YAML-tagged in Phase 1). Adding an `MCP HarnessMCPCapabilities` field to that struct allows existing capability query paths to expose MCP support without new Go logic per harness.

### Manifest Input Staging

`ContainerScriptHarness.Provision()` stages the merged `MCPServers` map as `inputs/mcp-servers.json`:

```json
{
  "chrome-devtools": {
    "transport": "stdio",
    "command": "chrome-devtools-mcp",
    "args": ["--headless", "--browser-url", "http://localhost:9222"],
    "env": {},
    "scope": "global"
  }
}
```

The manifest `inputs` block gains an `mcp_servers` entry:

```json
{
  "inputs": {
    "instructions": "$HOME/.scion/harness/inputs/instructions.md",
    "system_prompt": "$HOME/.scion/harness/inputs/system-prompt.md",
    "telemetry": "$HOME/.scion/harness/inputs/telemetry.json",
    "auth_candidates": "$HOME/.scion/harness/inputs/auth-candidates.json",
    "mcp_servers": "$HOME/.scion/harness/inputs/mcp-servers.json"
  }
}
```

Following the pattern established in Phases 4 and 5 for auth-candidates and telemetry: `provision.py` should read `mcp-servers.json` **by the well-known path** (`$HOME/.scion/harness/inputs/mcp-servers.json`) rather than from `manifest.Inputs.MCPServers`, because the manifest is written during `Provision()` but the input file is written by a separate staging call. A missing or empty file should be treated as "no MCP servers to configure" (not an error).

### Extended `config.yaml` for Declarative MCP Mapping

For harnesses with a straightforward JSON-merge MCP config format (Claude, Gemini), the harness `config.yaml` can declare the mapping to eliminate boilerplate in `provision.py`:

```yaml
# In harness-config config.yaml
mcp:
  global_config_file: .claude.json              # File for global-scope servers
  global_config_path: mcpServers                # JSON merge path (dot-separated)
  project_config_file: .claude.json             # File for project-scope servers (may be same)
  project_config_path: "projects.{workspace}.mcpServers"  # {workspace} is substituted
  transport_field: type                         # Field name in the native server object
  transport_map:                                # Scion transport name → native value
    stdio: stdio
    sse: sse
    streamable-http: streamable-http
```

The shipped `scion_harness.py` helper module (Phase 7) should include an `apply_mcp_servers(config, mcp_servers_path)` function that reads this declarative mapping and performs the merge, so harness `provision.py` scripts can handle MCP in a few lines rather than reimplementing JSON merge logic per harness.

## Integration with Decoupled Harness Implementation

> **Note:** The [Decoupled Harness Implementation](decoupled-harness-implementation.md) has an initial implementation through Phase 5 (OpenCode and Codex migrated to container-script, Phases 1–5 complete as of 2026-04-26). MCP provisioning implementation is deferred until this work is considered stable enough to extend (Phase 6 or Phase 7 scope).

### What Changed from the Original Design

The original section above was drafted before the decoupled harness implementation was built. Several assumptions in that draft are now superseded:

| Original assumption | Actual implementation |
|---|---|
| Scripts execute on the host during provisioning | Scripts execute **inside the container** via `sciontool harness provision` in a `pre-start` lifecycle hook |
| A separate `provision-mcp` command in the manifest | No sub-command routing — `provision` handles everything; MCP config is an input file |
| `MCPProvisioner` Go interface required | Not needed — staging + `provision.py` replaces it entirely |
| `ScriptHarness` reads config and may skip the script for simple merges | `ContainerScriptHarness` always stages; the script (or a helper function) handles the merge inside the container |
| Host Python dependency was a concern | Not applicable — scripts only run inside harness container images, which already ship Python |

### Implementation Path (Updated)

The schema work (defining `MCPServerConfig`, adding `mcp_servers` to `ScionConfig`, validation) can proceed independently at any time.

Provisioning implementation should be sequenced with the decoupled harness work:

1. **Schema only (can proceed now):** Add `MCPServerConfig` type, `mcp_servers` to `ScionConfig`, validation rules, and `mcp` capabilities block to `HarnessAdvancedCapabilities`. No staging, no provisioning. The field is parsed and stored.
2. **Staging (after Phase 5 is stable):** Add `mcp-servers.json` staging to `ContainerScriptHarness.Provision()`. Add `mcp_servers` input path to the manifest. No harness scripts yet — staging is a no-op from the harness's perspective.
3. **Helper module (Phase 7):** Include `apply_mcp_servers()` in `scion_harness.py`. Document the declarative `mcp:` block in `config.yaml`.
4. **Per-harness implementation:** As each harness's `provision.py` is written or updated (Claude, Gemini in Phase 6; community harnesses in Phase 7), add MCP server application using the helper. Update harness `config.yaml` with `capabilities.mcp` and `mcp` declarative mapping.
5. **Template cleanup:** Remove inline `mcpServers` from harness-config home files in templates. Templates use `mcp_servers` in `scion-agent.yaml` exclusively.

## Impact on Templates

### Example: `my-web-template` Before and After

**Before** — MCP servers defined in each harness-config's home files:

```
.scion/templates/my-web-template/
  scion-agent.yaml                        # services only
  harness-configs/
    claude-web/
      home/.claude.json                   # mcpServers: chrome-devtools
    gemini-web/
      home/.gemini/settings.json          # mcpServers: chrome-devtools
    opencode/
      home/.config/opencode/opencode.json # no MCP config (no support)
```

**After** — MCP servers defined once in `scion-agent.yaml`:

```
.scion/templates/my-web-template/
  scion-agent.yaml                        # services + mcp_servers
  harness-configs/
    claude-web/
      home/.claude.json                   # no mcpServers (provisioned from template)
    gemini-web/
      home/.gemini/settings.json          # no mcpServers (provisioned from template)
    opencode/
      home/.config/opencode/opencode.json # no MCP config (harness warns)
```

Updated `scion-agent.yaml`:
```yaml
default_harness_config: claude-web

services:
  - name: chromium
    command: ["chromium", "--headless", "--no-sandbox", "--remote-debugging-port=9222", "--remote-debugging-address=0.0.0.0"]
    restart: always
    ready_check:
      type: tcp
      target: "localhost:9222"
      timeout: "10s"

mcp_servers:
  chrome-devtools:
    transport: stdio
    command: chrome-devtools-mcp
    args: ["--headless", "--browser-url", "http://localhost:9222"]
```

## Open Questions

### Q1: Harness-Config Level MCP Overrides

Should harness-configs be able to override or extend the template-level `mcp_servers`? For example, a harness-config might need to add harness-specific arguments or environment variables to an MCP server.

**Options:**
- **A**: No overrides. Template `mcp_servers` are authoritative. Harness-specific adjustments go in `provision.py`.
- **B**: Harness-config `config.yaml` can define its own `mcp_servers` block that merges with (and overrides) the template's. Follows existing config merge precedence.
- **C**: Harness-config can only *remove* servers (via an exclude list), not add or modify.

**Recommendation:** Option B. It follows the existing merge pattern where harness-config specializes template-level config, and it keeps the door open for harness-specific MCP arguments without requiring a script.

### Q2: MCP Server Dependencies on Services

Should there be a formal mechanism to declare that an MCP server depends on a service being healthy before it starts?

**Options:**
- **A**: No formal mechanism. Template authors document the dependency. The service `ready_check` ensures it's running; the MCP server may fail and retry.
- **B**: Add `depends_on: <service-name>` to `MCPServerConfig`. Provisioning validates the referenced service exists. Runtime could delay MCP server registration until the service is healthy (if supported by the harness).

**Decision:** No formal mechanism. Template authors document the dependency. The service `ready_check` ensures the sidecar is running before the agent starts; harnesses handle MCP connection errors gracefully.

### Q3: MCP Servers That Are Also Services

Some MCP servers run as long-lived HTTP services (SSE or streamable-http transport). Should these be implicitly added to the `services` list for lifecycle management by sciontool?

**Options:**
- **A**: Keep services and MCP servers fully separate. If an MCP server needs lifecycle management, define it in both places.
- **B**: SSE/HTTP MCP servers are automatically registered as sciontool services with sensible defaults (restart: on-failure, ready_check based on URL).
- **C**: Add an optional `service` sub-block to `MCPServerConfig` for SSE/HTTP servers that opts into sciontool management.

**Decision:** Keep them separate. SSE/HTTP MCP servers that need lifecycle management are defined in the `services` block explicitly. No implicit registration.

### Q4: Handling Unsupported Transports

When a template defines an MCP server with a transport the active harness doesn't support, what should happen?

**Options:**
- **A**: Warn at provisioning time. The MCP server is silently skipped.
- **B**: Error at provisioning time. Fail the agent creation.
- **C**: Warn at provisioning time but still create the agent. Log which MCP servers were skipped and why.

**Decision:** Delegated to the harness `provision.py` script. Since MCP provisioning runs as part of the decoupled harness configuration (which may occur after agent creation), agent creation failure is not an option. The `provision.py` script is responsible for handling unsupported transports — it can warn, skip, or error at its discretion. This is a natural consequence of the decoupled harness execution model.

### Q5: Runtime MCP Server Management

Should scion provide any runtime management of MCP servers (start, stop, health check) beyond what the harness natively provides?

**Decision:** Config only. Scion writes the MCP server configuration into the harness's native format and stops there. The harness owns MCP server lifecycle. Sidecar process management goes through the `services` block.

## Summary

This design introduces a universal `mcp_servers` block in `scion-agent.yaml` that lets template authors define MCP server configurations once, in a harness-agnostic format. The provisioning layer translates these definitions into each harness's native config format. The schema supports stdio, SSE, and streamable-http transports, covers the common configuration surface area (command, args, env, URL, headers, scope), and integrates cleanly with the existing `services` block for sidecar dependencies.

Implementation is deferred until the [decoupled harness implementation](decoupled-harness-implementation.md) is complete, at which point MCP provisioning becomes a natural `provision-mcp` command in `provision.py` scripts — or, for the common case, a declarative mapping in `config.yaml`.
