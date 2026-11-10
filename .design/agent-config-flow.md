# Agent Configuration Assembly: Current State Analysis

## Overview

Agent configuration in Scion is assembled from multiple sources across two distinct
lifecycle phases: **creation time** (provisioning) and **start time** (launching the
container). This document surveys the current flow, maps where each configuration
value originates, and identifies inconsistencies and improvement opportunities.

---

## Configuration Sources (Precedence Hierarchy)

Settings are loaded via Koanf (`pkg/config/koanf.go`) in this merge order:

1. **Embedded defaults** — `pkg/config/embeds/default_settings.yaml` (compiled into binary)
2. **Global settings** — `~/.scion/settings.yaml`
3. **Grove settings** — `.scion/settings.yaml`
4. **Environment variables** — `SCION_*` prefix (top-level settings keys only)

Each layer deeply merges into the previous one (the settings loader,
`loadVersionedLayers` in `pkg/config/settings_v1.go`; the former
`MergeSettings()` helper has been removed). Harness configs, profiles, and
runtimes are all individually deep-merged field-by-field rather than
replaced wholesale.

The resulting `Settings` struct contains:
- `ActiveProfile` / `DefaultTemplate` — scalars
- `Runtimes` — map of runtime configurations (docker, container, kubernetes)
- `Harnesses` — map of harness base configs (image, user, env, volumes)
- `Profiles` — map of profiles, each referencing a runtime and optionally containing
  env, volumes, tmux overrides, and per-harness overrides

---

## Phase 1: Agent Creation (Provisioning)

Entry: `cmd/create.go` → `agent.Provision()` → `agent.GetAgent()` → `agent.ProvisionAgent()`

### What happens at creation time

#### 1. Directory structure creation (`provision.go:103-111`)
```
.scion/agents/<agent-name>/
├── home/             # Agent's container home directory
├── workspace/        # Git worktree (if applicable)
├── prompt.md         # Empty task file
└── scion-agent.json  # Persisted agent config (the final merged ScionConfig)
```

#### 2. Workspace resolution (`provision.go:121-191`)
Three cases, evaluated in order:
- **Explicit `--workspace` flag** → use that path directly as a volume mount
- **Git repository** → create a worktree at `agents/<name>/workspace` with a branch named
  after the agent (or `--branch` if provided)
- **Non-git directory** → mount the project directory (or cwd for global grove)

#### 3. Template chain loading and home copy (`provision.go:193-227`)
- `config.GetTemplateChain(templateName)` resolves the template by searching:
  1. Project templates (`.scion/templates/<name>/`)
  2. Global templates (`~/.scion/templates/<name>/`)
  3. Remote URIs (GitHub, archive URLs, rclone)
- Template inheritance is supported via chains (base → override), though currently
  `GetTemplateChain()` returns a single-element chain (`templates.go:204-214`).
- For each template in the chain:
  - Copy `home/` directory contents into the agent's `home/`
  - Load `scion-agent.json` (or `.yaml`) from the template
  - Merge configs via `MergeScionConfig()`

#### 4. Settings harness/profile merge (`provision.go:229-249`)
If settings are available and the template specifies a harness:
- `settings.ResolveHarness(profileName, harnessName)` is called, which:
  1. Starts with the base harness config (image, user, env, volumes)
  2. Merges profile-level env and volumes
  3. Merges profile-level `harness_overrides[harnessName]` (image, user, env, volumes, auth)
- A settings-sourced `ScionConfig` is built from the resolved harness's env, volumes,
  and auth type
- **The template config is merged OVER the settings config**, giving templates higher
  priority than settings for env/volumes:
  ```go
  finalScionCfg = config.MergeScionConfig(settingsCfg, finalScionCfg)  // provision.go:247
  ```

#### 5. Workspace volume injection (`provision.go:251-258`)
If workspace resolution produced an external source path (non-worktree cases), a
`/workspace` volume mount is appended to the config's volumes list.

#### 6. Persisted files written (`provision.go:260-299`)
Two files are created:
- **`scion-agent.json`** (in agent root) — the merged `ScionConfig` (harness, env,
  volumes, image, model, command_args, kubernetes, gemini settings). Note: `Info` is
  excluded from this file via `json:"-"` tag.
- **`agent-info.json`** (in `home/`) — metadata (`AgentInfo`): name, template, profile,
  grove, status, requested image.

#### 7. Harness-specific provisioning (`provision.go:301-312`)
- `harness.New(harnessName).Provision()` is called
- For Claude (`harness/claude_code.go:96-159`): reads `.claude.json` from the agent home,
  updates the `projects` map to point to the container workspace path
- After harness provisioning, the `scion-agent.json` is **reloaded** to pick up any
  changes the harness wrote (e.g., env vars injected into the config file)

### What is persisted at creation time

| Field | Source | File |
|-------|--------|------|
| `harness` | Template `scion-agent.json` | `scion-agent.json` |
| `env` | Template merged with settings harness/profile env | `scion-agent.json` |
| `volumes` | Template + settings + workspace mount | `scion-agent.json` |
| `image` | Template `scion-agent.json` (if set there) | `scion-agent.json` |
| `model` | Template `scion-agent.json` | `scion-agent.json` |
| `command_args` | Template `scion-agent.json` | `scion-agent.json` |
| `detached` | Template `scion-agent.json` | `scion-agent.json` |
| `kubernetes` | Template `scion-agent.json` | `scion-agent.json` |
| `gemini.auth_selectedType` | Settings harness/profile auth type | `scion-agent.json` |
| `name`, `template`, `profile`, `grove` | CLI args / resolution | `agent-info.json` |
| `status` ("created") | Hardcoded | `agent-info.json` |
| `image` (if `--image` flag used) | CLI flag | `agent-info.json` |

---

## Phase 2: Agent Start (Runtime Launch)

Entry: `cmd/start.go` → `agent.Start()` (`run.go:17`)

### What happens at start time

#### 1. Config reload via `GetAgent()` (`provision.go:404-481`)
When starting an existing agent:
- Loads `agent-info.json` to recover the template name
- Loads `scion-agent.json` from the agent directory
- Reloads the template chain and re-merges: `template chain → agent's scion-agent.json`
- This means **the final config at start time reflects current template content**, not a
  snapshot from creation time

#### 2. Image resolution (`run.go:99-141`)
Resolved through a cascade:
1. Hardcoded default: `"gemini-cli-sandbox"` / `"root"`
2. Settings: `settings.ResolveHarness()` → `hConfig.Image`, `hConfig.User`
3. Template/agent config: `finalScionCfg.Image` (overrides settings)
4. CLI `--image` flag (ultimate override)

**This is a start-time concern** — the image is NOT read from the persisted
`scion-agent.json` env unless the template explicitly set it there.

#### 3. Tmux resolution (`run.go:113-127`)
Cascade:
1. Runtime config: `settings.Runtimes[profile.Runtime].Tmux`
2. Profile override: `settings.Profiles[profileName].Tmux`

This is purely a start-time setting derived from current settings.

#### 4. Auth discovery (`run.go:145-157`)
- If `opts.Auth` is provided (e.g., from Hub auth provider), use that
- Otherwise, harness-specific discovery: e.g., Claude reads `ANTHROPIC_API_KEY` from
  the host environment
- Auth is **never persisted** — always discovered at start time

#### 5. Environment variable assembly (`run.go:187-201`, `buildAgentEnv()` at `run.go:329-363`)
The final env is built by merging:
1. `finalScionCfg.Env` — from the persisted/re-merged agent config (template + settings
   harness env from creation time)
2. `opts.Env` (extra env from CLI) — includes `SCION_AGENT_NAME`, `SCION_TEMPLATE_NAME`,
   `SCION_BROKER_NAME`
3. All values go through `util.ExpandEnv()` for `${VAR}` substitution against the
   **host's current environment**

Empty values after expansion are dropped with warnings.

#### 6. Volume assembly (`run.go:244-255`)
Uses `finalScionCfg.Volumes` from the persisted/re-merged config, with dedup logic to
avoid double-mounting `/workspace` when it was already handled via worktree support.

#### 7. Container launch (`run.go:220-273`)
Builds `runtime.RunConfig` from all resolved values and calls `runtime.Run()`.

### What is resolved only at start time

| Config | Source | Why late-bound |
|--------|--------|----------------|
| **Image** | Settings harness → template → CLI flag | Allows image updates without re-creating agents |
| **Unix username** | Settings harness | Tied to image, follows image resolution |
| **Tmux** | Settings runtime/profile | Runtime behavior, not agent identity |
| **Auth credentials** | Host environment / auth provider | Security: never persisted |
| **`${VAR}` expansion** | Host environment at start time | Values may change between runs |
| **`SCION_AGENT_NAME` etc.** | Injected at start | Runtime metadata |
| **Profile selection** | CLI `--profile` → saved → settings active | Allows changing profiles between runs |

---

## MergeScionConfig Semantics (`templates.go:401-514`)

| Field | Merge Strategy |
|-------|----------------|
| `Harness`, `ConfigDir`, `Image`, `Model` | Override wins if non-empty |
| `Env` | Map merge (override keys win, base keys preserved) |
| `Volumes` | Append (cumulative, no dedup) |
| `CommandArgs` | Override replaces entirely (not appended) |
| `Detached` | Override wins if non-nil |
| `Kubernetes` | Deep merge individual fields |
| `Gemini` | Deep merge individual fields |
| `Info` | Deep merge individual fields |

---

## Inconsistencies and Observations

### 1. Image resolution is split across both phases

**At creation**: The template's `scion-agent.json` may specify an `image`, and the
`--image` CLI flag is stored in `agent-info.json` (`provision.go:278-280`). But the
settings harness image is also merged into the `ScionConfig` env/volumes at creation.

**At start**: Image is re-resolved from settings, then overridden by template config,
then by CLI flag (`run.go:99-141`). The `agent-info.json` image is **not consulted** during
start — it's only metadata.

**Issue**: If a user passes `--image` at create time, it gets stored in `agent-info.json`
but is not used at start time. The comment at `provision.go:276-277` even acknowledges this:
`"Image and other fields will be resolved at runtime from settings"`. The stored image
in `agent-info.json` is effectively dead data for the start flow.

### 2. Template re-merging at start time creates drift potential

When `GetAgent()` is called for an existing agent (`provision.go:452-481`), it reloads
the template chain and re-merges it with the agent's `scion-agent.json`. This means if
someone modifies a template after agents were created from it, existing agents will pick
up the template changes on their next start.

**Trade-off**: This is useful for fleet-wide updates but violates the principle that
"configuration should be determined at create time." If the goal is create-time
finalization, the start path should load only from the persisted `scion-agent.json`
without re-merging from the template.

### 3. Settings env/volumes are merged at creation but re-resolved at start for image

At creation (`provision.go:230-248`), the settings harness env and volumes are merged
into the persisted `scion-agent.json`. But at start time (`run.go:105-111`), the settings
harness is consulted again for image and user — **not for env and volumes**.

This creates an asymmetry: changing a harness's env in settings requires re-creating
agents to pick up the change, but changing a harness's image takes effect on next start.

### 4. CommandArgs merge is "last wins" not cumulative

Unlike volumes (which append) and env (which merge maps), `CommandArgs` uses a replace
strategy (`templates.go:436-438`). If a template inherits from a base that sets
command args, the child completely replaces them. This is reasonable for args but differs
from the other collection types, which could be surprising.

### 5. Profile is saved but its effect is inconsistent

The profile is saved in `agent-info.json` at creation time. At start time, the
profile is re-resolved (`run.go:103-116`) with CLI `--profile` taking precedence over
the saved value (via `GetSavedProfile()` in `provision.go:317-333`). However, the
saved profile only affects start-time concerns (image, user, tmux) — the creation-time
merge already baked the profile's env/volumes into `scion-agent.json`.

If a user changes profiles between create and start, they get the new profile's
image/user/tmux but the old profile's env/volumes (from creation time).

### 6. No explicit "finalize" step

There's no clear boundary where config is considered "done." The `scion-agent.json`
written at creation time is a merge of template + settings, but start time then overlays
additional resolution on top. A more explicit model would either:
- Finalize everything into `scion-agent.json` at creation (except true runtime concerns)
- Or explicitly mark which fields are "snapshots" vs "re-resolved"

### 7. Harness provisioning may modify the config file that was just written

At `provision.go:307-312`, after writing `scion-agent.json`, the harness's `Provision()`
is called, which may modify files in the agent home. Then the config is **reloaded** from
disk. Currently only Claude's `Provision()` modifies `.claude.json` (not `scion-agent.json`),
but the reload pattern suggests this was designed to allow harnesses to mutate the agent
config — a potential source of confusion.

---

## Recommended Classification: Create-Time vs Start-Time

Based on the analysis, here's a proposed clean separation:

### Should be finalized at create time (snapshot into `scion-agent.json`)
- `harness` — fundamental identity of the agent
- `env` — from template + settings merge (excluding runtime-discovered secrets)
- `volumes` — from template + settings merge
- `image` — should be persisted as the resolved image, not re-resolved
- `model` — LLM model selection
- `command_args` — harness CLI arguments
- `detached` — interaction mode
- `kubernetes` — cluster targeting config
- `gemini.auth_selectedType` — auth strategy selection

### Should remain start-time (late-bound)
- **Auth credentials** (`ANTHROPIC_API_KEY`, OAuth tokens, etc.) — security requirement
- **`${VAR}` expansion** — env var values must reflect current host state
- **`SCION_AGENT_NAME`** and other injected metadata — runtime concerns
- **Tmux** — runtime behavior that may change between runs
- **Profile → runtime mapping** — determines which container runtime to use

### Currently ambiguous (needs decision)
- **Image**: Currently re-resolved at start. If the goal is create-time finalization,
  the resolved image should be persisted and used directly. If fleet-wide image updates
  are desired, keep it late-bound but document that explicitly.
- **Unix username**: Follows image, so should match image's binding.
- **Template re-merge at start**: Currently re-merges, which undermines create-time
  finalization. Should either be removed or made opt-in.

---

## Design Trade-off: Per-Agent vs Fleet Configuration

Finalizing config at creation time means:
- **Pro**: Agent config is self-contained, predictable, and editable after creation
- **Pro**: No surprises from settings changes affecting existing agents
- **Pro**: Agents can be inspected (`scion-agent.json`) to see exactly what they'll run with
- **Con**: Changing a setting (e.g., image tag) for many agents requires updating each one
- **Con**: Template improvements don't propagate to existing agents

This trade-off is acceptable and should be documented for users: if you want to change
configuration for many agents, you need to either re-create them or edit each one's
`scion-agent.json`. The benefit is that each agent's behavior is fully determined by its
own persisted config, making the system predictable and debuggable.
