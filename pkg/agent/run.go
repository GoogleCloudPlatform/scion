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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/google/uuid"
)

var ErrTmuxBinaryNotFound = errors.New("tmux binary not found")
var ErrContainerNameInUse = errors.New("agent name is already in use by a stopped container; please delete the existing agent or choose a different name")

func classifyLaunchRuntimeError(err error, resolvedImage string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, exec.ErrNotFound) || isTmuxShellNotFoundError(err) {
		return fmt.Errorf("failed to launch container in image %q: %w: %w", resolvedImage, ErrTmuxBinaryNotFound, err)
	}
	if isContainerNameInUseError(err) {
		return ErrContainerNameInUse
	}
	return fmt.Errorf("failed to launch container: %w", err)
}

func isContainerNameInUseError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return (strings.Contains(msg, "container name") && strings.Contains(msg, "already in use")) ||
		strings.Contains(msg, "is already in use by container")
}

func isTmuxShellNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "tmux: command not found") ||
		strings.Contains(msg, "tmux: not found")
}

// sortedEnvVarKeys returns the sorted key names of an env var map, for
// diagnostic logging that must not print the values themselves.
func sortedEnvVarKeys(envVars map[string]string) []string {
	keys := make([]string, 0, len(envVars))
	for k := range envVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (m *AgentManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	startEntry := time.Now()
	// callerHubEndpoint is opts.Env's SCION_HUB_ENDPOINT exactly as the
	// caller passed it in — captured before anything below ever writes to
	// opts.Env, so it can never reflect the agent-level Hub config or
	// template env overrides applied further down, both of which a creator
	// controls (inline req.Config.Hub, template hub.endpoint via
	// MergeScionConfig). Outside broker mode this (falling back to project
	// settings) is the one source Substrate's egress allowlist trusts; see
	// the trustedHubEndpoint computation below. In BrokerMode, egress trust
	// comes from opts.TrustedHubEndpoint instead — the runtime broker's own
	// operator-derived resolution, set separately from opts.Env so a
	// creator-controlled ResolvedEnv/Config.Env value can never reach it —
	// not from this variable, which in BrokerMode still only feeds the
	// agent's own delivered hub endpoint, never egress trust.
	var callerHubEndpoint string
	if opts.Env != nil {
		callerHubEndpoint = opts.Env["SCION_HUB_ENDPOINT"]
	}

	// Resolve project name early so we can scope the container lookup below.
	projectDir, err := config.GetResolvedProjectDir(opts.ProjectPath)
	if err != nil {
		return nil, err
	}
	projectName := config.GetProjectName(projectDir)

	// Determine the project ID for label-based filtering. In broker/hosted mode
	// this comes from env injected by the hub dispatcher.
	projectID := ""
	agentID := opts.Name
	if opts.Env != nil {
		if opts.Env["SCION_AGENT_ID"] != "" {
			agentID = opts.Env["SCION_AGENT_ID"]
		}
		projectID = opts.Env["SCION_PROJECT_ID"]
	}
	// Snapshot the dispatch-provided project ID now, before any
	// settings-driven env merging (resolveAuthEnvOverlay copying
	// harness-config env into opts.Env for absent keys, telemetry env, etc.)
	// can inject a project-controlled SCION_PROJECT_ID.
	// Used only by the nfs shared_dir_storage branch below (round 3 review
	// finding C1/S-L1): a project's harness_configs.<name>.env can set
	// these keys, and since resolveAuthEnvOverlay only fills in *absent*
	// keys, capturing the value here — before that overlay ever runs — is
	// what keeps it from being attacker-influenced. The general `projectID`
	// below is unaffected and keeps its existing settings.Hub.ProjectID
	// fallback for labels, RunConfig.ProjectID, etc.
	hubDispatchedProjectID := projectID
	// The hub agent ID, snapshotted for the same reason: an NFS agent home
	// is named after it (resolveHomeStorage), and it must come from the
	// dispatch, never from settings or the agent name.
	hubDispatchedAgentID := ""
	if opts.Env != nil {
		hubDispatchedAgentID = opts.Env["SCION_AGENT_ID"]
	}

	// Every new runtime entry carries a run ID (ptone/scion#2550). The hub
	// mints one per create/start dispatch; local/CLI mode and older hubs
	// send none, so mint it here instead. It is fixed this early so
	// provisioning can record it as the run that owns the agent's files
	// (ptone/scion#2675), before the container exists.
	if opts.RunID == "" {
		opts.RunID = uuid.NewString()
	}
	ctx = api.ContextWithRunID(ctx, opts.RunID)

	// 0. Check if container already exists (scoped to this project)
	slug := api.Slugify(opts.Name)
	agents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	if err == nil {
		for _, a := range agents {
			// Skip agents from a different project
			if !matchAgentProject(a, projectName, projectID) {
				continue
			}
			isRunning := a.Phase == string(state.PhaseRunning)
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
			// If it exists but not running (or we have a new task), we delete it so we can recreate it.
			// The delete is by name/ID found by name, so an async launch
			// checkpoints first: a launch the hub has already ended must
			// not remove a newer launch's agent.
			if opts.Checkpoint != nil {
				if err := opts.Checkpoint(ctx, runtime.CheckpointStepPreClean); err != nil {
					return nil, err
				}
			}
			if err := m.Runtime.Delete(ctx, runtime.RunRef{ID: runtime.AgentOperationID(a), RunID: a.RunID}); err != nil {
				return nil, fmt.Errorf("failed to cleanup existing container: %w", err)
			}
		}
	}

	// Record this run as the owner of an already provisioned agent's files
	// (provision-only, a restart, a resume) as soon as the previous run's
	// container is gone (ptone/scion#2675). From here on the hub keeps this
	// run even if the start fails, so the files must name it too, or a
	// delete for this run would leave them behind. A fresh provision below
	// records it as it writes agent-info.json; without agent-info.json this
	// is a no-op.
	if err := SetSavedRunID(opts.Name, opts.ProjectPath, opts.RunID); err != nil {
		slog.Warn("Start: failed to record the run ID in agent-info.json; a delete for this run may leave the agent's files behind",
			"agent", opts.Name, "run_id", opts.RunID, "error", err)
	}

	// If resuming, verify the agent exists before proceeding. Probe both
	// worktree and shared-workspace layouts since this runs before the
	// sharedWorkspace flag is folded into context.
	if opts.Resume {
		agentDir := config.ResolveAgentDir(projectDir, opts.Name)
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
	if opts.FreshProvision {
		ctx = api.ContextWithFreshProvision(ctx)
	}
	if opts.SharedWorkspace {
		ctx = api.ContextWithSharedWorkspace(ctx)
	}
	if isEmptyPerAgentStart(opts) {
		ctx = api.ContextWithEmptyPerAgentWorkspace(ctx)
	}
	if opts.HarnessConfigPath != "" {
		ctx = api.ContextWithHarnessConfigPath(ctx, opts.HarnessConfigPath)
	}
	if opts.TemplateName != "" {
		ctx = api.ContextWithTemplateName(ctx, opts.TemplateName)
	}

	// Build inline config for GetAgent by merging the dispatch InlineConfig
	// (which carries volumes, env, image, etc. from templates/harness configs)
	// with any --harness-auth override. Without this merge, custom volumes
	// from hub-dispatched agents are silently dropped.
	var startInlineConfig *api.ScionConfig
	if opts.InlineConfig != nil {
		startInlineConfig = opts.InlineConfig
	}
	if opts.HarnessAuth != "" && !harness.IsHarnessImplementationName(opts.HarnessAuth) {
		if startInlineConfig == nil {
			startInlineConfig = &api.ScionConfig{}
		}
		startInlineConfig.AuthSelectedType = opts.HarnessAuth
	}

	util.Debugf("Start: calling GetAgent name=%s template=%q image=%q harnessConfig=%q projectPath=%q profile=%q",
		opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.ProjectPath, opts.Profile)
	agentDir, agentHome, agentWorkspace, finalScionCfg, err := GetAgent(ctx, opts.Name, opts.Template, opts.Image, opts.HarnessConfig, opts.ProjectPath, opts.Profile, "", opts.Branch, opts.Workspace, startInlineConfig)
	if err != nil {
		return nil, err
	}
	// Empty-per-agent (design #2703): the request's mode, or the mode
	// persisted at provision, so a start that lost it (e.g. a dropped or
	// undecodable request body) still gets the private workspace, no repo
	// root and the right container env rather than legacy resolution.
	emptyPerAgent := isEmptyPerAgentStart(opts) || (finalScionCfg != nil && finalScionCfg.EmptyPerAgentWorkspace)
	if emptyPerAgent {
		env := make(map[string]string, len(opts.Env)+1)
		for k, v := range opts.Env {
			env[k] = v
		}
		env["SCION_WORKSPACE_MODE"] = string(store.SharingModeEmptyPerAgent)
		delete(env, "SCION_WORKSPACE_GIT")
		opts.Env = env
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
		if writeErr := os.WriteFile(promptFile, []byte(task), 0644); writeErr != nil {
			return nil, fmt.Errorf("failed to write prompt file %s: %w", promptFile, writeErr)
		}
	}

	// Load settings for registry resolution
	settings, settingsWarnings, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		util.Debugf("Start: LoadEffectiveSettings(%s) error: %v", projectDir, err)
	}
	config.PrintDeprecationWarnings(settingsWarnings)

	// Phase 5: Resolve project ID from settings if not already provided via env
	if projectID == "" && settings != nil && settings.Hub != nil {
		projectID = settings.Hub.ProjectID
	}

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

	// Resolve the template chain unconditionally: it drives both the
	// harness-config-dir search below and the explicit-image detection
	// further down. Prefer opts.Template when it is an absolute path (e.g.
	// hydrated template cache path from the broker). The display name stored
	// in finalScionCfg.Info.Template (e.g. "web-dev") may not resolve in the
	// project, but the original opts.Template path points to the actual
	// template directory containing harness-configs/.
	//
	// A template recorded as loaded from a content-addressed cache
	// (Info.TemplateHash) is not available by name here: Info.Template is
	// then a display name only, so it is never looked up, and the template
	// counts as unresolvable, as it did when the stored name was the cache
	// directory's hash.
	infoHydrated := infoTemplateHydrated(finalScionCfg)
	// templateUnresolvable is true only when a named template could not be
	// found at all (renamed or deleted since the agent was created) — not
	// when there is simply no template name to resolve. It gates the
	// recorded-image/pull-policy fallback below, so a restart of an agent
	// whose template disappeared doesn't silently drop to Hub settings or
	// the file default the way an empty templateChain otherwise would.
	templateUnresolvable := false
	templateName := ""
	if opts.Template != "" && filepath.IsAbs(opts.Template) {
		templateName = opts.Template
	}
	if templateName == "" && infoHydrated {
		templateUnresolvable = true
		util.Debugf("image resolution: template was loaded from a content-addressed cache (%s), not resolved by name", finalScionCfg.Info.TemplateHash)
	}
	if templateName == "" && !infoHydrated {
		if finalScionCfg != nil && finalScionCfg.Info != nil {
			templateName = finalScionCfg.Info.Template
		}
	}
	if templateName == "" && !infoHydrated {
		templateName = opts.Template
	}
	var templateChain []*config.Template
	var templatePaths []string
	if templateName != "" {
		if chain, err := config.GetTemplateChainInProject(templateName, opts.ProjectPath); err == nil {
			templateChain = chain
			for _, tpl := range chain {
				templatePaths = append(templatePaths, tpl.Path)
			}
		} else {
			templateUnresolvable = true
			util.Debugf("image resolution: template %q could not be resolved: %v", templateName, err)
		}
	}

	// Load on-disk harness-config for the container user and image (base layer).
	// The settings map may not define harness_configs, but the on-disk
	// config.yaml (seeded from harness embeds) always has the user field.
	// Also check template directories since harness-configs may be bundled
	// inside templates (§3.4 of agnostic-template-design).
	// resolvedHarnessConfigAuth captures the auth metadata from the resolved
	// on-disk harness config for use by the auth pipeline later.
	var resolvedHarnessConfigAuth *config.HarnessAuthMetadata
	// resolvedPullPolicy is the Kubernetes-only image pull policy, resolved
	// with the same three lower tiers as image (file default, then Hub
	// settings); the explicit-override tier is computed further down
	// alongside image's.
	resolvedPullPolicy := ""
	if harnessConfigName != "" {
		if hcDir, err := resolveHarnessConfigDir(ctx, harnessConfigName, projectDir, templatePaths...); err == nil {
			if hcDir.Config.Image != "" {
				resolvedImage = hcDir.Config.Image
				util.Debugf("image resolution: from on-disk harness-config image=%s path=%s", resolvedImage, hcDir.Path)
			}
			if hcDir.Config.ImagePullPolicy != "" {
				resolvedPullPolicy = hcDir.Config.ImagePullPolicy
			}
			if hcDir.Config.User != "" {
				unixUsername = hcDir.Config.User
			}
			if hcDir.Config.Auth != nil {
				resolvedHarnessConfigAuth = hcDir.Config.Auth
			}
		} else {
			util.Debugf("image resolution: on-disk harness-config %q not found: %v", harnessConfigName, err)
		}
	}

	if settings != nil && harnessConfigName != "" {
		// A local restart (no --profile) has no request-level profile to
		// resolve settings against; fall back to the profile the agent was
		// actually created with (finalScionCfg.Info.Profile, already loaded —
		// the same value agent.GetSavedProfile would read from
		// agent-info.json), matching the broker's own restart-dispatch
		// behavior (runtimebroker/handlers.go's GetSavedProfile calls).
		settingsProfile := opts.Profile
		if settingsProfile == "" && finalScionCfg != nil && finalScionCfg.Info != nil {
			settingsProfile = finalScionCfg.Info.Profile
		}
		hConfig, err := settings.ResolveHarnessConfig(settingsProfile, harnessConfigName)
		if err == nil {
			if hConfig.Image != "" {
				resolvedImage = hConfig.Image
				util.Debugf("image resolution: from settings harness-config image=%s", resolvedImage)
			}
			if hConfig.ImagePullPolicy != "" {
				resolvedPullPolicy = hConfig.ImagePullPolicy
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

	// The explicit tier, per field: the live template chain (later template
	// wins) as a base, then the CURRENT request's own inline config
	// (opts.InlineConfig — not startInlineConfig, which a bare --harness-auth
	// also makes non-nil with no image of its own) if it sets that field,
	// else the value ProvisionAgent recorded at creation time from the
	// inline config that agent was created with. The persisted value is
	// inline-only, never template-derived, so it never outranks the live
	// template chain the way a template snapshot would. Falling back per
	// field (not "any current inline config present") is what keeps a
	// create-time image pin alive across a restart that passes --harness-auth
	// or an unrelated --config field, e.g. --model. See
	// settings-precedence.md's "Container image and Kubernetes image pull
	// policy" section.
	explicitImage, explicitPullPolicy := "", ""
	for _, tpl := range templateChain {
		tplCfg, err := tpl.LoadConfig()
		if err != nil {
			continue
		}
		if tplCfg.Image != "" {
			explicitImage = tplCfg.Image
		}
		if tplCfg.Kubernetes != nil && tplCfg.Kubernetes.ImagePullPolicy != "" {
			explicitPullPolicy = tplCfg.Kubernetes.ImagePullPolicy
		}
	}
	// imageFromSnapshot/pullPolicyFromSnapshot track whether the
	// templateUnresolvable fallback below is what actually ends up supplying
	// resolvedImage/resolvedPullPolicy, as opposed to being overridden by a
	// higher-priority source afterward (a current or recorded inline value,
	// or — for image — an explicit dispatch --image). The warning at the end
	// of this section only fires when a flag is still true, so it never
	// claims the recorded snapshot was used when it wasn't.
	imageFromSnapshot, pullPolicyFromSnapshot := false, false
	if templateUnresolvable {
		// The named template itself is gone (renamed or deleted since this
		// agent was created) — not merely "no template pin". Fall back to
		// the full config ProvisionAgent persisted at creation (not just the
		// inline-only Info.ExplicitImage*), which already reflects whatever
		// that template contributed at the time, so a restart doesn't
		// silently drop to Hub settings or the file default just because
		// the template disappeared. An inline override, live or recorded,
		// still wins below, same as it would over a live template.
		if finalScionCfg != nil && finalScionCfg.Image != "" {
			explicitImage = finalScionCfg.Image
			imageFromSnapshot = true
		}
		if finalScionCfg != nil && finalScionCfg.Kubernetes != nil && finalScionCfg.Kubernetes.ImagePullPolicy != "" {
			explicitPullPolicy = finalScionCfg.Kubernetes.ImagePullPolicy
			pullPolicyFromSnapshot = true
		}
	}
	if opts.InlineConfig != nil && opts.InlineConfig.Image != "" {
		explicitImage = opts.InlineConfig.Image
		imageFromSnapshot = false
	} else if finalScionCfg != nil && finalScionCfg.Info != nil && finalScionCfg.Info.ExplicitImage != "" {
		explicitImage = finalScionCfg.Info.ExplicitImage
		imageFromSnapshot = false
	}
	if opts.InlineConfig != nil && opts.InlineConfig.Kubernetes != nil && opts.InlineConfig.Kubernetes.ImagePullPolicy != "" {
		explicitPullPolicy = opts.InlineConfig.Kubernetes.ImagePullPolicy
		pullPolicyFromSnapshot = false
	} else if finalScionCfg != nil && finalScionCfg.Info != nil && finalScionCfg.Info.ExplicitImagePullPolicy != "" {
		explicitPullPolicy = finalScionCfg.Info.ExplicitImagePullPolicy
		pullPolicyFromSnapshot = false
	}
	if explicitImage != "" {
		resolvedImage = explicitImage
		util.Debugf("image resolution: from explicit template/inline-config image=%s", resolvedImage)
	}
	if explicitPullPolicy != "" {
		resolvedPullPolicy = explicitPullPolicy
	}

	// Apply CLI/dispatch image override before registry rewrite so the
	// rewrite applies last regardless of the image source.
	if opts.Image != "" {
		resolvedImage = opts.Image
		imageFromSnapshot = false
		util.Debugf("image resolution: from CLI/dispatch --image flag image=%s", resolvedImage)
	}

	if imageFromSnapshot || pullPolicyFromSnapshot {
		var fields []string
		if imageFromSnapshot {
			fields = append(fields, "image")
		}
		if pullPolicyFromSnapshot {
			fields = append(fields, "pull policy")
		}
		warnings = append(warnings, fmt.Sprintf(
			"template %q could not be found; using the %s recorded at an earlier provision instead of the current template",
			templateName, strings.Join(fields, " and ")))
	}

	// Two-phase image resolution: for short-form (bare) images, prefer a
	// locally-built image over the registry version. If no local image
	// exists, fall back to the registry rewrite.
	// NOTE: The local-exists check only applies to runtimes with local image
	// storage (docker, podman, container). Runtimes like kubernetes and cloudrun
	// always return true from ImageExists (images are pulled on demand by the
	// node), so we must skip the local check to ensure registry rewrite applies.
	if settings != nil && resolvedImage != "" {
		imageRegistry := settings.ResolveImageRegistry(opts.Profile)
		if imageRegistry != "" && imagecheck.IsBareImageName(resolvedImage) {
			runtimeName := ""
			if m.Runtime != nil {
				runtimeName = m.Runtime.Name()
			}
			hasLocalImages := runtimeName == "docker" || runtimeName == "podman" ||
				runtimeName == "container" || runtimeName == "apple-container"
			localExists := false
			if hasLocalImages {
				var localErr error
				localExists, localErr = m.Runtime.ImageExists(ctx, resolvedImage)
				if localErr != nil {
					localExists = false
				}
			}
			if localExists {
				util.Debugf("image resolution: using local short-form image %s", resolvedImage)
			} else {
				rewritten := config.RewriteImageRegistry(resolvedImage, imageRegistry)
				if rewritten != resolvedImage {
					util.Debugf("image resolution: image_registry rewrite %s -> %s", resolvedImage, rewritten)
					resolvedImage = rewritten
				}
			}
		} else if imageRegistry != "" {
			rewritten := config.RewriteImageRegistry(resolvedImage, imageRegistry)
			if rewritten != resolvedImage {
				util.Debugf("image resolution: image_registry rewrite %s -> %s", resolvedImage, rewritten)
				resolvedImage = rewritten
			}
		}
	}

	if resolvedImage == "" {
		util.Debugf("image resolution FAILED: harnessConfigName=%q, finalScionCfg.Image=%q, opts.Image=%q, projectDir=%s",
			harnessConfigName, finalScionCfg.Image, opts.Image, projectDir)
		return nil, fmt.Errorf("no container image resolved for agent %q. Set 'image' in the harness-config config.yaml, specify --image, or configure a harness-config in settings", opts.Name)
	}

	util.Debugf("image resolution: final image=%s", resolvedImage)

	// Resolve the harness implementation. When we have a harness-config name,
	// route through harness.Resolve so container-script provisioners (and
	// future declarative-only harnesses) are honored. Otherwise fall back to
	// the legacy New() shim using the bare harness type.
	var h api.Harness
	var harnessConfigRevision string
	var noAuthConfig *config.HarnessNoAuthConfig
	if harnessConfigName != "" {
		var resolveTemplatePaths []string
		if opts.Template != "" && (filepath.IsAbs(opts.Template) || !infoHydrated) {
			tplName := opts.Template
			if !filepath.IsAbs(tplName) && finalScionCfg != nil && finalScionCfg.Info != nil && finalScionCfg.Info.Template != "" {
				tplName = finalScionCfg.Info.Template
			}
			if chain, err := config.GetTemplateChainInProject(tplName, opts.ProjectPath); err == nil {
				for _, tpl := range chain {
					resolveTemplatePaths = append(resolveTemplatePaths, tpl.Path)
				}
			}
		}
		resolved, err := harness.Resolve(ctx, harness.ResolveOptions{
			Name:          harnessConfigName,
			ProjectPath:   projectDir,
			TemplatePaths: resolveTemplatePaths,
			ProfileName:   profileName,
			Settings:      settings,
			ConfigDirPath: opts.HarnessConfigPath,
		})
		if err != nil {
			util.Debugf("harness.Resolve fell back to New(%q): %v", harnessName, err)
			h = harness.New(harnessName)
		} else {
			h = resolved.Harness
			noAuthConfig = resolved.Config.NoAuthConfig
			if resolved.ConfigDir != nil {
				harnessConfigRevision = config.ComputeHarnessConfigRevision(resolved.ConfigDir.Path)
			}
			util.Debugf("harness resolution: implementation=%s harness=%q", resolved.Implementation, resolved.Config.Harness)
		}
	} else {
		h = harness.New(harnessName)
	}

	// Reconcile the harness bundle for existing agents. Provision() is
	// idempotent and stages any missing files.
	if err := h.Provision(ctx, opts.Name, agentDir, agentHome, agentWorkspace); err != nil {
		util.Debugf("Start: harness reconciliation failed: %v", err)
	}

	// Re-stage the project pre-start hook on restart. The script content is
	// baked into StartOptions at agent-create time and forwarded by the broker,
	// so no Hub round-trip is needed. A write failure is fatal: if the file is
	// absent on a fresh restart the abort guard in init.go (projectHookStaged)
	// will not fire, so the hook would silently not run — worse than aborting.
	// Called unconditionally: with an empty script the helper clears any file
	// staged by an earlier occupant of this agent home.
	if err := harness.WriteProjectPreStartHook(agentHome, opts.ProjectPreStartHookScript); err != nil {
		return nil, fmt.Errorf("re-stage project pre-start hook: %w", err)
	}

	// Resolve auth metadata for the config-driven env var pipeline.
	// Prefer the on-disk harness config (already resolved above for image/
	// user); fall back to the settings entry.
	var authMeta *config.HarnessAuthMetadata
	if resolvedHarnessConfigAuth != nil {
		authMeta = resolvedHarnessConfigAuth
	} else if harnessConfigName != "" && settings != nil {
		if hcEntry, err := settings.ResolveHarnessConfig(profileName, harnessConfigName); err == nil && hcEntry.Auth != nil {
			authMeta = hcEntry.Auth
		}
	}

	// 3. Resolve credentials via new auth pipeline

	// Inject settings-declared env into opts.Env and build the auth overlay.
	// Extracted so the injection-then-overlay sequence is testable without
	// standing up a full agent; see resolveAuthEnvOverlay.
	authEnvOverlay, droppedBrokerEnvVars := resolveAuthEnvOverlay(&opts, settings, profileName, harnessConfigName)

	// Agents created in no-auth mode persist auth_selectedType "none". The
	// start, restart, resume and wake paths may reach here without NoAuth
	// set, so treat "none" as a request for no-auth mode instead of an auth
	// type. An explicit opts.HarnessAuth other than "none" still wins over
	// the persisted value.
	if isNoAuthSelection(opts.HarnessAuth, finalScionCfg) {
		opts.NoAuth = true
	}

	canFallbackToNoAuth := func() bool {
		return opts.HarnessAuth == "" && noAuthConfig != nil &&
			(noAuthConfig.Behavior == "drop-to-shell" || noAuthConfig.Behavior == "allow")
	}

	var auth api.AuthConfig
	var resolvedAuth *api.ResolvedAuth
	if !opts.NoAuth {
		auth = harness.GatherAuthWithEnv(authEnvOverlay, !opts.BrokerMode, authMeta)
		if opts.BrokerMode {
			harness.OverlayFileSecrets(&auth, opts.ResolvedSecrets, authMeta)
		}
		util.Debugf("auth: gathered credentials — selectedType=%q, hasADC=%t, cloudProject=%q, gcpMetadataMode=%q, envVarCount=%d, fileCount=%d, brokerMode=%t",
			auth.SelectedType,
			auth.GoogleAppCredentials != "",
			auth.GoogleCloudProject,
			auth.GCPMetadataMode,
			len(auth.EnvVars),
			len(auth.Files),
			opts.BrokerMode,
		)
		harness.OverlaySettings(&auth, h, agentDir)
		// Apply CLI harness auth override (--harness-auth) before resolution.
		// This has highest priority, overriding settings, templates, and harness configs.
		if opts.HarnessAuth != "" && !harness.IsHarnessImplementationName(opts.HarnessAuth) {
			auth.SelectedType = opts.HarnessAuth
		}
		// Auto-detect an auth type when nothing explicit selected one yet.
		autoDetectAuthSelectedType(&auth, authMeta, &opts)
		util.Debugf("auth: after overlay — selectedType=%q", auth.SelectedType)
		resolved, err := h.ResolveAuth(auth)
		if err != nil {
			if canFallbackToNoAuth() {
				util.Debugf("auth: resolution failed, falling back to no-auth mode: %v", err)
				opts.NoAuth = true
				warnings = append(warnings, "Auth: no credentials found, starting in no-auth mode")
				goto authDone
			}
			return nil, fmt.Errorf("auth resolution failed: %w", err)
		}
		if resolved == nil {
			// ResolveAuth returned nil without error — treat as no auth available.
			if canFallbackToNoAuth() {
				util.Debugf("auth: resolution returned nil, falling back to no-auth mode")
				opts.NoAuth = true
				warnings = append(warnings, "Auth: no credentials found, starting in no-auth mode")
				goto authDone
			}
			return nil, fmt.Errorf("auth resolution returned nil for method %q", auth.SelectedType)
		}
		// Keep a copy of the full resolved auth material for secret filtering.
		// Deep-copy the Files slice so in-place SourcePath clearing below
		// does not leak into resolvedForSecretFilter.
		resolvedForSecretFilter := *resolved
		resolvedForSecretFilter.Files = append([]api.FileMapping(nil), resolved.Files...)
		if opts.BrokerMode {
			// File content projection is handled by writeFileSecrets() from
			// ResolvedSecrets at container launch (via SCION_STAGED_SECRETS),
			// not by applyResolvedAuth from local paths. Clear SourcePath so
			// stageFileSecretFiles won't try to read host files, but preserve
			// ContainerPath so it can populate file_secret_files in
			// auth-candidates.json.
			for i := range resolved.Files {
				resolved.Files[i].SourcePath = ""
			}
		}
		util.Debugf("auth: resolved — method=%q, envVarKeys=%v, files=%d", resolved.Method, sortedEnvVarKeys(resolved.EnvVars), len(resolved.Files))
		if err := harness.ValidateAuth(resolved, opts.BrokerMode); err != nil {
			if canFallbackToNoAuth() {
				util.Debugf("auth: validation failed, falling back to no-auth mode: %v", err)
				opts.NoAuth = true
				warnings = append(warnings, "Auth: credential validation failed, starting in no-auth mode")
				goto authDone
			}
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
		configKeys := configAuthEnvKeySet(authMeta)
		opts.ResolvedSecrets = filterResolvedSecretsForResolvedAuth(opts.ResolvedSecrets, &resolvedForSecretFilter, configKeys)
		// The hub pre-merges environment-type secrets into ResolvedEnv before
		// dispatching to the broker (see pkg/hub/httpdispatcher.go), so auth
		// env keys copied into opts.Env via start_context's ResolvedEnv merge
		// would otherwise slip through even after ResolvedSecrets filtering.
		// Drop auth-candidate env keys that the resolved auth method does not
		// use, mirroring the ResolvedSecrets filter.
		if len(opts.Env) > 0 {
			requiredAuthEnv := make(map[string]struct{}, len(resolvedForSecretFilter.EnvVars))
			for k := range resolvedForSecretFilter.EnvVars {
				requiredAuthEnv[k] = struct{}{}
			}
			for k := range opts.Env {
				if !isAuthEnvKey(k, configKeys) {
					continue
				}
				if _, required := requiredAuthEnv[k]; !required {
					delete(opts.Env, k)
				}
			}
		}

		// Persist the resolved auth type so it can be reported to the Hub.
		// For auto-detected auth, opts.HarnessAuth may be empty; capture the
		// actual auth type (e.g. "api-key", "vertex-ai").
		// Prefer SCION_HARNESS_SELECTED_AUTH (the true auth type) over
		// resolved.Method, which for container-script harnesses is always
		// "container-script" — an implementation name, not a valid auth type.
		// Both branches reject harness implementation names so that
		// already-corrupted scion-agent.json data is not re-persisted
		// on resume (opts.HarnessAuth stays empty → no write-back).
		if opts.HarnessAuth == "" {
			if authType := resolved.EnvVars["SCION_HARNESS_SELECTED_AUTH"]; authType != "" && !harness.IsHarnessImplementationName(authType) {
				opts.HarnessAuth = authType
			} else if resolved.Method != "" && !harness.IsHarnessImplementationName(resolved.Method) {
				opts.HarnessAuth = resolved.Method
			}
		}

		// If opts.HarnessAuth is still a harness implementation name (e.g.
		// "container-script" from a corrupted scion-agent.json), clear it so
		// it does not leak into persistence or other downstream logic.
		if harness.IsHarnessImplementationName(opts.HarnessAuth) {
			opts.HarnessAuth = ""
		}

		// Surface resolved auth method so CLI can display it
		authDetail := resolved.Method
		if nativeType, ok := resolved.EnvVars["GEMINI_DEFAULT_AUTH_TYPE"]; ok {
			authDetail = fmt.Sprintf("%s (%s)", resolved.Method, nativeType)
		}
		warnings = append(warnings, fmt.Sprintf("Auth: resolved as %s", authDetail))
	}
authDone:
	if opts.NoAuth {
		// Clean up stale auth-candidates from a prior run so the
		// provisioner sees no candidates and runs in no-auth mode.
		authCandidatesPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "auth-candidates.json")
		if err := os.Remove(authCandidatesPath); err != nil && !os.IsNotExist(err) {
			util.Debugf("Start: failed to remove stale auth-candidates: %v", err)
		}
	}

	// Unconditionally clear corrupted opts.HarnessAuth. This runs even when
	// NoAuth is true (the auth block is skipped) to prevent re-persisting
	// a corrupted value from a prior run's scion-agent.json.
	if harness.IsHarnessImplementationName(opts.HarnessAuth) {
		opts.HarnessAuth = ""
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

	template := agentTemplateDisplayName(opts.TemplateName, finalScionCfg)

	if opts.Env == nil {
		opts.Env = make(map[string]string)
	}
	opts.Env["SCION_AGENT_NAME"] = opts.Name
	opts.Env["SCION_PROJECT"] = projectName
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
	// Re-resolve SCION_MODEL if it contains an unresolved size alias.
	// The hub may inject a raw alias (e.g. "large") when its store lacks
	// the harness config's model_aliases map; the broker has the on-disk
	// config and can resolve it here.
	if resolved, ok := reResolveModelAlias(opts.Env["SCION_MODEL"], finalScionCfg, harnessName); ok {
		util.Debugf("RunAgent: re-resolved leaked model alias %q → %q", opts.Env["SCION_MODEL"], resolved)
		opts.Env["SCION_MODEL"] = resolved
	}
	if _, ok := opts.Env["SCION_THINKING_LEVEL"]; !ok && finalScionCfg != nil && finalScionCfg.ThinkingLevel != nil {
		opts.Env["SCION_THINKING_LEVEL"] = strconv.Itoa(*finalScionCfg.ThinkingLevel)
	}
	if _, ok := opts.Env["SCION_CREATOR"]; !ok {
		if u, err := user.Current(); err == nil {
			opts.Env["SCION_CREATOR"] = u.Username
		}
	}
	opts.Env["SCION_CLI_MODE"] = "agent"

	// Determine whether hub is explicitly disabled in project settings.
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
		// project settings and server config values passed via opts.Env.
		if !hubDisabled && finalScionCfg.Hub != nil && finalScionCfg.Hub.Endpoint != "" {
			opts.Env["SCION_HUB_ENDPOINT"] = finalScionCfg.Hub.Endpoint
			opts.Env["SCION_HUB_URL"] = finalScionCfg.Hub.Endpoint
		}
	}

	// If hub endpoint not yet set from agent config or caller's opts.Env,
	// check project settings so locally-started agents in hub-connected
	// projects also get hub connectivity. projectSettingsHubEndpoint
	// records this specific source's own resolved value (see
	// trustedHubEndpoint below): this branch only ever runs when neither
	// callerHubEndpoint nor the agent-level Hub config above set it, so
	// project settings — an operator-controlled file, not a creator input —
	// is the only source that can land here.
	var projectSettingsHubEndpoint string
	if _, hubSet := opts.Env["SCION_HUB_ENDPOINT"]; !hubSet {
		if projectSettings, err := config.LoadSettings(projectDir); err == nil {
			if projectSettings.IsHubEnabled() {
				if ep := projectSettings.GetHubEndpoint(); ep != "" {
					opts.Env["SCION_HUB_ENDPOINT"] = ep
					opts.Env["SCION_HUB_URL"] = ep
					projectSettingsHubEndpoint = ep
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

	// trustedHubEndpoint is Substrate's egress allowlist's one trusted hub
	// source (RunConfig.TrustedHubEndpoint, see its own doc comment): in
	// broker mode, opts.TrustedHubEndpoint ONLY — the runtime broker's own
	// operator-derived resolution (request HubEndpoint, hub connection
	// endpoint, or broker config HubEndpoint; never ResolvedEnv/Config.Env,
	// which a creator can set — see
	// api.StartOptions.TrustedHubEndpoint's own doc comment), a field set
	// separately from opts.Env so a creator-controlled env value can never
	// reach it even when every operator tier is empty; if empty, no hub host
	// is trusted at all (fail closed; see substrateEgressHostnames). Outside
	// broker mode, callerHubEndpoint (opts.Env, captured at the top of Start
	// before any override could touch it), falling back to
	// projectSettingsHubEndpoint (an operator-controlled file). The
	// agent-level Hub config and template env overrides — both
	// creator-controlled, applied above and below — never feed this value:
	// they can still redirect the agent's own hub calls, but must never
	// widen what the actor's egress allowlist may reach.
	var trustedHubEndpoint string
	if opts.BrokerMode {
		trustedHubEndpoint = opts.TrustedHubEndpoint
	} else {
		trustedHubEndpoint = callerHubEndpoint
		if trustedHubEndpoint == "" {
			trustedHubEndpoint = projectSettingsHubEndpoint
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
	// authoritative about hub connectivity and the project settings on the
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
		// THE DELETE IS FIRST, AND UNCONDITIONAL, ON PURPOSE. #1348.
		//
		// opts.Env becomes `--env KEY=VALUE` in the runtime's argv, and that
		// argv is written to Cloud Logging on every start (#127). The token
		// must therefore leave opts.Env on EVERY path out of this block,
		// including the failure paths below -- opts.Env is the caller's map,
		// and the broker still holds it after Start returns an error.
		//
		// Deleting before the write, rather than after it, is what makes that
		// property structural: there is no path through the rest of this block
		// that can skip it, so a later edit cannot reintroduce the leak by
		// adding an early return. Nothing between here and buildAgentEnv reads
		// the key back -- the only other reference is the resolver at :721,
		// which is above this block and never runs again in this call.
		delete(opts.Env, "SCION_AUTH_TOKEN")

		// Refusing to start is deliberate, and it is a choice of an
		// availability failure over a security one. The tempting fix for a
		// failed token write is to leave the token in opts.Env as a fallback;
		// that would put a live credential into the argv above and turn a rare,
		// loud failure into a silent credential disclosure on a path nobody
		// watches. All three failure modes mean the agent home is not writable,
		// which is not a condition an agent can usefully run under anyway: it
		// would report started and be able to authenticate to nothing, because
		// NewClient() finds no token in either place and returns nil.
		if err := writeAgentTokenFile(agentHome, token); err != nil {
			return nil, fmt.Errorf("cannot start agent %q without a hub credential: %w", opts.Name, err)
		}
	}

	// Resolve host networking: when the hub endpoint is localhost or was
	// translated to host.docker.internal, use --network=host so the container
	// can reach the host's loopback interface directly. This also rewrites any
	// bridge hostnames back to localhost in opts.Env.
	networkMode := runtime.ResolveHostNetworking(m.Runtime.Name(), opts.Env)
	if networkMode != "" {
		opts.Env["SCION_NETWORK_MODE"] = networkMode
	}

	// Persist harness auth override to scion-agent.json so sciontool inside the container sees it.
	// The actual auth resolution override is applied earlier in the auth gathering block.
	//
	// When opts.HarnessAuth is empty but finalScionCfg carries a corrupted
	// auth_selectedType (harness implementation name from a prior bug), clear
	// it and rewrite the file to break the self-perpetuating corruption cycle.
	if opts.HarnessAuth != "" {
		if finalScionCfg == nil {
			finalScionCfg = &api.ScionConfig{}
		}
		finalScionCfg.AuthSelectedType = opts.HarnessAuth
		cfgData, marshalErr := json.MarshalIndent(finalScionCfg, "", "  ")
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to marshal agent config: %w", marshalErr)
		}
		configPath := filepath.Join(agentDir, "scion-agent.json")
		if writeErr := os.WriteFile(configPath, cfgData, 0644); writeErr != nil {
			return nil, fmt.Errorf("failed to write agent config %s: %w", configPath, writeErr)
		}
	} else if finalScionCfg != nil && harness.IsHarnessImplementationName(finalScionCfg.AuthSelectedType) {
		// Active corruption repair: clear the corrupted value and rewrite.
		finalScionCfg.AuthSelectedType = ""
		cfgData, marshalErr := json.MarshalIndent(finalScionCfg, "", "  ")
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to marshal repaired agent config: %w", marshalErr)
		}
		configPath := filepath.Join(agentDir, "scion-agent.json")
		if writeErr := os.WriteFile(configPath, cfgData, 0644); writeErr != nil {
			return nil, fmt.Errorf("failed to write agent config %s: %w", configPath, writeErr)
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
		provisionEnv, err := nativeTelemetryProvisionEnvForHarness(h.Name(), agentHome, effectiveTelemetry, opts.Env, opts.ResolvedSecrets)
		if err != nil {
			return nil, fmt.Errorf("failed to determine native telemetry backend: %w", err)
		}
		if err := telemetryApplier.ApplyTelemetrySettings(agentHome, effectiveTelemetry, provisionEnv); err != nil {
			return nil, fmt.Errorf("failed to apply telemetry settings: %w", err)
		}
	}

	// Stage universal mcp_servers map for the harness's container-side
	// provisioner. Built-in Go harnesses do not implement MCPSettingsApplier
	// and continue to use any inline MCP config in their home/ files.
	if mcpApplier, ok := h.(api.MCPSettingsApplier); ok && finalScionCfg != nil && len(finalScionCfg.MCPServers) > 0 {
		if err := mcpApplier.ApplyMCPSettings(agentHome, finalScionCfg.MCPServers); err != nil {
			return nil, fmt.Errorf("failed to apply mcp settings: %w", err)
		}
		util.Debugf("mcp: staged %d server(s) for harness=%q", len(finalScionCfg.MCPServers), harnessName)
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

	agentEnv, envWarnings, missingEnvKeys, droppedConfigEnv := buildAgentEnv(finalScionCfg, opts.Env, api.HubAgentDefaultsFromContext(ctx).DefaultEnv(), opts.BrokerMode)
	droppedBrokerEnvVars = append(droppedBrokerEnvVars, droppedConfigEnv...)
	hubOnlyEnvWarnings := warnDroppedBrokerEnv(agentID, opts.Env, droppedBrokerEnvVars)
	warnings = append(warnings, hubOnlyEnvWarnings...)
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

	// On resume/restart opts.Workspace is empty, so re-derive the explicit intent
	// from the persisted config to keep the explicit workspace plain-mounted.
	explicitWorkspace := opts.Workspace != "" || (finalScionCfg != nil && finalScionCfg.ExplicitWorkspace)
	repoRoot := detectRepoRoot(explicitWorkspace, effectiveWorkspace, projectDir)
	// Empty-per-agent (design #2703): the workspace is exactly the private
	// agents/<slug>/workspace directory, never a git checkout, so no repo
	// root is mounted even when projectDir sits in a git repository, and any
	// other source (e.g. a persisted /workspace volume) is refused.
	emptyPerAgentWorkspace := ""
	if emptyPerAgent {
		emptyPerAgentWorkspace = filepath.Join(agentDir, "workspace")
		if explicitWorkspace || filepath.Clean(effectiveWorkspace) != emptyPerAgentWorkspace {
			return nil, fmt.Errorf("empty-per-agent agent %q must use its private workspace %s, not %q", opts.Name, emptyPerAgentWorkspace, effectiveWorkspace)
		}
		repoRoot = ""
	}

	// Reject a workspace source that is not an allowed workspace path before
	// anything derived from it is used to set up a mount. This also catches
	// a value that was resolved and persisted by an older version of the
	// resolution logic above — it runs on every start, not just the ones
	// that just computed effectiveWorkspace fresh. Everything between here
	// and the workspace-storage-backend block below (if any) uses the
	// resolved, symlink-free path the validator returns, not the original
	// value, narrowing the window between "the path we checked" and "the
	// path we mount" to components that change after this call returns (see
	// ValidateWorkspaceSource's doc comment for what that does and does not
	// cover). A workspace-storage backend's own Realize call, below, can
	// still replace effectiveWorkspace with its own mount.HostPath
	// afterward -- a value this validator never sees, computed by that
	// backend's own resolver rather than read back from persisted or
	// request-supplied state.
	var roots []string
	if emptyPerAgentWorkspace != "" {
		// Contained in itself: settings.WorkspacePath or the project's repo
		// root, which workspaceSourceRoots derives, need not contain it, so
		// skip deriving them (and any error doing so) entirely.
		roots = []string{emptyPerAgentWorkspace}
	} else {
		var rootsErr error
		roots, rootsErr = workspaceSourceRoots(explicitWorkspace, effectiveWorkspace, settings, projectDir)
		if rootsErr != nil {
			return nil, rootsErr
		}
	}
	resolvedWorkspace, err := runtime.ValidateWorkspaceSource(effectiveWorkspace, roots...)
	if err != nil {
		// A global agent provisioned before <projectDir>/workspace existed
		// (or one whose persisted workspace was otherwise never brought
		// under it) is intentionally refused here on resume, now that the
		// global project's root is <projectDir>/workspace itself. The same
		// applies to a hub-dispatched, non-git project-configs project
		// whose persisted workspace is its own bare externalized directory
		// rather than a per-agent subdirectory under it (isAllowedProjectConfigsSubtree
		// never admits the bare directory itself). Name the two ways to
		// recover, since neither is obvious from the base error.
		if !explicitWorkspace && (config.IsGlobalProjectDir(projectDir) || isProjectConfigsPath(projectDir)) {
			return nil, fmt.Errorf("%w (delete and recreate this agent, or restart it with an explicit --workspace, to use a workspace under the project's own directory)", err)
		}
		return nil, err
	}
	effectiveWorkspace = resolvedWorkspace

	// Resolve repoRoot through any symlinks so the mount layout below
	// (ResolveContainerWorkspace and RunConfig.RepoRoot) is computed from the
	// same real, symlink-free spelling as effectiveWorkspace — otherwise a
	// symlinked repo path changes the container-side layout depending on
	// which of the two happens to still contain the symlink.
	if repoRoot != "" {
		resolvedRepoRoot, err := filepath.EvalSymlinks(repoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve repo root %s: %w", repoRoot, err)
		}
		repoRoot = resolvedRepoRoot
	}

	// Validate that the workspace directory exists before attempting to mount it.
	// This is a safety net — GetAgent should have created/recreated it, but if
	// the directory is still missing we fail early with a clear error rather than
	// letting the container runtime produce a cryptic mount error.
	if effectiveWorkspace != "" {
		if _, err := os.Stat(effectiveWorkspace); os.IsNotExist(err) {
			return nil, fmt.Errorf("workspace directory does not exist: %s (try deleting and recreating the agent)", effectiveWorkspace)
		}
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

	// Inject shared directory volumes from project settings or opts (hub-dispatched)
	var effectiveSharedDirs []api.SharedDir
	if settings != nil && len(settings.SharedDirs) > 0 {
		effectiveSharedDirs = settings.SharedDirs
	} else if len(opts.SharedDirs) > 0 {
		effectiveSharedDirs = opts.SharedDirs
	}
	// server.shared_dir_storage is global-only (design §3.2.1, AC5): read it
	// via config.LoadGlobalSettingsWithOverlay() (the global file plus the
	// co-located hub's DB overlay for runtimes and profiles, which can
	// override the backend per profile), never from the project-merged
	// `settings` above and never via LoadEffectiveSettings("") — an empty
	// path is NOT global-only, since it resolves a project from the
	// process's current working directory and merges that project's
	// settings.yaml on top of global (round 2 review findings C1/T1/S-F2).
	// A project's settings, including in-repo content from a cloned
	// repository, must not be able to redirect Docker bind-mount sources or
	// override the operator's NFS config. workspace_storage is untouched
	// and keeps its existing (pre-existing, out of scope) project-level
	// exposure — see design §3.2.6.
	var sharedDirStorageCfg *config.V1SharedDirStorageConfig
	recordedSharedDirBackend := ""
	// sharedDirStorageResolved is set only when the backend was chosen from
	// successfully loaded global settings, so a start that fell back to the
	// local layout after a load error never records that fallback.
	sharedDirStorageResolved := false
	// startGlobalSettings is the global settings snapshot the shared-dir
	// backend was chosen from, when it was loaded; the home storage below
	// is resolved from the same snapshot.
	var startGlobalSettings *config.VersionedSettings
	if len(effectiveSharedDirs) > 0 {
		recorded, recErr := readSharedDirStorageRecord(agentDir)
		if recErr != nil {
			return nil, recErr
		}
		recordedSharedDirBackend = recorded
		globalSettings, _, gErr := config.LoadGlobalSettingsWithOverlay()
		if gErr != nil {
			// A broken global settings file must fail closed (design G5)
			// ONLY when the operator plausibly intended to configure
			// shared_dir_storage — round 3 review disposition 6' (amended):
			// failing closed on every malformed global settings file,
			// including deployments that never touched this feature, would
			// regress essentially every agent start, since every project
			// has a default scratchpad shared dir (main tolerates this
			// exact input and starts with the legacy local layout). We
			// can't parse the broken file to check the real value, so fall
			// back to a raw substring check on its bytes.
			if config.GlobalSettingsMentions("shared_dir_storage") {
				return nil, fmt.Errorf("loading global settings for server.shared_dir_storage: %w", gErr)
			}
			slog.Warn("Start: failed to load global settings; server.shared_dir_storage was not found in the raw file, proceeding with the local shared-dir layout",
				"error", gErr)
		} else if globalSettings != nil && (globalSettings.Server == nil || globalSettings.Server.SharedDirStorage == nil) &&
			config.GlobalSettingsIsLegacyFormat() && config.GlobalSettingsMentions("shared_dir_storage") {
			// Round 4 review finding S-L1: a global settings.yaml with no
			// "schema_version: \"1\"" takes the LEGACY loader path, which
			// silently drops the entire server block — LoadGlobalSettings
			// returns err == nil with Server == nil, so the gErr != nil
			// branch above never runs. Without this check, an operator's
			// shared_dir_storage config would vanish with only a generic
			// "unrecognized keys" WARN, which is a worse silent failure
			// than the malformed-YAML case, and contradicts G5 fail-closed
			// for an operator who plausibly intended to configure it.
			//
			// Round 5 review finding C1=T1=S-L2: GlobalSettingsMentions is a
			// raw substring check, so it also fires on a well-formed v1 file
			// whose only mention of the key is a YAML comment (e.g. a
			// commented-out "# shared_dir_storage:" block, which is the
			// design's documented rollback path). For a file that LOADED
			// successfully as v1, the loader above is authoritative — no
			// parsed block means it genuinely was not configured, exactly
			// like main. The GlobalSettingsIsLegacyFormat() gate restricts
			// this fail-closed substring check to files that actually took
			// the legacy loader path, where there is no parsed struct to
			// trust and the substring is the only available signal; the
			// "(missing schema_version...)" wording below is therefore
			// always accurate when this branch fires.
			return nil, fmt.Errorf(
				"global settings mention server.shared_dir_storage but it was not loaded (missing schema_version: \"1\"?)")
		} else if globalSettings != nil {
			startGlobalSettings = globalSettings
			// The backend can be overridden per profile or runtime entry.
			// The profile is the one named for this start, else the one
			// the agent was created with (as for the shared-dir PVC
			// defaults below); an agent that recorded its backend keeps it.
			sdStorageProfile := opts.Profile
			if sdStorageProfile == "" && finalScionCfg != nil && finalScionCfg.Info != nil {
				sdStorageProfile = finalScionCfg.Info.Profile
			}
			cfg, err := selectSharedDirStorage(globalSettings, sdStorageProfile, recordedSharedDirBackend, opts.Name)
			if err != nil {
				return nil, err
			}
			sharedDirStorageCfg = cfg
			sharedDirStorageResolved = true
		}
	}
	// nfs shared_dir_storage keys its layout on hubDispatchedProjectID,
	// snapshotted above at Start entry — NOT the general projectID variable
	// (which falls back to settings.Hub.ProjectID / the project-id file)
	// and NOT a fresh read of opts.Env here. By this point opts.Env may
	// already have been filled in from project-level harness-config env by
	// resolveAuthEnvOverlay (it only fills *absent* keys, but that includes
	// SCION_PROJECT_ID when the hub didn't dispatch this
	// start), so re-reading opts.Env at this line would reopen exactly the
	// hole this snapshot closes (round 3 review finding C1/S-L1). The hub
	// sets SCION_PROJECT_ID unconditionally after merging any
	// user-supplied env for hub-dispatched starts
	// (pkg/hub/httpdispatcher.go DispatchAgentStart/DispatchAgentRestart,
	// "Identity vars at highest precedence"), so hubDispatchedProjectID is
	// authoritative whenever the hub dispatched this agent, and correctly
	// empty otherwise — project settings cannot choose which project's
	// shared tree an nfs-backed agent mounts (round 2 review finding S-F4).
	//
	// F-111 review (BLOCKING): resolveSharedDirs needs to know whether
	// server.workspace_storage.backend is "nfs" — a different config block
	// from sharedDirStorageCfg (server.shared_dir_storage) — so its local/
	// default branch can validate shared-dir names when they're about to
	// become NFS subPaths via the k8s runtime's nfsSharedDirs path.
	nfsWorkspaceBackend := settings != nil && settings.Server != nil &&
		settings.Server.WorkspaceStorage != nil && settings.Server.WorkspaceStorage.Backend == "nfs"
	sharedDirVolumes, sharedDirStorage, err := resolveSharedDirs(
		sharedDirStorageCfg, projectDir, hubDispatchedProjectID, m.Runtime.Name(), effectiveSharedDirs, containerWorkspace, nfsWorkspaceBackend)
	if err != nil {
		return nil, err
	}
	if len(effectiveSharedDirs) > 0 && recordedSharedDirBackend == "" && sharedDirStorageResolved {
		// Record the backend the agent's shared dirs were set up with, so
		// later starts keep using it even if settings change.
		if err := writeSharedDirStorageRecord(agentDir, sharedDirStorageBackendName(sharedDirStorageCfg)); err != nil {
			slog.Warn("Start: could not record the agent's shared-dir storage backend", "agent", opts.Name, "error", err)
		}
	}
	// Home storage: local, or (Kubernetes only) an NFS home on the export of
	// the profile's shared_dir_storage. Chosen at the agent's first start
	// and recorded, before any pod exists; later starts use the record.
	homeStorageProfile := opts.Profile
	if homeStorageProfile == "" && finalScionCfg != nil && finalScionCfg.Info != nil {
		homeStorageProfile = finalScionCfg.Info.Profile
	}
	homePlan, err := resolveHomeStorage(homeStorageInput{
		AgentDir:     agentDir,
		AgentName:    opts.Name,
		Slug:         slug,
		RuntimeName:  m.Runtime.Name(),
		Profile:      homeStorageProfile,
		AgentID:      hubDispatchedAgentID,
		ProjectID:    hubDispatchedProjectID,
		ExperimentOn: api.HubAgentDefaultsFromContext(ctx).ExperimentEnabled(experiments.K8sNFSHome),
		LoadSettings: func() (*config.VersionedSettings, error) {
			if startGlobalSettings != nil {
				return checkHomeStorageLoaded(startGlobalSettings)
			}
			return loadHomeStorageSettings()
		},
	})
	if err != nil {
		return nil, err
	}
	homeStorage, err := prepareHomeStorage(homePlan, hubDispatchedProjectID, slug)
	if err != nil {
		return nil, err
	}
	homeStorageBackend := ""
	if homeStorage != nil {
		homeStorageBackend = runtime.HomeStorageNFS
	}

	if len(sharedDirVolumes) > 0 {
		// Add SCION_VOLUMES env var for discoverability
		opts.Env["SCION_VOLUMES"] = "/scion-volumes"
	}

	workspaceBackendName := ""
	nfsUID := 0
	nfsGID := 0
	nfsPVClaimName := ""
	nfsSubPath := ""
	nfsSubPathRoot := ""
	nfsStorageClass := ""
	nfsWorkspacePreCreated := false
	nfsWorktreeName := ""
	nfsWorktreeBranch := ""
	nfsAgentDirName := ""
	nfsAgentBranch := ""
	nfsAgentDirEmpty := false

	if settings != nil && settings.Server != nil && settings.Server.WorkspaceStorage != nil {
		sharingMode := store.SharingModeWorktreePerAgent
		if opts.SharedWorkspace || opts.GitClone != nil {
			sharingMode = store.SharingModeSharedPlain
		}
		// Empty-per-agent never takes the WorktreePerAgent default above: it
		// has no shared checkout. Without NFS storage it is node-local (or
		// pod-local EmptyDir on Kubernetes). With NFS storage it gets its
		// own agent directory on the export (design #2703 P3), which only
		// the Kubernetes runtime can mount: see nfsEmptyAgentDirSelection.
		var emptyAgentDirName string
		if emptyPerAgent {
			sharingMode = store.SharingModeEmptyPerAgent
			var selErr error
			emptyAgentDirName, selErr = nfsEmptyAgentDirSelection(m.Runtime.Name(), settings.Server.WorkspaceStorage, opts.Name)
			if selErr != nil {
				return nil, selErr
			}
		}
		// On Kubernetes, a git project dispatched in worktree-per-agent mode
		// gets its own worktree under the shared checkout. Shared-plain and
		// every other runtime keep the layout above.
		// A git project dispatched in clone-per-agent mode gets its own
		// agent directory next to the project's workspace path instead,
		// with a workspace the agent container clones into. Paths are
		// still resolved from the project's workspace path (shared-plain).
		var worktreeName, worktreeBranch, agentDirName, agentBranch string
		if isKubernetesRuntime(m.Runtime.Name()) && !emptyPerAgent {
			worktreeName, worktreeBranch = nfsWorktreeSelection(opts.Env, opts.GitClone, opts.Name)
			if settings.Server.WorkspaceStorage.Backend == "nfs" {
				var selErr error
				agentDirName, agentBranch, selErr = nfsAgentDirSelection(opts.Env, opts.GitClone, opts.Name)
				if selErr != nil {
					return nil, selErr
				}
			}
		}
		if worktreeName != "" {
			sharingMode = store.SharingModeWorktreePerAgent
		}
		backend := runtime.SelectWorkspaceBackend(settings.Server.WorkspaceStorage, sharingMode)
		if backend.Name() == "nfs" {
			sharedDirNames := make([]string, 0, len(effectiveSharedDirs))
			for _, dir := range effectiveSharedDirs {
				sharedDirNames = append(sharedDirNames, dir.Name)
			}
			resolvedWorkspace, err := backend.Resolve(runtime.ResolveInput{
				ProjectID:      projectID,
				AgentID:        agentID,
				ProjectSlug:    api.Slugify(projectName),
				Mode:           sharingMode,
				SharedDirNames: sharedDirNames,
				ProjectDir:     projectDir,
			})
			if err != nil {
				return nil, fmt.Errorf("resolve workspace backend %q: %w", backend.Name(), err)
			}
			mount, err := backend.Realize(runtime.RealizeInput{
				Resolved:           resolvedWorkspace,
				ContainerWorkspace: containerWorkspace,
			})
			if err != nil {
				return nil, fmt.Errorf("realize workspace backend %q: %w", backend.Name(), err)
			}
			// Create the workspace subPath directory, and the directories of
			// shared dirs served from the same claim, before the pod exists,
			// so the kubelet does not have to (ptone/scion#2530). Shared dirs
			// with their own storage (sharedDirStorage set) are not on this
			// claim.
			var claimSharedDirNames []string
			if sharedDirStorage == nil {
				claimSharedDirNames = sharedDirNames
			}
			if emptyAgentDirName != "" {
				// Empty-per-agent: only the agent's own directory is ever
				// mounted, never the project's workspace path resolved
				// above. Without a PV claim the pod could not mount it, so
				// stop instead of falling back to the project's path.
				if mount.PVClaimName == "" {
					return nil, errEmptyPerAgentNFSNoClaim
				}
				nfsWorkspacePreCreated, err = ensureNFSAgentWorkspaceLeaf(m.Runtime.Name(), projectID, resolvedWorkspace, mount.PVClaimName, claimSharedDirNames, emptyAgentDirName)
				nfsAgentDirName = emptyAgentDirName
				nfsAgentDirEmpty = true
			} else if agentDirName != "" && mount.PVClaimName != "" {
				nfsWorkspacePreCreated, err = ensureNFSAgentWorkspaceLeaf(m.Runtime.Name(), projectID, resolvedWorkspace, mount.PVClaimName, claimSharedDirNames, agentDirName)
				nfsAgentDirName = agentDirName
				nfsAgentBranch = agentBranch
			} else {
				nfsWorkspacePreCreated, err = ensureNFSWorkspaceLeaf(m.Runtime.Name(), projectID, resolvedWorkspace, mount.PVClaimName, claimSharedDirNames)
			}
			if err != nil {
				return nil, err
			}
			if worktreeName != "" && mount.PVClaimName != "" {
				worktreePreCreated, err := ensureNFSWorktreeLeaf(m.Runtime.Name(), resolvedWorkspace, mount.PVClaimName, worktreeName)
				if err != nil {
					return nil, err
				}
				nfsWorkspacePreCreated = nfsWorkspacePreCreated && worktreePreCreated
				nfsWorktreeName = worktreeName
				nfsWorktreeBranch = worktreeBranch
			}

			workspaceBackendName = backend.Name()
			// Empty-per-agent keeps its private node-local path as the
			// workspace source: mount.HostPath is the project's shared
			// workspace, which this agent must never see.
			if mount.HostPath != "" && !nfsAgentDirEmpty {
				effectiveWorkspace = mount.HostPath
			}
			if mount.Target != "" {
				containerWorkspace = mount.Target
			}
			if nfsWorktreeName != "" {
				// The agent works in its worktree, mounted next to the
				// shared .git the same way as on the local runtimes. The
				// pod mounts both by subPath (see buildPod).
				containerWorkspace = runtime.NFSWorktreeContainerPath(nfsWorktreeName)
			}
			nfsPVClaimName = mount.PVClaimName
			nfsSubPath = mount.SubPath
			if settings.Server.WorkspaceStorage.NFS != nil {
				nfsSubPathRoot = settings.Server.WorkspaceStorage.NFS.SubPathRoot
				nfsUID = settings.Server.WorkspaceStorage.NFS.UID
				nfsGID = settings.Server.WorkspaceStorage.NFS.GID
				nfsStorageClass = settings.Server.WorkspaceStorage.NFS.StorageClass
			}
		}
	}

	// The agent's workspace placement, reported to the hub on every start
	// that gets here (so a re-provision refreshes it). See
	// workspacePlacementFor for when the workspace is on the export.
	workspacePlacement := workspacePlacementFor(workspacePlacementInput{
		Backend:       workspaceBackendName,
		Runtime:       m.Runtime.Name(),
		PVClaimName:   nfsPVClaimName,
		ClonePerAgent: opts.GitClone != nil && store.ResolveWorkspaceSharingMode(opts.Env["SCION_WORKSPACE_MODE"]) == store.SharingModeClonePerAgent,
		AgentDirName:  nfsAgentDirName,
	})

	// Kubernetes shared-dir PVC defaults from settings: the profile's value,
	// else its runtime entry's (applied below under the template/agent
	// kubernetes block). The profile is the one named for this start, else
	// the one the agent was created with. When shared-dir PVCs will be
	// needed, check the effective size here so a bad value fails with the
	// place it is set rather than a bare parse error from the runtime.
	//
	// safe_to_evict follows the same order: template/agent
	// kubernetes.safeToEvict, then the profile, then its runtime entry. On
	// other runtimes it is accepted and ignored, with a warning.
	var sdClass, sdSize string
	var settingsSafeToEvict *bool
	sdProfile := opts.Profile
	if sdProfile == "" && finalScionCfg != nil && finalScionCfg.Info != nil {
		sdProfile = finalScionCfg.Info.Profile
	}
	var tmplSafeToEvict *bool
	if finalScionCfg != nil && finalScionCfg.Kubernetes != nil {
		tmplSafeToEvict = finalScionCfg.Kubernetes.SafeToEvict
	}
	var safeToEvictSource string
	if settings != nil {
		settingsSafeToEvict, safeToEvictSource = settings.ResolveSafeToEvictWithSource(sdProfile)
	}
	effectiveSafeToEvict, safeToEvictFrom := settingsSafeToEvict, "settings "+safeToEvictSource
	if tmplSafeToEvict != nil {
		effectiveSafeToEvict, safeToEvictFrom = tmplSafeToEvict, "kubernetes.safeToEvict in the agent or template config"
	}
	if effectiveSafeToEvict != nil {
		if m.Runtime.Name() == "kubernetes" {
			slog.Debug("Start: resolved safe_to_evict", "agent", opts.Name, "value", *effectiveSafeToEvict, "source", safeToEvictFrom)
		} else {
			slog.Warn("Start: safe_to_evict applies only to the Kubernetes runtime; ignoring it", "agent", opts.Name, "runtime", m.Runtime.Name(), "source", safeToEvictFrom)
			settingsSafeToEvict = nil
		}
	}
	if settings != nil && m.Runtime.Name() == "kubernetes" {
		var sdSizeKey string
		sdClass, sdSize, sdSizeKey = settings.ResolveSharedDirDefaultsWithSource(sdProfile)
		if len(effectiveSharedDirs) > 0 {
			size, source := sdSize, "settings "+sdSizeKey
			if finalScionCfg != nil && finalScionCfg.Kubernetes != nil && finalScionCfg.Kubernetes.SharedDirSize != "" {
				size, source = finalScionCfg.Kubernetes.SharedDirSize, "kubernetes.shared_dir_size in the agent or template config"
			}
			if err := config.ValidateSharedDirSize(size); err != nil {
				return nil, fmt.Errorf("%s: %w", source, err)
			}
		}
	}

	// The run ID was fixed (minted if absent) at the top of Start.
	runID := opts.RunID
	// SCION_LAUNCH_ID carries the same value as the run label, so the
	// container's env and label cannot disagree. Any value from the
	// request or template env is replaced.
	agentEnv = withLaunchIDEnv(agentEnv, runID)

	runCfg := runtime.RunConfig{
		Name:                 containerName(projectName, opts.Name),
		Template:             template,
		UnixUsername:         unixUsername,
		Image:                resolvedImage,
		HomeDir:              agentHome,
		Workspace:            effectiveWorkspace,
		RepoRoot:             repoRoot,
		ContainerWorkspace:   containerWorkspace,
		ResolvedAuth:         resolvedAuth,
		Harness:              h,
		Project:              projectName,
		ProjectID:            projectID,
		WorkspaceBackendName: workspaceBackendName,
		NFSUID:               nfsUID,
		NFSGID:               nfsGID,
		NFSPVClaimName:       nfsPVClaimName,
		NFSSubPath:           nfsSubPath,
		NFSSubPathRoot:       nfsSubPathRoot,
		NFSStorageClass:      nfsStorageClass,
		// Lets the provisioning init container treat a failed chown as a
		// warning for a workspace directory the broker created.
		NFSWorkspacePreCreated: nfsWorkspacePreCreated,
		// Set only for worktree-per-agent git projects on the NFS backend:
		// the agent mounts worktrees/<agentID> and the shared .git instead
		// of the shared checkout.
		NFSWorktreeName:   nfsWorktreeName,
		NFSWorktreeBranch: nfsWorktreeBranch,
		// Set only for clone-per-agent git projects on the NFS backend: the
		// agent mounts agents/<agent name>/workspace and clones into it.
		NFSAgentDirName: nfsAgentDirName,
		NFSAgentBranch:  nfsAgentBranch,
		// Set only for empty-per-agent projects on the NFS backend: the
		// agent directory's workspace stays empty (no branch, no clone).
		NFSAgentDirEmpty: nfsAgentDirEmpty,
		// F-111 (design §9): drives the k8s runtime's NFS init container's
		// clone-vs-plain-provision choice (nfsProvisionCommand), not whether
		// provisioning happens at all — the init container is now gated
		// solely on WorkspaceBackendName=="nfs" && NFSPVClaimName != ""
		// (k8s_runtime.go's nfsInitContainerInjected), so a nil GitClone here
		// (a non-git, shared-plain project) still gets mkdir+chown, just no
		// clone. Mirrors the existing GitClone field above/below, which this
		// package already sets from the same opts.GitClone for other
		// purposes; previously nothing set GitCloneForInit at all, so the
		// k8s init container never ran for ANY project, git or not.
		// A shared-plain git project has no GitClone; its workspace clone
		// settings are used instead (nfsInitGitClone).
		GitCloneForInit:  nfsInitGitClone(opts),
		TelemetryEnabled: telemetryEnabled,
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
				// Whenever there is an effective workspace at all, buildCommonRunArgs's
				// own workspace-mount logic already handles it -- at /workspace in most
				// cases, but at /repo-root/<rel> for the worktree-subdirectory case,
				// which leaves the /workspace target slot free for a raw volume to
				// claim. Filter any /workspace-target volume out of the generic list in
				// every such case, rather than comparing effectiveWorkspace against
				// agentWorkspace by value: effectiveWorkspace is the resolved,
				// symlink-free path (see the validation call above), so that comparison
				// would silently start filtering (or stop filtering) purely because
				// agentWorkspace is not canonical, with no actual change in what is
				// being mounted.
				if effectiveWorkspace != "" {
					before := len(finalScionCfg.Volumes)
					volumes = filterWorkspaceVolume(finalScionCfg.Volumes)
					if len(volumes) < before {
						warnings = append(warnings, "ignoring volume targeting /workspace: the agent workspace is mounted by the runtime")
					}
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
			// Start from the template/agent config's Kubernetes settings
			// (namespace, resources, node selector, etc.), then ALWAYS
			// assign the separately-resolved ImagePullPolicy on top — never
			// only when it's non-empty. resolvedPullPolicy already covers
			// every legitimate tier (live template, current inline,
			// recorded inline, settings, file default), so it is
			// authoritative the same way resolvedImage is: a guarded
			// assignment would let finalScionCfg.Kubernetes.ImagePullPolicy
			// (a value ProvisionAgent persisted at an earlier provision)
			// survive as a stale fallback when every current tier resolves
			// to empty — e.g. after an operator removes a settings or
			// template pull-policy pin — exactly the staleness this
			// package's Image handling was written to avoid.
			//
			// Shared-dir PVC defaults from settings (profile, then the
			// profile's runtime entry) are filled in only where the
			// template/agent kubernetes block leaves them empty, and only
			// on the Kubernetes runtime. They are resolved here at start
			// time rather than persisted by ProvisionAgent, so a settings
			// change applies on the next start, matching ImagePullPolicy.
			var k8sCfg *api.KubernetesConfig
			if finalScionCfg != nil && finalScionCfg.Kubernetes != nil {
				cpy := *finalScionCfg.Kubernetes
				k8sCfg = &cpy
			}
			if resolvedPullPolicy != "" || k8sCfg != nil {
				if k8sCfg == nil {
					k8sCfg = &api.KubernetesConfig{}
				}
				k8sCfg.ImagePullPolicy = resolvedPullPolicy
			}
			if sdClass != "" || sdSize != "" {
				k8sCfg = config.ApplySharedDirDefaults(k8sCfg, sdClass, sdSize)
			}
			// safe_to_evict from settings (profile, then runtime entry),
			// only where the template/agent leaves it unset. Nil off
			// Kubernetes (cleared above).
			k8sCfg = config.ApplySafeToEvictDefault(k8sCfg, settingsSafeToEvict)
			// The broker-resolved Workload Identity ServiceAccount (GCP
			// identity mode "assign") is applied over the template and
			// persisted value, but only when non-empty: empty means no
			// mapping applies to this dispatch, not "clear the value". It is
			// not part of finalScionCfg, so it also applies when starting or
			// restarting an existing agent.
			if opts.ResolvedKubernetesServiceAccountName != "" {
				if k8sCfg == nil {
					k8sCfg = &api.KubernetesConfig{}
				}
				k8sCfg.ServiceAccountName = opts.ResolvedKubernetesServiceAccountName
			}
			return k8sCfg
		}(),
		GitClone:           opts.GitClone,
		TrustedHubEndpoint: trustedHubEndpoint,
		SharedDirs:         effectiveSharedDirs,
		SharedDirStorage:   sharedDirStorage,
		HomeStorageBackend: homeStorageBackend,
		HomeStorage:        homeStorage,
		BrokerMode:         opts.BrokerMode,
		NoAuth: opts.NoAuth && noAuthConfig != nil &&
			(noAuthConfig.Behavior == "drop-to-shell" || noAuthConfig.Behavior == "allow"),
		NoAuthMessage: func() string {
			if opts.NoAuth && noAuthConfig != nil && noAuthConfig.Behavior == "drop-to-shell" {
				return noAuthConfig.Message
			}
			return ""
		}(),
		NoAuthCommand: func() string {
			if opts.NoAuth && noAuthConfig != nil && noAuthConfig.Behavior == "drop-to-shell" {
				return noAuthConfig.Command
			}
			return ""
		}(),
		Debug:                util.DebugEnabled(),
		Resume:               opts.Resume,
		MetadataInterception: hasMetadataInterception(agentEnv),
		ExtraHosts:           mergeExtraHosts(opts.ExtraHosts, runtime.BridgeExtraHosts(m.Runtime.Name(), agentEnv)),
		NetworkMode:          networkMode,
		Labels: func() map[string]string {
			l := map[string]string{
				"scion.agent":          "true",
				"scion.name":           api.Slugify(opts.Name),
				"scion.template":       template,
				"scion.harness_config": harnessConfigName,
				"scion.harness_auth":   opts.HarnessAuth,
				"agent_id":             agentID,
				api.LabelRunID:         runID,
			}
			for k, v := range projectkeys.ProjectNameLabels(projectName) {
				l[k] = v
			}
			// Add project_id label for project-scoped agent isolation.
			if projectID != "" {
				for k, v := range projectkeys.ProjectIDLabels(projectID) {
					l[k] = v
				}
			}
			return l
		}(),
		Annotations: projectkeys.ProjectPathLabels(projectDir),
		// Async-launch hooks (design t1-async-create-v11.md §3.8.3,
		// §3.8.4); nil on the synchronous path.
		Checkpoint:        opts.Checkpoint,
		OnResourceCreated: opts.OnResourceCreated,
	}
	slog.Info("agent start: pre-runtime provisioning complete", "agent", opts.Name,
		"elapsed_ms", time.Since(startEntry).Milliseconds())
	id, err := m.Runtime.Run(ctx, runCfg)
	if err != nil {
		// Provisioning writes agent-info.json in "created" state before the
		// runtime launch. If the launch itself fails, keep the provisioned
		// workspace but flip the local state to "error" so list/status do not
		// report a phantom created agent forever.
		if updateErr := UpdateAgentConfig(opts.Name, opts.ProjectPath, "error", m.Runtime.Name(), profileName); updateErr != nil {
			util.Debugf("Start: failed to mark agent error in local config: %v", updateErr)
		}
		return nil, classifyLaunchRuntimeError(err, resolvedImage)
	}
	slog.Info("agent start: runtime.Run complete", "agent", opts.Name,
		"total_elapsed_ms", time.Since(startEntry).Milliseconds())

	// Phase is always "running" here, for both a fresh start and a resume:
	// state.Phase has no "resumed" value, and a non-standard phase string
	// confuses readers that only understand the canonical lifecycle phases
	// (e.g. the hub's post-wake readiness wait). Resume-specific display
	// text is derived independently at the CLI layer (see displayStatus in
	// cmd/common.go), so it does not need to be encoded in Phase.
	status := string(state.PhaseRunning)
	if updateErr := UpdateAgentConfig(opts.Name, opts.ProjectPath, status, m.Runtime.Name(), profileName); updateErr != nil {
		util.Debugf("Start: failed to update local agent status to %q: %v", status, updateErr)
	}

	// Fetch fresh info and verify the container is actually running
	allAgents, err := m.Runtime.List(ctx, map[string]string{"scion.name": slug})
	if err == nil {
		for _, a := range allAgents {
			if a.ContainerID == id || strings.EqualFold(a.Name, opts.Name) {
				// Check if the container has already exited
				if a.Phase == string(state.PhaseStopped) || a.Phase == string(state.PhaseError) {
					// Try to get logs for diagnosis
					logs, _ := m.Runtime.GetLogs(ctx, id)
					_ = m.Runtime.Delete(ctx, runtime.RunRef{ID: id, RunID: runID})
					return nil, fmt.Errorf("container started but exited immediately (status: %s). Container logs:\n%s", a.ContainerStatus, logs)
				}
				a.Detached = detached
				a.Warnings = warnings
				a.HubOnlyEnvWarnings = hubOnlyEnvWarnings
				a.Phase = status
				a.HarnessConfig = harnessConfigName
				a.HarnessConfigRevision = harnessConfigRevision
				a.HarnessAuth = opts.HarnessAuth
				a.Profile = profileName
				a.WorkspacePlacement = workspacePlacement
				return &a, nil
			}
		}
	}

	// Container ID returned but not found in listing — it may have exited and been removed
	warnings = append(warnings, "Container started but could not be verified as running")
	return &api.AgentInfo{
		ID:                    id,
		RunID:                 runID,
		Name:                  opts.Name,
		Phase:                 status,
		Detached:              detached,
		Warnings:              warnings,
		HubOnlyEnvWarnings:    hubOnlyEnvWarnings,
		HarnessConfig:         harnessConfigName,
		HarnessConfigRevision: harnessConfigRevision,
		HarnessAuth:           opts.HarnessAuth,
		Profile:               profileName,
		WorkspacePlacement:    workspacePlacement,
	}, nil
}

// workspacePlacementInput is what a start resolved about its workspace.
type workspacePlacementInput struct {
	// Backend is the workspace backend that served the workspace ("" for
	// the local backend).
	Backend string
	// Runtime is the runtime's name (runtime.Runtime.Name()).
	Runtime string
	// PVClaimName is the NFS PV claim the pod mounts the workspace from;
	// "" when the share has no pv_name.
	PVClaimName string
	// ClonePerAgent is true when the agent was dispatched as a git
	// clone-per-agent workspace.
	ClonePerAgent bool
	// AgentDirName is the agent's own directory on the export, when one
	// was chosen (clone-per-agent and empty-per-agent on Kubernetes).
	AgentDirName string
}

// exportMountingRuntimes are the non-Kubernetes runtimes that mount the nfs
// workspace backend's resolved path itself as the agent's workspace:
//   - docker, podman, container: buildCommonRunArgs bind-mounts
//     RunConfig.Workspace (the nfs host path) at the container workspace
//     (pkg/runtime/common.go); rootless podman refuses nfs (podman.go).
//   - cloudrun: mounts the export's <subPathRoot>/<projectID>/workspace as
//     an NFS volume at the container workspace (cloudrun_runtime.go
//     provisionCloudRunNFS and the "workspace" volume in Run).
//
// Any other runtime is not known to work on the export: cloudrun-sandbox
// copies the workspace to broker-local disk, substrate never mounts it, and
// a new runtime must be added here deliberately.
var exportMountingRuntimes = map[string]bool{
	"docker":    true,
	"podman":    true,
	"container": true,
	"cloudrun":  true,
}

// workspacePlacementFor returns the placement reported to the hub. It fails
// closed: export only when the workspace is known to be mounted from the
// shared NFS export, local otherwise. On Kubernetes the pod mounts the export
// only through a PV claim (without one it gets an EmptyDir), and a
// clone-per-agent workspace is on the export only in its own agent
// directory, which also needs the claim. Other runtimes count only when
// listed in exportMountingRuntimes.
func workspacePlacementFor(in workspacePlacementInput) string {
	if in.Backend != "nfs" {
		return api.WorkspacePlacementLocal
	}
	if isKubernetesRuntime(in.Runtime) {
		if in.PVClaimName == "" {
			return api.WorkspacePlacementLocal
		}
		if in.ClonePerAgent && in.AgentDirName == "" {
			return api.WorkspacePlacementLocal
		}
		return api.WorkspacePlacementExport
	}
	if exportMountingRuntimes[in.Runtime] {
		return api.WorkspacePlacementExport
	}
	return api.WorkspacePlacementLocal
}

// writeAgentTokenFile writes the agent's hub credential to the canonical token
// file in the agent home, atomically, via a temp file plus rename. Processes
// inside the container read the file rather than an environment variable, which
// would go stale after a token refresh.
//
// EVERY ERROR NAMES THE OPERATION THAT FAILED AND THE PATH IT FAILED ON, and
// wraps the underlying *os.PathError. There are three distinct failure modes
// here with three distinct causes -- an unwritable agent home, a full or
// read-only filesystem, and a token path occupied by something that is not a
// regular file -- and a single "failed to prepare agent credentials" would
// leave an operator with no way to tell them apart. See #1348 and PROTOCOLS.md
// §5. util.Debugf is deliberately not used for the failures: per #1339, debug
// output from this subsystem may not emit at all, so a Debugf here is a failure
// report that can vanish.
func writeAgentTokenFile(agentHome, token string) error {
	scionDir := filepath.Join(agentHome, ".scion")
	if err := os.MkdirAll(scionDir, 0700); err != nil {
		return fmt.Errorf("create agent token directory %s: %w", scionDir, err)
	}

	tokenPath := filepath.Join(scionDir, "scion-token")
	tmp := tokenPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(token), 0600); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write agent token file %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, tokenPath); err != nil {
		// Best effort: the temp file holds the credential, so leaving it behind
		// on a path we are about to abort on is worse than a lost error here.
		_ = os.Remove(tmp)
		return fmt.Errorf("install agent token file %s from %s: %w", tokenPath, tmp, err)
	}

	util.Debugf("Start: wrote agent token to %s", tokenPath)
	return nil
}

// detectRepoRoot resolves the git repo root to bind-mount for an agent, or ""
// for a plain in-place workspace mount. An explicit --workspace skips git
// detection: a workspace that merely sits inside a repo must not be widened to
// the whole repo, matching ProvisionAgent, which mounts it directly "even if
// inside a repo" (provision.go). explicit is set on first start (opts.Workspace)
// and re-derived on resume from the persisted ExplicitWorkspace flag, so the
// guarantee holds on every entry path.
func detectRepoRoot(explicit bool, effectiveWorkspace, projectDir string) string {
	if explicit {
		return ""
	}
	if effectiveWorkspace != "" && util.IsGitRepoDir(effectiveWorkspace) {
		commonDir, err := util.GetCommonGitDir(effectiveWorkspace)
		if err == nil {
			return filepath.Dir(commonDir)
		}
		return ""
	}
	if util.IsGitRepoDir(projectDir) {
		root, _ := util.RepoRootDir(projectDir)
		return root
	}
	return ""
}

// workspaceSourceRoots returns the per-project root(s) a resolved workspace
// source may fall under (any one of them is sufficient), for the fail-closed
// check that runs before any mount is set up. It mirrors the same resolution
// the non-git, no-explicit-workspace branches in provision.go's
// workspaceSource logic already went through, so a legitimately resolved
// source is never rejected by the containment check:
//   - explicit workspace: no root (the containment check is skipped; an
//     explicit --workspace may point anywhere, unchanged from today);
//   - git project: this project's OWN repo root, derived from projectDir —
//     never from effectiveWorkspace itself. (A source that happens to be
//     some other git checkout entirely would otherwise trivially "verify"
//     against its own repo; the root this function returns must always come
//     from the project the agent belongs to, not from whatever the source
//     claims to be.) Plus, when effectiveWorkspace is itself a worktree git
//     has registered for THIS project's repo, verified via `git worktree
//     list` against that repo rather than guessed from its path (see
//     util.IsRegisteredWorktree) and cross-checked against the project's own
//     common git dir — that worktree's own location. `git worktree add`
//     accepts any destination, so a worktree the project attached to via an
//     existing branch (provision.go's FindWorktreeByBranch) can legitimately
//     sit anywhere on disk; git's own worktree registry, scoped to this
//     project's repo, is the containment proof for that case, not a path
//     prefix. Worktree-membership verification failure (git unavailable, for
//     example) is not treated as membership — the source falls back to
//     plain repo-root containment, same as if it had never been checked. If
//     the project's own repo root cannot be resolved to a repository that
//     actually is projectDir's own — either because resolution fails
//     outright, or because it silently resolves to a different repository
//     entirely — the whole call refuses instead of silently returning no
//     roots, which the caller would otherwise treat as "no root available,
//     check the rootless floor only" and accept far more than intended. A
//     bare repository passes util.IsGitRepoDir (which checks only the exit
//     status, not git's "false" output) but has no top level to resolve.
//     Standalone, util.RepoRootDir then fails outright and the call refuses
//     on that error alone. Nested inside another repository's work tree,
//     util.RepoRootDir's parent-directory retry on failure returns the
//     ENCLOSING repository's top level instead of erroring — a real,
//     successfully-resolved root that just isn't projectDir's own — and
//     workspaceSharesProjectRepo's common-git-dir comparison is what refuses
//     that case, since a nested bare repo's common git dir never matches the
//     enclosing repository's. The global project takes this branch too when
//     its own directory (~/.scion) is itself a git work tree (a dotfiles
//     repository, for example); that shape returns its own dedicated,
//     actionable error instead of a root, since ProvisionAgent's resulting
//     ~/.scion/agents/<name>/workspace can never pass the ~/.scion floor in
//     runtime.ValidateWorkspaceSource, and deleting and recreating the agent
//     cannot change that classification;
//   - global project, no explicit workspace: <projectDir>/workspace, the
//     directory ProvisionAgent's global-project branch creates. A global
//     agent provisioned before that directory existed, whose persisted
//     workspace is some other directory entirely, is intentionally refused
//     on resume — see the caller's actionable-error wrapping;
//   - externalized/non-git project: resolveProjectRoot's settings.WorkspacePath
//     or parent-of-.scion fallback, which contain the resolved source in
//     those branches (settings.WorkspacePath is typically equal to it; the
//     parent-of-.scion fallback can also be a genuine ancestor of it).
//
// Whether projectDir counts as a git project here is decided by
// isGitWorkspaceProject, the same helper ProvisionAgent uses to decide how
// the workspace source was first resolved — so a workspace provisioned
// through the non-git branch (SCION_HOST_UID set at provision time) is
// checked against that same non-git root here, on every start including
// resume, rather than being re-classified as git and checked against a
// repo root it was never provisioned relative to.
func workspaceSourceRoots(explicitWorkspace bool, effectiveWorkspace string, settings *config.VersionedSettings, projectDir string) ([]string, error) {
	if explicitWorkspace {
		return nil, nil
	}
	if isGitWorkspaceProject(projectDir) {
		if config.IsGlobalProjectDir(projectDir) {
			// The global project's own directory (~/.scion) is, unusually,
			// itself a git work tree (a dotfiles repository, for example).
			// isGitWorkspaceProject then routes it through this git branch
			// instead of the "global, no explicit workspace" branch below,
			// so ProvisionAgent creates a per-agent worktree/workspace at
			// ~/.scion/agents/<name>/workspace, the ordinary git-project
			// shape -- not <projectDir>/workspace. That shape can never
			// validate here: the ~/.scion floor (runtime.ValidateWorkspaceSource)
			// only admits ~/.scion/workspace, ~/.scion/projects/<slug>, and
			// ~/.scion/project-configs/<dir>/.scion/agents/<agent-id>/workspace,
			// none of which this is, and root == ~/.scion itself is refused
			// outright as a misconfigured root. Unlike every other
			// rejection this function can produce, deleting and recreating
			// the agent cannot help -- the same classification would
			// recur -- so this returns its own actionable error instead of
			// falling through to the generic one, and the global-agent hint
			// wrapping at this function's Start() caller is never reached
			// for it (rootsErr returns immediately, before that wrapping
			// runs).
			return nil, fmt.Errorf("the global project directory %q is itself inside a git work tree; restart with an explicit --workspace", projectDir)
		}
		projectRepoRoot, err := util.RepoRootDir(projectDir)
		if err != nil || projectRepoRoot == "" || !workspaceSharesProjectRepo(projectDir, projectRepoRoot) {
			// util.RepoRootDir retries from the parent directory on
			// failure, so a bare repository nested inside another
			// repository's work tree does not error here: the walk-up
			// moves past the bare repo entirely and returns the ENCLOSING
			// repository's top level instead, which is not projectDir's
			// own repository at all. workspaceSharesProjectRepo's
			// common-git-dir comparison catches that case even when
			// RepoRootDir itself reports success.
			return nil, fmt.Errorf("project directory %q does not resolve to its own git work tree (a bare repository, for example); workspace source cannot be validated: restart with an explicit --workspace, or use a project inside a non-bare git work tree", projectDir)
		}
		roots := []string{projectRepoRoot}
		if effectiveWorkspace != "" {
			if ok, err := util.IsRegisteredWorktree(projectRepoRoot, effectiveWorkspace); err == nil && ok {
				if workspaceSharesProjectRepo(projectDir, effectiveWorkspace) {
					roots = append(roots, effectiveWorkspace)
				}
			}
		}
		return roots, nil
	}
	if config.IsGlobalProjectDir(projectDir) {
		return []string{filepath.Join(projectDir, "workspace")}, nil
	}
	return []string{resolveProjectRoot(settings, projectDir)}, nil
}

// workspaceSharesProjectRepo reports whether workspace's own common git
// directory matches projectDir's — an extra structural check alongside
// util.IsRegisteredWorktree, so worktree membership is confirmed two ways
// before it is trusted as a containment root.
func workspaceSharesProjectRepo(projectDir, workspace string) bool {
	projectCommonDir, err := util.GetCommonGitDir(projectDir)
	if err != nil {
		return false
	}
	workspaceCommonDir, err := util.GetCommonGitDir(workspace)
	if err != nil {
		return false
	}
	resolvedProjectCommonDir, err := filepath.EvalSymlinks(projectCommonDir)
	if err != nil {
		return false
	}
	resolvedWorkspaceCommonDir, err := filepath.EvalSymlinks(workspaceCommonDir)
	if err != nil {
		return false
	}
	return resolvedProjectCommonDir == resolvedWorkspaceCommonDir
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

// matchAgentProject returns true if the agent belongs to the given project.
// It checks the project_id label first (authoritative in hosted mode), then
// falls back to the project name label.
func matchAgentProject(a api.AgentInfo, projectName, projectID string) bool {
	// If we have a projectID, check the canonical project label first.
	if projectID != "" {
		if labelProjectID := projectkeys.ProjectIDFromLabels(a.Labels); labelProjectID != "" {
			return labelProjectID == projectID
		}
		if a.ProjectID != "" {
			return a.ProjectID == projectID
		}
	}
	// Fall back to project name matching
	if projectName != "" {
		if labelProject := projectkeys.ProjectNameFromLabels(a.Labels); labelProject != "" {
			return labelProject == projectName
		}
		if a.Project != "" {
			return a.Project == projectName
		}
	}
	// No project info on either side — match for backward compatibility
	return true
}

// containerName returns a project-scoped container name to prevent Docker name
// collisions when agents with the same name exist in different projects.
func containerName(projectName, agentName string) string {
	if projectName != "" {
		return projectName + "--" + agentName
	}
	return agentName
}

// buildAgentEnv merges the config layer (scionCfg.Env) with extraEnv
// (opts.Env, which carries the hub's resolved env for a broker-mode start)
// into the container environment. extraEnv wins on conflict.
//
// With brokerMode set, hub-only keys (see IsHubOnlyEnvKey) are skipped while
// iterating the config layer, matched on the post-expansion key, and each
// skipped value is returned in dropped. scionCfg itself is never modified.
// An empty hub-only key in extraEnv is omitted without being reported as
// missing: for those keys empty means "unset", never "required".
//
// defaultEnv is the lowest tier, below both layers (it carries the Hub's
// defaults, see api.HubAgentDefaults.DefaultEnv). Each entry is applied only
// when the key is absent from the merged env or has an empty value, i.e. when
// no layer would otherwise put it in the container.
func buildAgentEnv(scionCfg *api.ScionConfig, extraEnv map[string]string, defaultEnv map[string]string, brokerMode bool) (env []string, warnings []string, missingKeys []string, dropped []droppedBrokerEnv) {
	combined := make(map[string]string)

	if scionCfg != nil && scionCfg.Env != nil {
		for k, v := range scionCfg.Env {
			// Support variable substitution in keys and values
			expandedKey, _ := util.ExpandEnv(k)
			expandedValue, warned := util.ExpandEnv(v)

			if expandedKey == "" {
				continue
			}
			if skipBrokerLocalEnvKey(brokerMode, expandedKey) {
				// Checked before the host passthrough below, so an empty
				// marker never pulls in the broker host's value.
				dropped = append(dropped, droppedBrokerEnv{Key: expandedKey, Value: expandedValue, Layer: envLayerConfig})
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
	// Lowest tier: fill only what no layer above set.
	for k, v := range defaultEnv {
		if combined[k] == "" {
			combined[k] = v
		}
	}

	agentEnv := []string{}
	for k, v := range combined {
		if v == "" && skipBrokerLocalEnvKey(brokerMode, k) {
			continue
		}
		if v == "" {
			missingKeys = append(missingKeys, k)
			warnings = append(warnings, fmt.Sprintf("Warning: Environment variable '%s' has no value and will be omitted.", k))
			continue
		}
		agentEnv = append(agentEnv, fmt.Sprintf("%s=%s", k, v))
	}
	sortDroppedBrokerEnv(dropped)
	return agentEnv, warnings, missingKeys, dropped
}

// envLaunchID is the container env variable carrying the run ID, which
// sciontool presents to the hub's conduit endpoint as its launch id.
const envLaunchID = "SCION_LAUNCH_ID"

// withLaunchIDEnv returns env with every SCION_LAUNCH_ID entry replaced by
// a single one set to runID.
func withLaunchIDEnv(env []string, runID string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, envLaunchID+"=") {
			out = append(out, kv)
		}
	}
	return append(out, envLaunchID+"="+runID)
}

// resolveAuthEnvOverlay injects settings-declared env vars into opts.Env and
// returns the auth overlay that GatherAuthWithEnv consumes. It is the exact
// sequence Start runs at the point auth resolution begins, extracted so it can
// be exercised directly in tests.
//
// For a broker-mode start, hub-only keys (see IsHubOnlyEnvKey) in the
// harness-config entry are not merged; they are returned in dropped instead.
func resolveAuthEnvOverlay(opts *api.StartOptions, settings *config.VersionedSettings, profileName, harnessConfigName string) (overlay map[string]string, dropped []droppedBrokerEnv) {
	// Inject harness-config env into opts.Env BEFORE the auth overlay is built,
	// so GatherAuthWithEnv can see credentials the harness config declares
	// (GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION, ...).
	//
	// Runs in broker mode too. The gate that used to stand here excluded
	// hub-dispatched agents, which is every agent in a hub deployment — but only
	// from the AUTH OVERLAY: the same values already reach the container via
	// finalScionCfg.Env (provision.go:1098-1115, ungated). The gate therefore
	// bought no isolation, it only blinded auth resolution. See design §0.2.
	//
	// Profile env is fully retired — profiles.<p>.env was removed from both
	// the struct and the JSON schema (G3-full). This reads the harness config
	// only. See design §0.3.
	if settings != nil && harnessConfigName != "" {
		if hcEntry, err := settings.ResolveHarnessConfig(profileName, harnessConfigName); err == nil && len(hcEntry.Env) > 0 {
			if opts.Env == nil {
				opts.Env = make(map[string]string)
			}
			for k, v := range hcEntry.Env {
				if skipBrokerLocalEnvKey(opts.BrokerMode, k) {
					dropped = append(dropped, droppedBrokerEnv{Key: k, Value: v, Layer: envLayerHarnessConfigEntry})
					continue
				}
				if _, exists := opts.Env[k]; !exists { // never clobber hub-supplied values
					opts.Env[k] = v
				}
			}
		}
	}
	sortDroppedBrokerEnv(dropped)

	// Build a temporary auth overlay from resolved env-type secrets so auth
	// resolution can detect credentials without mutating opts.Env (which is
	// later projected into the container environment).
	return buildAuthEnvOverlay(opts.Env, opts.ResolvedSecrets), dropped
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

// isNoAuthSelection reports whether the effective auth selection for a
// start is the no-auth sentinel. A valid explicit harnessAuth decides on
// its own; otherwise the auth_selectedType persisted in scion-agent.json
// (cfg) is used. Harness implementation names are ignored as corrupted.
func isNoAuthSelection(harnessAuth string, cfg *api.ScionConfig) bool {
	if harnessAuth != "" && !harness.IsHarnessImplementationName(harnessAuth) {
		return harness.IsNoAuthType(harnessAuth)
	}
	return cfg != nil && harness.IsNoAuthType(cfg.AuthSelectedType)
}

// autoDetectAuthSelectedType sets auth.SelectedType when nothing explicit has
// selected one yet (CLI/template/profile overrides, scion-agent.json). It
// mirrors the precedence the hub preflight already uses
// (pkg/runtimebroker/handlers.go extractRequiredEnvKeys): file secrets, then
// env vars, then GCP identity.
//
// Without this, container-script harnesses (e.g. antigravity) never learn a
// resolved auth type for the auto-detect case: h.ResolveAuth forwards
// auth.SelectedType into SCION_HARNESS_SELECTED_AUTH / explicit_type
// (container_script_harness.go), which the container-side provisioner is
// meant to trust instead of re-guessing via its own, harness-local method
// ordering. Extracted (like resolveAuthEnvOverlay) so it can be exercised
// directly in tests. See ptone/scion#1873.
//
// gcpSAAssigned is derived from opts.Env["SCION_METADATA_MODE"] rather than a
// structured GCPIdentity: api.StartOptions carries no such field, and
// SCION_METADATA_MODE is the only channel this signal has across the
// runtimebroker -> agent package boundary (pkg/runtimebroker/start_context.go
// sets it for assign, block, and passthrough alike).
func autoDetectAuthSelectedType(auth *api.AuthConfig, authMeta *config.HarnessAuthMetadata, opts *api.StartOptions) {
	if auth.SelectedType != "" || authMeta == nil {
		return
	}
	// Skip in local/workstation mode: the GCP-identity signal
	// (SCION_METADATA_MODE) and opts.Env / opts.ResolvedSecrets only reflect
	// what the *broker* pre-resolved. GatherAuthWithEnv(authEnvOverlay,
	// localSources=true, authMeta) separately discovers host env vars via
	// os.Getenv and host credential files (e.g. ~/.claude/.credentials.json,
	// the local ADC file) that never pass through opts.Env/opts.ResolvedSecrets
	// at all — this function has no visibility into them. Deciding from opts
	// alone in local mode would bind to a narrower view of the world than
	// ResolveAuth/provision.py actually have, and could override a real host
	// credential this function simply cannot see. SCION_METADATA_MODE is also
	// broker-only by construction (pkg/runtimebroker/start_context.go), so
	// the identity leg has nothing to contribute locally anyway.
	if !opts.BrokerMode {
		return
	}

	fileSecretNames := make(map[string]struct{})
	for _, sec := range opts.ResolvedSecrets {
		if sec.Type == "file" {
			fileSecretNames[sec.Name] = struct{}{}
		}
	}

	envKeys := make(map[string]struct{})
	for k, v := range opts.Env {
		if v != "" {
			envKeys[k] = struct{}{}
		}
	}
	for _, sec := range opts.ResolvedSecrets {
		if sec.Type == "environment" || sec.Type == "" {
			target := sec.Target
			if target == "" {
				target = sec.Name
			}
			if target != "" {
				envKeys[target] = struct{}{}
			}
		}
	}

	// gcpSAAssigned mirrors the broker's own check (extractRequiredEnvKeys:
	// assign or passthrough both provide credentials).
	metadataMode := opts.Env["SCION_METADATA_MODE"]
	gcpSAAssigned := metadataMode == store.GCPMetadataModeAssign || metadataMode == store.GCPMetadataModePassthrough

	// AutoDetectAuthType runs the file -> env -> identity precedence as a
	// single call so a present default-type credential (e.g. antigravity's
	// AGY_TOKEN, claude's ANTHROPIC_API_KEY) always wins over the identity leg.
	if detected := harness.AutoDetectAuthType(authMeta, fileSecretNames, envKeys, gcpSAAssigned); detected != "" {
		auth.SelectedType = detected
	}
}

// filterResolvedSecretsForResolvedAuth drops auth-candidate secrets that are
// not required by the selected resolved auth method while preserving all
// non-auth secrets. configAuthKeys extends the hardcoded auth key set with
// keys from the harness config's auth metadata.
func filterResolvedSecretsForResolvedAuth(secrets []api.ResolvedSecret, resolved *api.ResolvedAuth, configAuthKeys map[string]struct{}) []api.ResolvedSecret {
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
		if !isAuthCandidateSecret(s, configAuthKeys) {
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

func isAuthCandidateSecret(s api.ResolvedSecret, configAuthKeys map[string]struct{}) bool {
	if (s.Type == "environment" || s.Type == "") && isAuthEnvKey(secretEnvTarget(s), configAuthKeys) {
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

func isAuthEnvKey(key string, extraAuthKeys ...map[string]struct{}) bool {
	// GCP shared fields are always auth-related regardless of harness config
	switch key {
	case "GOOGLE_CLOUD_PROJECT",
		"GCP_PROJECT",
		"ANTHROPIC_VERTEX_PROJECT_ID",
		"GOOGLE_CLOUD_REGION",
		"CLOUD_ML_REGION",
		"GOOGLE_CLOUD_LOCATION":
		return true
	}
	// All other auth env keys come from the config-driven key set
	for _, extra := range extraAuthKeys {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

// configAuthEnvKeySet builds a set of env var keys declared across all auth
// types in a harness config's auth metadata. Returns nil when no keys are
// declared. Used to extend isAuthEnvKey with config-driven keys.
func configAuthEnvKeySet(authMeta *config.HarnessAuthMetadata) map[string]struct{} {
	if authMeta == nil || len(authMeta.Types) == 0 {
		return nil
	}
	keys := make(map[string]struct{})
	for _, authType := range authMeta.Types {
		for _, req := range authType.RequiredEnv {
			for _, k := range req.AnyOf {
				keys[k] = struct{}{}
			}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
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
	case name == "CLAUDE_AUTH" || strings.HasSuffix(target, "/.claude/.credentials.json"):
		return "claude-auth"
	default:
		return ""
	}
}

// hasMetadataInterception checks if the agent env vars include metadata server
// configuration that requires iptables interception (assign or block mode).
//
// The comparison is exact. It used to be strings.HasPrefix, which disagreed with
// the broker's equality test on the same value: "SCION_METADATA_MODE=blocked"
// enabled interception here while the broker rejected it, so the two layers
// could reach opposite conclusions about the same env slice. Any suffix did it —
// "blockade", "assignment" — and prefix matching buys nothing, because these are
// whole "KEY=VALUE" entries with a closed set of legal values.
//
// Matching on any entry (rather than the last one) is deliberate and unchanged:
// with a contradictory duplicate the result is interception enabled, which is
// the restrictive direction.
func hasMetadataInterception(env []string) bool {
	for _, e := range env {
		if e == "SCION_METADATA_MODE=assign" || e == "SCION_METADATA_MODE=block" {
			return true
		}
	}
	return false
}

func mergeExtraHosts(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a))
	result := make([]string, 0, len(a)+len(b))
	for _, h := range a {
		host, _, _ := strings.Cut(h, ":")
		seen[host] = true
		result = append(result, h)
	}
	for _, h := range b {
		host, _, _ := strings.Cut(h, ":")
		if !seen[host] {
			result = append(result, h)
		}
	}
	return result
}

// reResolveModelAlias detects when SCION_MODEL contains an unresolved size
// alias (e.g. "large") that the hub failed to resolve, and returns a
// concrete model to use instead. It returns ("", false) when no
// re-resolution is needed — either because the value is not a known alias,
// or a concrete replacement could not be determined.
//
// It first tries cfg.Model, the broker's own resolved config (this is the
// pre-existing behavior for when the hub's alias resolution failed but the
// broker's local config resolved it). When cfg.Model is empty or is itself
// still the same unresolved alias — e.g. GetAgent had no local template
// chain to resolve against either — it falls back to the harness's built-in
// alias table (harnesses/<harnessName>/config.yaml) so a bare alias never
// reaches the harness verbatim.
func reResolveModelAlias(envModel string, cfg *api.ScionConfig, harnessName string) (string, bool) {
	if envModel == "" {
		return "", false
	}
	normalized := config.NormalizeModelAlias(envModel)
	if !config.KnownModelAliases[normalized] {
		return "", false
	}
	if cfg != nil && cfg.Model != "" && envModel != cfg.Model {
		return cfg.Model, true
	}
	if harnessName != "" {
		if aliases := harness.DefaultModelAliases(harnessName); len(aliases) > 0 {
			if resolved := config.ResolveModelAlias(envModel, aliases); resolved != envModel {
				return resolved, true
			}
		}
	}
	return "", false
}

// isEmptyPerAgentStart reports whether opts start an empty-per-agent agent
// (design #2703): set by the broker from the hub's WorkspaceMode, or carried
// as SCION_WORKSPACE_MODE in the agent env.
func isEmptyPerAgentStart(opts api.StartOptions) bool {
	return opts.EmptyPerAgentWorkspace ||
		store.ResolveWorkspaceSharingMode(opts.Env["SCION_WORKSPACE_MODE"]) == store.SharingModeEmptyPerAgent
}

// infoTemplateHydrated reports whether cfg's agent info records a template
// loaded from a content-addressed cache (see api.AgentInfo.TemplateHash).
func infoTemplateHydrated(cfg *api.ScionConfig) bool {
	return cfg != nil && cfg.Info != nil && cfg.Info.TemplateHash != ""
}

// agentTemplateDisplayName returns the template name used for the
// scion.template label and SCION_TEMPLATE_NAME: the dispatch's template slug
// when set, else the name recorded in agent info. A content hash is never
// returned; such a value yields "".
func agentTemplateDisplayName(slug string, cfg *api.ScionConfig) string {
	if slug != "" && !transfer.IsContentHash(slug) {
		return slug
	}
	if cfg != nil && cfg.Info != nil && !transfer.IsContentHash(cfg.Info.Template) {
		return cfg.Info.Template
	}
	return ""
}
