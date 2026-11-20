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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
	// InstanceKey and RuntimeTargetID identify a flat instance's selection
	// (flatDispatchSelection): its instance and persisted runtime target.
	// They are empty for a legacy selection, and ProfileName and
	// RuntimeEntryName are empty for a flat one.
	InstanceKey     string
	RuntimeTargetID string
}

// The remedies of the start/restart identity refusals: the legacy one names
// the profile and runtime settings a selection comes from; a flat
// instance's selection is fixed by its instance and runtime target instead.
const (
	legacyIdentityChangeRemedy = "recreate the agent, or restore the profile or runtime settings it was resolved with"
	flatIdentityChangeRemedy   = "recreate the agent, or start it on the Runtime Broker instance and runtime target it was resolved for"
)

// isFlat reports whether the selection is a flat instance's
// (flatDispatchSelection).
func (sel dispatchProfileSelection) isFlat() bool {
	return sel.InstanceKey != "" || sel.RuntimeTargetID != ""
}

// identityChangeWording names the selections a start/restart identity
// refusal compares, each with its two parts joined by joiner, and returns
// the remedy. When neither is a flat instance's selection, both are named
// by profile and runtime entry with the legacy remedy. When either is,
// both are named in flat terms (a flat selection by its instance key and
// runtime target ID, any other as having no Runtime Broker instance, never
// as empty profile and runtime entry names) with the flat remedy.
func identityChangeWording(was, late dispatchProfileSelection, joiner string) (wasText, lateText, remedy string) {
	if !was.isFlat() && !late.isFlat() {
		return fmt.Sprintf("profile %q%sruntime entry %q", was.ProfileName, joiner, was.RuntimeEntryName),
			fmt.Sprintf("profile %q%sruntime entry %q", late.ProfileName, joiner, late.RuntimeEntryName),
			legacyIdentityChangeRemedy
	}
	flatText := func(sel dispatchProfileSelection) string {
		if !sel.isFlat() {
			return "no Runtime Broker instance"
		}
		return fmt.Sprintf("Runtime Broker instance %q%sruntime target %q", sel.InstanceKey, joiner, sel.RuntimeTargetID)
	}
	return flatText(was), flatText(late), flatIdentityChangeRemedy
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
	// A flat instance selects no profile and no settings runtime entry:
	// its selection is its own instance.
	if s.isFlat() {
		return s.flatDispatchSelection(), nil
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
func (s *Server) resolveKubernetesAssignIdentity(ctx context.Context, in startContextInputs, mgr agent.Manager, isKubernetes bool, gcpMetadataMode, profile string) (kubernetesAssignIdentity, *startContextError) {
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

	if s.isFlat() {
		return s.resolveFlatKubernetesAssignIdentity(ctx, mgr, in, saEmail, projectID, sel)
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
	// The Workload Identity pool the broker reports to the hub
	// (heartbeatWorkloadIdentityTargets) is likewise read only from the
	// broker's settings.
	if config.ProjectSettingsHasWorkloadIdentityPool(in.ProjectPath, sel.RuntimeEntryName) {
		s.agentLifecycleLog.Warn(
			"workload_identity_pool is set in a project's own settings.yaml; it is never consulted there, only the broker's global settings are used for the Workload Identity pool",
			"agent", in.Name, "profile", sel.ProfileName, "runtime_entry", sel.RuntimeEntryName)
	}
	if config.ProjectSettingsHasCreateServiceAccounts(in.ProjectPath, sel.RuntimeEntryName) {
		s.agentLifecycleLog.Warn(
			"create_service_accounts is set in a project's own settings.yaml; it is never consulted there, only the broker's global settings are used",
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
	ksaSource := api.BrokerKSASourceMapped
	// The Workload Identity principal is the (namespace, KSA) pair, so the
	// namespace comes from the same operator settings as the mapping: the
	// selected runtime entry's namespace, else the runtime's default.
	operatorNamespace := resolveAssignNamespace(vs, sel.RuntimeEntryName)
	explicitKSA, explicitNamespace := explicitKubernetesIdentity(in)
	placementChecked := false
	if !mapped {
		// Discovery lists the namespace the pod will run in, so the
		// placement check runs first: it refuses a project override or a
		// forced runtime that would place the pod elsewhere.
		if sce := s.checkAssignPlacementMatchesGlobal(in, vs, sel, operatorNamespace); sce != nil {
			return kubernetesAssignIdentity{}, sce
		}
		placementChecked = true
		var sce *startContextError
		ksaName, sce = s.discoverAssignKSA(ctx, mgr, in.Name, operatorNamespace, saEmail, sel)
		if sce == nil {
			ksaSource = api.BrokerKSASourceDiscovered
			s.agentLifecycleLog.Info("discovered the Kubernetes ServiceAccount for GCP identity mode assign by annotation",
				"agent", in.Name, "service_account", saEmail, "namespace", operatorNamespace, "ksa", ksaName)
		} else if sce.Code == ErrCodeIdentityNotMapped &&
			sce.Details[api.BrokerErrDetailDiscovery] == api.BrokerKSADiscoveryNoMatch &&
			vs != nil && vs.Runtimes[sel.RuntimeEntryName].CreateServiceAccounts &&
			hubAuthorizedCreateKSA(ctx, in) {
			conventionalKSA, convErr := k8s.ConventionalKSAName(saEmail)
			if convErr != nil {
				return kubernetesAssignIdentity{}, &startContextError{Status: http.StatusBadRequest, Message: convErr.Error()}
			}
			// Check request-level explicit ServiceAccount and namespace
			// overrides before calling Create so a refused request never
			// leaves a newly created ServiceAccount behind on the cluster.
			if explicitKSA != "" && explicitKSA != conventionalKSA {
				return kubernetesAssignIdentity{}, s.ksaMismatchError(explicitKSA, conventionalKSA, saEmail, operatorNamespace, api.BrokerKSASourceCreated, sel)
			}
			if explicitNamespace != "" && explicitNamespace != operatorNamespace {
				return kubernetesAssignIdentity{}, explicitNamespaceMismatchError(explicitNamespace, operatorNamespace)
			}
			client, clientErr := s.assignClientsetFor(mgr)
			if clientErr != nil {
				return kubernetesAssignIdentity{}, sce
			}
			createdKSA, createSCE := s.createOrReuseAssignKSA(ctx, client, in.Name, operatorNamespace, conventionalKSA, saEmail, sel)
			if createSCE != nil {
				return kubernetesAssignIdentity{}, createSCE
			}
			ksaName = createdKSA
			ksaSource = api.BrokerKSASourceCreated
		} else {
			return kubernetesAssignIdentity{}, sce
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
	if explicitKSA != "" && explicitKSA != ksaName {
		return kubernetesAssignIdentity{}, s.ksaMismatchError(explicitKSA, ksaName, saEmail, operatorNamespace, ksaSource, sel)
	}

	if !placementChecked {
		if sce := s.checkAssignPlacementMatchesGlobal(in, vs, sel, operatorNamespace); sce != nil {
			return kubernetesAssignIdentity{}, sce
		}
	}
	if explicitNamespace != "" && explicitNamespace != operatorNamespace {
		return kubernetesAssignIdentity{}, explicitNamespaceMismatchError(explicitNamespace, operatorNamespace)
	}

	return kubernetesAssignIdentity{
		KSAName:   ksaName,
		SAEmail:   saEmail,
		ProjectID: projectID,
		Selection: sel,
	}, nil
}

// resolveFlatKubernetesAssignIdentity is resolveKubernetesAssignIdentity
// for a flat instance: the GSA-to-KSA mapping and opt-in ServiceAccount
// creation come from the instance's own snapshot (flatKubernetesIdentityPolicy)
// and the namespace is the instance's activated namespace. No profile, runtime
// entry, Hub overlay or project setting is read, and ServiceAccounts are never
// listed by annotation. When the mapping misses and both
// runtime_target.create_service_accounts and the Hub's per-dispatch CreateKSA
// flag are on, the broker reads the conventional <gsa-account-id> KSA by name
// (creating it if missing, reusing it only if Scion-labelled for saEmail, and
// refusing anything else with identity_ksa_conflict; ptone/scion#7967).
func (s *Server) resolveFlatKubernetesAssignIdentity(ctx context.Context, mgr agent.Manager, in startContextInputs, saEmail, projectID string, sel dispatchProfileSelection) (kubernetesAssignIdentity, *startContextError) {
	policy := s.flatKubernetesPolicy()
	explicitKSA, explicitNamespace := explicitKubernetesIdentity(in)
	ns := s.flatInstanceNamespace()
	ksaName, mapped := policy.mappedServiceAccount(saEmail)
	if !mapped {
		if !policy.createServiceAccounts || !hubAuthorizedCreateKSA(ctx, in) {
			return kubernetesAssignIdentity{}, &startContextError{
				Status: http.StatusBadRequest,
				Message: fmt.Sprintf(
					"GCP identity mode %q on the Kubernetes runtime has no Kubernetes ServiceAccount mapped for %q; add it to kubernetes_service_account_mappings in this flat Runtime Broker instance's runtime_target",
					store.GCPMetadataModeAssign, saEmail),
				Code:    ErrCodeIdentityNotMapped,
				Details: s.identityMappingErrorDetails(saEmail, sel),
			}
		}
		conventionalKSA, convErr := k8s.ConventionalKSAName(saEmail)
		if convErr != nil {
			return kubernetesAssignIdentity{}, &startContextError{Status: http.StatusBadRequest, Message: convErr.Error()}
		}
		// Refuse request-level ServiceAccount or namespace mismatches before
		// touching Kubernetes so a refused dispatch never leaves an orphan KSA.
		if explicitKSA != "" && explicitKSA != conventionalKSA {
			return kubernetesAssignIdentity{}, s.ksaMismatchError(explicitKSA, conventionalKSA, saEmail, ns, api.BrokerKSASourceCreated, sel)
		}
		if explicitNamespace != "" && explicitNamespace != ns {
			return kubernetesAssignIdentity{}, flatExplicitNamespaceMismatchError(explicitNamespace, ns)
		}
		if ns == "" {
			return kubernetesAssignIdentity{}, &startContextError{
				Status:  http.StatusBadRequest,
				Message: fmt.Sprintf("GCP identity mode %q on the Kubernetes runtime has no activated namespace for %q on this flat Runtime Broker instance", store.GCPMetadataModeAssign, saEmail),
				Code:    ErrCodeIdentityNotMapped,
				Details: s.identityMappingErrorDetails(saEmail, sel),
			}
		}
		client, clientErr := s.assignClientsetFor(mgr)
		if clientErr != nil {
			s.agentLifecycleLog.Warn("GCP identity mode assign: flat KSA lookup/create has no Kubernetes client",
				"agent", in.Name, "service_account", saEmail, "namespace", ns, "error", clientErr)
			return kubernetesAssignIdentity{}, &startContextError{
				Status: http.StatusBadRequest,
				Message: fmt.Sprintf(
					"GCP identity mode %q on the Kubernetes runtime could not look up or create the conventional ServiceAccount for %q: %v",
					store.GCPMetadataModeAssign, saEmail, clientErr),
				Code:    ErrCodeIdentityNotMapped,
				Details: s.identityMappingErrorDetails(saEmail, sel),
			}
		}
		resolvedKSA, createSCE := s.lookupOrCreateFlatAssignKSA(ctx, client, in.Name, ns, conventionalKSA, saEmail, sel)
		if createSCE != nil {
			return kubernetesAssignIdentity{}, createSCE
		}
		ksaName = resolvedKSA
	}
	if err := config.ValidateKubernetesServiceAccountMappings(map[string]string{saEmail: ksaName}); err != nil {
		return kubernetesAssignIdentity{}, &startContextError{Status: http.StatusBadRequest, Message: err.Error()}
	}
	if explicitKSA != "" && explicitKSA != ksaName {
		details := s.identityMappingErrorDetails(saEmail, sel)
		details[api.BrokerErrDetailRequestedKSA] = explicitKSA
		details[api.BrokerErrDetailMappedKSA] = ksaName
		return kubernetesAssignIdentity{}, &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"explicit Kubernetes ServiceAccount %q does not match the ServiceAccount %q mapped to %q; remove the explicit serviceAccountName or update this instance's kubernetes_service_account_mappings",
				explicitKSA, ksaName, saEmail),
			Code:    ErrCodeIdentityKSAMismatch,
			Details: details,
		}
	}
	if explicitNamespace != "" && explicitNamespace != ns {
		return kubernetesAssignIdentity{}, flatExplicitNamespaceMismatchError(explicitNamespace, ns)
	}
	return kubernetesAssignIdentity{KSAName: ksaName, SAEmail: saEmail, ProjectID: projectID, Selection: sel}, nil
}

func flatExplicitNamespaceMismatchError(explicitNamespace, instanceNamespace string) *startContextError {
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"explicit Kubernetes namespace %q does not match this flat Runtime Broker instance's namespace %q for GCP identity mode %q; remove the explicit namespace",
			explicitNamespace, instanceNamespace, store.GCPMetadataModeAssign),
	}
}

// assignDiscoveryTimeout bounds the ServiceAccount list discoverAssignKSA
// makes, so an unresponsive API server cannot hold a dispatch open.
const assignDiscoveryTimeout = 15 * time.Second

// discoverAssignKSA returns the Kubernetes ServiceAccount in namespace
// annotated iam.gke.io/gcp-service-account with saEmail, for a GCP identity
// "assign" dispatch that has no explicit mapping. Exactly one match is
// used. No match, a list error (including a forbidden one) and a missing
// Kubernetes client keep the identity_not_mapped refusal, with the reason
// added; more than one match is refused with the matching names, since the
// broker cannot choose between them.
//
// Each identity_not_mapped refusal is logged at Warn with its cause and
// carries the result as a fixed detail value (api.BrokerErrDetailDiscovery)
// and the namespace, so the hub can explain it without relaying API server
// text.
func (s *Server) discoverAssignKSA(ctx context.Context, mgr agent.Manager, agentName, namespace, saEmail string, sel dispatchProfileSelection) (string, *startContextError) {
	notMapped := func(result, reason string, cause error) *startContextError {
		attrs := []any{"agent", agentName, "service_account", saEmail, "namespace", namespace, "discovery", result}
		if cause != nil {
			attrs = append(attrs, "error", cause)
		}
		s.agentLifecycleLog.Warn("GCP identity mode assign: no Kubernetes ServiceAccount mapped or discovered by annotation", attrs...)
		details := s.identityMappingErrorDetails(saEmail, sel)
		details[api.BrokerErrDetailDiscovery] = result
		details[api.BrokerErrDetailNamespace] = namespace
		return &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"GCP identity mode %q on the Kubernetes runtime has no Kubernetes ServiceAccount mapped for %q; add it to kubernetes_service_account_mappings in the broker's kubernetes runtime or profile settings (%s)",
				store.GCPMetadataModeAssign, saEmail, reason),
			Code:    ErrCodeIdentityNotMapped,
			Details: details,
		}
	}
	rbacHint := fmt.Sprintf("discovery needs list (read-only) access to serviceaccounts in namespace %q", namespace)

	client, err := s.assignClientsetFor(mgr)
	if err != nil {
		return "", notMapped(api.BrokerKSADiscoveryUnavailable, fmt.Sprintf("ServiceAccount annotation discovery could not run: %v", err), err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	listCtx, cancel := context.WithTimeout(ctx, assignDiscoveryTimeout)
	defer cancel()
	names, err := k8s.FindServiceAccountsForGSA(listCtx, client, namespace, saEmail)
	if err != nil {
		return "", notMapped(api.BrokerKSADiscoveryListFailed, fmt.Sprintf("ServiceAccount annotation discovery failed to list serviceaccounts in namespace %q: %v; %s", namespace, err, rbacHint), err)
	}
	switch len(names) {
	case 0:
		return "", notMapped(api.BrokerKSADiscoveryNoMatch, fmt.Sprintf("annotation discovery found no ServiceAccount in namespace %q with %s: %s", namespace, k8s.WorkloadIdentityGSAAnnotation, saEmail), nil)
	case 1:
		return names[0], nil
	default:
		return "", &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"GCP identity mode %q on the Kubernetes runtime: more than one Kubernetes ServiceAccount in namespace %q is annotated with %s: %s (%s); add an explicit entry for %q to kubernetes_service_account_mappings in the broker's kubernetes runtime or profile settings to choose one",
				store.GCPMetadataModeAssign, namespace, k8s.WorkloadIdentityGSAAnnotation, saEmail, strings.Join(names, ", "), saEmail),
		}
	}
}

// assignClientsetFor returns the Kubernetes client for KSA discovery/creation,
// consulting s.assignKSAClientset first (test hook) and falling back to
// s.assignDiscoveryClientset.
func (s *Server) assignClientsetFor(mgr agent.Manager) (kubernetes.Interface, error) {
	if s.assignKSAClientset != nil {
		return s.assignKSAClientset(mgr)
	}
	return s.assignDiscoveryClientset(mgr)
}

// hubAuthorizedCreateKSA reports whether the Hub set CreateKSA on this
// dispatch's HubAgentDefaults (experiment hub.k8s_workload_identity_setup on
// AND a Hub-minted GSA). Start and restart attach HubAgentDefaults onto ctx
// before buildStartContext; create attaches them after buildStartContext, so
// in.Config.HubAgentDefaults is checked as well.
func hubAuthorizedCreateKSA(ctx context.Context, in startContextInputs) bool {
	if d := api.HubAgentDefaultsFromContext(ctx); d != nil && d.CreateKSA {
		return true
	}
	return in.Config != nil && in.Config.HubAgentDefaults != nil && in.Config.HubAgentDefaults.CreateKSA
}

// createOrReuseAssignKSA creates the conventional Kubernetes ServiceAccount
// ksaName in namespace for saEmail, or handles an AlreadyExists race/conflict
// without ever modifying an existing ServiceAccount (ptone/scion#7966).
//
// Legacy profile/runtime-entry path only: accepts an existing annotated
// ServiceAccount found on an AlreadyExists race (KSACreateReusedAnnotated),
// consistent with legacy annotation discovery. Flat instances must NOT call
// this wrapper: they call k8s.EnsureConventionalServiceAccount directly via
// lookupOrCreateFlatAssignKSA so annotated-only ServiceAccounts without
// app.kubernetes.io/managed-by=scion are refused as conflicts (ptone/scion#7967).
func (s *Server) createOrReuseAssignKSA(
	ctx context.Context,
	client kubernetes.Interface,
	agentName, namespace, ksaName, saEmail string,
	sel dispatchProfileSelection,
) (string, *startContextError) {
	if ctx == nil {
		ctx = context.Background()
	}
	createCtx, cancel := context.WithTimeout(ctx, assignDiscoveryTimeout)
	defer cancel()

	outcome, err := k8s.EnsureConventionalServiceAccount(createCtx, client, namespace, ksaName, saEmail)
	switch outcome {
	case k8s.KSACreateCreated:
		s.agentLifecycleLog.Info("created conventional Kubernetes ServiceAccount for GCP identity mode assign",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return ksaName, nil
	case k8s.KSACreateReusedManaged:
		s.agentLifecycleLog.Info("reused Scion-managed Kubernetes ServiceAccount after concurrent create",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return ksaName, nil
	case k8s.KSACreateReusedAnnotated:
		s.agentLifecycleLog.Info("reused annotated Kubernetes ServiceAccount found on create race",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return ksaName, nil
	case k8s.KSACreateConflict:
		s.agentLifecycleLog.Warn("GCP identity mode assign: conventional Kubernetes ServiceAccount exists with conflicting annotations",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return "", s.ksaConflictError(namespace, ksaName, saEmail, sel)
	case k8s.KSACreateForbiddenCreate:
		s.agentLifecycleLog.Warn("GCP identity mode assign: forbidden creating conventional Kubernetes ServiceAccount",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", err)
		return "", s.ksaPermissionNotMappedError(namespace, saEmail, api.BrokerKSAPermissionCreate, sel)
	case k8s.KSACreateForbiddenGet:
		s.agentLifecycleLog.Warn("GCP identity mode assign: forbidden reading existing conventional Kubernetes ServiceAccount",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", err)
		return "", s.ksaPermissionNotMappedError(namespace, saEmail, api.BrokerKSAPermissionGet, sel)
	default:
		s.agentLifecycleLog.Warn("GCP identity mode assign: failed creating conventional Kubernetes ServiceAccount",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", err)
		return "", &startContextError{
			Status:      http.StatusInternalServerError,
			Message:     fmt.Sprintf("creating conventional Kubernetes ServiceAccount %q in namespace %q for GCP identity mode %q", ksaName, namespace, store.GCPMetadataModeAssign),
			OriginalErr: err,
		}
	}
}

func (s *Server) ksaConflictError(namespace, ksaName, saEmail string, sel dispatchProfileSelection) *startContextError {
	details := s.identityMappingErrorDetails(saEmail, sel)
	details[api.BrokerErrDetailConflictingKSA] = ksaName
	details[api.BrokerErrDetailNamespace] = namespace
	return &startContextError{
		Status: http.StatusBadRequest,
		Code:   ErrCodeIdentityKSAConflict,
		Message: fmt.Sprintf(
			"Kubernetes ServiceAccount %q in namespace %q already exists and is not annotated with %s: %s; Scion never modifies an existing ServiceAccount. Annotate %q for %q if it belongs to this identity, map %q to another ServiceAccount in kubernetes_service_account_mappings, or delete the conflicting ServiceAccount",
			ksaName, namespace, k8s.WorkloadIdentityGSAAnnotation, saEmail, ksaName, saEmail, saEmail),
		Details: details,
	}
}

func (s *Server) ksaPermissionNotMappedError(namespace, saEmail, permission string, sel dispatchProfileSelection) *startContextError {
	details := s.identityMappingErrorDetails(saEmail, sel)
	details[api.BrokerErrDetailDiscovery] = api.BrokerKSADiscoveryNoMatch
	details[api.BrokerErrDetailNamespace] = namespace
	details[api.BrokerErrDetailMissingPermission] = permission
	return &startContextError{
		Status: http.StatusBadRequest,
		Code:   ErrCodeIdentityNotMapped,
		Message: fmt.Sprintf(
			"GCP identity mode %q on the Kubernetes runtime could not provision the conventional ServiceAccount for %q in namespace %q because the broker identity lacks %s permission in namespace %q; grant get, list, and create on serviceaccounts in namespace %q, or pre-provision a ServiceAccount and map it in kubernetes_service_account_mappings",
			store.GCPMetadataModeAssign, saEmail, namespace, permission, namespace, namespace),
		Details: details,
	}
}

// lookupOrCreateFlatAssignKSA resolves the conventional ServiceAccount ksaName
// in namespace for a flat Kubernetes instance (design §3.10, ptone/scion#7967):
// it performs one Get by name (never List), reuses an existing ServiceAccount
// ONLY when it carries both app.kubernetes.io/managed-by=scion and the
// annotation for saEmail, creates it via k8s.EnsureConventionalServiceAccount
// when NotFound, and refuses anything else (including annotated-only KSAs)
// with identity_ksa_conflict without ever modifying an existing ServiceAccount.
func (s *Server) lookupOrCreateFlatAssignKSA(
	ctx context.Context,
	client kubernetes.Interface,
	agentName, namespace, ksaName, saEmail string,
	sel dispatchProfileSelection,
) (string, *startContextError) {
	if ctx == nil {
		ctx = context.Background()
	}
	opCtx, cancel := context.WithTimeout(ctx, assignDiscoveryTimeout)
	defer cancel()

	existing, getErr := client.CoreV1().ServiceAccounts(namespace).Get(opCtx, ksaName, metav1.GetOptions{})
	switch {
	case getErr == nil:
		if k8s.ServiceAccountManagedForGSA(existing, saEmail) {
			s.agentLifecycleLog.Info("reused Scion-managed conventional Kubernetes ServiceAccount for GCP identity mode assign",
				"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
			return ksaName, nil
		}
		s.agentLifecycleLog.Warn("GCP identity mode assign: conventional Kubernetes ServiceAccount exists on flat instance without Scion ownership for this GSA",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return "", s.flatKSAConflictError(namespace, ksaName, saEmail, sel)
	case apierrors.IsForbidden(getErr):
		s.agentLifecycleLog.Warn("GCP identity mode assign: forbidden reading conventional Kubernetes ServiceAccount on flat instance",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", getErr)
		return "", s.flatKSAPermissionNotMappedError(namespace, saEmail, api.BrokerKSAPermissionGet, sel)
	case !apierrors.IsNotFound(getErr):
		s.agentLifecycleLog.Warn("GCP identity mode assign: failed reading conventional Kubernetes ServiceAccount on flat instance",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", getErr)
		return "", &startContextError{
			Status:      http.StatusInternalServerError,
			Message:     fmt.Sprintf("reading conventional Kubernetes ServiceAccount %q in namespace %q for GCP identity mode %q", ksaName, namespace, store.GCPMetadataModeAssign),
			OriginalErr: getErr,
		}
	}

	outcome, err := k8s.EnsureConventionalServiceAccount(opCtx, client, namespace, ksaName, saEmail)
	switch outcome {
	case k8s.KSACreateCreated:
		s.agentLifecycleLog.Info("created conventional Kubernetes ServiceAccount for GCP identity mode assign",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return ksaName, nil
	case k8s.KSACreateReusedManaged:
		s.agentLifecycleLog.Info("reused Scion-managed Kubernetes ServiceAccount after concurrent create",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return ksaName, nil
	case k8s.KSACreateReusedAnnotated, k8s.KSACreateConflict:
		s.agentLifecycleLog.Warn("GCP identity mode assign: conventional Kubernetes ServiceAccount created concurrently without Scion ownership for this GSA",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName)
		return "", s.flatKSAConflictError(namespace, ksaName, saEmail, sel)
	case k8s.KSACreateForbiddenCreate:
		s.agentLifecycleLog.Warn("GCP identity mode assign: forbidden creating conventional Kubernetes ServiceAccount on flat instance",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", err)
		return "", s.flatKSAPermissionNotMappedError(namespace, saEmail, api.BrokerKSAPermissionCreate, sel)
	case k8s.KSACreateForbiddenGet:
		s.agentLifecycleLog.Warn("GCP identity mode assign: forbidden reading existing conventional Kubernetes ServiceAccount on flat instance",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", err)
		return "", s.flatKSAPermissionNotMappedError(namespace, saEmail, api.BrokerKSAPermissionGet, sel)
	default:
		s.agentLifecycleLog.Warn("GCP identity mode assign: failed creating conventional Kubernetes ServiceAccount on flat instance",
			"agent", agentName, "service_account", saEmail, "namespace", namespace, "ksa", ksaName, "error", err)
		return "", &startContextError{
			Status:      http.StatusInternalServerError,
			Message:     fmt.Sprintf("creating conventional Kubernetes ServiceAccount %q in namespace %q for GCP identity mode %q", ksaName, namespace, store.GCPMetadataModeAssign),
			OriginalErr: err,
		}
	}
}

func (s *Server) flatKSAConflictError(namespace, ksaName, saEmail string, sel dispatchProfileSelection) *startContextError {
	details := s.identityMappingErrorDetails(saEmail, sel)
	details[api.BrokerErrDetailConflictingKSA] = ksaName
	details[api.BrokerErrDetailNamespace] = namespace
	return &startContextError{
		Status: http.StatusBadRequest,
		Code:   ErrCodeIdentityKSAConflict,
		Message: fmt.Sprintf(
			"Kubernetes ServiceAccount %q in namespace %q already exists and was not created by Scion for %s (%s=%s and %s: %s); Scion never modifies an existing ServiceAccount, and flat Runtime Broker instances never adopt an unlabelled ServiceAccount by annotation. Map %q to the intended ServiceAccount in runtime_target.kubernetes_service_account_mappings, or delete the conflicting ServiceAccount",
			ksaName, namespace, saEmail, k8s.ManagedByLabel, k8s.ManagedByScion, k8s.WorkloadIdentityGSAAnnotation, saEmail, saEmail),
		Details: details,
	}
}

func (s *Server) flatKSAPermissionNotMappedError(namespace, saEmail, permission string, sel dispatchProfileSelection) *startContextError {
	details := s.identityMappingErrorDetails(saEmail, sel)
	details[api.BrokerErrDetailNamespace] = namespace
	details[api.BrokerErrDetailMissingPermission] = permission
	return &startContextError{
		Status: http.StatusBadRequest,
		Code:   ErrCodeIdentityNotMapped,
		Message: fmt.Sprintf(
			"GCP identity mode %q on the Kubernetes runtime could not provision the conventional ServiceAccount for %q in namespace %q because the broker identity lacks %s permission in namespace %q; grant get and create on serviceaccounts in namespace %q, or pre-provision a ServiceAccount and map it in this flat Runtime Broker instance's runtime_target.kubernetes_service_account_mappings",
			store.GCPMetadataModeAssign, saEmail, namespace, permission, namespace, namespace),
		Details: details,
	}
}

func (s *Server) ksaMismatchError(explicitKSA, ksaName, saEmail, namespace, source string, sel dispatchProfileSelection) *startContextError {
	details := s.identityMappingErrorDetails(saEmail, sel)
	details[api.BrokerErrDetailRequestedKSA] = explicitKSA
	details[api.BrokerErrDetailMappedKSA] = ksaName
	details[api.BrokerErrDetailKSASource] = source
	msg := fmt.Sprintf(
		"explicit Kubernetes ServiceAccount %q does not match the ServiceAccount %q mapped to %q; remove the explicit serviceAccountName or update kubernetes_service_account_mappings",
		explicitKSA, ksaName, saEmail)
	switch source {
	case api.BrokerKSASourceDiscovered:
		msg = fmt.Sprintf(
			"explicit Kubernetes ServiceAccount %q does not match the ServiceAccount %q discovered for %q by its %s annotation in namespace %q; remove the explicit serviceAccountName, or add a kubernetes_service_account_mappings entry",
			explicitKSA, ksaName, saEmail, k8s.WorkloadIdentityGSAAnnotation, namespace)
		details[api.BrokerErrDetailNamespace] = namespace
	case api.BrokerKSASourceCreated:
		msg = fmt.Sprintf(
			"explicit Kubernetes ServiceAccount %q does not match the conventional ServiceAccount %q for %q in namespace %q; remove the explicit serviceAccountName, or add a kubernetes_service_account_mappings entry",
			explicitKSA, ksaName, saEmail, namespace)
		details[api.BrokerErrDetailNamespace] = namespace
	}
	return &startContextError{
		Status:  http.StatusBadRequest,
		Message: msg,
		Code:    ErrCodeIdentityKSAMismatch,
		Details: details,
	}
}

func explicitNamespaceMismatchError(explicitNamespace, operatorNamespace string) *startContextError {
	return &startContextError{
		Status: http.StatusBadRequest,
		Message: fmt.Sprintf(
			"explicit Kubernetes namespace %q does not match the namespace %q from the broker's runtime settings for GCP identity mode %q; remove the explicit namespace, or select a runtime entry that sets it",
			explicitNamespace, operatorNamespace, store.GCPMetadataModeAssign),
	}
}

// assignDiscoveryClientset returns the Kubernetes client of the runtime mgr
// dispatches to, for discoverAssignKSA. It errors when that runtime is not
// a Kubernetes runtime with a client.
func (s *Server) assignDiscoveryClientset(mgr agent.Manager) (kubernetes.Interface, error) {
	var rt scionrt.Runtime
	if am, ok := mgr.(*agent.AgentManager); ok {
		rt = am.Runtime
	} else if mgr != nil && mgr == s.manager {
		rt = s.runtime
	}
	k8sRT, ok := rt.(*scionrt.KubernetesRuntime)
	if !ok || k8sRT == nil || k8sRT.Client == nil || k8sRT.Client.Clientset == nil {
		return nil, errors.New("the selected runtime has no Kubernetes client")
	}
	return k8sRT.Client.Clientset, nil
}

// identityMappingErrorDetails returns the error details of an
// identity_not_mapped or identity_ksa_mismatch refusal: the GCP service
// account, the selected profile and runtime entry, and this broker's name.
// The hub builds its client message from them. Empty values are left out.
func (s *Server) identityMappingErrorDetails(saEmail string, sel dispatchProfileSelection) map[string]interface{} {
	details := map[string]interface{}{api.BrokerErrDetailServiceAccount: saEmail}
	for k, v := range map[string]string{
		api.BrokerErrDetailProfile:      sel.ProfileName,
		api.BrokerErrDetailRuntimeEntry: sel.RuntimeEntryName,
		api.BrokerErrDetailBroker:       s.config.BrokerName,
	} {
		if v != "" {
			details[k] = v
		}
	}
	return details
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
	if rt := s.currentRuntime(); s.config.ForceRuntime == rt.Name() {
		return rt, true
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
	lateIsKubernetes := isKubernetesRuntimeName(resolvedRuntimeType)
	var msg string
	switch {
	case opts.ResolvedKubernetesServiceAccountName != "":
		var was dispatchProfileSelection
		if early != nil {
			was = *early
		}
		if !lateIsKubernetes {
			wasText, _, remedy := identityChangeWording(was, dispatchProfileSelection{}, ", ")
			msg = fmt.Sprintf(
				"GCP identity mode %q was resolved for the Kubernetes runtime (%s), but this dispatch now resolves to runtime %q; %s",
				store.GCPMetadataModeAssign, wasText, resolvedRuntimeType, remedy)
		} else if early != nil {
			if late := lateSelection(); late != was {
				wasText, lateText, remedy := identityChangeWording(was, late, " and ")
				msg = fmt.Sprintf(
					"GCP identity mode %q was resolved for %s, but this dispatch now resolves to %s; %s",
					store.GCPMetadataModeAssign, wasText, lateText, remedy)
			}
		}
	case opts.Env["SCION_METADATA_MODE"] == store.GCPMetadataModeAssign:
		if lateIsKubernetes {
			msg = fmt.Sprintf(
				"GCP identity mode %q was resolved for a non-Kubernetes runtime, but this dispatch now resolves to the Kubernetes runtime %q; %s",
				store.GCPMetadataModeAssign, resolvedRuntimeType, legacyIdentityChangeRemedy)
		}
	}
	if msg == "" {
		return nil
	}
	return &startContextError{Status: http.StatusConflict, Message: msg}
}
