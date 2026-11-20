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
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
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
			name:    "non-empty match.dialect with empty expr refused",
			mutate:  func(r *CreateTriggerRequest) { r.Match = &TriggerMatchSpec{Dialect: "cel"} },
			wantMsg: "match.dialect requires a non-empty match.expr",
		},
		{
			name:    "non-empty subject.dialect with empty expr refused",
			mutate:  func(r *CreateTriggerRequest) { r.Subject = &TriggerSubjectSpec{Dialect: "cel"} },
			wantMsg: "subject.dialect requires a non-empty subject.expr",
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
	assert.NotNil(t, tr.Match.Rules)
	assert.Empty(t, tr.Match.Rules)
	assert.Empty(t, tr.Match.Dialect)
	assert.Empty(t, tr.Match.Expr)
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

type triggerRuleErrorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func decodeTriggerRuleError(t *testing.T, rec *httptest.ResponseRecorder) triggerRuleErrorBody {
	t.Helper()
	var env struct {
		Error triggerRuleErrorBody `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), "body=%s", rec.Body.String())
	return env.Error
}

func TestTriggersAuthoring_CELValidRulesCreatePatchResume(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cel-valid-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	matchExpr := `has(event.attributes.subject) && event.attributes.subject.matches("^release/") && has(event.payload) && has(event.payload.status) && event.payload.status == "ok"`
	subjExpr := `(has(event.payload) && has(event.payload.service) ? string(event.payload.service) : string(event.subject)) + ":" + string(trigger.name)`

	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "cel-valid-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match: &TriggerMatchSpec{
			Types: []string{"webhook.deploy.*"},
			Expr:  matchExpr,
		},
		Subject: &TriggerSubjectSpec{
			Expr: subjExpr,
		},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "deploy ready"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	assert.Equal(t, "cel", created.Match.Dialect)
	assert.Equal(t, matchExpr, created.Match.Expr)
	assert.Equal(t, []TriggerMatchRuleSpec{{Dialect: "cel", Expr: matchExpr}}, created.Match.Rules)
	assert.Equal(t, "cel", created.Subject.Dialect)
	assert.Equal(t, subjExpr, created.Subject.Expr)

	stored, err := f.store.GetTrigger(ctx, created.ID)
	require.NoError(t, err)
	assert.Empty(t, stored.MatchDialect)
	assert.Empty(t, stored.MatchRule)
	assert.Equal(t, []store.TriggerMatchRule{{Dialect: "cel", Expr: matchExpr}}, stored.MatchRules)
	assert.Equal(t, "cel", stored.SubjectDialect)
	assert.Equal(t, subjExpr, stored.SubjectRule)

	// Update rules via PATCH and then clear subject back to empty string (fallback subject).
	updatedMatch := `event.type == "webhook.deploy.finished"`
	emptyExpr := ""
	patchRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Match:   &TriggerMatchPatchSpec{Expr: &updatedMatch},
		Subject: &TriggerSubjectPatchSpec{Expr: &emptyExpr},
	})
	require.Equal(t, http.StatusOK, patchRec.Code, patchRec.Body.String())
	var updated TriggerResponse
	require.NoError(t, json.Unmarshal(patchRec.Body.Bytes(), &updated))
	assert.Equal(t, updatedMatch, updated.Match.Expr)
	assert.Empty(t, updated.Subject.Expr)

	// PATCH setting non-empty dialect alongside empty expr is also refused 400.
	celName := "cel"
	badEmptyExprDialectPatch := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Subject: &TriggerSubjectPatchSpec{Dialect: &celName, Expr: &emptyExpr},
	})
	require.Equal(t, http.StatusBadRequest, badEmptyExprDialectPatch.Code, badEmptyExprDialectPatch.Body.String())

	// Restore non-empty subject rule, pause, and resume -> re-compiles stored rules cleanly.
	restoredSubj := `event.subject`
	patchSubjRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Subject: &TriggerSubjectPatchSpec{Expr: &restoredSubj},
	})
	require.Equal(t, http.StatusOK, patchSubjRec.Code, patchSubjRec.Body.String())
	require.Equal(t, http.StatusOK, f.doTrigger(t, f.ownerAuth, created.ID+"/pause", http.MethodPost, nil).Code)
	resumeRec := f.doTrigger(t, f.ownerAuth, created.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())
}

func TestTriggersAuthoring_CELResultTypeMismatchRejected(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cel-type-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// 1. Static string-typed and int-typed match rules -> 400 invalid_rule (category=result_type).
	// (Note: event.* and trigger.* index map(string, dyn) and therefore have static type dyn;
	// string(...), literals, and size(...) have static non-bool types.)
	for i, expr := range []string{`"not-a-bool"`, `string(event.subject)`, `size(trigger.id)`} {
		strMatchRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   fmt.Sprintf("bad-str-match-%d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Expr: expr},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusBadRequest, strMatchRec.Code, "expr=%q body=%s", expr, strMatchRec.Body.String())
		errBody := decodeTriggerRuleError(t, strMatchRec)
		assert.Equal(t, ErrCodeInvalidRule, errBody.Code)
		assert.Equal(t, "match", errBody.Details["field"])
		assert.Equal(t, "filter", errBody.Details["kind"])
		assert.Equal(t, "result_type", errBody.Details["category"])
		assert.GreaterOrEqual(t, errBody.Details["line"].(float64), float64(1))
		assert.GreaterOrEqual(t, errBody.Details["column"].(float64), float64(1))
	}

	// 2. Bool-typed and int-typed subject rules -> 400 invalid_rule (category=result_type).
	for i, expr := range []string{`event.type == "webhook.received"`, `size(event.subject)`} {
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:    fmt.Sprintf("bad-subj-type-%d", i),
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Subject: &TriggerSubjectSpec{Expr: expr},
			Action:  TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		eb := decodeTriggerRuleError(t, rec)
		assert.Equal(t, ErrCodeInvalidRule, eb.Code)
		assert.Equal(t, "subject", eb.Details["field"])
		assert.Equal(t, "subject", eb.Details["kind"])
		assert.Equal(t, "result_type", eb.Details["category"])
		assert.GreaterOrEqual(t, eb.Details["line"].(float64), float64(1))
		assert.GreaterOrEqual(t, eb.Details["column"].(float64), float64(1))
	}

	// 3. PATCH into wrong result type is also rejected 400 without mutating stored trigger.
	okRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "patch-type-guard",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, okRec.Code)
	var okTr TriggerResponse
	require.NoError(t, json.Unmarshal(okRec.Body.Bytes(), &okTr))

	badMatch := `string(trigger.id)`
	patchBad := f.doTrigger(t, f.ownerAuth, okTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Expr: &badMatch},
	})
	require.Equal(t, http.StatusBadRequest, patchBad.Code, patchBad.Body.String())
	assert.Equal(t, "result_type", decodeTriggerRuleError(t, patchBad).Details["category"])
}

func TestTriggersAuthoring_CELNonLiteralMatchesRejected(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cel-regex-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	matchRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "dyn-regex-match",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: `event.subject.matches(event.id)`},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, matchRec.Code, matchRec.Body.String())
	matchErr := decodeTriggerRuleError(t, matchRec)
	assert.Equal(t, ErrCodeInvalidRule, matchErr.Code)
	assert.Equal(t, "match", matchErr.Details["field"])
	assert.Equal(t, "regex_literal_required", matchErr.Details["category"])
	assert.Equal(t, float64(1), matchErr.Details["line"])
	assert.Greater(t, matchErr.Details["column"].(float64), float64(1))

	subjRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "dyn-regex-subj",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Subject: &TriggerSubjectSpec{Expr: `event.subject.matches(event.type) ? "a" : "b"`},
		Action:  TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, subjRec.Code, subjRec.Body.String())
	subjErr := decodeTriggerRuleError(t, subjRec)
	assert.Equal(t, ErrCodeInvalidRule, subjErr.Code)
	assert.Equal(t, "subject", subjErr.Details["field"])
	assert.Equal(t, "regex_literal_required", subjErr.Details["category"])
}

func TestTriggersAuthoring_CELOversizeRulesRejected(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cel-size-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Exact boundary (4096 B match, 1024 B subject) succeeds.
	exactMatch := "true" + strings.Repeat(" ", 4096-len("true"))
	exactSubj := `"s"` + strings.Repeat(" ", 1024-len(`"s"`))
	exactRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "exact-cap-rules",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:   &TriggerMatchSpec{Expr: exactMatch},
		Subject: &TriggerSubjectSpec{Expr: exactSubj},
		Action:  TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, exactRec.Code, exactRec.Body.String())

	// Oversize match (4097 B) -> 400 invalid_rule (category=size).
	overMatchRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "oversize-match",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: exactMatch + " "},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, overMatchRec.Code, overMatchRec.Body.String())
	overMatchErr := decodeTriggerRuleError(t, overMatchRec)
	assert.Equal(t, ErrCodeInvalidRule, overMatchErr.Code)
	assert.Equal(t, "match", overMatchErr.Details["field"])
	assert.Equal(t, "size", overMatchErr.Details["category"])
	assert.Equal(t, float64(1), overMatchErr.Details["line"])
	assert.Equal(t, float64(1), overMatchErr.Details["column"])

	// Oversize subject (1025 B) -> 400 invalid_rule (category=size).
	overSubjRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "oversize-subj",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Subject: &TriggerSubjectSpec{Expr: exactSubj + " "},
		Action:  TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, overSubjRec.Code, overSubjRec.Body.String())
	overSubjErr := decodeTriggerRuleError(t, overSubjRec)
	assert.Equal(t, ErrCodeInvalidRule, overSubjErr.Code)
	assert.Equal(t, "subject", overSubjErr.Details["field"])
	assert.Equal(t, "size", overSubjErr.Details["category"])

	// Over-long regex literal (> 512 B) inside <= 4096 B match rule -> 400 category=size.
	overRegexRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "oversize-regex",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: `event.subject.matches("` + strings.Repeat("a", 513) + `")`},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, overRegexRec.Code, overRegexRec.Body.String())
	assert.Equal(t, "size", decodeTriggerRuleError(t, overRegexRec).Details["category"])
}

func TestTriggersAuthoring_CELSyntaxAndUnknownVariableWithPosition(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "cel-pos-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// 1. Multiline syntax error reports line 2 and column > 1 (preserving leading whitespace).
	syntaxRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "syntax-pos",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: "event.type == \"ok\" &&\n    ("},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, syntaxRec.Code, syntaxRec.Body.String())
	syntaxErr := decodeTriggerRuleError(t, syntaxRec)
	assert.Equal(t, ErrCodeInvalidRule, syntaxErr.Code)
	assert.Equal(t, "syntax", syntaxErr.Details["category"])
	assert.Equal(t, float64(2), syntaxErr.Details["line"])
	assert.GreaterOrEqual(t, syntaxErr.Details["column"].(float64), float64(5))
	assert.Contains(t, syntaxErr.Message, "line 2, column")

	// 2. Multiline unknown variable reports exact line 2, column 3 and names the variable.
	unknownVarRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "unknown-var-pos",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: "\n  unknown_var == 1"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, unknownVarRec.Code, unknownVarRec.Body.String())
	uvErr := decodeTriggerRuleError(t, unknownVarRec)
	assert.Equal(t, ErrCodeInvalidRule, uvErr.Code)
	assert.Equal(t, "unknown_variable", uvErr.Details["category"])
	assert.Equal(t, float64(2), uvErr.Details["line"])
	assert.Equal(t, float64(3), uvErr.Details["column"])
	assert.Contains(t, uvErr.Details["message"].(string), "unknown_var")

	// 3. Unknown function -> category=unknown_function.
	unknownFnRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "unknown-fn-pos",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Subject: &TriggerSubjectSpec{Expr: "bogusFunc(event.id)"},
		Action:  TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, unknownFnRec.Code, unknownFnRec.Body.String())
	ufErr := decodeTriggerRuleError(t, unknownFnRec)
	assert.Equal(t, ErrCodeInvalidRule, ufErr.Code)
	assert.Equal(t, "unknown_function", ufErr.Details["category"])
}

func TestTriggersAuthoring_UnsupportedDialectReturns400Never500(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "dialect-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Create with unknown ("rego") and denied ("system_one") dialects on match and subject,
	// with both empty and non-empty expr -> always 400 ErrCodeInvalidRule, never 500.
	for i, tc := range []struct {
		slot    string
		dialect string
		expr    string
	}{
		{"match", "rego", ""},
		{"match", "rego", "true"},
		{"match", "system_one", "check deploy"},
		{"subject", "rego", ""},
		{"subject", "system_one", "derive subject"},
	} {
		req := CreateTriggerRequest{
			Name:   fmt.Sprintf("bad-dialect-%d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		}
		if tc.slot == "match" {
			req.Match = &TriggerMatchSpec{Dialect: tc.dialect, Expr: tc.expr}
		} else {
			req.Subject = &TriggerSubjectSpec{Dialect: tc.dialect, Expr: tc.expr}
		}
		rec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, "case %d (%+v): %s", i, tc, rec.Body.String())
		eb := decodeTriggerRuleError(t, rec)
		assert.Equal(t, ErrCodeInvalidRule, eb.Code)
		assert.Equal(t, tc.slot, eb.Details["field"])
		assert.Equal(t, tc.dialect, eb.Details["dialect"])
		assert.Equal(t, "trigger", eb.Details["consumer"])
		assert.Equal(t, ruleengine.ReasonDialectNotAllowed, eb.Details["reason"])
		assert.Contains(t, eb.Message, ruleengine.ReasonDialectNotAllowed)
	}

	// PATCH into unsupported dialect -> 400 ErrCodeInvalidRule with ReasonDialectNotAllowed.
	okRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "dialect-patch-base",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, okRec.Code)
	var okTr TriggerResponse
	require.NoError(t, json.Unmarshal(okRec.Body.Bytes(), &okTr))

	sysOne := "system_one"
	patchRec := f.doTrigger(t, f.ownerAuth, okTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Dialect: &sysOne},
	})
	require.Equal(t, http.StatusBadRequest, patchRec.Code, patchRec.Body.String())
	patchErr := decodeTriggerRuleError(t, patchRec)
	assert.Equal(t, ErrCodeInvalidRule, patchErr.Code)
	assert.Equal(t, ruleengine.ReasonDialectNotAllowed, patchErr.Details["reason"])

	// Resume on a stored paused trigger with a disallowed dialect or broken rule -> 400 (never 500), stays paused.
	now := time.Now().UTC()
	pausedBadDialectID := api.NewUUID()
	require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
		ID:             pausedBadDialectID,
		ProjectID:      f.projectID,
		Name:           "stored-bad-dialect",
		Status:         store.TriggerStatusPaused,
		SourceKind:     store.TriggerSourceKindWebhook,
		SourceID:       ep.ID,
		MatchTypes:     []string{"webhook.received"},
		MatchDialect:   "system_one",
		MatchRule:      "admit event",
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
	resumeBadDialect := f.doTrigger(t, f.ownerAuth, pausedBadDialectID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusBadRequest, resumeBadDialect.Code, resumeBadDialect.Body.String())
	resumeErr := decodeTriggerRuleError(t, resumeBadDialect)
	assert.Equal(t, ErrCodeInvalidRule, resumeErr.Code)
	assert.Equal(t, "system_one", resumeErr.Details["dialect"])
	assert.Equal(t, ruleengine.ReasonDialectNotAllowed, resumeErr.Details["reason"])

	stillPaused, err := f.store.GetTrigger(ctx, pausedBadDialectID)
	require.NoError(t, err)
	assert.Equal(t, store.TriggerStatusPaused, stillPaused.Status)

	// Ungranted consumer on a bare rule engine -> 400 ErrCodeInvalidRule with ReasonConsumerNotRegistered (never 500).
	bareEng, _, err := newRuleEngine()
	require.NoError(t, err)
	origRules, origTriggerRules := f.srv.rules, f.srv.triggerRules
	f.srv.rules = bareEng
	f.srv.triggerRules = nil
	t.Cleanup(func() {
		f.srv.rules = origRules
		f.srv.triggerRules = origTriggerRules
	})
	ungrantedRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "ungranted-consumer",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, ungrantedRec.Code, ungrantedRec.Body.String())
	ungrantedErr := decodeTriggerRuleError(t, ungrantedRec)
	assert.Equal(t, ErrCodeInvalidRule, ungrantedErr.Code)
	assert.Equal(t, ruleengine.ReasonConsumerNotRegistered, ungrantedErr.Details["reason"])
	assert.Contains(t, ungrantedErr.Message, ruleengine.ReasonConsumerNotRegistered)
	f.srv.rules = origRules
	f.srv.triggerRules = origTriggerRules
}

func TestTriggersAuthoring_RulePatchBumpsAuthorizationRevision(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "rev-bump-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "rev-bump-rules",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "v1"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code)
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &tr))
	require.Equal(t, 1, tr.AuthorizationRevision)

	celDialect := "cel"
	matchExpr := `event.type == "webhook.received"`
	subjExpr := `event.subject`

	steps := []struct {
		actor   Identity
		actorID string
		patch   UpdateTriggerRequest
		wantRev int
	}{
		{
			actor:   f.adminAuth,
			actorID: f.admin.ID,
			patch:   UpdateTriggerRequest{Match: &TriggerMatchPatchSpec{Expr: &matchExpr}},
			wantRev: 2,
		},
		{
			actor:   f.ownerAuth,
			actorID: f.owner.ID,
			patch:   UpdateTriggerRequest{Match: &TriggerMatchPatchSpec{Dialect: &celDialect}},
			wantRev: 3,
		},
		{
			actor:   f.adminAuth,
			actorID: f.admin.ID,
			patch:   UpdateTriggerRequest{Subject: &TriggerSubjectPatchSpec{Expr: &subjExpr}},
			wantRev: 4,
		},
		{
			actor:   f.ownerAuth,
			actorID: f.owner.ID,
			patch:   UpdateTriggerRequest{Subject: &TriggerSubjectPatchSpec{Dialect: &celDialect}},
			wantRev: 5,
		},
	}

	for i, st := range steps {
		rec := f.doTrigger(t, st.actor, tr.ID, http.MethodPatch, st.patch)
		require.Equal(t, http.StatusOK, rec.Code, "step %d: %s", i, rec.Body.String())
		assert.Equal(t, fmt.Sprintf("%q", fmt.Sprintf("%d", st.wantRev)), rec.Header().Get("ETag"))
		stored, err := f.store.GetTrigger(ctx, tr.ID)
		require.NoError(t, err)
		assert.Equal(t, st.wantRev, stored.AuthorizationRevision, "step %d", i)
		assert.Equal(t, st.actorID, stored.InitiatorPrincipalID, "step %d", i)
	}
}

func TestTriggersAuthoring_AgentParityWithUserAuthoring(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "agent-cel-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Valid CEL authoring by agent succeeds identically to user authoring.
	okRec := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-cel-ok",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: `has(event.payload) && has(event.payload.ok) && event.payload.ok == true`},
		Subject: &TriggerSubjectSpec{
			Expr: `has(event.payload) && has(event.payload.pr) ? string(event.payload.pr) : string(event.subject)`,
		},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "pr event"},
	})
	require.Equal(t, http.StatusCreated, okRec.Code, okRec.Body.String())
	var tr TriggerResponse
	require.NoError(t, json.Unmarshal(okRec.Body.Bytes(), &tr))
	assert.Equal(t, 1, tr.AuthorizationRevision)

	// Agent pause + resume re-compiles stored CEL rules and bumps revision to 2
	// (while PATCH by an agent remains 403 per Stage 1 trigger edit policy).
	nextExpr := `has(event.attributes.event) && event.attributes.event == "deploy.finished"`
	patchRec := f.doTrigger(t, f.agentAAuth, tr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Expr: &nextExpr},
	})
	require.Equal(t, http.StatusForbidden, patchRec.Code, patchRec.Body.String())

	require.Equal(t, http.StatusOK, f.doTrigger(t, f.agentAAuth, tr.ID+"/pause", http.MethodPost, nil).Code)
	resumeRec := f.doTrigger(t, f.agentAAuth, tr.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, resumeRec.Code, resumeRec.Body.String())
	require.NoError(t, json.Unmarshal(resumeRec.Body.Bytes(), &tr))
	assert.Equal(t, 2, tr.AuthorizationRevision)

	// Agent invalid CEL and unsupported dialect return identical 400 structured errors to user authoring.
	ownerEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "owner-cel-parity-ep"})
	require.Equal(t, http.StatusCreated, ownerEpRec.Code)
	var ownerEp WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(ownerEpRec.Body.Bytes(), &ownerEp))

	agentBadCEL := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-cel-bad",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: `string(event.subject)`},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	ownerBadCEL := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "owner-cel-bad",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ownerEp.ID},
		Match:  &TriggerMatchSpec{Expr: `string(event.subject)`},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, agentBadCEL.Code, agentBadCEL.Body.String())
	require.Equal(t, http.StatusBadRequest, ownerBadCEL.Code, ownerBadCEL.Body.String())
	assert.Equal(t, decodeTriggerRuleError(t, ownerBadCEL), decodeTriggerRuleError(t, agentBadCEL))

	agentBadDialect := f.doTrigger(t, f.agentAAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "agent-dialect-bad",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Dialect: "rego", Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	ownerBadDialect := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "owner-dialect-bad",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ownerEp.ID},
		Match:  &TriggerMatchSpec{Dialect: "rego", Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, agentBadDialect.Code, agentBadDialect.Body.String())
	require.Equal(t, http.StatusBadRequest, ownerBadDialect.Code, ownerBadDialect.Body.String())
	assert.Equal(t, decodeTriggerRuleError(t, ownerBadDialect), decodeTriggerRuleError(t, agentBadDialect))
}

func TestTriggersAuthoring_PatchDispatchFieldOverBrokenStoredRuleRejected(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "stored-broken-rule-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	now := time.Now().UTC()
	origPayload := `{"agentId":"` + f.targetAgent.ID + `","message":"original message"}`

	for _, tc := range []struct {
		name        string
		matchRule   string
		subjectRule string
		wantField   string
	}{
		{
			name:        "stored_broken_match_rule",
			matchRule:   `string(event.subject)`,
			subjectRule: "",
			wantField:   "match",
		},
		{
			name:        "stored_broken_subject_rule",
			matchRule:   `true`,
			subjectRule: `event.type == "webhook.received"`,
			wantField:   "subject",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trID := api.NewUUID()
			require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
				ID:             trID,
				ProjectID:      f.projectID,
				Name:           "broken-" + tc.wantField + "-" + trID[:8],
				Status:         store.TriggerStatusActive,
				SourceKind:     store.TriggerSourceKindWebhook,
				SourceID:       ep.ID,
				MatchTypes:     []string{"webhook.received"},
				MatchDialect:   "cel",
				MatchRule:      tc.matchRule,
				ActionType:     "message",
				ActionPayload:  origPayload,
				SubjectDialect: "cel",
				SubjectRule:    tc.subjectRule,
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

			patchRec := f.doTrigger(t, f.adminAuth, trID, http.MethodPatch, UpdateTriggerRequest{
				Action: &TriggerActionSpec{
					Type:    "message",
					AgentID: f.targetAgent.ID,
					Message: "updated message",
				},
			})
			require.Equal(t, http.StatusBadRequest, patchRec.Code, patchRec.Body.String())
			eb := decodeTriggerRuleError(t, patchRec)
			assert.Equal(t, ErrCodeInvalidRule, eb.Code)
			assert.Equal(t, tc.wantField, eb.Details["field"])
			assert.Equal(t, "result_type", eb.Details["category"])

			stored, err := f.store.GetTrigger(ctx, trID)
			require.NoError(t, err)
			assert.Equal(t, origPayload, stored.ActionPayload, "stored action payload must remain unchanged")
			assert.Equal(t, 1, stored.AuthorizationRevision, "authorization revision must remain unchanged")
			assert.Equal(t, f.owner.ID, stored.InitiatorPrincipalID, "initiator attribution must remain unchanged")
			assert.Equal(t, tc.matchRule, stored.MatchRule)
			assert.Equal(t, tc.subjectRule, stored.SubjectRule)
		})
	}
}

func TestTriggersAuthoring_RejectControlAndFormatCharactersInRules(t *testing.T) {
	badExprs := []struct {
		label string
		expr  string
	}{
		{"nul", "event.type == \"\x00\""},
		{"bel_osc", "event.type == \"\x1b]0;pwned\x07\x1b[2J\""},
		{"ansi_comment", "true // \x1b[31mred"},
		{"vt", "true\x0b"},
		{"ff", "true\x0c"},
		{"del", "event.type == \"a\x7fb\""},
		{"c1_csi", "event.type == \"a\u009b2Jb\""},
		{"bidi_rlo_cf", "event.type == \"a\u202eb\""},
		{"bidi_lri_cf", "event.type == \"a\u2066b\""},
		{"zwsp_cf", "event.type == \"a\u200bb\""},
		{"bom_cf", "\ufefftrue"},
		{"line_sep_zl", "true // \u2028next"},
		{"para_sep_zp", "true // \u2029next"},
		{"lone_cr", "true // benign\r1/1 [cel] forged"},
		{"crlf", "event.type == \"webhook.received\" &&\r\ntrue"},
		{"invalid_utf8", "event.type == \"\xff\""},
	}
	for _, tc := range badExprs {
		t.Run("unit_"+tc.label, func(t *testing.T) {
			assert.Error(t, validateTriggerRuleExpr("match.expr", tc.expr))
			assert.Error(t, validateTriggerRuleExpr("subject.expr", tc.expr))
		})
	}
	require.NoError(t, validateTriggerRuleExpr("match.expr", "event.type == \"webhook.received\" &&\n\ttrue"))

	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "ctrl-rule-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// Multiline rules with \n and \t remain valid at authoring; \r is refused.
	multilineMatch := "event.type == \"webhook.received\" &&\n\thas(event.attributes.subject)"
	multilineSubj := "has(event.subject) ?\n\tstring(event.subject) : \"default\""
	validRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:    "multiline-valid-tr",
		Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:   &TriggerMatchSpec{Expr: multilineMatch},
		Subject: &TriggerSubjectSpec{Expr: multilineSubj},
		Action:  TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, validRec.Code, validRec.Body.String())
	var validTr TriggerResponse
	require.NoError(t, json.Unmarshal(validRec.Body.Bytes(), &validTr))

	// Create and PATCH reject C0 control (including \r) and Unicode Cf characters in match.expr and subject.expr (400).
	for i, bad := range []struct {
		matchExpr string
		subjExpr  string
	}{
		{matchExpr: "event.type == \"\x1b]0;pwned\x07\x1b[2J\"", subjExpr: "\"\x1b[31msubj\""},
		{matchExpr: "event.type == \"a\u202eb\"", subjExpr: "\"subj\u202e\""},
		{matchExpr: "true // \x1b[31mred", subjExpr: "\"subj\" // \u200bzwsp"},
		{matchExpr: "true // \r1/1 [cel] spoof", subjExpr: "\"subj\rspoof\""},
		{matchExpr: "true &&\r\nevent.subject != \"\"", subjExpr: "\"a\" +\r\n\"b\""},
	} {
		badMatchCreate := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   fmt.Sprintf("bad-match-ctrl-%d", i),
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Match:  &TriggerMatchSpec{Expr: bad.matchExpr},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusBadRequest, badMatchCreate.Code, badMatchCreate.Body.String())
		matchErr := decodeTriggerRuleError(t, badMatchCreate)
		assert.Equal(t, ErrCodeInvalidRule, matchErr.Code)
		assert.Contains(t, matchErr.Message, "match.expr must not contain control or format characters")

		badSubjCreate := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:    fmt.Sprintf("bad-subj-ctrl-%d", i),
			Source:  TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
			Subject: &TriggerSubjectSpec{Expr: bad.subjExpr},
			Action:  TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
		})
		require.Equal(t, http.StatusBadRequest, badSubjCreate.Code, badSubjCreate.Body.String())
		subjErr := decodeTriggerRuleError(t, badSubjCreate)
		assert.Equal(t, ErrCodeValidationError, subjErr.Code)
		assert.Contains(t, subjErr.Message, "subject.expr must not contain control or format characters")

		matchCopy := bad.matchExpr
		patchMatchRec := f.doTrigger(t, f.ownerAuth, validTr.ID, http.MethodPatch, UpdateTriggerRequest{
			Match: &TriggerMatchPatchSpec{Expr: &matchCopy},
		})
		require.Equal(t, http.StatusBadRequest, patchMatchRec.Code, patchMatchRec.Body.String())
		assert.Contains(t, decodeTriggerRuleError(t, patchMatchRec).Message, "match.expr must not contain control or format characters")

		subjCopy := bad.subjExpr
		patchSubjRec := f.doTrigger(t, f.ownerAuth, validTr.ID, http.MethodPatch, UpdateTriggerRequest{
			Subject: &TriggerSubjectPatchSpec{Expr: &subjCopy},
		})
		require.Equal(t, http.StatusBadRequest, patchSubjRec.Code, patchSubjRec.Body.String())
		assert.Contains(t, decodeTriggerRuleError(t, patchSubjRec).Message, "subject.expr must not contain control or format characters")
	}

	// Stored paused trigger with bidi/control characters in MatchRule or SubjectRule is rejected 400 on resume and dispatch PATCH.
	now := time.Now().UTC()
	for _, tc := range []struct {
		name      string
		matchRule string
		subjRule  string
		wantField string
	}{
		{"stored_bidi_match", "event.type == \"a\u202eb\"", "", "match.expr"},
		{"stored_esc_subject", "true", "\"\x1b[2J\"", "subject.expr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			trID := api.NewUUID()
			require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
				ID:             trID,
				ProjectID:      f.projectID,
				Name:           "paused-ctrl-" + trID[:8],
				Status:         store.TriggerStatusPaused,
				SourceKind:     store.TriggerSourceKindWebhook,
				SourceID:       ep.ID,
				MatchTypes:     []string{"webhook.received"},
				MatchDialect:   "cel",
				MatchRule:      tc.matchRule,
				ActionType:     "message",
				ActionPayload:  `{"agentId":"` + f.targetAgent.ID + `","message":"hi"}`,
				SubjectDialect: "cel",
				SubjectRule:    tc.subjRule,
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

			resumeRec := f.doTrigger(t, f.ownerAuth, trID+"/resume", http.MethodPost, nil)
			require.Equal(t, http.StatusBadRequest, resumeRec.Code, resumeRec.Body.String())
			assert.Contains(t, decodeTriggerRuleError(t, resumeRec).Message, tc.wantField+" must not contain control or format characters")

			dispatchPatchRec := f.doTrigger(t, f.ownerAuth, trID, http.MethodPatch, UpdateTriggerRequest{
				Action: &TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "updated"},
			})
			require.Equal(t, http.StatusBadRequest, dispatchPatchRec.Code, dispatchPatchRec.Body.String())
			assert.Contains(t, decodeTriggerRuleError(t, dispatchPatchRec).Message, tc.wantField+" must not contain control or format characters")

			stored, err := f.store.GetTrigger(ctx, trID)
			require.NoError(t, err)
			assert.Equal(t, store.TriggerStatusPaused, stored.Status)
			assert.Equal(t, 1, stored.AuthorizationRevision)
		})
	}
}

func TestTriggersAuthoring_UninitializedRuleEngineReturnsGeneric500(t *testing.T) {
	f := setupTriggersTestFixture(t)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "uninit-engine-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	baseRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "uninit-base-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, baseRec.Code, baseRec.Body.String())
	var baseTr TriggerResponse
	require.NoError(t, json.Unmarshal(baseRec.Body.Bytes(), &baseTr))
	require.Equal(t, http.StatusOK, f.doTrigger(t, f.ownerAuth, baseTr.ID+"/pause", http.MethodPost, nil).Code)

	origRules, origTriggerRules := f.srv.rules, f.srv.triggerRules
	t.Cleanup(func() {
		f.srv.rules = origRules
		f.srv.triggerRules = origTriggerRules
	})

	assertGeneric500 := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
		eb := decodeTriggerRuleError(t, rec)
		assert.Equal(t, ErrCodeInternalError, eb.Code)
		assert.Equal(t, "Internal server error", eb.Message)
		assert.Empty(t, eb.Details)
		assert.NotContains(t, rec.Body.String(), "not initialized")
		assert.NotContains(t, rec.Body.String(), "unknown CEL environment")
		assert.NotContains(t, rec.Body.String(), ErrCodeInvalidRule)
	}

	// 1. Nil rule engine state -> 500 Internal server error (never 400 invalid_rule or internal text).
	f.srv.rules = nil
	f.srv.triggerRules = nil

	assertGeneric500(t, f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "uninit-create",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	}))

	newExpr := `event.type == "webhook.received"`
	assertGeneric500(t, f.doTrigger(t, f.ownerAuth, baseTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Expr: &newExpr},
	}))

	assertGeneric500(t, f.doTrigger(t, f.ownerAuth, baseTr.ID+"/resume", http.MethodPost, nil))

	// 2. Half-initialized state (s.rules grants "trigger" but s.triggerRules is nil) -> generic 500.
	misEng, _, err := newRuleEngine()
	require.NoError(t, err)
	require.NoError(t, misEng.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)))
	f.srv.rules = misEng
	f.srv.triggerRules = nil

	assertGeneric500(t, f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "misconfig-create",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	}))

	// 3. Direct writeTriggerRuleError check on an arbitrary unexpected internal error -> generic 500, no text leak.
	rec := httptest.NewRecorder()
	writeTriggerRuleError(rec, "match", fmt.Errorf("ruleengine: unknown CEL environment %q", "trigger"))
	assertGeneric500(t, rec)
}

func TestTriggersAndWebhookEndpoints_OversizeRequestBodyReturns413(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	sendRaw := func(method, path string, raw []byte, handler func(http.ResponseWriter, *http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		reqCtx := contextWithIdentity(req.Context(), f.ownerAuth)
		reqCtx = contextWithCredentialContext(reqCtx, credentialContextForIdentity(f.ownerAuth))
		rec := httptest.NewRecorder()
		handler(rec, req.WithContext(reqCtx))
		return rec
	}

	assert413 := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		eb := decodeTriggerRuleError(t, rec)
		assert.Equal(t, "payload_too_large", eb.Code)
	}

	// 1. POST /webhook-endpoints: exact 64 KiB body succeeds (201), 64 KiB + 1 byte returns 413.
	epPath := "/api/v1/projects/" + f.projectID + "/webhook-endpoints"
	epPrefix := `{"name":"cap-ep","description":"ok"}`
	epExact := []byte(epPrefix + strings.Repeat(" ", maxTriggerRequestBodyBytes-len(epPrefix)))
	require.Len(t, epExact, maxTriggerRequestBodyBytes)

	epOKRec := sendRaw(http.MethodPost, epPath, epExact, func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleWebhookEndpoints(w, r, f.projectID, "")
	})
	require.Equal(t, http.StatusCreated, epOKRec.Code, epOKRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epOKRec.Body.Bytes(), &ep))

	epOverJSON := []byte(`{"name":"over-ep","description":"` + strings.Repeat("a", maxTriggerRequestBodyBytes) + `"}`)
	assert413(t, sendRaw(http.MethodPost, epPath, epOverJSON, func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleWebhookEndpoints(w, r, f.projectID, "")
	}))
	assert413(t, sendRaw(http.MethodPost, epPath, append(append([]byte{}, epExact...), ' '), func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleWebhookEndpoints(w, r, f.projectID, "")
	}))

	// 2. POST /triggers: exact 64 KiB body succeeds (201), 64 KiB + 1 byte and oversize JSON object return 413.
	trPath := "/api/v1/projects/" + f.projectID + "/triggers"
	trBase, err := json.Marshal(CreateTriggerRequest{
		Name:   "cap-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hello"},
	})
	require.NoError(t, err)
	trExact := append(append([]byte{}, trBase...), bytes.Repeat([]byte(" "), maxTriggerRequestBodyBytes-len(trBase))...)
	require.Len(t, trExact, maxTriggerRequestBodyBytes)

	trOKRec := sendRaw(http.MethodPost, trPath, trExact, func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleTriggers(w, r, f.projectID, "")
	})
	require.Equal(t, http.StatusCreated, trOKRec.Code, trOKRec.Body.String())
	var createdTr TriggerResponse
	require.NoError(t, json.Unmarshal(trOKRec.Body.Bytes(), &createdTr))

	trOverJSON := []byte(`{"name":"over-tr","source":{"kind":"webhook","id":"` + ep.ID + `"},"action":{"type":"message","agentId":"` + f.targetAgent.ID + `","message":"` + strings.Repeat("m", maxTriggerRequestBodyBytes) + `"}}`)
	assert413(t, sendRaw(http.MethodPost, trPath, trOverJSON, func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleTriggers(w, r, f.projectID, "")
	}))
	assert413(t, sendRaw(http.MethodPost, trPath, append(append([]byte{}, trExact...), ' '), func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleTriggers(w, r, f.projectID, "")
	}))

	// 3. PATCH /triggers/{id}: exact 64 KiB body succeeds (200), > 64 KiB returns 413 and leaves trigger unchanged.
	patchPath := trPath + "/" + createdTr.ID
	patchPrefix := `{"description":"patched-at-cap"}`
	patchExact := []byte(patchPrefix + strings.Repeat(" ", maxTriggerRequestBodyBytes-len(patchPrefix)))
	require.Len(t, patchExact, maxTriggerRequestBodyBytes)

	patchOKRec := sendRaw(http.MethodPatch, patchPath, patchExact, func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleTriggers(w, r, f.projectID, createdTr.ID)
	})
	require.Equal(t, http.StatusOK, patchOKRec.Code, patchOKRec.Body.String())

	patchOverJSON := []byte(`{"description":"` + strings.Repeat("d", maxTriggerRequestBodyBytes) + `"}`)
	assert413(t, sendRaw(http.MethodPatch, patchPath, patchOverJSON, func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleTriggers(w, r, f.projectID, createdTr.ID)
	}))
	assert413(t, sendRaw(http.MethodPatch, patchPath, append(append([]byte{}, patchExact...), ' '), func(w http.ResponseWriter, r *http.Request) {
		f.srv.handleTriggers(w, r, f.projectID, createdTr.ID)
	}))

	stored, err := f.store.GetTrigger(ctx, createdTr.ID)
	require.NoError(t, err)
	assert.Equal(t, "patched-at-cap", stored.Description)
}

func TestTriggersAuthoring_MatchRulesArrayAndScalarSugarMirrorAndAmbiguousMatch(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	// 1. Read normalization helper triggerMatchRules (§2.1, R1 fail-closed):
	//    - MatchRulesCorrupt returns errCorruptTriggerMatchRules (never falls open to MatchRule or nil)
	//    - non-empty MatchRules wins over legacy scalar columns and defaults empty dialect to "cel"
	//    - defensive non-empty MatchRule normalizes to 1-element slice
	//    - empty returns (nil, nil)
	corruptNorm, corruptErr := triggerMatchRules(store.Trigger{
		MatchRule:         "true",
		MatchRulesCorrupt: true,
	})
	assert.Nil(t, corruptNorm)
	require.ErrorIs(t, corruptErr, triggerrule.ErrCorruptMatchRules)
	reason, isSkip := triggerrule.SkipReason(corruptErr)
	assert.True(t, isSkip)
	assert.Equal(t, triggerrule.SkipReasonEvalFailed, reason)

	normMulti, err := triggerMatchRules(store.Trigger{
		MatchDialect: "cel",
		MatchRule:    "legacy-ignored",
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "", Expr: "a"},
			{Dialect: "system_one", Expr: "b"},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []triggerrule.Rule{
		{Dialect: "cel", Expr: "a"},
		{Dialect: "system_one", Expr: "b"},
	}, normMulti)

	normScalar, err := triggerMatchRules(store.Trigger{
		MatchRule: "event.type == 'webhook.received'",
	})
	require.NoError(t, err)
	assert.Equal(t, []triggerrule.Rule{
		{Dialect: "cel", Expr: "event.type == 'webhook.received'"},
	}, normScalar)

	normEmpty, err := triggerMatchRules(store.Trigger{})
	require.NoError(t, err)
	assert.Nil(t, normEmpty)

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "rules-array-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code, epRec.Body.String())
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	// 2. Multi-element rules[] create -> preserves order, defaults dialect to "cel", omits scalar mirror.
	multiRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "multi-rule-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match: &TriggerMatchSpec{
			Types: []string{"webhook.received"},
			Rules: []TriggerMatchRuleSpec{
				{Expr: `event.attributes["branch"] == "main"`},
				{Dialect: "cel", Expr: `has(event.payload.pr)`},
			},
		},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "ship it"},
	})
	require.Equal(t, http.StatusCreated, multiRec.Code, multiRec.Body.String())
	var multiResp TriggerResponse
	require.NoError(t, json.Unmarshal(multiRec.Body.Bytes(), &multiResp))
	wantMultiRules := []TriggerMatchRuleSpec{
		{Dialect: "cel", Expr: `event.attributes["branch"] == "main"`},
		{Dialect: "cel", Expr: `has(event.payload.pr)`},
	}
	assert.Equal(t, wantMultiRules, multiResp.Match.Rules)
	assert.Empty(t, multiResp.Match.Dialect)
	assert.Empty(t, multiResp.Match.Expr)

	var rawMulti map[string]any
	require.NoError(t, json.Unmarshal(multiRec.Body.Bytes(), &rawMulti))
	rawMultiMatch := rawMulti["match"].(map[string]any)
	assert.NotContains(t, rawMultiMatch, "dialect")
	assert.NotContains(t, rawMultiMatch, "expr")

	getMultiRec := f.doTrigger(t, f.ownerAuth, multiResp.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, getMultiRec.Code)
	var getMultiResp TriggerResponse
	require.NoError(t, json.Unmarshal(getMultiRec.Body.Bytes(), &getMultiResp))
	assert.Equal(t, wantMultiRules, getMultiResp.Match.Rules)
	assert.Empty(t, getMultiResp.Match.Dialect)
	assert.Empty(t, getMultiResp.Match.Expr)

	// 3. Scalar sugar create -> stored as 1-element MatchRules, response carries rules[] AND mirrors dialect/expr.
	scalarExpr := `event.attributes["env"] == "prod"`
	scalarRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "scalar-sugar-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Expr: scalarExpr},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "prod"},
	})
	require.Equal(t, http.StatusCreated, scalarRec.Code, scalarRec.Body.String())
	var scalarResp TriggerResponse
	require.NoError(t, json.Unmarshal(scalarRec.Body.Bytes(), &scalarResp))
	assert.Equal(t, []TriggerMatchRuleSpec{{Dialect: "cel", Expr: scalarExpr}}, scalarResp.Match.Rules)
	assert.Equal(t, "cel", scalarResp.Match.Dialect)
	assert.Equal(t, scalarExpr, scalarResp.Match.Expr)

	// 4. Empty rules: [] + empty expr -> Types-only trigger with non-null rules: [] and no scalar mirror.
	emptyRulesRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "empty-rules-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match: &TriggerMatchSpec{
			Types: []string{"webhook.received"},
			Rules: []TriggerMatchRuleSpec{},
		},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "types-only"},
	})
	require.Equal(t, http.StatusCreated, emptyRulesRec.Code, emptyRulesRec.Body.String())
	var rawEmpty map[string]any
	require.NoError(t, json.Unmarshal(emptyRulesRec.Body.Bytes(), &rawEmpty))
	rawEmptyMatch := rawEmpty["match"].(map[string]any)
	assert.Equal(t, []any{}, rawEmptyMatch["rules"])
	assert.NotContains(t, rawEmptyMatch, "dialect")
	assert.NotContains(t, rawEmptyMatch, "expr")

	// 5. Defensive legacy row read via GET /triggers/{id} mirrors 1-element rule.
	legacyID := api.NewUUID()
	now := time.Now().UTC()
	require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
		ID:            legacyID,
		ProjectID:     f.projectID,
		Name:          "defensive-legacy-tr",
		Status:        store.TriggerStatusPaused,
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		MatchRule:     "true",
		ActionType:    "message",
		ActionPayload: `{"agentId":"` + f.targetAgent.ID + `","message":"hi"}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		CreatedBy:     f.owner.ID,
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
	legacyGetRec := f.doTrigger(t, f.ownerAuth, legacyID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, legacyGetRec.Code, legacyGetRec.Body.String())
	var legacyResp TriggerResponse
	require.NoError(t, json.Unmarshal(legacyGetRec.Body.Bytes(), &legacyResp))
	assert.Equal(t, []TriggerMatchRuleSpec{{Dialect: "cel", Expr: "true"}}, legacyResp.Match.Rules)
	assert.Equal(t, "cel", legacyResp.Match.Dialect)
	assert.Equal(t, "true", legacyResp.Match.Expr)

	// 6. Ambiguous match requests (mixing rules[] and scalar dialect/expr) -> 400 invalid_rule {category: ambiguous_match}.
	sendRawTrigger := func(method, triggerPath, rawJSON string) *httptest.ResponseRecorder {
		path := "/api/v1/projects/" + f.projectID + "/triggers"
		if triggerPath != "" {
			path += "/" + triggerPath
		}
		req := httptest.NewRequest(method, path, strings.NewReader(rawJSON))
		req.Header.Set("Content-Type", "application/json")
		reqCtx := contextWithIdentity(req.Context(), f.ownerAuth)
		reqCtx = contextWithCredentialContext(reqCtx, credentialContextForIdentity(f.ownerAuth))
		rec := httptest.NewRecorder()
		f.srv.handleTriggers(rec, req.WithContext(reqCtx), f.projectID, triggerPath)
		return rec
	}

	assertAmbiguous := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		eb := decodeTriggerRuleError(t, rec)
		assert.Equal(t, ErrCodeInvalidRule, eb.Code)
		assert.Equal(t, "match", eb.Details["field"])
		assert.Equal(t, "ambiguous_match", eb.Details["category"])
	}

	for _, rawCreate := range []string{
		`{"name":"ambig-1","source":{"kind":"webhook","id":"` + ep.ID + `"},"match":{"expr":"true","rules":[{"dialect":"cel","expr":"true"}]},"action":{"type":"message","agentId":"` + f.targetAgent.ID + `","message":"hi"}}`,
		`{"name":"ambig-2","source":{"kind":"webhook","id":"` + ep.ID + `"},"match":{"expr":"true","rules":[]},"action":{"type":"message","agentId":"` + f.targetAgent.ID + `","message":"hi"}}`,
		`{"name":"ambig-3","source":{"kind":"webhook","id":"` + ep.ID + `"},"match":{"dialect":"cel","rules":[{"expr":"true"}]},"action":{"type":"message","agentId":"` + f.targetAgent.ID + `","message":"hi"}}`,
	} {
		assertAmbiguous(t, sendRawTrigger(http.MethodPost, "", rawCreate))
	}

	for _, rawPatch := range []string{
		`{"match":{"expr":"true","rules":[{"dialect":"cel","expr":"true"}]}}`,
		`{"match":{"dialect":"cel","rules":[{"dialect":"cel","expr":"true"}]}}`,
		`{"match":{"dialect":"cel"}}`, // scalar dialect-only patch over a 2-element trigger
	} {
		assertAmbiguous(t, sendRawTrigger(http.MethodPatch, multiResp.ID, rawPatch))
	}

	// 7. Empty or whitespace-only scalar fields alongside rules[] behave identically on create and PATCH (O4).
	sameMatchCreateRec := sendRawTrigger(http.MethodPost, "", `{"name":"trimmed-scalar-with-rules","source":{"kind":"webhook","id":"`+ep.ID+`"},"match":{"dialect":"","expr":"   ","rules":[{"dialect":"cel","expr":"true"}]},"action":{"type":"message","agentId":"`+f.targetAgent.ID+`","message":"hi"}}`)
	require.Equal(t, http.StatusCreated, sameMatchCreateRec.Code, sameMatchCreateRec.Body.String())
	var sameMatchCreated TriggerResponse
	require.NoError(t, json.Unmarshal(sameMatchCreateRec.Body.Bytes(), &sameMatchCreated))
	assert.Equal(t, []TriggerMatchRuleSpec{{Dialect: "cel", Expr: "true"}}, sameMatchCreated.Match.Rules)

	sameMatchPatchRec := sendRawTrigger(http.MethodPatch, multiResp.ID, `{"match":{"dialect":"","expr":"   ","rules":[{"dialect":"cel","expr":"true"}]}}`)
	require.Equal(t, http.StatusOK, sameMatchPatchRec.Code, sameMatchPatchRec.Body.String())
}

func TestTriggersAuthoring_CorruptStoredMatchRulesFailsClosed(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "corrupt-rules-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "corrupt-stored-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match: &TriggerMatchSpec{
			Rules: []TriggerMatchRuleSpec{
				{Dialect: "cel", Expr: `event.attributes["env"] == "prod"`},
			},
		},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "prod-only"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))

	// Corrupt match_rules directly in SQL storage to simulate DB-level corruption
	// (including empty-object and renamed-field JSON arrays alongside malformed JSON).
	cs := f.store.(*entadapter.CompositeStore)
	setRawMatchRules := func(raw string) {
		t.Helper()
		if cs.Dialect() == "postgres" {
			_, err := cs.DB().ExecContext(ctx, "UPDATE triggers SET match_rules = $1 WHERE id = $2", raw, created.ID)
			require.NoError(t, err)
		} else {
			_, err := cs.DB().ExecContext(ctx, "UPDATE triggers SET match_rules = ? WHERE id = ?", raw, created.ID)
			require.NoError(t, err)
		}
	}
	for _, probeRaw := range []string{`[{}]`, `[{"dialect":"cel","expression":"x"}]`} {
		setRawMatchRules(probeRaw)
		probeStored, gErr := f.store.GetTrigger(ctx, created.ID)
		require.NoError(t, gErr)
		assert.True(t, probeStored.MatchRulesCorrupt, "probe payload %s must be marked corrupt", probeRaw)
		assert.Nil(t, probeStored.MatchRules)
		pNorm, pErr := triggerMatchRules(*probeStored)
		assert.Nil(t, pNorm)
		require.ErrorIs(t, pErr, triggerrule.ErrCorruptMatchRules)
	}
	const secretCorruptPayload = "{corrupt-unparseable-raw-db-secret"
	setRawMatchRules(secretCorruptPayload)

	storedCorrupt, err := f.store.GetTrigger(ctx, created.ID)
	require.NoError(t, err)
	assert.True(t, storedCorrupt.MatchRulesCorrupt)
	assert.Nil(t, storedCorrupt.MatchRules)
	normRules, normErr := triggerMatchRules(*storedCorrupt)
	assert.Nil(t, normRules)
	require.ErrorIs(t, normErr, triggerrule.ErrCorruptMatchRules)

	// 1. GET /triggers/{id} and GET /triggers surface a content-free broken-rule state (never raw DB bytes, never healthy Types-only).
	getRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodGet, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	assert.NotContains(t, getRec.Body.String(), "corrupt-unparseable-raw-db-secret")
	var getResp TriggerResponse
	require.NoError(t, json.Unmarshal(getRec.Body.Bytes(), &getResp))
	assert.True(t, getResp.Match.RulesBroken)
	assert.Empty(t, getResp.Match.Rules)
	assert.Empty(t, getResp.Match.Dialect)
	assert.Empty(t, getResp.Match.Expr)

	listRec := f.doTrigger(t, f.ownerAuth, "", http.MethodGet, nil)
	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	assert.NotContains(t, listRec.Body.String(), "corrupt-unparseable-raw-db-secret")
	var listResp ListTriggersResponse
	require.NoError(t, json.Unmarshal(listRec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Triggers, 1)
	assert.True(t, listResp.Triggers[0].Match.RulesBroken)

	assertCorruptRefusal := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "corrupt-unparseable-raw-db-secret")
		eb := decodeTriggerRuleError(t, rec)
		assert.Equal(t, ErrCodeInvalidRule, eb.Code)
		assert.Contains(t, eb.Message, "[element 1]")
		elems, ok := eb.Details["elements"].([]any)
		require.True(t, ok)
		require.Len(t, elems, 1)
		e1 := elems[0].(map[string]any)
		assert.Equal(t, float64(1), e1["element"])
		assert.Equal(t, "corrupt_storage", e1["category"])
	}

	// 2. Dispatch-relevant PATCHes without replacing match.rules are refused 400 invalid_rule.
	newTypes := []string{"webhook.deploy.*"}
	celDialect := "cel"
	newSubj := `event.subject`
	assertCorruptRefusal(t, f.doTrigger(t, f.adminAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Action: &TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "must-refuse"},
	}))
	assertCorruptRefusal(t, f.doTrigger(t, f.adminAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Types: &newTypes},
	}))
	assertCorruptRefusal(t, f.doTrigger(t, f.adminAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Dialect: &celDialect},
	}))
	assertCorruptRefusal(t, f.doTrigger(t, f.adminAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Subject: &TriggerSubjectPatchSpec{Expr: &newSubj},
	}))

	// 3. Metadata-only PATCH and pause succeed so operators can quarantine; resume is refused while still corrupt.
	desc := "quarantined corrupt rule"
	metaRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{Description: &desc})
	require.Equal(t, http.StatusOK, metaRec.Code, metaRec.Body.String())
	pauseRec := f.doTrigger(t, f.ownerAuth, created.ID+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, pauseRec.Code, pauseRec.Body.String())
	assertCorruptRefusal(t, f.doTrigger(t, f.ownerAuth, created.ID+"/resume", http.MethodPost, nil))

	// 4. Replacing match.rules via PATCH repairs storage, clears rulesBroken, and allows resume.
	repairedRules := []TriggerMatchRuleSpec{{Dialect: "cel", Expr: `event.attributes["env"] == "prod"`}}
	repairRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Rules: &repairedRules},
	})
	require.Equal(t, http.StatusOK, repairRec.Code, repairRec.Body.String())
	var repairedResp TriggerResponse
	require.NoError(t, json.Unmarshal(repairRec.Body.Bytes(), &repairedResp))
	assert.False(t, repairedResp.Match.RulesBroken)
	assert.Equal(t, repairedRules, repairedResp.Match.Rules)

	resumeOKRec := f.doTrigger(t, f.ownerAuth, created.ID+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, resumeOKRec.Code, resumeOKRec.Body.String())
}

func TestTriggersAuthoring_ScalarMatchColumnsNeverWrittenNonEmptyGuard(t *testing.T) {
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs; ensures store.Trigger.MatchDialect and MatchRule are never written non-empty.
	goFiles, err := globGuardScanDirs(".", "*.go")
	require.NoError(t, err)
	require.NotEmpty(t, goFiles)

	isEmptyStringLit := func(expr ast.Expr) bool {
		lit, ok := expr.(*ast.BasicLit)
		return ok && lit.Kind == token.STRING && lit.Value == `""`
	}
	isFieldMaskTrue := func(recv, rhs ast.Expr) bool {
		recvIdent, ok := recv.(*ast.Ident)
		if !ok || recvIdent.Name != "fields" {
			return false
		}
		rhsIdent, ok := rhs.(*ast.Ident)
		return ok && rhsIdent.Name == "true"
	}

	fset := token.NewFileSet()
	inspectedFiles := 0
	for _, file := range goFiles {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		inspectedFiles++
		parsed, pErr := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, pErr, "parse %s", file)
		ast.Inspect(parsed, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				isMaskLit := false
				if sel, ok := node.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "TriggerFieldMask" {
					isMaskLit = true
				}
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					ident, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					if ident.Name != "MatchDialect" && ident.Name != "MatchRule" {
						continue
					}
					if isMaskLit {
						continue
					}
					if !isEmptyStringLit(kv.Value) {
						pos := fset.Position(kv.Pos())
						t.Errorf("%s:%d writes non-empty %s in composite literal", pos.Filename, pos.Line, ident.Name)
					}
				}
			case *ast.AssignStmt:
				for idx, lhs := range node.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if sel.Sel.Name != "MatchDialect" && sel.Sel.Name != "MatchRule" {
						continue
					}
					if idx >= len(node.Rhs) || (!isEmptyStringLit(node.Rhs[idx]) && !isFieldMaskTrue(sel.X, node.Rhs[idx])) {
						pos := fset.Position(sel.Pos())
						t.Errorf("%s:%d assigns non-empty value to %s", pos.Filename, pos.Line, sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	require.Greater(t, inspectedFiles, 0)

	// Part B: Runtime HTTP + database row verification across create, PATCH, pause, and resume.
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "scalar-guard-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	assertEmptyScalars := func(id string, wantRules []store.TriggerMatchRule) {
		t.Helper()
		stored, gErr := f.store.GetTrigger(ctx, id)
		require.NoError(t, gErr)
		assert.Empty(t, stored.MatchDialect, "triggers.match_dialect must never be written non-empty")
		assert.Empty(t, stored.MatchRule, "triggers.match_rule must never be written non-empty")
		assert.Equal(t, wantRules, stored.MatchRules)
	}

	// 1. Types-only create.
	typesRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "guard-types-only",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, typesRec.Code)
	var typesTr TriggerResponse
	require.NoError(t, json.Unmarshal(typesRec.Body.Bytes(), &typesTr))
	assertEmptyScalars(typesTr.ID, nil)

	// 2. Scalar sugar create.
	sugarRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "guard-scalar-sugar",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Dialect: "cel", Expr: "true"},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, sugarRec.Code)
	var sugarTr TriggerResponse
	require.NoError(t, json.Unmarshal(sugarRec.Body.Bytes(), &sugarTr))
	assertEmptyScalars(sugarTr.ID, []store.TriggerMatchRule{{Dialect: "cel", Expr: "true"}})

	// 3. PATCH scalar -> multi-element -> scalar -> pause -> resume -> clear to empty.
	twoRules := []TriggerMatchRuleSpec{
		{Dialect: "cel", Expr: `event.type == "webhook.received"`},
		{Dialect: "cel", Expr: `has(event.payload.ok)`},
	}
	require.Equal(t, http.StatusOK, f.doTrigger(t, f.ownerAuth, sugarTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Rules: &twoRules},
	}).Code)
	assertEmptyScalars(sugarTr.ID, []store.TriggerMatchRule{
		{Dialect: "cel", Expr: `event.type == "webhook.received"`},
		{Dialect: "cel", Expr: `has(event.payload.ok)`},
	})

	scalarPatchExpr := `event.subject != ""`
	require.Equal(t, http.StatusOK, f.doTrigger(t, f.ownerAuth, sugarTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Expr: &scalarPatchExpr},
	}).Code)
	assertEmptyScalars(sugarTr.ID, []store.TriggerMatchRule{{Dialect: "cel", Expr: scalarPatchExpr}})

	require.Equal(t, http.StatusOK, f.doTrigger(t, f.ownerAuth, sugarTr.ID+"/pause", http.MethodPost, nil).Code)
	require.Equal(t, http.StatusOK, f.doTrigger(t, f.ownerAuth, sugarTr.ID+"/resume", http.MethodPost, nil).Code)
	assertEmptyScalars(sugarTr.ID, []store.TriggerMatchRule{{Dialect: "cel", Expr: scalarPatchExpr}})

	emptyRules := []TriggerMatchRuleSpec{}
	require.Equal(t, http.StatusOK, f.doTrigger(t, f.ownerAuth, sugarTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Rules: &emptyRules},
	}).Code)
	assertEmptyScalars(sugarTr.ID, nil)
}

func TestTriggersAuthoring_MatchRulesCompileAllFailingElementsAndCaps(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "compile-all-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	mixedBadRules := []TriggerMatchRuleSpec{
		{Dialect: "cel", Expr: `event.type == "webhook.received"`},                             // element 1: valid
		{Dialect: "cel", Expr: "true &&\nstring(event.subject)"},                               // element 2: non-bool type error on multiline expr
		{Dialect: "system_one", Expr: "Does this PR touch authentication?"},                    // element 3: ungranted dialect -> dialect_not_allowed
		{Dialect: "cel", Expr: "event.type == \"a\u202eb\""},                                   // element 4: bidi format character
		{Dialect: "Bad-Dialect", Expr: "true"},                                                 // element 5: invalid dialect identifier
		{Dialect: "cel", Expr: `"` + strings.Repeat("x", triggerrule.MaxMatchRuleBytes) + `"`}, // element 6: > 4096 bytes
	}

	assertAllElementsReported := func(t *testing.T, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		eb := decodeTriggerRuleError(t, rec)
		assert.Equal(t, ErrCodeInvalidRule, eb.Code)
		assert.Equal(t, "match", eb.Details["field"])
		assert.Contains(t, eb.Message, "[element 2]")
		assert.Contains(t, eb.Message, "[element 3]")
		assert.NotContains(t, eb.Message, "[element 0]")

		rawElems, ok := eb.Details["elements"].([]any)
		require.True(t, ok, "details.elements must be a JSON array: %v", eb.Details)
		require.Len(t, rawElems, 5, "all 5 failing elements (1-based indices 2..6) must be reported together")

		e1 := rawElems[0].(map[string]any)
		assert.Equal(t, float64(2), e1["element"])
		assert.Equal(t, "type", e1["category"])
		assert.NotEmpty(t, e1["message"])
		assert.GreaterOrEqual(t, e1["line"].(float64), float64(1))
		assert.GreaterOrEqual(t, e1["column"].(float64), float64(1))

		e2 := rawElems[1].(map[string]any)
		assert.Equal(t, float64(3), e2["element"])
		assert.Equal(t, "system_one", e2["dialect"])
		assert.Equal(t, ruleengine.ReasonDialectNotAllowed, e2["reason"])

		e3 := rawElems[2].(map[string]any)
		assert.Equal(t, float64(4), e3["element"])
		assert.Equal(t, "control_characters", e3["category"])
		assert.Contains(t, e3["message"], "control or format characters")

		e4 := rawElems[3].(map[string]any)
		assert.Equal(t, float64(5), e4["element"])
		assert.Equal(t, "dialect", e4["category"])

		e5 := rawElems[4].(map[string]any)
		assert.Equal(t, float64(6), e5["element"])
		assert.Equal(t, "size", e5["category"])
	}

	// 1. Create reports ALL failing elements in one 400 invalid_rule response.
	badCreateRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "all-bad-create",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Rules: mixedBadRules},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	assertAllElementsReported(t, badCreateRec)

	// 2. PATCH reports ALL failing elements and leaves stored trigger untouched.
	eightValid := make([]TriggerMatchRuleSpec, triggerrule.MaxMatchRules)
	for i := range eightValid {
		eightValid[i] = TriggerMatchRuleSpec{
			Dialect: "cel",
			Expr:    fmt.Sprintf(`event.attributes["k%d"] != "drop"`, i),
		}
	}
	okEightRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "eight-rules-ok",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Rules: eightValid},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusCreated, okEightRec.Code, okEightRec.Body.String())
	var okEightTr TriggerResponse
	require.NoError(t, json.Unmarshal(okEightRec.Body.Bytes(), &okEightTr))
	assert.Len(t, okEightTr.Match.Rules, triggerrule.MaxMatchRules)

	badPatchRec := f.doTrigger(t, f.ownerAuth, okEightTr.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Rules: &mixedBadRules},
	})
	assertAllElementsReported(t, badPatchRec)

	// 3. Resume on a paused trigger with multiple failing stored elements reports ALL failing elements.
	pausedBadID := api.NewUUID()
	now := time.Now().UTC()
	storedBadRules := make([]store.TriggerMatchRule, len(mixedBadRules))
	for i, r := range mixedBadRules {
		storedBadRules[i] = store.TriggerMatchRule{Dialect: r.Dialect, Expr: r.Expr}
	}
	require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
		ID:            pausedBadID,
		ProjectID:     f.projectID,
		Name:          "paused-multi-bad",
		Status:        store.TriggerStatusPaused,
		SourceKind:    store.TriggerSourceKindWebhook,
		SourceID:      ep.ID,
		MatchTypes:    []string{"webhook.received"},
		MatchRules:    storedBadRules,
		ActionType:    "message",
		ActionPayload: `{"agentId":"` + f.targetAgent.ID + `","message":"hi"}`,
		SubjectMode:   store.TriggerSubjectModeNew,
		CreatedBy:     f.owner.ID,
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
	assertAllElementsReported(t, f.doTrigger(t, f.ownerAuth, pausedBadID+"/resume", http.MethodPost, nil))

	// 4. Nine elements -> 400 invalid_rule with category == "size".
	nineRules := make([]TriggerMatchRuleSpec, triggerrule.MaxMatchRules+1)
	for i := range nineRules {
		nineRules[i] = TriggerMatchRuleSpec{Dialect: "cel", Expr: "true"}
	}
	nineRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "nine-rules-bad",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Rules: nineRules},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, nineRec.Code, nineRec.Body.String())
	nineErr := decodeTriggerRuleError(t, nineRec)
	assert.Equal(t, ErrCodeInvalidRule, nineErr.Code)
	assert.Equal(t, "size", nineErr.Details["category"])
	assert.Equal(t, float64(triggerrule.MaxMatchRules+1), nineErr.Details["count"])

	// 5. Encoded JSON list > 16 KiB across <= 8 valid <= 4096-byte elements -> 400 invalid_rule {category: "size"}.
	bigValidExpr := fmt.Sprintf(`event.subject != "%s"`, strings.Repeat("z", 3600))
	require.LessOrEqual(t, len(bigValidExpr), triggerrule.MaxMatchRuleBytes)
	fiveBigRules := make([]TriggerMatchRuleSpec, 5)
	for i := range fiveBigRules {
		fiveBigRules[i] = TriggerMatchRuleSpec{Dialect: "cel", Expr: bigValidExpr}
	}
	over16KiBRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "over-16kib-rules",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Rules: fiveBigRules},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "hi"},
	})
	require.Equal(t, http.StatusBadRequest, over16KiBRec.Code, over16KiBRec.Body.String())
	over16Err := decodeTriggerRuleError(t, over16KiBRec)
	assert.Equal(t, ErrCodeInvalidRule, over16Err.Code)
	assert.Equal(t, "size", over16Err.Details["category"])
}

func TestTriggersAuthoring_MatchRulesRevisionBumpAndBrokenElementGuard(t *testing.T) {
	f := setupTriggersTestFixture(t)
	ctx := context.Background()

	epRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "rev-bump-ep"})
	require.Equal(t, http.StatusCreated, epRec.Code)
	var ep WebhookEndpointCreatedResponse
	require.NoError(t, json.Unmarshal(epRec.Body.Bytes(), &ep))

	ruleA := TriggerMatchRuleSpec{Dialect: "cel", Expr: `event.attributes["branch"] == "main"`}
	ruleB := TriggerMatchRuleSpec{Dialect: "cel", Expr: `has(event.payload.pr)`}

	createRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
		Name:   "rev-array-tr",
		Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: ep.ID},
		Match:  &TriggerMatchSpec{Rules: []TriggerMatchRuleSpec{ruleA, ruleB}},
		Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "v1"},
	})
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created TriggerResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	assert.Equal(t, 1, created.AuthorizationRevision)

	// 1. Reordering [A, B] -> [B, A] bumps authorization_revision and re-attributes initiator to admin.
	reordered := []TriggerMatchRuleSpec{ruleB, ruleA}
	reorderRec := f.doTrigger(t, f.adminAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Rules: &reordered},
	})
	require.Equal(t, http.StatusOK, reorderRec.Code, reorderRec.Body.String())
	storedAfterReorder, err := f.store.GetTrigger(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, storedAfterReorder.AuthorizationRevision)
	assert.Equal(t, f.admin.ID, storedAfterReorder.InitiatorPrincipalID)
	assert.Equal(t, []store.TriggerMatchRule{
		{Dialect: "cel", Expr: ruleB.Expr},
		{Dialect: "cel", Expr: ruleA.Expr},
	}, storedAfterReorder.MatchRules)

	// 2. Editing element text bumps authorization_revision and re-attributes initiator back to owner.
	editedText := []TriggerMatchRuleSpec{
		ruleB,
		{Dialect: "cel", Expr: `event.attributes["branch"] == "release"`},
	}
	editTextRec := f.doTrigger(t, f.ownerAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Rules: &editedText},
	})
	require.Equal(t, http.StatusOK, editTextRec.Code, editTextRec.Body.String())
	storedAfterText, err := f.store.GetTrigger(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 3, storedAfterText.AuthorizationRevision)
	assert.Equal(t, f.owner.ID, storedAfterText.InitiatorPrincipalID)

	// 3. Adding a third element bumps authorization_revision and re-attributes initiator to admin.
	threeRules := append(append([]TriggerMatchRuleSpec{}, editedText...), TriggerMatchRuleSpec{
		Dialect: "cel",
		Expr:    `event.subject != ""`,
	})
	addElemRec := f.doTrigger(t, f.adminAuth, created.ID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Rules: &threeRules},
	})
	require.Equal(t, http.StatusOK, addElemRec.Code, addElemRec.Body.String())
	storedAfterAdd, err := f.store.GetTrigger(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, storedAfterAdd.AuthorizationRevision)
	assert.Equal(t, f.admin.ID, storedAfterAdd.InitiatorPrincipalID)

	// 4. PATCH of a dispatch-relevant field over a stored non-compiling element (1-based element 2) -> 400, trigger unchanged.
	brokenID := api.NewUUID()
	now := time.Now().UTC()
	origPayload := `{"agentId":"` + f.targetAgent.ID + `","message":"keep-me"}`
	require.NoError(t, f.store.CreateTrigger(ctx, &store.Trigger{
		ID:         brokenID,
		ProjectID:  f.projectID,
		Name:       "broken-elem-tr",
		Status:     store.TriggerStatusActive,
		SourceKind: store.TriggerSourceKindWebhook,
		SourceID:   ep.ID,
		MatchTypes: []string{"webhook.received"},
		MatchRules: []store.TriggerMatchRule{
			{Dialect: "cel", Expr: "true"},
			{Dialect: "cel", Expr: "string(event.subject)"}, // non-bool element 2
		},
		ActionType:    "message",
		ActionPayload: origPayload,
		SubjectMode:   store.TriggerSubjectModeNew,
		CreatedBy:     f.owner.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  "user",
			InitiatorPrincipalID:    f.owner.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   5,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}))

	actionPatchRec := f.doTrigger(t, f.adminAuth, brokenID, http.MethodPatch, UpdateTriggerRequest{
		Action: &TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "should-not-save"},
	})
	require.Equal(t, http.StatusBadRequest, actionPatchRec.Code, actionPatchRec.Body.String())
	actionPatchErr := decodeTriggerRuleError(t, actionPatchRec)
	assert.Equal(t, ErrCodeInvalidRule, actionPatchErr.Code)
	assert.Contains(t, actionPatchErr.Message, "[element 2]")
	elems, ok := actionPatchErr.Details["elements"].([]any)
	require.True(t, ok)
	require.Len(t, elems, 1)
	assert.Equal(t, float64(2), elems[0].(map[string]any)["element"])

	newTypes := []string{"webhook.deploy.*"}
	typesPatchRec := f.doTrigger(t, f.adminAuth, brokenID, http.MethodPatch, UpdateTriggerRequest{
		Match: &TriggerMatchPatchSpec{Types: &newTypes},
	})
	require.Equal(t, http.StatusBadRequest, typesPatchRec.Code, typesPatchRec.Body.String())

	storedBroken, err := f.store.GetTrigger(ctx, brokenID)
	require.NoError(t, err)
	assert.Equal(t, origPayload, storedBroken.ActionPayload)
	assert.Equal(t, []string{"webhook.received"}, storedBroken.MatchTypes)
	assert.Equal(t, 5, storedBroken.AuthorizationRevision)
	assert.Equal(t, f.owner.ID, storedBroken.InitiatorPrincipalID)
}

type failCreateTriggerTxStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *failCreateTriggerTxStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !s.fault.Active() {
		return s.Store.WithTx(ctx, fn)
	}
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&failCreateTriggerInnerStore{Store: tx, parent: s})
	})
}

type failCreateTriggerInnerStore struct {
	store.Store
	parent *failCreateTriggerTxStore
}

func (s *failCreateTriggerInnerStore) CreateTrigger(ctx context.Context, tr *store.Trigger) error {
	if s.parent.fault.Active() {
		return errors.New("injected CreateTrigger database error")
	}
	return s.Store.CreateTrigger(ctx, tr)
}

func TestWebhookEndpointCreate_DefaultTrigger(t *testing.T) {
	t.Run("one-call create returns both IDs, matches defaults, records audit, and fires end-to-end", func(t *testing.T) {
		f := setupTriggersTestFixture(t)
		ctx := context.Background()

		rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name:        "one-call-deploys",
			Description: "One-call endpoint and default trigger",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentName: f.targetAgent.Slug,
				Message:   "Handle incoming webhook event",
				Interrupt: true,
				Wake:      true,
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var created WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		require.NotEmpty(t, created.ID)
		require.NotEmpty(t, created.Secret)
		require.NotEmpty(t, created.TriggerID)
		require.NotNil(t, created.Trigger)
		assert.Equal(t, created.TriggerID, created.Trigger.ID)
		assert.Equal(t, "one-call-deploys", created.Trigger.Name)
		assert.Equal(t, TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: created.ID}, created.Trigger.Source)
		assert.Equal(t, []string{"webhook.received"}, created.Trigger.Match.Types)
		assert.Empty(t, created.Trigger.Match.Rules)
		assert.Empty(t, created.Trigger.Match.Dialect)
		assert.Empty(t, created.Trigger.Match.Expr)
		assert.Equal(t, store.TriggerSubjectModeNew, created.Trigger.Subject.Mode)
		assert.Empty(t, created.Trigger.Subject.Expr)
		assert.Equal(t, store.TriggerActionMessage, created.Trigger.Action.Type)
		assert.Equal(t, f.targetAgent.ID, created.Trigger.Action.AgentID)
		assert.Equal(t, f.targetAgent.Slug, created.Trigger.Action.AgentName)
		assert.Equal(t, "Handle incoming webhook event", created.Trigger.Action.Message)
		assert.True(t, created.Trigger.Action.Interrupt)
		assert.True(t, created.Trigger.Action.Wake)

		// Stored row uses 3b-1 ordered contract (empty scalar match columns) and owner attribution.
		storedTr, err := f.store.GetTrigger(ctx, created.TriggerID)
		require.NoError(t, err)
		assert.Empty(t, storedTr.MatchDialect)
		assert.Empty(t, storedTr.MatchRule)
		assert.Empty(t, storedTr.MatchRules)
		assert.Equal(t, store.DelegationPrincipalUser, storedTr.InitiatorPrincipalKind)
		assert.Equal(t, f.owner.ID, storedTr.InitiatorPrincipalID)
		assert.Equal(t, 1, storedTr.AuthorizationRevision)

		// Audit record for webhook_endpoint_create includes both endpoint_id and trigger_id, never the secret.
		audits, _, err := f.store.ListMutationAudits(ctx, store.MutationAuditFilter{
			MutationType: mutationTypeWebhookEndpointCreate,
			TargetID:     created.ID,
			Limit:        10,
		})
		require.NoError(t, err)
		require.Len(t, audits, 1)
		assert.NotContains(t, audits[0].AfterSummary, created.Secret)
		var afterSummary map[string]any
		require.NoError(t, json.Unmarshal([]byte(audits[0].AfterSummary), &afterSummary))
		assert.Equal(t, created.ID, afterSummary["endpoint_id"])
		assert.Equal(t, created.TriggerID, afterSummary["trigger_id"])

		// End-to-end: signed POST to the one-call endpoint fires its default trigger.
		disp := &recordingDispatcher{}
		f.srv.SetDispatcher(disp)
		require.NotNil(t, f.srv.startTriggerDrainer(ctx))
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			f.srv.stopTriggerDrainer(stopCtx)
		})
		time.Sleep(30 * time.Millisecond)

		payload := []byte(`{"deploy":"v1.2.3"}`)
		ts := time.Now().Unix()
		sig := trigger.SignScionV1(created.Secret, ts, "deliv-one-call-1", "", "", payload)
		hookReq := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/"+created.ID, bytes.NewReader(payload))
		hookReq.RemoteAddr = "198.51.100.10:12345"
		hookReq.Header.Set("Content-Type", "application/json")
		hookReq.Header.Set(trigger.HeaderDelivery, "deliv-one-call-1")
		hookReq.Header.Set(trigger.HeaderSignature, sig)
		hookRec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(hookRec, hookReq)
		require.Equal(t, http.StatusAccepted, hookRec.Code, hookRec.Body.String())

		require.Eventually(t, func() bool {
			return len(disp.getCalls()) == 1
		}, 1500*time.Millisecond, 15*time.Millisecond)
		assert.Contains(t, disp.getCalls()[0].Message, "Handle incoming webhook event")

		histRec := f.doTrigger(t, f.ownerAuth, created.TriggerID+"/history", http.MethodGet, nil)
		require.Equal(t, http.StatusOK, histRec.Code, histRec.Body.String())
		var hist ListTriggerFiresResponse
		require.NoError(t, json.Unmarshal(histRec.Body.Bytes(), &hist))
		require.Len(t, hist.Fires, 1)
		assert.Equal(t, store.TriggerFireStatusFired, hist.Fires[0].Status)
	})

	t.Run("toSelf resolves strictly from Hub AgentIdentity and rejects spoofing or human callers", func(t *testing.T) {
		f := setupTriggersTestFixture(t)
		ctx := context.Background()

		// 1. Human caller with toSelf: true -> 400 validation_error, nothing persisted.
		humanSelfRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "human-self-invalid",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				ToSelf:  true,
				Message: "should fail",
			},
		})
		assert.Equal(t, http.StatusBadRequest, humanSelfRec.Code, humanSelfRec.Body.String())
		_, err := f.store.GetWebhookEndpointByName(ctx, f.projectID, "human-self-invalid")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// 2. Agent caller combining toSelf with agentId or agentName -> 400 validation_error, nothing persisted.
		conflictSelfRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "agent-self-conflict",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				ToSelf:  true,
				AgentID: f.agentB.ID,
				Message: "should fail",
			},
		})
		assert.Equal(t, http.StatusBadRequest, conflictSelfRec.Code, conflictSelfRec.Body.String())
		_, err = f.store.GetWebhookEndpointByName(ctx, f.projectID, "agent-self-conflict")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// 3. Agent A calling with toSelf: true -> 201 targeting Agent A from its Hub identity.
		agentSelfRec := f.doWebhookEndpoint(t, f.agentAAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "agent-a-self-ep",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				ToSelf:  true,
				Message: "Deliver to self",
				Wake:    true,
			},
		})
		require.Equal(t, http.StatusCreated, agentSelfRec.Code, agentSelfRec.Body.String())
		var selfCreated WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(agentSelfRec.Body.Bytes(), &selfCreated))
		require.NotNil(t, selfCreated.Trigger)
		assert.Equal(t, f.agentA.ID, selfCreated.Trigger.Action.AgentID)
		assert.Equal(t, f.agentA.Slug, selfCreated.Trigger.Action.AgentName)

		storedEp, err := f.store.GetWebhookEndpoint(ctx, selfCreated.ID)
		require.NoError(t, err)
		assert.Equal(t, store.DelegationPrincipalAgent, storedEp.OwnerPrincipalKind)
		assert.Equal(t, f.agentA.ID, storedEp.OwnerPrincipalID)

		storedTr, err := f.store.GetTrigger(ctx, selfCreated.TriggerID)
		require.NoError(t, err)
		assert.Equal(t, store.DelegationPrincipalAgent, storedTr.InitiatorPrincipalKind)
		assert.Equal(t, f.agentA.ID, storedTr.InitiatorPrincipalID)
	})

	t.Run("authority parity: UAT scopes and target message authorization match two-call path without partial writes", func(t *testing.T) {
		f := setupTriggersTestFixture(t)
		ctx := context.Background()
		projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.projectID}

		// 1. UAT with webhook_endpoint:create ONLY (no trigger:create) succeeds without defaultTrigger, fails 403 with defaultTrigger.
		epOnlyUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "webhook_endpoint:create")
		bareOK := f.doWebhookEndpoint(t, epOnlyUAT, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "uat-bare-ep"})
		require.Equal(t, http.StatusCreated, bareOK.Code, bareOK.Body.String())

		deniedNoTrCreate := f.doWebhookEndpoint(t, epOnlyUAT, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "uat-denied-no-tr-create",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentID: f.targetAgent.ID,
				Message: "hi",
			},
		})
		assert.Equal(t, http.StatusForbidden, deniedNoTrCreate.Code, deniedNoTrCreate.Body.String())
		_, err := f.store.GetWebhookEndpointByName(ctx, f.projectID, "uat-denied-no-tr-create")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// 2. UAT missing webhook_endpoint:read fails 403 when defaultTrigger is requested, leaving no endpoint behind.
		noEpReadUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "webhook_endpoint:create", "trigger:create", "agent:message")
		deniedNoEpRead := f.doWebhookEndpoint(t, noEpReadUAT, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "uat-denied-no-ep-read",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentID: f.targetAgent.ID,
				Message: "hi",
			},
		})
		assert.Equal(t, http.StatusForbidden, deniedNoEpRead.Code, deniedNoEpRead.Body.String())
		_, err = f.store.GetWebhookEndpointByName(ctx, f.projectID, "uat-denied-no-ep-read")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// 3. UAT missing agent:message fails target authorization without creating the endpoint.
		noAgentMsgUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "webhook_endpoint:create", "webhook_endpoint:read", "trigger:create")
		deniedNoMsg := f.doWebhookEndpoint(t, noAgentMsgUAT, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "uat-denied-no-msg",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentID: f.targetAgent.ID,
				Message: "hi",
			},
		})
		assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, deniedNoMsg.Code, deniedNoMsg.Body.String())
		_, err = f.store.GetWebhookEndpointByName(ctx, f.projectID, "uat-denied-no-msg")
		assert.ErrorIs(t, err, store.ErrNotFound)

		// 4. Full UAT scopes succeed and record bounded ceiling on the default trigger.
		fullUAT := scheduleTestUAT(t, f.ownerAuth, projBoundary, "webhook_endpoint:create", "webhook_endpoint:read", "trigger:create", "agent:message")
		fullOK := f.doWebhookEndpoint(t, fullUAT, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "uat-one-call-ok",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentID: f.targetAgent.ID,
				Message: "hi",
			},
		})
		require.Equal(t, http.StatusCreated, fullOK.Code, fullOK.Body.String())
		var uatCreated WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(fullOK.Body.Bytes(), &uatCreated))
		storedUATTr, err := f.store.GetTrigger(ctx, uatCreated.TriggerID)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, storedUATTr.AuthorityCeiling.Kind)
		assert.Equal(t, store.InitiatorCredentialKindUAT, storedUATTr.InitiatorCredentialKind)

		// 5. Missing target agent -> 404, no endpoint or secret created.
		missingTarget := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "missing-target-ep",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentName: "nonexistent-agent",
				Message:   "hi",
			},
		})
		assert.Equal(t, http.StatusNotFound, missingTarget.Code, missingTarget.Body.String())
		_, err = f.store.GetWebhookEndpointByName(ctx, f.projectID, "missing-target-ep")
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("atomic single-transaction rollback on trigger name collision and store fault", func(t *testing.T) {
		srv, s, _, fault := testServerWithStoreFault(t, func(inner store.Store, sw *storeFaultSwitch) *failCreateTriggerTxStore {
			return &failCreateTriggerTxStore{Store: inner, fault: sw}
		})
		f := setupTriggersTestFixtureOnServer(t, srv, s)
		ctx := context.Background()

		// Create an existing endpoint "other-ep" and a trigger named "taken-trigger-name" attached to it.
		otherEpRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{Name: "other-ep"})
		require.Equal(t, http.StatusCreated, otherEpRec.Code, otherEpRec.Body.String())
		var otherEp WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(otherEpRec.Body.Bytes(), &otherEp))

		existingTrRec := f.doTrigger(t, f.ownerAuth, "", http.MethodPost, CreateTriggerRequest{
			Name:   "taken-trigger-name",
			Source: TriggerSourceSpec{Kind: store.TriggerSourceKindWebhook, ID: otherEp.ID},
			Action: TriggerActionSpec{Type: "message", AgentID: f.targetAgent.ID, Message: "existing"},
		})
		require.Equal(t, http.StatusCreated, existingTrRec.Code, existingTrRec.Body.String())

		secretsBefore, err := f.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: f.srv.hubID})
		require.NoError(t, err)

		// 1. Name collision on trigger -> 409 conflict naming trigger, and endpoint + secret roll back completely.
		collisionRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "taken-trigger-name",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentID: f.targetAgent.ID,
				Message: "new",
			},
		})
		require.Equal(t, http.StatusConflict, collisionRec.Code, collisionRec.Body.String())
		assert.Contains(t, collisionRec.Body.String(), "a trigger with this name already exists in this project")
		_, err = f.store.GetWebhookEndpointByName(ctx, f.projectID, "taken-trigger-name")
		assert.ErrorIs(t, err, store.ErrNotFound, "endpoint insert must roll back when default trigger creation fails")
		secretsAfterCollision, err := f.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: f.srv.hubID})
		require.NoError(t, err)
		assert.Len(t, secretsAfterCollision, len(secretsBefore), "internal secret insert must roll back when default trigger creation fails")

		// 2. Store fault on CreateTrigger inside WithTx -> 500, and endpoint + secret + audit roll back completely.
		fault.Arm()
		faultRec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "faulted-one-call",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentID: f.targetAgent.ID,
				Message: "new",
			},
		})
		assert.Equal(t, http.StatusInternalServerError, faultRec.Code, faultRec.Body.String())
		_, err = s.GetWebhookEndpointByName(ctx, f.projectID, "faulted-one-call")
		assert.ErrorIs(t, err, store.ErrNotFound, "endpoint insert must roll back on CreateTrigger store fault")
		secretsAfterFault, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: f.srv.hubID})
		require.NoError(t, err)
		assert.Len(t, secretsAfterFault, len(secretsBefore), "internal secret insert must roll back on CreateTrigger store fault")
	})

	t.Run("stamps default trigger with the same fixture provenance as its endpoint", func(t *testing.T) {
		f := setupTriggersTestFixture(t)
		ctx := context.Background()

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

		rec := f.doWebhookEndpoint(t, tiAuth, "", http.MethodPost, CreateWebhookEndpointRequest{
			Name: "fixture-one-call-hook",
			DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
				AgentID: f.targetAgent.ID,
				Message: "fixture provenance check",
			},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

		var created WebhookEndpointCreatedResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		require.NotEmpty(t, created.ID)
		require.NotEmpty(t, created.TriggerID)

		storedEp, err := f.store.GetWebhookEndpoint(ctx, created.ID)
		require.NoError(t, err)
		storedTr, err := f.store.GetTrigger(ctx, created.TriggerID)
		require.NoError(t, err)

		assert.Equal(t, ti.ID, storedEp.Fixture.RootID)
		assert.Equal(t, f.owner.ID, storedEp.Fixture.IssuerID)
		assert.Equal(t, storedEp.Fixture, storedTr.Fixture, "default trigger must carry the same fixture provenance stamp as its endpoint")
	})

	t.Run("rejects empty, whitespace, targetless, and oversized defaultTrigger payloads without creating endpoint or secret", func(t *testing.T) {
		f := setupTriggersTestFixture(t)
		ctx := context.Background()

		secretsBefore, err := f.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: f.srv.hubID})
		require.NoError(t, err)

		cases := []struct {
			name    string
			req     CreateWebhookEndpointRequest
			wantMsg string
		}{
			{
				name: "empty message",
				req: CreateWebhookEndpointRequest{
					Name: "bad-empty-msg",
					DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
						AgentID: f.targetAgent.ID,
						Message: "",
					},
				},
				wantMsg: "action.message is required for message actions",
			},
			{
				name: "whitespace-only message",
				req: CreateWebhookEndpointRequest{
					Name: "bad-ws-msg",
					DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
						AgentID: f.targetAgent.ID,
						Message: "   \t\n  ",
					},
				},
				wantMsg: "action.message is required for message actions",
			},
			{
				name: "missing target",
				req: CreateWebhookEndpointRequest{
					Name: "bad-no-target",
					DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
						Message: "hello",
					},
				},
				wantMsg: "action.agentId or action.agentName is required for message actions",
			},
			{
				name: "oversized message (>16 KiB)",
				req: CreateWebhookEndpointRequest{
					Name: "bad-big-msg",
					DefaultTrigger: &WebhookEndpointDefaultTriggerSpec{
						AgentID: f.targetAgent.ID,
						Message: strings.Repeat("m", maxTriggerActionMessageBytes+1),
					},
				},
				wantMsg: "action.message exceeds maximum length of 16384 bytes",
			},
		}

		for _, tc := range cases {
			rec := f.doWebhookEndpoint(t, f.ownerAuth, "", http.MethodPost, tc.req)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", tc.name, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.wantMsg, tc.name)
			_, err := f.store.GetWebhookEndpointByName(ctx, f.projectID, tc.req.Name)
			assert.ErrorIs(t, err, store.ErrNotFound, "%s must not create an endpoint", tc.name)
		}

		// Oversized request body (>64 KiB) returns 413 Payload Too Large.
		overBody := []byte(`{"name":"bad-big-body","defaultTrigger":{"agentId":"` + f.targetAgent.ID + `","message":"` + strings.Repeat("b", maxTriggerRequestBodyBytes) + `"}}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.projectID+"/webhook-endpoints", bytes.NewReader(overBody))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(contextWithIdentity(req.Context(), f.ownerAuth))
		overRec := httptest.NewRecorder()
		f.srv.handleWebhookEndpoints(overRec, req, f.projectID, "")
		assert.Equal(t, http.StatusRequestEntityTooLarge, overRec.Code, overRec.Body.String())

		secretsAfter, err := f.store.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeHub, ScopeID: f.srv.hubID})
		require.NoError(t, err)
		assert.Len(t, secretsAfter, len(secretsBefore), "rejected defaultTrigger requests must not persist webhook secrets")
	})
}
