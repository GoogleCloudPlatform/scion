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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func (m *AgentManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	// 0. Check if container already exists
	agents, err := m.Runtime.List(ctx, nil)
	if err == nil {
		for _, a := range agents {
			if a.ContainerID == opts.Name || strings.EqualFold(a.Name, opts.Name) || strings.EqualFold(strings.TrimPrefix(a.Name, "/"), opts.Name) {
				status := strings.ToLower(a.ContainerStatus)
				isRunning := strings.HasPrefix(status, "up") || status == "running"
				if isRunning {
					// If a new task is provided, we might want to recreate even if running
					// but if no task provided, we just return the running one
					if opts.Task == "" {
						a.Detached = true
						if opts.Detached != nil {
							a.Detached = *opts.Detached
						}
						a.Phase = "running"
						return &a, nil
					}
				}
				// If it exists but not running (or we have a new task), we delete it so we can recreate it
				if err := m.Runtime.Delete(ctx, a.ContainerID); err != nil {
					return nil, fmt.Errorf("failed to cleanup existing container: %w", err)
				}
			}
		}
	}

	projectDir, err := config.GetResolvedProjectDir(opts.GrovePath)
	if err != nil {
		return nil, err
	}
	groveName := config.GetGroveName(projectDir)

	// If resuming, verify the agent exists before proceeding
	if opts.Resume {
		agentDir := filepath.Join(projectDir, "agents", opts.Name)
		if _, err := os.Stat(agentDir); os.IsNotExist(err) {
			return nil, fmt.Errorf("cannot resume agent '%s': agent does not exist. Use 'scion start' to create a new agent", opts.Name)
		}
	}

	if opts.BrokerMode {
		ctx = api.ContextWithBrokerMode(ctx)
	}
	if opts.GitClone != nil {
		ctx = api.ContextWithGitClone(ctx, opts.GitClone)
	}

	// Build inline config for GetAgent, ensuring --harness-auth is applied
	// before harness Provision() runs (which reads auth_selectedType to decide
	// which env vars to inject).
	var startInlineConfig *api.ScionConfig
	if opts.HarnessAuth != "" {
		startInlineConfig = &api.ScionConfig{AuthSelectedType: opts.HarnessAuth}
	}

	util.Debugf("Start: calling GetAgent name=%s template=%q image=%q harnessConfig=%q grovePath=%q profile=%q",
		opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.GrovePath, opts.Profile)
	agentDir, agentHome, agentWorkspace, finalScionCfg, err := GetAgent(ctx, opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.GrovePath, opts.Profile, "", opts.Branch, opts.Workspace, startInlineConfig)
	if err != nil {
		return nil, err
	}
	if finalScionCfg != nil {
		util.Debugf("Start: GetAgent returned config: harness=%q harnessConfig=%q defaultHarnessConfig=%q image=%q",
			finalScionCfg.Harness, finalScionCfg.HarnessConfig, finalScionCfg.DefaultHarnessConfig, finalScionCfg.Image)
	} else {
		util.Debugf("Start: GetAgent returned nil config")
	}

	promptFile := filepath.Join(agentDir, "prompt.md")
	promptFileContent := ""
	if content, err := os.ReadFile(promptFile); err == nil {
		promptFileContent = strings.TrimSpace(string(content))
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to read prompt file %s: %w", promptFile, err)
	}

	task := opts.Task

	if task == "" && !opts.Resume {
		task = promptFileContent
	} else if task != "" {
		// Explicit prompt always wins — write/overwrite prompt.md
		_ = os.WriteFile(promptFile, []byte(task), 0644)
	}

	// Load settings for registry resolution
	settings, settingsWarnings, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		util.Debugf("Start: LoadEffectiveSettings(%s) error: %v", projectDir, err)
	}
	config.PrintDeprecationWarnings(settingsWarnings)

	harnessName := ""
	if finalScionCfg != nil {
		harnessName = finalScionCfg.Harness
	}

	// Resolve harness config name using the unified resolution chain.
	// finalScionCfg acts as both stored config (resume) and template config.
	harnessConfigName := ""
	if hcRes, err := config.ResolveHarnessConfigName(config.HarnessConfigInputs{
		CLIFlag:      opts.HarnessConfig,
		StoredConfig: finalScionCfg,
		TemplateCfg:  finalScionCfg,
		Settings:     settings,
		ProfileName:  opts.Profile,
	}); err == nil {
		harnessConfigName = hcRes.Name
	}

	// Default values
	resolvedImage := ""
	unixUsername := "root"
	profileName := opts.Profile

	util.Debugf("image resolution: starting, harnessConfigName=%s", harnessConfigName)

	// Load on-disk harness-config for the container user and image (base layer).
	// The settings map may not define harness_configs, but the on-disk
	// config.yaml (seeded from harness embeds) always has the user field.
	// Also check template directories since harness-configs may be bundled
	// inside templates (§3.4 of agnostic-template-design).
	if harnessConfigName != "" {
		var templatePaths []string
		// Prefer opts.Template when it is an absolute path (e.g. hydrated
		// template cache path from the broker). The display name stored in
		// finalScionCfg.Info.Template (e.g. "web-dev") may not resolve in
		// the grove, but the original opts.Template path points to the
		// actual template directory containing harness-configs/.
		templateName := ""
		if opts.Template != "" && filepath.IsAbs(opts.Template) {
			templateName = opts.Template
		}
		if templateName == "" {
			if finalScionCfg != nil && finalScionCfg.Info != nil {
				templateName = finalScionCfg.Info.Template
			}
		}
		if templateName == "" {
			templateName = opts.Template
		}
		if templateName != "" {
			if chain, err := config.GetTemplateChainInGrove(templateName, opts.GrovePath); err == nil {
				for _, tpl := range chain {
					templatePaths = append(templatePaths, tpl.Path)
				}
			}
		}
		if hcDir, err := config.FindHarnessConfigDir(harnessConfigName, projectDir, templatePaths...); err == nil {
			if hcDir.Config.Image != "" {
				resolvedImage = hcDir.Config.Image
				util.Debugf("image resolution: from on-disk harness-config image=%s path=%s", resolvedImage, hcDir.Path)
			}
			if hcDir.Config.User != "" {
				unixUsername = hcDir.Config.User
			}
		} else {
			util.Debugf("image resolution: on-disk harness-config %q not found: %v", harnessConfigName, err)
		}
	}

	if settings != nil && harnessConfigName != "" {
		hConfig, err := settings.ResolveHarnessConfig(opts.Profile, harnessConfigName)
		if err == nil {
			if hConfig.Image != "" {
				resolvedImage = hConfig.Image
				util.Debugf("image resolution: from settings harness-config image=%s", resolvedImage)
			}
			if hConfig.User != "" {
				unixUsername = hConfig.User
			}
		} else {
			util.Debugf("image resolution: settings harness-config %q not found", harnessConfigName)
		}
	}

	if settings != nil {
		if profileName == "" {
			profileName = settings.ActiveProfile
		}
		// Merge settings-level telemetry config into finalScionCfg so that
		// cloud export configuration (endpoint, protocol, batch, etc.) and
		// the TelemetryEnabled flag are available at start time. This mirrors
		// the merge in ProvisionAgent.
		if settings.Telemetry != nil {
			util.Debugf("Start: merging settings telemetry config (cloud=%v, endpoint=%q)",
				settings.Telemetry.Cloud != nil, func() string {
					if settings.Telemetry.Cloud != nil {
						return settings.Telemetry.Cloud.Endpoint
					}
					return ""
				}())
			settingsCfg := &api.ScionConfig{
				Telemetry: config.ConvertV1TelemetryToAPI(settings.Telemetry),
			}
			finalScionCfg = config.MergeScionConfig(settingsCfg, finalScionCfg)
		} else {
			util.Debugf("Start: settings.Telemetry is nil, skipping telemetry merge")
		}
	}

	// Apply User from ScionConfig (higher priority than harness-config/settings)
	if finalScionCfg != nil && finalScionCfg.User != "" {
		unixUsername = finalScionCfg.User
		util.Debugf("user resolution: from ScionConfig user=%s", unixUsername)
	}

	var warnings []string

	if finalScionCfg != nil && finalScionCfg.Image != "" {
		resolvedImage = finalScionCfg.Image
		util.Debugf("image resolution: from agent/template config image=%s", resolvedImage)
	}

	// Apply image_registry rewrite to whatever image was resolved above.
	// This rewrites the registry prefix for scion-* images. An explicit
	// --image flag below takes full precedence (no rewrite).
	if settings != nil && resolvedImage != "" {
		imageRegistry := settings.ResolveImageRegistry(opts.Profile)
		if imageRegistry != "" {
			rewritten := config.RewriteImageRegistry(resolvedImage, imageRegistry)
			if rewritten != resolvedImage {
				util.Debugf("image resolution: image_registry rewrite %s -> %s", resolvedImage, rewritten)
				resolvedImage = rewritten
			}
		}
	}

	// CLI Overrides
	if opts.Image != "" {
		resolvedImage = opts.Image
		util.Debugf("image resolution: from CLI --image flag image=%s", resolvedImage)
	}

	if resolvedImage == "" {
		util.Debugf("image resolution FAILED: harnessConfigName=%q, finalScionCfg.Image=%q, opts.Image=%q, projectDir=%s",
			harnessConfigName, finalScionCfg.Image, opts.Image, projectDir)
		return nil, fmt.Errorf("no container image resolved for agent %q. Set 'image' in the harness-config config.yaml, specify --image, or configure a harness-config in settings", opts.Name)
	}

	util.Debugf("image resolution: final image=%s", resolvedImage)

	h := harness.New(harnessName)

	// 3. Resolve credentials via new auth pipeline

	// Inject profile/harness-config env vars into opts.Env BEFORE building the
	// auth overlay so that GatherAuthWithEnv can see credentials like
	// GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION declared in the active
	// settings profile.
	if settings != nil && !opts.BrokerMode {
		var settingsEnv map[string]string
		if harnessConfigName != "" {
			if hcEntry, err := settings.ResolveHarnessConfig(profileName, harnessConfigName); err == nil {
				settingsEnv = hcEntry.Env
			}
		} else if profileName != "" {
			if p, ok := settings.Profiles[profileName]; ok {
				settingsEnv = p.Env
			}
		}
		if len(settingsEnv) > 0 {
			if opts.Env == nil {
				opts.Env = make(map[string]string)
			}
			for k, v := range settingsEnv {
				if _, exists := opts.Env[k]; !exists {
					opts.Env[k] = v
				}
			}
		}
	}

	// Build a temporary auth overlay from resolved env-type secrets so auth
	// resolution can detect credentials without mutating opts.Env (which is
	// later projected into the container environment).
	authEnvOverlay := buildAuthEnvOverlay(opts.Env, opts.ResolvedSecrets)

	var auth api.AuthConfig
	var resolvedAuth *api.ResolvedAuth
	if !opts.NoAuth {
		auth = harness.GatherAuthWithEnv(authEnvOverlay, !opts.BrokerMode)
		if opts.BrokerMode {
			harness.OverlayFileSecrets(&auth, opts.ResolvedSecrets)
		}
		util.Debugf("auth: gathered credentials — selectedType=%q, hasGeminiKey=%t, hasGoogleKey=%t, hasOAuth=%t, hasADC=%t, hasAnthropicKey=%t, hasClaudeOAuthToken=%t, cloudProject=%q, gcpMetadataMode=%q, brokerMode=%t",
			auth.SelectedType,
			auth.GeminiAPIKey != "",
			auth.GoogleAPIKey != "",
			auth.OAuthCreds != "",
			auth.GoogleAppCredentials != "",
			auth.AnthropicAPIKey != "",
			auth.ClaudeOAuthToken != "",
			auth.GoogleCloudProject,
			auth.GCPMetadataMode,
			opts.BrokerMode,
		)
		harness.OverlaySettings(&auth, h, agentDir)
		// Apply CLI harness auth override (--harness-auth) before resolution.
		// This has highest priority, overriding settings, templates, and harness configs.
		if opts.HarnessAuth != "" {
			auth.SelectedType = opts.HarnessAuth
		}
		util.Debugf("auth: after overlay — selectedType=%q", auth.SelectedType)
		resolved, err := h.ResolveAuth(auth)
		if err != nil {
			return nil, fmt.Errorf("auth resolution failed: %w", err)
		}
		// Keep a copy of the full resolved auth material for secret filtering.
		resolvedForSecretFilter := *resolved
		if opts.BrokerMode {
			// File projection is handled by writeFileSecrets() from ResolvedSecrets
			// at container launch, not by applyResolvedAuth from local paths.
			resolved.Files = nil
		}
		util.Debugf("auth: resolved — method=%q, envVars=%v, files=%d", resolved.Method, resolved.EnvVars, len(resolved.Files))
		if err := harness.ValidateAuth(resolved); err != nil {
			return nil, fmt.Errorf("auth validation failed: %w", err)
		}
		// Allow harnesses to update their native settings files (e.g. Gemini settings.json)
		if applier, ok := h.(api.AuthSettingsApplier); ok {
			if err := applier.ApplyAuthSettings(agentHome, resolved); err != nil {
				return nil, fmt.Errorf("failed to apply auth settings: %w", err)
			}
			util.Debugf("auth: applied harness-specific settings for %q", harnessName)
		}
		resolvedAuth = resolved
		opts.ResolvedSecrets = filterResolvedSecretsForResolvedAuth(opts.ResolvedSecrets, &resolvedForSecretFilter)

		// Persist the resolved auth method so it can be reported to the Hub.
		// For auto-detected auth, opts.HarnessAuth may be empty; capture the
		// actual method the harness selected (e.g. "api-key", "vertex-ai").
		if opts.HarnessAuth == "" && resolved.Method != "" {
			opts.HarnessAuth = resolved.Method
		}

		// Surface resolved auth method so CLI can display it
		authDetail := resolved.Method
		if nativeType, ok := resolved.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"]; ok {
			authDetail = fmt.Sprintf("%s (%s)", resolved.Method, nativeType)
		}
		warnings = append(warnings, fmt.Sprintf("Auth: resolved as %s", authDetail))
	}

	// 4. Launch container
	detached := true

	if finalScionCfg != nil {
		detached = finalScionCfg.IsDetached()
	}

	if opts.Detached != nil {
		detached = *opts.Detached
	}

	exists, err := m.Runtime.ImageExists(ctx, resolvedImage)
	if err != nil || !exists {
		if err := m.Runtime.PullImage(ctx, resolvedImage); err != nil {
			return nil, fmt.Errorf("failed to pull image '%s': %w", resolvedImage, err)
		}
	}

	template := ""
	if finalScionCfg != nil && finalScionCfg.Info != nil {
		template = finalScionCfg.Info.Template
	}
	// Prefer human-friendly template slug over cache path or UUID
	if opts.TemplateName != "" {
		template = opts.TemplateName
	}

	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}
	opts.Env["SCION_AGENT_NAME"] = opts.Name
	opts.Env["SCION_GROVE"] = groveName
	if template != "" {
		opts.Env["SCION_TEMPLATE_NAME"] = template
	} else {
		opts.Env["SCION_TEMPLATE_NAME"] = "custom"
	}
	// Full template reference (cache path, URI, or name) for debugging
	if opts.Template != "" {
		opts.Env["SCION_TEMPLATE"] = opts.Template
	}
	if _, ok := opts.Env["SCION_BROKER_NAME"]; !ok {
		opts.Env["SCION_BROKER_NAME"] = "local"
	}
	// Inject harness and model identifiers for telemetry labeling.
	if _, ok := opts.Env["SCION_HARNESS"]; !ok && harnessName != "" {
		opts.Env["SCION_HARNESS"] = harnessName
	}
	if _, ok := opts.Env["SCION_MODEL"]; !ok && finalScionCfg != nil && finalScionCfg.Model != "" {
		opts.Env["SCION_MODEL"] = finalScionCfg.Model
	}
	if _, ok := opts.Env["SCION_CREATOR"]; !ok {
		if u, err := user.Current(); err == nil {
			opts.Env["SCION_CREATOR"] = u.Username
		}
	}

	// Determine whether hub is explicitly disabled in grove settings.
	// When disabled, we suppress hub env var injection from agent config
	// and template env sections (but not from caller-provided opts.Env,
	// which may come from an authoritative source like the runtime broker).
	hubDisabled := settings != nil && settings.IsHubExplicitlyDisabled()

	// Inject agent limit env vars from scion config
	if finalScionCfg != nil {
		if finalScionCfg.MaxTurns > 0 {
			opts.Env["SCION_MAX_TURNS"] = strconv.Itoa(finalScionCfg.MaxTurns)
		}
		if finalScionCfg.MaxModelCalls > 0 {
			opts.Env["SCION_MAX_MODEL_CALLS"] = strconv.Itoa(finalScionCfg.MaxModelCalls)
		}
		if finalScionCfg.MaxDuration != "" {
			opts.Env["SCION_MAX_DURATION"] = finalScionCfg.MaxDuration
		}
		// Agent-level hub endpoint takes highest priority, overriding
		// grove settings and server config values passed via opts.Env.
		if !hubDisabled && finalScionCfg.Hub != nil && finalScionCfg.Hub.Endpoint != "" {
			opts.Env["SCION_HUB_ENDPOINT"] = finalScionCfg.Hub.Endpoint
			opts.Env["SCION_HUB_URL"] = finalScionCfg.Hub.Endpoint
		}
	}

	// If hub endpoint not yet set from agent config or caller's opts.Env,
	// check grove settings so locally-started agents in hub-connected
	// groves also get hub connectivity.
	if _, hubSet := opts.Env["SCION_HUB_ENDPOINT"]; !hubSet {
		if groveSettings, err := config.LoadSettings(projectDir); err == nil {
			if groveSettings.IsHubEnabled() {
				if ep := groveSettings.GetHubEndpoint(); ep != "" {
					opts.Env["SCION_HUB_ENDPOINT"] = ep
					opts.Env["SCION_HUB_URL"] = ep
				}
			}
		}
	}
	// If hub endpoint is now set but no auth token, resolve dev auth token
	// from the host filesystem (env vars or ~/.scion/dev-token file).
	if _, ok := opts.Env["SCION_HUB_ENDPOINT"]; ok {
		if _, tokenSet := opts.Env["SCION_AUTH_TOKEN"]; !tokenSet {
			if token := apiclient.ResolveDevToken(); token != "" {
				opts.Env["SCION_AUTH_TOKEN"] = token
			}
		}
	}

	// Explicit SCION_HUB_ENDPOINT in scion config env section takes
	// final priority. This allows templates to specify a container-
	// accessible endpoint (e.g. http://host.docker.internal:8080)
	// that differs from the host-level hub endpoint.
	if !hubDisabled && finalScionCfg != nil && finalScionCfg.Env != nil {
		if ep, ok := finalScionCfg.Env["SCION_HUB_ENDPOINT"]; ok && ep != "" {
			expandedEp, _ := util.ExpandEnv(ep)
			if expandedEp != "" {
				opts.Env["SCION_HUB_ENDPOINT"] = expandedEp
				opts.Env["SCION_HUB_URL"] = expandedEp
			}
		}
	}

	// When hub is explicitly disabled, strip hub env vars from both
	// opts.Env and the scion config env section to prevent leakage
	// through buildAgentEnv (which processes scionCfg.Env independently).
	// In broker mode, never strip hub env vars — the broker handler is
	// authoritative about hub connectivity and the grove settings on the
	// broker host may not accurately reflect the hub-connected state.
	if hubDisabled && !opts.BrokerMode {
		delete(opts.Env, "SCION_HUB_ENDPOINT")
		delete(opts.Env, "SCION_HUB_URL")
		if finalScionCfg != nil && finalScionCfg.Env != nil {
			delete(finalScionCfg.Env, "SCION_HUB_ENDPOINT")
			delete(finalScionCfg.Env, "SCION_HUB_URL")
		}
	}

	// Write the agent token to the canonical token file in the agent home
	// directory so that all processes inside the container read from the file
	// rather than relying on an environment variable that goes stale after
	// token refresh.
	if token, ok := opts.Env["SCION_AUTH_TOKEN"]; ok && token != "" {
		scionDir := filepath.Join(agentHome, ".scion")
		if err := os.MkdirAll(scionDir, 0700); err != nil {
			util.Debugf("Start: failed to create .scion dir for token file: %v", err)
		} else {
			tokenPath := filepath.Join(scionDir, "scion-token")
			tmp := tokenPath + ".tmp"
			if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
				util.Debugf("Start: failed to write token file: %v", err)
			} else if err := os.Rename(tmp, tokenPath); err != nil {
				util.Debugf("Start: failed to rename token file: %v", err)
				os.Remove(tmp)
			} else {
				util.Debugf("Start: wrote agent token to %s", tokenPath)
			}
		}
		delete(opts.Env, "SCION_AUTH_TOKEN")
	}

	// Resolve Docker host networking: when the hub endpoint is localhost or was
	// translated to host.docker.internal, use --network=host so the container
	// can reach the host's loopback interface directly. This also rewrites any
	// bridge hostnames back to localhost in opts.Env.
	dockerNetworkMode := runtime.ResolveDockerNetworking(m.Runtime.Name(), opts.Env)
	if dockerNetworkMode != "" {
		opts.Env["SCION_NETWORK_MODE"] = dockerNetworkMode
	}

	// Persist harness auth override to scion-agent.json so sciontool inside the container sees it.
	// The actual auth resolution override is applied earlier in the auth gathering block.
	if opts.HarnessAuth != "" {
		if finalScionCfg == nil {
			finalScionCfg = &api.ScionConfig{}
		}
		finalScionCfg.AuthSelectedType = opts.HarnessAuth
		cfgData, marshalErr := json.MarshalIndent(finalScionCfg, "", "  ")
		if marshalErr == nil {
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), cfgData, 0644)
		}
	}

	// Apply CLI telemetry override (--enable-telemetry / --disable-telemetry).
	// This has highest priority, overriding settings, templates, and harness configs.
	if opts.TelemetryOverride != nil {
		util.Debugf("Start: applying TelemetryOverride=%v", *opts.TelemetryOverride)
		if finalScionCfg == nil {
			finalScionCfg = &api.ScionConfig{}
		}
		if finalScionCfg.Telemetry == nil {
			finalScionCfg.Telemetry = &api.TelemetryConfig{}
		}
		finalScionCfg.Telemetry.Enabled = opts.TelemetryOverride
	} else {
		util.Debugf("Start: no TelemetryOverride set")
	}

	// Allow harnesses to reconcile native telemetry config files with the
	// effective telemetry settings before container launch.
	var effectiveTelemetry *api.TelemetryConfig
	if finalScionCfg != nil {
		effectiveTelemetry = finalScionCfg.Telemetry
	}
	if telemetryApplier, ok := h.(api.TelemetrySettingsApplier); ok {
		if err := telemetryApplier.ApplyTelemetrySettings(agentHome, effectiveTelemetry, opts.Env); err != nil {
			return nil, fmt.Errorf("failed to apply telemetry settings: %w", err)
		}
	}

	// Inject telemetry config as env vars for sciontool.
	// Only set vars not already present (respecting explicit overrides).
	if finalScionCfg != nil && finalScionCfg.Telemetry != nil {
		telemetryEnv := config.TelemetryConfigToEnv(finalScionCfg.Telemetry)
		util.Debugf("Start: injecting %d telemetry env vars", len(telemetryEnv))
		for k, v := range telemetryEnv {
			if _, exists := opts.Env[k]; !exists {
				opts.Env[k] = v
			}
		}
	}

	agentEnv, envWarnings, missingEnvKeys := buildAgentEnv(finalScionCfg, opts.Env)
	if len(missingEnvKeys) > 0 {
		sort.Strings(missingEnvKeys)
		if opts.BrokerMode {
			// In broker mode, empty env vars are passthrough markers that the
			// hub didn't resolve (e.g., profile-level keys irrelevant to the
			// selected harness). Warn but don't block agent start.
			envWarnings = append(envWarnings, fmt.Sprintf("Warning: %d environment variable(s) have no value and will be omitted: %s",
				len(missingEnvKeys), strings.Join(missingEnvKeys, ", ")))
		} else {
			return nil, fmt.Errorf("cannot start agent: %d required environment variable(s) have no value: %s",
				len(missingEnvKeys), strings.Join(missingEnvKeys, ", "))
		}
	}
	warnings = append(warnings, envWarnings...)

	// Determine the effective workspace path. If agentWorkspace is empty but we have
	// a volume mounted to /workspace (e.g., shared worktree case), use that source path.
	effectiveWorkspace := agentWorkspace
	if effectiveWorkspace == "" && finalScionCfg != nil {
		effectiveWorkspace = extractWorkspaceFromVolumes(finalScionCfg.Volumes)
	}

	repoRoot := ""
	if effectiveWorkspace != "" && util.IsGitRepoDir(effectiveWorkspace) {
		commonDir, err := util.GetCommonGitDir(effectiveWorkspace)
		if err == nil {
			repoRoot = filepath.Dir(commonDir)
		}
	} else if util.IsGitRepoDir(projectDir) {
		repoRoot, _ = util.RepoRootDir(projectDir)
	}

	// Telemetry defaults to enabled when not explicitly set to false.
	telemetryEnabled := finalScionCfg != nil && finalScionCfg.Telemetry != nil &&
		(finalScionCfg.Telemetry.Enabled == nil || *finalScionCfg.Telemetry.Enabled)
	util.Debugf("Start: telemetryEnabled=%v (hasCfg=%v, hasTelem=%v, cloud=%v)",
		telemetryEnabled,
		finalScionCfg != nil,
		finalScionCfg != nil && finalScionCfg.Telemetry != nil,
		finalScionCfg != nil && finalScionCfg.Telemetry != nil && finalScionCfg.Telemetry.Cloud != nil)

	// Compute the container-side workspace path for volume mount targets.
	containerWorkspace := runtime.ResolveContainerWorkspace(repoRoot, effectiveWorkspace, opts.GitClone)

	// Inject shared directory volumes from grove settings or opts (hub-dispatched)
	var effectiveSharedDirs []api.SharedDir
	if settings != nil && len(settings.SharedDirs) > 0 {
		effectiveSharedDirs = settings.SharedDirs
	} else if len(opts.SharedDirs) > 0 {
		effectiveSharedDirs = opts.SharedDirs
	}
	var sharedDirVolumes []api.VolumeMount
	if len(effectiveSharedDirs) > 0 {
		if err := config.EnsureSharedDirs(projectDir, effectiveSharedDirs); err != nil {
			util.Debugf("Start: failed to ensure shared dirs: %v", err)
		}
		sdVolumes, err := config.SharedDirsToVolumeMounts(projectDir, effectiveSharedDirs, containerWorkspace)
		if err != nil {
			util.Debugf("Start: failed to resolve shared dir volumes: %v", err)
		} else {
			sharedDirVolumes = sdVolumes
			// Add SCION_VOLUMES env var for discoverability
			opts.Env["SCION_VOLUMES"] = "/scion-volumes"
		}
	}

	runCfg := runtime.RunConfig{
		Name:               opts.Name,
		Template:           template,
		UnixUsername:       unixUsername,
		Image:              resolvedImage,
		HomeDir:            agentHome,
		Workspace:          effectiveWorkspace,
		RepoRoot:           repoRoot,
		ContainerWorkspace: containerWorkspace,
		ResolvedAuth:       resolvedAuth,
		Harness:            h,
		TelemetryEnabled:   telemetryEnabled,
		Task: func() string {
			// When task_flag is set, task is delivered via CommandArgs instead
			if finalScionCfg != nil && finalScionCfg.TaskFlag != "" {
				return ""
			}
			return task
		}(),
		CommandArgs: func() []string {
			var args []string
			if finalScionCfg != nil {
				args = finalScionCfg.CommandArgs
				if finalScionCfg.Model != "" {
					// Prepend model flag so it appears before user args but is passed in baseArgs
					args = append([]string{"--model", finalScionCfg.Model}, args...)
				}
				// If task_flag is configured, append task as a flag value
				if finalScionCfg.TaskFlag != "" && task != "" {
					args = append(args, finalScionCfg.TaskFlag, task)
				}
			}
			return args
		}(),
		Env:             agentEnv,
		ResolvedSecrets: opts.ResolvedSecrets,
		Volumes: func() []api.VolumeMount {
			var volumes []api.VolumeMount
			if finalScionCfg != nil {
				// If we extracted effectiveWorkspace from a /workspace volume mount,
				// filter it out to avoid a duplicate mount (the buildCommonRunArgs
				// will handle the workspace mount properly with worktree support).
				if effectiveWorkspace != "" && effectiveWorkspace != agentWorkspace {
					volumes = filterWorkspaceVolume(finalScionCfg.Volumes)
				} else {
					volumes = finalScionCfg.Volumes
				}
			}
			// Append shared directory volumes
			volumes = append(volumes, sharedDirVolumes...)
			return volumes
		}(),
		Resources: func() *api.ResourceSpec {
			if finalScionCfg != nil {
				return finalScionCfg.Resources
			}
			return nil
		}(),
		Kubernetes: func() *api.KubernetesConfig {
			if finalScionCfg != nil {
				return finalScionCfg.Kubernetes
			}
			return nil
		}(),
		GitClone:             opts.GitClone,
		SharedDirs:           effectiveSharedDirs,
		BrokerMode:           opts.BrokerMode,
		Debug:                util.DebugEnabled(),
		Resume:               opts.Resume,
		MetadataInterception: hasMetadataInterception(agentEnv),
		ExtraHosts:           runtime.BridgeExtraHosts(m.Runtime.Name(), agentEnv),
		NetworkMode:          dockerNetworkMode,
		Labels: func() map[string]string {
			l := map[string]string{
				"scion.agent":          "true",
				"scion.name":           api.Slugify(opts.Name),
				"scion.grove":          groveName,
				"scion.template":       template,
				"scion.harness_config": harnessConfigName,
				"scion.harness_auth":   opts.HarnessAuth,
			}
			// Add grove_id label for grove-scoped agent isolation.
			// In broker/hosted mode this comes from the SCION_GROVE_ID env var
			// injected by the hub dispatcher.
			if groveID := opts.Env["SCION_GROVE_ID"]; groveID != "" {
				l["scion.grove_id"] = groveID
			}
			return l
		}(),
		Annotations: map[string]string{
			"scion.grove_path": projectDir,
		},
	}
	id, err := m.Runtime.Run(ctx, runCfg)
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") ||
			strings.Contains(err.Error(), "tmux: command not found") ||
			strings.Contains(err.Error(), "tmux: not found") {
			return nil, fmt.Errorf("failed to launch container: tmux binary not found in image '%s'. "+
				"Ensure the image has tmux installed. Error: %w", resolvedImage, err)
		}
		return nil, fmt.Errorf("failed to launch container: %w", err)
	}

	status := "running"
	if opts.Resume {
		status = "resumed"
	}
	_ = UpdateAgentConfig(opts.Name, opts.GrovePath, status, m.Runtime.Name(), profileName)

	// Fetch fresh info and verify the container is actually running
	slug := api.Slugify(opts.Name)
	allAgents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	if err == nil {
		for _, a := range allAgents {
			if a.ContainerID == id || strings.EqualFold(a.Name, opts.Name) {
				// Check if the container has already exited
				containerStatus := strings.ToLower(a.ContainerStatus)
				if strings.Contains(containerStatus, "exited") || strings.Contains(containerStatus, "dead") {
					// Try to get logs for diagnosis
					logs, _ := m.Runtime.GetLogs(ctx, id)
					_ = m.Runtime.Delete(ctx, id)
					return nil, fmt.Errorf("container started but exited immediately (status: %s). Container logs:\n%s", a.ContainerStatus, logs)
				}
				a.Detached = detached
				a.Warnings = warnings
				a.Phase = status
				a.HarnessConfig = harnessConfigName
				a.HarnessAuth = opts.HarnessAuth
				a.Profile = profileName
				return &a, nil
			}
		}
	}

	// Container ID returned but not found in listing — it may have exited and been removed
	warnings = append(warnings, "Container started but could not be verified as running")
	return &api.AgentInfo{ID: id, Name: opts.Name, Phase: status, Detached: detached, Warnings: warnings, HarnessConfig: harnessConfigName, HarnessAuth: opts.HarnessAuth, Profile: profileName}, nil
}

// extractWorkspaceFromVolumes finds a volume mounted to /workspace and returns its source path.
// This is used when an agent shares an existing worktree from another agent.
func extractWorkspaceFromVolumes(volumes []api.VolumeMount) string {
	for _, v := range volumes {
		if v.Target == "/workspace" {
			return v.Source
		}
	}
	return ""
}

// filterWorkspaceVolume removes volumes targeting /workspace from the list.
// This is used when the workspace will be handled by the RepoRoot/Workspace logic
// in buildCommonRunArgs instead of as a generic volume mount.
func filterWorkspaceVolume(volumes []api.VolumeMount) []api.VolumeMount {
	var filtered []api.VolumeMount
	for _, v := range volumes {
		if v.Target != "/workspace" {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

func buildAgentEnv(scionCfg *api.ScionConfig, extraEnv map[string]string) ([]string, []string, []string) {
	combined := make(map[string]string)
	var warnings []string
	var missingKeys []string

	if scionCfg != nil && scionCfg.Env != nil {
		for k, v := range scionCfg.Env {
			// Support variable substitution in keys and values
			expandedKey, _ := util.ExpandEnv(k)
			expandedValue, warned := util.ExpandEnv(v)

			if expandedKey == "" {
				continue
			}
			// If the value is empty and we warned about a missing variable,
			// skip adding it to combined to avoid a redundant warning later.
			if expandedValue == "" && warned {
				continue
			}
			// If the value is empty (no variable reference was used),
			// treat the key as an implicit host env passthrough: look up
			// the environment variable of the same name on the host.
			if expandedValue == "" {
				if hostVal, ok := os.LookupEnv(expandedKey); ok && hostVal != "" {
					expandedValue = hostVal
				}
			}
			combined[expandedKey] = expandedValue
		}
	}
	// Add extraEnv
	for k, v := range extraEnv {
		combined[k] = v
	}

	agentEnv := []string{}
	for k, v := range combined {
		if v == "" {
			missingKeys = append(missingKeys, k)
			warnings = append(warnings, fmt.Sprintf("Warning: Environment variable '%s' has no value and will be omitted.", k))
			continue
		}
		agentEnv = append(agentEnv, fmt.Sprintf("%s=%s", k, v))
	}
	return agentEnv, warnings, missingKeys
}

// buildAuthEnvOverlay creates an auth-only view of the environment by layering
// env-type resolved secrets over baseEnv without mutating baseEnv.
func buildAuthEnvOverlay(baseEnv map[string]string, secrets []api.ResolvedSecret) map[string]string {
	overlay := make(map[string]string, len(baseEnv))
	for k, v := range baseEnv {
		overlay[k] = v
	}
	for _, s := range secrets {
		if (s.Type != "environment" && s.Type != "") || s.Value == "" {
			continue
		}
		target := s.Target
		if target == "" {
			target = s.Name
		}
		if target == "" {
			continue
		}
		if existing, exists := overlay[target]; !exists || existing == "" {
			overlay[target] = s.Value
		}
	}
	return overlay
}

// filterResolvedSecretsForResolvedAuth drops auth-candidate secrets that are
// not required by the selected resolved auth method while preserving all
// non-auth secrets.
func filterResolvedSecretsForResolvedAuth(secrets []api.ResolvedSecret, resolved *api.ResolvedAuth) []api.ResolvedSecret {
	if len(secrets) == 0 || resolved == nil {
		return secrets
	}

	requiredEnv := make(map[string]struct{}, len(resolved.EnvVars))
	for k := range resolved.EnvVars {
		requiredEnv[k] = struct{}{}
	}

	requiredFileKinds := make(map[string]struct{})
	for _, f := range resolved.Files {
		kind := authFileKind("", f.ContainerPath)
		if kind != "" {
			requiredFileKinds[kind] = struct{}{}
		}
	}

	filtered := make([]api.ResolvedSecret, 0, len(secrets))
	for _, s := range secrets {
		if !isAuthCandidateSecret(s) {
			filtered = append(filtered, s)
			continue
		}

		keep := false
		switch s.Type {
		case "file":
			if kind := authFileKind(s.Name, s.Target); kind != "" {
				_, keep = requiredFileKinds[kind]
			}
		case "environment", "":
			target := s.Target
			if target == "" {
				target = s.Name
			}
			_, keep = requiredEnv[target]
		}

		if keep {
			filtered = append(filtered, s)
		}
	}

	return filtered
}

func isAuthCandidateSecret(s api.ResolvedSecret) bool {
	if (s.Type == "environment" || s.Type == "") && isAuthEnvKey(secretEnvTarget(s)) {
		return true
	}
	if s.Type == "file" && authFileKind(s.Name, s.Target) != "" {
		return true
	}
	return false
}

func secretEnvTarget(s api.ResolvedSecret) string {
	if s.Target != "" {
		return s.Target
	}
	return s.Name
}

func isAuthEnvKey(key string) bool {
	switch key {
	case "GEMINI_API_KEY",
		"GOOGLE_API_KEY",
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"CODEX_API_KEY",
		"GOOGLE_CLOUD_PROJECT",
		"GCP_PROJECT",
		"ANTHROPIC_VERTEX_PROJECT_ID",
		"GOOGLE_CLOUD_REGION",
		"CLOUD_ML_REGION",
		"GOOGLE_CLOUD_LOCATION":
		return true
	default:
		return false
	}
}

func authFileKind(name, target string) string {
	switch {
	case name == "gcloud-adc" || strings.HasSuffix(target, "/application_default_credentials.json"):
		return "adc"
	case name == "GEMINI_OAUTH_CREDS" || strings.HasSuffix(target, "/oauth_creds.json"):
		return "gemini-oauth"
	case name == "CODEX_AUTH" || strings.HasSuffix(target, "/.codex/auth.json"):
		return "codex-auth"
	case name == "OPENCODE_AUTH" || strings.HasSuffix(target, "/opencode/auth.json"):
		return "opencode-auth"
	case name == "CLAUDE_CREDENTIALS" || strings.HasSuffix(target, "/.claude/.credentials.json"):
		return "claude-credentials"
	default:
		return ""
	}
}

// hasMetadataInterception checks if the agent env vars include metadata server
// configuration that requires iptables interception (assign or block mode).
func hasMetadataInterception(env []string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, "SCION_METADATA_MODE=assign") || strings.HasPrefix(e, "SCION_METADATA_MODE=block") {
			return true
		}
	}
	return false
}
