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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Token admission on the scheduled-event and schedule routes: reads, list,
// cancellation, deletion and pause admit a user access token with the
// matching scheduled_event selector; authoring (create of either event type,
// update, resume) refuses every token before any target lookup. Every token
// here is minted through UserAccessTokenService and sent through the server
// handler.

// scheduleTokenAllSelectors is every selector a schedule route could use,
// plus the agent selectors scheduled authoring checks.
var scheduleTokenAllSelectors = []string{
	"scheduled_event:read", "scheduled_event:list", "scheduled_event:update", "scheduled_event:delete",
	"agent:create", "agent:message",
}

// scheduleTokenFixture is a schedule test project with a project owner who
// can mint tokens.
type scheduleTokenFixture struct {
	srv     *Server
	store   store.Store
	project string
	owner   UserIdentity
}

func newScheduleTokenFixture(t *testing.T, name string) scheduleTokenFixture {
	t.Helper()
	srv, s, projectID := setupScheduleTest(t)
	owner := setupScopedDispatchAgentOwner(t, srv, s, projectID, tid(name+"-owner"))
	ensureHubMembership(context.Background(), s, owner.ID())
	return scheduleTokenFixture{srv: srv, store: s, project: projectID, owner: owner}
}

// mint returns a real token of the fixture owner.
func (f scheduleTokenFixture) mint(t *testing.T, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.owner.ID()), CreateTokenParams{
		UserID: f.owner.ID(), Name: "sched-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	return key
}

// tokens returns a project token and a hub token of the owner with scopes.
func (f scheduleTokenFixture) tokens(t *testing.T, scopes ...string) map[string]string {
	t.Helper()
	return map[string]string{
		"project token": f.mint(t, projectBoundary(f.project), scopes...),
		"hub token":     f.mint(t, hubBoundary(), scopes...),
	}
}

func (f scheduleTokenFixture) path(rest string) string {
	return "/api/v1/projects/" + f.project + rest
}

// scheduleTokenRaw sends a raw body with a token through the server handler.
func scheduleTokenRaw(t *testing.T, srv *Server, key, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (f scheduleTokenFixture) scheduleStatus(t *testing.T, id string) string {
	t.Helper()
	sc, err := f.store.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	return sc.Status
}

func (f scheduleTokenFixture) scheduleCount(t *testing.T) int {
	t.Helper()
	res, err := f.store.ListSchedules(context.Background(), store.ScheduleFilter{ProjectID: f.project}, store.ListOptions{})
	require.NoError(t, err)
	return len(res.Items)
}

func (f scheduleTokenFixture) eventCount(t *testing.T) int {
	t.Helper()
	res, err := f.store.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: f.project}, store.ListOptions{})
	require.NoError(t, err)
	return len(res.Items)
}

// createOwnerEvent creates a one-shot dispatch_agent event as the owner over
// a session and returns its ID.
func (f scheduleTokenFixture) createOwnerEvent(t *testing.T) string {
	t.Helper()
	rec := doAuthoredEventRequest(t, f.srv, f.owner, f.project,
		CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "sched-token-" + tid("evt")})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var evt store.ScheduledEvent
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evt))
	return evt.ID
}

// TestScheduledMessageAuthoring_RefusesTokensBeforeTargetLookup requires the
// scheduled-message authoring rule to refuse a token before the target is
// resolved: a token naming a target that does not exist is refused with the
// GOV_PENDING reason, where a session naming the same target is admitted
// (the target is checked again when the event fires). Over the routes, a
// token is refused before the body is read, and nothing is written.
func TestScheduledMessageAuthoring_RefusesTokensBeforeTargetLookup(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-msg-lookup")
	missingTarget := `{"agentId":"` + tid("no-such-agent") + `","message":"x"}`

	for label, key := range f.tokens(t, scheduleTokenAllSelectors...) {
		ctx := realTokenContext(t, f.srv, key)
		for _, args := range []struct{ payload, agentID, agentName string }{
			{missingTarget, "", ""},
			{"", "", "no-such-agent"},
			{"", "", ""},
		} {
			rec := httptest.NewRecorder()
			ok := f.srv.authorizeScheduledMessageAuthoring(rec, requestWithContext(ctx, http.MethodPost, f.path("/schedules"), nil),
				f.project, args.payload, args.agentID, args.agentName)
			assert.False(t, ok, "%s: %+v", label, args)
			assertScheduleAuthoringRefused(t, rec)
		}

		before, beforeEvents := f.scheduleCount(t), f.eventCount(t)
		cases := []struct{ name, path, body string }{
			{"message schedule, unknown target", "/schedules", `{"name":"tok-msg","cronExpr":"0 * * * *","eventType":"message","agentName":"no-such-agent","message":"x"}`},
			{"message event, unknown target", "/scheduled-events", `{"eventType":"message","fireIn":"1h","payload":` + jsonQuote(missingTarget) + `}`},
			{"dispatch_agent schedule", "/schedules", `{"name":"tok-da","cronExpr":"0 * * * *","eventType":"dispatch_agent","agentName":"tok-worker"}`},
			{"dispatch_agent event", "/scheduled-events", `{"eventType":"dispatch_agent","fireIn":"1h","agentName":"tok-worker"}`},
			{"malformed schedule body", "/schedules", `{`},
			{"malformed event body", "/scheduled-events", `{`},
		}
		for _, tc := range cases {
			rec := scheduleTokenRaw(t, f.srv, key, http.MethodPost, f.path(tc.path), tc.body)
			t.Logf("%s, %s: %d", label, tc.name, rec.Code)
			assertScheduleAuthoringRefused(t, rec)
		}
		assert.Equal(t, before, f.scheduleCount(t), "%s: no schedule written", label)
		assert.Equal(t, beforeEvents, f.eventCount(t), "%s: no event written", label)
	}

	// A session naming the same missing target is admitted by the rule.
	session := authoredRequest(t, f.owner, http.MethodPost, f.path("/schedules"), nil)
	rec := httptest.NewRecorder()
	assert.True(t, f.srv.authorizeScheduledMessageAuthoring(rec, session, f.project, missingTarget, "", ""), rec.Body.String())
}

// jsonQuote returns s as a JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestScheduleResume_RefusesTokensForEveryEventType requires a resume to
// refuse every token, for a message and a dispatch_agent schedule alike,
// on both boundaries and with every schedule selector: the schedule stays
// paused and its authorization revision stays as it was. The owner's session
// resumes the same schedules.
func TestScheduleResume_RefusesTokensForEveryEventType(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-resume-tok")
	keys := f.tokens(t, scheduleTokenAllSelectors...)

	for _, eventType := range []string{"message", "dispatch_agent"} {
		id := createOwnerSchedule(t, f.srv, f.owner, f.project, "resume-tok-"+eventType, eventType)
		pauseSchedule(t, f.srv, f.owner, f.project, id)
		before := loadScheduleRevision(t, f.store, id)

		for label, key := range keys {
			rec := doRequestWithToken(t, f.srv, key, http.MethodPost, f.path("/schedules/"+id+"/resume"), nil)
			assertScheduleAuthoringRefused(t, rec)
			assert.Equal(t, store.ScheduleStatusPaused, f.scheduleStatus(t, id), "%s, %s", eventType, label)
			assert.Equal(t, before, loadScheduleRevision(t, f.store, id), "%s, %s", eventType, label)
		}

		rec := doAuthoredScheduleRequest(t, f.srv, f.owner, f.project, id+"/resume", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: session resume: %s", eventType, rec.Body.String())
	}
}

// TestScheduleUpdate_RefusesTokensForEveryEventType requires an update to
// refuse every token, for a message and a dispatch_agent schedule alike, on
// both boundaries and with every schedule selector, whatever the update
// changes (a name only, the payload, or the status). Nothing changes. The
// owner's session updates the same schedules.
func TestScheduleUpdate_RefusesTokensForEveryEventType(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-update-tok")
	keys := f.tokens(t, scheduleTokenAllSelectors...)

	for _, eventType := range []string{"message", "dispatch_agent"} {
		name := "update-tok-" + eventType
		id := createOwnerSchedule(t, f.srv, f.owner, f.project, name, eventType)
		payload := `{"agentName":"retarget"}`
		if eventType == "message" {
			payload = `{"agentName":"retarget","message":"x"}`
		}
		before := loadScheduleRevision(t, f.store, id)

		for label, key := range keys {
			for _, body := range []UpdateScheduleRequest{
				{Name: name + "-renamed"},
				{Payload: payload},
				{Status: store.ScheduleStatusPaused},
			} {
				rec := doRequestWithToken(t, f.srv, key, http.MethodPatch, f.path("/schedules/"+id), body)
				assertScheduleAuthoringRefused(t, rec)
				sc, err := f.store.GetSchedule(context.Background(), id)
				require.NoError(t, err)
				assert.Equal(t, name, sc.Name, "%s, %s", eventType, label)
				assert.Equal(t, store.ScheduleStatusActive, sc.Status, "%s, %s", eventType, label)
				assert.Equal(t, before, loadScheduleRevision(t, f.store, id), "%s, %s", eventType, label)
			}
		}

		rec := doAuthoredScheduleRequest(t, f.srv, f.owner, f.project, id, http.MethodPatch, UpdateScheduleRequest{Name: name + "-renamed"})
		require.Equal(t, http.StatusOK, rec.Code, "%s: session update: %s", eventType, rec.Body.String())
	}
}

// TestSchedulePause_AdmitsTokenWithUpdateSelector requires a pause by a
// token to need scheduled_event:update on a boundary that contains the
// schedule's project: a project token or a hub token with the selector
// pauses the schedule without changing its authorization revision; a token
// without the selector, or a project token for another project, is refused
// and the schedule stays active.
func TestSchedulePause_AdmitsTokenWithUpdateSelector(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-pause-tok")
	ctx := context.Background()

	other := tid("sched-pause-other")
	require.NoError(t, f.store.CreateProject(ctx, &store.Project{ID: other, Name: "pause-other", Slug: other}))
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, other, f.owner.ID()))

	refused := map[string]string{
		"project token without the selector": f.mint(t, projectBoundary(f.project), "scheduled_event:read", "scheduled_event:list", "scheduled_event:delete"),
		"hub token without the selector":     f.mint(t, hubBoundary(), "scheduled_event:read", "scheduled_event:list", "scheduled_event:delete"),
		"project token for another project":  f.mint(t, projectBoundary(other), "scheduled_event:update"),
	}
	id := createOwnerSchedule(t, f.srv, f.owner, f.project, "pause-refused", "message")
	for label, key := range refused {
		rec := doRequestWithToken(t, f.srv, key, http.MethodPost, f.path("/schedules/"+id+"/pause"), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s: %s", label, rec.Body.String())
		assert.Equal(t, store.ScheduleStatusActive, f.scheduleStatus(t, id), label)
	}

	for label, key := range f.tokens(t, "scheduled_event:update") {
		for _, eventType := range []string{"message", "dispatch_agent"} {
			id := createOwnerSchedule(t, f.srv, f.owner, f.project, "pause-"+eventType+"-"+label, eventType)
			before := loadScheduleRevision(t, f.store, id)
			rec := doRequestWithToken(t, f.srv, key, http.MethodPost, f.path("/schedules/"+id+"/pause"), nil)
			require.Equal(t, http.StatusOK, rec.Code, "%s, %s: %s", label, eventType, rec.Body.String())
			assert.Equal(t, store.ScheduleStatusPaused, f.scheduleStatus(t, id))
			assert.Equal(t, before, loadScheduleRevision(t, f.store, id), "a pause writes no new revision")
		}
	}
}

// TestScheduleUpdateSelector_DoesNotAdmitAuthoring requires a token holding
// only scheduled_event:update to be refused on every authoring route
// (create of either event type on both collections, update, resume) with
// the GOV_PENDING reason, while the same token pauses a schedule.
func TestScheduleUpdateSelector_DoesNotAdmitAuthoring(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-upd-sel")
	paused := createOwnerSchedule(t, f.srv, f.owner, f.project, "upd-sel-paused", "dispatch_agent")
	pauseSchedule(t, f.srv, f.owner, f.project, paused)
	active := createOwnerSchedule(t, f.srv, f.owner, f.project, "upd-sel-active", "message")
	before := f.scheduleCount(t)

	for label, key := range f.tokens(t, "scheduled_event:update") {
		authoring := []struct {
			method, path string
			body         interface{}
		}{
			{http.MethodPost, "/schedules", CreateScheduleRequest{Name: "upd-sel-new", CronExpr: "0 * * * *", EventType: "message", AgentName: authzHelperAgentSlug, Message: "x"}},
			{http.MethodPost, "/schedules", CreateScheduleRequest{Name: "upd-sel-new-da", CronExpr: "0 * * * *", EventType: "dispatch_agent", AgentName: "w"}},
			{http.MethodPost, "/scheduled-events", CreateScheduledEventRequest{EventType: "message", FireIn: "1h", AgentName: authzHelperAgentSlug, Message: "x"}},
			{http.MethodPost, "/scheduled-events", CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "w"}},
			{http.MethodPatch, "/schedules/" + active, UpdateScheduleRequest{Name: "x"}},
			{http.MethodPost, "/schedules/" + paused + "/resume", nil},
		}
		for _, tc := range authoring {
			rec := doRequestWithToken(t, f.srv, key, tc.method, f.path(tc.path), tc.body)
			t.Logf("%s: %s %s: %d", label, tc.method, tc.path, rec.Code)
			assertScheduleAuthoringRefused(t, rec)
		}
		assert.Equal(t, before, f.scheduleCount(t), label)
		assert.Equal(t, store.ScheduleStatusPaused, f.scheduleStatus(t, paused), label)
	}

	key := f.mint(t, projectBoundary(f.project), "scheduled_event:update")
	rec := doRequestWithToken(t, f.srv, key, http.MethodPost, f.path("/schedules/"+active+"/pause"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, store.ScheduleStatusPaused, f.scheduleStatus(t, active))
}

// TestScheduleTokens_ReadsListsAndDeletesUseExactSelectors requires each
// token-admitted schedule route to need its own selector: list routes need
// scheduled_event:list, single-record reads and history need
// scheduled_event:read, and cancel or delete need scheduled_event:delete. A
// token with a different scheduled_event selector is refused.
func TestScheduleTokens_ReadsListsAndDeletesUseExactSelectors(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-exact-sel")
	schedule := createOwnerSchedule(t, f.srv, f.owner, f.project, "exact-sel", "message")
	event := f.createOwnerEvent(t)

	reads := []struct{ path, selector string }{
		{"/schedules", "scheduled_event:list"},
		{"/scheduled-events", "scheduled_event:list"},
		{"/schedules/" + schedule, "scheduled_event:read"},
		{"/schedules/" + schedule + "/history", "scheduled_event:read"},
		{"/scheduled-events/" + event, "scheduled_event:read"},
	}
	for _, tc := range reads {
		wrong := "scheduled_event:read"
		if tc.selector == wrong {
			wrong = "scheduled_event:list"
		}
		for _, boundary := range []TokenBoundary{projectBoundary(f.project), hubBoundary()} {
			rec := doRequestWithToken(t, f.srv, f.mint(t, boundary, tc.selector), http.MethodGet, f.path(tc.path), nil)
			assert.Equal(t, http.StatusOK, rec.Code, "%s with %s (%s): %s", tc.path, tc.selector, boundary.Kind, rec.Body.String())
			rec = doRequestWithToken(t, f.srv, f.mint(t, boundary, wrong, "scheduled_event:update", "scheduled_event:delete"), http.MethodGet, f.path(tc.path), nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s without %s (%s): %s", tc.path, tc.selector, boundary.Kind, rec.Body.String())
		}
	}

	without := f.mint(t, projectBoundary(f.project), "scheduled_event:read", "scheduled_event:list", "scheduled_event:update")
	for _, path := range []string{"/schedules/" + schedule, "/scheduled-events/" + event} {
		rec := doRequestWithToken(t, f.srv, without, http.MethodDelete, f.path(path), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "%s without the delete selector: %s", path, rec.Body.String())
	}
	_, err := f.store.GetSchedule(context.Background(), schedule)
	require.NoError(t, err, "a refused delete leaves the schedule")

	with := f.mint(t, projectBoundary(f.project), "scheduled_event:delete")
	for _, path := range []string{"/schedules/" + schedule, "/scheduled-events/" + event} {
		rec := doRequestWithToken(t, f.srv, with, http.MethodDelete, f.path(path), nil)
		assert.Equal(t, http.StatusNoContent, rec.Code, "%s with the delete selector: %s", path, rec.Body.String())
	}
	evt, err := f.store.GetScheduledEvent(context.Background(), event)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventCancelled, evt.Status)
}
