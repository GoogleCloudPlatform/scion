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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestNormalizeWorkloadIdentityTargets_KSACreation(t *testing.T) {
	in := []store.WorkloadIdentityTarget{
		{Profile: "both", Pool: "p-project.svc.id.goog", Namespace: "agents", KSACreation: true},
		{Profile: "create-only", Pool: "", Namespace: "agents", KSACreation: true},
		{Profile: "empty-both", Pool: "", Namespace: "agents", KSACreation: false},
		{Profile: "bad-pool", Pool: "not-a-pool", Namespace: "agents", KSACreation: true},
	}
	got, drops := normalizeWorkloadIdentityTargets(in, false)
	assert.Equal(t, workloadIdentityTargetDrops{invalid: 1}, drops)
	require.Equal(t, []store.WorkloadIdentityTarget{
		{Profile: "both", Pool: "p-project.svc.id.goog", Namespace: "agents", KSACreation: true},
		{Profile: "create-only", Pool: "", Namespace: "agents", KSACreation: true},
	}, got)

	// A ksaCreation-only target (Pool == "") is still reported as missing a
	// Workload Identity pool at mint.
	broker := &store.RuntimeBroker{
		ID:                      "b1",
		Name:                    "broker-a",
		Capabilities:            &store.BrokerCapabilities{WITargetsV1: true},
		WorkloadIdentityTargets: []store.WorkloadIdentityTarget{{Profile: "create-only", Pool: "", Namespace: "agents", KSACreation: true}},
		Profiles:                []store.BrokerProfile{{Name: "create-only", Type: "kubernetes"}},
	}
	plan := workloadIdentityMintPlanFor([]*store.RuntimeBroker{broker})
	assert.Empty(t, plan.targets)
	require.Len(t, plan.missing, 1)
	assert.Equal(t, []string{"broker-a/create-only"}, plan.missing[0].profiles)
}

func TestWithDispatchCreateKSA_WireCompatible(t *testing.T) {
	assert.Nil(t, withDispatchCreateKSA(nil, false), "false must leave empty wire defaults nil for file-mode parity")

	hd := withDispatchCreateKSA(nil, true)
	require.NotNil(t, hd)
	assert.True(t, hd.CreateKSA)

	blob, err := json.Marshal(&RemoteAgentConfig{HubAgentDefaults: hd})
	require.NoError(t, err)
	assert.Contains(t, string(blob), `"createKSA":true`)

	var decoded runtimebroker.CreateAgentConfig
	require.NoError(t, json.Unmarshal(blob, &decoded))
	require.NotNil(t, decoded.HubAgentDefaults)
	assert.True(t, decoded.HubAgentDefaults.CreateKSA)
}

func ksaCreatableTestBroker(now time.Time, ksaCreation bool, mappings ...store.BrokerProfileSAMapping) *store.RuntimeBroker {
	reportedAt := now.Add(-time.Minute)
	var wiTargets []store.WorkloadIdentityTarget
	if ksaCreation {
		wiTargets = []store.WorkloadIdentityTarget{{
			Profile:     "gke",
			Pool:        "cluster-project.svc.id.goog",
			Namespace:   "wi-agents",
			KSACreation: true,
		}}
	}
	return &store.RuntimeBroker{
		ID:                      "b1",
		Name:                    "broker-a",
		LastHeartbeat:           reportedAt,
		Capabilities:            &store.BrokerCapabilities{WITargetsV1: true},
		WorkloadIdentityTargets: wiTargets,
		Profiles: []store.BrokerProfile{{
			Name:                   "gke",
			Type:                   "kubernetes",
			MappingsReported:       true,
			MappingsComplete:       true,
			MappingsReportVersion:  api.BrokerSAReportVersion,
			MappingsReportedAt:     &reportedAt,
			ServiceAccountMappings: mappings,
		}},
	}
}

func TestProfileSAMappingDecision_Creatable(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const gsa = "scion-worker@hub-proj.iam.gserviceaccount.com"

	t.Run("creatable when complete, fresh, ksaCreation true, and allowCreate true", func(t *testing.T) {
		b := ksaCreatableTestBroker(now, true)
		d := profileSAMappingDecisionWithCreate(b, "gke", gsa, true, now)
		assert.Equal(t, SAMappingStateCreatable, d.State)
		assert.Equal(t, "wi-agents", d.Namespace)
		assert.Nil(t, kubernetesIdentityNotMappedWithCreate(b, "gke", &store.GCPIdentityConfig{
			MetadataMode:        store.GCPMetadataModeAssign,
			ServiceAccountEmail: gsa,
		}, true, now))
	})

	t.Run("not_mapped when allowCreate is false (experiment off or BYO GSA)", func(t *testing.T) {
		b := ksaCreatableTestBroker(now, true)
		d := profileSAMappingDecisionWithCreate(b, "gke", gsa, false, now)
		assert.Equal(t, SAMappingStateNotMapped, d.State)
		assert.NotNil(t, kubernetesIdentityNotMappedWithCreate(b, "gke", &store.GCPIdentityConfig{
			MetadataMode:        store.GCPMetadataModeAssign,
			ServiceAccountEmail: gsa,
		}, false, now))
	})

	t.Run("not_mapped when broker ksaCreation is false", func(t *testing.T) {
		b := ksaCreatableTestBroker(now, false)
		d := profileSAMappingDecisionWithCreate(b, "gke", gsa, true, now)
		assert.Equal(t, SAMappingStateNotMapped, d.State)
	})

	t.Run("mapped wins over creatable when GSA is already in ServiceAccountMappings", func(t *testing.T) {
		b := ksaCreatableTestBroker(now, true, store.BrokerProfileSAMapping{
			GSA: gsa, KSA: "existing-ksa", Namespace: "wi-agents", Source: api.BrokerKSASourceDiscovered,
		})
		d := profileSAMappingDecisionWithCreate(b, "gke", gsa, true, now)
		assert.Equal(t, SAMappingStateMapped, d.State)
	})

	t.Run("unknown stays unknown even when ksaCreation and allowCreate are true", func(t *testing.T) {
		b := ksaCreatableTestBroker(now, true)
		b.Profiles[0].MappingsComplete = false
		d := profileSAMappingDecisionWithCreate(b, "gke", gsa, true, now)
		assert.Equal(t, SAMappingStateUnknown, d.State)
		assert.Equal(t, SAMappingReasonReportIncomplete, d.Reason)
	})
}

func TestAssignStatusAndStatusViewAndWarnings_Creatable(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	const mintedGSA = "scion-minted@hub-proj.iam.gserviceaccount.com"
	const byoGSA = "byo@other-proj.iam.gserviceaccount.com"

	b := ksaCreatableTestBroker(now, true)
	ab := assignStatusBroker{broker: b}

	mintedAssign := assignStatusForWithCreate(ab, "gke", mintedGSA, true, now)
	assert.Equal(t, SAMappingStateCreatable, mintedAssign.State)
	assert.Equal(t, "wi-agents", mintedAssign.Namespace)
	assert.Contains(t, mintedAssign.Message, "KSA created at start:")

	byoAssign := assignStatusForWithCreate(ab, "gke", byoGSA, false, now)
	assert.Equal(t, SAMappingStateNotMapped, byoAssign.State)

	srv := &Server{}
	rows := srv.brokerKubernetesProfileMappings([]*store.RuntimeBroker{b})
	require.Len(t, rows, 1)

	mappings := gcpSAProfileMappingsFor(mintedGSA, true, rows, now)
	require.Len(t, mappings, 1)
	assert.Equal(t, GCPSAMappingCreatable, mappings[0].State)
	assert.Equal(t, "scion-minted", mappings[0].KubernetesServiceAccount)
	assert.Equal(t, "wi-agents", mappings[0].Namespace)
	assert.Equal(t, api.BrokerKSASourceCreated, mappings[0].Source)

	step := gcpSANextStep(true, mappings, []GCPServiceAccountDefault{{Kind: GCPSADefaultProfile, Profile: "gke"}})
	assert.Equal(t, GCPSANextStepNone, step.Code, "creatable profile must not report a missing mapping nextStep")

	mappedCount, reportedCount, _ := gcpSAMappedCountFor(mintedGSA, true, rows, now)
	assert.Equal(t, 1, mappedCount)
	assert.Equal(t, 1, reportedCount)

	view := projectSAMappingViewFrom(rows)
	mintedSA := &store.GCPServiceAccount{Scope: store.ScopeProject, ScopeID: "p1", Email: mintedGSA, Managed: true}
	byoSA := &store.GCPServiceAccount{Scope: store.ScopeProject, ScopeID: "p1", Email: byoGSA, Managed: false}

	warnsOn := view.warningsAtWithExperiment([]*store.GCPServiceAccount{mintedSA, byoSA}, true, now)
	require.Len(t, warnsOn, 1, "only BYO GSA should warn when experiment is on and profile has ksaCreation")
	assert.Contains(t, warnsOn[0], byoGSA)

	warnsOff := view.warningsAtWithExperiment([]*store.GCPServiceAccount{mintedSA, byoSA}, false, now)
	assert.Len(t, warnsOff, 2, "both GSAs warn when experiment is off")
}

func TestStatusView_CreatableProfile_WorkloadIdentityBoundAndNotBound(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	withWIBindingClock(t, now)

	const (
		mintedGSA      = "scion-minted@hub-proj.iam.gserviceaccount.com"
		expectedMember = "serviceAccount:cluster-project.svc.id.goog[wi-agents/scion-minted]"
	)

	b := ksaCreatableTestBroker(now, true)
	ab := assignStatusBroker{broker: b}
	sa := &store.GCPServiceAccount{
		ID:                 "sa-minted",
		Scope:              store.ScopeProject,
		ScopeID:            "proj-1",
		Email:              mintedGSA,
		ProjectID:          "hub-proj",
		Verified:           true,
		VerificationStatus: store.GCPVerificationVerified,
		Managed:            true,
	}
	defaults := []GCPServiceAccountDefault{{Kind: GCPSADefaultProfile, Profile: "gke"}}

	t.Run("creatable profile with Workload Identity binding bound checks conventional KSA and nextStep is none", func(t *testing.T) {
		admin := &wiReadStubAdmin{
			membersFn: func(int, string, string) ([]string, error) {
				return []string{expectedMember}, nil
			},
		}
		srv := &Server{gcpIAMAdmin: admin}
		rows := srv.brokerKubernetesProfileMappings([]*store.RuntimeBroker{b})
		mappings := gcpSAProfileMappingsFor(mintedGSA, true, rows, now)
		binding := srv.evaluateWorkloadIdentityStatus(context.Background(), sa, []*store.RuntimeBroker{b}, mappings, now)
		step := gcpSANextStepForBinding(true, mappings, defaults, binding)

		assign := assignStatusForWithCreate(ab, "gke", mintedGSA, true, now)
		assert.Equal(t, SAMappingStateCreatable, assign.State)
		assert.Contains(t, assign.Message, "KSA created at start:")

		require.Len(t, mappings, 1)
		assert.Equal(t, GCPSAMappingCreatable, mappings[0].State)
		assert.Equal(t, "scion-minted", mappings[0].KubernetesServiceAccount)
		assert.Equal(t, "wi-agents", mappings[0].Namespace)
		assert.Equal(t, api.BrokerKSASourceCreated, mappings[0].Source)
		require.NotNil(t, mappings[0].WorkloadIdentityBinding)
		assert.Equal(t, GCPSABindingBound, mappings[0].WorkloadIdentityBinding.State)
		assert.Equal(t, expectedMember, mappings[0].WorkloadIdentityBinding.Member)
		assert.Empty(t, mappings[0].WorkloadIdentityBinding.ManualStep)

		assert.Equal(t, GCPSABindingBound, binding.State)
		require.Len(t, binding.Targets, 1)
		assert.Equal(t, "scion-minted", binding.Targets[0].KSA)
		assert.Equal(t, expectedMember, binding.Targets[0].Member)
		assert.Equal(t, GCPSABindingBound, binding.Targets[0].State)

		assert.Equal(t, GCPSANextStepNone, step.Code)
		assert.Equal(t, "Nothing missing that the hub can check.", step.Message)
	})

	t.Run("creatable profile with Workload Identity binding not_bound keeps KSA created at start and returns not_bound grant step", func(t *testing.T) {
		admin := &wiReadStubAdmin{
			membersFn: func(int, string, string) ([]string, error) {
				return nil, nil
			},
		}
		srv := &Server{gcpIAMAdmin: admin}
		rows := srv.brokerKubernetesProfileMappings([]*store.RuntimeBroker{b})
		mappings := gcpSAProfileMappingsFor(mintedGSA, true, rows, now)
		binding := srv.evaluateWorkloadIdentityStatus(context.Background(), sa, []*store.RuntimeBroker{b}, mappings, now)
		step := gcpSANextStepForBinding(true, mappings, defaults, binding)

		assign := assignStatusForWithCreate(ab, "gke", mintedGSA, true, now)
		assert.Equal(t, SAMappingStateCreatable, assign.State)
		assert.Contains(t, assign.Message, "KSA created at start:")

		require.Len(t, mappings, 1)
		assert.Equal(t, GCPSAMappingCreatable, mappings[0].State)
		assert.Equal(t, "scion-minted", mappings[0].KubernetesServiceAccount)
		require.NotNil(t, mappings[0].WorkloadIdentityBinding)
		assert.Equal(t, GCPSABindingNotBound, mappings[0].WorkloadIdentityBinding.State)
		assert.Equal(t, expectedMember, mappings[0].WorkloadIdentityBinding.Member)
		assert.Contains(t, mappings[0].WorkloadIdentityBinding.ManualStep, expectedMember)

		assert.Equal(t, GCPSABindingNotBound, binding.State)
		require.Len(t, binding.Targets, 1)
		assert.Equal(t, "scion-minted", binding.Targets[0].KSA)
		assert.Equal(t, expectedMember, binding.Targets[0].Member)
		assert.Equal(t, GCPSABindingNotBound, binding.Targets[0].State)
		assert.Contains(t, binding.Targets[0].ManualStep, expectedMember)

		assert.Equal(t, GCPSANextStepNotBound, step.Code)
		assert.Contains(t, step.Message, expectedMember)
		assert.Contains(t, step.Message, "gcloud iam service-accounts add-iam-policy-binding")
	})
}

func TestIdentityMappingDispatchError_KSAConflictAndPermissions(t *testing.T) {
	t.Run("identity_ksa_conflict relays 400 with conflicting KSA and namespace and no raw broker text", func(t *testing.T) {
		details := map[string]any{
			api.BrokerErrDetailServiceAccount: "scion-worker@hub-proj.iam.gserviceaccount.com",
			api.BrokerErrDetailProfile:        "gke",
			api.BrokerErrDetailRuntimeEntry:   "k8s",
			api.BrokerErrDetailBroker:         "broker-a",
			api.BrokerErrDetailConflictingKSA: "scion-worker",
			api.BrokerErrDetailNamespace:      "agents",
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]any{
			"code":    ErrCodeIdentityKSAConflict,
			"message": "raw broker operator text must not leak",
			"details": details,
		}})
		err := &brokerStatusError{StatusCode: http.StatusBadRequest, Body: string(body)}

		rec := httptest.NewRecorder()
		require.True(t, relayIdentityMappingError(rec, err))
		require.Equal(t, http.StatusBadRequest, rec.Code)

		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assert.Equal(t, ErrCodeIdentityKSAConflict, resp.Error.Code)
		assert.Contains(t, resp.Error.Message, `"scion-worker"`)
		assert.Contains(t, resp.Error.Message, `namespace "agents"`)
		assert.Contains(t, resp.Error.Message, "kubernetes_service_account_mappings")
		assert.NotContains(t, resp.Error.Message, "raw broker operator text")
		assert.Equal(t, "scion-worker", resp.Error.Details[api.BrokerErrDetailConflictingKSA])
		assert.Equal(t, "agents", resp.Error.Details[api.BrokerErrDetailNamespace])
	})

	t.Run("identity_not_mapped with missingPermission preserves detail and names RBAC permission", func(t *testing.T) {
		for _, perm := range []string{api.BrokerKSAPermissionCreate, api.BrokerKSAPermissionGet} {
			details := map[string]any{
				api.BrokerErrDetailServiceAccount:    "scion-worker@hub-proj.iam.gserviceaccount.com",
				api.BrokerErrDetailProfile:           "gke",
				api.BrokerErrDetailBroker:            "broker-a",
				api.BrokerErrDetailDiscovery:         api.BrokerKSADiscoveryNoMatch,
				api.BrokerErrDetailNamespace:         "agents",
				api.BrokerErrDetailMissingPermission: perm,
			}
			body, _ := json.Marshal(map[string]any{"error": map[string]any{
				"code":    ErrCodeIdentityNotMapped,
				"message": "raw apiserver forbidden text",
				"details": details,
			}})
			err := &brokerStatusError{StatusCode: http.StatusBadRequest, Body: string(body)}

			ime, ok := identityMappingDispatchError(err)
			require.True(t, ok)
			assert.Equal(t, ErrCodeIdentityNotMapped, ime.Code)
			assert.Equal(t, perm, ime.Details[api.BrokerErrDetailMissingPermission])
			assert.Contains(t, ime.Message, perm)
			assert.Contains(t, ime.Message, `namespace "agents"`)
			assert.NotContains(t, ime.Message, "raw apiserver forbidden text")
		}
	})

	t.Run("identity_ksa_mismatch with created source", func(t *testing.T) {
		details := map[string]any{
			api.BrokerErrDetailServiceAccount: "scion-worker@hub-proj.iam.gserviceaccount.com",
			api.BrokerErrDetailProfile:        "gke",
			api.BrokerErrDetailBroker:         "broker-a",
			api.BrokerErrDetailRequestedKSA:   "custom-ksa",
			api.BrokerErrDetailMappedKSA:      "scion-worker",
			api.BrokerErrDetailKSASource:      api.BrokerKSASourceCreated,
			api.BrokerErrDetailNamespace:      "agents",
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]any{
			"code":    ErrCodeIdentityKSAMismatch,
			"message": "broker operator text",
			"details": details,
		}})
		err := &brokerStatusError{StatusCode: http.StatusBadRequest, Body: string(body)}
		ime, ok := identityMappingDispatchError(err)
		require.True(t, ok)
		assert.Equal(t, ErrCodeIdentityKSAMismatch, ime.Code)
		assert.Equal(t, api.BrokerKSASourceCreated, ime.Details[api.BrokerErrDetailKSASource])
		assert.Contains(t, ime.Message, `"scion-worker"`)
		assert.Contains(t, ime.Message, "conventional")
	})
}
