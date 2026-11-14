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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Authoring of one-shot exec events (ptone/scion#7883, AC2, AC13): the
// caller needs agent.attach on the target, the rule live /exec uses.

// execAuthoringFixture is bypassAgentsSetup with exec schedules on.
func execAuthoringFixture(t *testing.T) *bypassAgentsFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	ensureTestScheduler(f.srv)
	enableExecSchedules(t, f.srv)
	// The fixture owner holds project access only; give it the
	// scheduled-event permissions a project member has.
	bindPermsToUser(t, f.store, f.owner.ID, scheduledEventPerms)
	return f
}

// ensureTestScheduler gives srv an unstarted scheduler when the fixture
// server has none (bypassAgentsServer builds none), so create handlers can
// persist and arm events.
func ensureTestScheduler(srv *Server) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.scheduler == nil {
		srv.scheduler = NewScheduler(srv.store, slog.Default())
	}
}

// scheduledEventPerms are the scheduled_event permissions of a project member.
var scheduledEventPerms = []string{
	"scheduled_event.read", "scheduled_event.list", "scheduled_event.create",
	"scheduled_event.update", "scheduled_event.delete",
}

// scheduledEventUATScopes are the matching UAT scopes.
var scheduledEventUATScopes = []string{
	"scheduled_event:read", "scheduled_event:list", "scheduled_event:create",
	"scheduled_event:update", "scheduled_event:delete", store.UATScopeProjectRead,
}

func execEventsPath(f *bypassAgentsFixture) string {
	return "/api/v1/projects/" + f.proj.ID + "/scheduled-events"
}

// execCreateBody is a create request for an exec event on agentID.
func execCreateBody(agentID string, argv ...string) map[string]any {
	return map[string]any{"eventType": "exec", "agentId": agentID, "command": argv, "fireIn": "1h"}
}

// storedExecEvents lists the project's exec events.
func storedExecEvents(t *testing.T, f *bypassAgentsFixture) []store.ScheduledEvent {
	t.Helper()
	res, err := f.store.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{
		ProjectID: f.proj.ID, EventType: scheduledEventTypeExec,
	}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	return res.Items
}

// seedExecEvent stores a pending exec event on targetID directly.
func seedExecEvent(t *testing.T, f *bypassAgentsFixture, targetID string) *store.ScheduledEvent {
	t.Helper()
	p := scheduledExecPayload{AgentID: targetID, Command: []string{"true"}}
	raw, err := p.encode()
	require.NoError(t, err)
	evt := withSessionRevision(store.ScheduledEvent{
		ID: api.NewUUID(), ProjectID: f.proj.ID, EventType: scheduledEventTypeExec,
		FireAt: time.Now().Add(time.Hour), Payload: raw, Status: store.ScheduledEventPending,
		CreatedBy: f.owner.ID,
	}, f.owner.ID)
	require.NoError(t, f.store.CreateScheduledEvent(context.Background(), &evt))
	return &evt
}

// auditCapture routes the server's audit logger to a buffer.
func auditCapture(t *testing.T, srv *Server) *scheduledMessageLogCapture {
	t.Helper()
	c := &scheduledMessageLogCapture{}
	prev := srv.GetAuditLogger()
	srv.SetAuditLogger(&LogAuditLogger{log: slog.New(slog.NewTextHandler(c, nil))})
	t.Cleanup(func() { srv.SetAuditLogger(prev) })
	return c
}

func decodeEventResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	return out
}

func TestScheduledExecAuthoring_OwnerAllowedAndResolved(t *testing.T) {
	f := execAuthoringFixture(t)
	sentinel := "EXECARGV-" + uuid.NewString()
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f),
		map[string]any{"eventType": "exec", "agentName": f.sibling.Slug, "command": []string{"sh", "-c", sentinel}, "timeoutSeconds": 30, "fireIn": "1h"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	resp := decodeEventResponse(t, rec)
	assert.Contains(t, resp["payload"], sentinel, "the owner holds attach: argv revealed")

	evts := storedExecEvents(t, f)
	require.Len(t, evts, 1)
	p, err := decodeScheduledExecPayload(evts[0].Payload)
	require.NoError(t, err)
	assert.Equal(t, f.sibling.ID, p.AgentID, "agentId resolved at authoring")
	assert.Equal(t, f.sibling.Slug, p.AgentName)
	assert.Equal(t, 30, p.TimeoutSeconds)
	assert.Equal(t, store.ScheduledEventPending, evts[0].Status)
	assert.Equal(t, store.DelegationPrincipalUser, evts[0].InitiatorPrincipalKind)
	assert.Equal(t, f.owner.ID, evts[0].InitiatorPrincipalID)
}

// TestScheduledExecAuthoring_UserMatrix: stock project-admin and hub-admin
// roles do not carry agent.attach on another user's agent; a UAT needs the
// agent:attach scope.
func TestScheduledExecAuthoring_UserMatrix(t *testing.T) {
	f := execAuthoringFixture(t)
	ctx := context.Background()

	projAdminID := tid("exec-proj-admin")
	createTestUserWithProjectRole(t, f.store, projAdminID, "exec-proj-admin@test", f.proj.ID, store.ProjectRoleAdmin)
	ensureHubMembership(ctx, f.store, projAdminID)
	projAdmin := NewAuthenticatedUser(projAdminID, "exec-proj-admin@test", "PA", store.UserRoleMember, "api")

	hubAdminID := tid("exec-hub-admin")
	createTestUserWithRole(t, f.store, hubAdminID, "exec-hub-admin@test", store.UserRoleAdmin, store.SystemRoleHubAdmin)
	hubAdmin := NewAuthenticatedUser(hubAdminID, "exec-hub-admin@test", "HA", store.UserRoleAdmin, "api")

	attachUAT := NewScopedUserIdentity(authUser(f.owner), f.proj.ID, append([]string{store.UATScopeAgentAttach, store.UATScopeAgentRead}, scheduledEventUATScopes...))
	manageUAT := NewScopedUserIdentity(authUser(f.owner), f.proj.ID, append([]string{store.UATScopeAgentManage, store.UATScopeAgentRead}, scheduledEventUATScopes...))

	cases := []struct {
		name     string
		identity Identity
		allowed  bool
	}{
		{"owner", authUser(f.owner), true},
		{"project-admin", projAdmin, false},
		{"hub-admin", hubAdmin, false},
		{"UAT with agent:attach", attachUAT, true},
		{"UAT without agent:attach", manageUAT, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(storedExecEvents(t, f))
			rec := requestAsIdentity(t, f.srv, c.identity, http.MethodPost, execEventsPath(f), execCreateBody(f.sibling.ID, "true"))
			if c.allowed {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				assert.Len(t, storedExecEvents(t, f), before+1)
				return
			}
			assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
			assert.Len(t, storedExecEvents(t, f), before, "nothing stored on a refusal")
		})
	}
}

// TestScheduledExecAuthoring_AgentMatrix: a full-role agent with a live
// delegation chain may author exec on itself, its child and a same-root
// sibling; never on another user's agent, nor without the lifecycle scope,
// nor with a delegated credential.
func TestScheduledExecAuthoring_AgentMatrix(t *testing.T) {
	f := execAuthoringFixture(t)
	markEdgeBackfillComplete(t, f.store)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, f.caller.ID, f.proj.ID)
	foreign := foreignOwnedAgent(t, f, "exec-foreign")

	cases := []struct {
		name    string
		target  *store.Agent
		scopes  []AgentTokenScope
		allowed bool
	}{
		{"self", f.caller, []AgentTokenScope{ScopeAgentLifecycle}, true},
		{"child", f.child, []AgentTokenScope{ScopeAgentLifecycle}, true},
		{"sibling", f.sibling, []AgentTokenScope{ScopeAgentLifecycle}, true},
		{"other user's agent", foreign, []AgentTokenScope{ScopeAgentLifecycle}, false},
		{"baseline (no lifecycle scope)", f.sibling, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := len(storedExecEvents(t, f))
			rec := f.asAgent(t, http.MethodPost, execEventsPath(f), execCreateBody(c.target.ID, "true"), c.scopes...)
			if c.allowed {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				assert.Len(t, storedExecEvents(t, f), before+1)
				return
			}
			assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
			assert.Len(t, storedExecEvents(t, f), before)
		})
	}

	t.Run("delegated agent credential", func(t *testing.T) {
		before := len(storedExecEvents(t, f))
		delegated := &DelegatedAgentIdentity{agentID: f.caller.ID}
		rec := requestAsIdentity(t, f.srv, delegated, http.MethodPost, execEventsPath(f), execCreateBody(f.caller.ID, "true"))
		assert.NotEqual(t, http.StatusCreated, rec.Code, rec.Body.String())
		assert.GreaterOrEqual(t, rec.Code, 400)
		assert.Len(t, storedExecEvents(t, f), before)
	})
}

func TestScheduledExecAuthoring_ManagedRuntimeRejected(t *testing.T) {
	f := execAuthoringFixture(t)
	managed := &store.Agent{
		ID: tid("exec-managed"), Slug: tid("exec-managed"), Name: "exec-managed", ProjectID: f.proj.ID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
		Runtime: "managed:test",
	}
	require.True(t, isManagedAgentRuntime(managed.Runtime))
	require.NoError(t, f.store.CreateAgent(context.Background(), managed))
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(managed.ID, "true"))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeUnsupportedCapability, decodeTargetAPIError(t, rec).Code)
	assert.Empty(t, storedExecEvents(t, f))
}

// TestScheduledExecAuthoring_FlagOff (AC1): with either hub flag off an exec
// event is answered like any unknown event type.
func TestScheduledExecAuthoring_FlagOff(t *testing.T) {
	for _, flags := range []map[string]bool{
		{conduitExperiment: true, experiments.ExecSchedules: false},
		{conduitExperiment: false, experiments.ExecSchedules: true},
	} {
		f := bypassAgentsSetup(t)
		ensureTestScheduler(f.srv)
		setHubExperiments(t, f.srv, flags)
		bindPermsToUser(t, f.store, f.owner.ID, scheduledEventPerms)
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(f.sibling.ID, "true"))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "unsupported event type: exec")
		assert.NotContains(t, rec.Body.String(), "supported: message, dispatch_agent, exec")
		assert.Empty(t, storedExecEvents(t, f))
	}
}

// TestScheduledExecAuthoring_PayloadBounds (AC13): every bound is enforced
// at authoring and nothing is stored.
func TestScheduledExecAuthoring_PayloadBounds(t *testing.T) {
	f := execAuthoringFixture(t)
	many := make([]string, 65)
	for i := range many {
		many[i] = "a"
	}
	big := strings.Repeat("x", 4097)
	total := make([]string, 5)
	for i := range total {
		total[i] = strings.Repeat("y", 4000)
	}
	body := func(mut func(m map[string]any)) map[string]any {
		m := execCreateBody(f.sibling.ID, "true")
		mut(m)
		return m
	}
	cases := map[string]map[string]any{
		"65 args":            body(func(m map[string]any) { m["command"] = many }),
		"NUL":                body(func(m map[string]any) { m["command"] = []string{"a\x00b"} }),
		"element over 4096":  body(func(m map[string]any) { m["command"] = []string{big} }),
		"total over 16384":   body(func(m map[string]any) { m["command"] = total }),
		"timeout 3601":       body(func(m map[string]any) { m["timeoutSeconds"] = 3601 }),
		"timeout -1":         body(func(m map[string]any) { m["timeoutSeconds"] = -1 }),
		"empty command":      body(func(m map[string]any) { m["command"] = []string{} }),
		"empty argv0":        body(func(m map[string]any) { m["command"] = []string{""} }),
		"wake in payload":    {"eventType": "exec", "fireIn": "1h", "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"],"wake":true}`},
		"unknown field":      {"eventType": "exec", "fireIn": "1h", "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"],"cwd":"/"}`},
		"trailing data":      {"eventType": "exec", "fireIn": "1h", "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"]} {}`},
		"payload and fields": {"eventType": "exec", "fireIn": "1h", "agentId": f.sibling.ID, "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"]}`},
		"message field":      body(func(m map[string]any) { m["message"] = "hi" }),
		"wake field":         body(func(m map[string]any) { m["wake"] = true }),
		"template field":     body(func(m map[string]any) { m["template"] = "x" }),
		"no target":          {"eventType": "exec", "fireIn": "1h", "command": []string{"true"}},
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), b)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Empty(t, storedExecEvents(t, f))
		})
	}

	t.Run("raw payload accepted", func(t *testing.T) {
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f),
			map[string]any{"eventType": "exec", "fireIn": "1h", "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"],"timeoutSeconds":3600}`})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})

	t.Run("exec fields on a message event", func(t *testing.T) {
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f),
			map[string]any{"eventType": "message", "agentId": f.sibling.ID, "message": "hi", "command": []string{"true"}, "fireIn": "1h"})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})
}

// TestScheduledExecAuthoring_BodyLimit: create bodies over 64 KiB are
// refused for every event type.
func TestScheduledExecAuthoring_BodyLimit(t *testing.T) {
	f := execAuthoringFixture(t)
	b := execCreateBody(f.sibling.ID, "true")
	b["padding"] = strings.Repeat("p", scheduleRequestBodyLimit+1)
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), b)
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	assert.Less(t, rec.Code, 500, rec.Body.String())
	assert.Empty(t, storedExecEvents(t, f))

	m := map[string]any{"eventType": "message", "agentId": f.sibling.ID, "message": strings.Repeat("m", scheduleRequestBodyLimit+1), "fireIn": "1h"}
	rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), m)
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	assert.Less(t, rec.Code, 500, rec.Body.String())
}

// TestScheduledExecAuthoring_Quota: 20 active exec events per target, 200
// per project.
func TestScheduledExecAuthoring_Quota(t *testing.T) {
	t.Run("per target", func(t *testing.T) {
		f := execAuthoringFixture(t)
		for i := 0; i < scheduledExecMaxPerTarget; i++ {
			seedExecEvent(t, f, f.sibling.ID)
		}
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(f.sibling.ID, "true"))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)
		// Another target still has room.
		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(f.child.ID, "true"))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
	t.Run("per project", func(t *testing.T) {
		f := execAuthoringFixture(t)
		for i := 0; i < scheduledExecMaxPerProject; i++ {
			seedExecEvent(t, f, uuid.NewString())
		}
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(f.sibling.ID, "true"))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)
	})
}

// TestScheduledExecAuthoring_RateLimited: one principal gets a burst of 10
// exec authoring attempts, then 429 with Retry-After.
func TestScheduledExecAuthoring_RateLimited(t *testing.T) {
	f := execAuthoringFixture(t)
	for i := 0; i < scheduledExecAuthoringBurst; i++ {
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(f.sibling.ID, "true"))
		require.Equal(t, http.StatusCreated, rec.Code, "attempt %d: %s", i, rec.Body.String())
	}
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f), execCreateBody(f.sibling.ID, "true"))
	require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeRateLimited, decodeTargetAPIError(t, rec).Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Len(t, storedExecEvents(t, f), scheduledExecAuthoringBurst)
}

// TestScheduledExecAuthoring_Audit (AC12): create and cancel emit
// content-free schedule.exec records.
func TestScheduledExecAuthoring_Audit(t *testing.T) {
	f := execAuthoringFixture(t)
	logs := auditCapture(t, f.srv)
	sentinel := "EXECAUDIT-" + uuid.NewString()
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f),
		execCreateBody(f.sibling.ID, "/usr/bin/env", "TOKEN="+sentinel, "true"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id, _ := decodeEventResponse(t, rec)["id"].(string)
	require.NotEmpty(t, id)

	rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodDelete, execEventsPath(f)+"/"+id, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	out := logs.String()
	assert.Contains(t, out, "audit_action=schedule.exec.create")
	assert.Contains(t, out, "audit_action=schedule.exec.delete")
	assert.Contains(t, out, "command_sha256=")
	assert.Contains(t, out, "argc=3")
	assert.Contains(t, out, "argv0=env")
	assert.Contains(t, out, "scheduled_event_id="+id)
	assert.NotContains(t, out, sentinel, "no argv beyond argv0 in the audit")
	assert.NotContains(t, out, "/usr/bin/env", "argv0 is the basename only")
}

// TestScheduledExecAuthoring_TargetDeletionCancelsPending (AC11, one-shot):
// deleting the target cancels its pending exec events.
func TestScheduledExecAuthoring_TargetDeletionCancelsPending(t *testing.T) {
	f := execAuthoringFixture(t)
	hit := seedExecEvent(t, f, f.sibling.ID)
	miss := seedExecEvent(t, f, f.child.ID)
	f.srv.cancelScheduledEventsForAgent(context.Background(), f.sibling)
	got, err := f.store.GetScheduledEvent(context.Background(), hit.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, got.Status)
	got, err = f.store.GetScheduledEvent(context.Background(), miss.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventPending, got.Status)
}
