// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// GatherAuth populates an AuthConfig from the environment and filesystem.
// It is source-agnostic: it checks env vars and well-known file paths
// without knowing which harness will consume the result.
func GatherAuth() api.AuthConfig {
	return GatherAuthWithEnv(nil, true)
}

// GatherAuthWithEnv is like GatherAuth but checks the provided env overlay
// before falling back to os.Getenv for each key. This allows hub-resolved
// or CLI-gathered env vars (passed via opts.Env) to be visible during auth
// resolution, even when the broker process itself lacks those env vars.
//
// When localSources is false (broker mode), the lookup function only checks
// the env map and never falls back to os.Getenv(), and filesystem scanning
// for well-known credential files is skipped entirely. This prevents broker
// operator credentials from leaking into hub-dispatched agents.
func GatherAuthWithEnv(env map[string]string, localSources bool) api.AuthConfig {
	lookup := func(key string) string {
		if v, ok := env[key]; ok && v != "" {
			return v
		}
		if localSources {
			return os.Getenv(key)
		}
		return ""
	}

	auth := api.AuthConfig{
		// Env-var sourced fields
		GeminiAPIKey:     lookup("GEMINI_API_KEY"),
		GoogleAPIKey:     lookup("GOOGLE_API_KEY"),
		AnthropicAPIKey:  lookup("ANTHROPIC_API_KEY"),
		ClaudeOAuthToken: lookup("CLAUDE_CODE_OAUTH_TOKEN"),
		OpenAIAPIKey:     lookup("OPENAI_API_KEY"),
		CodexAPIKey:      lookup("CODEX_API_KEY"),
		GoogleCloudProject: util.FirstNonEmpty(
			lookup("GOOGLE_CLOUD_PROJECT"),
			lookup("GCP_PROJECT"),
			lookup("ANTHROPIC_VERTEX_PROJECT_ID"),
		),
		GoogleCloudRegion: util.FirstNonEmpty(
			lookup("GOOGLE_CLOUD_REGION"),
			lookup("CLOUD_ML_REGION"),
			lookup("GOOGLE_CLOUD_LOCATION"),
		),
		GoogleAppCredentials: lookup("GOOGLE_APPLICATION_CREDENTIALS"),
		GCPMetadataMode:      lookup("SCION_METADATA_MODE"),
	}

	// File-sourced fields: check well-known paths (skip in broker mode)
	if localSources {
		home, _ := os.UserHomeDir()

		if auth.GoogleAppCredentials == "" && home != "" {
			adcPath := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
			if _, err := os.Stat(adcPath); err == nil {
				auth.GoogleAppCredentials = adcPath
			}
		}

		if home != "" {
			oauthPath := filepath.Join(home, ".gemini", "oauth_creds.json")
			if _, err := os.Stat(oauthPath); err == nil {
				auth.OAuthCreds = oauthPath
			}

			codexPath := filepath.Join(home, ".codex", "auth.json")
			if _, err := os.Stat(codexPath); err == nil {
				auth.CodexAuthFile = codexPath
			}

			opencodePath := filepath.Join(home, ".local", "share", "opencode", "auth.json")
			if _, err := os.Stat(opencodePath); err == nil {
				auth.OpenCodeAuthFile = opencodePath
			}

			// Claude Code's rotating credentials store. Unlike Gemini/Codex/
			// OpenCode we do NOT parse this file — we treat it as an opaque
			// file to mount into the container so Claude Code can read and
			// refresh it natively. The access token inside rotates; scraping
			// it at gather time would hand the container a stale snapshot.
			claudeCredsPath := filepath.Join(home, ".claude", ".credentials.json")
			if _, err := os.Stat(claudeCredsPath); err == nil {
				auth.ClaudeAuthFile = claudeCredsPath
			}
		}
	}

	return auth
}

// OverlayFileSecrets bridges file-type ResolvedSecrets from the hub into
// AuthConfig fields so that ResolveAuth can determine the correct auth method.
// It maps well-known secret names/targets to the corresponding AuthConfig fields
// using the target path as a sentinel value (the actual file content is projected
// into the container by writeFileSecrets at launch time).
func OverlayFileSecrets(auth *api.AuthConfig, secrets []api.ResolvedSecret) {
	for _, s := range secrets {
		if s.Type != "file" {
			continue
		}
		target := s.Target
		name := s.Name

		switch {
		case name == "gcloud-adc" ||
			strings.HasSuffix(target, "/application_default_credentials.json"):
			auth.GoogleAppCredentials = target
		case name == "GEMINI_OAUTH_CREDS" ||
			strings.HasSuffix(target, "/oauth_creds.json"):
			auth.OAuthCreds = target
		case name == "CODEX_AUTH" ||
			strings.HasSuffix(target, "/.codex/auth.json"):
			auth.CodexAuthFile = target
		case name == "OPENCODE_AUTH" ||
			strings.HasSuffix(target, "/opencode/auth.json"):
			auth.OpenCodeAuthFile = target
		case name == "CLAUDE_AUTH" ||
			strings.HasSuffix(target, "/.claude/.credentials.json"):
			auth.ClaudeAuthFile = target
		}
	}
}

// OverlaySettings applies settings-based overrides to an AuthConfig.
// It reads AuthSelectedType from scion-agent.json (top-level), which is
// populated from scion's settings chain during provisioning.
// Note: we intentionally do NOT fall back to the host's harness settings
// (e.g. ~/.gemini/settings.json) because those contain harness-internal
// auth type values (like "oauth-personal") that are not valid universal types.
// agentDir is the directory containing scion-agent.json (which may differ
// from filepath.Dir(agentHome) when split storage is active).
func OverlaySettings(auth *api.AuthConfig, h api.Harness, agentDir string) {
	selectedType := ""

	// Check scion-agent.json for top-level auth_selectedType
	scionAgentPath := filepath.Join(agentDir, "scion-agent.json")
	if data, err := os.ReadFile(scionAgentPath); err == nil {
		var cfg api.ScionConfig
		if err := json.Unmarshal(data, &cfg); err == nil {
			selectedType = cfg.AuthSelectedType
		}
	}

	auth.SelectedType = selectedType
}

// ValidateAuth checks a ResolvedAuth for completeness before container launch.
// It acts as a post-resolution safety net: ResolveAuth should produce correct
// results, but ValidateAuth catches any bugs or race conditions (e.g., a
// credential file deleted between GatherAuth and container launch).
func ValidateAuth(resolved *api.ResolvedAuth) error {
	if resolved == nil {
		return fmt.Errorf("auth validation failed: resolved auth is nil")
	}

	if resolved.Method == "" {
		return fmt.Errorf("auth validation failed: no auth method selected")
	}

	// Check for empty env var values — an env var with an empty value
	// indicates a bug in ResolveAuth (it should not emit keys it cannot fill).
	var emptyVars []string
	for k, v := range resolved.EnvVars {
		if v == "" {
			emptyVars = append(emptyVars, k)
		}
	}
	if len(emptyVars) > 0 {
		return fmt.Errorf("auth validation failed: env vars have empty values: %s", strings.Join(emptyVars, ", "))
	}

	// Check file mappings: source must exist, container path must be set.
	for _, f := range resolved.Files {
		if f.ContainerPath == "" {
			return fmt.Errorf("auth validation failed: file mapping for %q has no container path", f.SourcePath)
		}
		if _, err := os.Stat(f.SourcePath); err != nil {
			return fmt.Errorf("auth validation failed: credential file %q does not exist: %w", f.SourcePath, err)
		}
	}

	return nil
}

// RequiredAuthSecrets maps a (harnessName, authSelectedType) pair to
// file-type secrets required by that combination. This is the file-secret
// counterpart to RequiredAuthEnvKeys (which covers env var requirements).
// For vertex-ai auth, the ADC credential file is required unless a GCP
// service account is assigned (gcpSAAssigned), in which case the metadata
// server provides credentials and no ADC file is needed.
// Returns nil for auth methods that have no file-secret requirements.
func RequiredAuthSecrets(harnessName, authSelectedType string, gcpSAAssigned bool) []api.RequiredSecret {
	effectiveType := authSelectedType
	if effectiveType == "" {
		effectiveType = "api-key"
	}

	switch harnessName {
	case "claude", "gemini", "opencode", "codex":
		if effectiveType == "vertex-ai" && !gcpSAAssigned {
			return []api.RequiredSecret{
				{
					Key:                "gcloud-adc",
					Type:               "file",
					Description:        "Google Cloud Application Default Credentials (ADC) file for vertex-ai authentication",
					AlternativeEnvKeys: []string{"GOOGLE_APPLICATION_CREDENTIALS"},
				},
			}
		}
	}

	return nil
}

// DetectAuthTypeFromFileSecrets checks whether resolved file secrets can
// satisfy an alternative auth method for the given harness. This mirrors
// the auto-detect priority in each harness's ResolveAuth: when no auth
// type is explicitly selected, harnesses try API key first but fall back
// to file-based auth (OAuth, ADC, etc.) when credentials are available.
//
// Returns the effective auth type (e.g., "auth-file", "vertex-ai") if
// file secrets satisfy it, or "" if no file-based auth is possible.
// The caller should use the returned type to override the default "api-key"
// assumption during env-gather, preventing false requirements.
func DetectAuthTypeFromFileSecrets(harnessName string, fileSecretNames map[string]struct{}) string {
	switch harnessName {
	case "gemini":
		// Auto-detect priority: api-key → OAuth (auth-file) → ADC (vertex-ai)
		if _, ok := fileSecretNames["GEMINI_OAUTH_CREDS"]; ok {
			return "auth-file"
		}
		if _, ok := fileSecretNames["gcloud-adc"]; ok {
			return "vertex-ai"
		}
	case "claude":
		// Auto-detect priority: api-key → oauth-token (env) → auth-file → ADC (vertex-ai)
		if _, ok := fileSecretNames["CLAUDE_AUTH"]; ok {
			return "auth-file"
		}
		if _, ok := fileSecretNames["gcloud-adc"]; ok {
			return "vertex-ai"
		}
	case "codex":
		if _, ok := fileSecretNames["CODEX_AUTH"]; ok {
			return "auth-file"
		}
	case "opencode":
		if _, ok := fileSecretNames["OPENCODE_AUTH"]; ok {
			return "auth-file"
		}
	}
	return ""
}

// DetectAuthTypeFromEnvVars checks whether resolved env vars can satisfy
// an alternative auth method for the given harness. For example,
// GOOGLE_APPLICATION_CREDENTIALS signals that ADC credentials are available,
// so vertex-ai auth can be used without a gcloud-adc file secret.
func DetectAuthTypeFromEnvVars(harnessName string, envKeys map[string]struct{}) string {
	switch harnessName {
	case "claude":
		if _, ok := envKeys["CLAUDE_CODE_OAUTH_TOKEN"]; ok {
			return "oauth-token"
		}
		if _, ok := envKeys["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
			return "vertex-ai"
		}
	case "gemini":
		if _, ok := envKeys["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
			return "vertex-ai"
		}
	}
	return ""
}

// DetectAuthTypeFromGCPIdentity returns "vertex-ai" when a GCP service
// account is assigned to the agent. Harnesses that support vertex-ai auth
// can use the metadata server for credentials instead of an ADC file.
func DetectAuthTypeFromGCPIdentity(harnessName string, gcpSAAssigned bool) string {
	if !gcpSAAssigned {
		return ""
	}
	switch harnessName {
	case "claude", "gemini":
		return "vertex-ai"
	}
	return ""
}

// RequiredAuthEnvKeys maps a (harnessName, authSelectedType) pair to the
// env var key groups required by that combination. Each inner slice is a
// set of alternatives — any one key satisfying the group is sufficient
// (e.g., GEMINI_API_KEY or GOOGLE_API_KEY for gemini api-key auth).
// Returns nil for unknown/unset combinations or harnesses with no
// intrinsic auth requirements (e.g., generic).
func RequiredAuthEnvKeys(harnessName, authSelectedType string) [][]string {
	// When authType is empty (unset), default to api-key — it's the
	// first-choice method in every harness's ResolveAuth(). This ensures
	// env-gather detects missing keys and returns 202 so the CLI can
	// collect them from the user's environment.
	effectiveType := authSelectedType
	if effectiveType == "" {
		effectiveType = "api-key"
	}

	switch harnessName {
	case "claude":
		switch effectiveType {
		case "api-key":
			return [][]string{{"ANTHROPIC_API_KEY"}}
		case "oauth-token":
			return [][]string{{"CLAUDE_CODE_OAUTH_TOKEN"}}
		case "auth-file":
			return nil
		case "vertex-ai":
			return [][]string{{"GOOGLE_CLOUD_PROJECT"}, {"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}}
		}
	case "gemini":
		switch effectiveType {
		case "api-key":
			return [][]string{{"GEMINI_API_KEY", "GOOGLE_API_KEY"}}
		case "vertex-ai":
			return [][]string{{"GOOGLE_CLOUD_PROJECT"}, {"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}}
		}
	case "opencode":
		switch effectiveType {
		case "api-key":
			return [][]string{{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"}}
		}
	case "codex":
		switch effectiveType {
		case "api-key":
			return [][]string{{"CODEX_API_KEY", "OPENAI_API_KEY"}}
		}
	}

	return nil
}
