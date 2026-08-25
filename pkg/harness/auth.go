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
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// GatherAuth populates an AuthConfig from the environment and filesystem.
// It is source-agnostic: it checks env vars and well-known file paths
// without knowing which harness will consume the result.
func GatherAuth() api.AuthConfig {
	return GatherAuthWithEnv(nil, true, nil)
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
//
// When authMeta is non-nil, env vars declared in the harness config's
// auth.types[*].required_env groups are gathered into AuthConfig.EnvVars,
// enabling config-driven auth passthrough without hardcoded Go fields.
func GatherAuthWithEnv(env map[string]string, localSources bool, authMeta *config.HarnessAuthMetadata) api.AuthConfig {
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
		// GCP shared fields with multi-source fallback resolution
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

	// File-sourced fields: check well-known ADC path (skip in broker mode).
	// Per-harness file discovery (OAuth, Codex, OpenCode, Claude credential
	// files) is now handled via gatherConfigFiles below.
	if localSources {
		home, _ := os.UserHomeDir()

		if auth.GoogleAppCredentials == "" && home != "" {
			adcPath := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
			if _, err := os.Stat(adcPath); err == nil {
				auth.GoogleAppCredentials = adcPath
			}
		}

		// Gather harness-declared file credentials from well-known paths
		if authMeta != nil && home != "" {
			auth.Files = gatherConfigFiles(authMeta, home)
		}
	}

	// Populate EnvVars from config-driven auth metadata. Every env key
	// declared in any auth type's required_env groups is looked up; keys
	// with non-empty values are included.
	if authMeta != nil {
		auth.EnvVars = gatherConfigEnvVars(lookup, authMeta)
	}

	return auth
}

// gatherConfigFiles discovers harness-declared file-based credentials from
// well-known home directory paths. It reads the TargetSuffix from each
// auth type's required_files entries, resolves the suffix against the user's
// home directory, and records any files that exist. Returns nil when no
// files are found.
func gatherConfigFiles(authMeta *config.HarnessAuthMetadata, home string) map[string]string {
	if authMeta == nil || len(authMeta.Types) == 0 || home == "" {
		return nil
	}
	var result map[string]string
	seen := make(map[string]struct{})
	for _, authType := range authMeta.Types {
		for _, rf := range authType.RequiredFiles {
			if rf.Field == "" || rf.TargetSuffix == "" {
				continue
			}
			if _, ok := seen[rf.Field]; ok {
				continue
			}
			seen[rf.Field] = struct{}{}
			// TargetSuffix starts with "/" (e.g. "/.claude/.credentials.json").
			// Resolve against home to get the absolute path.
			filePath := filepath.Join(home, rf.TargetSuffix)
			if _, err := os.Stat(filePath); err == nil {
				if result == nil {
					result = make(map[string]string)
				}
				result[rf.Field] = filePath
			}
		}
	}
	return result
}

// gatherConfigEnvVars collects env var values for all keys declared in any
// auth type's required_env groups. Returns nil when no values are found.
func gatherConfigEnvVars(lookup func(string) string, authMeta *config.HarnessAuthMetadata) map[string]string {
	if authMeta == nil || len(authMeta.Types) == 0 {
		return nil
	}
	var result map[string]string
	seen := make(map[string]struct{})
	for _, authType := range authMeta.Types {
		for _, req := range authType.RequiredEnv {
			for _, key := range req.AnyOf {
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if v := lookup(key); v != "" {
					if result == nil {
						result = make(map[string]string)
					}
					result[key] = v
				}
			}
		}
	}
	return result
}

// OverlayFileSecrets bridges file-type ResolvedSecrets from the hub into
// AuthConfig using config-driven field mappings. It reads field mappings
// from the harness config's auth.types entries and sets the corresponding
// AuthConfig fields. When a secret's Name matches a declared field mapping,
// the config-driven path is used. For secrets that don't match any declared
// Name, it falls back to target-path-suffix matching (preserving backward
// compatibility with secrets created before field mappings were added).
func OverlayFileSecrets(auth *api.AuthConfig, secrets []api.ResolvedSecret, authMeta *config.HarnessAuthMetadata) {
	fieldMap := buildFieldMap(authMeta)

	for _, s := range secrets {
		if s.Type != "file" {
			continue
		}
		if fieldName, ok := fieldMap[s.Name]; ok && fieldName != "" {
			setAuthConfigField(auth, fieldName, s.Target)
			continue
		}
		// Fallback: match by target path suffix for backward compat
		setAuthConfigFieldByTargetSuffix(auth, s.Target)
	}
}

// buildFieldMap collects secret-name -> AuthConfig field mappings from all
// auth types declared in the harness config.
func buildFieldMap(authMeta *config.HarnessAuthMetadata) map[string]string {
	m := make(map[string]string)
	if authMeta == nil {
		return m
	}
	for _, authType := range authMeta.Types {
		for _, rf := range authType.RequiredFiles {
			if rf.Name != "" && rf.Field != "" {
				m[rf.Name] = rf.Field
			}
		}
	}
	return m
}

// setAuthConfigField sets the named field on AuthConfig to the given value.
// "GoogleAppCredentials" sets the first-class GCP shared field; all other
// field names go into the Files map.
func setAuthConfigField(auth *api.AuthConfig, field, value string) {
	switch field {
	case "GoogleAppCredentials":
		auth.GoogleAppCredentials = value
	default:
		if auth.Files == nil {
			auth.Files = make(map[string]string)
		}
		auth.Files[field] = value
	}
}

// setAuthConfigFieldByTargetSuffix matches a file secret's target path to an
// AuthConfig field using path suffix heuristics for backward compatibility.
func setAuthConfigFieldByTargetSuffix(auth *api.AuthConfig, target string) {
	switch {
	case strings.HasSuffix(target, "/application_default_credentials.json"):
		auth.GoogleAppCredentials = target
	case strings.HasSuffix(target, "/oauth_creds.json"):
		setAuthConfigField(auth, "OAuthCreds", target)
	case strings.HasSuffix(target, "/.codex/auth.json"):
		setAuthConfigField(auth, "CodexAuthFile", target)
	case strings.HasSuffix(target, "/opencode/auth.json"):
		setAuthConfigField(auth, "OpenCodeAuthFile", target)
	case strings.HasSuffix(target, "/.claude/.credentials.json"):
		setAuthConfigField(auth, "ClaudeAuthFile", target)
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

	// Guard: reject harness implementation names (e.g. "container-script")
	// that may have leaked into scion-agent.json via a data-corruption bug.
	// These are never valid auth types and would confuse auth resolution.
	// The active repair at run.go cleans up the persisted value, but this
	// guard prevents the corrupted value from entering the auth path on
	// the first restart after corruption.
	if !IsHarnessImplementationName(selectedType) {
		auth.SelectedType = selectedType
	}
}

// ValidateAuth checks a ResolvedAuth for completeness before container launch.
// It acts as a post-resolution safety net: ResolveAuth should produce correct
// results, but ValidateAuth catches any bugs or race conditions (e.g., a
// credential file deleted between GatherAuth and container launch).
func ValidateAuth(resolved *api.ResolvedAuth, brokerMode bool) error {
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
		if f.SourcePath == "" {
			if brokerMode {
				// Broker mode: SourcePath intentionally cleared — file content
				// is staged via SCION_STAGED_SECRETS, not local paths.
				continue
			}
			return fmt.Errorf("auth validation failed: file mapping for container path %q has no source path", f.ContainerPath)
		}
		if _, err := os.Stat(f.SourcePath); err != nil {
			return fmt.Errorf("auth validation failed: credential file %q does not exist: %w", f.SourcePath, err)
		}
	}

	return nil
}

// Config-driven auth preflight.
//
// The functions below read the declarative `auth:` block from a
// harness-config.yaml (parsed into config.HarnessAuthMetadata) to drive
// all auth preflight decisions: required env keys, required file secrets,
// and auth-type auto-detection.
//
// Detection precedence
// --------------------
// When several env vars or file secrets are present at once, the
// functions use a deterministic precedence rule:
//
//   1. Build the candidate set: every auth type that any present key maps
//      to via authMeta.Autodetect.{Env|Files}.
//   2. If the harness's default_type appears in the candidate set, return
//      "" — the caller is already on the default and no override is needed.
//   3. Otherwise return the alphabetically-smallest candidate. For the
//      built-in harness set this matches the operational preference order:
//      "auth-file" < "oauth-token" < "vertex-ai". Future harnesses with
//      non-monotonic preferences should pick auth type names that sort in
//      their preferred order.

// AuthMetadataAvailable reports whether a HarnessConfigEntry carries the
// declarative auth block needed by the auth preflight functions.
func AuthMetadataAvailable(entry *config.HarnessConfigEntry) bool {
	if entry == nil || entry.Auth == nil {
		return false
	}
	if len(entry.Auth.Types) == 0 && len(entry.Auth.Autodetect.Env) == 0 && len(entry.Auth.Autodetect.Files) == 0 {
		return false
	}
	return true
}

// RequiredAuthEnvKeysFromConfig returns the env-var alternative groups for
// the given auth type as declared in authMeta.Types.
func RequiredAuthEnvKeysFromConfig(authMeta *config.HarnessAuthMetadata, authSelectedType string) [][]string {
	if authMeta == nil {
		return nil
	}
	effective := authSelectedType
	if effective == "" {
		effective = authMeta.DefaultType
		if effective == "" {
			effective = "api-key"
		}
	}
	t, ok := authMeta.Types[effective]
	if !ok {
		return nil
	}
	if len(t.RequiredEnv) == 0 {
		return nil
	}
	groups := make([][]string, 0, len(t.RequiredEnv))
	for _, req := range t.RequiredEnv {
		if len(req.AnyOf) == 0 {
			continue
		}
		group := append([]string(nil), req.AnyOf...)
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return nil
	}
	return groups
}

// RequiredAuthSecretsFromConfig returns only file requirements explicitly marked
// `required: true` — documentary files (e.g. CLAUDE_AUTH for Claude's
// auth-file type, which the user mounts from a locally-resolved file) are
// not preflight-enforced. File requirements with
// SkippedWhenGCPServiceAccountAssigned are dropped when gcpSAAssigned is
// true, mirroring the compiled behavior for vertex-ai with workload identity.
func RequiredAuthSecretsFromConfig(authMeta *config.HarnessAuthMetadata, authSelectedType string, gcpSAAssigned bool) []api.RequiredSecret {
	if authMeta == nil {
		return nil
	}
	effective := authSelectedType
	if effective == "" {
		effective = authMeta.DefaultType
		if effective == "" {
			effective = "api-key"
		}
	}
	t, ok := authMeta.Types[effective]
	if !ok {
		return nil
	}
	if len(t.RequiredFiles) == 0 {
		return nil
	}
	out := make([]api.RequiredSecret, 0, len(t.RequiredFiles))
	for _, f := range t.RequiredFiles {
		if !f.Required {
			continue
		}
		if f.SkippedWhenGCPServiceAccountAssigned && gcpSAAssigned {
			continue
		}
		fileType := f.Type
		if fileType == "" {
			fileType = "file"
		}
		out = append(out, api.RequiredSecret{
			Key:                f.Name,
			Type:               fileType,
			Description:        f.Description,
			AlternativeEnvKeys: append([]string(nil), f.AlternativeEnvKeys...),
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// DetectAuthTypeFromFileSecretsFromConfig uses authMeta.Autodetect.Files to
// map each present file-secret name to a candidate auth type.
func DetectAuthTypeFromFileSecretsFromConfig(authMeta *config.HarnessAuthMetadata, fileSecretNames map[string]struct{}) string {
	if authMeta == nil {
		return ""
	}
	return pickAutodetectCandidate(authMeta.DefaultType, authMeta.Autodetect.Files, fileSecretNames)
}

// DetectAuthTypeFromEnvVarsFromConfig detects auth type from env vars
// using authMeta.Autodetect.Env mappings.
func DetectAuthTypeFromEnvVarsFromConfig(authMeta *config.HarnessAuthMetadata, envKeys map[string]struct{}) string {
	if authMeta == nil {
		return ""
	}
	return pickAutodetectCandidate(authMeta.DefaultType, authMeta.Autodetect.Env, envKeys)
}

// DetectAuthTypeFromGCPIdentityFromConfig returns "vertex-ai" only when the
// harness declares a vertex-ai auth type (so the metadata server actually
// has a use) and gcpSAAssigned is true.
func DetectAuthTypeFromGCPIdentityFromConfig(authMeta *config.HarnessAuthMetadata, gcpSAAssigned bool) string {
	if !gcpSAAssigned || authMeta == nil {
		return ""
	}
	if _, ok := authMeta.Types["vertex-ai"]; ok {
		return "vertex-ai"
	}
	return ""
}

// pickAutodetectCandidate implements the deterministic precedence rule
// documented above: prefer the default_type, otherwise return the
// alphabetically-smallest candidate.
func pickAutodetectCandidate(defaultType string, autodetect map[string]string, presentKeys map[string]struct{}) string {
	if len(autodetect) == 0 || len(presentKeys) == 0 {
		return ""
	}
	candidates := make(map[string]struct{})
	for key, authType := range autodetect {
		if authType == "" {
			continue
		}
		if _, ok := presentKeys[key]; ok {
			candidates[authType] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return ""
	}
	if defaultType != "" {
		if _, ok := candidates[defaultType]; ok {
			return ""
		}
	}
	sorted := make([]string, 0, len(candidates))
	for c := range candidates {
		sorted = append(sorted, c)
	}
	sort.Strings(sorted)
	return sorted[0]
}
