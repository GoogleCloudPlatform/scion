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

//go:build !no_sqlite

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
		MappingsReported:      true,
		MappingsComplete:      true,
		MappingsHash:          "h1",
		MappingsReportedAt:    &at,
		MappingsReportVersion: api.BrokerSAReportVersion,
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
		{name: "complete report without a version (4a-only broker) allows", profile: func(p *store.BrokerProfile) { p.MappingsReportVersion = 0 }},
		{name: "complete report at an older version allows", profile: func(p *store.BrokerProfile) { p.MappingsReportVersion = api.BrokerSAReportVersion - 1 }},
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
// agent on that profile with GCP identity assign for gsa. With persist,
// the project and the agent are stored too, so a recorded run would show
// on the row.
func precheckDispatcher(t *testing.T, gsa string, persist bool, mutate ...func(a *store.Agent)) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent, store.Store) {
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
	for _, m := range mutate {
		m(agent)
	}
	if persist {
		require.NoError(t, memStore.CreateProject(ctx, &store.Project{ID: tid("project-1"), Name: "precheck", Slug: "precheck"}))
		require.NoError(t, memStore.CreateAgent(ctx, agent))
	}
	return dispatcher, mockClient, agent, memStore
}

// precheckTokenCounter counts token authorizations and signatures and
// refuses them, so a test can show no token was minted.
type precheckTokenCounter struct {
	authorize, sign int
}

func (g *precheckTokenCounter) AuthorizeAgentToken(context.Context, *store.Agent) (AgentTokenGrant, error) {
	g.authorize++
	return AgentTokenGrant{}, fmt.Errorf("unexpected token authorization")
}

func (g *precheckTokenCounter) SignAgentToken(AgentTokenGrant, string) (string, *store.AgentCredential, error) {
	g.sign++
	return "", nil, fmt.Errorf("unexpected token signature")
}

type precheckOp struct {
	name   string
	run    func(d *HTTPAgentDispatcher, a *store.Agent) error
	called func(m *mockRuntimeBrokerClient) bool
}

func precheckOps(ctx context.Context) []precheckOp {
	return []precheckOp{
		{"create", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreate(ctx, a)
			return err
		}, func(m *mockRuntimeBrokerClient) bool { return m.createCalled }},
		{"create with gather", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchAgentCreateWithGather(ctx, a)
			return err
		}, func(m *mockRuntimeBrokerClient) bool { return m.createCalled }},
		{"finalize env", func(d *HTTPAgentDispatcher, a *store.Agent) error {
			_, err := d.DispatchFinalizeEnv(ctx, a, nil)
			return err
		}, func(m *mockRuntimeBrokerClient) bool { return m.createCalled }},
		{"provision", func(d *HTTPAgentDispatcher, a *store.Agent) error { return d.DispatchAgentProvision(ctx, a) },
			func(m *mockRuntimeBrokerClient) bool { return m.createCalled }},
		{"start", func(d *HTTPAgentDispatcher, a *store.Agent) error { return d.DispatchAgentStart(ctx, a, "task", false) },
			func(m *mockRuntimeBrokerClient) bool { return m.startCalled }},
		{"restart", func(d *HTTPAgentDispatcher, a *store.Agent) error { return d.DispatchAgentRestart(ctx, a) },
			func(m *mockRuntimeBrokerClient) bool { return m.restartCalled }},
	}
}

func TestDispatch_KubernetesIdentityPrecheck(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		t.Run(op.name+" rejects before dispatch", func(t *testing.T) {
			d, m, a, s := precheckDispatcher(t, precheckGSA, true)
			tokens := &precheckTokenCounter{}
			d.SetTokenGenerator(tokens)
			before, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)

			err = op.run(d, a)
			require.Error(t, err)
			ime, ok := identityMappingDispatchError(err)
			require.True(t, ok, "relayed as identity_not_mapped: %v", err)
			assert.Equal(t, ErrCodeIdentityNotMapped, ime.Code)
			assert.Equal(t, identityCheckedByHubReport, ime.Details["checkedBy"])
			assert.True(t, isConfirmedStartNotActedOnError(err), "classified as not acted on")
			assert.False(t, op.called(m), "no broker call")
			assert.Zero(t, tokens.authorize, "no token authorized")
			assert.Zero(t, tokens.sign, "no token signed")
			after, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, before.RunID, after.RunID, "no run recorded")
			assert.Equal(t, before.RunID, a.RunID, "no run minted")
		})
		t.Run(op.name+" dispatches a mapped GSA", func(t *testing.T) {
			d, m, a, _ := precheckDispatcher(t, precheckOther, false)
			err := op.run(d, a)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity, "no identity refusal: %v", err)
			assert.True(t, op.called(m), "broker called")
		})
	}
}

// Start and restart run the placement, standing and launch guards before
// the precheck: an agent held by the standing guard is refused as such,
// not as unmapped.
func TestDispatch_KubernetesIdentityPrecheckAfterGuards(t *testing.T) {
	ctx := context.Background()
	for _, op := range precheckOps(ctx) {
		if op.name != "start" && op.name != "restart" {
			continue
		}
		t.Run(op.name, func(t *testing.T) {
			d, m, a, _ := precheckDispatcher(t, precheckGSA, false)
			d.SetRequiredStandingCheck(func(context.Context, *store.Agent) error { return fmt.Errorf("held") })
			err := op.run(d, a)
			require.ErrorIs(t, err, ErrAgentNotInStanding)
			_, isIdentity := identityMappingDispatchError(err)
			assert.False(t, isIdentity)
			assert.False(t, op.called(m))
		})
	}
}

// The hub's refusal counts as a start that was not acted on, wherever a
// dispatch error is classified: the start claim is released, a
// provisioned agent's failed start is settled, and a reincarnation start
// left no container.
func TestIdentityNotMappedPrecheck_NotActedOn(t *testing.T) {
	err := fmt.Errorf("dispatch start: %w", &identityNotMappedPrecheck{Account: precheckGSA, Profile: "gke", Broker: "broker-a"})
	assert.True(t, isConfirmedStartNotActedOnError(err))
	assert.Equal(t, startReleased, startOutcomeOf(err))
	assert.True(t, startDidNotHappen(err))
	assert.True(t, reincarnationStartLeftNoContainer(err))
}

// A refusal raised on the executing node of a cross-node dispatch is
// carried on the dispatch row and rebuilt as the same typed error.
func TestIdentityNotMappedPrecheck_CrossNodeEnvelope(t *testing.T) {
	pre := &identityNotMappedPrecheck{Account: precheckGSA, Profile: "gke", Broker: "broker-a", Namespace: "agents"}
	result := dispatchFailureResult(fmt.Errorf("dispatch start: %w", pre))
	require.NotEmpty(t, result)
	err := dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: result, Error: "text"})
	var got *identityNotMappedPrecheck
	require.ErrorAs(t, err, &got)
	assert.Equal(t, *pre, *got)
	ime, ok := identityMappingDispatchError(err)
	require.True(t, ok)
	assert.Equal(t, ErrCodeIdentityNotMapped, ime.Code)
	assert.True(t, isConfirmedStartNotActedOnError(err))
}

// Handler-level start under start claims: the hub's refusal is a 400
// identity_not_mapped and releases the claim, so a retry after the
// mapping is fixed is not refused as a start in progress.
func TestStartClaimWiring_IdentityNotMappedReleasesClaim(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()
	broker, err := f.s.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	broker.Profiles = []store.BrokerProfile{precheckProfile(time.Now())}
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, broker))
	// A registered, verified project service account, so the start gate
	// (checkGCPAssignmentAdmissible) admits the assignment.
	now := time.Now()
	require.NoError(t, f.s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
		ID: tid("precheck-sa"), Scope: store.ScopeProject, ScopeID: f.projectID, Email: precheckGSA,
		ProjectID: "example-project", Verified: true, VerifiedAt: now,
		VerificationStatus: store.GCPVerificationVerified, CreatedBy: "test", CreatedAt: now,
	}))
	cur := getAgent(t, f.s, a.ID)
	cur.AppliedConfig.Profile = "gke"
	cur.AppliedConfig.GCPIdentity = precheckAssign(precheckGSA)
	cur.AppliedConfig.GCPIdentity.ServiceAccountID = tid("precheck-sa")
	cur.AppliedConfig.GCPIdentity.ProjectID = "example-project"
	require.NoError(t, f.s.UpdateAgent(ctx, cur))

	precheck := NewHTTPAgentDispatcherWithClient(f.s, &mockRuntimeBrokerClient{}, false, slog.Default())
	starts := 0
	d.start = func(ctx context.Context, a *store.Agent) error {
		starts++
		return precheck.kubernetesIdentityPrecheck(ctx, a)
	}

	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusBadRequest, code, body)
	errCode, details := errorDetails(body)
	assert.Equal(t, ErrCodeIdentityNotMapped, errCode)
	assert.Equal(t, identityCheckedByHubReport, details["checkedBy"])
	got := getAgent(t, f.s, a.ID)
	assert.Empty(t, got.StartClaimID, "the refusal releases the start claim")
	assert.NotEqual(t, store.StartClaimUnconfirmed, got.StartClaimState)

	// The operator maps the GSA; the broker's next report lists it.
	broker, err = f.s.GetRuntimeBroker(ctx, f.brokerID)
	require.NoError(t, err)
	broker.Profiles[0].ServiceAccountMappings = append(broker.Profiles[0].ServiceAccountMappings,
		store.BrokerProfileSAMapping{GSA: precheckGSA, KSA: "agent-ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped})
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, broker))

	code, body = lifecycle(t, f, a.ID, "start")
	assert.NotEqual(t, http.StatusConflict, code, "the retry is not refused as a start in progress: %v", body)
	assert.Less(t, code, 300, body)
	assert.Equal(t, 2, starts)
}

// The report version travels from the heartbeat to the stored profile: a
// complete report with the version can refuse, the same report from a
// broker that predates the version (a 4a-only broker) cannot.
func TestApplyProfileSAMappings_ReportVersionGatesPrecheck(t *testing.T) {
	now := time.Now()
	report := func(version int) brokerProfileSAMappings {
		return brokerProfileSAMappings{
			Name:                   "gke",
			ServiceAccountMappings: []store.BrokerProfileSAMapping{{GSA: precheckOther, KSA: "other-ksa", Namespace: "agents", Source: api.BrokerKSASourceMapped}},
			Complete:               true,
			ReportVersion:          version,
		}
	}
	for _, tc := range []struct {
		name    string
		version int
		reject  bool
	}{
		{"versioned report refuses", api.BrokerSAReportVersion, true},
		{"unversioned report is unknown", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profiles := []store.BrokerProfile{{Name: "gke", Type: "kubernetes"}}
			changed, _ := applyProfileSAMappings(profiles, []brokerProfileSAMappings{report(tc.version)},
				[]brokerProfileSAMappingsHash{{Name: "gke", Hash: "h"}}, now)
			require.True(t, changed)
			assert.Equal(t, tc.version, profiles[0].MappingsReportVersion)
			broker := &store.RuntimeBroker{Name: "broker-a", Profiles: profiles}
			got := kubernetesIdentityNotMapped(broker, "gke", precheckAssign(precheckGSA), now)
			assert.Equal(t, tc.reject, got != nil)

			// The same entries at another version are a different report.
			assert.True(t, profileSAReportEqual(&profiles[0], report(tc.version), "h"))
			assert.False(t, profileSAReportEqual(&profiles[0], report(tc.version+1), "h"))
		})
	}
}
