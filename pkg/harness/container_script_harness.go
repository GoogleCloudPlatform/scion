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
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// ContainerScriptHarness is a thin api.Harness implementation that stages a
// harness-config bundle into the agent home and defers harness-native file
// rewrites to a provisioning script that runs inside the agent container.
//
// Host or broker code never invokes the script. Provision() copies the script,
// the manifest, staged inputs, candidate secrets, and a trusted lifecycle hook
// wrapper under agent_home/.scion/. sciontool init runs the wrapper inside the
// container during the pre-start lifecycle hook.
type ContainerScriptHarness struct {
	// entry is the resolved harness-config entry from config.yaml plus any
	// settings overrides.
	entry config.HarnessConfigEntry

	// configDirPath is the absolute path to the on-disk harness-config dir
	// (e.g. ~/.scion/harness-configs/<name>/). Empty for synthetic configs.
	configDirPath string
}

// NewContainerScriptHarness constructs a ContainerScriptHarness from a resolved
// harness-config directory. configDirPath is the absolute path to the
// harness-config directory; entry is the parsed config.yaml after settings
// overlay.
func NewContainerScriptHarness(configDirPath string, entry config.HarnessConfigEntry) (*ContainerScriptHarness, error) {
	if entry.Provisioner == nil {
		return nil, fmt.Errorf("container-script harness requires a provisioner block")
	}
	if entry.Harness == "" {
		return nil, fmt.Errorf("container-script harness requires harness name in config.yaml")
	}
	if configDirPath == "" {
		return nil, fmt.Errorf("container-script harness requires harness-config directory path")
	}
	return &ContainerScriptHarness{
		entry:         entry,
		configDirPath: configDirPath,
	}, nil
}

// Name returns the harness type from config.yaml.
func (c *ContainerScriptHarness) Name() string { return c.entry.Harness }

// DefaultConfigDir returns the harness-native config directory (e.g. .claude).
func (c *ContainerScriptHarness) DefaultConfigDir() string { return c.entry.ConfigDir }

// SkillsDir returns the harness skills subdirectory.
func (c *ContainerScriptHarness) SkillsDir() string { return c.entry.SkillsDir }

// GetInterruptKey returns the configured interrupt key, defaulting to C-c.
func (c *ContainerScriptHarness) GetInterruptKey() string {
	if c.entry.InterruptKey == "" {
		return "C-c"
	}
	return c.entry.InterruptKey
}

// GetInterruptSequence returns the configured interrupt key sequence.
// When interrupt_signal is "sequence" or interrupt_sequence is populated,
// each entry is sent as a separate tmux send-keys call.
func (c *ContainerScriptHarness) GetInterruptSequence() []string {
	if c.entry.InterruptSignal == "sequence" || len(c.entry.InterruptSequence) > 0 {
		return c.entry.InterruptSequence
	}
	return nil
}

// GetHarnessEmbedsFS returns an empty FS — container-script harnesses do not
// own embedded files; their files live on disk in the harness-config dir.
func (c *ContainerScriptHarness) GetHarnessEmbedsFS() (embed.FS, string) {
	return embed.FS{}, ""
}

// HasSystemPrompt reports whether the harness has a native system prompt file
// staged in the agent home. Container-script harnesses always advertise the
// declared file path; the script writes the file during pre-start.
func (c *ContainerScriptHarness) HasSystemPrompt(agentHome string) bool {
	if c.entry.SystemPromptFile == "" {
		return false
	}
	target := filepath.Join(agentHome, c.entry.SystemPromptFile)
	_, err := os.Stat(target)
	return err == nil
}

// AdvancedCapabilities returns the configured capability matrix from config.yaml.
func (c *ContainerScriptHarness) AdvancedCapabilities() api.HarnessAdvancedCapabilities {
	if c.entry.Capabilities == nil {
		return api.HarnessAdvancedCapabilities{Harness: c.entry.Harness}
	}
	caps := *c.entry.Capabilities
	caps.Harness = c.entry.Harness
	return caps
}

// GetCommand builds the harness CLI invocation from the declarative
// command spec. Falls back to baseArgs alone if no command is declared.
//
// The resume_flag string is split on whitespace, so a multi-token flag like
// "resume --last" becomes two argv entries. Single-token flags like "--continue"
// are unaffected.
func (c *ContainerScriptHarness) GetCommand(task string, resume bool, baseArgs []string) []string {
	cmd := c.entry.Command
	if cmd == nil {
		args := append([]string{}, baseArgs...)
		if task != "" {
			args = append(args, task)
		}
		return args
	}

	resumeTokens := []string{}
	if resume && cmd.ResumeFlag != "" {
		resumeTokens = strings.Fields(cmd.ResumeFlag)
	}

	args := append([]string{}, cmd.Base...)
	args = append(args, resumeTokens...)
	args = append(args, baseArgs...)

	if task != "" {
		switch cmd.TaskPosition {
		case "before_base_args":
			// place task and flag before baseArgs (rebuild)
			pre := append([]string{}, cmd.Base...)
			pre = append(pre, resumeTokens...)
			if cmd.TaskFlag != "" {
				pre = append(pre, cmd.TaskFlag, task)
			} else {
				pre = append(pre, task)
			}
			args = append(pre, baseArgs...)
		default: // after_base_args (default) and "positional"
			if cmd.TaskFlag != "" {
				args = append(args, cmd.TaskFlag, task)
			} else {
				args = append(args, task)
			}
		}
	}
	return args
}

// GetEnv returns the templated env for the harness.
func (c *ContainerScriptHarness) GetEnv(agentName, agentHome, unixUsername string) map[string]string {
	out := map[string]string{
		"SCION_AGENT_NAME": agentName,
	}
	for k, v := range c.entry.EnvTemplate {
		out[k] = expandEnvTemplate(v, agentName, agentHome, unixUsername)
	}
	return out
}

// GetTelemetryEnv returns harness-specific telemetry env vars.
// Container-script harnesses do not currently configure native telemetry env
// from Go; the script writes telemetry files into the container.
func (c *ContainerScriptHarness) GetTelemetryEnv() map[string]string { return nil }

// InjectAgentInstructions stages instruction content under
// agent_home/.scion/harness/inputs/instructions.md. The container-side
// pre-start script copies it to the harness-native location declared in
// config.yaml (instructions_file).
func (c *ContainerScriptHarness) InjectAgentInstructions(agentHome string, content []byte) error {
	return c.stageInputFile(agentHome, "instructions.md", content)
}

// InjectSystemPrompt stages system-prompt content under
// agent_home/.scion/harness/inputs/system-prompt.md. The container-side script
// honors system_prompt_mode (native | prepend_to_instructions | none).
func (c *ContainerScriptHarness) InjectSystemPrompt(agentHome string, content []byte) error {
	return c.stageInputFile(agentHome, "system-prompt.md", content)
}

// ResolveAuth returns a container-side auth plan: env candidates flow as files
// under .scion/harness/secrets/, and any harness-native file mappings declared
// in config_dir-bound auth metadata are surfaced to the runtime so the
// container can mount them. Final harness-native auth selection happens in the
// pre-start script.
func (c *ContainerScriptHarness) ResolveAuth(auth api.AuthConfig) (*api.ResolvedAuth, error) {
	resolved := &api.ResolvedAuth{
		Method:  "container-script",
		EnvVars: map[string]string{},
	}

	// Pass through non-secret discovery values so the script and broker can
	// use them. SCION_HARNESS_AUTH_CANDIDATES is a manifest-style hint.
	if auth.SelectedType != "" {
		resolved.EnvVars["SCION_HARNESS_SELECTED_AUTH"] = auth.SelectedType
	}

	// Forward any explicit auth env vars to the container. The script may
	// move them into final harness-native files. Harness-config metadata
	// could also drive this declaratively, but keeping it permissive at the
	// staging layer matches the design's "stage all candidates" guidance.
	addIfPresent := func(key, val string) {
		if val != "" {
			resolved.EnvVars[key] = val
		}
	}
	addIfPresent("ANTHROPIC_API_KEY", auth.AnthropicAPIKey)
	addIfPresent("CLAUDE_CODE_OAUTH_TOKEN", auth.ClaudeOAuthToken)
	addIfPresent("OPENAI_API_KEY", auth.OpenAIAPIKey)
	addIfPresent("GEMINI_API_KEY", auth.GeminiAPIKey)
	addIfPresent("GOOGLE_API_KEY", auth.GoogleAPIKey)
	addIfPresent("GOOGLE_CLOUD_PROJECT", auth.GoogleCloudProject)
	addIfPresent("GOOGLE_CLOUD_REGION", auth.GoogleCloudRegion)
	addIfPresent("CODEX_API_KEY", auth.CodexAPIKey)

	// Forward config-driven auth env vars. These come from harness config
	// metadata (auth.types[*].required_env) and are gathered by
	// GatherAuthWithEnv. They are additive — hardcoded fields above take
	// precedence if the same key appears in both.
	for k, v := range auth.EnvVars {
		if _, exists := resolved.EnvVars[k]; !exists {
			resolved.EnvVars[k] = v
		}
	}

	if auth.GoogleAppCredentials != "" {
		resolved.Files = append(resolved.Files, api.FileMapping{
			SourcePath:    auth.GoogleAppCredentials,
			ContainerPath: "~/.config/gcloud/application_default_credentials.json",
		})
	}
	if auth.ClaudeAuthFile != "" {
		resolved.Files = append(resolved.Files, api.FileMapping{
			SourcePath:    auth.ClaudeAuthFile,
			ContainerPath: "~/.claude/.credentials.json",
		})
	}
	if auth.CodexAuthFile != "" {
		resolved.Files = append(resolved.Files, api.FileMapping{
			SourcePath:    auth.CodexAuthFile,
			ContainerPath: "~/.codex/auth.json",
		})
	}
	if auth.OpenCodeAuthFile != "" {
		resolved.Files = append(resolved.Files, api.FileMapping{
			SourcePath:    auth.OpenCodeAuthFile,
			ContainerPath: "~/.local/share/opencode/auth.json",
		})
	}
	if auth.OAuthCreds != "" {
		resolved.Files = append(resolved.Files, api.FileMapping{
			SourcePath:    auth.OAuthCreds,
			ContainerPath: "~/.scion/oauth_creds.json",
		})
	}

	// The auth_candidates manifest written during Provision() captures the full
	// candidate set for the script. ResolveAuth provides the env/file material
	// the runtime needs to project secrets into the container.
	return resolved, nil
}

// ProvisionManifest is the JSON payload written to
// agent_home/.scion/harness/manifest.json and read by the container-side
// provisioner script.
type ProvisionManifest struct {
	SchemaVersion    int                       `json:"schema_version"`
	Command          string                    `json:"command"`
	AgentName        string                    `json:"agent_name"`
	AgentHome        string                    `json:"agent_home"`
	AgentWorkspace   string                    `json:"agent_workspace"`
	HarnessBundleDir string                    `json:"harness_bundle_dir"`
	HarnessConfig    config.HarnessConfigEntry `json:"harness_config"`
	Inputs           ProvisionInputs           `json:"inputs"`
	Outputs          ProvisionOutputs          `json:"outputs"`
	Platform         ProvisionPlatform         `json:"platform"`
}

type ProvisionInputs struct {
	Instructions   string `json:"instructions,omitempty"`
	SystemPrompt   string `json:"system_prompt,omitempty"`
	Telemetry      string `json:"telemetry,omitempty"`
	AuthCandidates string `json:"auth_candidates,omitempty"`
	MCPServers     string `json:"mcp_servers,omitempty"`
	ResolvedSkills string `json:"resolved_skills,omitempty"`
}

type ProvisionOutputs struct {
	Env          string `json:"env"`
	ResolvedAuth string `json:"resolved_auth"`
	Status       string `json:"status,omitempty"`
}

type ProvisionPlatform struct {
	GOOS   string `json:"goos,omitempty"`
	GOARCH string `json:"goarch,omitempty"`
}

// Provision stages the container bundle into the agent home. It does not
// execute provision.py. The bundle layout matches the design doc:
//
//	agent_home/.scion/harness/
//	  config.yaml
//	  provision.py
//	  manifest.json
//	  inputs/...
//	  outputs/  (writable by the script)
//	  secrets/  (populated by runtime secret projection)
//	agent_home/.scion/hooks/pre-start.d/20-harness-provision  (trusted wrapper)
func (c *ContainerScriptHarness) Provision(ctx context.Context, agentName, agentDir, agentHome, agentWorkspace string) error {
	bundleHostPath := filepath.Join(agentHome, ".scion", "harness")
	bundleContainerPath := containerBundlePath(agentHome)

	for _, sub := range []string{"", "inputs", "outputs", "secrets"} {
		if err := os.MkdirAll(filepath.Join(bundleHostPath, sub), 0755); err != nil {
			return fmt.Errorf("create bundle dir %q: %w", sub, err)
		}
	}

	// Copy config.yaml from the harness-config root into the bundle.
	if err := copyHarnessConfigFile(filepath.Join(c.configDirPath, "config.yaml"), filepath.Join(bundleHostPath, "config.yaml")); err != nil {
		return fmt.Errorf("stage config.yaml: %w", err)
	}

	// Copy provision.py if present in the harness-config dir.
	provisionSrc := filepath.Join(c.configDirPath, "provision.py")
	if fileExistsHelper(provisionSrc) {
		if err := copyHarnessConfigFile(provisionSrc, filepath.Join(bundleHostPath, "provision.py")); err != nil {
			return fmt.Errorf("stage provision.py: %w", err)
		}
		if err := os.Chmod(filepath.Join(bundleHostPath, "provision.py"), 0755); err != nil {
			return fmt.Errorf("chmod provision.py: %w", err)
		}
	}

	// Stage capture_auth.py and capture-auth-config.json into the bundle.
	if err := c.stageCaptureAuthConfig(agentHome); err != nil {
		return fmt.Errorf("stage capture-auth assets: %w", err)
	}

	// Copy dialect.yaml if present.
	dialectSrc := filepath.Join(c.configDirPath, "dialect.yaml")
	if fileExistsHelper(dialectSrc) {
		if err := copyHarnessConfigFile(dialectSrc, filepath.Join(bundleHostPath, "dialect.yaml")); err != nil {
			return fmt.Errorf("stage dialect.yaml: %w", err)
		}
	}

	// Stage scion_harness.py next to provision.py so the in-container script
	// can import it. The lib mode (vendored | injected) determines the source.
	if err := c.stageHarnessLib(bundleHostPath); err != nil {
		return err
	}

	manifest := ProvisionManifest{
		SchemaVersion:    1,
		Command:          "provision",
		AgentName:        agentName,
		AgentHome:        agentHome,
		AgentWorkspace:   agentWorkspace,
		HarnessBundleDir: bundleContainerPath,
		HarnessConfig:    c.entry,
		Inputs:           ProvisionInputs{},
		Outputs: ProvisionOutputs{
			Env:          filepath.Join(bundleContainerPath, "outputs", "env.json"),
			ResolvedAuth: filepath.Join(bundleContainerPath, "outputs", "resolved-auth.json"),
			Status:       filepath.Join(bundleContainerPath, "outputs", "status.json"),
		},
		Platform: ProvisionPlatform{GOOS: "linux", GOARCH: "amd64"},
	}

	// Reflect already-staged inputs in the manifest so the script can find them.
	if fileExistsHelper(filepath.Join(bundleHostPath, "inputs", "instructions.md")) {
		manifest.Inputs.Instructions = filepath.Join(bundleContainerPath, "inputs", "instructions.md")
	}
	if fileExistsHelper(filepath.Join(bundleHostPath, "inputs", "system-prompt.md")) {
		manifest.Inputs.SystemPrompt = filepath.Join(bundleContainerPath, "inputs", "system-prompt.md")
	}
	if fileExistsHelper(filepath.Join(bundleHostPath, "inputs", "telemetry.json")) {
		manifest.Inputs.Telemetry = filepath.Join(bundleContainerPath, "inputs", "telemetry.json")
	}
	if fileExistsHelper(filepath.Join(bundleHostPath, "inputs", "auth-candidates.json")) {
		manifest.Inputs.AuthCandidates = filepath.Join(bundleContainerPath, "inputs", "auth-candidates.json")
	}
	if fileExistsHelper(filepath.Join(bundleHostPath, "inputs", "mcp-servers.json")) {
		manifest.Inputs.MCPServers = filepath.Join(bundleContainerPath, "inputs", "mcp-servers.json")
	}
	if fileExistsHelper(filepath.Join(bundleHostPath, "inputs", "resolved-skills.json")) {
		manifest.Inputs.ResolvedSkills = filepath.Join(bundleContainerPath, "inputs", "resolved-skills.json")
	}

	manifestPath := filepath.Join(bundleHostPath, "manifest.json")
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, manifestData, 0644); err != nil {
		return fmt.Errorf("write manifest.json: %w", err)
	}

	if err := writeHookWrapper(agentHome, bundleContainerPath); err != nil {
		return fmt.Errorf("stage lifecycle hook wrapper: %w", err)
	}
	return nil
}

// ApplyAuthSettings stages resolved auth metadata into
// agent_home/.scion/harness/inputs/auth-candidates.json so the container-side
// script can finalize harness-native auth selection on every start/resume.
//
// For env-based credentials, the secret value is also written to
// agent_home/.scion/harness/secrets/<NAME> (mode 0600) and the path is recorded
// in the candidates file under env_secret_files. Scripts that need the actual
// value (e.g. Codex writes its API key into .codex/auth.json) read the file
// because sciontool harness provision strips secret env vars from the script's
// process environment for containment.
//
// For file-based credentials declared in required_files (e.g. auth-file mode),
// the file content is read from the host SourcePath and staged as a secret file
// under agent_home/.scion/harness/secrets/<NAME> (mode 0600). The path is
// recorded in file_secret_files in auth-candidates.json so the container-side
// script can write a fresh writable copy. The FileMapping is removed from
// resolved.Files so the runtime does not bind-mount the file read-only.
func (c *ContainerScriptHarness) ApplyAuthSettings(agentHome string, resolved *api.ResolvedAuth) error {
	if resolved == nil {
		return nil
	}

	envSecretFiles, err := c.stageEnvSecretFiles(agentHome, resolved.EnvVars)
	if err != nil {
		return err
	}

	fileSecretFiles, remainingFiles, err := c.stageFileSecretFiles(agentHome, resolved.Files)
	if err != nil {
		return err
	}
	// Remove staged-as-secret FileMappings from resolved so the runtime does
	// not also bind-mount them (which would create a read-only overlay that
	// prevents the container-side script from writing the file).
	resolved.Files = remainingFiles

	payload := map[string]interface{}{
		"schema_version":    1,
		"explicit_type":     c.entry.AuthSelectedType,
		"resolved_method":   resolved.Method,
		"env_vars":          sortedKeys(resolved.EnvVars),
		"env_secret_files":  envSecretFiles,
		"file_secret_files": fileSecretFiles,
		"files":             fileMappingsToJSON(resolved.Files),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal auth candidates: %w", err)
	}
	return c.stageInputFile(agentHome, "auth-candidates.json", data)
}

// stageFileSecretFiles reads the content of each FileMapping whose ContainerPath
// matches a required_files declaration in the harness config, writes it to
// agent_home/.scion/harness/secrets/<NAME> (mode 0600), and returns:
//   - fileSecretFiles: map of name -> "$HOME/.scion/harness/secrets/<NAME>" for
//     the staged secrets (to be written into file_secret_files in auth-candidates.json)
//   - remainingFiles: the FileMappings that were NOT staged as secrets and should
//     still be passed to the runtime for bind-mounting
//
// This prevents read-only bind-mounts for credential files that the container-side
// provisioner script needs to write (e.g. Codex auth.json).
func (c *ContainerScriptHarness) stageFileSecretFiles(agentHome string, files []api.FileMapping) (map[string]string, []api.FileMapping, error) {
	fileSecretFiles := map[string]string{}
	if len(files) == 0 || c.entry.Auth == nil {
		return fileSecretFiles, files, nil
	}

	// Build a lookup from container path suffix → credential name using the
	// harness config's required_files declarations.
	type fileReq struct {
		name         string
		targetSuffix string
	}
	var reqs []fileReq
	for _, authType := range c.entry.Auth.Types {
		for _, rf := range authType.RequiredFiles {
			if rf.Name == "" || rf.TargetSuffix == "" {
				continue
			}
			reqs = append(reqs, fileReq{name: rf.Name, targetSuffix: rf.TargetSuffix})
		}
	}
	if len(reqs) == 0 {
		return fileSecretFiles, files, nil
	}

	// Normalize a container path by expanding ~ to $HOME and stripping trailing
	// slashes so comparison is consistent. Absolute paths (e.g.
	// /home/scion/.codex/auth.json) are returned unchanged; tilde paths are
	// expanded to $HOME/... form.
	normalize := func(p string) string {
		p = strings.TrimRight(p, "/")
		if strings.HasPrefix(p, "~/") {
			p = "$HOME/" + p[2:]
		}
		return p
	}

	dir := filepath.Join(agentHome, ".scion", "harness", "secrets")
	dirCreated := false

	var remaining []api.FileMapping
	for _, f := range files {
		normCP := normalize(f.ContainerPath)

		// Find a matching required_file entry by container path suffix.
		// Use HasSuffix so that both tilde paths (~/.codex/auth.json →
		// $HOME/.codex/auth.json) and absolute paths
		// (/home/scion/.codex/auth.json) match the same suffix declaration.
		var matchedName string
		for _, req := range reqs {
			suffix := strings.TrimRight(req.targetSuffix, "/")
			if !strings.HasPrefix(suffix, "/") {
				suffix = "/" + suffix
			}
			if strings.HasSuffix(normCP, suffix) {
				matchedName = req.name
				break
			}
		}

		if matchedName == "" || !isSafeEnvName(matchedName) {
			// Not a declared file credential or unsafe name — keep as bind-mount.
			remaining = append(remaining, f)
			continue
		}

		if f.SourcePath == "" {
			// No host path to read; keep as bind-mount fallback.
			remaining = append(remaining, f)
			continue
		}

		content, err := os.ReadFile(f.SourcePath)
		if err != nil {
			return nil, nil, fmt.Errorf("read credential file %s (%s): %w", matchedName, f.SourcePath, err)
		}

		if !dirCreated {
			if err := os.MkdirAll(dir, 0700); err != nil {
				return nil, nil, fmt.Errorf("create secrets dir: %w", err)
			}
			dirCreated = true
		}

		target := filepath.Join(dir, matchedName)
		if err := os.WriteFile(target, content, 0600); err != nil {
			return nil, nil, fmt.Errorf("write file secret %s: %w", matchedName, err)
		}
		fileSecretFiles[matchedName] = "$HOME/.scion/harness/secrets/" + matchedName
		// Do NOT add to remaining — this file is now staged as a secret and
		// must not be bind-mounted.
	}

	return fileSecretFiles, remaining, nil
}

// stageEnvSecretFiles writes each non-empty env value to
// agent_home/.scion/harness/secrets/<NAME> with mode 0600 and returns a map
// of env-var name -> container-relative secret file path. The returned paths
// use the literal "$HOME/.scion/harness/secrets/<NAME>" form so they remain
// portable across host/container path layouts.
func (c *ContainerScriptHarness) stageEnvSecretFiles(agentHome string, envVars map[string]string) (map[string]string, error) {
	out := map[string]string{}
	if len(envVars) == 0 {
		return out, nil
	}
	dir := filepath.Join(agentHome, ".scion", "harness", "secrets")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}
	for _, name := range sortedKeys(envVars) {
		val := envVars[name]
		if val == "" {
			continue
		}
		// Defensive: only allow conventional env-var names so a hostile
		// caller cannot direct us to write outside the secrets dir.
		if !isSafeEnvName(name) {
			continue
		}
		target := filepath.Join(dir, name)
		if err := os.WriteFile(target, []byte(val), 0600); err != nil {
			return nil, fmt.Errorf("write secret %s: %w", name, err)
		}
		out[name] = "$HOME/.scion/harness/secrets/" + name
	}
	return out, nil
}

// isSafeEnvName accepts conventional POSIX env-var names: a letter or
// underscore, then letters, digits, or underscores.
func isSafeEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// ApplyMCPSettings stages the universal mcp_servers map into
// agent_home/.scion/harness/inputs/mcp-servers.json so the container-side
// provision.py can translate it into the harness's native MCP config. An empty
// or nil map is a no-op (no file written) so existing inline harness MCP
// configuration in home/ files keeps working unchanged.
func (c *ContainerScriptHarness) ApplyMCPSettings(agentHome string, mcpServers map[string]api.MCPServerConfig) error {
	if len(mcpServers) == 0 {
		return nil
	}
	payload := map[string]interface{}{
		"schema_version": 1,
		"mcp_servers":    mcpServers,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal mcp servers input: %w", err)
	}
	return c.stageInputFile(agentHome, "mcp-servers.json", data)
}

// ApplyTelemetrySettings stages telemetry config into
// agent_home/.scion/harness/inputs/telemetry.json.
func (c *ContainerScriptHarness) ApplyTelemetrySettings(agentHome string, telemetry *api.TelemetryConfig, env map[string]string) error {
	payload := map[string]interface{}{
		"schema_version": 1,
		"telemetry":      telemetry,
		"env":            env,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal telemetry input: %w", err)
	}
	return c.stageInputFile(agentHome, "telemetry.json", data)
}

// stageCaptureAuthConfig delegates to the shared StageCaptureAuthAssets
// helper to generate inputs/capture-auth-config.json from the harness
// config's auth.types.*.required_files declarations.
func (c *ContainerScriptHarness) stageCaptureAuthConfig(agentHome string) error {
	return StageCaptureAuthAssets(agentHome, c.configDirPath, c.entry.Auth)
}

// stageInputFile writes content under agent_home/.scion/harness/inputs/<name>.
// Inputs are not secrets; mode 0644 is fine.
func (c *ContainerScriptHarness) stageInputFile(agentHome, name string, content []byte) error {
	dir := filepath.Join(agentHome, ".scion", "harness", "inputs")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create inputs dir: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, name), content, 0644)
}

// containerBundlePath returns the path the script will see inside the
// container. The container always sees the bundle at $HOME/.scion/harness;
// agentHome on the host may be split storage. Use the trailing path so that
// inside the container, paths begin from the user's home directory.
func containerBundlePath(_ string) string {
	// The container-side $HOME differs from the host-side agentHome path.
	// Provisioner scripts read the manifest from $HOME/.scion/harness, so
	// we encode that path verbatim. The wrapper is responsible for setting
	// $HOME correctly.
	return "$HOME/.scion/harness"
}

// stageHarnessLib stages scion_harness.py into the bundle directory according
// to the provisioner.lib mode declared in config.yaml.
//
//   - vendored: copy from the installed harness-config directory; hard error if
//     the file is missing (packaging bug — the author declared vendored but
//     forgot to include the file).
//   - injected (or absent): write the binary-embedded copy (legacy behavior).
//
// Both modes log the staged source and LIB_VERSION for diagnostics.
func (c *ContainerScriptHarness) stageHarnessLib(bundleHostPath string) error {
	dst := filepath.Join(bundleHostPath, "scion_harness.py")
	libMode := "injected"
	if c.entry.Provisioner != nil && c.entry.Provisioner.Lib != "" {
		libMode = c.entry.Provisioner.Lib
	}

	var stagedSource string
	switch libMode {
	case "vendored":
		src := filepath.Join(c.configDirPath, "scion_harness.py")
		if !fileExistsHelper(src) {
			return fmt.Errorf("stage scion_harness.py: provisioner.lib is %q but %s does not exist — "+
				"run 'go generate ./harnesses/' or include scion_harness.py in the harness bundle", libMode, src)
		}
		if err := copyHarnessConfigFile(src, dst); err != nil {
			return fmt.Errorf("stage scion_harness.py (vendored): %w", err)
		}
		stagedSource = "vendored"
	default:
		if err := writeSharedHarnessHelper(dst); err != nil {
			return fmt.Errorf("stage scion_harness.py (injected): %w", err)
		}
		stagedSource = "injected"
	}

	staged, err := os.ReadFile(dst)
	if err == nil {
		version := parseLibVersion(string(staged))
		slog.Info("staged scion_harness.py", "source", stagedSource, "lib_version", version)
	}
	return nil
}

// parseLibVersion extracts the LIB_VERSION value from scion_harness.py source.
// Returns "unknown" if the marker is not found.
func parseLibVersion(src string) string {
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "LIB_VERSION") && strings.Contains(line, "=") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				v := strings.TrimSpace(parts[1])
				v = strings.Trim(v, "\"'")
				if v != "" {
					return v
				}
			}
		}
	}
	return "unknown"
}

func writeHookWrapper(agentHome, bundleContainerPath string) error {
	dir := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	wrapper := fmt.Sprintf(`#!/bin/sh
# Generated by scion. Do not edit by hand.
set -eu
exec sciontool harness provision --manifest "%s/manifest.json"
`, bundleContainerPath)
	target := filepath.Join(dir, "20-harness-provision")
	if err := os.WriteFile(target, []byte(wrapper), 0755); err != nil {
		return err
	}
	return nil
}

func copyHarnessConfigFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	_, err = io.Copy(out, in)
	return err
}

func fileExistsHelper(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Sort for determinism so the staged file content is reproducible.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

func fileMappingsToJSON(files []api.FileMapping) []map[string]string {
	out := make([]map[string]string, 0, len(files))
	for _, f := range files {
		out = append(out, map[string]string{
			"container_path": f.ContainerPath,
		})
	}
	return out
}

// expandEnvTemplate expands the simple {{ .Field }} placeholders supported in
// env_template. We avoid pulling in text/template to keep the surface narrow
// and predictable; only the documented fields are honored.
func expandEnvTemplate(value, agentName, agentHome, unixUsername string) string {
	replacements := map[string]string{
		"{{ .AgentName }}":    agentName,
		"{{ .AgentHome }}":    agentHome,
		"{{ .UnixUsername }}": unixUsername,
	}
	out := value
	for placeholder, replacement := range replacements {
		out = strings.ReplaceAll(out, placeholder, replacement)
	}
	return out
}

// StageCaptureAuthAssets stages capture_auth.py and its config file into the
// harness bundle directory at agentHome/.scion/harness/.
//
// configDirPath is the harness-config directory containing capture_auth.py.
// authMeta provides the required_files declarations used to generate the
// capture-auth-config.json input.
func StageCaptureAuthAssets(agentHome, configDirPath string, authMeta *config.HarnessAuthMetadata) error {
	bundleDir := filepath.Join(agentHome, ".scion", "harness")
	inputsDir := filepath.Join(bundleDir, "inputs")

	for _, dir := range []string{bundleDir, inputsDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("create dir %q: %w", dir, err)
		}
	}

	captureAuthSrc := filepath.Join(configDirPath, "capture_auth.py")
	if fileExistsHelper(captureAuthSrc) {
		dst := filepath.Join(bundleDir, "capture_auth.py")
		if err := copyHarnessConfigFile(captureAuthSrc, dst); err != nil {
			return fmt.Errorf("stage capture_auth.py: %w", err)
		}
		if err := os.Chmod(dst, 0755); err != nil {
			return fmt.Errorf("chmod capture_auth.py: %w", err)
		}
	}

	if authMeta == nil || len(authMeta.Types) == 0 {
		return nil
	}

	type credEntry struct {
		Key    string `json:"key"`
		Source string `json:"source"`
		Type   string `json:"type"`
		Target string `json:"target"`
	}

	var creds []credEntry
	for _, authType := range authMeta.Types {
		for _, rf := range authType.RequiredFiles {
			// Entries with empty TargetSuffix (e.g. gcloud-adc) are intentionally
			// excluded — these credentials come from well-known system paths and don't
			// use the suffix-based source derivation.
			if rf.Name == "" || rf.TargetSuffix == "" {
				continue
			}
			fileType := rf.Type
			if fileType == "" {
				fileType = "file"
			}
			suffix := rf.TargetSuffix
			if !strings.HasPrefix(suffix, "/") {
				suffix = "/" + suffix
			}
			source := "~" + suffix
			creds = append(creds, credEntry{
				Key:    rf.Name,
				Source: source,
				Type:   fileType,
				Target: source,
			})
		}
	}

	if len(creds) == 0 {
		return nil
	}

	sort.Slice(creds, func(i, j int) bool { return creds[i].Key < creds[j].Key })

	payload := map[string]interface{}{
		"schema_version": 1,
		"credentials":    creds,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal capture-auth config: %w", err)
	}
	return os.WriteFile(filepath.Join(inputsDir, "capture-auth-config.json"), data, 0644)
}
