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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authzHelperAgentSlug is the slug of the agent seedScheduleAuthorAgent
// stores; message schedules in these tests target it, so the
// scheduled-message rule resolves a real target.
const authzHelperAgentSlug = "schedule-author-agent"

// authoredRequest builds a request carrying identity and the credential
// context the auth middleware derives from it, so the authoring handlers
// record the request's attribution as they do in production.
func authoredRequest(t *testing.T, identity Identity, method, path string, body interface{}) *http.Request {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
	return req.WithContext(ctx)
}

// doAuthoredScheduleRequest sends an authored request to the schedules
// route.
func doAuthoredScheduleRequest(t *testing.T, srv *Server, identity Identity, projectID, schedulePath, method string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := authoredRequest(t, identity, method, "/api/v1/projects/"+projectID+"/schedules/"+schedulePath, body)
	rec := httptest.NewRecorder()
	srv.handleSchedules(rec, req, projectID, schedulePath)
	return rec
}

// doAuthoredEventRequest sends an authored create to the scheduled-events
// route.
func doAuthoredEventRequest(t *testing.T, srv *Server, identity Identity, projectID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	req := authoredRequest(t, identity, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", body)
	rec := httptest.NewRecorder()
	srv.handleScheduledEvents(rec, req, projectID, "")
	return rec
}

// scheduleRevision is the authority part of a stored schedule: attribution,
// revision and ceiling.
type scheduleRevision struct {
	Attribution store.InitiatorAttribution
	Ceiling     store.EffectCeiling
}

func loadScheduleRevision(t *testing.T, s store.Store, id string) scheduleRevision {
	t.Helper()
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	return scheduleRevision{Attribution: sc.InitiatorAttribution, Ceiling: sc.AuthorityCeiling}
}

// createOwnerSchedule creates a schedule of eventType as owner through the
// handler and returns its ID.
func createOwnerSchedule(t *testing.T, srv *Server, owner Identity, projectID, name, eventType string) string {
	t.Helper()
	req := CreateScheduleRequest{Name: name, CronExpr: "0 * * * *", EventType: eventType, AgentName: "worker"}
	if eventType == "message" {
		req.AgentName = authzHelperAgentSlug
		req.Message = "ping"
	}
	rec := doAuthoredScheduleRequest(t, srv, owner, projectID, "", http.MethodPost, req)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	return created.ID
}

func pauseSchedule(t *testing.T, srv *Server, owner Identity, projectID, id string) {
	t.Helper()
	rec := doAuthoredScheduleRequest(t, srv, owner, projectID, id+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A session-authored schedule records the principal ceiling together with
// its attribution, on create and on a one-shot event.
func TestScheduleAuthoringRecordsSessionCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-ceiling-owner"))

	id := createOwnerSchedule(t, srv, owner, projectID, "session-ceiling", "dispatch_agent")
	rev := loadScheduleRevision(t, s, id)
	assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, rev.Ceiling)
	assert.Equal(t, store.InitiatorCredentialKindSession, rev.Attribution.InitiatorCredentialKind)
	assert.Equal(t, owner.ID(), rev.Attribution.InitiatorPrincipalID)

	evtReq := CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "one-shot"}
	rec := doAuthoredEventRequest(t, srv, owner, projectID, evtReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))
	stored, err := s.GetScheduledEvent(context.Background(), evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, stored.AuthorityCeiling)
}

// An agent-authored schedule records the agent's bounded write ceiling.
func TestScheduleAuthoringRecordsAgentCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	author := authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate)

	id := createOwnerSchedule(t, srv, author, projectID, "agent-ceiling", "dispatch_agent")
	rev := loadScheduleRevision(t, s, id)
	want, _, err := srv.authzService.sourceEffectCeiling(context.Background(), author)
	require.NoError(t, err)
	assert.Equal(t, store.EffectCeilingBounded, rev.Ceiling.Kind)
	assert.Equal(t, want.PermissionIDs, rev.Ceiling.PermissionIDs)
	assert.Equal(t, store.InitiatorCredentialKindAgent, rev.Attribution.InitiatorCredentialKind)
}

// A name-only edit changes neither the attribution, the ceiling nor the
// revision, even when made by another principal.
func TestSchedMetadataEditKeepsAuthority(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-meta-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "meta-edit", "dispatch_agent")
	before := loadScheduleRevision(t, s, id)

	editor := authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate)
	rec := doAuthoredScheduleRequest(t, srv, editor, projectID, id, http.MethodPatch, UpdateScheduleRequest{Name: "meta-edit-renamed"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	after := loadScheduleRevision(t, s, id)
	assert.Equal(t, before, after)
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "meta-edit-renamed", sc.Name)
}

// Re-enabling a paused schedule through PATCH re-authorizes the caller and
// writes attribution, ceiling and revision together; a denied caller leaves
// all three unchanged.
func TestPatchEnableReauthorizes(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-enable-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "patch-enable", "dispatch_agent")
	pauseSchedule(t, srv, owner, projectID, id)
	before := loadScheduleRevision(t, s, id)

	// An agent without agent:create is denied, and nothing changes.
	denied := authzHelperAgent(projectID, ScopeProjectRead)
	rec := doAuthoredScheduleRequest(t, srv, denied, projectID, id, http.MethodPatch, UpdateScheduleRequest{Status: store.ScheduleStatusActive})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, before, loadScheduleRevision(t, s, id))
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, sc.Status)

	// An agent with agent:create re-enables it and becomes the revision.
	allowed := authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate)
	rec = doAuthoredScheduleRequest(t, srv, allowed, projectID, id, http.MethodPatch, UpdateScheduleRequest{Status: store.ScheduleStatusActive})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := loadScheduleRevision(t, s, id)
	assert.Equal(t, authzHelperAgentID, after.Attribution.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindAgent, after.Attribution.InitiatorCredentialKind)
	assert.Equal(t, before.Attribution.AuthorizationRevision+1, after.Attribution.AuthorizationRevision)
	assert.Equal(t, store.EffectCeilingBounded, after.Ceiling.Kind)
}

// fireRevisionCeiling calls revisionAuthorityCeiling for identity and
// returns its result and the response it wrote.
func fireRevisionCeiling(srv *Server, identity Identity, projectID string) (store.EffectCeiling, bool, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", nil)
	req = req.WithContext(contextWithIdentity(req.Context(), identity))
	rec := httptest.NewRecorder()
	c, ok := srv.revisionAuthorityCeiling(rec, req, projectID, ActionCreate)
	return c, ok, rec
}

// uatLookupFailingStore fails GetUserAccessToken.
type uatLookupFailingStore struct {
	store.Store
}

func (f *uatLookupFailingStore) GetUserAccessToken(context.Context, string) (*store.UserAccessToken, error) {
	return nil, errors.New("injected access token lookup fault")
}

// The revision ceiling is computed before any write: a lookup fault answers
// 500, and a credential that cannot be recorded answers 403, for every event
// type.
func TestSchedCreateCeilingErrorDeniesWrite(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-ceil-err-owner"))

	t.Run("lookup fault", func(t *testing.T) {
		uat := NewScopedUserIdentityWithCeiling(owner, projectID, []string{"agent:create"}, "uat-lookup-fault",
			permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.create"}})
		srv.authzService.store = &uatLookupFailingStore{Store: s}
		defer func() { srv.authzService.store = s }()
		_, ok, rec := fireRevisionCeiling(srv, uat, projectID)
		assert.False(t, ok)
		assert.Equal(t, http.StatusInternalServerError, rec.Code)
	})

	t.Run("unknown ceiling version", func(t *testing.T) {
		uat := NewScopedUserIdentityWithCeiling(owner, projectID, []string{"agent:create"}, "",
			permissions.FrozenPermissionCeiling{Version: 99, PermissionIDs: []string{"agent.create"}})
		_, ok, rec := fireRevisionCeiling(srv, uat, projectID)
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))
	})

	t.Run("identity kind not accepted", func(t *testing.T) {
		fed := &federatedTestIdentity{id: tid("sched-ceil-fed"), email: "fed@example.com", role: "member"}
		_, ok, rec := fireRevisionCeiling(srv, fed, projectID)
		assert.False(t, ok)
		assert.Equal(t, http.StatusForbidden, rec.Code)
		assert.Contains(t, rec.Body.String(), "cannot authorize scheduled work")
	})

	t.Run("session", func(t *testing.T) {
		c, ok, _ := fireRevisionCeiling(srv, owner, projectID)
		assert.True(t, ok)
		assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, c)
	})
}

// A ceiling denial is logged with the authoring action: create on the
// create paths, update on an edit or resume.
func TestSchedCeilingDenialLogsAuthoringAction(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)
	capture := &capturingHandler{}
	restoreLog := slog.Default()
	slog.SetDefault(slog.New(capture))
	t.Cleanup(func() { slog.SetDefault(restoreLog) })

	fed := &federatedTestIdentity{id: tid("sched-ceil-log-fed"), email: "fed@example.com", role: "member"}
	for _, action := range []Action{ActionCreate, ActionUpdate} {
		capture.mu.Lock()
		capture.records = nil
		capture.mu.Unlock()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectID+"/schedules", nil)
		req = req.WithContext(contextWithIdentity(req.Context(), fed))
		rec := httptest.NewRecorder()
		_, ok := srv.revisionAuthorityCeiling(rec, req, projectID, action)
		require.False(t, ok)
		require.Equal(t, http.StatusForbidden, rec.Code)
		logRec, found := findRecord(capture.all(), "authorization denied")
		require.True(t, found, "expected a denial log line")
		assert.Equal(t, string(action), fmt.Sprint(recordAttrs(logRec)["action"]))
	}
}

// messageWriteCredential is a credential whose authority cannot be recorded
// on a schedule revision, and the response text that names why.
type messageWriteCredential struct {
	name     string
	identity Identity
	cause    string
}

// unrecordableMessageWriteCredentials returns, for the setupScheduleTest
// project, every credential fixture whose ceiling cannot be recorded, each
// admitted far enough to reach the revision ceiling on a message write:
//   - a federated user holding the project owner role;
//   - an agent whose row is not stored (an orphaned chain);
//   - the seeded author agent with no active delegation edge after the edge
//     backfill completed (the chain has no recorded provenance);
//   - a local development user other than the recognized one.
func unrecordableMessageWriteCredentials(t *testing.T, srv *Server, s store.Store, projectID string) []messageWriteCredential {
	t.Helper()
	ctx := context.Background()
	fedUserID := tid("sched-msg-unrec-fed")
	fed := &federatedTestIdentity{id: fedUserID, email: "fedunrec@example.com", displayName: "Fed", role: "member"}
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: fedUserID, Email: fed.Email(), DisplayName: fed.DisplayName(), Role: "member", Status: "active"}))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, fedUserID))

	orphan := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: tid("sched-msg-unrec-unstored-agent")},
		ProjectID: projectID,
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentCreate},
	}}

	markEdgeBackfillComplete(t, s)
	agent := authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate)

	devID := tid("sched-msg-unrec-dev")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: devID, Email: "devunrec@example.com", DisplayName: "Dev", Role: "member", Status: "active"}))
	require.NoError(t, srv.createProjectOwnerRoleBinding(ctx, projectID, devID))
	dev := &DevUser{id: devID, email: "devunrec@example.com", displayName: "Dev"}

	return []messageWriteCredential{
		{name: "federated user", identity: fed, cause: "cannot authorize scheduled work"},
		{name: "agent whose row is not stored", identity: orphan, cause: "delegation record is missing or inconsistent"},
		{name: "agent without recorded provenance", identity: agent, cause: "delegation record is missing or inconsistent"},
		{name: "unrecognized local development user", identity: dev, cause: "cannot authorize scheduled work"},
	}
}

// A message event or schedule write by a credential whose authority cannot
// be recorded is refused with the same 403 as a dispatch_agent write, and
// writes nothing: no event or schedule row on create, and no attribution,
// ceiling, revision or status change on PATCH or resume.
func TestMessageScheduleWriteDeniedWhenCeilingUnrecordable(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-msg-unrec-owner"))
	creds := unrecordableMessageWriteCredentials(t, srv, s, projectID)

	assertCeilingDenial := func(t *testing.T, c messageWriteCredential, rec *httptest.ResponseRecorder) {
		t.Helper()
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), string(DeniedByDelegationCeiling))
		assert.Contains(t, rec.Body.String(), c.cause)
	}
	// The target is not stored, so scheduled-message authoring admits each
	// credential and the refusal comes from the revision ceiling alone.
	const unrecTarget = "unrec-ghost-target"
	createUnrecTargetSchedule := func(t *testing.T, name string) string {
		t.Helper()
		rec := doAuthoredScheduleRequest(t, srv, owner, projectID, "", http.MethodPost,
			CreateScheduleRequest{Name: name, CronExpr: "0 * * * *", EventType: "message", AgentName: unrecTarget, Message: "ping"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created store.Schedule
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
		return created.ID
	}
	countEvents := func(t *testing.T) int {
		t.Helper()
		res, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{})
		require.NoError(t, err)
		return len(res.Items)
	}
	countSchedules := func(t *testing.T) int {
		t.Helper()
		res, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{})
		require.NoError(t, err)
		return len(res.Items)
	}

	for i, c := range creds {
		suffix := fmt.Sprintf("-%d", i)
		t.Run(c.name, func(t *testing.T) {
			t.Run("create event", func(t *testing.T) {
				before := countEvents(t)
				rec := doAuthoredEventRequest(t, srv, c.identity, projectID,
					CreateScheduledEventRequest{EventType: "message", FireIn: "30m", AgentName: unrecTarget, Message: "hi"})
				assertCeilingDenial(t, c, rec)
				assert.Equal(t, before, countEvents(t), "no event is stored")
			})

			t.Run("create schedule", func(t *testing.T) {
				before := countSchedules(t)
				rec := doAuthoredScheduleRequest(t, srv, c.identity, projectID, "", http.MethodPost,
					CreateScheduleRequest{Name: "unrec-msg" + suffix, CronExpr: "0 * * * *", EventType: "message", AgentName: unrecTarget, Message: "hi"})
				assertCeilingDenial(t, c, rec)
				assert.Equal(t, before, countSchedules(t), "no schedule is stored")
			})

			t.Run("PATCH", func(t *testing.T) {
				id := createUnrecTargetSchedule(t, "unrec-msg-patch"+suffix)
				before, err := s.GetSchedule(ctx, id)
				require.NoError(t, err)
				rec := doAuthoredScheduleRequest(t, srv, c.identity, projectID, id, http.MethodPatch,
					UpdateScheduleRequest{CronExpr: "30 * * * *"})
				assertCeilingDenial(t, c, rec)
				after, err := s.GetSchedule(ctx, id)
				require.NoError(t, err)
				assert.Equal(t, before.CronExpr, after.CronExpr)
				assert.Equal(t, before.InitiatorAttribution, after.InitiatorAttribution)
				assert.Equal(t, before.AuthorityCeiling, after.AuthorityCeiling)
			})

			t.Run("resume", func(t *testing.T) {
				id := createUnrecTargetSchedule(t, "unrec-msg-resume"+suffix)
				pauseSchedule(t, srv, owner, projectID, id)
				before := loadScheduleRevision(t, s, id)
				rec := doAuthoredScheduleRequest(t, srv, c.identity, projectID, id+"/resume", http.MethodPost, nil)
				assertCeilingDenial(t, c, rec)
				assert.Equal(t, before, loadScheduleRevision(t, s, id))
				sc, err := s.GetSchedule(ctx, id)
				require.NoError(t, err)
				assert.Equal(t, store.ScheduleStatusPaused, sc.Status)
			})
		})
	}
}

// --- resume re-authorizes every schedule type ------------------------------

func TestResumeScopedUATDenied_DispatchAgent(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("resume-uat-da-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "resume-uat-da", "dispatch_agent")
	pauseSchedule(t, srv, owner, projectID, id)
	before := loadScheduleRevision(t, s, id)

	// The authoring credential gate refuses a UAT on the resume route
	// before the schedule is read, so the revision stays as it was.
	scoped := NewScopedUserIdentity(owner, projectID, []string{"scheduled_event:update"})
	rec := doAuthoredScheduleRequest(t, srv, scoped, projectID, id+"/resume", http.MethodPost, nil)
	assertScheduleAuthoringRefused(t, rec)
	assert.Equal(t, before, loadScheduleRevision(t, s, id))
}

func TestResumeScopedUATDenied_Message(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("resume-uat-msg-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "resume-uat-msg", "message")
	pauseSchedule(t, srv, owner, projectID, id)
	before := loadScheduleRevision(t, s, id)

	// Refused by the authoring credential gate, as above.
	scoped := NewScopedUserIdentity(owner, projectID, []string{"scheduled_event:update", "agent:message"})
	rec := doAuthoredScheduleRequest(t, srv, scoped, projectID, id+"/resume", http.MethodPost, nil)
	assertScheduleAuthoringRefused(t, rec)
	assert.Equal(t, before, loadScheduleRevision(t, s, id))
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, sc.Status)
}

// nonAdmittedScheduleUser returns a stored, active user with no binding in
// projectID.
func nonAdmittedScheduleUser(t *testing.T, s store.Store, id string) UserIdentity {
	t.Helper()
	u := NewAuthenticatedUser(id, id+"@test.com", "Outsider", "member", "api")
	require.NoError(t, s.CreateUser(context.Background(), &store.User{ID: id, Email: u.Email(), DisplayName: u.DisplayName(), Role: "member", Status: "active"}))
	return u
}

// scheduleOnlyUser returns a stored, active user whose only binding in
// projectID is a project role holding the scheduled_event permissions: the
// route-level schedule check admits it, and nothing else in the project
// does, so a resume denial comes from the resume re-authorization.
func scheduleOnlyUser(t *testing.T, s store.Store, projectID, id string) UserIdentity {
	t.Helper()
	ctx := context.Background()
	u := nonAdmittedScheduleUser(t, s, id)
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        id + "-schedules",
		Description: "scheduled events only",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"scheduled_event.list", "scheduled_event.read", "scheduled_event.update"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      id,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
	return u
}

func TestResumeNonAdmittedDenied_DispatchAgent(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("resume-na-da-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "resume-na-da", "dispatch_agent")
	pauseSchedule(t, srv, owner, projectID, id)
	before := loadScheduleRevision(t, s, id)

	outsider := scheduleOnlyUser(t, s, projectID, tid("resume-na-da-outsider"))
	rec := doAuthoredScheduleRequest(t, srv, outsider, projectID, id+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), agentCreateDenyMessage, "the denial is the resume's agent-create re-authorization")
	assert.Equal(t, before, loadScheduleRevision(t, s, id))
}

func TestResumeNonAdmittedDenied_Message(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("resume-na-msg-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "resume-na-msg", "message")
	pauseSchedule(t, srv, owner, projectID, id)
	before := loadScheduleRevision(t, s, id)

	outsider := scheduleOnlyUser(t, s, projectID, tid("resume-na-msg-outsider"))
	rec := doAuthoredScheduleRequest(t, srv, outsider, projectID, id+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not authorized to message this agent", "the denial is the resume's scheduled-message re-authorization")
	assert.Equal(t, before, loadScheduleRevision(t, s, id))
}

// Resuming a dispatch_agent schedule requires agent creation in the project:
// an agent without agent:create is denied even though it may update the
// schedule; with agent:create it resumes and becomes the revision.
func TestResumeDispatchAgentRequiresAgentCreate(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("resume-create-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "resume-create", "dispatch_agent")
	pauseSchedule(t, srv, owner, projectID, id)

	rec := doAuthoredScheduleRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead), projectID, id+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), string(ScopeAgentCreate))

	rec = doAuthoredScheduleRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, id+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	after := loadScheduleRevision(t, s, id)
	assert.Equal(t, authzHelperAgentID, after.Attribution.InitiatorPrincipalID)
	assert.Equal(t, store.EffectCeilingBounded, after.Ceiling.Kind)
}

// A denied resume leaves attribution, ceiling, revision and status
// unchanged; an allowed one writes all of them together.
func TestResumeDenyLeavesAttributionCeilingRevision(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("resume-deny-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "resume-deny", "dispatch_agent")
	pauseSchedule(t, srv, owner, projectID, id)
	beforeRow, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)

	rec := doAuthoredScheduleRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead), projectID, id+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	afterRow, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, beforeRow.InitiatorAttribution, afterRow.InitiatorAttribution)
	assert.Equal(t, beforeRow.AuthorityCeiling, afterRow.AuthorityCeiling)
	assert.Equal(t, beforeRow.Status, afterRow.Status)
	assert.Equal(t, beforeRow.NextRunAt, afterRow.NextRunAt)

	rec = doAuthoredScheduleRequest(t, srv, owner, projectID, id+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	resumed, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, resumed.Status)
	assert.Equal(t, beforeRow.AuthorizationRevision+1, resumed.AuthorizationRevision)
	assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, resumed.AuthorityCeiling)
}

// concurrentReattributeStore lands a competing re-attribution just before
// the first UpdateSchedule it sees.
type concurrentReattributeStore struct {
	store.Store
	fired bool
}

func (c *concurrentReattributeStore) UpdateSchedule(ctx context.Context, sc *store.Schedule, fields store.ScheduleFieldMask,
	prevRevision int, prevRevisionKnown bool, attribution *store.InitiatorAttribution) error {
	if !c.fired {
		c.fired = true
		competing, err := c.GetSchedule(ctx, sc.ID)
		if err != nil {
			return err
		}
		attr := competing.InitiatorAttribution
		attr.AuthorizationRevision++
		competing.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
		if err := c.Store.UpdateSchedule(ctx, competing, store.ScheduleFieldMask{}, prevRevision, prevRevisionKnown, &attr); err != nil {
			return err
		}
	}
	return c.Store.UpdateSchedule(ctx, sc, fields, prevRevision, prevRevisionKnown, attribution)
}

// A resume racing a concurrent re-attribution answers 409 and writes
// nothing of its own.
func TestResumeConcurrent409(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("resume-409-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "resume-409", "dispatch_agent")
	pauseSchedule(t, srv, owner, projectID, id)
	before := loadScheduleRevision(t, s, id)

	srv.store = &concurrentReattributeStore{Store: s}
	defer func() { srv.store = s }()
	rec := doAuthoredScheduleRequest(t, srv, authzHelperAgent(projectID, ScopeProjectRead, ScopeAgentCreate), projectID, id+"/resume", http.MethodPost, nil)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	after, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, before.Attribution.AuthorizationRevision+1, after.AuthorizationRevision, "only the competing write landed")
	assert.Equal(t, before.Attribution.InitiatorPrincipalID, after.InitiatorPrincipalID)
	assert.Equal(t, store.ScheduleStatusPaused, after.Status)
}

// Pause and delete write their audit record in the same transaction as the
// change: an injected audit failure rolls the change back.
func TestSchedulePauseDeleteAuditInTransaction(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-audit-tx-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "audit-tx", "message")

	srv.store = &createTxFaultStore{Store: s, auditErrFor: mutationTypeSchedulePause}
	rec := doAuthoredScheduleRequest(t, srv, owner, projectID, id+"/pause", http.MethodPost, nil)
	assert.NotEqual(t, http.StatusOK, rec.Code)
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, sc.Status, "a failed pause audit rolls the pause back")

	srv.store = &createTxFaultStore{Store: s, auditErrFor: mutationTypeScheduleDelete}
	rec = doAuthoredScheduleRequest(t, srv, owner, projectID, id, http.MethodDelete, nil)
	assert.NotEqual(t, http.StatusNoContent, rec.Code)
	_, err = s.GetSchedule(context.Background(), id)
	require.NoError(t, err, "a failed delete audit rolls the delete back")
	srv.store = s

	rec = doAuthoredScheduleRequest(t, srv, owner, projectID, id+"/pause", http.MethodPost, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetType: "schedule", MutationType: mutationTypeSchedulePause})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, id, recs[0].TargetID)
	assert.Equal(t, owner.ID(), recs[0].ActorPrincipalID)
}

// A cancel's audit is written in the cancel's transaction: a failed audit
// write fails the cancel and leaves the event pending with its timer armed.
// The record has the same shape as before: mutation type, target and the
// request's actor and credential, matching a pause audit by the same
// caller.
func TestCancelScheduledEventAuditInTransaction(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-cancel-tx-owner"))

	rec := doAuthoredEventRequest(t, srv, owner, projectID,
		CreateScheduledEventRequest{EventType: "message", FireIn: "1h", AgentName: authzHelperAgentSlug, Message: "ping"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	timerArmed := func() bool {
		srv.scheduler.mu.Lock()
		defer srv.scheduler.mu.Unlock()
		_, ok := srv.scheduler.timers[created.ID]
		return ok
	}
	require.True(t, timerArmed())

	cancel := func() *httptest.ResponseRecorder {
		req := authoredRequest(t, owner, http.MethodDelete, "/api/v1/projects/"+projectID+"/scheduled-events/"+created.ID, nil)
		rec := httptest.NewRecorder()
		srv.handleScheduledEvents(rec, req, projectID, created.ID)
		return rec
	}

	srv.store = &createTxFaultStore{Store: s, auditErrFor: mutationTypeScheduledEventCancel}
	rec = cancel()
	assert.NotEqual(t, http.StatusNoContent, rec.Code)
	evt, err := s.GetScheduledEvent(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventPending, evt.Status, "a failed cancel audit rolls the cancel back")
	assert.True(t, timerArmed(), "a failed cancel leaves the timer armed")
	srv.store = s

	rec = cancel()
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	evt, err = s.GetScheduledEvent(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, evt.Status)
	assert.False(t, timerArmed(), "a committed cancel stops the timer")

	recs, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeScheduledEventCancel})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	got := recs[0]
	assert.Equal(t, "scheduled_event_cancel", got.MutationType)
	assert.Equal(t, "scheduled_event", got.TargetType)
	assert.Equal(t, created.ID, got.TargetID)
	assert.Equal(t, owner.ID(), got.ActorPrincipalID)

	// The actor and credential fields match a pause audit by the same caller.
	id := createOwnerSchedule(t, srv, owner, projectID, "cancel-shape", "message")
	pauseSchedule(t, srv, owner, projectID, id)
	pauses, _, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{MutationType: mutationTypeSchedulePause, TargetID: id})
	require.NoError(t, err)
	require.Len(t, pauses, 1)
	assert.Equal(t, pauses[0].ActorPrincipalKind, got.ActorPrincipalKind)
	assert.Equal(t, pauses[0].ActorPrincipalID, got.ActorPrincipalID)
	assert.Equal(t, pauses[0].ActorCredentialType, got.ActorCredentialType)
	assert.Equal(t, pauses[0].ActorCredentialID, got.ActorCredentialID)
	assert.NotEmpty(t, got.ActorCredentialType)
}

// A recurring fire copies the schedule's ceiling onto the materialized
// event together with its attribution.
func TestExecuteScheduleCopiesAuthorityCeiling(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid("sched-copy-owner"))
	id := createOwnerSchedule(t, srv, owner, projectID, "copy-ceiling", "message")
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	sc.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.create"}}

	srv.executeSchedule(context.Background(), *sc, sc.CreatedAt)

	res, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID, ScheduleID: id}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, sc.AuthorityCeiling.Kind, res.Items[0].AuthorityCeiling.Kind)
	assert.Equal(t, sc.AuthorityCeiling.PermissionIDs, res.Items[0].AuthorityCeiling.PermissionIDs)
	assert.Equal(t, sc.InitiatorAttribution, res.Items[0].InitiatorAttribution)
}
