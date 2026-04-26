# Decoupled Harness Implementation: Script-Based Provisioning

## Motivation

Today, every harness implementation lives as compiled Go code inside the scion binary (`pkg/harness/`). Each harness performs a similar set of operations — writing config files, injecting auth credentials, rewriting settings JSON/YAML/TOML — but the specifics are unique per harness. This means:

1. **Adding a new harness requires Go expertise and a fork/PR** — even though the harness logic is mostly file manipulation and string templating, not systems programming.
2. **The binary grows with every harness** — Claude, Gemini, OpenCode, Codex each add ~300-500 lines of Go code for what is essentially JSON/YAML/TOML rewriting.
3. **The plugin system (`pkg/plugin/`) is heavyweight** — hashicorp/go-plugin requires building a separate Go binary, RPC serialization, and process lifecycle management. This is appropriate for long-running services (message brokers) but overkill for file-writing provisioning logic.
4. **Harness images already ship a Python interpreter** — every scion container image includes Python for `sciontool` and utility scripts. A harness provisioning script would have zero additional dependencies.

The core insight: **harness provisioning is inherently a scripting problem, not a systems programming problem.** The operations are: read config, write files, template strings, move things into place. This is what scripting languages excel at.

## Proposal

Replace the compiled Go harness implementations with **Python provisioning scripts** that live inside each harness-config directory, but do **not** execute those scripts on the host or Runtime Broker. The scion binary resolves declarative metadata, stages the harness bundle and manifests into the agent home, and `sciontool init` executes the script inside the agent container through the lifecycle hook system before launching the harness process.

### Review Critique: Assumptions to Make Explicit

The proposal is viable, but the first implementation should be treated as a controlled migration rather than a direct replacement. The current design depends on several assumptions that need to be explicit in the plan:

1. **Harness implementation selection needs more context than `harness.New(name)` currently receives.** Today the factory accepts only the harness type string (`claude`, `gemini`, etc.). A script-backed harness needs the resolved harness-config directory path, the parsed `config.yaml`, and the template/grove/global precedence context. This requires a new resolver/factory API; it cannot be implemented only inside the existing `New(harnessName string)` function.
2. **Existing installations already have seeded harness-config directories.** Adding `provision.py` to embedded defaults is not enough. Machines with `~/.scion/harness-configs/<name>/` already present need a safe upgrade path that adds missing files and schema fields without clobbering user edits.
3. **Hub-distributed harness configs become executable code, but execution must be contained.** `scion harness-config sync/pull` currently moves configuration artifacts. With scripts, the same path can distribute executable code to brokers. Brokers should stage that code into agent containers, never execute it on the broker host, and still need artifact hashes, provenance, and operator policy for which scripts may be staged.
4. **Auth preflight is not only `ResolveAuth()`.** Broker dispatch currently uses compiled helper logic such as required auth env keys, required file secrets, and file-secret auth type detection before `ResolveAuth()` runs. Container-script harnesses need declarative metadata for these preflight decisions, or remote dispatch will keep needing Go changes for every new harness.
5. **Seeding currently maps most embedded files under `home/`.** `provision.py`, `dialect.yaml`, schemas, and script fixtures must be copied to the harness-config root and later staged under `agent_home/.scion/harness/`, not into arbitrary locations in the agent home. The seed/copy path needs an explicit top-level file allowlist.
6. **Container images, not hosts, must satisfy interpreter dependencies.** The script execution target is the agent container. The relevant requirement is that the selected harness image contains the interpreter declared by `provisioner.command`, usually Python 3.
7. **Script activation must be explicit.** During migration, merely finding a `provision.py` should not silently change behavior for a built-in harness. Use a config field such as `provisioner.type: container-script` plus `interface_version` so upgrades can stage scripts without activating them unexpectedly.
8. **Pre-start scripts cannot mutate the parent process environment directly.** `sciontool init` must define a contract for hook-produced overlays, for example a JSON env file and generated harness config files, then load those overlays before starting the child harness process.

### What Moves Out of Go

| Current Go Method | Replacement |
|---|---|
| `Provision()` | Host-side staging plus container-side `provision.py provision` in the `pre-start` lifecycle hook |
| `InjectAgentInstructions()` | Stage instruction content; container-side script writes to the harness-native location |
| `InjectSystemPrompt()` | Stage system prompt content; container-side script writes or downgrades it |
| `ResolveAuth()` | Declarative host/broker preflight plus container-side auth selection from staged candidate secrets |
| `ApplyAuthSettings()` | Container-side script updates native harness config from staged/resolved auth |
| `ApplyTelemetrySettings()` | Container-side script writes telemetry config (e.g., Codex TOML) from staged effective telemetry |

### What Stays in Go

| Concern | Why it stays |
|---|---|
| `Name()`, `DefaultConfigDir()`, `SkillsDir()`, `GetInterruptKey()`, `GetEmbedDir()` | Static metadata — declared in `config.yaml`, not logic |
| `GetCommand()` | Simple command construction — declarable in `config.yaml` |
| `GetEnv()` | Simple env var mapping — declarable in `config.yaml` |
| `AdvancedCapabilities()` | Capability advertisement — declarable in `config.yaml` |
| `HasSystemPrompt()` | Simple file existence check — can be derived from config |
| Auth gathering, validation, overlay, and secret projection into the container | Cross-cutting concern shared by all harnesses (`auth.go`) |
| Required auth env/file preflight | Must run before broker dispatch; move harness-specific tables into declarative config, but keep enforcement in Go |
| Staging harness bundle, manifests, and lifecycle hook wrapper | Trusted host/broker code copies data into the agent home; it does not execute untrusted scripts |
| Loading hook-produced env overlays before launching the child process | `sciontool init` owns process supervision and child environment construction |
| Container launch, volume mounting, image resolution | Runtime layer (`pkg/runtime/`, `pkg/agent/run.go`) |
| Template/harness-config loading and merging | Config layer (`pkg/config/`) |

## Design

### Harness-Config Directory Structure (Extended)

```
~/.scion/harness-configs/claude/
  config.yaml              # Declarative metadata (existing, extended)
  provision.py             # Provisioning script (NEW)
  home/                    # Base home directory files (existing)
    .bashrc
    .claude/
      settings.json
```

### Extended `config.yaml` Schema

The existing `config.yaml` fields are preserved. New fields capture metadata that is currently returned by Go methods. The schema should live with the existing versioned settings/config schema so `scion config` and `harness-config` commands can validate it.

```yaml
# Existing fields
harness: claude
image: scion-claude:latest
user: scion

# Script activation. Presence of provision.py alone is not enough; this field
# prevents a staged upgrade from changing behavior unexpectedly.
provisioner:
  type: container-script       # builtin | container-script | plugin | generic
  interface_version: 1
  command: ["python3", "/home/scion/.scion/harness/provision.py"]
  timeout: 30s
  lifecycle_events:
    - pre-start                # required for provisioning before harness launch
  required_image_tools:
    - python3

# New declarative metadata (replaces simple Go getters)
config_dir: .claude                # DefaultConfigDir()
skills_dir: .claude/skills         # SkillsDir()
interrupt_key: Escape              # GetInterruptKey()
instructions_file: .claude/CLAUDE.md   # Target for InjectAgentInstructions()
system_prompt_file: .claude/system-prompt.md  # Target for InjectSystemPrompt()
system_prompt_mode: native         # native | prepend_to_instructions | none

# Command construction (replaces GetCommand())
command:
  base: ["claude", "--no-chrome", "--dangerously-skip-permissions"]
  resume_flag: "--continue"
  task_flag: "--message"           # or null if task is positional
  task_position: after_base_args    # after_base_args | before_base_args | positional
  system_prompt_flag: "--system-prompt"

# Environment variables (replaces GetEnv())
env_template:
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1"
  SCION_AGENT_NAME: "{{ .AgentName }}"

# Capability advertisement (replaces AdvancedCapabilities()). Keep this shape
# aligned with api.HarnessAdvancedCapabilities rather than using booleans,
# because callers display support level and reason text.
capabilities:
  limits:
    max_turns: { support: yes }
    max_model_calls: { support: no, reason: "No model-count hook events" }
    max_duration: { support: yes }
  telemetry:
    enabled: { support: yes }
    native_emitter: { support: yes }
  prompts:
    system_prompt: { support: yes }
    agent_instructions: { support: yes }
  auth:
    api_key: { support: yes }
    auth_file: { support: no }
    oauth_token: { support: yes }
    vertex_ai: { support: yes }

# Broker/hub auth preflight metadata. This replaces the harness-specific
# switch statements in RequiredAuthEnvKeys, RequiredAuthSecrets, and auth type
# detection helpers.
auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["ANTHROPIC_API_KEY"]
    oauth-token:
      required_env:
        - any_of: ["CLAUDE_CODE_OAUTH_TOKEN"]
    auth-file:
      required_files:
        - name: CLAUDE_AUTH
          target_suffix: "/.claude/.credentials.json"
    vertex-ai:
      required_env:
        - any_of: ["GOOGLE_CLOUD_PROJECT"]
        - any_of: ["GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"]
      required_files:
        - name: gcloud-adc
          type: file
          description: "Google Cloud Application Default Credentials (ADC) file for vertex-ai authentication"
          alternative_env_keys: ["GOOGLE_APPLICATION_CREDENTIALS"]
          skipped_when_gcp_service_account_assigned: true
  autodetect:
    env:
      CLAUDE_CODE_OAUTH_TOKEN: oauth-token
      GOOGLE_APPLICATION_CREDENTIALS: vertex-ai
      GOOGLE_CLOUD_PROJECT: vertex-ai
    files:
      CLAUDE_AUTH: auth-file
      gcloud-adc: vertex-ai

# Hook dialect specification (from harness-plugin-challenges.md Tier 1)
dialect:
  event_name_field: event_type
  mappings:
    tool_use:
      event: tool-start
      fields:
        tool_name: .tool_name
    # ...
```

With this extended config, a **thin Go "container-script harness"** can implement the `api.Harness` interface from `config.yaml` data while staging complex operations (provisioning, auth resolution, settings rewriting) for `provision.py` to perform inside the container.

The concrete Go type can either import `pkg/config.HarnessConfigEntry` or move the shared struct to `pkg/api`. The implementation plan must choose one. If `pkg/harness` imports `pkg/config`, verify no import cycle is introduced by future changes; today `pkg/config` depends on `pkg/api`, not `pkg/harness`.

### Staged Container Bundle

The host or broker never invokes `provision.py`. Instead, `Provision()` stages a deterministic bundle into the agent home, which is already mounted into the container:

```text
agent-home/
  .scion/
    harness/
      config.yaml                 # resolved harness-config metadata
      provision.py                # copied from harness-config root
      dialect.yaml                # optional hook dialect spec
      manifest.json               # command-independent context
      inputs/
        instructions.md           # staged agent instructions, if present
        system-prompt.md          # staged system prompt, if present
        telemetry.json            # effective telemetry config, if present
        auth-candidates.json      # non-secret auth metadata and secret file refs
      secrets/
        ...                       # projected secret files or env-secret files, mode 0600
      outputs/
        env.json                  # generated by pre-start script and loaded by sciontool init
        resolved-auth.json        # generated by pre-start script for diagnostics/resume
        status.json               # optional script status/errors
    hooks/
      pre-start.d/
        20-harness-provision      # trusted wrapper generated by Scion
```

The generated hook wrapper is trusted Scion code. Its job is only to call the staged provisioner inside the container with a manifest path:

```sh
#!/bin/sh
set -eu
exec sciontool harness provision --manifest "$HOME/.scion/harness/manifest.json"
```

`sciontool harness provision` is a new subcommand that validates the manifest, verifies paths stay under `$HOME/.scion/harness` or `$HOME`, applies the timeout, executes `provisioner.command`, validates outputs, and writes failures to normal sciontool status/log channels. The lifecycle manager can keep its generic script hook model, but `sciontool init` should set `SCION_HOOKS_DIR=$HOME/.scion/hooks` for staged per-agent hooks or merge that directory with `/etc/scion/hooks`.

### Script Interface

The provisioning script runs inside the agent container. It receives context via a JSON manifest on **stdin** or via `--manifest <path>` from `sciontool harness provision`, and returns structured outputs by writing files under `agent_home/.scion/harness/outputs/`. Errors are signaled via non-zero exit code and stderr, which `sciontool` captures into the agent log/status.

#### Commands

**`provision`** — Called during agent creation (replaces `Harness.Provision()`)

```bash
sciontool harness provision --manifest "$HOME/.scion/harness/manifest.json"
```

Manifest:
```json
{
  "schema_version": 1,
  "command": "provision",
  "agent_name": "researcher",
  "agent_home": "/home/scion",
  "agent_workspace": "/workspace",
  "harness_bundle_dir": "/home/scion/.scion/harness",
  "harness_config": { /* full resolved HarnessConfigEntry after profile overrides */ },
  "scion_config": { /* full final ScionConfig from scion-agent.json */ },
  "inputs": {
    "instructions": "/home/scion/.scion/harness/inputs/instructions.md",
    "system_prompt": "/home/scion/.scion/harness/inputs/system-prompt.md",
    "telemetry": "/home/scion/.scion/harness/inputs/telemetry.json",
    "auth_candidates": "/home/scion/.scion/harness/inputs/auth-candidates.json"
  },
  "outputs": {
    "env": "/home/scion/.scion/harness/outputs/env.json",
    "resolved_auth": "/home/scion/.scion/harness/outputs/resolved-auth.json"
  },
  "platform": { "goos": "linux", "goarch": "amd64" }
}
```

The script performs whatever file operations are needed inside the mounted home directory. For Claude, this means updating `.claude.json` with workspace paths, writing `CLAUDE.md`, applying auth settings, and emitting an env overlay if the selected auth method requires environment variables for the child harness process.

**`inject-instructions`** — Write agent instructions to harness-specific location

This is not a separate host-side invocation. The host stages instruction content in `inputs/instructions.md`; the container-side `provision` command receives:

```json
{
  "schema_version": 1,
  "command": "provision",
  "inputs": {
    "instructions": "/home/scion/.scion/harness/inputs/instructions.md"
  },
  "target_file": ".claude/CLAUDE.md"
}
```

Note: `target_file` comes from `config.yaml` so most scripts can use a generic "write content to target" implementation. Custom scripts can override for harness-specific behavior (e.g., Claude's casing cleanup).

**`inject-prompt`** — Write system prompt to harness-specific location

Same staging pattern as `inject-instructions`, using `inputs/system-prompt.md` and `system_prompt_file` / `system_prompt_mode` from config.

**`resolve-auth`** — Select authentication method from available credentials

Auth selection also runs inside the container during `provision`. Host/broker code stages candidate auth metadata and projected secrets:

```json
{
  "schema_version": 1,
  "command": "provision",
  "auth_candidates": {
    "explicit_type": "api-key",
    "env_secret_files": {
      "ANTHROPIC_API_KEY": "/home/scion/.scion/harness/secrets/ANTHROPIC_API_KEY"
    },
    "file_secrets": {
      "gcloud-adc": "/home/scion/.config/gcloud/application_default_credentials.json",
      "CLAUDE_AUTH": "/home/scion/.claude/.credentials.json"
    },
    "metadata": {
      "google_cloud_project": "",
      "google_cloud_region": "",
      "gcp_metadata_mode": ""
    }
  }
}
```

Response file (`outputs/resolved-auth.json`):
```json
{
  "method": "api-key",
  "env_vars": {
    "ANTHROPIC_API_KEY": { "from_file": "/home/scion/.scion/harness/secrets/ANTHROPIC_API_KEY" }
  },
  "files": []
}
```

Env overlay file (`outputs/env.json`) consumed by `sciontool init` before launching the child process:

```json
{
  "ANTHROPIC_API_KEY": { "from_file": "/home/scion/.scion/harness/secrets/ANTHROPIC_API_KEY" }
}
```

`sciontool init` resolves `from_file` entries into environment variables in memory immediately before starting the child process. It should not write secret values back to logs or persistent JSON.

**`apply-auth`** — Update harness-native config files after auth resolution

This becomes part of the container-side `provision` command. The script modifies settings files in-place using the selected method (e.g., Claude writes API key fingerprint to `.claude.json`, Gemini writes `selectedType` to `settings.json`).

**`apply-telemetry`** — Reconcile telemetry configuration

This also becomes part of the container-side `provision` command. Harnesses like Codex read `inputs/telemetry.json` and write native OTEL configuration files such as the `[otel]` section in `config.toml`.

#### Error Handling

- Exit code 0: success
- Exit code 1: error — stderr is captured and surfaced to user
- Exit code 2: unsupported command — harness does not implement this operation. Go treats this as a no-op only for optional operations; required operations fail unless the declarative config provides a fallback.
- Output files must be valid JSON where applicable and are size-limited.
- Stderr is included in user-facing errors but should be truncated and scrubbed for obvious secret values.
- Every invocation runs with a context timeout from `provisioner.timeout`.
- The script runner sets a minimal environment plus explicit `SCION_*` context variables inside the container. It must not inherit broker host environment variables.
- `sciontool init` fails startup on required pre-start provisioning errors, because continuing would launch a misconfigured harness.

### Go-Side Implementation: `ContainerScriptHarness`

A single Go struct replaces individual host-side Go harness logic for script-capable harnesses, but it **stages files only**. It does not execute the harness script.

```go
// pkg/harness/container_script_harness.go

type ContainerScriptHarness struct {
    config             config.HarnessConfigEntry // Parsed and resolved config.yaml/settings entry
    scriptPath         string                    // Path to provision.py
    configDir          string                    // Path to harness-config directory
}

func (h *ContainerScriptHarness) Name() string {
    return h.config.Harness
}

func (h *ContainerScriptHarness) DefaultConfigDir() string {
    return h.config.ConfigDir  // New field from extended config.yaml
}

func (h *ContainerScriptHarness) GetCommand(task string, resume bool, baseArgs []string) []string {
    // Build from config.yaml command spec
    cmd := append([]string{}, h.config.Command.Base...)
    if resume && h.config.Command.ResumeFlag != "" {
        cmd = append(cmd, h.config.Command.ResumeFlag)
    }
    // ... task flag handling
    return cmd
}

func (h *ContainerScriptHarness) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
    manifest := ProvisionManifest{
        SchemaVersion:  1,
        Command:       "provision",
        AgentName:     agentName,
        AgentHome:     "/home/scion",
        AgentWorkspace: "/workspace",
        BundleDir:     "/home/scion/.scion/harness",
        HarnessConfig: h.config,
    }
    return h.stageContainerBundle(ctx, agentDir, agentHome, manifest)
}

func (h *ContainerScriptHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
    // Host-side result is an unresolved container auth plan. The actual
    // harness-native selection happens in the pre-start hook from staged
    // candidate secret files.
    return buildContainerAuthPlan(h.config.Auth, auth)
}

func (h *ContainerScriptHarness) stageContainerBundle(ctx context.Context, agentDir, agentHome string, manifest ProvisionManifest) error {
    // Copy provision.py/dialect.yaml/config.yaml to agentHome/.scion/harness,
    // write inputs and manifest.json, and generate .scion/hooks/pre-start.d/20-harness-provision.
    return nil
}
```

`ResolveAuth()` remains in the Go interface because current runtime launch code expects it. For container-script harnesses it should return a plan that causes Scion to project all candidate secrets needed by the declarative auth metadata, not a final harness-native auth decision. The final decision and native config rewrite happen inside the pre-start hook.

For env-based credentials, the plan should prefer secret-file projection over direct child environment injection. For example, a host or Hub `ANTHROPIC_API_KEY` becomes a `0600` file under `.scion/harness/secrets/ANTHROPIC_API_KEY`; the pre-start script selects it and emits `outputs/env.json`; `sciontool init` resolves that file into the child process environment in memory. This keeps the broker from executing harness code while also avoiding over-broad auth env injection before the harness script has selected a method.

`InjectAgentInstructions()`, `InjectSystemPrompt()`, and `ApplyTelemetrySettings()` should stage inputs under `.scion/harness/inputs/` rather than writing harness-native files directly. Existing built-in harnesses can keep direct behavior during migration.

### Updated Harness Resolution API

The current `harness.New(harnessName string)` cannot discover script harnesses because it does not receive the resolved harness-config directory. Add a second API and migrate provisioning/start call sites to it:

```go
type ResolveOptions struct {
    Name          string
    GrovePath     string
    TemplatePaths []string
    ProfileName   string
    Settings      *config.VersionedSettings
}

type ResolvedHarness struct {
    Harness       api.Harness
    ConfigName    string
    ConfigDir     *config.HarnessConfigDir
    Config        config.HarnessConfigEntry
    Implementation string // builtin | container-script | plugin | generic
}

func Resolve(ctx context.Context, opts ResolveOptions) (*ResolvedHarness, error) {
    hcDir, err := config.FindHarnessConfigDir(opts.Name, opts.GrovePath, opts.TemplatePaths...)
    if err != nil && opts.Name != "generic" {
        return nil, err
    }

    entry := config.HarnessConfigEntry{Harness: "generic"}
    if hcDir != nil {
        entry = hcDir.Config
    }
    if opts.Settings != nil {
        settingsEntry, _ := opts.Settings.ResolveHarnessConfig(opts.ProfileName, opts.Name)
        entry = mergeHarnessConfigEntries(entry, settingsEntry)
    }

    if entry.Provisioner.Type == "container-script" {
        h, err := newContainerScriptHarness(hcDir.Path, entry)
        if err != nil {
            return nil, err
        }
        return &ResolvedHarness{Harness: h, ConfigName: opts.Name, ConfigDir: hcDir, Config: entry, Implementation: "container-script"}, nil
    }

    if builtin := newBuiltin(entry.Harness); builtin != nil {
        return &ResolvedHarness{Harness: builtin, ConfigName: opts.Name, ConfigDir: hcDir, Config: entry, Implementation: "builtin"}, nil
    }

    if pluginManager != nil && pluginManager.HasPlugin(PluginTypeHarness, entry.Harness) {
        h, err := newPluginHarness(entry.Harness)
        if err != nil {
            return nil, err
        }
        return &ResolvedHarness{Harness: h, ConfigName: opts.Name, ConfigDir: hcDir, Config: entry, Implementation: "plugin"}, nil
    }

    return &ResolvedHarness{Harness: newDeclarativeGenericHarness(entry), ConfigName: opts.Name, ConfigDir: hcDir, Config: entry, Implementation: "generic"}, nil
}
```

Keep `harness.New(name)` as a compatibility shim for tests and legacy call sites, but production paths that already resolve harness-config names should use `Resolve()` so implementation selection is based on the actual config artifact. The first call sites to migrate are `pkg/agent/provision.go`, `pkg/agent/run.go`, `cmd/harness_config.go reset`, Runtime Broker dispatch/provision paths, and tests that assert built-in behavior.

During migration, the priority order should be:

1. **Explicit container-script harness** — resolved harness-config has `provisioner.type: container-script` and a compatible `interface_version`
2. **Built-in Go harness** — compiled implementation for legacy configs and fallback
3. **Go-plugin harness** — if no built-in handles the harness type, or if a future config explicitly selects `provisioner.type: plugin`
4. **Declarative generic** — config.yaml-only, no provisioning logic

This avoids accidental behavior changes when a release adds `provision.py` to a built-in harness-config directory but the local config has not opted in.

### Where Scripts Execute

Scripts execute **only inside the agent container** as part of `sciontool init` lifecycle processing, normally in the `pre-start` hook before the child harness process launches. Host and broker code may copy scripts and manifests into the mounted agent home, but must not invoke them.

This intentionally changes the execution model from today's compiled Go harness provisioning. The host still composes the agent home, writes `scion-agent.json`, projects secrets, and prepares the launch config; the container-side hook performs harness-native file rewrites and emits an env overlay that `sciontool init` consumes before starting the child process.

## Interaction with Existing Plugin System

The script-based approach and go-plugin approach serve different needs:

| Dimension | Container-Script Harness | Go-Plugin Harness |
|---|---|---|
| **Language** | Python (or any interpreter) | Go (or any language via gRPC) |
| **Complexity** | File manipulation, config rewriting | Complex logic, external service integration |
| **Distribution** | Drop files in harness-config directory; Scion stages them into the agent home | Build and install a binary |
| **Process model** | Container lifecycle hook before harness launch | Long-running RPC server |
| **When to use** | 90% of harnesses — "my CLI needs these config files" | Edge cases — auth flows requiring OAuth dance, custom API calls |

The go-plugin system remains available for cases that genuinely need compiled code or long-running processes. Container-script harnesses are the **recommended default** for new harnesses.

### Priority Order in Harness Resolution

Use the explicit priority order from the updated resolution API above. Do not key behavior on `provision.py` existence alone. A local or hub-pulled config must declare `provisioner.type: container-script` before Scion stages the script for container execution.

## Relationship to Sideloading

Scion already supports **binary sideloading** via `SCION_DEV_BINARIES` — mounting local binaries into the container at `/opt/scion/bin`. Combined with script-based harnesses, this enables a fully external harness workflow:

1. **Container image**: Use a generic base image (e.g., `scion-base`) or any image with Python + the target CLI
2. **Harness logic**: Provide `config.yaml` + `provision.py` in a harness-config directory
3. **Hook dialect**: Provide `dialect.yaml` for event normalization (per harness-plugin-challenges.md Tier 1)
4. **CLI binary**: Sideload the harness CLI binary into the container

This means a community contributor can add support for a new coding agent (e.g., Cursor, Aider, Continue) without:
- Writing any Go code
- Building a custom container image
- Understanding the scion plugin RPC system
- Forking the scion repository

## Existing Installation Upgrade Plan

Existing installations need a deliberate migration path because seeded harness-config directories are long-lived user-owned files.

### Upgrade Rules

1. **Do not mutate existing agent homes during upgrade.** Already-created agents keep their `home/`, `workspace/`, `scion-agent.json`, and `agent-info.json`. Container-script harnesses affect new provisioning and resume/start-time auth or telemetry reconciliation only after the harness-config is upgraded and explicitly opts in.
2. **Do not silently stage new scripts for built-in harnesses.** Ship scripts and extended config fields first, but keep built-in Go behavior unless the local harness-config has `provisioner.type: container-script`.
3. **Preserve user-owned fields.** User edits to `image`, `user`, `env`, `volumes`, `secrets`, `auth_selected_type`, and local files under `home/` must survive an upgrade.
4. **Update managed default fields additively.** Missing fields such as `config_dir`, `skills_dir`, `capabilities`, `auth`, and `command` can be merged into existing configs. Existing non-empty user values win.
5. **Back up before changing config files.** Any automatic upgrade that writes `config.yaml` creates `config.yaml.bak.<timestamp>` unless run in a dry-run mode.

### CLI Flow

Add a command rather than relying only on `scion init --machine --force`:

```bash
scion harness-config upgrade --dry-run
scion harness-config upgrade
scion harness-config upgrade claude --activate-script
```

Recommended behavior:

- `--dry-run` reports which harness-configs are legacy, which files would be added, which fields would be merged, and whether local modifications prevent automatic activation.
- Default upgrade adds missing top-level support files (`provision.py`, `dialect.yaml`, examples/schemas if present) and merges additive config fields, but does not overwrite existing `home/` files and does not set `provisioner.type: container-script` for built-in harnesses.
- `--activate-script` switches `provisioner.type` from `builtin` to `container-script` for a named harness after validating the script, image tool requirements, schema version, staging plan, and a fixture run inside a container.
- `--force` is reserved for resetting to embedded defaults and should retain today’s destructive semantics. It should not be the normal upgrade path.

`scion init --machine` can call the same upgrade planner and print a concise summary, but should not clobber existing custom harness-configs. This is a change from the current seeding behavior where `config.yaml` is always rewritten by `SeedHarnessConfig()`; the implementation should distinguish first-time seed, additive upgrade, and forced reset.

### File Seeding and Packaging Changes

Update harness-config seeding so files are placed intentionally:

| Source file in embed/harness-config | Destination |
|---|---|
| `config.yaml` | harness-config root |
| `provision.py` | harness-config root |
| `dialect.yaml` | harness-config root |
| `schema/*.json`, `examples/*`, `tests/fixtures/*` | same relative path under harness-config root |
| `home/**` | same relative path under `home/` |
| legacy single files such as `settings.json`, `config.toml`, `bashrc` | preserve current mapping during migration, then move embeds toward explicit `home/**` layout |

This avoids the current problem where unknown embedded files are mapped under the agent home by `mapEmbedFileToHomePath()`. For new script-capable harness-configs, prefer an explicit on-disk layout in embeds:

```text
pkg/harness/claude/embeds/
  config.yaml
  provision.py
  dialect.yaml
  home/
    .bashrc
    .claude/
      settings.json
```

### Hosted Hub and Broker Upgrade

Container-script harnesses affect hosted deployments in three places:

1. **Hub artifact format:** `harness-config sync` must include root-level scripts, dialect specs, schema files, and fixtures in the uploaded artifact. The Hub should store a content hash and expose the harness-config revision used for an agent.
2. **Broker cache/install:** Brokers should cache pulled harness-configs by revision, validate hashes before staging, and reject container-script harnesses if `allow_container_script_harnesses` is false in broker settings.
3. **Trust policy:** Brokers never execute harness scripts on the host. They may still need policy for which Hub-provided script bundles can be staged into agent containers, because those scripts can access the agent's projected secrets and workspace.

Existing Hub harness-configs without scripts remain valid. Agents dispatched with a legacy config use built-in harnesses if available or fail with an actionable error if the target broker does not support that harness type.

### Compatibility Matrix

| Scenario | Expected behavior |
|---|---|
| Existing local install, no upgrade | Built-in harnesses continue to work |
| Existing install, additive upgrade only | Config gains metadata and files; built-in behavior remains active |
| Existing install, script activated | New agents and resumed agents use `ContainerScriptHarness`; existing home files are not rewritten by upgrade, but container pre-start hooks may update harness-native config at launch |
| Broker receives legacy built-in harness-config | Uses built-in if compiled into broker, otherwise fails clearly |
| Broker receives container-script harness-config but image lacks required tools | Fails before or during launch with an image requirement error |
| Broker receives unapproved Hub container-script harness-config | Fails staging policy check; user sees approval requirement |

## Alternatives Considered

### Chosen Approach: Execute Scripts Inside Container via `sciontool init`

Run `provision.py` inside the container as part of `sciontool init`'s startup sequence, using a staged `pre-start` lifecycle hook.

**Pros:**
- Script has access to the exact runtime environment (same Python, same paths)
- No host Python dependency
- Keeps untrusted or Hub-distributed harness scripts off the broker host
- Can handle runtime-only configuration that depends on container paths, mounted secrets, or image-installed CLIs

**Costs and required changes:**
- Host-side provisioning must become staging: copy scripts, manifests, content, telemetry, and candidate auth material into the mounted agent home.
- `sciontool init` must run required pre-start hooks before launching the child process and fail startup if required harness provisioning fails.
- `sciontool init` must load hook-produced env overlays after pre-start hooks and before child launch.
- Runtime auth injection must support projecting candidate secret values as files rather than relying only on final env vars chosen before container launch.
- Broker dispatch still needs declarative auth preflight metadata so it can request the right secrets before the container starts.

**Verdict:** Accepted. This is the target architecture because broker-host script execution is not acceptable.

### Alternative B: Use Starlark Instead of Python

[Starlark](https://github.com/google/starlark-go) is a Python-like language embeddable in Go. Scripts would execute in-process with controlled capabilities.

**Pros:**
- No external Python dependency
- Sandboxed execution — script cannot access network, arbitrary filesystem, etc.
- Deterministic — no version skew between Python installations

**Cons:**
- Starlark is a restricted subset of Python — no `import`, no standard library, no `json` module without explicit host injection
- Writing a Starlark harness would require learning a new (albeit similar) language
- We'd need to expose filesystem operations, JSON parsing, TOML writing, etc. as Starlark built-in functions — essentially building a scripting SDK
- The scion container images already ship Python, and script execution is container-side, so Starlark no longer solves the primary security concern.

**Verdict:** Deferred. Starlark remains a possible future sandboxing layer, but it is not needed to avoid broker-host execution.

### Alternative C: Declarative-Only (No Scripts)

Extend `config.yaml` to be fully declarative — express all provisioning as templated file writes:

```yaml
provision:
  files:
    - target: .claude.json
      template: |
        { "projects": { "{{ .AgentWorkspace }}": {} } }
    - target: .claude/CLAUDE.md
      content_from: instructions
```

**Pros:**
- No external interpreter needed
- Easy to validate and test
- Configuration-as-code

**Cons:**
- Some provisioning logic is genuinely procedural — Claude's `.claude.json` merges with existing content, Codex's TOML rewriting removes and rebuilds sections, Gemini's settings update nested JSON paths
- A sufficiently powerful template language becomes Turing-complete (see: Helm charts)
- Auth resolution involves conditional logic that doesn't map cleanly to templates

**Verdict:** Partially adopted. Simple file targets (instructions, system prompt) can be declarative via `config.yaml` fields (`instructions_file`, `system_prompt_file`). Complex provisioning remains scripted. The Go implementation stages declarative inputs and the container-side script applies them.

### Alternative D: Keep Everything in Go, Improve Plugin Authoring

Double down on the go-plugin approach: improve the reference harness, add scaffolding, make it easier to write Go plugins.

**Pros:**
- Single technology stack
- Type safety across the boundary
- Existing infrastructure

**Cons:**
- Go expertise is a hard requirement for harness authors
- Plugin binaries must be compiled for the target platform
- The go-plugin RPC overhead is unnecessary for file-writing operations
- Most harness logic is 50-100 lines of "read JSON, modify field, write JSON" — this is scripting, not systems programming

**Verdict:** Go-plugin remains available for complex cases but is not the recommended path for typical harnesses.

## Open Questions

### Q1: Container Interpreter Dependency

Container-script harnesses require the declared interpreter in the selected harness image. Is this acceptable?

- **Local host / broker**: No Python requirement for harness scripts
- **Container images**: Must include `provisioner.required_image_tools`, usually `python3`
- **Mitigation**: Built-in Go harnesses remain available as fallback. The script approach is opt-in per harness-config.

**Recommendation:** Validate required image tools during `harness-config upgrade --activate-script` when a runtime image is locally available, and fail clearly during `sciontool harness provision` if the image does not contain the declared command.

### Q2: Script Versioning and Compatibility

How do we handle changes to the manifest schema?

- **Option A**: Version field in manifest (`"schema_version": "1"`), scripts check and fail gracefully
- **Option B**: Semantic versioning of the script interface, scripts declare compatibility
- **Option C**: Keep the manifest additive-only — new fields are optional, old scripts ignore them

**Recommendation:** Option C (additive-only) for simplicity. Include a `schema_version` field for future-proofing but don't enforce it initially.

### Q3: Script Testing

How do harness script authors test their `provision.py`?

- The JSON manifest format is self-contained — scripts can be tested with fixture JSON files
- A `scion harness test <name>` command could scaffold a temporary agent directory and run the script against it
- Integration tests can run the script in a temporary directory and verify the output files

### Q4: Migration Path for Built-in Harnesses

Should we migrate existing built-in harnesses (Claude, Gemini, etc.) to script-based implementations?

- **Yes**: Proves the approach, reduces binary size, simplifies maintenance
- **No**: Built-in harnesses work fine; only use scripts for new harnesses
- **Hybrid**: Migrate one harness (e.g., OpenCode, which has the simplest Provision) as a proof of concept, then evaluate

**Recommendation:** Hybrid, but keep built-in implementations through at least one release after script parity is proven. Migrate OpenCode first (no Provision logic, simple auth), then Codex (moderate complexity), then evaluate whether Claude and Gemini benefit enough to justify migration.

### Q5: Script Execution Security

Scripts execute inside the agent container with the same privileges as the harness process. Is additional sandboxing needed?

- Scripts are installed by the user/admin or pulled from the Hub into a harness-config artifact
- Broker-host execution is prohibited
- Container isolation limits host impact, but scripts can access the agent workspace and any secrets projected into the container
- Hub-sourced scripts still need provenance and staging policy

**Recommendation:** No extra sandboxing beyond the agent container for v1, but enforce "never execute on broker host", validate artifact hashes before staging, and allow broker policy to reject untrusted Hub-sourced script bundles.

### Q6: Declarative Fallback for Simple Harnesses

Some harnesses (like Generic) have no provisioning logic at all. Should the container-script harness handle the fully-declarative case (no `provision.py`) as well?

**Recommendation:** Yes. If `config.yaml` provides all necessary metadata and no `provision.py` exists, Scion should stage no pre-start provisioner and handle simple command/env/capability metadata declaratively. This subsumes the current `Generic` harness over time.

### Q7: Sciontool Integration

`sciontool init` currently runs generic lifecycle hooks but does not understand harness provisioning outputs. What should it add?

**Recommendation:** Add a focused `sciontool harness provision` subcommand plus env-overlay loading in `sciontool init`. Keep execution wired through lifecycle hooks rather than creating a second unrelated startup pipeline.

### Q8: Remote Script Trust and Approval

Should a broker stage a script harness-config pulled from the Hub automatically?

**Recommendation:** No for v1. Add broker settings such as:

```yaml
broker:
  allow_container_script_harnesses: false
  trusted_harness_config_publishers: []
```

Local harness-configs can be trusted by local filesystem ownership. Hub-distributed scripts should require operator approval, a trusted publisher, or signed artifact metadata before staging into an agent container.

### Q9: Required Auth Metadata Ownership

Should auth preflight metadata live entirely in `config.yaml`, or should `provision.py --describe-auth` return it dynamically?

**Recommendation:** Keep required env/file metadata declarative in `config.yaml`. Broker dispatch needs this information before the container exists, and static metadata is easier to validate, display, and sync through the Hub.

### Q10: Script Standard Library

Should each `provision.py` be standalone, or should Scion ship a helper library?

**Recommendation:** Ship a small helper module or copyable `scion_harness.py` with functions for manifest parsing, atomic JSON/TOML writes, path containment checks, and result formatting. Standalone scripts reduce dependency issues, but without helpers each harness will duplicate risky file-rewrite logic.

### Q11: Path Safety

Should scripts be allowed to write outside `agent_home`?

**Recommendation:** Default no for host paths because scripts do not run on the host. Inside the container, the manifest should describe allowed roots (`agent_home`, harness bundle, and maybe `agent_workspace` read-only unless explicitly requested). The `sciontool harness provision` wrapper should validate paths before invocation and validate output paths afterward.

### Q12: Factory Precedence for Plugins

Should go-plugin harnesses be allowed to override built-in harness names?

**Recommendation:** Keep current behavior for compatibility: built-ins win unless a resolved config explicitly selects `provisioner.type: plugin`. This prevents a globally installed plugin from changing behavior for `claude`/`gemini` unexpectedly.

## Implementation Plan

### Phase 0: Inventory, Golden Tests, and Migration Guardrails

**Goal:** Lock down current behavior before replacing implementations.

1. Add or refresh golden tests for current Claude, Gemini, Codex, OpenCode, and Generic behavior:
   - Command construction with task/resume/base args
   - Env generation
   - Instruction and system prompt injection
   - Provisioned file layouts
   - Auth resolution, required env keys, required file secrets, and auth type detection
   - Auth/telemetry apply hooks on resume/start
2. Add fixture-based tests for existing seeded `config.yaml` files so upgrade logic can prove it preserves user fields.
3. Add a feature flag or config gate so container-script staging can be enabled per harness-config without changing built-in behavior.
4. Define acceptance criteria: a script migration is complete only when the staged container hook produces byte-for-byte equivalent or intentionally documented output for each golden fixture.

### Phase 1: Extended Config Schema, Validation, and Upgrade Planner

**Status:** Complete as of 2026-04-25.

**Goal:** Make harness-configs expressive enough for declarative metadata and safe upgrade.

1. [x] Extend `HarnessConfigEntry` with `provisioner`, `config_dir`, `skills_dir`, `interrupt_key`, `instructions_file`, `system_prompt_file`, `system_prompt_mode`, `command`, `capabilities`, declarative `auth`, and optional dialect metadata.
2. [x] Update JSON/YAML schema files and config validation paths.
3. [x] Move or share structs carefully to avoid import cycles between `pkg/config`, `pkg/harness`, and `pkg/api`.
4. [x] Implement an additive harness-config upgrade planner with dry-run output and backups.
5. [x] Add `scion harness-config upgrade [name] [--dry-run] [--activate-script] [--force]`.
6. [x] Update `SeedHarnessConfig()` and `SeedHarnessConfigFromFS()` to copy root-level support files intentionally and to support explicit `home/**` layouts.
7. [x] Update `harness-config sync/pull` and Hub artifact handling to preserve scripts, dialects, fixtures, hashes, and revisions.

Implementation notes:

- Extended harness metadata lives in `pkg/config.HarnessConfigEntry` to avoid introducing a `pkg/harness -> pkg/config` dependency in Phase 1. Capability structs remain in `pkg/api` and now include YAML tags so the same shape can be used in config files and API responses.
- Standalone harness-config `config.yaml` files are validated by wrapping them into the existing settings v1 schema under `harness_configs._`. The schema now allows arbitrary non-empty harness names so external harness-configs such as `adk` remain valid, while provisioner/auth/command substructures are still validated.
- Embedded built-in harness configs now declare `provisioner.type: builtin`. This deliberately stages declarative metadata without activating script behavior.
- The additive upgrade planner merges missing fields recursively and treats existing non-empty user values as authoritative. Any write to `config.yaml` creates `config.yaml.bak.<timestamp>` first. `--dry-run` produces the same action plan without touching files.
- `--activate-script` is name-scoped and refuses to run unless `provision.py` exists either in the current harness-config or embedded defaults. It only flips `provisioner.type`; actual script staging/execution is deferred to Phase 2.
- Seeding now has an explicit root-support-file allowlist for `provision.py`, `dialect.yaml`, `schema*/`, `examples/`, and `tests/fixtures/`, and supports explicit `home/**` layouts. Legacy single-file embeds still map to their existing home locations for compatibility.
- `harness-config sync/pull` already walked and transferred the full harness-config directory with per-file hashes and an aggregate content hash, so root scripts/dialects/fixtures are preserved by the existing artifact path once seeding stops hiding them under `home/`.

### Phase 2: ContainerScriptHarness Staging and Resolver API

**Status:** Complete as of 2026-04-25.

**Goal:** Ship the `ContainerScriptHarness` Go implementation behind an explicit opt-in, with no host-side script execution.

1. [x] Add `harness.Resolve(ctx, ResolveOptions)` and migrate production call sites away from raw `harness.New(name)` where resolved harness-config context is available.
2. [x] Implement `ContainerScriptHarness` in `pkg/harness/container_script_harness.go`:
   - All simple getters read from extended `config.yaml`
   - `Provision()` stages the harness bundle, manifests, inputs, and trusted lifecycle hook wrapper under `agent_home/.scion/`
   - `ResolveAuth()` returns a container auth plan that projects candidate secrets, not a final harness-native decision
   - `ApplyAuthSettings()` and `ApplyTelemetrySettings()` stage inputs for the pre-start hook rather than editing native files on the host
   - `InjectAgentInstructions()` and `InjectSystemPrompt()` stage content under `.scion/harness/inputs/`
3. [x] Add `sciontool harness provision`:
   - Executes only inside the container
   - Validates manifest paths and schema
   - Applies timeout from `provisioner.timeout`
   - Runs with a minimal environment plus explicit `SCION_*` context variables
   - Validates generated `outputs/env.json` and `outputs/resolved-auth.json`
   - Scrubs secrets from stderr/status/log output
4. [x] Add declarative generic harness behavior for config-only harnesses.
5. [x] Add comprehensive tests for `ContainerScriptHarness` staging and `sciontool harness provision` with mock scripts, invalid JSON, timeout, unsupported command, stderr, missing container interpreter, and missing script cases.
6. [x] Keep `harness.New(name)` as a legacy/test shim.

Implementation notes:

- `pkg/harness` now imports `pkg/config` so `ContainerScriptHarness` can use the canonical `HarnessConfigEntry`. Phase 1 explicitly avoided this dependency by keeping the new struct in `pkg/config`. To prevent the resulting test-time import cycle (some `pkg/config` tests use real `harness.Codex` / `harness.OpenCode` for their embedded FS), three of those tests moved to a sibling `package config_test` file (`pkg/config/harness_config_external_test.go`). No production code changed direction; the cycle only appears when `package config` test files import `pkg/harness`.
- `harness.Resolve` is the new constructor. Priority order matches the design: explicit `provisioner.type: container-script` → built-in (`claude`/`gemini`/`opencode`/`codex`) → registered go-plugin → declarative-generic → legacy `Generic`. The declarative-generic path activates only when `config.yaml` carries declarative metadata (e.g. `command.base`, `capabilities`, `env_template`, `config_dir`, `skills_dir`); a bare entry still falls through to `Generic` so passthrough auth keeps working.
- `harness.New(harnessName)` is preserved unchanged as a compatibility shim. The `cmd/harness_config.go reset` command intentionally keeps using it: `reset` semantics restore the *embedded* defaults of the underlying built-in harness, which a container-script wrapper cannot provide. The reset path therefore skips `Resolve` by design.
- Production call-site migration covers the two paths the design called out as critical: `pkg/agent/provision.go` (skills + provisioning) and `pkg/agent/run.go` (start path). Both pass the resolved harness-config name plus template paths and settings through `Resolve`. When `Resolve` errors (e.g. config dir missing in unusual states), the start path falls back to `harness.New` with a debug log so existing flows do not regress.
- `ContainerScriptHarness.Provision()` always stages the bundle under `agent_home/.scion/harness/` and writes a trusted hook wrapper at `agent_home/.scion/hooks/pre-start.d/20-harness-provision`. The wrapper invokes `sciontool harness provision --manifest "$HOME/.scion/harness/manifest.json"`. The manifest's path fields encode `$HOME/.scion/harness/...` literally (not the host path) so the same manifest is correct inside any container whose `$HOME` matches the agent user.
- `ResolveAuth()` returns `Method: "container-script"` and surfaces non-secret hints (e.g. `SCION_HARNESS_SELECTED_AUTH`) plus any present credentials so the runtime's existing secret projection still works. The final harness-native auth decision happens later in the container-side script. This deliberately preserves today's runtime behavior (env vars and file mappings are still injected at container launch) while the script-driven selection from `outputs/resolved-auth.json` is introduced in Phase 2.5.
- `ApplyAuthSettings` and `ApplyTelemetrySettings` write JSON inputs under `agent_home/.scion/harness/inputs/`. They are invoked from the existing `AuthSettingsApplier` / `TelemetrySettingsApplier` interfaces, so callers that already check those interfaces pick up the staging behavior automatically.
- `sciontool harness provision` lives in `cmd/sciontool/commands/harness.go`. To avoid pulling `pkg/harness` (and through it `pkg/config`) into the in-container binary, the manifest type is duplicated as a minimal `containerProvisionManifest` with just the fields sciontool needs to validate and dispatch. Schema versions are gated (rejects > 1 today). The runner enforces an absolute `--manifest` path, refuses any path in inputs/outputs that escapes `$HOME` or the bundle root, runs with a minimal environment that propagates only `HOME`, `PATH`, `LANG`, `TZ`, and `SCION_*`, applies `provisioner.timeout` (default 30s), and validates that `outputs/env.json` / `outputs/resolved-auth.json` are valid JSON ≤ 1 MiB. Exit code 2 is reported as "unsupported command" per the design, exit code 1 (or any non-zero) surfaces stderr (truncated to 4 KiB and scrubbed using the `auth-candidates.json` value list).
- The lifecycle hook wrapper is generated 0755 via `os.WriteFile`; existing `pkg/sciontool/hooks/lifecycle.go` will only execute hooks with the executable bit set. `sciontool init` does not yet read `outputs/env.json` — that overlay loader is Phase 2.5 work and is intentionally out of scope here.
- Tests cover staging layout (bundle directories, manifest contents, hook wrapper contents/perms), env templating, command construction with task/resume/base flags, auth candidate staging, container-script vs builtin vs declarative-generic resolver dispatch, and seven distinct sciontool harness provision failure modes (manifest path escape, schema version, missing provisioner, timeout, invalid env JSON, script stderr propagation, exit code 2). Secret scrubbing has its own dedicated test.

### Phase 2.5: `sciontool init` Env Overlay Support

**Status:** Complete as of 2026-04-25.

**Goal:** Let pre-start provisioning affect the child harness process without mutating the parent process environment from a hook subprocess.

1. [x] Set or merge a per-agent hooks directory, e.g. `$HOME/.scion/hooks`, into lifecycle hook discovery.
2. [x] Run required pre-start harness provisioning before starting sidecar services or the child harness when the staged manifest marks it required.
3. [x] After pre-start hooks complete, read `$HOME/.scion/harness/outputs/env.json`.
4. [x] Resolve `{ "from_file": "..." }` entries into child environment variables in memory.
5. [x] Merge generated env with existing launch env using explicit precedence rules: CLI/runtime env > generated harness env > config defaults, unless the harness config declares otherwise.
6. [x] Fail startup if required env output is invalid or references missing secret files.

Implementation notes:

- `LifecycleManager.HooksDir` (single string) was replaced with `HooksDirs []string` and an `AddHooksDir(dir string)` mutator. `NewLifecycleManager` now parses `$SCION_HOOKS_DIR` as a colon-separated list (empty entries skipped) and falls back to `/etc/scion/hooks`. `sciontool init` calls `AddHooksDir(filepath.Join(agentHome, ".scion", "hooks"))` immediately after constructing the manager so per-agent staged hooks (e.g. the container-script wrapper at `$HOME/.scion/hooks/pre-start.d/20-harness-provision`) participate in standard discovery without leaking grove paths into in-container code.
- `runScriptHooks` walks every dir in order; within each dir, single-file forms (`<event>`, `<event>.sh`) execute first, then `<event>.d/` entries sorted lexically. The lexical sort is now explicit rather than relying on `os.ReadDir`'s contract, because the numeric-prefix convention (`10-foo`, `20-harness-provision`) demands deterministic order.
- "Required pre-start provisioning" is detected by looking for `agent_home/.scion/harness/manifest.json` and parsing its `harness_config.provisioner.type`. A `container-script` provisioner with `pre-start` in `lifecycle_events` (or no explicit `lifecycle_events` — the documented default) sets `Required=true`. Hook failures abort startup in that case; the agent transitions to `PhaseError` with a status message captured from the hook error rather than silently launching a misconfigured child. Built-in (`type: builtin`) manifests, or no manifest at all, keep today's "best-effort" pre-start semantics.
- Env overlay loading lives in `pkg/sciontool/hooks/envoverlay.go` rather than under supervisor or init.go because (a) supervisor must remain trivially testable without filesystem dependencies and (b) the hooks package already owns lifecycle-hook plumbing so the overlay parser sits next to its caller. The loader accepts either string values or `{"from_file": "<path>"}` objects, validates env-key syntax (alpha+underscore start, alnum/underscore tail), enforces a 1 MiB overlay cap, and a 64 KiB cap on each from_file referent. From_file paths must live within an explicit allowedRoots list (passed by init.go as `[harnessBundleDir, agentHome]`) so a malicious script cannot exfiltrate `/etc/shadow` into the child env. Trailing whitespace on from_file content is stripped — token files often ship with a trailing newline that breaks `Bearer` comparisons.
- The staged manifest encodes container paths as `$HOME/...` literals (matching how `pkg/harness/container_script_harness.go` writes `outputs.env`). `hooks.ResolveContainerPath` rewrites the `$HOME/` prefix using the runtime `agentHome` before the loader opens the file, so the manifest stays portable across host/container path differences.
- Env overlay merging follows the design's precedence rule: runtime env wins over harness overlay. The merge happens in `supervisor.Run()` after `SCION_EXTRA_PATH` expansion via a new `mergeEnvOverlay` helper that reads `EnvOverlay` from the supervisor `Config`. Existing keys in `cmd.Env` are never overwritten; overlay keys that don't conflict are appended in deterministic alphabetical order. This means a broker-supplied `ANTHROPIC_API_KEY` always beats the script-resolved one — matching the design's "CLI/runtime env > generated harness env > config defaults" hierarchy because container launch env vars carry both broker and CLI sources.
- The supervisor refactor is intentionally minimal: `Config.EnvOverlay map[string]string` and the merge call. The supervisor still does not import `pkg/sciontool/hooks` — overlay parsing happens in init.go and the resolved map is passed into `Config`. This keeps the supervisor unit tests independent of the JSON schema and from filesystem state.
- Tests cover: env overlay schema (string vs from_file, missing/escaping/oversized files, invalid keys, malformed JSON); manifest detection (no manifest, container-script default, explicit `lifecycle_events: [post-start]` opt-out, `builtin` type, malformed manifest); lifecycle multi-dir execution order (system before agent, scripts before handlers, lexical sort within `.d/`, missing dirs are skipped, failing scripts surface errors); supervisor merge semantics (runtime env wins, nil overlay is passthrough, deterministic order). Pre-existing supervisor and hook tests continue to pass unchanged.

### Phase 3: Auth Preflight and Hosted Policy

**Status:** Complete as of 2026-04-25.

**Goal:** Remove compiled auth preflight tables as an extension blocker and make hosted execution safe.

1. [x] Implement config-driven replacements for:
   - `RequiredAuthEnvKeys`
   - `RequiredAuthSecrets`
   - `DetectAuthTypeFromFileSecrets`
   - `DetectAuthTypeFromEnvVars`
   - `DetectAuthTypeFromGCPIdentity`
2. [x] Keep built-in tables as fallback for legacy configs during migration.
3. [x] Add broker setting `allow_container_script_harnesses` and enforce it before staging Hub-sourced scripts.
4. [x] Add Hub/broker artifact hash validation and include harness-config revision in agent metadata.
5. [x] Update Runtime Broker dispatch tests to cover legacy built-in configs, container-script configs, untrusted script staging, and image missing required tools.

Implementation notes:

- The five preflight functions now have config-driven counterparts in `pkg/harness/auth.go` (`RequiredAuthEnvKeysFromConfig`, `RequiredAuthSecretsFromConfig`, `DetectAuthTypeFromFileSecretsFromConfig`, `DetectAuthTypeFromEnvVarsFromConfig`, `DetectAuthTypeFromGCPIdentityFromConfig`) that take a `*config.HarnessAuthMetadata` and otherwise mirror the legacy signatures. `pkg/harness` already imported `pkg/config` after Phase 2, so no new dependency surface was introduced. A small helper `AuthMetadataAvailable(*HarnessConfigEntry) bool` lets callers decide whether to use the config-driven path or fall back to the compiled tables.
- The autodetect map in `HarnessAuthMetadata` is unordered, so the *FromConfig detectors use a deterministic precedence rule documented in `auth.go`: build the candidate set from present keys, return `""` (no override) if any candidate matches `default_type`, otherwise return the alphabetically-smallest non-default candidate. This matches the legacy compiled order for the existing built-in harnesses (`api-key` > `auth-file` > `oauth-token` > `vertex-ai`) without needing a new explicit-priority schema field. A future harness with non-monotonic preferences should pick auth type names that sort in its preferred order; if that turns out to be brittle in practice, an explicit `detect_priority` array can be added without a schema break.
- `RequiredAuthSecretsFromConfig` honors a new `required: true` flag on `HarnessAuthFileRequirement` rather than emitting every declared file. The legacy compiled `RequiredAuthSecrets` only flagged vertex-ai's `gcloud-adc` as a broker-must-supply secret; auth-file's `CLAUDE_AUTH`/`GEMINI_OAUTH_CREDS`/`CODEX_AUTH`/`OPENCODE_AUTH` were documentary (the user mounts a locally-resolved file). Marking the field explicit avoids over-flagging auth-file types as "missing secret" during preflight and preserves behavior parity with the compiled tables. Embedded vertex-ai entries for Claude and Gemini set `required: true`; the schema and JSON schema both add the field.
- The Gemini embedded `auth.autodetect` map was corrected as part of this phase: `GOOGLE_APPLICATION_CREDENTIALS` was previously mapped to `auth-file` (incorrect — Gemini's auth-file uses OAuth creds, not ADC) and `GEMINI_OAUTH_CREDS` was missing from `autodetect.files`. Both were aligned with the compiled detector behavior and `gemini_cli.go` resolution, plus `GOOGLE_API_KEY: api-key` was added to autodetect to match `RequiredAuthEnvKeys` test fixtures.
- The runtime broker handler (`pkg/runtimebroker/handlers.go::extractRequiredEnvKeys`) now resolves `authMeta` from the harness-config dir and from settings overrides during the same pass that resolves `harnessType`/`authType`, then calls `harness.AuthMetadataAvailable` to choose between the config-driven and compiled paths. Each of the five detection/requirement steps switches on `useConfigDriven` so a partial migration (config has an `auth:` block but the broker is older) keeps working: the broker prefers the metadata when present.
- The broker policy gate lives in `pkg/runtimebroker/harness_policy.go::evaluateHarnessConfigPolicy`. It reads the resolved `HarnessConfigEntry` and refuses container-script dispatches with HTTP 403 unless `ServerConfig.AllowContainerScriptHarnesses` is true. The check fires from `createAgent` *before* `buildStartContext`, so a refused dispatch never mounts grove state, downloads workspaces, or projects secrets. `lookupHarnessConfigForPolicy` reuses the same name-resolution logic as `extractRequiredEnvKeys` (grove path → settings path → settings entry) so the two flows agree on which harness-config a request targets.
- The setting flows end-to-end: `RuntimeBrokerConfig.AllowContainerScriptHarnesses` lives in `pkg/config/hub_config.go`, the V1 settings YAML mapping is in `pkg/config/settings_v1.go::V1BrokerConfig`, the JSON schema documents `broker.allow_container_script_harnesses` with env var `SCION_SERVER_BROKER_ALLOWCONTAINERSCRIPTHARNESSES`, and `cmd/server_foreground.go` plumbs the value into `runtimebroker.ServerConfig` at startup.
- Hub artifact hash validation is enforced inside `pullHarnessConfigFromHub` (`cmd/harness_config.go`). The pull is now two-phase: download all files into memory and hash-verify them against the manifest's per-file `Hash` before writing anything to disk, so a hash mismatch never leaves a partially-installed harness-config dir. `verifyHarnessConfigArtifactHash` in `cmd/harness_config_transfer.go` uses `transfer.HashBytes` for the comparison and skips files whose announced hash is empty (legacy artifacts pre-date file-level hashes — operators can detect missing hashes via the manifest's overall `ContentHash` printed after a successful sync).
- Existing hub mock tests were updated: `cmd/harness_config_hub_test.go` now announces the real SHA-256 of the canonical `"harness: codex\n"` payload via a top-level `configYAMLHashCodex` constant, so hash validation passes during `TestPullHarnessConfigFromHub_FallsBackToHubFileAPIForLocalStorageURLs`. The change was forced by Phase 3's tighter validation; the test previously used a placeholder string that the unverified pull silently accepted.
- `api.AgentInfo.HarnessConfigRevision` and `runtimebroker.AgentResponse.HarnessConfigRevision` carry the bundle revision back to the Hub. `pkg/config.ComputeHarnessConfigRevision(dirPath)` walks the dir, hashes each file, and combines the per-file hashes into a single `sha256:<hex>` revision string. The revision is stamped at agent provisioning time in both `pkg/agent/provision.go` and `pkg/agent/run.go` (the latter falls back to `harness.New` for bare types, in which case the revision is empty — there is no on-disk dir to hash). For Hub-distributed harness-configs the resulting revision should match the manifest `ContentHash` once the Hub side adopts the same combiner; for local-only configs it is still useful as a stable local audit value.
- Broker dispatch tests live in `pkg/runtimebroker/handlers_dispatch_test.go`. The helper `dispatchTestEnv` creates an isolated `$HOME` *and* cwd so the broker's harness-config search (which uses `config.GetGlobalDir()`) lands in the test-controlled tree. Tests cover: legacy built-in dispatch (allowed even when allow=false), container-script dispatch blocked by default with the standard `ErrCodeForbidden` envelope, container-script dispatch allowed when `allow=true`, dispatch with no harness-config (policy gate is a no-op), config-driven preflight using a custom `auth.types.api-key.required_env` (would surface the wrong key if the compiled fallback were still active), compiled-table fallback for configs without an `auth:` block, and the agent-response harness-config-revision pass-through. `TestDispatchMissingRequiredImageTools` documents that broker-side image-tool validation is intentionally deferred to `sciontool harness provision` (Phase 2) — the broker accepts the dispatch and the in-container script fails fast if the declared interpreter is missing. If a future broker-side preflight is added, that test should be updated to expect a pre-launch failure.

### Phase 4: Migrate OpenCode as Proof of Concept

**Status:** Complete as of 2026-04-25.

**Goal:** Validate the script approach with the simplest existing harness.

1. [x] Write `provision.py` for OpenCode harness (auth resolution only; no Provision logic).
2. [x] Write extended `config.yaml` for OpenCode with all declarative fields and auth preflight metadata.
3. [x] Run OpenCode golden tests against both built-in and script implementations.
4. [x] Enable `--activate-script` for OpenCode locally and in a controlled broker test, verifying the script runs only inside the container.
5. [x] Do not remove `pkg/harness/opencode.go` until script behavior is proven through at least one release cycle or until the project intentionally drops built-in fallback. (Built-in remains; activation is opt-in.)
6. [x] Document the script authoring experience: what was easy, what was painful, and what helper functions should be added.

Implementation notes:

- `provision.py` lives at `pkg/harness/opencode/embeds/provision.py` and is picked up by the existing seeding/upgrade pipeline (Phase 1's root-support-file allowlist already covers `provision.py`). Operators see the script appear under `~/.scion/harness-configs/opencode/provision.py` after a normal `scion init` or `harness-config upgrade` run, but the provisioner type stays `builtin` until they explicitly run `harness-config upgrade opencode --activate-script`. This is the staged-without-activated invariant Phase 1 promised — Phase 4 exercises it for the first time.
- The script is intentionally stdlib-only and ~200 lines. It does **not** attempt to re-implement OpenCode's CLI behavior; its job is to (a) read `inputs/auth-candidates.json`, (b) apply the same precedence as the compiled OpenCode harness (`SCION_HARNESS_SELECTED_AUTH` → `ANTHROPIC_API_KEY` → `OPENAI_API_KEY` → `~/.local/share/opencode/auth.json`), (c) write `outputs/resolved-auth.json` for diagnostics/resume, and (d) write an empty `outputs/env.json` because OpenCode does its own env precedence and the candidate keys are already projected at container launch. Failing to find any usable method exits 1 with an actionable message, mirroring `OpenCode.ResolveAuth`'s pre-launch failure.
- The script reads auth-candidates by **path**, not by trusting `manifest.Inputs.AuthCandidates`. The host's `Provision()` writes the manifest before `ApplyAuthSettings()` writes auth-candidates.json, so the manifest always points to "" for that input on first provision — by the time the in-container hook fires, the file exists on disk but the manifest is stale. Loading the file directly avoids reissuing the manifest after each Apply call. Codex/Claude provisioners should follow the same pattern.
- Authoring friction worth lifting into Phase 7's helper module: (a) container-path expansion (`~`, `$HOME`) and "file is mounted at this path" detection, (b) atomic JSON output with `os.replace`, (c) the schema-version check on inputs, (d) safe scrubbing helpers symmetric to `cmd/sciontool/commands/harness.go`'s stderr scrubber. Every harness will need these.
- The compiled harness puts task **before** baseArgs (`opencode --prompt <task> <args...>`). The Phase 1 OpenCode YAML had `task_position: after_base_args`, which produced `opencode <args...> --prompt <task>` and broke parity for any caller passing harness args. Phase 4's parity test caught this and the YAML was corrected to `before_base_args`. Lesson: every migrated harness needs an explicit `GetCommand` parity test exercising the no-task / task-only / task+baseArgs shapes; capability and getter parity alone won't catch positional bugs.
- Container-script `ResolveAuth` for OpenCode passes ALL candidates through (env vars + auth file mapping) instead of collapsing to one method. The runtime then projects every candidate into the container, and the in-container script picks one. This deliberately diverges from the compiled harness's "pick now, project only that one" contract — but it is required because the script can't see the container's launch env (sciontool harness provision strips most env vars for containment), so the host must surface every candidate to the script via `auth-candidates.json` (env-var names) and `Files` mappings (file paths). The chosen method is recorded in `outputs/resolved-auth.json` and is the value clients should display.
- The script is invoked by `sciontool harness provision`, which only inherits a minimal env (`HOME`, `PATH`, `LANG`, `TZ`, and `SCION_*` vars). The script therefore cannot read `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` from `os.environ` — it learns about their presence from `auth-candidates.json`'s `env_vars` list (names only). This is by design (containment), but it means scripts must never expect raw secret values in their env. Phase 7 docs should call this out prominently.
- Test coverage lives in `pkg/harness/opencode_parity_test.go` and exercises: seeding (provision.py at root, opencode.json under home), `--activate-script` flips `provisioner.type` and creates a backup, `Resolve`-driven dispatch picks the container-script wrapper after activation, ContainerScriptHarness vs OpenCode parity for Name/DefaultConfigDir/SkillsDir/InterruptKey/GetCommand/AdvancedCapabilities, Provision() stages provision.py byte-identically and emits the trusted hook wrapper, and two end-to-end script integrations that actually run python3 against synthetic manifests (one happy-path with OPENAI_API_KEY, one no-creds asserting the expected error string). Integration tests skip if python3 is unavailable so the suite stays portable.
- Broker-side activation is gated by Phase 3's `allow_container_script_harnesses` server setting; no new broker flow was needed for OpenCode. Operators who want script provisioning in hosted mode flip the broker setting and run `harness-config upgrade opencode --activate-script` against the broker's local harness-config dir (or sync an activated config from the Hub once the operator has opted in). The Phase 3 dispatch tests already cover the refusal path; Phase 4 did not add broker-specific tests because the script's behavior is identical regardless of dispatch source.
- The compiled `pkg/harness/opencode.go` is unchanged and remains the default. Removing it before Phase 5 (Codex parity) and Phase 6 (Claude/Gemini decision) would lose the parity oracle — keep it until at least one release after Phase 5 ships, per step 5 above.

### Phase 5: Migrate Codex

**Status:** Complete as of 2026-04-26.

**Goal:** Validate with a harness that has real provisioning complexity (TOML rewriting, auth file writing, telemetry reconciliation).

1. [x] Write `provision.py` for Codex covering:
   - `provision` / TOML config
   - `resolve-auth`
   - `apply-auth` / auth file behavior
   - `apply-telemetry` / TOML `[otel]` section reconciliation
2. [x] Write extended `config.yaml` for Codex.
3. [x] Run golden tests and parity tests against the existing `pkg/harness/codex.go` and `codex_config.go`.
4. [x] Verify telemetry settings precedence still matches `pkg/agent/run.go`: CLI override, template/config, settings, and env. (The compiled host-side precedence is unchanged; the script consumes the already-resolved `effectiveTelemetry` that `run.go` passes to `ApplyTelemetrySettings` and re-applies the same `SCION_*OTEL_*` env override rules locally.)
5. [x] Keep built-in fallback until parity and hosted broker behavior are proven. (Built-in remains; activation is operator opt-in via `harness-config upgrade codex --activate-script`.)

Implementation notes:

- `provision.py` lives at `pkg/harness/codex/embeds/provision.py` and is staged automatically by Phase 1's root-support-file allowlist. Operators see it appear at `~/.scion/harness-configs/codex/provision.py` after `scion init` or `harness-config upgrade`, but `provisioner.type` stays `builtin` until they explicitly run `harness-config upgrade codex --activate-script`. Same staged-without-activated invariant Phase 4 established for OpenCode.
- Phase 5 surfaced two latent bugs in the Phase 2 ContainerScriptHarness wrapper that OpenCode never tripped, both fixed in `pkg/harness/container_script_harness.go`:
  1. **Multi-token resume_flag.** `ResumeFlag` was appended as a single argv string. OpenCode's `--continue` is one token so parity worked; Codex's `resume --last` is two tokens, and the wrapper would have produced `[..., "resume --last"]` (a single bogus arg) instead of `[..., "resume", "--last"]`. Fixed by splitting `cmd.ResumeFlag` on whitespace in `GetCommand`. Documented in the field comment so callers know what they get.
  2. **Codex YAML positional/before_base_args mismatch.** The compiled harness emits `codex <base...> <task> <baseArgs...>`, so `task_position` must be `before_base_args` (same lesson the Phase 4 implementation note flagged for OpenCode). The original Phase 1 YAML used `positional`, which the wrapper conflates with `after_base_args` and would have produced `<base...> <baseArgs...> <task>`. Tests `TestCodexContainerScriptGetCommandParity` cover the four shapes (resume_no_task / task_only / task_with_base_args / no_task_with_base_args) so a future regression here cannot pass silently.
- **Secret value file staging.** Codex's defining difference from OpenCode: the harness reads its credential from `~/.codex/auth.json`, not from env, so the in-container script needs the actual API key VALUE — but `sciontool harness provision` strips secret env vars from the script's process for containment. Phase 5 closes that gap by extending `ContainerScriptHarness.ApplyAuthSettings`:
  - Each non-empty value in `resolved.EnvVars` is written to `agent_home/.scion/harness/secrets/<NAME>` (mode 0600).
  - The container path (`$HOME/.scion/harness/secrets/<NAME>`) is recorded in a new `env_secret_files` map inside `inputs/auth-candidates.json`.
  - The auth-candidates file (mode 0644) still contains only names and paths — never raw secret values. A regression test asserts the value string is absent from that file.
  - Env names are validated against a POSIX-conventional pattern (`[A-Za-z_][A-Za-z0-9_]*`) before being used as a filename, so a hostile caller cannot pass `../../etc/passwd` as an env name and direct the writer outside the secrets dir.
  This pattern is generic — any future harness that needs to materialize a secret into a native config file can use the same `env_secret_files` map without further Go changes. The OpenCode script ignores the map (it never needed values), so Phase 4 behavior is unchanged.
- **scrubSecrets blind spot closed.** `cmd/sciontool/commands/harness.go::scrubSecrets` previously only walked `auth-candidates.json`, but that file (post-Phase 5) intentionally does not contain raw secret values — only names and paths. So scrubbing was effectively a no-op for the values it most needed to redact. Phase 5 adds `loadStagedSecretValues` which reads the contents of every file under `$HOME/.scion/harness/secrets/`, so a script that accidentally echoes its decoded API key still gets `[REDACTED]` in surfaced stderr. The 8-char minimum and trailing-newline strip are preserved.
- **TOML reconciliation in Python without a TOML library.** Codex's compiled `reconcileConfig` does line-based TOML rewriting in Go because the file format mixes top-level keys, dotted-section headers (`[projects."/workspace"]`), and inline tables that no off-the-shelf round-trip writer in Python's stdlib handles. The script mirrors the Go behavior exactly: read the file, strip the existing `[otel]` block (and its preceding blank line), conditionally append a freshly-built `[otel]` section using the same exporter/headers/log_user_prompt resolution rules. The build-otel-section function uses identical TRUE/FALSE casing and identical header formatting (`"k" = "v"`, comma-joined, sorted) so a side-by-side diff between the compiled output and the script output is empty for the same input. Three integration tests assert this: telemetry-enabled (preserves a custom_key set by the user), telemetry-disabled (strips a pre-existing [otel] section), log_user_prompt include/exclude precedence (exclude beats include).
- **Manifest staleness pattern reused.** Like OpenCode (Phase 4 note), the Codex script reads `inputs/auth-candidates.json` and `inputs/telemetry.json` by *path*, not by trusting `manifest.Inputs.AuthCandidates` / `manifest.Inputs.Telemetry`. The manifest is written during `Provision()` while the inputs are written later by `ApplyAuthSettings` and `ApplyTelemetrySettings`, so the manifest's input-path entries are typically empty on first provision. Loading from the well-known path under `$HOME/.scion/harness/inputs/` works in both first-provision and resume scenarios.
- **API-key write parity quirk.** The compiled `Codex.ApplyAuthSettings` writes `{"auth_mode": "apikey", "OPENAI_API_KEY": "<value>"}` regardless of which env-var name was the source — Codex itself only reads the `OPENAI_API_KEY` field from `auth.json`. The script preserves this: when `env_key == "CODEX_API_KEY"`, the *value* still gets written under `OPENAI_API_KEY` in the JSON. Test `TestCodexProvisionScript_Integration_APIKey` asserts both the field name and the value while CODEX_API_KEY was the staged env var. Resolved-auth.json records the *original* env_var name (CODEX_API_KEY) so callers can display the actual source.
- **Test coverage:** `pkg/harness/codex_parity_test.go` has 13 test cases. Seeding (provision.py at root, config.toml under home/.codex/), `--activate-script` flow with backup, getter parity (Name/DefaultConfigDir/SkillsDir/InterruptKey/Capabilities), `GetCommand` parity (4 shapes including the resume-flag whitespace-split case), `Provision()` staging (script byte-identical, hook wrapper present), `ApplyAuthSettings` secret file staging (value present, perm 0600, candidate JSON does not leak value, invalid env names rejected), and 5 end-to-end Python integration cases (api-key with auth.json write, telemetry-enabled with TOML preservation, telemetry-disabled with [otel] strip, log_user_prompt filter include-only and exclude-overrides-include, no-creds error). Integration tests skip when `python3` is missing so the suite stays portable.
- **Compiled `pkg/harness/codex.go` and `codex_config.go` are unchanged and remain the default.** Phase 5 step 5 says to keep built-in fallback until parity is proven; per Phase 4 step 5's "at least one release after Phase 5 ships" guidance, removal should wait until Phase 6 has decided Claude/Gemini's path and any hosted broker rollout has settled. The compiled harness is also the parity oracle for any future YAML changes.

### Phase 6: Evaluate Claude and Gemini Migration

**Goal:** Decide whether to migrate the most complex harnesses.

1. Assess complexity of Claude's `.claude.json` manipulation, OAuth/auth-file handling, Vertex AI behavior, and API key fingerprint updates.
2. Assess complexity of Gemini's nested `settings.json` updates, OAuth/ADC/Vertex selection, and system prompt file behavior.
3. If complexity is manageable, migrate behind opt-in; if the Go code is clearer and safer, keep built-ins and use container-script harnesses only for external/community harnesses.
4. Document the decision criteria for future harness authors.

### Phase 7: Community Harness Template and Documentation

**Goal:** Make it easy for external contributors to add harnesses.

1. Create a template harness-config directory with annotated `config.yaml` and `provision.py`
2. Add `scion harness init <name>` command that scaffolds from the template
3. Add `scion harness test <name>` command for local testing
4. Write contributor documentation: "Adding a New Harness to Scion"
5. Integrate with declarative dialect spec (from harness-plugin-challenges.md Tier 1) so container-script harnesses can also declare their hook event mapping
6. Document operational guidance for brokers: script staging policy, artifact approval, image tool requirements, and rollback to built-in harnesses.

## Summary

Script-based harness provisioning extracts the "file manipulation" concern out of compiled Go and into Python scripts that live alongside harness configuration, while keeping script execution inside the agent container through `sciontool` lifecycle hooks. Host and broker code stage manifests, inputs, and secrets; `sciontool init` runs the harness provisioner before launch and consumes generated env/config overlays. The approach is additive — built-in Go harnesses and go-plugin harnesses continue to work — and can be adopted incrementally, one harness at a time.
