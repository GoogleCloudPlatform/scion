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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	precheckGSA   = "agent@example-project.iam.gserviceaccount.com"
	precheckOther = "other@example-project.iam.gserviceaccount.com"
)

// precheckProfile is a Kubernetes profile with a recent, complete report
// that maps only precheckOther.
func precheckProfile(now time.Time) store.BrokerProfile {
	at := now.Add(-time.Minute)
	return store.BrokerProfile{
		Name: "gke",
		Type: "kubernetes",
		ServiceAccountMappings: []store.BrokerProfileSAMapping{
			{GSA: precheckOther, KSA: "other-ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped},
		},
		MappingsReported:   true,
		MappingsComplete:   true,
		MappingsHash:       "h1",
		MappingsReportedAt: &at,
	}
}

func precheckAssign(email string) *store.GCPIdentityConfig {
	return &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountEmail: email}
}

func TestKubernetesIdentityNotMapped(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		profile   func(p *store.BrokerProfile)
		selection string
		gcpID     *store.GCPIdentityConfig
		reject    bool
	}{
		{name: "recent complete report without the GSA rejects", reject: true},
		{name: "k8s alias type rejects", profile: func(p *store.BrokerProfile) { p.Type = "k8s" }, reject: true},
		{name: "report just inside the threshold rejects", profile: func(p *store.BrokerProfile) {
			at := now.Add(-profileSAReportFreshFor)
			p.MappingsReportedAt = &at
		}, reject: true},
		{name: "GSA present allows", profile: func(p *store.BrokerProfile) {
			p.ServiceAccountMappings = append(p.ServiceAccountMappings, store.BrokerProfileSAMapping{GSA: precheckGSA, KSA: "a", Namespace: "agents", Source: api.BrokerKSASourceDiscovered})
		}},
		{name: "GSA present in another case allows", gcpID: precheckAssign("Agent@Example-Project.iam.gserviceaccount.com"), profile: func(p *store.BrokerProfile) {
			p.ServiceAccountMappings = append(p.ServiceAccountMappings, store.BrokerProfileSAMapping{GSA: precheckGSA, KSA: "a"})
		}},
		{name: "ambiguous GSA allows", profile: func(p *store.BrokerProfile) { p.AmbiguousGSAs = []string{precheckGSA} }},
		{name: "stale report allows", profile: func(p *store.BrokerProfile) {
			at := now.Add(-profileSAReportFreshFor - time.Second)
			p.MappingsReportedAt = &at
		}},
		{name: "nil reported-at allows", profile: func(p *store.BrokerProfile) { p.MappingsReportedAt = nil }},
		{name: "incomplete report allows", profile: func(p *store.BrokerProfile) {
			p.MappingsComplete = false
			p.MappingsIncompleteReason = api.BrokerKSADiscoveryListFailed
		}},
		{name: "incomplete under ForceRuntime allows", profile: func(p *store.BrokerProfile) {
			p.MappingsComplete = false
			p.MappingsIncompleteReason = api.BrokerSAReportForceRuntime
		}},
		{name: "no report (older broker) allows", profile: func(p *store.BrokerProfile) {
			p.MappingsReported, p.MappingsComplete, p.MappingsReportedAt, p.ServiceAccountMappings = false, false, nil, nil
		}},
		{name: "non-Kubernetes profile with an old complete report allows", profile: func(p *store.BrokerProfile) { p.Type = "docker" }},
		{name: "no GSA allows", gcpID: precheckAssign("")},
		{name: "mode not assign allows", gcpID: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock, ServiceAccountEmail: precheckGSA}},
		{name: "no GCP identity allows", gcpID: nil},
		{name: "agent without profile allows", selection: "-"},
		{name: "profile not on the broker allows", selection: "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := precheckProfile(now)
			if tt.profile != nil {
				tt.profile(&p)
			}
			broker := &store.RuntimeBroker{ID: "b1", Name: "broker-a", Profiles: []store.BrokerProfile{p}}
			gcpID := precheckAssign(precheckGSA)
			if tt.gcpID != nil || tt.name == "no GCP identity allows" {
				gcpID = tt.gcpID
			}
			sel := "gke"
			switch tt.selection {
			case "-":
				sel = ""
			case "":
			default:
				sel = tt.selection
			}
			got := kubernetesIdentityNotMapped(broker, sel, gcpID, now)
			if !tt.reject {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, identityNotMappedPrecheck{Account: precheckGSA, Profile: "gke", Broker: "broker-a", Namespace: "agents"}, *got)
		})
	}
}

// The hub's refusal is relayed like the broker's: HTTP 400,
// identity_not_mapped, the names and the fix, checkedBy=hub_report.
func TestIdentityNotMappedPrecheck_Relay(t *testing.T) {
	err := fmt.Errorf("dispatch create: %w", &identityNotMappedPrecheck{Account: precheckGSA, Profile: "gke", Broker: "broker-a", Namespace: "agents"})
	rec := httptest.NewRecorder()
	require.True(t, relayIdentityMappingError(rec, err))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Message string                 `json:"message"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, ErrCodeIdentityNotMapped, body.Error.Code)
	msg := body.Error.Message
	assert.Contains(t, msg, `GCP service account "`+precheckGSA+`" is not mapped on profile "gke" of broker "broker-a"`)
	assert.Contains(t, msg, "kubernetes_service_account_mappings")
	assert.Contains(t, msg, "iam.gke.io/gcp-service-account="+precheckGSA)
	assert.Contains(t, msg, `namespace "agents"`)
	assert.Contains(t, msg, kubernetesIdentityMappingDocsURL)
	assert.NotContains(t, msg, "ready")
	assert.Equal(t, identityCheckedByHubReport, body.Error.Details["checkedBy"])
	assert.Equal(t, precheckGSA, body.Error.Details[api.BrokerErrDetailServiceAccount])
	assert.Equal(t, "gke", body.Error.Details[api.BrokerErrDetailProfile])
	assert.Equal(t, "broker-a", body.Error.Details[api.BrokerErrDetailBroker])
	assert.Equal(t, "agents", body.Error.Details[api.BrokerErrDetailNamespace])

	text := dispatchFailureText(err)
	assert.Contains(t, text, ErrCodeIdentityNotMapped+": ")
	assert.Contains(t, text, "is not mapped on profile")
}

// precheckDispatcher stores a broker whose "gke" profile has a recent,
// complete report, and returns a dispatcher with a mock client and an
// agent on that profile with GCP identity assign for gsa.
func precheckDispatcher(t *testing.T, gsa string) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID:       tid("host-1"),
		Name:     "broker-a",
		Slug:     "broker-a",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{precheckProfile(time.Now())},
	}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))
	mockClient := &mockRuntimeBrokerClient{}
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, mockClient, false, slog.Default())
	agent := &store.Agent{
		ID:              tid("agent-1"),
		Name:            "test-agent",
		Slug:            "test-agent",
		ProjectID:       tid("project-1"),
		RuntimeBrokerID: tid("host-1"),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			Profile:       "gke",
			GCPIdentity:   precheckAssign(gsa),
		},
	}
	return dispatcher, mockClient, agent
}

func TestDispatch_KubernetesIdentityPrecheck(t *testing.T) {
	ctx := context.Background()
	ops := []struct {
		name   string
		run    func(d *HTTPAgentDispatcher, a *store.Agent) error
		called func(m *mockRuntimeBrokerClient) bool
	}{
		{"create", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreate(ctx, a)
			return err
		},
			func(m *mockRuntimeBrokerClient) bool { return m.createCalled }},
		{"create with gather", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreateWithGather(ctx, a)
			return err
		}, func(m *mockRuntimeBrokerClient) bool { return m.createCalled }},
		{"provision", func(d *HTTPAgentDispatcher, a *store.Agent) error { return d.DispatchAgentProvision(ctx, a) },
			func(m *mockRuntimeBrokerClient) bool { return m.createCalled }},
		{"start", func(d *HTTPAgentDispatcher, a *store.Agent) error { return d.DispatchAgentStart(ctx, a, "task", false) },
			func(m *mockRuntimeBrokerClient) bool { return m.startCalled }},
		{"restart", func(d *HTTPAgentDispatcher, a *store.Agent) error { return d.DispatchAgentRestart(ctx, a) },
			func(m *mockRuntimeBrokerClient) bool { return m.restartCalled }},
	}
	for _, op := range ops {
		t.Run(op.name+" rejects before dispatch", func(t *testing.T) {
			d, m, a := precheckDispatcher(t, precheckGSA)
			err := op.run(d, a)
			require.Error(t, err)
			ime, ok := identityMappingDispatchError(err)
			require.True(t, ok, "relayed as identity_not_mapped: %v", err)
			assert.Equal(t, ErrCodeIdentityNotMapped, ime.Code)
			assert.Equal(t, identityCheckedByHubReport, ime.Details["checkedBy"])
			assert.False(t, op.called(m), "no broker call")
		})
		t.Run(op.name+" dispatches a mapped GSA", func(t *testing.T) {
			d, m, a := precheckDispatcher(t, precheckOther)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no identity refusal: %v", err)
			assert.True(t, op.called(m), "broker called")
		})
	}
}
