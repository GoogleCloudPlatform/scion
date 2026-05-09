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
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Annotation keys for project settings stored in project annotations.
const (
	projectSettingDefaultTemplate      = "scion.io/default-template"
	projectSettingDefaultHarnessConfig = "scion.io/default-harness-config"
	projectSettingTelemetryEnabled     = "scion.io/telemetry-enabled"
	projectSettingActiveProfile        = "scion.io/active-profile"

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
	settings.ActiveProfile = project.Annotations[projectSettingActiveProfile]

	if val, ok := project.Annotations[projectSettingTelemetryEnabled]; ok {
		if b, err := strconv.ParseBool(val); err == nil {
			settings.TelemetryEnabled = &b
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
	setOrDelete(project.Annotations, projectSettingActiveProfile, settings.ActiveProfile)

	if settings.TelemetryEnabled != nil {
		project.Annotations[projectSettingTelemetryEnabled] = strconv.FormatBool(*settings.TelemetryEnabled)
	} else {
		delete(project.Annotations, projectSettingTelemetryEnabled)
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

	// Apply resource defaults
	if hasResources {
		projectRes := projectResourceSpecToAPI(settings.DefaultResources)
		if projectRes != nil {
			if ac.InlineConfig.Resources == nil {
				ac.InlineConfig.Resources = projectRes
			}
			// If inline already has resources, don't override — agent/template level wins
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
