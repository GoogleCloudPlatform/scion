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

// Annotation keys for grove settings stored in grove annotations.
const (
	groveSettingDefaultTemplate      = "scion.io/default-template"
	groveSettingDefaultHarnessConfig = "scion.io/default-harness-config"
	groveSettingTelemetryEnabled     = "scion.io/telemetry-enabled"
	groveSettingActiveProfile        = "scion.io/active-profile"

	// Default agent limits
	groveSettingDefaultMaxTurns      = "scion.io/default-max-turns"
	groveSettingDefaultMaxModelCalls = "scion.io/default-max-model-calls"
	groveSettingDefaultMaxDuration   = "scion.io/default-max-duration"

	// Default GCP identity
	groveSettingDefaultGCPIdentityMode = "scion.io/default-gcp-identity-mode"
	groveSettingDefaultGCPIdentitySAID = "scion.io/default-gcp-identity-service-account-id"

	// Default resource spec (flat keys)
	groveSettingDefaultResourcesCPUReq = "scion.io/default-resources-cpu-request"
	groveSettingDefaultResourcesMemReq = "scion.io/default-resources-memory-request"
	groveSettingDefaultResourcesCPULim = "scion.io/default-resources-cpu-limit"
	groveSettingDefaultResourcesMemLim = "scion.io/default-resources-memory-limit"
	groveSettingDefaultResourcesDisk   = "scion.io/default-resources-disk"
)

// handleProjectSettings handles GET/PUT on /api/v1/groves/{groveId}/settings.
func (s *Server) handleProjectSettings(w http.ResponseWriter, r *http.Request, groveID string) {
	ctx := r.Context()

	grove, err := s.store.GetProject(ctx, groveID)
	if err != nil {
		if err == store.ErrNotFound {
			NotFound(w, "Grove")
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
				Type:    "grove",
				ID:      grove.ID,
				OwnerID: grove.OwnerID,
			}, ActionRead)
			if !decision.Allowed {
				Forbidden(w)
				return
			}
		}

		writeJSON(w, http.StatusOK, projectSettingsFromAnnotations(grove))

	case http.MethodPut:
		if userIdent, ok := identity.(UserIdentity); ok {
			decision := s.authzService.CheckAccess(ctx, userIdent, Resource{
				Type:    "grove",
				ID:      grove.ID,
				OwnerID: grove.OwnerID,
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

		applyProjectSettingsToAnnotations(grove, &req)

		if err := s.store.UpdateProject(ctx, grove); err != nil {
			writeErrorFromErr(w, err, "")
			return
		}

		s.events.PublishProjectUpdated(ctx, grove)
		writeJSON(w, http.StatusOK, projectSettingsFromAnnotations(grove))

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

	settings.DefaultTemplate = project.Annotations[groveSettingDefaultTemplate]
	settings.DefaultHarnessConfig = project.Annotations[groveSettingDefaultHarnessConfig]
	settings.ActiveProfile = project.Annotations[groveSettingActiveProfile]

	if val, ok := project.Annotations[groveSettingTelemetryEnabled]; ok {
		if b, err := strconv.ParseBool(val); err == nil {
			settings.TelemetryEnabled = &b
		}
	}

	// Default agent limits
	if val, ok := project.Annotations[groveSettingDefaultMaxTurns]; ok {
		if n, err := strconv.Atoi(val); err == nil {
			settings.DefaultMaxTurns = n
		}
	}
	if val, ok := project.Annotations[groveSettingDefaultMaxModelCalls]; ok {
		if n, err := strconv.Atoi(val); err == nil {
			settings.DefaultMaxModelCalls = n
		}
	}
	settings.DefaultMaxDuration = project.Annotations[groveSettingDefaultMaxDuration]

	// Default GCP identity
	settings.DefaultGCPIdentityMode = project.Annotations[groveSettingDefaultGCPIdentityMode]
	settings.DefaultGCPIdentityServiceAccountID = project.Annotations[groveSettingDefaultGCPIdentitySAID]

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
	cpuReq := annotations[groveSettingDefaultResourcesCPUReq]
	memReq := annotations[groveSettingDefaultResourcesMemReq]
	cpuLim := annotations[groveSettingDefaultResourcesCPULim]
	memLim := annotations[groveSettingDefaultResourcesMemLim]
	disk := annotations[groveSettingDefaultResourcesDisk]

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

// applyProjectSettingsToAnnotations writes grove settings into the grove's annotations map.
func applyProjectSettingsToAnnotations(grove *store.Project, settings *hubclient.ProjectSettings) {
	if grove.Annotations == nil {
		grove.Annotations = make(map[string]string)
	}

	setOrDelete(grove.Annotations, groveSettingDefaultTemplate, settings.DefaultTemplate)
	setOrDelete(grove.Annotations, groveSettingDefaultHarnessConfig, settings.DefaultHarnessConfig)
	setOrDelete(grove.Annotations, groveSettingActiveProfile, settings.ActiveProfile)

	if settings.TelemetryEnabled != nil {
		grove.Annotations[groveSettingTelemetryEnabled] = strconv.FormatBool(*settings.TelemetryEnabled)
	} else {
		delete(grove.Annotations, groveSettingTelemetryEnabled)
	}

	// Default GCP identity
	setOrDelete(grove.Annotations, groveSettingDefaultGCPIdentityMode, settings.DefaultGCPIdentityMode)
	setOrDelete(grove.Annotations, groveSettingDefaultGCPIdentitySAID, settings.DefaultGCPIdentityServiceAccountID)

	// Default agent limits
	setOrDeleteInt(grove.Annotations, groveSettingDefaultMaxTurns, settings.DefaultMaxTurns)
	setOrDeleteInt(grove.Annotations, groveSettingDefaultMaxModelCalls, settings.DefaultMaxModelCalls)
	setOrDelete(grove.Annotations, groveSettingDefaultMaxDuration, settings.DefaultMaxDuration)

	// Default resources (flat keys)
	if settings.DefaultResources != nil {
		res := settings.DefaultResources
		if res.Requests != nil {
			setOrDelete(grove.Annotations, groveSettingDefaultResourcesCPUReq, res.Requests.CPU)
			setOrDelete(grove.Annotations, groveSettingDefaultResourcesMemReq, res.Requests.Memory)
		} else {
			delete(grove.Annotations, groveSettingDefaultResourcesCPUReq)
			delete(grove.Annotations, groveSettingDefaultResourcesMemReq)
		}
		if res.Limits != nil {
			setOrDelete(grove.Annotations, groveSettingDefaultResourcesCPULim, res.Limits.CPU)
			setOrDelete(grove.Annotations, groveSettingDefaultResourcesMemLim, res.Limits.Memory)
		} else {
			delete(grove.Annotations, groveSettingDefaultResourcesCPULim)
			delete(grove.Annotations, groveSettingDefaultResourcesMemLim)
		}
		setOrDelete(grove.Annotations, groveSettingDefaultResourcesDisk, res.Disk)
	} else {
		delete(grove.Annotations, groveSettingDefaultResourcesCPUReq)
		delete(grove.Annotations, groveSettingDefaultResourcesMemReq)
		delete(grove.Annotations, groveSettingDefaultResourcesCPULim)
		delete(grove.Annotations, groveSettingDefaultResourcesMemLim)
		delete(grove.Annotations, groveSettingDefaultResourcesDisk)
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

// applyProjectDefaults applies grove-level defaults from annotations to the agent's
// AppliedConfig and InlineConfig. Only fills in values that are not already set
// (0 or empty), so explicit agent/template-level values are preserved.
func applyProjectDefaults(ac *store.AgentAppliedConfig, grove *store.Project) {
	if ac == nil || grove == nil || grove.Annotations == nil {
		return
	}

	settings := projectSettingsFromAnnotations(grove)

	// Apply default harness config (only if not already set)
	if ac.HarnessConfig == "" && settings.DefaultHarnessConfig != "" {
		ac.HarnessConfig = settings.DefaultHarnessConfig
	}

	// Check if there are any grove limit/resource defaults to apply
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
		groveRes := projectResourceSpecToAPI(settings.DefaultResources)
		if groveRes != nil {
			if ac.InlineConfig.Resources == nil {
				ac.InlineConfig.Resources = groveRes
			}
			// If inline already has resources, don't override — agent/template level wins
		}
	}
}

// groveResourceSpecToAPI converts a ProjectResourceSpec to an api.ResourceSpec.
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
