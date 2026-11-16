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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type triggersTestFixture struct {
	srv          *Server
	store        store.Store
	projectID    string
	otherProjID  string
	owner        *store.User
	admin        *store.User
	member       *store.User
	viewer       *store.User
	ownerAuth    UserIdentity
	adminAuth    UserIdentity
	memberAuth   UserIdentity
	viewerAuth   UserIdentity
	agentA       *store.Agent
	agentB       *store.Agent
	agentNone    *store.Agent
	targetAgent  *store.Agent
	agentAAuth   AgentIdentity
	agentBAuth   AgentIdentity
	agentNoneJWT AgentIdentity
}

func enableTriggersExperimentForTest(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range srv.experimentRegistry().All() {
		if e.Name == experiments.Triggers {
			e.Default = enabled
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

func setupTriggersTestFixture(t *testing.T) *triggersTestFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetHubID("test-hub-triggers")
	srv.SetSecretBackend(secret.NewLocalBackendWithCipher(s, srv.hubID, srv.cipher))
	srv.config.HubEndpoint = "https://hub-public.example.com"
	enableTriggersExperimentForTest(t, srv, true)

	ctx := context.Background()
	now := time.Now().UTC()

	mkUser := func(tag string) (*store.User, UserIdentity) {
		u := &store.User{
			ID:          tid("trig-" + tag),
			Email:       tag + "@triggers.example.com",
			DisplayName: "Trig " + tag,
			Role:        store.UserRoleMember,
			Status:      "active",
			Created:     now,
		}
		require.NoError(t, s.CreateUser(ctx, u))
		return u, NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "api")
	}

	owner, ownerAuth := mkUser("owner")
	admin, adminAuth := mkUser("admin")
	member, memberAuth := mkUser("member")
	viewer, viewerAuth := mkUser("viewer")

	proj := &store.Project{
		ID:      tid("trig-proj-1"),
		Name:    "Triggers Test Project",
		Slug:    "triggers-test-project",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	otherProj := &store.Project{
		ID:      tid("trig-proj-2"),
		Name:    "Triggers Other Project",
		Slug:    "triggers-other-project",
		OwnerID: owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, otherProj))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, otherProj.ID, owner.ID))

	bindRole := func(userID, roleName string) {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          proj.ID,
			CreatedBy:        owner.ID,
			CreatedAt:        now,
		})
		require.NoError(t, err)
	}
	bindRole(owner.ID, store.ProjectRoleOwner)
	bindRole(admin.ID, store.ProjectRoleAdmin)
	bindRole(member.ID, store.ProjectRoleMember)
	bindRole(viewer.ID, store.ProjectRoleViewer)

	mkAgent := func(slug string, scopes ...AgentTokenScope) (*store.Agent, AgentIdentity) {
		ag := &store.Agent{
			ID:        tid("trig-agent-" + slug),
			Slug:      slug,
			Name:      slug,
			ProjectID: proj.ID,
			Phase:     string(state.PhaseRunning),
			CreatedBy: owner.ID,
			OwnerID:   owner.ID,
			Ancestry:  []string{owner.ID},
		}
		require.NoError(t, s.CreateAgent(ctx, ag))
		require.NoError(t, s.CreateDelegationEdge(ctx, &store.DelegationEdge{
			ID:            api.NewUUID(),
			DelegatorType: store.DelegationPrincipalUser,
			DelegatorID:   owner.ID,
			DelegateType:  store.DelegationPrincipalAgent,
			DelegateID:    ag.ID,
			ScopeType:     store.RoleScopeProject,
			ScopeID:       proj.ID,
			Role:          string(AgentRoleBaseline),
			EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
			Active:        true,
			CreatedAt:     now,
		}))
		jwtID := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: ag.ID},
			ProjectID: proj.ID,
			Scopes:    scopes,
		}}
		return ag, jwtID
	}

	agentA, agentAAuth := mkAgent("agent-a", ScopeProjectRead)
	agentB, agentBAuth := mkAgent("agent-b", ScopeProjectRead)
	agentNone, agentNoneJWT := mkAgent("agent-none")
	targetAgent, _ := mkAgent("lead", ScopeProjectRead)

	return &triggersTestFixture{
		srv:          srv,
		store:        s,
		projectID:    proj.ID,
		otherProjID:  otherProj.ID,
		owner:        owner,
		admin:        admin,
		member:       member,
		viewer:       viewer,
		ownerAuth:    ownerAuth,
		adminAuth:    adminAuth,
		memberAuth:   memberAuth,
		viewerAuth:   viewerAuth,
		agentA:       agentA,
		agentB:       agentB,
		agentNone:    agentNone,
		targetAgent:  targetAgent,
		agentAAuth:   agentAAuth,
		agentBAuth:   agentBAuth,
		agentNoneJWT: agentNoneJWT,
	}
}

func (f *triggersTestFixture) doWebhookEndpoint(t *testing.T, identity Identity, subPath, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/projects/" + f.projectID + "/webhook-endpoints"
	if subPath != "" {
		path += "/" + subPath
	}
	req := authoredRequest(t, identity, method, path, body)
	rec := httptest.NewRecorder()
	f.srv.handleWebhookEndpoints(rec, req, f.projectID, subPath)
	return rec
}

func (f *triggersTestFixture) doTriggerWithHeaders(t *testing.T, identity Identity, subPath, method string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/api/v1/projects/" + f.projectID + "/triggers"
	if subPath != "" {
		path += "/" + subPath
	}
	req := authoredRequest(t, identity, method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.handleTriggers(rec, req, f.projectID, subPath)
	return rec
}

func (f *triggersTestFixture) doTrigger(t *testing.T, identity Identity, subPath, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return f.doTriggerWithHeaders(t, identity, subPath, method, body, nil)
}

func TestTriggersExperimentOff_AllRoutesReturn404(t *testing.T) {
	f := setupTriggersTestFixture(t)
	enableTriggersExperimentForTest(t, f.srv, false)

	for _, tc := range []struct {
		name string
		run  func() *httptest.ResponseRecorder
	}{
		{"GET /webhook-endpoints", func() *httptest.ResponseRecorder { return f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodGet, nil) }},
		{"POST /webhook-endpoints", func() *httptest.ResponseRecorder {
			return f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "off-ep"})
		}},
		{"GET /webhook-endpoints/{id}", func() *httptest.ResponseRecorder {
			return f.doWebhookEndpoint(t, f.ownerAuth, "some-id", http.MethodGet, nil)
		}},
		{"DELETE /webhook-endpoints/{id}", func() *httptest.ResponseRecorder {
			return f.doWebhookEndpoint(t, f.ownerAuth, "some-id", http.MethodDelete, nil)
		}},
		{"GET /triggers", func() *httptest.ResponseRecorder { return f.doTrigger(t, f.ownerAuth, "", http.MethodGet, nil) }},
		{"POST /triggers", func() *httptest.ResponseRecorder {
			return f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{Name: "off-tr"})
		}},
		{"GET /triggers/{id}", func() *httptest.ResponseRecorder { return f.doTrigger(t, f.ownerAuth, "some-id", http.MethodGet, nil) }},
		{"PATCH /triggers/{id}", func() *httptest.ResponseRecorder {
			return f.doTrigger(t, f.ownerAuth, "some-id", http.MethodPatch, UpdateTriggerRequest{})
		}},
		{"DELETE /triggers/{id}", func() *httptest.ResponseRecorder {
			return f.doTrigger(t, f.ownerAuth, "some-id", http.MethodDelete, nil)
		}},
		{"POST /triggers/{id}/pause", func() *httptest.ResponseRecorder {
			return f.doTrigger(t, f.ownerAuth, "some-id/pause", http.MethodPost, nil)
		}},
		{"POST /triggers/{id}/resume", func() *httptest.ResponseRecorder {
			return f.doTrigger(t, f.ownerAuth, "some-id/resume", http.MethodPost, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := tc.run()
			assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		})
	}
}

func TestWebhookEndpointCRUD_SecretLifecycleAndCascadePause(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// 1. Create endpoint -> 201 with HubEndpointForWebhooks URL and one-time 32-byte base64url secret.
	rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:        "github-deploys",
		Description: "GitHub deployment webhook",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, "github-deploys", created.Name)
	assert.Equal(t, "https://hub-public.example.com/api/v1/hooks/"+created.ID, created.URL)
	assert.Equal(t, store.WebhookSignatureModeScionV1, created.SignatureMode)
	assert.Equal(t, 1, created.SecretVersion)
	rawSecretBytes, err := base64.RawURLEncoding.DecodeString(created.Secret)
	require.NoError(t, err)
	assert.Len(t, rawSecretBytes, webhookEndpointSecretBytes)

	// 2. Duplicate name in same project -> 409 conflict.
	dupRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "github-deploys"})
	assert.Equal(t, http.StatusConflict, dupRec.Code, dupRec.Body.String())

	// 3. Stored secret row is hub-scoped SecretTypeInternal and unseals to created.Secret.
	secretKey := store.WebhookEndpointSecretKey(f.projectID, created.ID, 1)
	secretMeta, err := f.store.GetSecret(ctx, secretKey, store.ScopeHub, f.srv.hubID)
	require.NoError(t, err)
	assert.Equal(t, store.SecretTypeInternal, secretMeta.SecretType)
	encryptedVal, err := f.store.GetSecretValue(ctx, secretKey, store.ScopeHub, f.srv.hubID)
	require.NoError(t, err)
	unsealed, err := f.srv.unsealWebhookEndpointSecret(encryptedVal)
	require.NoError(t, err)
	assert.Equal(t, created.Secret, unsealed)

	// 4. Generic /api/v1/secrets refuses PUT/DELETE with 409 internal_secret and hides from GET/LIST.
	devAdmin := NewAuthenticatedUser(DevUserID, "dev@localhost", "Dev", store.UserRoleAdmin, "dev")
	for _, m := range []struct {
		method string
		body   any
		want   int
	}{
		{http.MethodGet, nil, http.StatusNotFound},
		{http.MethodPut, SetSecretRequest{Value: base64.StdEncoding.EncodeToString([]byte("overwrite")), Scope: store.ScopeHub}, http.StatusConflict},
		{http.MethodPatch, map[string]any{"scope": store.ScopeHub, "description": "updated"}, http.StatusConflict},
		{http.MethodDelete, nil, http.StatusConflict},
	} {
		sReq := authoredRequest(t, devAdmin, m.method, "/api/v1/secrets/"+secretKey+"?scope=hub", m.body)
		sRec := httptest.NewRecorder()
		f.srv.handleSecretByKey(sRec, sReq)
		assert.Equal(t, m.want, sRec.Code, "%s /api/v1/secrets/%s: %s", m.method, secretKey, sRec.Body.String())
		if m.want == http.StatusConflict {
			assert.Equal(t, ErrCodeInternalSecret, decodeErrorCode(t, sRec.Body.Bytes()))
		}
	}
	listReq := authoredRequest(t, devAdmin, http.MethodGet, "/api/v1/secrets?scope=hub", nil)
	listRec := httptest.NewRecorder()
	f.srv.handleSecrets(listRec, listReq)
	require.Equal(t, http.StatusOK, listRec.Code)
	assert.NotContains(t, listRec.Body.String(), secretKey)

	// 5. Create active trigger referencing endpoint by name, then DELETE endpoint -> pauses trigger (source_deleted) & deletes secret row.
	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "on-deploy",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: "github-deploys"},
		Action: TriggerActionSpec{Type: "message", AgentName: f.targetAgent.Slug, Message: "deployed"},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))
	assert.Equal(t, created.ID, tr.Source.ID, "name reference must resolve to authoritative endpoint UUID")

	delRec := f.doWebhookEndpoint(t, f.ownerAuth, created.ID, http.MethodDelete, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, delRec.Body.String())

	_, err = f.store.GetSecret(ctx, secretKey, store.ScopeHub, f.srv.hubID)
	assert.ErrorIs(t, err, store.ErrNotFound, "DELETE /webhook-endpoints/{id} must remove internal secret row")

	pausedTr, err := f.store.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, pausedTr.Status)
	assert.Equal(t, store.TriggerStatusReasonSourceDeleted, pausedTr.StatusReason)

	// Resuming a trigger whose source endpoint was deleted fails with 404 webhook endpoint not found.
	resRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/resume", http.MethodPost, nil)
	assert.Equal(t, http.StatusNotFound, resRec.Code, resRec.Body.String())
	assert.Contains(t, resRec.Body.String(), webhookEndpointNotFoundMessage)

	// 6. Project deletion cascades triggers, endpoints, and hub-scoped webhook endpoint internal secrets.
	ep2Rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cascade-ep"})
	require.Equal(t, http.StatusCreated, ep2Rec.Code, ep2Rec.Body.String())
	var ep2 WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(ep2Rec.Body.Bytes(), &ep2))
	ep2SecretKey := store.WebhookEndpointSecretKey(f.projectID, ep2.ID, 1)

	ownerSession := NewAuthenticatedUser(f.owner.ID, f.owner.Email, f.owner.DisplayName, f.owner.Role, "web")
	delSvc := NewProjectDeletionService(f.store, f.srv.authzService, slog.Default())
	delRes, delDecision := delSvc.Delete(contextWithIdentity(ctx, ownerSession), ProjectDeleteRequest{
		ProjectID: f.projectID,
		Actor:     ownerSession,
		Force:     true,
	})
	require.Nil(t, delDecision, "expected project delete to succeed: %+v", delDecision)
	require.NotNil(t, delRes)
	assert.Equal(t, 1, delRes.CascadeSummary.Triggers, "CascadeSummary.Triggers must count cascaded trigger rows")
	_, err = f.store.GetWebhookEndpoint(ctx, ep2.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "project delete must cascade webhook endpoints")
	_, err = f.store.GetSecret(ctx, ep2SecretKey, store.ScopeHub, f.srv.hubID)
	assert.ErrorIs(t, err, store.ErrNotFound, "project delete must cascade hub-scoped webhook endpoint secret rows")
}

func TestTriggersAndWebhookEndpoints_CapsReturn409LimitExceeded(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// 1. 20 webhook endpoints succeed; 21st returns 409 limit_exceeded.
	var epIDs []string
	for i := 0; i < store.MaxWebhookEndpointsPerProject; i++ {
		rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: fmt.Sprintf("cap-ep-%02d", i),
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var ep WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ep))
		epIDs = append(epIDs, ep.ID)
	}
	overEp := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cap-ep-overflow"})
	require.Equal(t, http.StatusConflict, overEp.Code, overEp.Body.String())
	assert.Equal(t, ErrCodeLimitExceeded, decodeErrorCode(t, overEp.Body.Bytes()))

	// 2. 50 active triggers on epIDs[0] succeed; 51st active returns 409 limit_exceeded on create AND on resume.
	var firstTrID string
	for i := 0; i < store.MaxActiveTriggersPerSource; i++ {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   fmt.Sprintf("src-cap-%03d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[0]},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if i == 0 {
			var tr TriggerResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tr))
			firstTrID = tr.ID
		}
	}
	overSrcCreate := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "src-cap-overflow",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[0]},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
	})
	require.Equal(t, http.StatusConflict, overSrcCreate.Code, overSrcCreate.Body.String())
	assert.Equal(t, ErrCodeLimitExceeded, decodeErrorCode(t, overSrcCreate.Body.Bytes()))

	// Pause firstTrID -> create 50th active -> resume firstTrID must ALSO return 409 limit_exceeded!
	pauseRec := f.doTrigger(t, f.ownerAuth, firstTrID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code)
	replacementRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "src-cap-replacement",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[0]},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
	})
	require.Equal(t, http.StatusCreated, replacementRec.Code)

	overSrcResume := f.doTrigger(t, f.ownerAuth, firstTrID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusConflict, overSrcResume.Code, overSrcResume.Body.String())
	assert.Equal(t, ErrCodeLimitExceeded, decodeErrorCode(t, overSrcResume.Body.Bytes()))

	// 3. Seed remaining triggers up to MaxTriggersPerProject (200); 201st returns 409 limit_exceeded.
	curCount, err := f.store.CountTriggers(ctx, store.TriggerFilter{ProjectID: f.projectID})
	require.NoError(t, err)
	now := time.Now().UTC()
	for i := curCount; i < store.MaxTriggersPerProject; i++ {
		require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
			ID:             api.NewUUID(),
			ProjectID:      f.projectID,
			Name:           fmt.Sprintf("proj-cap-seed-%03d", i),
			Status:         store.TriggerStatusPaused,
			SourceKind:     store.TriggerSourceKindWebhook,
			SourceID:       epIDs[1],
			MatchTypes:     []string{"webhook.received"},
			MatchDialect:   "cel",
			ActionType:     "message",
			ActionPayload:  `{"agentId":"` + f.targetAgent.ID + `","message":"hi"}`,
			SubjectDialect: "cel",
			SubjectMode:    store.TriggerSubjectModeNew,
			CreatedBy:      f.owner.ID,
			InitiatorAttribution: store.InitiatorAttribution{
				InitiatorPrincipalKind:  "user",
				InitiatorPrincipalID:    f.owner.ID,
				InitiatorCredentialKind: store.InitiatorCredentialKindSession,
				AttributionVersion:      1,
				AuthorizationRevision:   1,
			},
			CreatedAt: now,
			UpdatedAt: now,
		}))
	}
	overProjCreate := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "proj-cap-overflow",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[1]},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
	})
	require.Equal(t, http.StatusConflict, overProjCreate.Code, overProjCreate.Body.String())
	assert.Equal(t, ErrCodeLimitExceeded, decodeErrorCode(t, overProjCreate.Body.Bytes()))
}

func TestTriggersAuthoring_Stage1b1ValidationRefusals(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "val-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	validBase := func(name string) CreateTriggerRequest {
		return CreateTriggerRequest{
			Name:   name,
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hello"},
		}
	}

	refusalCases := []struct {
		name    string
		mutate  func(*CreateTriggerRequest)
		wantMsg string
	}{
		{
			name:    "non-empty match.expr refused in 1b-1",
			mutate:  func(r *CreateTriggerRequest) { r.Match = &TriggerMatchSpec{Expr: "true"} },
			wantMsg: "match_rule is not yet supported",
		},
		{
			name:    "non-cel match.dialect refused",
			mutate:  func(r *CreateTriggerRequest) { r.Match = &TriggerMatchSpec{Dialect: "rego"} },
			wantMsg: "unsupported match dialect",
		},
		{
			name:    "invalid match.types pattern refused",
			mutate:  func(r *CreateTriggerRequest) { r.Match = &TriggerMatchSpec{Types: []string{"UPPERCASE"}} },
			wantMsg: "match_types",
		},
		{
			name:    "non-empty subject.expr refused in 1b-1",
			mutate:  func(r *CreateTriggerRequest) { r.Subject = &TriggerSubjectSpec{Expr: "event.subject"} },
			wantMsg: "subject_rule is not yet supported",
		},
		{
			name:    "subject.mode skip refused in 1b-1",
			mutate:  func(r *CreateTriggerRequest) { r.Subject = &TriggerSubjectSpec{Mode: "skip"} },
			wantMsg: `subject_mode "skip" is not yet supported`,
		},
		{
			name:    "non-message action.type refused in Stage 1",
			mutate:  func(r *CreateTriggerRequest) { r.Action.Type = "dispatch_agent" },
			wantMsg: "unsupported action type",
		},
	}

	for i, tc := range refusalCases {
		t.Run(tc.name, func(t *testing.T) {
			req := validBase(fmt.Sprintf("bad-%d", i))
			tc.mutate(&req)
			rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var errEnv struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errEnv))
			assert.Contains(t, errEnv.Error.Message, tc.wantMsg)
		})
	}

	// Default match.types = ["webhook.received"] and subject.mode = "new" when omitted.
	okRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, validBase("defaults-ok"))
	require.Equal(t, http.StatusCreated, okRec.Code, okRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(okRec.Body.Bytes(), &tr))
	assert.Equal(t, []string{"webhook.received"}, tr.Match.Types)
	assert.Equal(t, "cel", tr.Match.Dialect)
	assert.Equal(t, store.TriggerSubjectModeNew, tr.Subject.Mode)
}

func TestTriggersLifecycle_UpdateCASPauseResumeAndRevision(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cas-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "cas-trigger",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "v1"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	assert.Equal(t, `"1"`, createRec.Header().Get("ETag"))
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &tr))
	assert.Equal(t, 1, tr.AuthorizationRevision)

	// Stale If-Match -> 409 revision_conflict.
	desc := "stale"
	staleRec := f.doTriggerWithHeaders(t, f.adminAuth, tr.ID, http.MethodPatch,
		UpdateTriggerRequest{Description: &desc}, map[string]string{"If-Match": `"99"`})
	require.Equal(t, http.StatusConflict, staleRec.Code, staleRec.Body.String())
	assert.Equal(t, ErrCodeRevisionConflict, decodeErrorCode(t, staleRec.Body.Bytes()))

	// Metadata-only PATCH by admin preserves revision=1 and original owner attribution.
	newDesc := "metadata-only"
	metaRec := f.doTriggerWithHeaders(t, f.adminAuth, tr.ID, http.MethodPatch,
		UpdateTriggerRequest{Description: &newDesc}, map[string]string{"If-Match": `"1"`})
	require.Equal(t, http.StatusOK, metaRec.Code, metaRec.Body.String())
	assert.Equal(t, `"1"`, metaRec.Header().Get("ETag"))
	storedAfterMeta, err := f.store.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, storedAfterMeta.AuthorizationRevision)
	assert.Equal(t, f.owner.ID, storedAfterMeta.InitiatorPrincipalID, "metadata-only PATCH must preserve initiator attribution")

	// Dispatch-affecting PATCH by admin bumps revision=2 and re-attributes to admin.
	patchedTypes := []string{"webhook.deploy.*"}
	dispatchRec := f.doTriggerWithHeaders(t, f.adminAuth, tr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Types: &patchedTypes},
	}, map[string]string{"If-Match": `"1"`})
	require.Equal(t, http.StatusOK, dispatchRec.Code, dispatchRec.Body.String())
	assert.Equal(t, `"2"`, dispatchRec.Header().Get("ETag"))
	storedAfterDispatch, err := f.store.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, storedAfterDispatch.AuthorizationRevision)
	assert.Equal(t, f.admin.ID, storedAfterDispatch.InitiatorPrincipalID, "dispatch-affecting PATCH must re-attribute to editor")

	// Pause + Resume by owner bumps revision=3 and re-attributes back to owner.
	pauseRec := f.doTrigger(t, f.adminAuth, tr.ID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())
	resumeRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())
	assert.Equal(t, `"3"`, resumeRec.Header().Get("ETag"))
	storedAfterResume, err := f.store.GetTrigger(ctx, tr.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, storedAfterResume.AuthorizationRevision)
	assert.Equal(t, f.owner.ID, storedAfterResume.InitiatorPrincipalID)
	assert.Equal(t, store.TriggerStatusActive, storedAfterResume.Status)

	// Test-fixture provenance stamp recorded when created by a test-fixture user.
	ti := tiStoreFixture(t, f.store, f.owner.ID, time.Now().Add(time.Hour))
	rdOwner, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rdOwner.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ti.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectID,
		CreatedBy:        f.owner.ID,
		CreatedAt:        time.Now(),
	})
	require.NoError(t, err)
	tiAuth := NewAuthenticatedUser(ti.ID, ti.Email, ti.DisplayName, store.UserRoleMember, "api")
	tiRec := f.doTrigger(t, tiAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "fixture-stamped",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, tiRec.Code, tiRec.Body.String())
	var tiTr TriggerResponse
	require.NoError(t, json.Unmarshal(tiRec.Body.Bytes(), &tiTr))
	storedTI, err := f.store.GetTrigger(ctx, tiTr.ID)
	require.NoError(t, err)
	assert.Equal(t, ti.ID, storedTI.Fixture.RootID)
	assert.Equal(t, f.owner.ID, storedTI.Fixture.IssuerID)
}

func TestTriggersAuthzMatrix(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// Seed one user-owned endpoint and trigger via owner.
	ownerEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "owner-secret-hook"})
	require.Equal(t, http.StatusCreated, ownerEpRec.Code, ownerEpRec.Body.String())
	var ownerEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(ownerEpRec.Body.Bytes(), &ownerEp))

	ownerTrRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "owner-trigger",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ownerEp.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "owner-msg"},
	})
	require.Equal(t, http.StatusCreated, ownerTrRec.Code, ownerTrRec.Body.String())
	var ownerTr TriggerResponse
	require.NoError(t, json.Unmarshal(ownerTrRec.Body.Bytes(), &ownerTr))

	t.Run("project roles: admin, member, viewer", func(t *testing.T) {
		// Admin: can create/list/read/update/pause/resume, CANNOT delete endpoint or trigger.
		adminEpRec := f.doWebhookEndpoint(t, f.adminAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "admin-ep"})
		require.Equal(t, http.StatusCreated, adminEpRec.Code, adminEpRec.Body.String())
		var adminEp WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(adminEpRec.Body.Bytes(), &adminEp))
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.adminAuth, adminEp.ID, http.MethodDelete, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.adminAuth, ownerTr.ID, http.MethodDelete, nil).Code)

		// Member: can create/list/read endpoint & own-agent/messageable trigger; CANNOT update/pause/resume/delete.
		memEpRec := f.doWebhookEndpoint(t, f.memberAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "member-ep"})
		require.Equal(t, http.StatusCreated, memEpRec.Code, memEpRec.Body.String())
		assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, f.memberAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doWebhookEndpoint(t, f.memberAuth, ownerEp.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, f.memberAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, f.memberAuth, ownerTr.ID, http.MethodGet, nil).Code)
		desc := "nope"
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.memberAuth, ownerTr.ID, http.MethodPatch, UpdateTriggerRequest{Description: &desc}).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.memberAuth, ownerTr.ID+"/pause", http.MethodPost, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.memberAuth, ownerTr.ID+"/resume", http.MethodPost, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.memberAuth, ownerTr.ID, http.MethodDelete, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.memberAuth, ownerEp.ID, http.MethodDelete, nil).Code)

		// Viewer: 403 on all 9 operations.
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.viewerAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.viewerAuth, ownerEp.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.viewerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "v-ep"}).Code)
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.viewerAuth, ownerEp.ID, http.MethodDelete, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.viewerAuth, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.viewerAuth, ownerTr.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.viewerAuth, "", http.MethodPost, CreateTriggerRequest{Name: "v-tr"}).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.viewerAuth, ownerTr.ID, http.MethodPatch, UpdateTriggerRequest{Description: &desc}).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.viewerAuth, ownerTr.ID, http.MethodDelete, nil).Code)
	})

	t.Run("UAT scopes and project boundary", func(t *testing.T) {
		projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}
		wrongProjBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.otherProjID}

		noTriggerScope := scheduleTestUAT(t, f.ownerAuth, projBoundary, "project:read")
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, noTriggerScope, "", http.MethodGet, nil).Code)

		// Missing webhook_endpoint:read -> source resolution fails closed as 404.
		missingEpRead := scheduleTestUAT(t, f.ownerAuth, projBoundary, "trigger:create", "agent:message")
		recMissingEp := f.doTrigger(t, missingEpRead, "", http.MethodPost, CreateTriggerRequest{
			Name:   "uat-no-ep-read",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ownerEp.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		assert.Equal(t, http.StatusNotFound, recMissingEp.Code, recMissingEp.Body.String())

		// Wrong project boundary -> 403.
		wrongBoundaryUAT := scheduleTestUAT(t, f.ownerAuth, wrongProjBoundary, "trigger:create", "webhook_endpoint:read", "agent:message")
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, wrongBoundaryUAT, "", http.MethodPost, CreateTriggerRequest{
			Name:   "uat-wrong-proj",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ownerEp.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		}).Code)

		// Full scopes on matching project boundary -> 201 and records bounded ceiling.
		fullUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "trigger:create", "webhook_endpoint:read", "agent:message")
		okUAT := f.doTrigger(t, fullUAT, "", http.MethodPost, CreateTriggerRequest{
			Name:   "uat-ok",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ownerEp.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusCreated, okUAT.Code, okUAT.Body.String())
		var uatTr TriggerResponse
		require.NoError(t, json.Unmarshal(okUAT.Body.Bytes(), &uatTr))
		storedUAT, err := f.store.GetTrigger(ctx, uatTr.ID)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, storedUAT.AuthorityCeiling.Kind)
		assert.Equal(t, store.InitiatorCredentialKindUAT, storedUAT.InitiatorCredentialKind)
	})

	t.Run("agent isolation, PATCH refusal, and cross-endpoint 404 parity", func(t *testing.T) {
		// Role none (no ScopeProjectRead) -> 403.
		assert.Equal(t, http.StatusForbidden, f.doWebhookEndpoint(t, f.agentNoneJWT, "", http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentNoneJWT, "", http.MethodGet, nil).Code)

		// Agent A creates its own webhook endpoint; Agent B creates its own.
		epARec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-a-hook"})
		require.Equal(t, http.StatusCreated, epARec.Code, epARec.Body.String())
		var epA WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(epARec.Body.Bytes(), &epA))

		epBRec := f.doWebhookEndpoint(t, f.agentBAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-b-hook"})
		require.Equal(t, http.StatusCreated, epBRec.Code, epBRec.Body.String())
		var epB WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(epBRec.Body.Bytes(), &epB))

		// Agent A list returns scope="caller" and ONLY epA (hides ownerEp and epB).
		listARec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodGet, nil)
		require.Equal(t, http.StatusOK, listARec.Code, listARec.Body.String())
		var listA ListWebhookEndpointsResponse
		require.NoError(t, json.Unmarshal(listARec.Body.Bytes(), &listA))
		assert.Equal(t, "caller", listA.Scope)
		require.Len(t, listA.Endpoints, 1)
		assert.Equal(t, epA.ID, listA.Endpoints[0].ID)

		// F1 / AC 7: Agent A GET and DELETE on user-owned or Agent-B-owned endpoint return 404 IDENTICAL to missing UUID!
		missingGet := f.doWebhookEndpoint(t, f.agentAAuth, api.NewUUID(), http.MethodGet, nil)
		require.Equal(t, http.StatusNotFound, missingGet.Code)
		ownerEpGet := f.doWebhookEndpoint(t, f.agentAAuth, ownerEp.ID, http.MethodGet, nil)
		agentBEpGet := f.doWebhookEndpoint(t, f.agentAAuth, epB.ID, http.MethodGet, nil)
		assert.Equal(t, missingGet.Code, ownerEpGet.Code)
		assert.JSONEq(t, missingGet.Body.String(), ownerEpGet.Body.String(), "agent GET on user endpoint must be byte-equivalent JSON to missing")
		assert.JSONEq(t, missingGet.Body.String(), agentBEpGet.Body.String(), "agent GET on other agent endpoint must be byte-equivalent JSON to missing")

		missingDel := f.doWebhookEndpoint(t, f.agentAAuth, api.NewUUID(), http.MethodDelete, nil)
		require.Equal(t, http.StatusNotFound, missingDel.Code)
		ownerEpDel := f.doWebhookEndpoint(t, f.agentAAuth, ownerEp.ID, http.MethodDelete, nil)
		agentBEpDel := f.doWebhookEndpoint(t, f.agentAAuth, epB.ID, http.MethodDelete, nil)
		assert.JSONEq(t, missingDel.Body.String(), ownerEpDel.Body.String(), "agent DELETE on user endpoint must be byte-equivalent JSON to missing")
		assert.JSONEq(t, missingDel.Body.String(), agentBEpDel.Body.String(), "agent DELETE on other agent endpoint must be byte-equivalent JSON to missing")

		// F1 / AC 7: Agent A creating a trigger referencing user's or Agent B's endpoint BY ID OR BY NAME -> 404 identical to missing!
		missingSrcCreate := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "probe-missing",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: "does-not-exist-anywhere"},
			Action: TriggerActionSpec{Type: "message", AgentID: f.agentA.ID, Message: "self"},
		})
		require.Equal(t, http.StatusNotFound, missingSrcCreate.Code)
		for _, ref := range []string{ownerEp.ID, "owner-secret-hook", epB.ID, "agent-b-hook"} {
			probeRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
				Name:   "probe-" + ref[:6],
				Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ref},
				Action: TriggerActionSpec{Type: "message", AgentID: f.agentA.ID, Message: "self"},
			})
			assert.Equal(t, http.StatusNotFound, probeRec.Code, "source ref %q must return 404", ref)
			assert.JSONEq(t, missingSrcCreate.Body.String(), probeRec.Body.String(), "source ref %q must not leak existence vs missing", ref)
		}

		// Agent A creates self-targeted trigger on its own endpoint -> 201!
		trARec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "agent-a-self-trigger",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: "agent-a-hook"},
			Action: TriggerActionSpec{Type: "message", AgentID: f.agentA.ID, Message: "wake me"},
		})
		require.Equal(t, http.StatusCreated, trARec.Code, trARec.Body.String())
		var trA TriggerResponse
		require.NoError(t, json.Unmarshal(trARec.Body.Bytes(), &trA))

		// Agent A trigger list returns scope="caller" and ONLY trA.
		trListARec := f.doTrigger(t, f.agentAAuth, "", http.MethodGet, nil)
		require.Equal(t, http.StatusOK, trListARec.Code)
		var trListA ListTriggersResponse
		require.NoError(t, json.Unmarshal(trListARec.Body.Bytes(), &trListA))
		assert.Equal(t, "caller", trListA.Scope)
		require.Len(t, trListA.Triggers, 1)
		assert.Equal(t, trA.ID, trListA.Triggers[0].ID)

		// Agent PATCH on its own trigger -> 403!
		desc := "agent-edit"
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentAAuth, trA.ID, http.MethodPatch, UpdateTriggerRequest{Description: &desc}).Code)

		// Agent A GET/pause/delete on owner's trigger -> 403!
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentAAuth, ownerTr.ID, http.MethodGet, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentAAuth, ownerTr.ID+"/pause", http.MethodPost, nil).Code)
		assert.Equal(t, http.StatusForbidden, f.doTrigger(t, f.agentAAuth, ownerTr.ID, http.MethodDelete, nil).Code)

		// Agent A CAN pause, resume, and delete its own trigger, and delete its own endpoint.
		assert.Equal(t, http.StatusOK, f.doTrigger(t, f.agentAAuth, trA.ID+"/pause", http.MethodPost, nil).Code)
		assert.Equal(t, http.StatusOK, f.doTrigger(t, f.agentAAuth, trA.ID+"/resume", http.MethodPost, nil).Code)
		assert.Equal(t, http.StatusNoContent, f.doTrigger(t, f.agentAAuth, trA.ID, http.MethodDelete, nil).Code)
		assert.Equal(t, http.StatusNoContent, f.doWebhookEndpoint(t, f.agentAAuth, epA.ID, http.MethodDelete, nil).Code)
	})

	t.Run("credential-kind allow-list refuses disallowed credentials", func(t *testing.T) {
		bodyBytes, _ := json.Marshal(CreateTriggerRequest{Name: "bad-cred"})
		ctx := contextWithIdentity(context.Background(), f.ownerAuth)
		ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindHubDelivery})

		for _, route := range []struct {
			name string
			call func(*httptest.ResponseRecorder, *http.Request)
		}{
			{
				name: "POST /webhook-endpoints",
				call: func(w *httptest.ResponseRecorder, r *http.Request) {
					f.srv.handleWebhookEndpoints(w, r, f.projectID, "")
				},
			},
			{
				name: "POST /triggers",
				call: func(w *httptest.ResponseRecorder, r *http.Request) {
					f.srv.handleTriggers(w, r, f.projectID, "")
				},
			},
			{
				name: "PATCH /triggers/{id}",
				call: func(w *httptest.ResponseRecorder, r *http.Request) {
					r.Method = http.MethodPatch
					f.srv.handleTriggers(w, r, f.projectID, ownerTr.ID)
				},
			},
			{
				name: "POST /triggers/{id}/resume",
				call: func(w *httptest.ResponseRecorder, r *http.Request) {
					f.srv.handleTriggers(w, r, f.projectID, ownerTr.ID+"/resume")
				},
			},
		} {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.projectID+"/triggers", bytes.NewReader(bodyBytes)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			route.call(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", route.name, rec.Body.String())
			assert.Contains(t, rec.Body.String(), triggerAuthoringCredentialRefusedMessage)
		}
	})
}
