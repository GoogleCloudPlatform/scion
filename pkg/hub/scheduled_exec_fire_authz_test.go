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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fire-time authorization of exec events (ptone/scion#7883, AC3): every fire
// re-checks agent.attach on the target under the event's authorization
// revision; a refusal records the generic "exec refused", audits
// schedule.exec.refused with the detailed reason, and runs nothing.

// schedExec is the scheduled fire fixture with exec schedules on and a
// running target the creator owns (created with the creator's session).
type schedExec struct {
	*schedFire
	target *store.Agent
}

func newSchedExec(t *testing.T, name string) *schedExec {
	t.Helper()
	f := newSchedFire(t, name)
	enableExecSchedules(t, f.srv)
	// Mint agent scopes from the agent's role, as production does (the
	// test server's dev auth otherwise widens every role to full).
	f.srv.authzService.mintDevAuthOverride = false
	target := f.sessionAgent(t, name+"-target")
	f.setRunning(t, target)
	return &schedExec{schedFire: f, target: target}
}

// setRunning marks agent running in the store.
func (f *schedFire) setRunning(t *testing.T, agent *store.Agent) {
	t.Helper()
	ctx := context.Background()
	a, err := f.store.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	a.Phase = "running"
	require.NoError(t, f.store.UpdateAgent(ctx, a))
	*agent = *a
}

// execEvent returns a one-shot exec event on target with no revision.
func (f *schedExec) execEvent(target *store.Agent, argv ...string) store.ScheduledEvent {
	p := scheduledExecPayload{AgentID: target.ID, AgentName: target.Slug, Command: argv}
	raw, _ := p.encode()
	return store.ScheduledEvent{
		ID:        api.NewUUID(),
		ProjectID: f.proj.ID,
		EventType: scheduledEventTypeExec,
		Payload:   raw,
		Status:    store.ScheduledEventFired,
		CreatedBy: f.creator.ID,
		FireAt:    time.Now(),
	}
}

// authorize runs the fire-time decision for evt.
func (f *schedExec) authorize(t *testing.T, evt store.ScheduledEvent) error {
	t.Helper()
	p, err := decodeScheduledExecPayload(evt.Payload)
	require.NoError(t, err)
	_, _, _, err = f.srv.authorizeScheduledExecFire(context.Background(), evt, p)
	return err
}

// assertRefused fires evt and asserts the generic refusal, the refused
// audit record with a detailed reason, and that no run was admitted.
func (f *schedExec) assertRefused(t *testing.T, evt store.ScheduledEvent) {
	t.Helper()
	logs := captureDefaultLog(t)
	err := f.srv.fireScheduledExec(context.Background(), evt)
	require.Error(t, err)
	assert.Equal(t, execReasonRefused, err.Error())
	_, ok := asScheduledSkip(err)
	assert.False(t, ok, "a refusal is a failure, not a skip")
	_, gerr := f.store.GetScheduledExecRun(context.Background(), evt.ID)
	assert.ErrorIs(t, gerr, store.ErrNotFound, "nothing admitted")
	out := logs.String()
	assert.Contains(t, out, "audit_action=schedule.exec.refused")
	assert.Contains(t, out, "scheduled_event_id="+evt.ID)
}

func TestScheduledExecFire_AuthzMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("owner session revision is authorized", func(t *testing.T) {
		f := newSchedExec(t, "sexec-owner")
		evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
		require.NoError(t, f.authorize(t, evt))
		// Past authorization the fire reaches exec-start; with no conduit
		// router here the hub gate stops it (skipped, never failed).
		err := f.srv.fireScheduledExec(ctx, evt)
		reason, ok := asScheduledSkip(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, execReasonDisabled, reason)
		run, gerr := f.store.GetScheduledExecRun(ctx, evt.ID)
		require.NoError(t, gerr)
		assert.Equal(t, store.ScheduledExecOutcomeSkipped, run.Outcome)
		assert.Equal(t, f.creator.ID, run.InitiatorPrincipalID)
		assert.Equal(t, f.target.RunID, run.LaunchID)
	})

	t.Run("project-admin without a relationship is refused", func(t *testing.T) {
		f := newSchedExec(t, "sexec-padmin")
		admin := hubMemberUser(t, f.store, "sexec-padmin-admin")
		grantFixtureRole(t, f.bypassAgentsFixture, admin.ID, store.ProjectRoleAdmin)
		evt := withSessionRevision(f.execEvent(f.target, "true"), admin.ID)
		require.ErrorIs(t, f.authorize(t, evt), errScheduledAuthorityDenied)
		f.assertRefused(t, evt)
	})

	t.Run("hub-admin without a relationship is refused", func(t *testing.T) {
		f := newSchedExec(t, "sexec-hadmin")
		admin := hubMemberUser(t, f.store, "sexec-hadmin-admin")
		u, err := f.store.GetUser(ctx, admin.ID)
		require.NoError(t, err)
		u.Role = store.UserRoleAdmin
		require.NoError(t, f.store.UpdateUser(ctx, u))
		grantFixtureRole(t, f.bypassAgentsFixture, admin.ID, store.ProjectRoleMember)
		evt := withSessionRevision(f.execEvent(f.target, "true"), admin.ID)
		require.Error(t, f.authorize(t, evt))
		f.assertRefused(t, evt)
	})

	t.Run("uat revision", func(t *testing.T) {
		attach := uatSelectorsFor(t, scheduledExecPermission)
		t.Run("with agent:attach is authorized", func(t *testing.T) {
			f := newSchedExec(t, "sexec-uat-ok")
			tok := f.storedUAT(t, append(attach, readonlyRoleUATSelectors(t)...)...)
			require.NoError(t, f.authorize(t, withUATRevision(f.execEvent(f.target, "true"), tok)))
		})
		t.Run("without agent:attach is refused", func(t *testing.T) {
			f := newSchedExec(t, "sexec-uat-noattach")
			tok := f.storedUAT(t, append(uatSelectorsFor(t, "agent.lifecycle"), readonlyRoleUATSelectors(t)...)...)
			evt := withUATRevision(f.execEvent(f.target, "true"), tok)
			err := f.authorize(t, evt)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "revision ceiling does not allow agent.attach")
			f.assertRefused(t, evt)
		})
		t.Run("revoked after scheduling is refused", func(t *testing.T) {
			f := newSchedExec(t, "sexec-uat-rev")
			tok := f.storedUAT(t, append(attach, readonlyRoleUATSelectors(t)...)...)
			evt := withUATRevision(f.execEvent(f.target, "true"), tok)
			require.NoError(t, f.authorize(t, evt))
			require.NoError(t, f.store.RevokeUserAccessToken(ctx, tok.ID))
			err := f.authorize(t, evt)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "access token revoked")
			f.assertRefused(t, evt)
		})
	})

	t.Run("agent principal revision", func(t *testing.T) {
		t.Run("full-role self is authorized", func(t *testing.T) {
			f := newSchedExec(t, "sexec-self")
			require.NoError(t, f.authorize(t, withAgentRevision(t, f.srv, f.execEvent(f.target, "true"), f.target.ID)))
		})
		t.Run("full-role same-root sibling is authorized", func(t *testing.T) {
			f := newSchedExec(t, "sexec-sib")
			sender := f.sessionAgent(t, "sexec-sib-sender")
			require.NoError(t, f.authorize(t, withAgentRevision(t, f.srv, f.execEvent(f.target, "true"), sender.ID)))
		})
		t.Run("another user's agent is refused", func(t *testing.T) {
			f := newSchedExec(t, "sexec-foreign")
			sender := f.sessionAgent(t, "sexec-foreign-sender")
			foreign := foreignOwnedAgent(t, f.bypassAgentsFixture, "sexec-foreign-target")
			f.setRunning(t, foreign)
			evt := withAgentRevision(t, f.srv, f.execEvent(foreign, "true"), sender.ID)
			require.Error(t, f.authorize(t, evt))
			f.assertRefused(t, evt)
		})
		t.Run("baseline role is refused", func(t *testing.T) {
			f := newSchedExec(t, "sexec-baseline")
			a, _ := f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: "sexec-baseline-sender", AgentRole: string(AgentRoleBaseline)}), "sexec-baseline-sender")
			evt := withAgentRevision(t, f.srv, f.execEvent(f.target, "true"), a.ID)
			require.Error(t, f.authorize(t, evt))
			f.assertRefused(t, evt)
		})
		t.Run("role downgraded after scheduling is refused", func(t *testing.T) {
			f := newSchedExec(t, "sexec-downgrade")
			sender := f.sessionAgent(t, "sexec-downgrade-sender")
			evt := withAgentRevision(t, f.srv, f.execEvent(f.target, "true"), sender.ID)
			require.NoError(t, f.authorize(t, evt))
			a, err := f.store.GetAgent(ctx, sender.ID)
			require.NoError(t, err)
			require.NotNil(t, a.AppliedConfig)
			a.AppliedConfig.AgentRole = string(AgentRoleBaseline)
			require.NoError(t, f.store.UpdateAgent(ctx, a))
			require.Error(t, f.authorize(t, evt))
			f.assertRefused(t, evt)
		})
	})

	t.Run("revocation after scheduling", func(t *testing.T) {
		t.Run("relationship removed", func(t *testing.T) {
			// The owner relationship needs active project access: removing
			// the creator's project membership ends it.
			f := newSchedExec(t, "sexec-rel")
			evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
			require.NoError(t, f.authorize(t, evt))
			bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.creator.ID)
			require.NoError(t, err)
			require.NotEmpty(t, bindings)
			for _, b := range bindings {
				require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
			}
			require.ErrorIs(t, f.authorize(t, evt), errScheduledAuthorityDenied)
			f.assertRefused(t, evt)
		})
		t.Run("user suspended", func(t *testing.T) {
			f := newSchedExec(t, "sexec-sus")
			evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
			u, err := f.store.GetUser(ctx, f.creator.ID)
			require.NoError(t, err)
			u.Status = store.UserStatusSuspended
			require.NoError(t, f.store.UpdateUser(ctx, u))
			err = f.authorize(t, evt)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), reasonPrincipalInactive)
			f.assertRefused(t, evt)
		})
		t.Run("target held", func(t *testing.T) {
			f := newSchedExec(t, "sexec-held")
			evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
			_, err := f.store.CreateAgentHolds(ctx, []*store.AgentHold{{
				AgentID: f.target.ID, ProjectID: f.proj.ID, Cause: store.AgentHoldCauseOwnerAccessEnded,
				RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: f.creator.ID,
				Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "test",
			}})
			require.NoError(t, err)
			err = f.authorize(t, evt)
			require.ErrorIs(t, err, errScheduledAuthorityDenied)
			assert.Contains(t, err.Error(), "standing")
			f.assertRefused(t, evt)
		})
		t.Run("legacy attribution", func(t *testing.T) {
			f := newSchedExec(t, "sexec-legacy")
			f.assertRefused(t, f.execEvent(f.target, "true"))
		})
	})

	t.Run("target resolved by stored agentId only", func(t *testing.T) {
		f := newSchedExec(t, "sexec-byid")
		evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
		// A payload naming a missing agent id never falls back to the name.
		p := scheduledExecPayload{AgentID: api.NewUUID(), AgentName: f.target.Slug, Command: []string{"true"}}
		evt.Payload, _ = p.encode()
		require.ErrorIs(t, f.authorize(t, evt), errScheduledExecTargetGone)
		f.assertRefused(t, evt)
	})
}

// uatSelectorsFor returns the UAT scope selectors that carry permissionID.
func uatSelectorsFor(t *testing.T, permissionID string) []string {
	t.Helper()
	p, ok := registryPermission(permissionID)
	require.True(t, ok, permissionID)
	require.NotEmpty(t, p.UATScope, permissionID)
	return []string{p.UATScope}
}

// TestScheduledExecFire_Preflight: a fire that authorization admits still
// runs nothing for a target that is not running (D4): skipped, not failed,
// and no run is admitted.
func TestScheduledExecFire_Preflight(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(a *store.Agent)
	}{
		{"stopped", func(a *store.Agent) { a.Phase = "stopped" }},
		{"suspended", func(a *store.Agent) { a.Phase = "suspended" }},
		{"starting", func(a *store.Agent) { a.Phase = "starting" }},
		{"managed runtime", func(a *store.Agent) { a.Runtime = "managed:gemini" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedExec(t, "sexec-pre")
			a, err := f.store.GetAgent(ctx, f.target.ID)
			require.NoError(t, err)
			tc.mutate(a)
			require.NoError(t, f.store.UpdateAgent(ctx, a))
			evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
			err = f.srv.fireScheduledExec(ctx, evt)
			reason, ok := asScheduledSkip(err)
			require.True(t, ok, "%v", err)
			assert.Equal(t, execReasonTargetNotRunning, reason)
			_, gerr := f.store.GetScheduledExecRun(ctx, evt.ID)
			assert.ErrorIs(t, gerr, store.ErrNotFound, "no run admitted")
		})
	}

	t.Run("flag off skips before authorization", func(t *testing.T) {
		for _, flags := range []map[string]bool{
			{conduitExperiment: true, "hub.exec_schedules": false},
			{conduitExperiment: false, "hub.exec_schedules": true},
		} {
			f := newSchedExec(t, "sexec-flag")
			setHubExperiments(t, f.srv, flags)
			evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
			reason, ok := asScheduledSkip(f.srv.fireScheduledExec(ctx, evt))
			require.True(t, ok)
			assert.Equal(t, execReasonDisabled, reason)
		}
	})

	t.Run("missed window", func(t *testing.T) {
		f := newSchedExec(t, "sexec-late")
		evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
		evt.FireAt = time.Now().Add(-scheduledExecMaxWindow - time.Minute)
		reason, ok := asScheduledSkip(f.srv.fireScheduledExec(ctx, evt))
		require.True(t, ok)
		assert.Equal(t, execReasonMissedWindow, reason)
	})

	t.Run("invalid stored payload fails closed", func(t *testing.T) {
		f := newSchedExec(t, "sexec-badpayload")
		for _, raw := range []string{
			`{"agentId":"` + f.target.ID + `","command":[]}`,
			`{"agentId":"` + f.target.ID + `","command":["a\u0000b"]}`,
			`{"agentId":"` + f.target.ID + `","command":["true"],"timeoutSeconds":3601}`,
			`{"agentId":"` + f.target.ID + `","command":["true"],"wake":true}`,
			`{"agentId":"` + f.target.ID + `","command":["true"],"extra":1}`,
			`{"command":["true"]}`,
			`not json`,
		} {
			evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
			evt.Payload = raw
			err := f.srv.fireScheduledExec(ctx, evt)
			require.ErrorIs(t, err, errScheduledExecPayloadInvalid, raw)
		}
	})

	t.Run("capacity", func(t *testing.T) {
		f := newSchedExec(t, "sexec-cap")
		prev := scheduledExecMaxTracked
		scheduledExecMaxTracked = 0
		t.Cleanup(func() { scheduledExecMaxTracked = prev })
		evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
		reason, ok := asScheduledSkip(f.srv.fireScheduledExec(ctx, evt))
		require.True(t, ok)
		assert.Equal(t, execReasonCapacity, reason)
	})

	t.Run("recurring exec never runs in phase 1", func(t *testing.T) {
		f := newSchedExec(t, "sexec-recurring")
		evt := withSessionRevision(f.execEvent(f.target, "true"), f.creator.ID)
		evt.ScheduleID = api.NewUUID()
		err := f.srv.fireScheduledExec(ctx, evt)
		require.Error(t, err)
		assert.Equal(t, execReasonRecurring, err.Error())
	})
}
