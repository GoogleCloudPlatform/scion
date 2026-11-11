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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Schedule authoring by a user access token: create, update and resume
// admit a token with scheduled_event:create or scheduled_event:update on a
// boundary that contains the schedule's project, after the event type's
// target checks. Each admitted revision records the token's attribution and
// its frozen ceiling and boundary, and a fire runs under that recorded
// ceiling and boundary. Every token is minted through
// UserAccessTokenService and sent through the server handler.

// scheduleTokenAuthoringSelectors are the selectors a token needs to create
// both event types against an agent it may message.
var scheduleTokenAuthoringSelectors = []string{"scheduled_event:create", "agent:create", "agent:read", "agent:message"}

// assertTokenRevision checks that a scheduled row records tok as its
// author: uat attribution naming the token, and the token's bounded
// ceiling with its boundary.
func assertTokenRevision(t *testing.T, label string, tok *store.UserAccessToken, attr store.InitiatorAttribution, c store.EffectCeiling) {
	t.Helper()
	assert.Equal(t, store.InitiatorCredentialKindUAT, attr.InitiatorCredentialKind, label)
	assert.Equal(t, tok.ID, attr.InitiatorCredentialID, label)
	assert.Equal(t, tok.UserID, attr.InitiatorPrincipalID, label)
	assert.Equal(t, store.EffectCeilingBounded, c.Kind, label)
	assert.Equal(t, tok.CeilingVersion, c.Version, label)
	assert.ElementsMatch(t, tok.CeilingPermissionIDs, c.PermissionIDs, label)
	assert.Equal(t, tok.BoundaryKind, c.BoundaryKind, label)
	assert.Equal(t, tok.ProjectID, c.BoundaryProjectID, label)
}

// decodeCreatedID returns the ID of the schedule or event a 201 response
// carries.
func decodeCreatedID(t *testing.T, body []byte) string {
	t.Helper()
	var created struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &created))
	require.NotEmpty(t, created.ID)
	return created.ID
}

// ownerOfOtherProjectOnly creates a hub member who owns a new project and
// holds no role in the fixture project, and returns the user's ID.
func (f scheduleTokenFixture) ownerOfOtherProjectOnly(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	projectID := tid(name)
	require.NoError(t, f.store.CreateProject(ctx, &store.Project{ID: projectID, Name: name, Slug: projectID}))
	user := setupScopedDispatchAgentOwner(t, f.srv, f.store, projectID, tid(name+"-user"))
	ensureHubMembership(ctx, f.store, user.ID())
	return user.ID()
}

// mintFor returns a real token of userID, minted through the token service.
func (f scheduleTokenFixture) mintFor(t *testing.T, userID string, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "sched-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token of %s with %v", boundary.Kind, userID, scopes)
	return key
}

// TestScheduleAuthoringToken_RequiresCreateSelector requires creating a
// message or dispatch_agent schedule or one-shot event with a token to need
// scheduled_event:create on a boundary that contains the project:
//   - a project token or a hub token with the selector (and the event
//     type's agent selectors) creates all four, and each row records the
//     token's attribution, ceiling and boundary;
//   - a token without the selector, holding every other schedule and agent
//     selector, is refused with 403 on both boundaries;
//   - a project token for another project the owner owns is refused;
//   - a hub token with the selectors is refused when its user holds no
//     authority in the project (the user owns only another project);
//   - the event type's target checks still apply: without agent:message the
//     token may not schedule a message to an agent it can read, and without
//     agent:create it may not schedule a dispatch_agent run.
//
// Refused requests write nothing. The owner's session creates the same four
// rows, each recording a principal ceiling.
func TestScheduleAuthoringToken_RequiresCreateSelector(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-auth-create")
	other := f.otherOwnedProject(t, "sched-auth-create-other")
	outsider := f.ownerOfOtherProjectOnly(t, "sched-auth-create-outsider")

	type createCase struct{ name, path, body string }
	creates := func(tag string) []createCase {
		return []createCase{
			{"message schedule", "/schedules", `{"name":"` + tag + `-msg","cronExpr":"0 * * * *","eventType":"message","agentName":"` + authzHelperAgentSlug + `","message":"x"}`},
			{"message event", "/scheduled-events", `{"eventType":"message","fireIn":"1h","agentName":"` + authzHelperAgentSlug + `","message":"x"}`},
			{"dispatch_agent schedule", "/schedules", `{"name":"` + tag + `-da","cronExpr":"0 * * * *","eventType":"dispatch_agent","agentName":"` + tag + `-worker"}`},
			{"dispatch_agent event", "/scheduled-events", `{"eventType":"dispatch_agent","fireIn":"1h","agentName":"` + tag + `-worker"}`},
		}
	}
	refuseAll := func(label, key string) {
		t.Helper()
		before, beforeEvents := f.scheduleCount(t), f.eventCount(t)
		for _, tc := range creates("refused") {
			rec := scheduleTokenRaw(t, f.srv, key, http.MethodPost, f.path(tc.path), tc.body)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s, %s: %s", label, tc.name, rec.Body.String())
		}
		assert.Equal(t, before, f.scheduleCount(t), "%s: no schedule written", label)
		assert.Equal(t, beforeEvents, f.eventCount(t), "%s: no event written", label)
	}

	// Without scheduled_event:create, on either boundary.
	withoutCreate := []string{"scheduled_event:read", "scheduled_event:list", "scheduled_event:update", "scheduled_event:delete", "agent:create", "agent:read", "agent:message"}
	refuseAll("project token without the selector", f.mint(t, projectBoundary(f.project), withoutCreate...))
	refuseAll("hub token without the selector", f.mint(t, hubBoundary(), withoutCreate...))
	// On the wrong boundary.
	refuseAll("project token for another project", f.mint(t, projectBoundary(other), scheduleTokenAuthoringSelectors...))
	// Without live authority in the project.
	refuseAll("hub token of a user who owns only another project", f.mintFor(t, outsider, hubBoundary(), scheduleTokenAuthoringSelectors...))

	// The event type's target checks.
	noMessage := f.mint(t, projectBoundary(f.project), "scheduled_event:create", "agent:read")
	for _, tc := range creates("nomsg")[:2] {
		rec := scheduleTokenRaw(t, f.srv, noMessage, http.MethodPost, f.path(tc.path), tc.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "without agent:message, %s: %s", tc.name, rec.Body.String())
	}
	noCreate := f.mint(t, projectBoundary(f.project), "scheduled_event:create", "agent:read", "agent:message")
	for _, tc := range creates("noagent")[2:] {
		rec := scheduleTokenRaw(t, f.srv, noCreate, http.MethodPost, f.path(tc.path), tc.body)
		assert.Equal(t, http.StatusForbidden, rec.Code, "without agent:create, %s: %s", tc.name, rec.Body.String())
	}
	assert.Equal(t, 0, f.scheduleCount(t), "no schedule written by a refused token")
	assert.Equal(t, 0, f.eventCount(t), "no event written by a refused token")

	// Admitted on both boundaries.
	for _, boundary := range []TokenBoundary{projectBoundary(f.project), hubBoundary()} {
		key, tok := f.mintToken(t, boundary, scheduleTokenAuthoringSelectors...)
		for _, tc := range creates("tok-" + string(boundary.Kind)) {
			label := string(boundary.Kind) + " token, " + tc.name
			rec := scheduleTokenRaw(t, f.srv, key, http.MethodPost, f.path(tc.path), tc.body)
			require.Equal(t, http.StatusCreated, rec.Code, "%s: %s", label, rec.Body.String())
			id := decodeCreatedID(t, rec.Body.Bytes())
			if tc.path == "/schedules" {
				sc, err := f.store.GetSchedule(context.Background(), id)
				require.NoError(t, err)
				assertTokenRevision(t, label, tok, sc.InitiatorAttribution, sc.AuthorityCeiling)
			} else {
				f.srv.scheduler.StopEventTimer(id)
				evt, err := f.store.GetScheduledEvent(context.Background(), id)
				require.NoError(t, err)
				assertTokenRevision(t, label, tok, evt.InitiatorAttribution, evt.AuthorityCeiling)
			}
		}
	}

	// The owner's session creates the same rows.
	for _, tc := range creates("session") {
		var body interface{}
		if tc.path == "/schedules" {
			var req CreateScheduleRequest
			require.NoError(t, json.Unmarshal([]byte(tc.body), &req))
			body = req
			rec := doAuthoredScheduleRequest(t, f.srv, f.owner, f.project, "", http.MethodPost, body)
			require.Equal(t, http.StatusCreated, rec.Code, "session, %s: %s", tc.name, rec.Body.String())
			sc, err := f.store.GetSchedule(context.Background(), decodeCreatedID(t, rec.Body.Bytes()))
			require.NoError(t, err)
			assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, sc.AuthorityCeiling, tc.name)
			continue
		}
		var req CreateScheduledEventRequest
		require.NoError(t, json.Unmarshal([]byte(tc.body), &req))
		rec := doAuthoredEventRequest(t, f.srv, f.owner, f.project, req)
		require.Equal(t, http.StatusCreated, rec.Code, "session, %s: %s", tc.name, rec.Body.String())
		id := decodeCreatedID(t, rec.Body.Bytes())
		f.srv.scheduler.StopEventTimer(id)
		evt, err := f.store.GetScheduledEvent(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, evt.AuthorityCeiling, tc.name)
	}
}

// TestScheduleResumeToken_RechecksTarget requires a resume by a token to
// re-check the stored schedule's target with the resuming token's own
// authority, at resume time, before anything is written:
//   - a message schedule whose target the token may not message (no
//     agent:message) is refused on both boundaries, though the target was
//     checked when the schedule was created;
//   - a dispatch_agent schedule is refused for a token without agent:create;
//   - a token without scheduled_event:update, or a project token for
//     another project, is refused;
//   - a hub token with the resuming selectors is refused when its user
//     holds no authority in the project (the user owns only another
//     project).
//
// A refused resume leaves the schedule paused with its revision as it was.
// A token with the selectors resumes the schedule, and the new revision
// records the token's attribution, ceiling and boundary. The owner's
// session resumes it too, recording a principal ceiling.
func TestScheduleResumeToken_RechecksTarget(t *testing.T) {
	f := newScheduleTokenFixture(t, "sched-resume-target")
	other := f.otherOwnedProject(t, "sched-resume-target-other")
	outsider := f.ownerOfOtherProjectOnly(t, "sched-resume-target-outsider")

	type resumeCheck struct {
		eventType string
		refused   map[string]string
		admitting []string
	}
	checks := []resumeCheck{
		{
			eventType: "message",
			refused: map[string]string{
				"project token without agent:message":          f.mint(t, projectBoundary(f.project), "scheduled_event:update", "agent:read"),
				"hub token without agent:message":              f.mint(t, hubBoundary(), "scheduled_event:update", "agent:read"),
				"project token without scheduled_event:update": f.mint(t, projectBoundary(f.project), "scheduled_event:create", "agent:read", "agent:message"),
				"project token for another project":            f.mint(t, projectBoundary(other), "scheduled_event:update", "agent:read", "agent:message"),
				"hub token of a user outside the project":      f.mintFor(t, outsider, hubBoundary(), "scheduled_event:update", "agent:message"),
			},
			admitting: []string{"scheduled_event:update", "agent:message"},
		},
		{
			eventType: "dispatch_agent",
			refused: map[string]string{
				"project token without agent:create":           f.mint(t, projectBoundary(f.project), "scheduled_event:update", "agent:read"),
				"hub token without agent:create":               f.mint(t, hubBoundary(), "scheduled_event:update", "agent:read"),
				"project token without scheduled_event:update": f.mint(t, projectBoundary(f.project), "scheduled_event:create", "agent:create"),
				"project token for another project":            f.mint(t, projectBoundary(other), "scheduled_event:update", "agent:create"),
				"hub token of a user outside the project":      f.mintFor(t, outsider, hubBoundary(), "scheduled_event:update", "agent:create"),
			},
			admitting: []string{"scheduled_event:update", "agent:create"},
		},
	}

	for _, c := range checks {
		id := createOwnerSchedule(t, f.srv, f.owner, f.project, "resume-target-"+c.eventType, c.eventType)
		pauseSchedule(t, f.srv, f.owner, f.project, id)
		before := loadScheduleRevision(t, f.store, id)

		for label, key := range c.refused {
			rec := doRequestWithToken(t, f.srv, key, http.MethodPost, f.path("/schedules/"+id+"/resume"), nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s, %s: %s", c.eventType, label, rec.Body.String())
			assert.Equal(t, store.ScheduleStatusPaused, f.scheduleStatus(t, id), "%s, %s", c.eventType, label)
			assert.Equal(t, before, loadScheduleRevision(t, f.store, id), "%s, %s", c.eventType, label)
		}

		for _, boundary := range []TokenBoundary{projectBoundary(f.project), hubBoundary()} {
			label := c.eventType + ", " + string(boundary.Kind) + " token"
			prev := loadScheduleRevision(t, f.store, id)
			key, tok := f.mintToken(t, boundary, c.admitting...)
			rec := doRequestWithToken(t, f.srv, key, http.MethodPost, f.path("/schedules/"+id+"/resume"), nil)
			require.Equal(t, http.StatusOK, rec.Code, "%s: %s", label, rec.Body.String())
			assert.Equal(t, store.ScheduleStatusActive, f.scheduleStatus(t, id), label)
			after := loadScheduleRevision(t, f.store, id)
			assertTokenRevision(t, label, tok, after.Attribution, after.Ceiling)
			assert.Equal(t, prev.Attribution.AuthorizationRevision+1, after.Attribution.AuthorizationRevision, label)
			pauseSchedule(t, f.srv, f.owner, f.project, id)
		}

		rec := doAuthoredScheduleRequest(t, f.srv, f.owner, f.project, id+"/resume", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: session resume: %s", c.eventType, rec.Body.String())
		assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, loadScheduleRevision(t, f.store, id).Ceiling, c.eventType)
	}
}

// schedFireMint mints a real token of the scheduled dispatch fixture's
// creator.
func schedFireMint(t *testing.T, f *schedFire, boundary TokenBoundary, scopes ...string) (string, *store.UserAccessToken) {
	t.Helper()
	key, tok, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.creator.ID), CreateTokenParams{
		UserID: f.creator.ID, Name: "sched-fire-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	return key, tok
}

// withFireScheduler gives srv a scheduler with the message and
// dispatch_agent event handlers, so the routes can store events and a test
// can fire them.
func withFireScheduler(srv *Server, s store.Store) {
	srv.scheduler = NewScheduler(s, slog.Default())
	srv.scheduler.RegisterEventHandler("message", srv.messageEventHandler())
	srv.scheduler.RegisterEventHandler("dispatch_agent", srv.dispatchAgentEventHandler())
}

// createTokenEvent creates a one-shot event with key over the route and
// returns the stored event, its timer stopped.
func createTokenEvent(t *testing.T, srv *Server, s store.Store, key, projectID string, body interface{}) store.ScheduledEvent {
	t.Helper()
	rec := doRequestWithToken(t, srv, key, http.MethodPost, "/api/v1/projects/"+projectID+"/scheduled-events", body)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	id := decodeCreatedID(t, rec.Body.Bytes())
	srv.scheduler.StopEventTimer(id)
	evt, err := s.GetScheduledEvent(context.Background(), id)
	require.NoError(t, err)
	return *evt
}

// fireStoredEvent fires evt through the scheduler and returns the stored
// event afterwards.
func fireStoredEvent(t *testing.T, srv *Server, s store.Store, evt store.ScheduledEvent) *store.ScheduledEvent {
	t.Helper()
	srv.scheduler.fireEvent(context.Background(), evt, false)
	got, err := s.GetScheduledEvent(context.Background(), evt.ID)
	require.NoError(t, err)
	return got
}

// TestScheduledFire_UsesRecordedTokenCeiling requires a token-authored
// schedule or event to fire under the ceiling recorded when the token
// authored it:
//   - a dispatch_agent event, and a run of a dispatch_agent schedule,
//     create a child whose delegation edge carries exactly the recorded
//     token ceiling;
//   - a message event whose recorded ceiling carries agent.message is
//     delivered, and one whose recorded ceiling does not is refused at fire
//     time and fails with nothing delivered.
func TestScheduledFire_UsesRecordedTokenCeiling(t *testing.T) {
	t.Run("dispatch_agent", func(t *testing.T) {
		f := newSchedFire(t, "fire-tok-ceil")
		withFireScheduler(f.srv, f.store)
		selectors := append(minimalSelectors(t), "scheduled_event:create")
		key, tok := schedFireMint(t, f, projectBoundary(f.proj.ID), selectors...)

		// One-shot event, fired by the scheduler.
		evt := createTokenEvent(t, f.srv, f.store, key, f.proj.ID,
			CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "fire-tok-ceil-c"})
		assertTokenRevision(t, "dispatch_agent event", tok, evt.InitiatorAttribution, evt.AuthorityCeiling)
		got := fireStoredEvent(t, f.srv, f.store, evt)
		require.Equal(t, store.ScheduledEventFired, got.Status, got.Error)
		_, edge := f.child(t, "fire-tok-ceil-c")
		assertSchedulerProvenance(t, evt, edge, store.DelegationPrincipalUser, f.creator.ID)
		assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
		assert.Equal(t, evt.AuthorityCeiling.Version, edge.Version)
		assert.Equal(t, evt.AuthorityCeiling.PermissionIDs, edge.PermissionIDs)

		// Recurring schedule, run by the schedule evaluator.
		rec := doRequestWithToken(t, f.srv, key, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/schedules",
			CreateScheduleRequest{Name: "fire-tok-ceil-r", CronExpr: "0 * * * *", EventType: "dispatch_agent", AgentName: "fire-tok-ceil-r"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		sc, err := f.store.GetSchedule(context.Background(), decodeCreatedID(t, rec.Body.Bytes()))
		require.NoError(t, err)
		assertTokenRevision(t, "dispatch_agent schedule", tok, sc.InitiatorAttribution, sc.AuthorityCeiling)
		f.srv.executeSchedule(context.Background(), *sc, sc.CreatedAt)
		res, err := f.store.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ScheduleID: sc.ID}, store.ListOptions{})
		require.NoError(t, err)
		require.Len(t, res.Items, 1)
		require.Equal(t, store.ScheduledEventFired, res.Items[0].Status, res.Items[0].Error)
		assert.Equal(t, sc.AuthorityCeiling.PermissionIDs, res.Items[0].AuthorityCeiling.PermissionIDs)
		_, edge = f.child(t, "fire-tok-ceil-r")
		assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
		assert.Equal(t, sc.AuthorityCeiling.PermissionIDs, edge.PermissionIDs)
	})

	t.Run("message", func(t *testing.T) {
		f := newSchedMsg(t, "fire-tok-msg")
		withFireScheduler(f.srv, f.store)
		body := CreateScheduledEventRequest{EventType: "message", FireIn: "1h", AgentID: f.target.ID, Message: "scheduled hello"}

		withMessage, tok := schedFireMint(t, f.schedFire, projectBoundary(f.proj.ID), "scheduled_event:create", "agent:read", "agent:message")
		evt := createTokenEvent(t, f.srv, f.store, withMessage, f.proj.ID, body)
		assertTokenRevision(t, "message event with agent:message", tok, evt.InitiatorAttribution, evt.AuthorityCeiling)
		require.Contains(t, evt.AuthorityCeiling.PermissionIDs, scheduledMessagePermission)
		got := fireStoredEvent(t, f.srv, f.store, evt)
		require.Equal(t, store.ScheduledEventFired, got.Status, got.Error)
		f.assertDelivered(t, 1)

		// A token without agent:read or agent:message cannot see the
		// target, so authoring treats it like an unknown agent; the
		// recorded ceiling has no agent.message and the fire is refused.
		without, tok := schedFireMint(t, f.schedFire, projectBoundary(f.proj.ID), "scheduled_event:create")
		evt = createTokenEvent(t, f.srv, f.store, without, f.proj.ID, body)
		assertTokenRevision(t, "message event without agent:message", tok, evt.InitiatorAttribution, evt.AuthorityCeiling)
		require.NotContains(t, evt.AuthorityCeiling.PermissionIDs, scheduledMessagePermission)
		err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
		require.ErrorIs(t, err, errScheduledAuthorityDenied)
		assert.Contains(t, err.Error(), "revision ceiling does not allow "+scheduledMessagePermission)
		got = fireStoredEvent(t, f.srv, f.store, evt)
		assert.Equal(t, store.ScheduledEventFailed, got.Status)
		assert.Equal(t, errScheduledMessageRefused.Error(), got.Error)
		f.assertDelivered(t, 1)
	})
}

// TestScheduledFire_UsesRecordedTokenBoundary requires a token-authored
// event to fire under the boundary recorded when the token authored it, for
// a project token and a hub token alike: the fire resolves a token identity
// with that boundary, a dispatch_agent child's delegation edge carries the
// recorded boundary, and a message is delivered under it. A revision that
// records a project token for another project fires under that token's
// boundary, which does not contain the event's project: the fire fails with
// no child created and nothing delivered.
func TestScheduledFire_UsesRecordedTokenBoundary(t *testing.T) {
	t.Run("dispatch_agent", func(t *testing.T) {
		f := newSchedFire(t, "fire-tok-bnd")
		withFireScheduler(f.srv, f.store)
		selectors := append(minimalSelectors(t), "scheduled_event:create")

		for _, boundary := range []TokenBoundary{projectBoundary(f.proj.ID), hubBoundary()} {
			slug := "fire-tok-bnd-" + string(boundary.Kind)
			key, tok := schedFireMint(t, f, boundary, selectors...)
			evt := createTokenEvent(t, f.srv, f.store, key, f.proj.ID,
				CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: slug})
			assertTokenRevision(t, slug, tok, evt.InitiatorAttribution, evt.AuthorityCeiling)
			assert.Equal(t, string(boundary.Kind), evt.AuthorityCeiling.BoundaryKind, slug)
			assert.Equal(t, boundary.ProjectID, evt.AuthorityCeiling.BoundaryProjectID, slug)

			_, identity, err := f.srv.resolveScheduledAuthority(context.Background(), evt)
			require.NoError(t, err, slug)
			scoped, ok := identity.(*ScopedUserIdentity)
			require.True(t, ok, "%s: the fire runs as a token identity, got %T", slug, identity)
			assert.Equal(t, boundary, scoped.Boundary(), slug)

			got := fireStoredEvent(t, f.srv, f.store, evt)
			require.Equal(t, store.ScheduledEventFired, got.Status, "%s: %s", slug, got.Error)
			_, edge := f.child(t, slug)
			assert.Equal(t, evt.AuthorityCeiling.BoundaryKind, edge.BoundaryKind, slug)
			assert.Equal(t, evt.AuthorityCeiling.BoundaryProjectID, edge.BoundaryProjectID, slug)
		}
	})

	t.Run("message", func(t *testing.T) {
		f := newSchedMsg(t, "fire-tok-msg-bnd")
		withFireScheduler(f.srv, f.store)
		body := CreateScheduledEventRequest{EventType: "message", FireIn: "1h", AgentID: f.target.ID, Message: "scheduled hello"}

		for i, boundary := range []TokenBoundary{projectBoundary(f.proj.ID), hubBoundary()} {
			label := string(boundary.Kind) + " token"
			key, tok := schedFireMint(t, f.schedFire, boundary, "scheduled_event:create", "agent:read", "agent:message")
			evt := createTokenEvent(t, f.srv, f.store, key, f.proj.ID, body)
			assertTokenRevision(t, label, tok, evt.InitiatorAttribution, evt.AuthorityCeiling)

			_, identity, err := f.srv.resolveScheduledAuthority(context.Background(), evt)
			require.NoError(t, err, label)
			scoped, ok := identity.(*ScopedUserIdentity)
			require.True(t, ok, "%s: the fire runs as a token identity, got %T", label, identity)
			assert.Equal(t, boundary, scoped.Boundary(), label)

			got := fireStoredEvent(t, f.srv, f.store, evt)
			require.Equal(t, store.ScheduledEventFired, got.Status, "%s: %s", label, got.Error)
			f.assertDelivered(t, i+1)
		}
	})

	t.Run("boundary without the event's project", func(t *testing.T) {
		f := newSchedMsg(t, "fire-tok-bnd-out")
		withFireScheduler(f.srv, f.store)
		ctx := context.Background()
		other := tid("fire-tok-bnd-out-p")
		require.NoError(t, f.store.CreateProject(ctx, &store.Project{ID: other, Name: "fire-tok-bnd-out-p", Slug: other}))
		require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, other, f.creator.ID))

		// rerecord returns evt with its revision recording elsewhere: the
		// credential and the ceiling's boundary.
		rerecord := func(evt store.ScheduledEvent, elsewhere *store.UserAccessToken) store.ScheduledEvent {
			evt.InitiatorCredentialID = elsewhere.ID
			evt.AuthorityCeiling.PermissionIDs = append([]string(nil), elsewhere.CeilingPermissionIDs...)
			evt.AuthorityCeiling.BoundaryKind = elsewhere.BoundaryKind
			evt.AuthorityCeiling.BoundaryProjectID = elsewhere.ProjectID
			return evt
		}
		assertOutsideBoundary := func(label string, evt store.ScheduledEvent, wantErr string) {
			t.Helper()
			require.Equal(t, other, evt.AuthorityCeiling.BoundaryProjectID, label)
			_, identity, err := f.srv.resolveScheduledAuthority(ctx, evt)
			require.NoError(t, err, label)
			scoped, ok := identity.(*ScopedUserIdentity)
			require.True(t, ok, "%s: the fire runs as a token identity, got %T", label, identity)
			assert.Equal(t, projectBoundary(other), scoped.Boundary(), label)
			got := fireStoredEvent(t, f.srv, f.store, evt)
			assert.Equal(t, store.ScheduledEventFailed, got.Status, label)
			assert.Contains(t, got.Error, wantErr, label)
		}

		dispatch := append(minimalSelectors(t), "scheduled_event:create")
		key, _ := schedFireMint(t, f.schedFire, projectBoundary(f.proj.ID), dispatch...)
		_, elsewhere := schedFireMint(t, f.schedFire, projectBoundary(other), dispatch...)
		evt := createTokenEvent(t, f.srv, f.store, key, f.proj.ID,
			CreateScheduledEventRequest{EventType: "dispatch_agent", FireIn: "1h", AgentName: "fire-tok-bnd-out-c"})
		assertOutsideBoundary("dispatch_agent event", rerecord(evt, elsewhere), "not authorized to create agents in project")
		f.assertNoChild(t, "fire-tok-bnd-out-c")

		message := []string{"scheduled_event:create", "agent:read", "agent:message"}
		key, _ = schedFireMint(t, f.schedFire, projectBoundary(f.proj.ID), message...)
		_, elsewhere = schedFireMint(t, f.schedFire, projectBoundary(other), message...)
		evt = createTokenEvent(t, f.srv, f.store, key, f.proj.ID,
			CreateScheduledEventRequest{EventType: "message", FireIn: "1h", AgentID: f.target.ID, Message: "scheduled hello"})
		evt = rerecord(evt, elsewhere)
		err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "agent.message permission denied")
		assertOutsideBoundary("message event", evt, errScheduledMessageRefused.Error())
		f.assertDelivered(t, 0)
	})
}
