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

package hub

import (
	"net/http"
	"strconv"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Annotation keys for project settings stored in project annotations.
//
// When adding a key here, add it to projectSettingKeys below as well.
// TestProjectSettingKeys_NoDrift enforces this.
const (
	projectSettingDefaultTemplate        = "scion.io/default-template"
	projectSettingDefaultHarnessConfig   = "scion.io/default-harness-config"
	projectSettingDefaultModel           = "scion.io/default-model"
	projectSettingDefaultThinkingLevel   = "scion.io/default-thinking-level"
	projectSettingTelemetryEnabled       = "scion.io/telemetry-enabled"
	projectSettingAutoExposePortsEnabled = "scion.io/auto-expose-ports-enabled"
	projectSettingActiveProfile          = "scion.io/active-profile"

	// Default agent limits
	projectSettingDefaultMaxTurns      = "scion.io/default-max-turns"
	projectSettingDefaultMaxModelCalls = "scion.io/default-max-model-calls"
	projectSettingDefaultMaxDuration   = "scion.io/default-max-duration"

	// Default GCP identity
	projectSettingDefaultGCPIdentityMode = "scion.io/default-gcp-identity-mode"
	projectSettingDefaultGCPIdentitySAID = "scion.io/default-gcp-identity-service-account-id"

	// Default resource spec (flat keys)
	projectSettingDefaultResourcesCPUReq = "scion.io/default-resources-cpu-request"
	projectSettingDefaultResourcesMemReq = "scion.io/default-resources-memory-request"
	projectSettingDefaultResourcesCPULim = "scion.io/default-resources-cpu-limit"
	projectSettingDefaultResourcesMemLim = "scion.io/default-resources-memory-limit"
	projectSettingDefaultResourcesDisk   = "scion.io/default-resources-disk"
)

// projectSettingKeys is the authoritative list of scion.io/* annotation keys
// that constitute project settings. Anything not in this list is not a project
// setting: it will not be copied by clone, nor reported by the resolved
// settings endpoint.
//
// No production code reads this list yet. The intended consumers are the
// project clone endpoint and the resolved-settings endpoint, which land in
// later phases of this workstream; the list is introduced ahead of them so that
// "copy the project settings" has one precise definition rather than three
// approximate ones. Until those land, the registry's working value is
// TestProjectSettingKeys_NoDrift below, which fails the build when a new
// projectSetting* constant is not registered. Registry and guard are a single
// executable invariant and should stay together — the list without the test is
// merely an unused variable, and is reported as one by the linter.
//
// This is the single source of truth for "what is a project setting". A key
// omitted here would be silently dropped when a project is cloned; a key
// wrongly added here would be exposed in API responses and propagated into
// clones. Errors in both directions are user-visible bugs, so treat edits to
// this list as a change to the project-settings contract rather than as a list
// edit.
//
// Two properties are maintained deliberately and are enforced by
// TestProjectSettingKeys_NoDrift:
//
//  1. Every projectSetting* constant declared above appears here exactly once.
//     A new setting that is not registered fails the build's tests rather than
//     going unnoticed until someone loses it on clone.
//  2. The order matches the constant declaration order above, which in turn
//     matches the table in .design/project-templates.md §3.1, so all three can
//     be diffed by eye.
//
// Note the scope: these are keys in project.Annotations. project.Labels is a
// separate map that also carries scion.io/* keys — scion.io/system and
// scion.io/global, set on the Global project at cmd/server_broker.go. Those are
// system markers rather than project settings, so they do not belong here.
// (Other scion.io/* keys such as scion.io/plugin, scion.io/broker-type and
// scion.io/broker-role are RuntimeBroker labels and never appear on a project
// at all.)
//
// Phase 4 (clone) label policy, recorded here because this comment is the
// nearest thing to a spec for it: clone copies scion.dev/* labels and drops the
// scion.io/* prefix entirely — a prefix rule rather than a two-key denylist, so
// that a future system marker is not silently propagated into clones.
//
// One scion.dev/ label is excluded: store.LabelWorkspaceMode
// ("scion.dev/workspace-mode") is NOT copied. It is derived for the new project
// from the clone request and the new project's git remote, by the same
// validation the create path applies (handlers_projects_core.go), which sets it
// only when there is a git remote and the mode is one of the two valid values.
// Copying it raw would bypass that check and let a clone carry a workspace mode
// inconsistent with its own remote, which IsSharedWorkspace() and
// IsWorktreePerAgent() would then evaluate against mismatched state.
//
// Finally, do not try to "complete" this list from hubclient.ProjectSettings.
// That struct also carries Bucket, Runtimes, Harnesses and Profiles, which the
// settings endpoint accepts on PUT, silently ignores, and never returns on GET.
// They are not annotation-backed, so their absence here is correct and loses
// nothing on clone.
var projectSettingKeys = []string{
	projectSettingDefaultTemplate,
	projectSettingDefaultHarnessConfig,
	projectSettingDefaultModel,
	projectSettingDefaultThinkingLevel,
	projectSettingTelemetryEnabled,
	projectSettingAutoExposePortsEnabled,
	projectSettingActiveProfile,

	// Default agent limits
	projectSettingDefaultMaxTurns,
	projectSettingDefaultMaxModelCalls,
	projectSettingDefaultMaxDuration,

	// Default GCP identity
	projectSettingDefaultGCPIdentityMode,
	projectSettingDefaultGCPIdentitySAID,

	// Default resource spec (flat keys)
	projectSettingDefaultResourcesCPUReq,
	projectSettingDefaultResourcesMemReq,
	projectSettingDefaultResourcesCPULim,
	projectSettingDefaultResourcesMemLim,
	projectSettingDefaultResourcesDisk,
}

// handleProjectSettings handles GET/PUT on /api/v1/projects/{projectId}/settings.
func (s *Server) handleProjectSettings(w http.ResponseWriter, r *http.Request, projectID string) {
	ctx := r.Context()

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Project")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "project",
				ID:      project.ID,
				OwnerID: project.OwnerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		}

		writeJSON(w, http.StatusOK, projectSettingsFromAnnotations(project))

	case http.MethodPut:
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "project",
				ID:      project.ID,
				OwnerID: project.OwnerID,
			}, ActionUpdate)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		} else {
			Forbidden(w)
			return
		}

		var req hubclient.ProjectSettings
		if err := readJSON(r, &req); err != nil {
			BadRequest(w, "Invalid request body: "+err.Error())
			return
		}

		if req.DefaultThinkingLevel != nil {
			if tl := *req.DefaultThinkingLevel; tl < 0 || tl > 100 {
				BadRequest(w, "thinking_level must be between 0 and 100")
				return
			}
		}

		applyProjectSettingsToAnnotations(project, &req)

		if err := s.store.UpdateProject(ctx, project); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		s.events.PublishProjectUpdated(ctx, project)
		writeJSON(w, http.StatusOK, projectSettingsFromAnnotations(project))

	default:
		MethodNotAllowed(w)
	}
}

// projectSettingsFromAnnotations reads project settings from the project's annotations map.
func projectSettingsFromAnnotations(project *store.Project) *hubclient.ProjectSettings {
	settings := &hubclient.ProjectSettings{}
	if project.Annotations == nil {
		return settings
	}

	settings.DefaultTemplate = project.Annotations[projectSettingDefaultTemplate]
	settings.DefaultHarnessConfig = project.Annotations[projectSettingDefaultHarnessConfig]
	settings.DefaultModel = project.Annotations[projectSettingDefaultModel]
	if val, ok := project.Annotations[projectSettingDefaultThinkingLevel]; ok {
		if n, err := strconv.Atoi(val); err == nil {
			settings.DefaultThinkingLevel = &n
		}
	}
	settings.ActiveProfile = project.Annotations[projectSettingActiveProfile]

	if val, ok := project.Annotations[projectSettingTelemetryEnabled]; ok {
		if b, err := strconv.ParseBool(val); err == nil {
			settings.TelemetryEnabled = &b
		}
	}

	if val, ok := project.Annotations[projectSettingAutoExposePortsEnabled]; ok {
		if b, err := strconv.ParseBool(val); err == nil {
			settings.AutoExposePortsEnabled = &b
		}
	}

	// Default agent limits
	if val, ok := project.Annotations[projectSettingDefaultMaxTurns]; ok {
		if n, err := strconv.Atoi(val); err == nil {
			settings.DefaultMaxTurns = n
		}
	}
	if val, ok := project.Annotations[projectSettingDefaultMaxModelCalls]; ok {
		if n, err := strconv.Atoi(val); err == nil {
			settings.DefaultMaxModelCalls = n
		}
	}
	settings.DefaultMaxDuration = project.Annotations[projectSettingDefaultMaxDuration]

	// Default GCP identity
	settings.DefaultGCPIdentityMode = project.Annotations[projectSettingDefaultGCPIdentityMode]
	settings.DefaultGCPIdentityServiceAccountID = project.Annotations[projectSettingDefaultGCPIdentitySAID]

	// Default resources (flat annotation keys)
	res := projectResourcesFromAnnotations(project.Annotations)
	if res != nil {
		settings.DefaultResources = res
	}

	return settings
}

// projectResourcesFromAnnotations reads the flat resource annotation keys into a ProjectResourceSpec.
// Returns nil if no resource annotations are set.
func projectResourcesFromAnnotations(annotations map[string]string) *hubclient.ProjectResourceSpec {
	cpuReq := annotations[projectSettingDefaultResourcesCPUReq]
	memReq := annotations[projectSettingDefaultResourcesMemReq]
	cpuLim := annotations[projectSettingDefaultResourcesCPULim]
	memLim := annotations[projectSettingDefaultResourcesMemLim]
	disk := annotations[projectSettingDefaultResourcesDisk]

	if cpuReq == "" && memReq == "" && cpuLim == "" && memLim == "" && disk == "" {
		return nil
	}

	res := &hubclient.ProjectResourceSpec{Disk: disk}
	if cpuReq != "" || memReq != "" {
		res.Requests = &hubclient.ProjectResourceList{CPU: cpuReq, Memory: memReq}
	}
	if cpuLim != "" || memLim != "" {
		res.Limits = &hubclient.ProjectResourceList{CPU: cpuLim, Memory: memLim}
	}
	return res
}

// applyProjectSettingsToAnnotations writes project settings into the project's annotations map.
func applyProjectSettingsToAnnotations(project *store.Project, settings *hubclient.ProjectSettings) {
	if project.Annotations == nil {
		project.Annotations = make(map[string]string)
	}

	setOrDelete(project.Annotations, projectSettingDefaultTemplate, settings.DefaultTemplate)
	setOrDelete(project.Annotations, projectSettingDefaultHarnessConfig, settings.DefaultHarnessConfig)
	setOrDelete(project.Annotations, projectSettingDefaultModel, settings.DefaultModel)
	if settings.DefaultThinkingLevel != nil {
		project.Annotations[projectSettingDefaultThinkingLevel] = strconv.Itoa(*settings.DefaultThinkingLevel)
	} else {
		delete(project.Annotations, projectSettingDefaultThinkingLevel)
	}
	setOrDelete(project.Annotations, projectSettingActiveProfile, settings.ActiveProfile)

	if settings.TelemetryEnabled != nil {
		project.Annotations[projectSettingTelemetryEnabled] = strconv.FormatBool(*settings.TelemetryEnabled)
	} else {
		delete(project.Annotations, projectSettingTelemetryEnabled)
	}

	if settings.AutoExposePortsEnabled != nil {
		project.Annotations[projectSettingAutoExposePortsEnabled] = strconv.FormatBool(*settings.AutoExposePortsEnabled)
	} else {
		delete(project.Annotations, projectSettingAutoExposePortsEnabled)
	}

	// Default GCP identity
	setOrDelete(project.Annotations, projectSettingDefaultGCPIdentityMode, settings.DefaultGCPIdentityMode)
	setOrDelete(project.Annotations, projectSettingDefaultGCPIdentitySAID, settings.DefaultGCPIdentityServiceAccountID)

	// Default agent limits
	setOrDeleteInt(project.Annotations, projectSettingDefaultMaxTurns, settings.DefaultMaxTurns)
	setOrDeleteInt(project.Annotations, projectSettingDefaultMaxModelCalls, settings.DefaultMaxModelCalls)
	setOrDelete(project.Annotations, projectSettingDefaultMaxDuration, settings.DefaultMaxDuration)

	// Default resources (flat keys)
	if settings.DefaultResources != nil {
		res := settings.DefaultResources
		if res.Requests != nil {
			setOrDelete(project.Annotations, projectSettingDefaultResourcesCPUReq, res.Requests.CPU)
			setOrDelete(project.Annotations, projectSettingDefaultResourcesMemReq, res.Requests.Memory)
		} else {
			delete(project.Annotations, projectSettingDefaultResourcesCPUReq)
			delete(project.Annotations, projectSettingDefaultResourcesMemReq)
		}
		if res.Limits != nil {
			setOrDelete(project.Annotations, projectSettingDefaultResourcesCPULim, res.Limits.CPU)
			setOrDelete(project.Annotations, projectSettingDefaultResourcesMemLim, res.Limits.Memory)
		} else {
			delete(project.Annotations, projectSettingDefaultResourcesCPULim)
			delete(project.Annotations, projectSettingDefaultResourcesMemLim)
		}
		setOrDelete(project.Annotations, projectSettingDefaultResourcesDisk, res.Disk)
	} else {
		delete(project.Annotations, projectSettingDefaultResourcesCPUReq)
		delete(project.Annotations, projectSettingDefaultResourcesMemReq)
		delete(project.Annotations, projectSettingDefaultResourcesCPULim)
		delete(project.Annotations, projectSettingDefaultResourcesMemLim)
		delete(project.Annotations, projectSettingDefaultResourcesDisk)
	}
}

// setOrDeleteInt sets an annotation to the string representation of n, or deletes it if n is 0.
func setOrDeleteInt(m map[string]string, key string, n int) {
	if n > 0 {
		m[key] = strconv.Itoa(n)
	} else {
		delete(m, key)
	}
}

// setOrDelete sets an annotation key to value, or deletes it if value is empty.
func setOrDelete(m map[string]string, key, value string) {
	if value == "" {
		delete(m, key)
	} else {
		m[key] = value
	}
}

// applyProjectDefaults applies project-level defaults from annotations to the agent's
// AppliedConfig and InlineConfig. Only fills in values that are not already set
// (0 or empty), so explicit agent/template-level values are preserved.
func applyProjectDefaults(ac *store.AgentAppliedConfig, project *store.Project) {
	if ac == nil || project == nil || project.Annotations == nil {
		return
	}

	settings := projectSettingsFromAnnotations(project)

	// Apply default harness config (only if not already set)
	if ac.HarnessConfig == "" && settings.DefaultHarnessConfig != "" {
		ac.HarnessConfig = settings.DefaultHarnessConfig
	}

	// Apply default model (only if not already set by agent/template/CLI)
	if ac.Model == "" && settings.DefaultModel != "" {
		ac.Model = settings.DefaultModel
	}

	// Apply default thinking level (only if not already set)
	if ac.ThinkingLevel == nil && settings.DefaultThinkingLevel != nil {
		ac.ThinkingLevel = settings.DefaultThinkingLevel
	}

	// Apply the project's active profile (only if not already set by
	// agent/CLI). The guard is load-bearing: the request tier already works —
	// handlers_agent_create_helpers.go stamps AppliedConfig.Profile from
	// req.Profile — so an unconditional write here would clobber an explicit
	// user choice. Only the project tier was missing: scion.io/active-profile
	// was parsed and persisted but never applied to any agent.
	//
	// Second precedence edge, less obvious than request-over-project: this also
	// places the project above the broker's own active profile. The broker fell
	// back to its local settings.ActiveProfile when the hub sent nothing — in
	// extractRequiredEnvKeys, and again inside ResolveRuntime, which treats an
	// empty profile as "use vs.ActiveProfile". A project value now pre-empts
	// both, so this affects harness-config resolution and env/secret extraction
	// as well as runtime selection. Intended — hub configuration should outrank
	// broker-local defaults for a hub-created agent — and it degrades
	// gracefully: resolveManagerForOpts returns the default manager when
	// ResolveRuntime errors, so a stale annotation cannot fail dispatch.
	if ac.Profile == "" && settings.ActiveProfile != "" {
		ac.Profile = settings.ActiveProfile
	}

	// Check if there are any project limit/resource defaults to apply
	hasLimits := settings.DefaultMaxTurns > 0 || settings.DefaultMaxModelCalls > 0 || settings.DefaultMaxDuration != ""
	hasResources := settings.DefaultResources != nil
	if !hasLimits && !hasResources {
		return
	}

	// Ensure InlineConfig exists
	if ac.InlineConfig == nil {
		ac.InlineConfig = &api.ScionConfig{}
	}

	// Apply limit defaults (only if not already set)
	if ac.InlineConfig.MaxTurns == 0 && settings.DefaultMaxTurns > 0 {
		ac.InlineConfig.MaxTurns = settings.DefaultMaxTurns
	}
	if ac.InlineConfig.MaxModelCalls == 0 && settings.DefaultMaxModelCalls > 0 {
		ac.InlineConfig.MaxModelCalls = settings.DefaultMaxModelCalls
	}
	if ac.InlineConfig.MaxDuration == "" && settings.DefaultMaxDuration != "" {
		ac.InlineConfig.MaxDuration = settings.DefaultMaxDuration
	}

	// Apply resource defaults, field by field.
	//
	// This used to be all-or-nothing: the project's whole ResourceSpec was
	// installed if InlineConfig.Resources was nil and discarded entirely
	// otherwise. That made a template setting a single field — say a memory
	// limit — silently drop every unrelated project default, including the
	// disk size and both CPU values. It was also inconsistent with the three
	// limits stamped immediately above (MaxTurns, MaxModelCalls, MaxDuration),
	// which have always merged per field.
	//
	// MergeResourceSpec(base, override) is the canonical per-field merge and is
	// what the neighbouring template path already uses
	// (pkg/config/settings.go:234, called from pkg/config/templates.go:744).
	// The project is the base and the existing agent/template value is the
	// override, so a field set at agent/template level still wins and only
	// unset fields fall through to the project — the same precedence as
	// before, applied at field granularity instead of struct granularity.
	//
	// That equivalence holds *within this function*. Downstream it does shift
	// something: what changes is the set of fields left empty for lower tiers
	// to fill. A project cpu-limit that used to be discarded here arrived at
	// the broker empty and was supplied by the profile tier, the broker's local
	// settings.DefaultResources, or finally BuiltinDefaultResources
	// (pkg/agent/provision.go). It now arrives populated, so those tiers no
	// longer fire for that field. That is the intended direction — an explicit
	// project setting should outrank a broker built-in — but it means the
	// effective value can change for a deployment that was relying on a broker
	// default to fill a gap this function was creating.
	if hasResources {
		if projectRes := projectResourceSpecToAPI(settings.DefaultResources); projectRes != nil {
			ac.InlineConfig.Resources = config.MergeResourceSpec(projectRes, ac.InlineConfig.Resources)
		}
	}
}

// projectResourceSpecToAPI converts a ProjectResourceSpec to an api.ResourceSpec.
func projectResourceSpecToAPI(grs *hubclient.ProjectResourceSpec) *api.ResourceSpec {
	if grs == nil {
		return nil
	}
	res := &api.ResourceSpec{Disk: grs.Disk}
	if grs.Requests != nil {
		res.Requests = api.ResourceList{CPU: grs.Requests.CPU, Memory: grs.Requests.Memory}
	}
	if grs.Limits != nil {
		res.Limits = api.ResourceList{CPU: grs.Limits.CPU, Memory: grs.Limits.Memory}
	}
	return res
}
