# Agent Credential Resolution

## Overview

Scion uses a unified four-stage pipeline to resolve LLM credentials for every agent, regardless of harness type or deployment mode (local or hub-dispatched). The pipeline separates credential discovery from harness-specific resolution, ensuring consistent behavior and clear error reporting.

## Auth Resolution Pipeline

```
1. GatherAuth()           — Scan env vars and well-known file paths (harness-agnostic)
       ↓
2. OverlaySettings()      — Apply harness-config and settings.json overrides
       ↓
3. harness.ResolveAuth()  — Select best auth method per harness preference order
       ↓
4. ValidateAuth()         — Safety-net checks before container launch
       ↓
   Container provisioning — Inject resolved env vars and mount credential files
```

### Stage 1: GatherAuth

**File**: `pkg/harness/auth.go`

Populates an `AuthConfig` struct from the process environment and well-known credential file paths. This function is harness-agnostic — it collects all possible credentials regardless of which harness will consume them.

**Environment variables scanned**:

| Env Var | AuthConfig Field | Fallback Chain |
|---------|-----------------|----------------|
| `GEMINI_API_KEY` | GeminiAPIKey | — |
| `GOOGLE_API_KEY` | GoogleAPIKey | — |
| `ANTHROPIC_API_KEY` | AnthropicAPIKey | — |
| `OPENAI_API_KEY` | OpenAIAPIKey | — |
| `CODEX_API_KEY` | CodexAPIKey | — |
| `GOOGLE_APPLICATION_CREDENTIALS` | GoogleAppCredentials | — |
| `GOOGLE_CLOUD_PROJECT` | GoogleCloudProject | `GCP_PROJECT` → `ANTHROPIC_VERTEX_PROJECT_ID` |
| `GOOGLE_CLOUD_LOCATION` | GoogleCloudRegion | `GOOGLE_CLOUD_REGION` → `CLOUD_ML_REGION` (warns when LOCATION and REGION differ; ptone/scion#4719) |

**File paths probed** (if not already set via env var):

| Path | AuthConfig Field | Condition |
|------|-----------------|-----------|
| `~/.config/gcloud/application_default_credentials.json` | GoogleAppCredentials | Only if `GOOGLE_APPLICATION_CREDENTIALS` not set |
| `~/.gemini/oauth_creds.json` | OAuthCreds | Always checked |
| `~/.codex/auth.json` | CodexAuthFile | Always checked |
| `~/.local/share/opencode/auth.json` | OpenCodeAuthFile | Always checked |

### Stage 2: OverlaySettings

**File**: `pkg/harness/auth.go`

Applies settings-based overrides to the `AuthConfig`. Currently only active for the Gemini harness — returns immediately for all others.

**For Gemini, the priority chain for `SelectedType`**:
1. `scion-agent.json` → `cfg.Gemini.AuthSelectedType`
2. Agent settings → `~/.gemini/settings.json` → `security.auth.selectedType`
3. Host settings → global agent settings → `security.auth.selectedType`

API key fallback: If no API key was found from env vars, checks agent settings (`apiKey` field) and host settings (`apiKey` field), assigning to `GeminiAPIKey`.

### Stage 3: ResolveAuth (Per-Harness)

Each harness implements `ResolveAuth(AuthConfig) (*ResolvedAuth, error)` with its own preference order. The method examines the populated `AuthConfig` and returns a `ResolvedAuth` containing the selected auth method, environment variables to inject, and files to mount.

See [Per-Harness Preference Orders](#per-harness-preference-orders) below.

### Stage 4: ValidateAuth

**File**: `pkg/harness/auth.go`

Post-resolution safety net. Catches bugs or race conditions between credential gathering and container launch.

**Checks performed**:
1. `resolved != nil` — resolved auth must not be nil
2. `resolved.Method != ""` — an auth method must be selected
3. All `resolved.EnvVars` values are non-empty — no env var may have an empty value
4. All `resolved.Files` entries have a non-empty `ContainerPath`
5. All `resolved.Files` source paths exist on disk (`os.Stat`)

## AuthConfig Fields

**File**: `pkg/api/types.go`

```go
type AuthConfig struct {
    // Google/Gemini auth
    GeminiAPIKey         string
    GoogleAPIKey         string
    GoogleAppCredentials string
    GoogleCloudProject   string
    GoogleCloudRegion    string
    OAuthCreds           string

    // Anthropic auth
    AnthropicAPIKey      string

    // OpenAI/Codex auth
    OpenAIAPIKey         string
    CodexAPIKey          string
    CodexAuthFile        string
    OpenCodeAuthFile     string

    // Auth mode selection
    SelectedType         string
}
```

## ResolvedAuth Types

**File**: `pkg/api/types.go`

```go
type ResolvedAuth struct {
    Method  string            // e.g. "anthropic-api-key", "vertex-ai", "passthrough"
    EnvVars map[string]string // env vars to inject into container
    Files   []FileMapping     // files to copy/mount into container
}

type FileMapping struct {
    SourcePath    string // absolute host path
    ContainerPath string // target path in container (~ = home placeholder)
}
```

The `~` prefix in `ContainerPath` is expanded to the container user's home directory by the runtime layer at launch time.

## Per-Harness Preference Orders

### Claude Code

**Preference order**: API key → Vertex AI → error

| Priority | Method | Required Fields | Env Vars Set | Files Mounted |
|----------|--------|----------------|-------------|---------------|
| 1 | `anthropic-api-key` | `AnthropicAPIKey` | `ANTHROPIC_API_KEY` | — |
| 2 | `vertex-ai` | `GoogleAppCredentials` + `GoogleCloudProject` + `GoogleCloudRegion` | `CLAUDE_CODE_USE_VERTEX=1`, `CLOUD_ML_REGION`, `ANTHROPIC_VERTEX_PROJECT_ID`, `GOOGLE_APPLICATION_CREDENTIALS` | ADC → `~/.config/gcp/application_default_credentials.json` |

**Error**: `"claude: no valid auth method found; set ANTHROPIC_API_KEY for direct API access, or provide GOOGLE_APPLICATION_CREDENTIALS + GOOGLE_CLOUD_PROJECT + GOOGLE_CLOUD_REGION for Vertex AI"`

### Gemini CLI

Gemini supports both explicit mode selection (via `SelectedType`) and auto-detection.

#### Explicit Mode (SelectedType is set)

| SelectedType | Method | Required Fields | Env Vars Set | Files Mounted |
|-------------|--------|----------------|-------------|---------------|
| `gemini-api-key` | `gemini-api-key` | `GeminiAPIKey` or `GoogleAPIKey` | `GEMINI_DEFAULT_AUTH_TYPE=gemini-api-key`, `GEMINI_API_KEY` or `GOOGLE_API_KEY` | — |
| `oauth-personal` | `oauth-personal` | `OAuthCreds` | `GEMINI_DEFAULT_AUTH_TYPE=oauth-personal`, optional `GOOGLE_CLOUD_PROJECT` | OAuth creds → `~/.gemini/oauth_creds.json` |
| `vertex-ai` | `vertex-ai` | `GoogleCloudProject` | `GEMINI_DEFAULT_AUTH_TYPE=vertex-ai`, `GOOGLE_CLOUD_PROJECT`, optional `GOOGLE_CLOUD_REGION` | Optional ADC file |
| `compute-default-credentials` | `compute-default-credentials` | — | `GEMINI_DEFAULT_AUTH_TYPE=compute-default-credentials`, optional `GOOGLE_CLOUD_PROJECT` | Optional ADC file |

#### Auto-Detect Mode (SelectedType is empty)

| Priority | Method | Trigger |
|----------|--------|---------|
| 1 | `gemini-api-key` | `GeminiAPIKey` or `GoogleAPIKey` is set |
| 2 | `compute-default-credentials` | `GoogleAppCredentials` is set |
| 3 | `oauth-personal` | `OAuthCreds` is set |

**Error**: `"gemini: no valid auth method found; set GEMINI_API_KEY or GOOGLE_API_KEY for API key auth, provide GOOGLE_APPLICATION_CREDENTIALS for ADC, or set up OAuth credentials at ~/.gemini/oauth_creds.json"`

### Generic

**Strategy**: Passthrough — maps all available credentials into the container. Never errors.

| Method | Behavior |
|--------|----------|
| `passthrough` | Injects all available API keys, project/region vars, and credential files |

**Env vars** (if present): `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`, `GOOGLE_API_KEY`, `OPENAI_API_KEY`, `CODEX_API_KEY`, `GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_REGION`, `GOOGLE_APPLICATION_CREDENTIALS`

**Files** (if present): OAuth creds → `~/.scion/oauth_creds.json`, Codex auth → `~/.codex/auth.json`, OpenCode auth → `~/.local/share/opencode/auth.json`, ADC → `~/.config/gcp/application_default_credentials.json`

### OpenCode

**Preference order**: Anthropic API key → OpenAI API key → auth file → error

| Priority | Method | Required Fields | Env Vars Set | Files Mounted |
|----------|--------|----------------|-------------|---------------|
| 1 | `anthropic-api-key` | `AnthropicAPIKey` | `ANTHROPIC_API_KEY` | — |
| 2 | `openai-api-key` | `OpenAIAPIKey` | `OPENAI_API_KEY` | — |
| 3 | `opencode-auth-file` | `OpenCodeAuthFile` | — | Auth file → `~/.local/share/opencode/auth.json` |

**Error**: `"opencode: no valid auth method found; set ANTHROPIC_API_KEY or OPENAI_API_KEY, or provide auth credentials at ~/.local/share/opencode/auth.json"`

### Codex

**Preference order**: Codex API key → OpenAI API key → auth file → error

| Priority | Method | Required Fields | Env Vars Set | Files Mounted |
|----------|--------|----------------|-------------|---------------|
| 1 | `codex-api-key` | `CodexAPIKey` | `CODEX_API_KEY` | — |
| 2 | `openai-api-key` | `OpenAIAPIKey` | `OPENAI_API_KEY` | — |
| 3 | `codex-auth-file` | `CodexAuthFile` | — | Auth file → `~/.codex/auth.json` |

**Error**: `"codex: no valid auth method found; set CODEX_API_KEY or OPENAI_API_KEY, or provide auth credentials at ~/.codex/auth.json"`

## Hub Secret → AuthConfig Mapping

When agents are dispatched via the Hub, secrets are resolved and projected into the agent's environment before `GatherAuth()` runs. Environment-type secrets become env vars; file-type secrets are written to target paths. The `GatherAuth()` function then picks them up the same way it picks up host-level credentials.

| Hub Secret Name | Type | Target | AuthConfig Field |
|----------------|------|--------|-----------------|
| `GEMINI_API_KEY` | environment | `GEMINI_API_KEY` | GeminiAPIKey |
| `GOOGLE_API_KEY` | environment | `GOOGLE_API_KEY` | GoogleAPIKey |
| `ANTHROPIC_API_KEY` | environment | `ANTHROPIC_API_KEY` | AnthropicAPIKey |
| `OPENAI_API_KEY` | environment | `OPENAI_API_KEY` | OpenAIAPIKey |
| `CODEX_API_KEY` | environment | `CODEX_API_KEY` | CodexAPIKey |
| `GOOGLE_CLOUD_PROJECT` | environment | `GOOGLE_CLOUD_PROJECT` | GoogleCloudProject |
| `GOOGLE_CLOUD_REGION` | environment | `GOOGLE_CLOUD_REGION` | GoogleCloudRegion |
| `GOOGLE_APPLICATION_CREDENTIALS` | file | `~/.config/gcloud/application_default_credentials.json` | GoogleAppCredentials |
| `GEMINI_OAUTH_CREDS` | file | `~/.gemini/oauth_creds.json` | OAuthCreds |
| `CODEX_AUTH` | file | `~/.codex/auth.json` | CodexAuthFile |
| `OPENCODE_AUTH` | file | `~/.local/share/opencode/auth.json` | OpenCodeAuthFile |

For file-type secrets, the Hub stores base64-encoded content and the runtime projects them to the target path before the auth pipeline runs.

## Container File Path Mappings

Credential files are mapped from host paths to standardized container paths:

| Source File | Container Path | Used By |
|------------|---------------|---------|
| `~/.config/gcloud/application_default_credentials.json` | `~/.config/gcp/application_default_credentials.json` | Claude, Gemini, Generic |
| `~/.gemini/oauth_creds.json` | `~/.gemini/oauth_creds.json` | Gemini |
| `~/.gemini/oauth_creds.json` | `~/.scion/oauth_creds.json` | Generic |
| `~/.codex/auth.json` | `~/.codex/auth.json` | Codex, Generic |
| `~/.local/share/opencode/auth.json` | `~/.local/share/opencode/auth.json` | OpenCode, Generic |

Note: The ADC file uses a different container path (`gcp/` instead of `gcloud/`) to avoid conflicting with other gcloud state that may exist in the container.

## RequiredEnvKeys

Each harness declares which env vars are required for the hub env-gather flow via `RequiredEnvKeys(authSelectedType string) []string`:

| Harness | authSelectedType | Required Keys |
|---------|-----------------|---------------|
| Claude | any | `["ANTHROPIC_API_KEY"]` |
| Gemini | `gemini-api-key` | `["GEMINI_API_KEY"]` |
| Gemini | `vertex-ai` | `["GOOGLE_CLOUD_PROJECT"]` |
| Gemini | `oauth-personal` / `compute-default-credentials` | `[]` |
| Gemini | (default) | `["GEMINI_API_KEY"]` |
| Generic | any | `[]` |
| OpenCode | any | `["ANTHROPIC_API_KEY"]` |
| Codex | any | `[]` |

## Error Handling

### ResolveAuth Errors

Each harness produces actionable error messages when no valid auth method can be determined. Error messages follow a consistent pattern:

```
<harness>: no valid auth method found; <how to fix>
```

Explicit-mode errors (Gemini with `SelectedType` set) identify the specific method that failed and what credential is missing.

### ValidateAuth Errors

Post-resolution validation errors use the prefix `auth validation failed:` followed by the specific issue:

| Error | Cause |
|-------|-------|
| `auth validation failed: resolved auth is nil` | Bug in harness — `ResolveAuth` returned nil without error |
| `auth validation failed: no auth method selected` | Bug in harness — `Method` field is empty |
| `auth validation failed: env vars have empty values: VAR1, VAR2` | Bug in harness — env var keys present but values empty |
| `auth validation failed: file mapping for <path> has no container path` | Bug in harness — file mapping missing container path |
| `auth validation failed: credential file <path> does not exist: <err>` | Credential file was removed between gathering and validation |

## Pipeline Execution

**File**: `pkg/agent/run.go`

The pipeline is executed in `agent.Start()`:

```go
auth := harness.GatherAuth()
harness.OverlaySettings(&auth, h, agentHome)
resolved, err := h.ResolveAuth(auth)
if err != nil {
    return err  // No valid auth method — actionable error message
}
if err := harness.ValidateAuth(resolved); err != nil {
    return err  // Safety-net validation failure
}
// Pass resolved.EnvVars and resolved.Files to container provisioning
```

For hub-dispatched agents, `ResolvedSecrets` (environment-type) are injected into `opts.Env` before the pipeline runs, making them visible to `GatherAuth()` via standard env var reads. File-type secrets are projected to their target paths before the pipeline runs, making them visible to `GatherAuth()` via `os.Stat()`.
