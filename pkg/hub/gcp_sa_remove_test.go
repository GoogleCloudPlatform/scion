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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Removing a GCP service account: impact report, in-use refusal, ?force=true
// and audit events (ptone/scion#4022).

// recordingSAAuditLogger records registry audit events. It embeds the plain
// logger so the base AuditLogger interface is satisfied unchanged.
type recordingSAAuditLogger struct {
	plainAuditLogger
	mu     sync.Mutex
	events []*GCPServiceAccountAuditEvent
}

func (l *recordingSAAuditLogger) LogGCPServiceAccountEvent(_ context.Context, e *GCPServiceAccountAuditEvent) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}

func (l *recordingSAAuditLogger) ofType(t GCPServiceAccountAuditEventType) []*GCPServiceAccountAuditEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []*GCPServiceAccountAuditEvent
	for _, e := range l.events {
		if e.EventType == t {
			out = append(out, e)
		}
	}
	return out
}

func saRemovePath(projectID, saID string) string {
	return fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts/%s", projectID, saID)
}

func setSARemoveProjectAnnotations(t *testing.T, s store.Store, projectID string, kv map[string]string) {
	t.Helper()
	ctx := context.Background()
	p, err := s.GetProject(ctx, projectID)
	require.NoError(t, err)
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	for k, v := range kv {
		p.Annotations[k] = v
	}
	require.NoError(t, s.UpdateProject(ctx, p))
}

func decodeInUseImpact(t *testing.T, body []byte) (string, string, GCPServiceAccountImpact) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string                     `json:"code"`
			Message string                     `json:"message"`
			Details map[string]json.RawMessage `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp))
	var impact GCPServiceAccountImpact
	require.NoError(t, json.Unmarshal(resp.Error.Details["impact"], &impact))
	return resp.Error.Code, resp.Error.Message, impact
}

func TestGCPSARemove_RefusedWhileProjectDefaultsReferenceIt(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	audit := &recordingSAAuditLogger{}
	srv.SetAuditLogger(audit)
	sa := mkSA(t, s, "sa-rm-refused", "rm-refused@example.com", store.ScopeProject, project.ID, owner.ID)
	setSARemoveProjectAnnotations(t, s, project.ID, map[string]string{
		projectSettingDefaultGCPIdentityMode:          store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID:          sa.ID,
		projectSettingDefaultGCPIdentitySAIDByProfile: fmt.Sprintf(`{"k8s":%q}`, sa.ID),
	})

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, saRemovePath(project.ID, sa.ID), nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	code, msg, impact := decodeInUseImpact(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeSAInUse, code)
	assert.Contains(t, msg, "force")
	assert.NotContains(t, msg, sa.Email, "the refusal names defaults, not the account email")
	assert.Equal(t, []GCPServiceAccountImpactDefault{
		{Tier: GCPSADefaultTierProject, ProjectID: project.ID, Clearable: true},
		{Tier: GCPSADefaultTierProfile, ProjectID: project.ID, Profile: "k8s", Clearable: true},
	}, impact.Defaults)
	assert.NotEmpty(t, impact.ManualCleanup)

	assert.True(t, saExists(t, s, sa.ID), "a refused delete must leave the account registered")

	events := audit.ofType(GCPSAAuditDelete)
	require.Len(t, events, 1)
	assert.False(t, events[0].Success)
	assert.Equal(t, gcpSAAuditOutcomeRefusedInUse, events[0].Outcome)
}

func TestGCPSARemove_ForceClearsDefaultsThenDeletes(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	audit := &recordingSAAuditLogger{}
	srv.SetAuditLogger(audit)
	sa := mkSA(t, s, "sa-rm-force", "rm-force@example.com", store.ScopeProject, project.ID, owner.ID)
	other := mkSA(t, s, "sa-rm-force-other", "rm-force-other@example.com", store.ScopeProject, project.ID, owner.ID)
	setSARemoveProjectAnnotations(t, s, project.ID, map[string]string{
		projectSettingDefaultGCPIdentityMode:          store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID:          sa.ID,
		projectSettingDefaultGCPIdentitySAIDByProfile: fmt.Sprintf(`{"k8s":%q,"gke":%q}`, sa.ID, other.ID),
		projectSettingDefaultTemplate:                 "unrelated-template",
	})
	evSpy := &projectUpdatedSpy{}
	srv.SetEventPublisher(evSpy)

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, saRemovePath(project.ID, sa.ID)+"?force=true", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp DeleteGCPServiceAccountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.Deleted)
	assert.Len(t, resp.ClearedDefaults, 2)
	assert.NotEmpty(t, resp.Impact.ManualCleanup)

	assert.False(t, saExists(t, s, sa.ID))

	p, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	_, hasSA := p.Annotations[projectSettingDefaultGCPIdentitySAID]
	assert.False(t, hasSA, "the project default must no longer name the account")
	assert.Equal(t, store.GCPMetadataModeBlock, p.Annotations[projectSettingDefaultGCPIdentityMode],
		"a cleared assign default becomes block, never unset (which could fall through to a broader default)")
	assert.Equal(t, map[string]string{"gke": other.ID}, profileDefaultSAIDsFromAnnotations(p.Annotations),
		"only the per-profile entries naming the account are removed")
	assert.Equal(t, "unrelated-template", p.Annotations[projectSettingDefaultTemplate],
		"a forced clear keeps every unrelated project setting (#3942)")
	assert.Equal(t, []string{project.ID}, evSpy.updated(), "a forced clear publishes the project-updated event")

	events := audit.ofType(GCPSAAuditDelete)
	require.Len(t, events, 1)
	assert.True(t, events[0].Success)
	assert.Equal(t, "true", events[0].Details["force"])
	assert.Equal(t, "2", events[0].Details["defaults_cleared"])
}

func TestGCPSARemove_AgentsDoNotBlock(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()
	sa := mkSA(t, s, "sa-rm-agents", "rm-agents@example.com", store.ScopeProject, project.ID, owner.ID)
	agent := &store.Agent{
		ID: tid("agent-rm-ref"), Slug: "agent-rm-ref", Name: "agent-rm-ref",
		ProjectID: project.ID, CreatedBy: owner.ID, OwnerID: owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID, ServiceAccountEmail: sa.Email,
		}},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("agent-rm-unrelated"), Slug: "agent-rm-unrelated", Name: "agent-rm-unrelated",
		ProjectID: project.ID, CreatedBy: owner.ID, OwnerID: owner.ID,
	}))

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, saRemovePath(project.ID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "agents referencing the account must not block removal: %s", rec.Body.String())

	var resp DeleteGCPServiceAccountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Impact.AgentCount)
	assert.Equal(t, 1, resp.Impact.VisibleAgentCount)
	assert.Empty(t, resp.Impact.HiddenAgentCounts)
	require.Len(t, resp.Impact.Agents, 1)
	assert.Equal(t, agent.ID, resp.Impact.Agents[0].ID)
	assert.Empty(t, resp.ClearedDefaults)

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, sa.ID, got.AppliedConfig.GCPIdentity.ServiceAccountID,
		"the agent keeps its reference and fails at next start")
}

func TestGCPSARemove_HubDefaultBlocksEvenWithForce(t *testing.T) {
	// The caller is the project owner and the account's creator, so the
	// project default is clearable and visible; only the hub default blocks.
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	sa := mkSA(t, s, "sa-rm-hubdef", "rm-hubdef@example.com", store.ScopeHub, "hub-instance-1", owner.ID)
	setSARemoveProjectAnnotations(t, s, project.ID, map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID: sa.ID,
	})
	srv.mu.Lock()
	srv.config.AgentDefaults.DefaultGCPIdentityMode = store.GCPMetadataModeAssign
	srv.config.AgentDefaults.DefaultGCPIdentityServiceAccountID = sa.ID
	srv.mu.Unlock()

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, flatSAPath+sa.ID+"?force=true", nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	code, msg, impact := decodeInUseImpact(t, rec.Body.Bytes())
	assert.Equal(t, ErrCodeSAInUse, code)
	assert.Contains(t, msg, "hub admin")
	assert.Zero(t, impact.HiddenDefaultCount)
	assert.Contains(t, impact.Defaults, GCPServiceAccountImpactDefault{Tier: GCPSADefaultTierHub, Clearable: false})
	assert.Contains(t, impact.Defaults, GCPServiceAccountImpactDefault{
		Tier: GCPSADefaultTierProject, ProjectID: project.ID, Clearable: true,
	}, "a hub-scoped account's report covers every project's defaults")

	assert.True(t, saExists(t, s, sa.ID))
	p, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, sa.ID, p.Annotations[projectSettingDefaultGCPIdentitySAID],
		"a refused forced delete clears nothing")
}

func TestGCPSARemove_ReportsBrokerMappingsAndManagedRetention(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()
	sa := mkSA(t, s, "sa-rm-mapped", "rm-mapped@example.com", store.ScopeProject, project.ID, owner.ID)
	sa.Managed = true
	require.NoError(t, s.UpdateGCPServiceAccount(ctx, sa))

	broker := &store.RuntimeBroker{
		ID: tid("broker-rm-mapped"), Name: "rm-broker", Slug: "rm-broker", Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "k8s", Type: "kubernetes", MappingsReported: true,
				ServiceAccountMappings: []store.BrokerProfileSAMapping{{GSA: strings.ToUpper(sa.Email)}}},
			{Name: "local", Type: "docker"},
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: "online",
	}))

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, saRemovePath(project.ID, sa.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp DeleteGCPServiceAccountResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []GCPServiceAccountImpactMapping{
		{BrokerID: broker.ID, BrokerName: broker.Name, Profile: "k8s"},
	}, resp.Impact.BrokerMappings)
	assert.True(t, resp.Impact.Managed)
	joined := strings.Join(resp.Impact.ManualCleanup, "\n")
	assert.Contains(t, joined, "kubernetes_service_account_mappings")
	assert.Contains(t, joined, "Kubernetes ServiceAccount")
	assert.Contains(t, joined, "workloadIdentityUser")
	assert.Contains(t, joined, "retained in GCP")
	assert.NotContains(t, joined, sa.Email)
}

func TestGCPSARemove_InvalidForceRejected(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	sa := mkSA(t, s, "sa-rm-badforce", "rm-badforce@example.com", store.ScopeProject, project.ID, owner.ID)

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, saRemovePath(project.ID, sa.ID)+"?force=maybe", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.True(t, saExists(t, s, sa.ID))
}

func TestClearProjectDefaultsReferencing_NilProjectIsANoop(t *testing.T) {
	assert.NotPanics(t, func() { clearProjectDefaultsReferencing(nil, "sa-1") })
	assert.NotPanics(t, func() { clearProjectDefaultsReferencing(&store.Project{ID: "p1"}, "sa-1") })
}

func TestClearProjectDefaultsReferencing_NonAssignModeKept(t *testing.T) {
	p := &store.Project{ID: "p1", Annotations: map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModePassthrough,
		projectSettingDefaultGCPIdentitySAID: "sa-1",
	}}
	clearProjectDefaultsReferencing(p, "sa-1")
	assert.Equal(t, store.GCPMetadataModePassthrough, p.Annotations[projectSettingDefaultGCPIdentityMode],
		"a stale account ID under a non-assign mode is removed without changing the mode")
	_, has := p.Annotations[projectSettingDefaultGCPIdentitySAID]
	assert.False(t, has)
}

func TestGCPSAAudit_RegisterAndVerifyCarryNoEmail(t *testing.T) {
	srv, _, owner, _, _, project := setupGCPAuthzTest(t)
	audit := &recordingSAAuditLogger{}
	srv.SetAuditLogger(audit)
	srv.SetGCPTokenGenerator(&mockGCPTokenGenerator{email: "hub@example.com"})

	const email = "audit-register@example.com"
	rec := doRequestAsUser(t, srv, owner, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/gcp-service-accounts", project.ID),
		map[string]string{"email": email, "projectId": "gcp-proj"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.GCPServiceAccount
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	reg := audit.ofType(GCPSAAuditRegister)
	require.Len(t, reg, 1)
	assert.Equal(t, created.ID, reg[0].ServiceAccountID)
	assert.Equal(t, store.ScopeProject, reg[0].Scope)
	assert.Equal(t, project.ID, reg[0].ScopeID)
	assert.Equal(t, owner.ID, reg[0].ActorID)
	assert.Equal(t, store.GCPVerificationVerified, reg[0].Details["verification"])

	rec = doRequestAsUser(t, srv, owner, http.MethodPost, saRemovePath(project.ID, created.ID)+"/verify", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	ver := audit.ofType(GCPSAAuditVerify)
	require.Len(t, ver, 1)
	assert.True(t, ver[0].Success)
	assert.Equal(t, gcpSAAuditOutcomeVerified, ver[0].Outcome)
	assert.Equal(t, owner.ID, ver[0].ActorID)

	srv.SetGCPTokenGenerator(&mockGCPTokenGeneratorVerifyFail{email: "hub@example.com", verifyErr: errors.New("denied for " + email)})
	rec = doRequestAsUser(t, srv, owner, http.MethodPost, saRemovePath(project.ID, created.ID)+"/verify", nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	ver = audit.ofType(GCPSAAuditVerify)
	require.Len(t, ver, 2)
	assert.False(t, ver[1].Success)
	assert.Equal(t, gcpSAAuditOutcomeVerifyFailed, ver[1].Outcome)

	for _, e := range audit.events {
		b, err := json.Marshal(e)
		require.NoError(t, err)
		assert.NotContains(t, string(b), email, "audit events must not carry the account email")
		assert.False(t, e.Timestamp.IsZero())
		assert.WithinDuration(t, time.Now(), e.Timestamp, time.Minute)
	}
}

// A caller who cannot see another project learns only counts about it: no
// agent names or ids, no project id, no profile name. A default there is not
// clearable by this caller, so even a forced delete stays a 409.
func TestGCPSARemove_ReportRedactsWhatTheCallerCannotSee(t *testing.T) {
	srv, s, owner, _, outsider, project := setupGCPAuthzTest(t)
	ctx := context.Background()
	sa := mkSA(t, s, "sa-rm-redact", "rm-redact@example.com", store.ScopeHub, "hub-instance-1", outsider.ID)
	setSARemoveProjectAnnotations(t, s, project.ID, map[string]string{
		projectSettingDefaultGCPIdentitySAIDByProfile: fmt.Sprintf(`{"secret-profile":%q}`, sa.ID),
	})
	hidden := &store.Agent{
		ID: tid("agent-rm-hidden"), Slug: "agent-rm-hidden-slug", Name: "agent-rm-hidden-name",
		ProjectID: project.ID, CreatedBy: owner.ID, OwnerID: owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID,
		}},
	}
	require.NoError(t, s.CreateAgent(ctx, hidden))

	// Precondition: the outsider cannot read the agent through the API.
	get := doRequestAsUser(t, srv, outsider, http.MethodGet,
		fmt.Sprintf("/api/v1/projects/%s/agents/%s", project.ID, hidden.ID), nil)
	require.NotEqual(t, http.StatusOK, get.Code, "fixture: the outsider must not be able to read the agent")

	for _, path := range []string{flatSAPath + sa.ID, flatSAPath + sa.ID + "?force=true"} {
		rec := doRequestAsUser(t, srv, outsider, http.MethodDelete, path, nil)
		require.Equal(t, http.StatusConflict, rec.Code, "%s: %s", path, rec.Body.String())
		body := rec.Body.String()
		for _, secret := range []string{project.ID, hidden.ID, hidden.Name, hidden.Slug, "secret-profile"} {
			assert.NotContains(t, body, secret, "%s: the report must not disclose %q", path, secret)
		}

		code, msg, impact := decodeInUseImpact(t, rec.Body.Bytes())
		assert.Equal(t, ErrCodeSAInUse, code)
		assert.Contains(t, msg, "1 default(s) in other projects")
		assert.Contains(t, msg, "an admin of those projects must change them")
		assert.Equal(t, []GCPServiceAccountImpactDefault{
			{Tier: GCPSADefaultTierProfile, Clearable: false, Redacted: true},
		}, impact.Defaults)
		assert.Equal(t, 1, impact.HiddenDefaultCount)
		assert.Empty(t, impact.Agents)
		assert.Equal(t, 1, impact.AgentCount)
		assert.Zero(t, impact.VisibleAgentCount)
		assert.Equal(t, []int{1}, impact.HiddenAgentCounts)
	}

	assert.True(t, saExists(t, s, sa.ID))
	p, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"secret-profile": sa.ID}, profileDefaultSAIDsFromAnnotations(p.Annotations),
		"a default the caller may not change is never cleared")
}

// projectUpdatedSpy records PublishProjectUpdated calls.
type projectUpdatedSpy struct {
	noopEventPublisher
	mu  sync.Mutex
	ids []string
}

func (e *projectUpdatedSpy) PublishProjectUpdated(_ context.Context, p *store.Project) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ids = append(e.ids, p.ID)
}

func (e *projectUpdatedSpy) updated() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.ids...)
}

// failingSADeleteStore fails service account deletes once its switch is
// armed (installStoreFault).
type failingSADeleteStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *failingSADeleteStore) DeleteGCPServiceAccount(ctx context.Context, id string) error {
	if !f.fault.Active() {
		return f.Store.DeleteGCPServiceAccount(ctx, id)
	}
	return errors.New("injected delete failure")
}

// Clearing and deleting are not atomic: when the delete fails after a forced
// clear, the caller is told which defaults are already cleared.
func TestGCPSARemove_ForceThenDeleteFailsReportsClearedDefaults(t *testing.T) {
	srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *failingSADeleteStore {
		return &failingSADeleteStore{Store: inner, fault: f}
	})
	owner, _, _, project := setupGCPAuthzFixture(t, srv, s)
	sa := mkSA(t, s, "sa-rm-delfail", "rm-delfail@example.com", store.ScopeProject, project.ID, owner.ID)
	setSARemoveProjectAnnotations(t, s, project.ID, map[string]string{
		projectSettingDefaultGCPIdentityMode: store.GCPMetadataModeAssign,
		projectSettingDefaultGCPIdentitySAID: sa.ID,
	})
	fault.Arm()

	rec := doRequestAsUser(t, srv, owner, http.MethodDelete, saRemovePath(project.ID, sa.ID)+"?force=true", nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())

	var resp struct {
		Error struct {
			Message string `json:"message"`
			Details struct {
				ClearedDefaults []GCPServiceAccountImpactDefault `json:"clearedDefaults"`
			} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Contains(t, resp.Error.Message, "stay cleared")
	assert.Equal(t, []GCPServiceAccountImpactDefault{
		{Tier: GCPSADefaultTierProject, ProjectID: project.ID, Clearable: true},
	}, resp.Error.Details.ClearedDefaults)

	assert.True(t, saExists(t, s, sa.ID), "the account is still registered")
	p, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPMetadataModeBlock, p.Annotations[projectSettingDefaultGCPIdentityMode],
		"the default cleared before the failure stays cleared")
}

// Agents are named only for a user caller, the same rule as defaults: an
// agent identity (which readableAgentRows would pass every row for) gets
// counts only.
func TestGCPSAImpact_AgentCallerGetsNoAgentNames(t *testing.T) {
	srv, s, owner, _, _, project := setupGCPAuthzTest(t)
	ctx := context.Background()
	sa := mkSA(t, s, "sa-rm-agentcaller", "rm-agentcaller@example.com", store.ScopeProject, project.ID, owner.ID)
	ref := &store.Agent{
		ID: tid("agent-rm-agentcaller"), Slug: "agent-rm-agentcaller", Name: "agent-rm-agentcaller",
		ProjectID: project.ID, CreatedBy: owner.ID, OwnerID: owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID,
		}},
	}
	require.NoError(t, s.CreateAgent(ctx, ref))

	agentCtx := contextWithIdentity(ctx, newAgentIdentityFromStore(ref))
	impact, err := srv.gcpServiceAccountImpact(agentCtx, sa)
	require.NoError(t, err)
	assert.Empty(t, impact.Agents)
	assert.Equal(t, 1, impact.AgentCount)
	assert.Zero(t, impact.VisibleAgentCount)
	assert.Equal(t, []int{1}, impact.HiddenAgentCounts)

	userCtx := contextWithIdentity(ctx, NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, string(ClientTypeWeb)))
	impact, err = srv.gcpServiceAccountImpact(userCtx, sa)
	require.NoError(t, err)
	require.Len(t, impact.Agents, 1, "the project owner, a user who may read the agent, sees its name")
	assert.Equal(t, ref.ID, impact.Agents[0].ID)
}
