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

package runtimebroker

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// dispatchProfileSelection names the settings entries a dispatch resolves
// to: the effective profile name and the `runtimes:` map key that profile
// selects. Both are used only to look up operator-configured Kubernetes
// settings (the ServiceAccount mapping and the pinned namespace); the
// runtime type itself is always taken from resolveManagerForOpts.
type dispatchProfileSelection struct {
	// ProfileName is opts.Profile if non-empty, else the project-merged
	// settings' ActiveProfile, or "" when the selection came from
	// ForceRuntime or the broker's bare default.
	ProfileName string
	// RuntimeEntryName is the `runtimes:` map key the profile selects (for
	// example "k8s"), or "" when there is no settings-based entry. When the
	// broker runs with ForceRuntime, no profile is consulted and this is the
	// ForceRuntime runtime type name itself (for example "kubernetes"), so
	// the mapping and namespace for such a broker are read from the
	// runtimes: entry keyed by that type name.
	RuntimeEntryName string
}

// resolveDispatchProfileSelection returns the profile and runtime entry
// names opts resolves to, following the same ForceRuntime-then-settings
// order as resolveManagerForOpts, without building or caching a runtime.
//
// A project settings load error is logged and yields an empty selection;
// resolveDispatchProfileSelectionWithError returns it instead.
func (s *Server) resolveDispatchProfileSelection(opts api.StartOptions) dispatchProfileSelection {
	sel, err := s.resolveDispatchProfileSelectionWithError(opts)
	if err != nil {
		s.agentLifecycleLog.Warn("failed to load project settings for profile selection",
			"projectPath", opts.ProjectPath, "error", err)
	}
	return sel
}

// resolveDispatchProfileSelectionWithError is resolveDispatchProfileSelection
// returning the project settings load error instead of logging it.
func (s *Server) resolveDispatchProfileSelectionWithError(opts api.StartOptions) (dispatchProfileSelection, error) {
	// A flat instance selects no profile and no settings runtime entry.
	if s.isFlat() {
		return dispatchProfileSelection{}, nil
	}
	if _, forced := s.forcedRuntime(); forced {
		return dispatchProfileSelection{RuntimeEntryName: s.config.ForceRuntime}, nil
	}

	projectDir, _ := config.GetResolvedProjectDir(opts.ProjectPath)
	vs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		return dispatchProfileSelection{}, err
	}
	if vs == nil {
		return dispatchProfileSelection{}, nil
	}
	if _, _, err := vs.ResolveRuntime(opts.Profile); err != nil {
		return dispatchProfileSelection{}, nil
	}
	effectiveProfile := opts.Profile
	if effectiveProfile == "" {
		effectiveProfile = vs.ActiveProfile
	}
	sel := dispatchProfileSelection{ProfileName: effectiveProfile}
	if profile, ok := vs.Profiles[effectiveProfile]; ok {
		sel.RuntimeEntryName = profile.Runtime
	}
	return sel, nil
}

// kubernetesAssignIdentity is the result of resolving a GCP identity
// "assign" dispatch on the Kubernetes runtime. The zero value means the
// dispatch is not one.
type kubernetesAssignIdentity struct {
	// KSAName is the Kubernetes ServiceAccount the pod runs as.
	KSAName string
	// SAEmail and ProjectID are the GCP service account and project, set as
	// informational env on the agent.
	SAEmail   string
	ProjectID string
	// Selection is the profile and runtime entry the mapping and the
	// namespace were read from. The start and restart handlers compare it
	// with their own later resolution.
	Selection dispatchProfileSelection
}

// resolveKubernetesAssignIdentity resolves the Kubernetes ServiceAccount a
// GCP identity "assign" dispatch on the Kubernetes runtime runs as. It
// returns empty values and a nil error when the dispatch is not a
// Kubernetes "assign" dispatch.
//
// The GSA-to-KSA mapping and the namespace are read only from the broker's
// own global settings, never from a project's settings: the mapping decides
// which GCP identity the pod gets, so a project's own settings.yaml cannot
// redirect it. A project-level mapping is ignored with a warning.
//
// The pod's namespace also comes only from operator settings: the selected
// runtime entry's namespace, then the Kubernetes runtime's own default
// namespace. Profiles carry no namespace; a profile that needs another
// namespace selects its own runtime entry. An explicit request-level
// namespace is accepted only when it equals that resolved namespace.
func (s *Server) resolveKubernetesAssignIdentity(in startContextInputs, isKubernetes bool, gcpMetadataMode, profile string) (kubernetesAssignIdentity, *startContextError) {
	var saEmail, projectID, ksaName string
	if !isKubernetes || gcpMetadataMode != store.GCPMetadataModeAssign {
		return kubernetesAssignIdentity{}, nil
	}

	if in.Config != nil && in.Config.GCPIdentity != nil {
		saEmail = in.Config.GCPIdentity.SAEmail
		projectID = in.Config.GCPIdentity.ProjectID
	} else {
		saEmail = in.ResolvedEnv["SCION_METADATA_SA_EMAIL"]
		projectID = in.ResolvedEnv["SCION_METADATA_PROJECT_ID"]
	}
	// GCP service account emails are lowercase; normalize once so the
	// lookup, the validation and the messages below all agree.
	saEmail = strings.ToLower(saEmail)
	if saEmail == "" {
		return kubernetesAssignIdentity{}, &startContextError{
			Status:  http.StatusBadRequest,
			Message: fmt.Sprintf("GCP identity mode %q requires a service account email, and none was provided", store.GCPMetadataModeAssign),
		}
	}

	sel, err := s.resolveDispatchProfileSelectionWithError(api.StartOptions{
		ProjectPath: in.ProjectPath,
		Profile:     profile,
	})
	if err != nil {
		return kubernetesAssignIdentity{}, &startContextError{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("loading the project's settings to select the profile and runtime entry for GCP identity mode %q: %v", store.GCPMetadataModeAssign, err),
		}
	}

	// ProjectSettingsHasKubernetesServiceAccountMappings reads the project's
	// settings.yaml on its own, without the global layer underneath, so a
	// mapping inherited from global settings is not mistaken for one the
	// project set itself.
	if config.ProjectSettingsHasKubernetesServiceAccountMappings(in.ProjectPath, sel.RuntimeEntryName, sel.ProfileName) {
		s.agentLifecycleLog.Warn(
			"kubernetes_service_account_mappings is set in a project's own settings.yaml; it is never consulted there, only the broker's global settings are used for this mapping",
			"agent", in.Name, "profile", sel.ProfileName, "runtime_entry", sel.RuntimeEntryName)
	}

	// Global file plus the DB-backed overlay only: no project layer and no
	// SCION_ environment provider.
	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		return kubernetesAssignIdentity{}, &startContextError{
			Status:  http.StatusInternalServerError,
			Message: "loading the broker's global settings for the Kubernetes ServiceAccount mapping: " + err.Error(),
		}
	}
	mapped := false
	if vs != nil {
		ksaName, mapped = vs.ResolveKubernetesServiceAccountMappingForSelection(sel.ProfileName, sel.RuntimeEntryName, saEmail)
	}
	if !mapped {
		return kubernetesAssignIdentity{}, &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"GCP identity mode %q on the Kubernetes runtime has no Kubernetes ServiceAccount mapped for %q; add it to kubernetes_service_account_mappings in the broker's kubernetes runtime or profile settings",
				store.GCPMetadataModeAssign, saEmail),
		}
	}
	// settings.yaml can be hand-edited without going through any write-time
	// validator, so the entry is validated where it is used.
	if err := config.ValidateKubernetesServiceAccountMappings(map[string]string{saEmail: ksaName}); err != nil {
		return kubernetesAssignIdentity{}, &startContextError{Status: http.StatusBadRequest, Message: err.Error()}
	}

	// The mapping is authoritative: a differing explicit request-level
	// name (inline config or the create request's Kubernetes config) is
	// refused. A template-only serviceAccountName is overridden by the
	// mapping in pkg/agent, like any other request-level value.
	explicitKSA, explicitNamespace := explicitKubernetesIdentity(in)
	if explicitKSA != "" && explicitKSA != ksaName {
		return kubernetesAssignIdentity{}, &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"explicit Kubernetes ServiceAccount %q does not match the ServiceAccount %q mapped to %q; remove the explicit serviceAccountName or update kubernetes_service_account_mappings",
				explicitKSA, ksaName, saEmail),
		}
	}

	// The Workload Identity principal is the (namespace, KSA) pair, so the
	// namespace comes from the same operator settings as the mapping: the
	// selected runtime entry's namespace, else the runtime's default.
	operatorNamespace := resolveAssignNamespace(vs, sel.RuntimeEntryName)
	if sce := s.checkAssignPlacementMatchesGlobal(in, vs, sel, operatorNamespace); sce != nil {
		return kubernetesAssignIdentity{}, sce
	}
	if explicitNamespace != "" {
		if explicitNamespace != operatorNamespace {
			return kubernetesAssignIdentity{}, &startContextError{
				Status: http.StatusBadRequest,
				Message: fmt.Sprintf(
					"explicit Kubernetes namespace %q does not match the namespace %q from the broker's runtime settings for GCP identity mode %q; remove the explicit namespace, or select a runtime entry that sets it",
					explicitNamespace, operatorNamespace, store.GCPMetadataModeAssign),
			}
		}
	}

	return kubernetesAssignIdentity{
		KSAName:   ksaName,
		SAEmail:   saEmail,
		ProjectID: projectID,
		Selection: sel,
	}, nil
}

// resolveAssignNamespace returns the namespace an "assign" dispatch's pod
// runs in, from operator settings only: the runtime entry's namespace, else
// the Kubernetes runtime's default namespace (the same fallback the runtime
// itself uses when the entry sets none).
func resolveAssignNamespace(vs *config.VersionedSettings, runtimeEntryName string) string {
	if vs != nil {
		if ns, ok := vs.ResolveKubernetesNamespace(runtimeEntryName); ok {
			return ns
		}
	}
	return scionrt.DefaultKubernetesNamespace()
}

// checkAssignPlacementMatchesGlobal refuses an "assign" dispatch whose pod
// would be placed with a different namespace or context than the global
// runtime entry the mapping was read from. Placement reads the runtime
// entry from the project's settings merged over the global ones, so a
// project can override runtimes.<entry>.namespace or .context; the mapping
// is only valid for the global entry's values. Under ForceRuntime the
// broker places the pod on the forced runtime as configured, so its
// namespace is compared instead.
func (s *Server) checkAssignPlacementMatchesGlobal(in startContextInputs, globalVS *config.VersionedSettings, sel dispatchProfileSelection, operatorNamespace string) *startContextError {
	entry := sel.RuntimeEntryName
	if rt, forced := s.forcedRuntime(); forced {
		k8sRT, ok := rt.(*scionrt.KubernetesRuntime)
		if !ok || k8sRT.DefaultNamespace == operatorNamespace {
			return nil
		}
		return &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"GCP identity mode %q: the broker's runtime %q places pods in namespace %q, but the global settings for runtime entry %q resolve namespace %q; set runtimes.%s.namespace in the broker's global settings to the runtime's namespace",
				store.GCPMetadataModeAssign, rt.Name(), k8sRT.DefaultNamespace, entry, operatorNamespace, entry),
		}
	}

	projectDir, _ := config.GetResolvedProjectDir(in.ProjectPath)
	pvs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		return &startContextError{
			Status:  http.StatusInternalServerError,
			Message: "loading the project's settings to check the Kubernetes runtime entry for GCP identity mode \"" + store.GCPMetadataModeAssign + "\": " + err.Error(),
		}
	}
	placementNamespace := resolveAssignNamespace(pvs, entry)
	if placementNamespace != operatorNamespace {
		return &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"GCP identity mode %q: runtime entry %q resolves namespace %q in the project's settings but %q in the broker's global settings, where the Kubernetes ServiceAccount mapping is read; remove the project's runtimes.%s.namespace override",
				store.GCPMetadataModeAssign, entry, placementNamespace, operatorNamespace, entry),
		}
	}
	var globalContext, placementContext string
	if globalVS != nil {
		globalContext = globalVS.Runtimes[entry].Context
	}
	if pvs != nil {
		placementContext = pvs.Runtimes[entry].Context
	}
	if placementContext != globalContext {
		return &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"GCP identity mode %q: runtime entry %q sets context %q in the project's settings but %q in the broker's global settings, where the Kubernetes ServiceAccount mapping is read; remove the project's runtimes.%s.context override",
				store.GCPMetadataModeAssign, entry, placementContext, globalContext, entry),
		}
	}
	return nil
}

// forcedRuntime returns the runtime a ForceRuntime broker places agents on,
// following the same order as resolveManagerForOpts, and whether
// ForceRuntime applies.
func (s *Server) forcedRuntime() (scionrt.Runtime, bool) {
	if s.config.ForceRuntime == "" {
		return nil, false
	}
	if s.config.ForceRuntime == s.runtime.Name() {
		return s.runtime, true
	}
	if aux, ok := s.findAuxiliaryRuntimeByType(s.config.ForceRuntime); ok {
		return aux.Runtime, true
	}
	return nil, false
}

// explicitKubernetesIdentity returns the request-level Kubernetes
// ServiceAccount name and namespace: inline config first, then the create
// request's own Kubernetes config. Template values are not visible here.
func explicitKubernetesIdentity(in startContextInputs) (serviceAccountName, namespace string) {
	if in.InlineConfig != nil && in.InlineConfig.Kubernetes != nil {
		serviceAccountName = in.InlineConfig.Kubernetes.ServiceAccountName
		namespace = in.InlineConfig.Kubernetes.Namespace
	}
	if in.Config != nil && in.Config.Kubernetes != nil {
		if serviceAccountName == "" {
			serviceAccountName = in.Config.Kubernetes.ServiceAccountName
		}
		if namespace == "" {
			namespace = in.Config.Kubernetes.Namespace
		}
	}
	return serviceAccountName, namespace
}

// rejectKubernetesAssignRuntimeChange refuses a start or restart whose
// later, authoritative resolution (resolvedRuntimeType, and lateSelection
// for the profile and runtime entry) does not match what buildStartContext
// resolved the GCP identity "assign" env for:
//
//   - a Workload Identity ServiceAccount was resolved for Kubernetes, but
//     the later runtime is not Kubernetes, or the later profile or runtime
//     entry differs from the one the mapping and namespace were read from;
//   - the env was built for "assign" on a non-Kubernetes runtime (the
//     metadata emulator), but the later runtime is Kubernetes, so no
//     ServiceAccount mapping was resolved for it.
//
// Running either dispatch would not apply the assigned identity as
// configured, so it is refused before any side effect.
func rejectKubernetesAssignRuntimeChange(opts api.StartOptions, early *dispatchProfileSelection, resolvedRuntimeType string, lateSelection func() dispatchProfileSelection) *startContextError {
	const remedy = "recreate the agent, or restore the profile or runtime settings it was resolved with"
	lateIsKubernetes := isKubernetesRuntimeName(resolvedRuntimeType)
	var msg string
	switch {
	case opts.ResolvedKubernetesServiceAccountName != "":
		var was dispatchProfileSelection
		if early != nil {
			was = *early
		}
		if !lateIsKubernetes {
			msg = fmt.Sprintf(
				"GCP identity mode %q was resolved for the Kubernetes runtime (profile %q, runtime entry %q), but this dispatch now resolves to runtime %q; %s",
				store.GCPMetadataModeAssign, was.ProfileName, was.RuntimeEntryName, resolvedRuntimeType, remedy)
		} else if early != nil {
			if late := lateSelection(); late != was {
				msg = fmt.Sprintf(
					"GCP identity mode %q was resolved for profile %q and runtime entry %q, but this dispatch now resolves to profile %q and runtime entry %q; %s",
					store.GCPMetadataModeAssign, was.ProfileName, was.RuntimeEntryName, late.ProfileName, late.RuntimeEntryName, remedy)
			}
		}
	case opts.Env["SCION_METADATA_MODE"] == store.GCPMetadataModeAssign:
		if lateIsKubernetes {
			msg = fmt.Sprintf(
				"GCP identity mode %q was resolved for a non-Kubernetes runtime, but this dispatch now resolves to the Kubernetes runtime %q; %s",
				store.GCPMetadataModeAssign, resolvedRuntimeType, remedy)
		}
	}
	if msg == "" {
		return nil
	}
	return &startContextError{Status: http.StatusConflict, Message: msg}
}
