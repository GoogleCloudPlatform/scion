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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Scheduled message fires run under the event's authorization revision
// (resolveScheduledAuthority), never under CreatedBy (ptone/scion#2341).

// schedMsg is a scheduled message fixture: the scheduled dispatch fixture,
// with the creator's member role replaced by project-admin (which carries
// agent.message),
// plus a project-mode target agent in the fixture project and a recording
// dispatcher.
type schedMsg struct {
	*schedFire
	target *store.Agent
	spy    *containmentDispatchSpy
}

func newSchedMsg(t *testing.T, name string) *schedMsg {
	t.Helper()
	f := newSchedFire(t, name)
	bindings, err := f.store.ListRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, f.creator.ID)
	require.NoError(t, err)
	for _, b := range bindings {
		require.NoError(t, f.store.DeleteRoleBinding(context.Background(), b.ID))
	}
	grantFixtureRole(t, f.bypassAgentsFixture, f.creator.ID, store.ProjectRoleAdmin)
	target := &store.Agent{
		ID: api.NewUUID(), Name: name + "-target", Slug: name + "-target",
		ProjectID: f.proj.ID, MessageMode: store.MessageModeProject,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), target))
	spy := &containmentDispatchSpy{}
	f.srv.SetDispatcher(spy)
	return &schedMsg{schedFire: f, target: target, spy: spy}
}

// msgEvent returns a message event to the target whose history creator is
// the fixture creator and which carries no authorization revision.
func (f *schedMsg) msgEvent() store.ScheduledEvent {
	payload, _ := json.Marshal(MessageEventPayload{AgentID: f.target.ID, Message: "scheduled hello"})
	return store.ScheduledEvent{
		ID:         api.NewUUID(),
		ProjectID:  f.proj.ID,
		EventType:  "message",
		Payload:    string(payload),
		CreatedBy:  f.creator.ID,
		ScheduleID: "sched-msg",
		FireAt:     time.Now(),
	}
}

func (f *schedMsg) fireMsg(t *testing.T, evt store.ScheduledEvent) error {
	t.Helper()
	return f.srv.messageEventHandler()(context.Background(), evt)
}

func (f *schedMsg) assertDelivered(t *testing.T, n int) {
	t.Helper()
	assert.Len(t, f.spy.getCalls(), n, "scheduled message deliveries")
}

// assertRefused asserts the handler refused with the constant public
// refusal and nothing was delivered.
func (f *schedMsg) assertRefused(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, errScheduledMessageRefused.Error(), err.Error())
	f.assertDelivered(t, 0)
}

// messageSelectors returns UAT selectors that carry agent.message.
func messageSelectors(t *testing.T) []string {
	t.Helper()
	p, ok := registryPermission(scheduledMessagePermission)
	require.True(t, ok)
	require.NotEmpty(t, p.UATScope)
	return []string{p.UATScope}
}

// scheduledMessageLogCapture records the default logger's output for the
// test and restores the previous default logger afterwards.
type scheduledMessageLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *scheduledMessageLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *scheduledMessageLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureDefaultLog(t *testing.T) *scheduledMessageLogCapture {
	t.Helper()
	c := &scheduledMessageLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return c
}

// userLookupFaultStore fails every user lookup with a non-NotFound error.
type userLookupFaultStore struct {
	store.Store
}

var errUserLookupFault = errors.New("user lookup unavailable")

func (s *userLookupFaultStore) GetUser(context.Context, string) (*store.User, error) {
	return nil, errUserLookupFault
}

func TestScheduledMessageAuthority(t *testing.T) {
	ctx := context.Background()

	t.Run("R1 session revision with agent.message access fires", func(t *testing.T) {
		f := newSchedMsg(t, "smsg-r1")
		require.NoError(t, f.fireMsg(t, withSessionRevision(f.msgEvent(), f.creator.ID)))
		f.assertDelivered(t, 1)
	})

	t.Run("R2 legacy attribution is denied with the remedy text", func(t *testing.T) {
		f := newSchedMsg(t, "smsg-r2")
		evt := f.msgEvent() // CreatedBy names an admitted member; no revision
		err := f.fireMsg(t, evt)
		require.Error(t, err)
		assert.Equal(t, errScheduledAuthorityUnrecorded.Error(), err.Error())
		f.assertDelivered(t, 0)

		t.Run("legacy_unknown credential kind", func(t *testing.T) {
			evt := withSessionRevision(f.msgEvent(), f.creator.ID)
			evt.InitiatorCredentialKind = store.InitiatorCredentialKindLegacyUnknown
			err := f.fireMsg(t, evt)
			require.Error(t, err)
			assert.Equal(t, errScheduledAuthorityUnrecorded.Error(), err.Error())
			f.assertDelivered(t, 0)
		})
		t.Run("missing target records the same text", func(t *testing.T) {
			// The outcome is decided before the target is looked up.
			evt := f.msgEvent()
			evt.Payload = `{"agentId":"` + api.NewUUID() + `","message":"x"}`
			err := f.fireMsg(t, evt)
			require.Error(t, err)
			assert.Equal(t, errScheduledAuthorityUnrecorded.Error(), err.Error())
			f.assertDelivered(t, 0)
		})
	})

	t.Run("R3 recorded revision with unrecorded ceiling is denied", func(t *testing.T) {
		f := newSchedMsg(t, "smsg-r3")
		evt := withSessionRevision(f.msgEvent(), f.creator.ID)
		evt.AuthorityCeiling = store.EffectCeiling{}
		err := f.fireMsg(t, evt)
		require.Error(t, err)
		assert.Equal(t, errScheduledAuthorityUnrecorded.Error(), err.Error())
		f.assertDelivered(t, 0)
	})

	t.Run("R4 uat revision", func(t *testing.T) {
		t.Run("live token fires", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r4-live")
			tok := f.storedUAT(t, messageSelectors(t)...)
			require.NoError(t, f.fireMsg(t, withUATRevision(f.msgEvent(), tok)))
			f.assertDelivered(t, 1)
		})
		t.Run("revoked token denies", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r4-rev")
			tok := f.storedUAT(t, messageSelectors(t)...)
			require.NoError(t, f.store.RevokeUserAccessToken(ctx, tok.ID))
			evt := withUATRevision(f.msgEvent(), tok)
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "access token revoked")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
		t.Run("expired token denies", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r4-exp")
			tok := f.storedUATExpiring(t, time.Now().Add(-time.Minute), messageSelectors(t)...)
			evt := withUATRevision(f.msgEvent(), tok)
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "access token expired")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
		t.Run("token owned by another user denies", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r4-own")
			other := hubMemberUser(t, f.store, "smsg-r4-own-other")
			grantFixtureRole(t, f.bypassAgentsFixture, other.ID, store.ProjectRoleMember)
			tok := f.storedUATOwnedBy(t, other.ID, time.Now().Add(24*time.Hour), messageSelectors(t)...)
			evt := withUATRevision(f.msgEvent(), tok)
			evt.InitiatorPrincipalID = f.creator.ID
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "owner does not match")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
		t.Run("token without agent.message denies", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r4-scope")
			tok := f.storedUAT(t, readonlyRoleUATSelectors(t)...)
			evt := withUATRevision(f.msgEvent(), tok)
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "revision ceiling does not allow agent.message")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
		t.Run("revision ceiling without agent.message denies a token that carries it", func(t *testing.T) {
			// The frozen revision ceiling bounds the send even when the live
			// token would allow it.
			f := newSchedMsg(t, "smsg-r4-frozen")
			tok := f.storedUAT(t, append(messageSelectors(t), readonlyRoleUATSelectors(t)...)...)
			evt := withUATRevision(f.msgEvent(), tok)
			narrow := uatCeilingFromSelectors(t, readonlyRoleUATSelectors(t)...)
			evt.AuthorityCeiling.PermissionIDs = narrow.PermissionIDs
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "revision ceiling does not allow agent.message")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
	})

	t.Run("R5 principal lost project access after scheduling is denied", func(t *testing.T) {
		f := newSchedMsg(t, "smsg-r5")
		evt := withSessionRevision(f.msgEvent(), f.creator.ID)
		bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.creator.ID)
		require.NoError(t, err)
		require.NotEmpty(t, bindings)
		for _, b := range bindings {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
		}
		err = authorizeScheduledMessageFireFor(f.srv, evt, f.target)
		require.ErrorIs(t, err, errScheduledAuthorityDenied)
		assert.Contains(t, err.Error(), "lacks admission to the project")
		f.assertRefused(t, f.fireMsg(t, evt))
	})

	t.Run("R6 suspended or deleted principal is denied", func(t *testing.T) {
		t.Run("suspended", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r6-sus")
			u, err := f.store.GetUser(ctx, f.creator.ID)
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(ctx, u))
			evt := withSessionRevision(f.msgEvent(), f.creator.ID)
			err = authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), reasonPrincipalInactive)
			f.assertRefused(t, f.fireMsg(t, evt))
		})
		t.Run("deleted", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r6-del")
			gone := hubMemberUser(t, f.store, "smsg-r6-del-gone")
			grantFixtureRole(t, f.bypassAgentsFixture, gone.ID, store.ProjectRoleMember)
			evt := withSessionRevision(f.msgEvent(), gone.ID)
			require.NoError(t, f.store.DeleteUser(ctx, gone.ID))
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), reasonPrincipalInactive)
			f.assertRefused(t, f.fireMsg(t, evt))
		})
	})

	t.Run("R7 resume re-records the revision and the schedule fires", func(t *testing.T) {
		f := newSchedMsg(t, "smsg-r7")
		ownerUser := hubMemberUser(t, f.store, "smsg-r7-owner")
		grantFixtureRole(t, f.bypassAgentsFixture, ownerUser.ID, store.ProjectRoleOwner)
		owner := authUser(ownerUser)
		f.srv.scheduler = NewScheduler(f.store, slog.Default())
		f.srv.scheduler.RegisterEventHandler("message", f.srv.messageEventHandler())

		// A legacy message schedule: CreatedBy only, no recorded revision.
		legacy := f.msgEvent()
		sched := &store.Schedule{
			ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "smsg-r7", CronExpr: "0 * * * *",
			EventType: "message", Payload: legacy.Payload,
			Status: store.ScheduleStatusActive, CreatedBy: f.creator.ID,
		}
		require.NoError(t, f.store.CreateSchedule(ctx, sched))

		seen := map[string]bool{}
		fireOnce := func(t *testing.T) store.ScheduledEvent {
			t.Helper()
			sc, err := f.store.GetSchedule(ctx, sched.ID)
			require.NoError(t, err)
			f.srv.executeSchedule(ctx, *sc, time.Now())
			res, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{ScheduleID: sched.ID}, store.ListOptions{})
			require.NoError(t, err)
			var fresh []store.ScheduledEvent
			for _, e := range res.Items {
				if !seen[e.ID] {
					seen[e.ID] = true
					fresh = append(fresh, e)
				}
			}
			require.Len(t, fresh, 1)
			return fresh[0]
		}

		failed := fireOnce(t)
		assert.Equal(t, store.ScheduledEventFailed, failed.Status)
		assert.Equal(t, errScheduledAuthorityUnrecorded.Error(), failed.Error)
		f.assertDelivered(t, 0)

		rec := doAuthoredScheduleRequest(t, f.srv, owner, f.proj.ID, sched.ID+"/pause", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		rec = doAuthoredScheduleRequest(t, f.srv, owner, f.proj.ID, sched.ID+"/resume", http.MethodPost, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		resumed, err := f.store.GetSchedule(ctx, sched.ID)
		require.NoError(t, err)
		assert.Equal(t, ownerUser.ID, resumed.InitiatorPrincipalID)
		assert.Equal(t, store.InitiatorCredentialKindSession, resumed.InitiatorCredentialKind)
		assert.Equal(t, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, resumed.AuthorityCeiling)
		assert.Greater(t, resumed.AuthorizationRevision, sched.AuthorizationRevision)

		fired := fireOnce(t)
		assert.Equal(t, store.ScheduledEventFired, fired.Status, fired.Error)
		assert.Empty(t, fired.Error)
		f.assertDelivered(t, 1)
	})

	t.Run("R8 a CreatedBy other than the revision principal grants nothing", func(t *testing.T) {
		t.Run("admitted CreatedBy, outsider principal", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r8-a")
			outsider := hubMemberUser(t, f.store, "smsg-r8-a-out")
			evt := withSessionRevision(f.msgEvent(), outsider.ID)
			require.Equal(t, f.creator.ID, evt.CreatedBy)
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "lacks admission to the project")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
		t.Run("outsider CreatedBy, admitted principal", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r8-b")
			outsider := hubMemberUser(t, f.store, "smsg-r8-b-out")
			evt := withSessionRevision(f.msgEvent(), f.creator.ID)
			evt.CreatedBy = outsider.ID
			require.NoError(t, f.fireMsg(t, evt))
			f.assertDelivered(t, 1)
		})
	})

	t.Run("R9 agent principal revision", func(t *testing.T) {
		t.Run("fires inside its project", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r9-in")
			sender := f.sessionAgent(t, "smsg-r9-in-sender")
			require.NoError(t, f.fireMsg(t, withAgentRevision(t, f.srv, f.msgEvent(), sender.ID)))
			f.assertDelivered(t, 1)
		})
		t.Run("branch-mode target without a relationship is denied by the agent message rule", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r9-br")
			sender := f.sessionAgent(t, "smsg-r9-br-sender")
			f.target.MessageMode = store.MessageModeBranch
			require.NoError(t, f.store.UpdateAgent(ctx, f.target))
			evt := withAgentRevision(t, f.srv, f.msgEvent(), sender.ID)
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "scheduled_message_denied")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
		t.Run("denied cross-project", func(t *testing.T) {
			f := newSchedMsg(t, "smsg-r9-x")
			require.NotEqual(t, f.proj.ID, f.stranger.ProjectID)
			evt := withMockAgentRevision(f.msgEvent(), f.stranger.ID)
			err := authorizeScheduledMessageFireFor(f.srv, evt, f.target)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "not in the event's project")
			f.assertRefused(t, f.fireMsg(t, evt))
		})
	})

	t.Run("R10 lookup fault is denied with the constant refusal and logged", func(t *testing.T) {
		f := newSchedMsg(t, "smsg-r10")
		evt := withSessionRevision(f.msgEvent(), f.creator.ID)
		f.srv.store = &userLookupFaultStore{Store: f.store}
		logs := captureDefaultLog(t)

		err := f.fireMsg(t, evt)
		f.assertRefused(t, err)
		assert.False(t, errors.Is(err, errScheduledAuthorityUnrecorded))
		out := logs.String()
		assert.Contains(t, out, "scheduled message authority denied at fire time")
		assert.Contains(t, out, errUserLookupFault.Error())
		assert.Contains(t, out, evt.ID)
	})

	t.Run("R11 federated-user revision is denied as unrecorded", func(t *testing.T) {
		f := newSchedMsg(t, "smsg-r11")
		fedUser := hubMemberUser(t, f.store, "smsg-r11-fed")
		grantFixtureRole(t, f.bypassAgentsFixture, fedUser.ID, store.ProjectRoleOwner)
		fed := &federatedTestIdentity{id: fedUser.ID, email: fedUser.Email, displayName: fedUser.DisplayName, role: "member"}
		f.srv.scheduler = NewScheduler(f.store, slog.Default())

		// Author through the handler, as a federated user's request records it.
		rec := doAuthoredEventRequest(t, f.srv, fed, f.proj.ID,
			CreateScheduledEventRequest{EventType: "message", FireIn: "30m", AgentID: f.target.ID, Message: "hi"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var created store.ScheduledEvent
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
		stored, err := f.store.GetScheduledEvent(ctx, created.ID)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeiling{}, stored.AuthorityCeiling, "a federated revision records the unrecorded ceiling")

		err = f.fireMsg(t, *stored)
		require.Error(t, err)
		assert.Equal(t, errScheduledAuthorityUnrecorded.Error(), err.Error())
		f.assertDelivered(t, 0)

		// The same principal with a recorded ceiling is still not a user
		// principal the resolver admits.
		evt := *stored
		evt.AuthorityCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
		evt.InitiatorPrincipalKind = string(PrincipalKindFederatedUser)
		evt.InitiatorCredentialKind = store.InitiatorCredentialKindSession
		err = authorizeScheduledMessageFireFor(f.srv, evt, f.target)
		require.Error(t, err)
		assert.True(t, errors.Is(err, errScheduledAuthorityDenied) || errors.Is(err, errScheduledAuthorityUnrecorded), err.Error())
		f.assertRefused(t, f.fireMsg(t, evt))
	})
}

// A scoped access token is not refused by the scheduled-message authoring
// function: its revision records the token's ceiling, and each fire
// re-checks the token (TestScheduledMessageAuthority, R4).
func TestScheduledMessageAuthoringAdmitsScopedToken(t *testing.T) {
	f := newSchedMsg(t, "smsg-author-uat")
	tok := f.storedUAT(t, messageSelectors(t)...)
	scoped := NewScopedUserIdentityWithBoundary(authUser(f.creator),
		TokenBoundary{Kind: BoundaryKind(tok.BoundaryKind), ProjectID: tok.ProjectID},
		tok.Scopes, tok.ID, tok.NormalizedCeiling())
	req := authoredRequest(t, scoped, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/scheduled-events", nil)
	w := httptest.NewRecorder()
	ok := f.srv.authorizeScheduledMessageAuthoring(w, req, f.proj.ID, `{"agentId":"`+f.target.ID+`","message":"x"}`, "", "")
	assert.True(t, ok, w.Body.String())
	assert.False(t, strings.Contains(w.Body.String(), "scoped access tokens"), w.Body.String())

	ceiling, ok, rec := fireRevisionCeiling(f.srv, scoped, f.proj.ID, "message")
	require.True(t, ok, rec.Body.String())
	assert.Equal(t, store.EffectCeilingBounded, ceiling.Kind)
	assert.Contains(t, ceiling.PermissionIDs, scheduledMessagePermission)
}
