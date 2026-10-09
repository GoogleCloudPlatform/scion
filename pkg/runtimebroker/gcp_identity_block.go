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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// kubernetesBlockIdentity is the result of resolving a GCP identity "block"
// dispatch on the Kubernetes runtime. The zero value (nil Identity) means
// the dispatch is not one.
type kubernetesBlockIdentity struct {
	// Identity is threaded onto StartOptions.KubernetesBlockIdentity.
	Identity *api.KubernetesBlockIdentity
	// Selection is the profile and runtime entry the block ServiceAccount
	// was read from. The start and restart handlers compare it with their
	// own later resolution.
	Selection dispatchProfileSelection
}

// resolveKubernetesBlockIdentity resolves how a GCP identity "block"
// dispatch on the Kubernetes runtime runs (ptone/scion#4034). It returns a
// zero value and nil error when the dispatch is not a Kubernetes "block"
// dispatch.
//
// GKE cannot give a single pod "no GCP identity" on a Workload Identity node
// pool, so block on Kubernetes means a zero-privilege identity: the pod runs
// as the operator's block ServiceAccount (kubernetes_block_service_account
// on the selected profile, then its runtime entry), or, when none is
// configured, as the namespace's default ServiceAccount, deferring to the
// namespace admin for what that account may do. Either way the Kubernetes
// runtime also turns off the API token mount and requires Workload Identity
// nodes. Block never falls back to passthrough.
//
// Like the "assign" mapping, the setting is read only from the broker's own
// global settings and the DB-backed overlay, never from a project's
// settings.yaml: it decides which identity the pod gets.
func (s *Server) resolveKubernetesBlockIdentity(in startContextInputs, isKubernetes bool, gcpMetadataMode, profile string) (kubernetesBlockIdentity, *startContextError) {
	if !isKubernetes || gcpMetadataMode != store.GCPMetadataModeBlock {
		return kubernetesBlockIdentity{}, nil
	}

	sel, err := s.resolveDispatchProfileSelectionWithError(api.StartOptions{
		ProjectPath: in.ProjectPath,
		Profile:     profile,
	})
	if err != nil {
		return kubernetesBlockIdentity{}, &startContextError{
			Status:  http.StatusInternalServerError,
			Message: fmt.Sprintf("loading the project's settings to select the profile and runtime entry for GCP identity mode %q: %v", store.GCPMetadataModeBlock, err),
		}
	}

	vs, _, err := config.LoadGlobalSettingsWithOverlay()
	if err != nil {
		return kubernetesBlockIdentity{}, &startContextError{
			Status:  http.StatusInternalServerError,
			Message: "loading the broker's global settings for the Kubernetes block ServiceAccount: " + err.Error(),
		}
	}
	ksaName, configured := vs.ResolveKubernetesBlockServiceAccountForSelection(sel.ProfileName, sel.RuntimeEntryName)
	if configured {
		// settings.yaml can be hand-edited without the schema validator.
		if err := config.ValidateKubernetesBlockServiceAccount(ksaName); err != nil {
			return kubernetesBlockIdentity{}, &startContextError{Status: http.StatusBadRequest, Message: err.Error()}
		}
	}

	// A request-level serviceAccountName that differs from the block
	// ServiceAccount is refused rather than silently replaced: the caller
	// asked for both no GCP identity and a specific Kubernetes identity.
	// A template-only value is replaced in pkg/agent.
	if explicitKSA, _ := explicitKubernetesIdentity(in); explicitKSA != "" && explicitKSA != ksaName {
		want := fmt.Sprintf("the block ServiceAccount %q", ksaName)
		if !configured {
			want = "the namespace's default ServiceAccount, since no kubernetes_block_service_account is configured"
		}
		return kubernetesBlockIdentity{}, &startContextError{
			Status: http.StatusBadRequest,
			Message: fmt.Sprintf(
				"explicit Kubernetes ServiceAccount %q conflicts with GCP identity mode %q, which runs the pod as %s; remove the explicit serviceAccountName",
				explicitKSA, store.GCPMetadataModeBlock, want),
		}
	}

	return kubernetesBlockIdentity{
		Identity:  &api.KubernetesBlockIdentity{ServiceAccountName: ksaName},
		Selection: sel,
	}, nil
}

// rejectKubernetesBlockRuntimeChange refuses a start or restart whose later,
// authoritative resolution does not match what buildStartContext resolved
// the GCP identity "block" settings for:
//
//   - block settings were resolved for Kubernetes, but the later runtime is
//     not Kubernetes, or the later profile or runtime entry differs from the
//     one the block ServiceAccount was read from;
//   - the mode is "block", but no Kubernetes block settings were resolved
//     (buildStartContext classified a non-Kubernetes runtime) and the later
//     runtime is Kubernetes.
//
// Running either dispatch could start a block pod without the block
// ServiceAccount, the token-mount setting or the node selector, so it is
// refused before any side effect. Never falls back to passthrough.
func rejectKubernetesBlockRuntimeChange(opts api.StartOptions, early *dispatchProfileSelection, resolvedRuntimeType string, lateSelection func() dispatchProfileSelection) *startContextError {
	const remedy = "recreate the agent, or restore the profile or runtime settings it was resolved with"
	lateIsKubernetes := isKubernetesRuntimeName(resolvedRuntimeType)
	var msg string
	switch {
	case opts.KubernetesBlockIdentity != nil:
		var was dispatchProfileSelection
		if early != nil {
			was = *early
		}
		if !lateIsKubernetes {
			msg = fmt.Sprintf(
				"GCP identity mode %q was resolved for the Kubernetes runtime (profile %q, runtime entry %q), but this dispatch now resolves to runtime %q; %s",
				store.GCPMetadataModeBlock, was.ProfileName, was.RuntimeEntryName, resolvedRuntimeType, remedy)
		} else if early != nil {
			if late := lateSelection(); late != was {
				msg = fmt.Sprintf(
					"GCP identity mode %q was resolved for profile %q and runtime entry %q, but this dispatch now resolves to profile %q and runtime entry %q; %s",
					store.GCPMetadataModeBlock, was.ProfileName, was.RuntimeEntryName, late.ProfileName, late.RuntimeEntryName, remedy)
			}
		}
	case opts.Env["SCION_METADATA_MODE"] == store.GCPMetadataModeBlock:
		if lateIsKubernetes {
			msg = fmt.Sprintf(
				"GCP identity mode %q was resolved for a non-Kubernetes runtime, but this dispatch now resolves to the Kubernetes runtime %q; %s",
				store.GCPMetadataModeBlock, resolvedRuntimeType, remedy)
		}
	}
	if msg == "" {
		return nil
	}
	return &startContextError{Status: http.StatusConflict, Message: msg}
}
