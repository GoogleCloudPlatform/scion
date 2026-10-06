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
	"context"
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
	projectSettingDefaultHarnessAuth     = "scion.io/default-harness-auth"
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

	// Agent authorization
	projectSettingMaxAgentRole     = "scion.io/max-agent-role"
	projectSettingDefaultAgentRole = "scion.io/default-agent-role"
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
// ("scion.dev/workspace-mode") is NOT copied raw. deriveCloneWorkspaceMode
// (project_workspace_mode.go) re-derives it from the source's label: the mode
// is kept only when store.ValidateWorkspaceMode accepts it for the clone's
// git-ness (after any gitRemote override) and dropped otherwise, so a non-git
// per-agent template stays empty-per-agent and becomes clone-per-agent with a
// git remote, while e.g. worktree-per-agent on a non-git clone is dropped. A
// legacy raw "empty-per-agent" label is normalised to "per-agent" only on a
// non-git source. Copying it raw would let a clone carry a workspace mode
// inconsistent with its own remote, which SharingMode() and its callers would
// then evaluate against mismatched state.
//
// Finally, do not try to "complete" this list from hubclient.ProjectSettings.
// That struct also carries Bucket, Runtimes, Harnesses and Profiles, which the
// settings endpoint accepts on PUT, silently ignores, and never returns on GET.
// They are not annotation-backed, so their absence here is correct and loses
// nothing on clone.
var projectSettingKeys = []string{
	projectSettingDefaultTemplate,
	projectSettingDefaultHarnessConfig,
	projectSettingDefaultHarnessAuth,
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

	// Agent authorization
	projectSettingMaxAgentRole,
	projectSettingDefaultAgentRole,
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
		// Project isolation runs before the authorization check so a cross-project
		// agent caller keeps its 404 and is not told the project exists.
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			if project.ID != agentIdent.ProjectID() {
				NotFound(w, "Project")
				return
			}
		}
		if !s.authorize(w, r, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionRead) {
			return
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

		if req.MaxAgentRole != "" && !ValidAgentRole(AgentRole(req.MaxAgentRole)) {
			BadRequest(w, "maxAgentRole must be one of none, readonly, baseline, full")
			return
		}

		if req.DefaultAgentRole != "" && !ValidAgentRole(AgentRole(req.DefaultAgentRole)) {
			BadRequest(w, "defaultAgentRole must be one of none, readonly, baseline, full")
			return
		}

		if !s.validateDefaultGCPIdentity(w, ctx, project, &req) {
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
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// validateDefaultGCPIdentity rejects a default GCP identity that agent creation
// would later refuse to apply. It writes the error response itself and returns
// false when the caller must stop.
//
// Without this, the PUT stored the service account ID unvalidated and returned
// 200, while createAgentInProject silently fell back to metadataMode=block for
// every agent created afterwards. The operator saw a saved setting that did
// nothing, with no error at any layer. The three checks below are exactly the
// three conditions that path tests, so a 200 here means the setting will apply.
//
// Verified is checked at write time and can go stale afterwards: the service
// account may later be deleted or un-verified, which puts a validly-saved
// default back into the silent-block path. That residue is deliberately not
// handled here — it is tracked separately against the consumption site, because
// no amount of write-time validation can subsume it.
func (s *Server) validateDefaultGCPIdentity(w http.ResponseWriter, ctx context.Context, project *store.Project, req *hubclient.ProjectSettings) bool {
	// block is not offered as a NEW selection for a Kubernetes-bound project:
	// the broker rejects it at dispatch anyway, so saving it here would
	// reproduce the same silent-defect shape the assign/no-SA check below
	// exists to prevent. This only fires for a project whose linked brokers
	// are reliably known to be Kubernetes-only (see projectIsKubernetesBound)
	// — a project with no linked broker, or with a mix of runtime types, is
	// left alone.
	//
	// It must not fire on a no-op re-save of an already-stored "block": PUT is
	// a full replace and the settings page resends the current value on every
	// save (project-settings.ts), so treating every "block" in the body as new
	// would turn this into a forced migration of stored values — stored block
	// defaults are not migrated or rewritten. Comparing against the stored
	// annotation is what makes this a check on the transition, not on the
	// value.
	if req.DefaultGCPIdentityMode == store.GCPMetadataModeBlock &&
		(project.Annotations == nil || project.Annotations[projectSettingDefaultGCPIdentityMode] != store.GCPMetadataModeBlock) {
		k8sBound, err := s.projectIsKubernetesBound(ctx, project.ID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return false
		}
		if k8sBound {
			BadRequest(w, "default GCP identity mode 'block' is not available for a Kubernetes-bound project; choose 'passthrough' or 'assign' instead")
			return false
		}
	}

	// mode=assign with no service account is the same defect wearing different
	// clothes: the consumption path falls straight through to block.
	if req.DefaultGCPIdentityMode == store.GCPMetadataModeAssign && req.DefaultGCPIdentityServiceAccountID == "" {
		BadRequest(w, "default GCP identity mode 'assign' requires a service account; set defaultGCPIdentityServiceAccountID or choose another mode")
		return false
	}

	// Empty means clear. Clearing must always be permitted — it is the
	// operator's only escape from a value that has since gone bad.
	if req.DefaultGCPIdentityServiceAccountID == "" {
		return true
	}

	// Deliberately identical to the not-reachable message below. Distinguishing
	// them would make this endpoint an existence oracle: a project owner could
	// enumerate other projects' service account IDs by watching which ones fail
	// differently. "Does not exist" and "exists but is not yours" are one answer.
	//
	// The literal moved to msgSANotAvailableInProject — same string, no wire
	// change here — because the agent create and PATCH paths had NOT followed
	// this rule and now do. Three copies of a string whose entire value is that
	// they match is three chances to stop matching.
	const notAvailable = msgSANotAvailableInProject

	sa, err := s.store.GetGCPServiceAccount(ctx, req.DefaultGCPIdentityServiceAccountID)
	if err != nil {
		if err == store.ErrNotFound {
			BadRequest(w, notAvailable)
			return false
		}
		writeErrorFromErr(w, err, "")
		return false
	}

	if !sa.ReachableFromProject(project.ID) {
		BadRequest(w, notAvailable)
		return false
	}

	// Safe to be specific: this service account is already readable by this
	// caller, so naming its state discloses nothing they cannot already see.
	if !gcpServiceAccountVerified(sa) {
		BadRequest(w, "GCP service account is not verified; verify it before setting it as the project default")
		return false
	}

	return true
}

// isKubernetesRuntimeType reports whether a BrokerProfile.Type names the
// Kubernetes runtime, under any spelling the runtime factory accepts
// (pkg/runtime/factory.go): "kubernetes" itself, the "k8s" alias, and
// "remote" (normalized to "kubernetes" before dispatch). Phase 1
// (pkg/runtimebroker/start_context.go, ptone/scion#2338) classifies by this
// same string set, checked against the resolved runtime's Name() rather than
// a stored profile field, so the two cannot drift on which spellings count.
//
// A profile's Type is the settings *runtime key* the profile's `runtime:`
// field references (cmd/server_broker.go, pkg/runtimebroker/handlers.go build
// it directly from that map), not a resolved type — but it IS exactly that
// runtime key, regardless of what the profile itself is named: a profile
// named "my-cluster" with `runtime: kubernetes` reports Type "kubernetes",
// correctly, because "kubernetes" is the runtime key, not the profile name
// (the profile name is never inspected). The gap is a custom-named *runtime
// entry*: `runtimes.gke-prod: {type: kubernetes}`
// referenced by `runtime: gke-prod` reports Type "gke-prod" and is missed
// here (pkg/config/settings_v1.go ResolveRuntime resolves it through
// V1RuntimeConfig.Type, which this function never sees). The same gap runs in
// reverse: a runtime key spelled "kubernetes"/"k8s"/"remote" with an explicit
// `type: docker` (or any other non-Kubernetes type) is misclassified as
// Kubernetes here, though it dispatches as that other type. Both gaps need
// the broker to report its resolved type instead of the profile's runtime
// key; out of scope for this check.
func isKubernetesRuntimeType(t string) bool {
	switch t {
	case "kubernetes", "k8s", "remote":
		return true
	default:
		return false
	}
}

// brokerIsKubernetesOnly reports whether every profile registered on broker
// names the Kubernetes runtime (isKubernetesRuntimeType). A broker with no
// registered profiles is not considered Kubernetes-only — there is nothing to
// confirm the type from, and this must not guess.
func brokerIsKubernetesOnly(broker *store.RuntimeBroker) bool {
	if broker == nil || len(broker.Profiles) == 0 {
		return false
	}
	for _, p := range broker.Profiles {
		if !isKubernetesRuntimeType(p.Type) {
			return false
		}
	}
	return true
}

// projectIsKubernetesBound reports whether a project's target runtime is
// reliably known to be Kubernetes: the project has at least one linked
// runtime broker (project_providers), and every linked broker is
// Kubernetes-only per brokerIsKubernetesOnly.
//
// A project with no linked broker, with a linked broker whose record no
// longer exists (store.ErrNotFound), or with a mix of runtime types
// across its linked brokers, is NOT reliably Kubernetes-bound — this returns
// false rather than guess in those cases, which is the deliberately
// permissive side: it only ever blocks a write it can confirm the broker will
// reject anyway. A real store error (anything other than ErrNotFound) is
// propagated rather than silently treated as "allow": that failure mode is a
// backend problem the caller should see as a 500, not a policy decision.
//
// This enumerates ALL of the project's project_providers rows, unfiltered.
// The web UI's equivalent check (project-settings.ts, isBrokerKubernetesOnly)
// instead uses GET /api/v1/runtime-brokers?projectId=, which is scoped to
// brokers the caller may read and excludes scion.io/plugin-labelled brokers
// (handlers_runtime_brokers.go). The two can disagree — e.g. the UI may
// disable Block because the caller cannot see a linked docker broker, while
// this function (and therefore the actual write) does not, and allows it.
// That is acceptable: the UI's role here is cosmetic (hide/disable an option
// this function would reject anyway), and this function is the actual gate.
func (s *Server) projectIsKubernetesBound(ctx context.Context, projectID string) (bool, error) {
	providers, err := s.store.GetProjectProviders(ctx, projectID)
	if err != nil {
		return false, err
	}
	if len(providers) == 0 {
		return false, nil
	}
	for _, provider := range providers {
		broker, err := s.store.GetRuntimeBroker(ctx, provider.BrokerID)
		if err != nil {
			if err == store.ErrNotFound {
				// The provider link outlived its broker record (or points at
				// one this caller cannot resolve). Cannot confirm this
				// broker's runtime type; do not guess.
				return false, nil
			}
			return false, err
		}
		if !brokerIsKubernetesOnly(broker) {
			return false, nil
		}
	}
	return true, nil
}

// projectSettingsFromAnnotations reads project settings from the project's annotations map.
func projectSettingsFromAnnotations(project *store.Project) *hubclient.ProjectSettings {
	settings := &hubclient.ProjectSettings{}
	if project.Annotations == nil {
		return settings
	}

	settings.DefaultTemplate = project.Annotations[projectSettingDefaultTemplate]
	settings.DefaultHarnessConfig = project.Annotations[projectSettingDefaultHarnessConfig]
	settings.DefaultHarnessAuth = project.Annotations[projectSettingDefaultHarnessAuth]
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

	// Agent authorization
	settings.MaxAgentRole = project.Annotations[projectSettingMaxAgentRole]
	settings.DefaultAgentRole = project.Annotations[projectSettingDefaultAgentRole]

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
	setOrDelete(project.Annotations, projectSettingDefaultHarnessAuth, settings.DefaultHarnessAuth)
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

	// Agent authorization
	setOrDelete(project.Annotations, projectSettingMaxAgentRole, settings.MaxAgentRole)
	setOrDelete(project.Annotations, projectSettingDefaultAgentRole, settings.DefaultAgentRole)

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

	// Apply default harness auth (only if not already set)
	if ac.HarnessAuth == "" && settings.DefaultHarnessAuth != "" {
		ac.HarnessAuth = settings.DefaultHarnessAuth
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
