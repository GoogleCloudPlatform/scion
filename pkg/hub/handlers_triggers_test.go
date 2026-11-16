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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
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
	srv.reconcileTriggerDrainer(context.Background())
}

func setupTriggersTestFixture(t *testing.T) *triggersTestFixture {
	t.Helper()
	srv, s := testServer(t)
	return setupTriggersTestFixtureOnServer(t, srv, s)
}

func setupTriggersTestFixtureOnServer(t *testing.T, srv *Server, s store.Store) *triggersTestFixture {
	t.Helper()
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
	routeSubPath, query, hasQuery := strings.Cut(subPath, "?")
	path := "/api/v1/projects/" + f.projectID + "/webhook-endpoints"
	if routeSubPath != "" {
		path += "/" + routeSubPath
	}
	if hasQuery {
		path += "?" + query
	}
	req := authoredRequest(t, identity, method, path, body)
	rec := httptest.NewRecorder()
	f.srv.handleWebhookEndpoints(rec, req, f.projectID, routeSubPath)
	return rec
}

func (f *triggersTestFixture) doTriggerWithHeaders(t *testing.T, identity Identity, subPath, method string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	routeSubPath, query, hasQuery := strings.Cut(subPath, "?")
	path := "/api/v1/projects/" + f.projectID + "/triggers"
	if routeSubPath != "" {
		path += "/" + routeSubPath
	}
	if hasQuery {
		path += "?" + query
	}
	req := authoredRequest(t, identity, method, path, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.handleTriggers(rec, req, f.projectID, routeSubPath)
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

type triggerQuotaLockOrderStore struct {
	store.Store
	fault          *storeFaultSwitch
	lockCalls      atomic.Int32
	unlockedCounts atomic.Int32
	lockedCounts   atomic.Int32
	lastLockedProj atomic.Value
}

type triggerQuotaLockOrderTxStore struct {
	store.Store
	parent *triggerQuotaLockOrderStore
	locked bool
}

func (s *triggerQuotaLockOrderStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !s.fault.Active() {
		return s.Store.WithTx(ctx, fn)
	}
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&triggerQuotaLockOrderTxStore{Store: tx, parent: s})
	})
}

func (s *triggerQuotaLockOrderTxStore) LockTriggerQuota(ctx context.Context, projectID string) error {
	if err := s.Store.LockTriggerQuota(ctx, projectID); err != nil {
		return err
	}
	s.locked = true
	s.parent.lockCalls.Add(1)
	s.parent.lastLockedProj.Store(projectID)
	return nil
}

func (s *triggerQuotaLockOrderTxStore) CountWebhookEndpoints(ctx context.Context, projectID string) (int, error) {
	if s.locked {
		s.parent.lockedCounts.Add(1)
	} else {
		s.parent.unlockedCounts.Add(1)
	}
	return s.Store.CountWebhookEndpoints(ctx, projectID)
}

func (s *triggerQuotaLockOrderTxStore) CountTriggers(ctx context.Context, filter store.TriggerFilter) (int, error) {
	if s.locked {
		s.parent.lockedCounts.Add(1)
	} else {
		s.parent.unlockedCounts.Add(1)
	}
	return s.Store.CountTriggers(ctx, filter)
}

// TestTriggers_QuotaLockBeforeCountInTx verifies in-process (even on SQLite)
// that every cap-enforcing handler path — endpoint creation, trigger creation,
// trigger PATCH re-sourcing, and trigger resume — acquires tx.LockTriggerQuota
// inside WithTx before reading CountWebhookEndpoints or CountTriggers.
func TestTriggers_QuotaLockBeforeCountInTx(t *testing.T) {
	srv, s, spy, fault := testServerWithStoreFault(t, func(inner store.Store, sw *storeFaultSwitch) *triggerQuotaLockOrderStore {
		return &triggerQuotaLockOrderStore{Store: inner, fault: sw}
	})
	f := setupTriggersTestFixtureOnServer(t, srv, s)
	fault.Arm()

	// 1. POST /webhook-endpoints
	ep1Rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "lock-ep-1"})
	require.Equal(t, http.StatusCreated, ep1Rec.Code, ep1Rec.Body.String())
	var ep1 WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(ep1Rec.Body.Bytes(), &ep1))
	assert.Equal(t, int32(1), spy.lockCalls.Load())
	assert.Equal(t, int32(0), spy.unlockedCounts.Load())

	ep2Rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "lock-ep-2"})
	require.Equal(t, http.StatusCreated, ep2Rec.Code, ep2Rec.Body.String())
	var ep2 WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(ep2Rec.Body.Bytes(), &ep2))
	assert.Equal(t, int32(2), spy.lockCalls.Load())
	assert.Equal(t, int32(0), spy.unlockedCounts.Load())

	// 2. POST /triggers (counts both project triggers and active per-source triggers under one lock)
	trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "lock-tr-1",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep1.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))
	assert.Equal(t, int32(3), spy.lockCalls.Load())
	assert.Equal(t, int32(0), spy.unlockedCounts.Load())

	// 3. PATCH /triggers/{id} re-sourcing an active trigger
	patchRec := f.doTrigger(t, f.ownerAuth, tr.ID, http.MethodPatch, UpdateTriggerRequest{
		Source: &TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep2.ID},
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())
	assert.Equal(t, int32(4), spy.lockCalls.Load())
	assert.Equal(t, int32(0), spy.unlockedCounts.Load())

	// 4. POST /triggers/{id}/pause followed by POST /triggers/{id}/resume
	pauseRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())

	resumeRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())
	assert.Equal(t, int32(5), spy.lockCalls.Load())
	assert.Equal(t, int32(0), spy.unlockedCounts.Load(), "every quota count inside WithTx must follow LockTriggerQuota")
	assert.GreaterOrEqual(t, spy.lockedCounts.Load(), int32(6))
	assert.Equal(t, f.projectID, spy.lastLockedProj.Load())
}

func runTriggersConcurrentCapsAtLastSlot(t *testing.T, f *triggersTestFixture) {
	t.Helper()
	ctx := context.Background()
	const contenders = 6

	// 1. Concurrent webhook endpoint creation at MaxWebhookEndpointsPerProject - 1 (19 -> 20).
	var epIDs []string
	for i := 0; i < store.MaxWebhookEndpointsPerProject-1; i++ {
		rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: fmt.Sprintf("race-seed-ep-%02d", i),
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var ep WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ep))
		epIDs = append(epIDs, ep.ID)
	}
	{
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, contenders)
		for i := 0; i < contenders; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
					Name: fmt.Sprintf("race-win-ep-%02d", i),
				})
				codes[i] = rec.Code
			}()
		}
		close(start)
		wg.Wait()
		created, conflicts := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflicts++
			default:
				t.Fatalf("unexpected endpoint race status %d (codes=%v)", c, codes)
			}
		}
		assert.Equal(t, 1, created, "exactly one concurrent endpoint create at cap-1 may win")
		assert.Equal(t, contenders-1, conflicts)
		cnt, err := f.store.CountWebhookEndpoints(ctx, f.projectID)
		require.NoError(t, err)
		assert.Equal(t, store.MaxWebhookEndpointsPerProject, cnt)
	}

	// 2. Concurrent trigger creation at MaxActiveTriggersPerSource - 1 (49 -> 50) on epIDs[0].
	var firstActiveOnEp0 string
	for i := 0; i < store.MaxActiveTriggersPerSource-1; i++ {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   fmt.Sprintf("race-src-seed-%03d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[0]},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		if i == 0 {
			var tr TriggerResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tr))
			firstActiveOnEp0 = tr.ID
		}
	}
	{
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, contenders)
		for i := 0; i < contenders; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
					Name:   fmt.Sprintf("race-src-win-%03d", i),
					Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[0]},
					Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
				})
				codes[i] = rec.Code
			}()
		}
		close(start)
		wg.Wait()
		created, conflicts := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflicts++
			default:
				t.Fatalf("unexpected active-source create race status %d (codes=%v)", c, codes)
			}
		}
		assert.Equal(t, 1, created, "exactly one concurrent trigger create at source cap-1 may win")
		assert.Equal(t, contenders-1, conflicts)
		srcCnt, err := f.store.CountTriggers(ctx, store.TriggerFilter{
			ProjectID:  f.projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epIDs[0],
			Status:     store.TriggerStatusActive,
		})
		require.NoError(t, err)
		assert.Equal(t, store.MaxActiveTriggersPerSource, srcCnt)
	}

	// 3. Concurrent resume at MaxActiveTriggersPerSource - 1 on epIDs[0]:
	// create contenders paused triggers on epIDs[0] (by temporarily pausing existing ones,
	// creating, and restoring so we have contenders distinct paused triggers and 49 active).
	var pausedIDs []string
	// Pause contenders triggers on epIDs[0] so active drops to 50 - contenders,
	// create contenders new triggers and pause them, then resume contenders - 1 of the
	// originals so active == 49 with contenders paused triggers ready to race!
	listRes, err := f.store.ListTriggers(ctx, store.TriggerFilter{
		ProjectID:  f.projectID,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   epIDs[0],
		Status:     store.TriggerStatusActive,
	}, store.ListOptions{Limit: contenders})
	require.NoError(t, err)
	require.Len(t, listRes.Items, contenders)
	for _, it := range listRes.Items {
		rec := f.doTrigger(t, f.ownerAuth, it.ID+"/pause", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		pausedIDs = append(pausedIDs, it.ID)
	}
	// Create contenders-1 fresh active triggers on epIDs[0] to bring active count back to 49,
	// leaving len(pausedIDs) == contenders paused triggers on epIDs[0]!
	for i := 0; i < contenders-1; i++ {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   fmt.Sprintf("race-resume-fill-%02d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[0]},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	_ = firstActiveOnEp0
	{
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, contenders)
		for i, id := range pausedIDs {
			i, id := i, id
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rec := f.doTrigger(t, f.ownerAuth, id+"/resume", http.MethodPost, nil)
				codes[i] = rec.Code
			}()
		}
		close(start)
		wg.Wait()
		resumed, conflicts := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusOK:
				resumed++
			case http.StatusConflict:
				conflicts++
			default:
				t.Fatalf("unexpected resume race status %d (codes=%v)", c, codes)
			}
		}
		assert.Equal(t, 1, resumed, "exactly one concurrent resume at source cap-1 may win")
		assert.Equal(t, contenders-1, conflicts)
		srcCnt, err := f.store.CountTriggers(ctx, store.TriggerFilter{
			ProjectID:  f.projectID,
			SourceKind: store.TriggerSourceKindWebhook,
			SourceID:   epIDs[0],
			Status:     store.TriggerStatusActive,
		})
		require.NoError(t, err)
		assert.Equal(t, store.MaxActiveTriggersPerSource, srcCnt)
	}

	// 4. Fill remaining project triggers up to MaxTriggersPerProject - 1 (199 -> 200) across epIDs[1..4],
	// then race contenders concurrent trigger creates on epIDs[5].
	curTotal, err := f.store.CountTriggers(ctx, store.TriggerFilter{ProjectID: f.projectID})
	require.NoError(t, err)
	for i := curTotal; i < store.MaxTriggersPerProject-1; i++ {
		epIdx := 1 + ((i - curTotal) / store.MaxActiveTriggersPerSource)
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   fmt.Sprintf("race-proj-seed-%03d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[epIdx]},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	}
	{
		start := make(chan struct{})
		var wg sync.WaitGroup
		codes := make([]int, contenders)
		for i := 0; i < contenders; i++ {
			i := i
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
					Name:   fmt.Sprintf("race-proj-win-%03d", i),
					Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epIDs[6]},
					Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ping"},
				})
				codes[i] = rec.Code
			}()
		}
		close(start)
		wg.Wait()
		created, conflicts := 0, 0
		for _, c := range codes {
			switch c {
			case http.StatusCreated:
				created++
			case http.StatusConflict:
				conflicts++
			default:
				t.Fatalf("unexpected project-cap create race status %d (codes=%v)", c, codes)
			}
		}
		assert.Equal(t, 1, created, "exactly one concurrent trigger create at project cap-1 may win")
		assert.Equal(t, contenders-1, conflicts)
		projCnt, err := f.store.CountTriggers(ctx, store.TriggerFilter{ProjectID: f.projectID})
		require.NoError(t, err)
		assert.Equal(t, store.MaxTriggersPerProject, projCnt)
	}
}

// TestTriggers_ConcurrentCaps_LastSlot runs the concurrent cap races on SQLite
// (where single-writer pool serialization applies; see
// TestTriggers_QuotaLockBeforeCountInTx for lock-order verification and
// TestTriggers_ConcurrentCaps_Postgres for multi-connection PostgreSQL READ
// COMMITTED).
func TestTriggers_ConcurrentCaps_LastSlot(t *testing.T) {
	f := setupTriggersTestFixture(t)
	runTriggersConcurrentCapsAtLastSlot(t, f)
}

// TestTriggers_ConcurrentCaps_Postgres runs the same concurrent cap races under
// PostgreSQL READ COMMITTED (wired into HUB_INTEGRATION_POSTGRES_TESTS), where
// pg_advisory_xact_lock(LockTriggerQuota, StableProjectHash(projectID))
// prevents concurrent writers at cap-1 from exceeding the cap (ptone/scion#8028).
func TestTriggers_ConcurrentCaps_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	srv, s := testServerWithStore(t, entadapter.NewCompositeStore(enttest.NewClient(t)))
	f := setupTriggersTestFixtureOnServer(t, srv, s)
	runTriggersConcurrentCapsAtLastSlot(t, f)
}

func TestTriggersAndWebhookEndpoints_ListByNameFilter(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epOwnerRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "owner-only-ep"})
	require.Equal(t, http.StatusCreated, epOwnerRec.Code, epOwnerRec.Body.String())
	var epOwner WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epOwnerRec.Body.Bytes(), &epOwner))

	epAgentRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-a-ep"})
	require.Equal(t, http.StatusCreated, epAgentRec.Code, epAgentRec.Body.String())
	var epAgent WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epAgentRec.Body.Bytes(), &epAgent))

	trOwnerRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "owner-only-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epOwner.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, trOwnerRec.Code, trOwnerRec.Body.String())

	trAgentRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-a-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: epAgent.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, trAgentRec.Code, trAgentRec.Body.String())

	// Exact ?name= filters endpoints and triggers for human owner.
	listEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "?name=agent-a-ep", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listEpRec.Code, listEpRec.Body.String())
	var epList ListWebhookEndpointsResponse
	require.NoError(t, json.Unmarshal(listEpRec.Body.Bytes(), &epList))
	require.Len(t, epList.Endpoints, 1)
	assert.Equal(t, "agent-a-ep", epList.Endpoints[0].Name)

	listTrRec := f.doTrigger(t, f.ownerAuth, "?name=agent-a-tr", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listTrRec.Code, listTrRec.Body.String())
	var trList ListTriggersResponse
	require.NoError(t, json.Unmarshal(listTrRec.Body.Bytes(), &trList))
	require.Len(t, trList.Triggers, 1)
	assert.Equal(t, "agent-a-tr", trList.Triggers[0].Name)

	// Caller-scoped agent filtering still hides another principal's rows even when queried by exact ?name=!
	agentProbeOwnerEp := f.doWebhookEndpoint(t, f.agentAAuth, "?name=owner-only-ep", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, agentProbeOwnerEp.Code)
	require.NoError(t, json.Unmarshal(agentProbeOwnerEp.Body.Bytes(), &epList))
	assert.Empty(t, epList.Endpoints)

	agentProbeOwnerTr := f.doTrigger(t, f.agentAAuth, "?name=owner-only-tr", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, agentProbeOwnerTr.Code)
	require.NoError(t, json.Unmarshal(agentProbeOwnerTr.Body.Bytes(), &trList))
	assert.Empty(t, trList.Triggers)
}

func TestTriggersAndWebhookEndpoints_RejectControlAndFormatCharactersInNames(t *testing.T) {
	badNames := []struct {
		label string
		value string
	}{
		{"ansi_esc", "hook-\x1b[31mred"},
		{"osc_52", "hook-\x1b]52;c;ZHVtYg==\x07"},
		{"nul", "hook\x00name"},
		{"del", "hook\x7fname"},
		{"c1_csi", "hook\u009b2J"},
		{"bidi_rlo_cf", "hook\u202ehidden"},
		{"zwsp_cf", "hook\u200bname"},
		{"bom_cf", "\ufeffhook"},
		{"line_sep_zl", "hook\u2028name"},
		{"para_sep_zp", "hook\u2029name"},
		{"trailing_lf", "hook\n"},
		{"leading_tab", "\thook"},
		{"slash", "team/hook"},
		{"invalid_utf8", "hook\xffname"},
	}

	for _, tc := range badNames {
		t.Run("unit_"+tc.label, func(t *testing.T) {
			_, err := validateWebhookEndpointName(tc.value)
			assert.Error(t, err, "validateWebhookEndpointName(%q)", tc.value)
			_, err = validateTriggerName(tc.value)
			assert.Error(t, err, "validateTriggerName(%q)", tc.value)
		})
	}

	gotEp, err := validateWebhookEndpointName("  clean-endpoint-01  ")
	require.NoError(t, err)
	assert.Equal(t, "clean-endpoint-01", gotEp)
	gotTr, err := validateTriggerName("  clean-trigger-01  ")
	require.NoError(t, err)
	assert.Equal(t, "clean-trigger-01", gotTr)

	f := setupTriggersTestFixture(t)
	validEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
		Name:        "valid-ep",
		Description: "line 1\nline 2",
	})
	require.Equal(t, http.StatusCreated, validEpRec.Code, validEpRec.Body.String())
	var validEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(validEpRec.Body.Bytes(), &validEp))

	validTrRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:        "valid-tr",
		Description: "line 1\nline 2",
		Source:      TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: validEp.ID},
		Action:      TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, validTrRec.Code, validTrRec.Body.String())
	var validTr TriggerResponse
	require.NoError(t, json.Unmarshal(validTrRec.Body.Bytes(), &validTr))

	for _, bad := range []string{"agent-\x1b[2Jspoof", "agent-\u202espoof"} {
		epRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: bad})
		assert.Equal(t, http.StatusBadRequest, epRec.Code, "endpoint create with %q must fail validation", bad)

		trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   bad,
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: validEp.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		assert.Equal(t, http.StatusBadRequest, trRec.Code, "trigger create with %q must fail validation", bad)

		badCopy := bad
		patchRec := f.doTrigger(t, f.ownerAuth, validTr.ID, http.MethodPatch, UpdateTriggerRequest{Name: &badCopy})
		assert.Equal(t, http.StatusBadRequest, patchRec.Code, "trigger PATCH with %q must fail validation", bad)

		badDescEp := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name:        "desc-check-ep",
			Description: bad,
		})
		assert.Equal(t, http.StatusBadRequest, badDescEp.Code, "endpoint description with %q must fail validation", bad)

		badDescPatch := f.doTrigger(t, f.ownerAuth, validTr.ID, http.MethodPatch, UpdateTriggerRequest{Description: &badCopy})
		assert.Equal(t, http.StatusBadRequest, badDescPatch.Code, "trigger description PATCH with %q must fail validation", bad)
	}
}

type triggerTxOrderRecorder struct {
	store.Store
	fault    *storeFaultSwitch
	mu       sync.Mutex
	beforeTx func()
	ops      [][]string
}

func (r *triggerTxOrderRecorder) reset(beforeTx func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.beforeTx = beforeTx
	r.ops = nil
}

func (r *triggerTxOrderRecorder) recordedOps() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.ops))
	for i, s := range r.ops {
		cp := make([]string, len(s))
		copy(cp, s)
		out[i] = cp
	}
	return out
}

func (r *triggerTxOrderRecorder) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if r.fault != nil && !r.fault.Active() {
		return r.Store.WithTx(ctx, fn)
	}
	r.mu.Lock()
	hook := r.beforeTx
	r.beforeTx = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}

	var txOps []string
	err := r.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&triggerTxOrderTx{Store: tx, ops: &txOps})
	})
	r.mu.Lock()
	r.ops = append(r.ops, txOps)
	r.mu.Unlock()
	return err
}

type triggerTxOrderTx struct {
	store.Store
	ops *[]string
}

func (t *triggerTxOrderTx) LockTriggerQuota(ctx context.Context, projectID string) error {
	*t.ops = append(*t.ops, "LockTriggerQuota")
	return t.Store.LockTriggerQuota(ctx, projectID)
}

func (t *triggerTxOrderTx) GetWebhookEndpoint(ctx context.Context, id string) (*store.WebhookEndpoint, error) {
	*t.ops = append(*t.ops, "GetWebhookEndpoint")
	return t.Store.GetWebhookEndpoint(ctx, id)
}

func (t *triggerTxOrderTx) DeleteWebhookEndpoint(ctx context.Context, projectID, id string) ([]string, []string, error) {
	*t.ops = append(*t.ops, "DeleteWebhookEndpoint")
	return t.Store.DeleteWebhookEndpoint(ctx, projectID, id)
}

func (t *triggerTxOrderTx) CreateTrigger(ctx context.Context, trigger *store.Trigger) error {
	*t.ops = append(*t.ops, "CreateTrigger")
	return t.Store.CreateTrigger(ctx, trigger)
}

func (t *triggerTxOrderTx) UpdateTrigger(ctx context.Context, trigger *store.Trigger, fields store.TriggerFieldMask, expectedRevision int, expectedRevisionKnown bool, newAttribution *store.InitiatorAttribution) error {
	*t.ops = append(*t.ops, "UpdateTrigger")
	return t.Store.UpdateTrigger(ctx, trigger, fields, expectedRevision, expectedRevisionKnown, newAttribution)
}

func assertOpOrder(t *testing.T, txOps []string, first, second string) {
	t.Helper()
	firstIdx, secondIdx := -1, -1
	for i, op := range txOps {
		if op == first && firstIdx == -1 {
			firstIdx = i
		}
		if op == second && secondIdx == -1 {
			secondIdx = i
		}
	}
	require.NotEqual(t, -1, firstIdx, "expected %s in tx ops %v", first, txOps)
	require.NotEqual(t, -1, secondIdx, "expected %s in tx ops %v", second, txOps)
	assert.Less(t, firstIdx, secondIdx, "expected %s before %s in tx ops %v", first, second, txOps)
}

func TestTriggers_EndpointDeleteRaceOrdering(t *testing.T) {
	srv, baseStore, recStore, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *triggerTxOrderRecorder {
		return &triggerTxOrderRecorder{Store: inner, fault: f}
	})
	f := setupTriggersTestFixtureOnServer(t, srv, recStore)
	fault.Arm()
	ctx := context.Background()

	// Capture reference 404 JSON from an unknown endpoint ID.
	refNotFoundRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "ref-missing-ep",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: api.NewUUID()},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusNotFound, refNotFoundRec.Code)
	refNotFoundBody := refNotFoundRec.Body.String()

	createEndpoint := func(t *testing.T, name string) WebhookEndpointCreatedResponse {
		t.Helper()
		rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: name})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var ep WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ep))
		return ep
	}

	t.Run("delete_endpoint_locks_quota_first", func(t *testing.T) {
		ep := createEndpoint(t, "race-del-order-ep")
		recStore.reset(nil)
		delRec := f.doWebhookEndpoint(t, f.ownerAuth, ep.ID, http.MethodDelete, nil)
		require.Equal(t, http.StatusNoContent, delRec.Code)
		ops := recStore.recordedOps()
		require.Len(t, ops, 1)
		assertOpOrder(t, ops[0], "LockTriggerQuota", "DeleteWebhookEndpoint")
	})

	t.Run("create_trigger_locks_before_reread_and_refuses_when_delete_commits_first", func(t *testing.T) {
		ep := createEndpoint(t, "race-create-del-ep")
		recStore.reset(func() {
			// Concurrent DELETE commits after pre-tx resolveTriggerWebhookSource succeeded.
			delRec := f.doWebhookEndpoint(t, f.ownerAuth, ep.ID, http.MethodDelete, nil)
			require.Equal(t, http.StatusNoContent, delRec.Code)
		})

		createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "race-create-tr",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusNotFound, createRec.Code, createRec.Body.String())
		assert.JSONEq(t, refNotFoundBody, createRec.Body.String())

		ops := recStore.recordedOps()
		require.Len(t, ops, 2, "expected inner delete tx then outer create tx: %v", ops)
		assertOpOrder(t, ops[0], "LockTriggerQuota", "DeleteWebhookEndpoint")
		assertOpOrder(t, ops[1], "LockTriggerQuota", "GetWebhookEndpoint")

		activeCount, err := baseStore.CountTriggers(ctx, store.TriggerFilter{
			ProjectID: f.projectID,
			SourceID:  ep.ID,
			Status:    store.TriggerStatusActive,
		})
		require.NoError(t, err)
		assert.Zero(t, activeCount, "delete committed first must leave zero active triggers")
	})

	t.Run("create_trigger_refuses_when_endpoint_disabled_before_tx", func(t *testing.T) {
		ep := createEndpoint(t, "race-create-disabled-ep")
		recStore.reset(func() {
			_, uErr := baseStore.(*entadapter.CompositeStore).DB().ExecContext(ctx,
				"UPDATE webhook_endpoints SET status = ? WHERE id = ?",
				store.WebhookEndpointStatusDisabled, ep.ID)
			require.NoError(t, uErr)
		})

		createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "race-disabled-tr",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusNotFound, createRec.Code, createRec.Body.String())
		assert.JSONEq(t, refNotFoundBody, createRec.Body.String())

		ops := recStore.recordedOps()
		require.Len(t, ops, 1)
		assertOpOrder(t, ops[0], "LockTriggerQuota", "GetWebhookEndpoint")

		activeCount, err := baseStore.CountTriggers(ctx, store.TriggerFilter{
			ProjectID: f.projectID,
			SourceID:  ep.ID,
			Status:    store.TriggerStatusActive,
		})
		require.NoError(t, err)
		assert.Zero(t, activeCount)
	})

	t.Run("resume_trigger_locks_before_reread_and_refuses_when_delete_commits_first", func(t *testing.T) {
		ep := createEndpoint(t, "race-resume-del-ep")
		trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "race-resume-tr",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
		var tr TriggerResponse
		require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

		pauseRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/pause", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, pauseRec.Code)

		recStore.reset(func() {
			delRec := f.doWebhookEndpoint(t, f.ownerAuth, ep.ID, http.MethodDelete, nil)
			require.Equal(t, http.StatusNoContent, delRec.Code)
		})

		resumeRec := f.doTrigger(t, f.ownerAuth, tr.ID+"/resume", http.MethodPost, nil)
		require.Equal(t, http.StatusNotFound, resumeRec.Code, resumeRec.Body.String())
		assert.JSONEq(t, refNotFoundBody, resumeRec.Body.String())

		ops := recStore.recordedOps()
		require.Len(t, ops, 2)
		assertOpOrder(t, ops[0], "LockTriggerQuota", "DeleteWebhookEndpoint")
		assertOpOrder(t, ops[1], "LockTriggerQuota", "GetWebhookEndpoint")

		stored, err := baseStore.GetTrigger(ctx, tr.ID)
		require.NoError(t, err)
		assert.Equal(t, store.TriggerStatusPaused, stored.Status)
		assert.Equal(t, store.TriggerStatusReasonSourceDeleted, stored.StatusReason)
	})

	t.Run("source_patch_locks_before_reread_and_refuses_when_delete_commits_first", func(t *testing.T) {
		ep1 := createEndpoint(t, "race-patch-ep1")
		ep2 := createEndpoint(t, "race-patch-ep2")
		trRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "race-patch-tr",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep1.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusCreated, trRec.Code, trRec.Body.String())
		var tr TriggerResponse
		require.NoError(t, json.Unmarshal(trRec.Body.Bytes(), &tr))

		recStore.reset(func() {
			delRec := f.doWebhookEndpoint(t, f.ownerAuth, ep2.ID, http.MethodDelete, nil)
			require.Equal(t, http.StatusNoContent, delRec.Code)
		})

		patchRec := f.doTrigger(t, f.ownerAuth, tr.ID, http.MethodPatch, UpdateTriggerRequest{
			Source: &TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep2.ID},
		})
		require.Equal(t, http.StatusNotFound, patchRec.Code, patchRec.Body.String())
		assert.JSONEq(t, refNotFoundBody, patchRec.Body.String())

		ops := recStore.recordedOps()
		require.Len(t, ops, 2)
		assertOpOrder(t, ops[0], "LockTriggerQuota", "DeleteWebhookEndpoint")
		assertOpOrder(t, ops[1], "LockTriggerQuota", "GetWebhookEndpoint")

		activeOnDeleted, err := baseStore.CountTriggers(ctx, store.TriggerFilter{
			ProjectID: f.projectID,
			SourceID:  ep2.ID,
			Status:    store.TriggerStatusActive,
		})
		require.NoError(t, err)
		assert.Zero(t, activeOnDeleted, "delete committed first must leave zero active triggers on deleted target endpoint")

		stored, err := baseStore.GetTrigger(ctx, tr.ID)
		require.NoError(t, err)
		assert.Equal(t, ep1.ID, stored.SourceID)
	})
}
