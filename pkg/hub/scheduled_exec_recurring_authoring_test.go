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
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execRecurringSchedulesPath(f *bypassAgentsFixture) string {
	return "/api/v1/projects/" + f.proj.ID + "/schedules"
}

func execRecurringCreateBody(name, agentID string, argv ...string) map[string]any {
	return map[string]any{
		"name":      name,
		"cronExpr":  "0 * * * *",
		"eventType": "exec",
		"agentId":   agentID,
		"command":   argv,
	}
}

func execRecurringStoredSchedules(t *testing.T, f *bypassAgentsFixture) []store.Schedule {
	t.Helper()
	res, err := f.store.ListSchedules(context.Background(), store.ScheduleFilter{
		ProjectID: f.proj.ID,
		EventType: scheduledEventTypeExec,
	}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	return res.Items
}

func execRecurringSeedSchedule(t *testing.T, f *bypassAgentsFixture, targetID, status string) *store.Schedule {
	t.Helper()
	p := scheduledExecPayload{AgentID: targetID, Command: []string{"true"}}
	raw, err := p.encode()
	require.NoError(t, err)
	next := time.Now().Add(time.Hour)
	sc := &store.Schedule{
		ID:        api.NewUUID(),
		ProjectID: f.proj.ID,
		Name:      "seed-exec-" + uuid.NewString()[:8],
		CronExpr:  "0 * * * *",
		EventType: scheduledEventTypeExec,
		Payload:   raw,
		Status:    status,
		NextRunAt: &next,
		CreatedBy: f.owner.ID,
		InitiatorAttribution: store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalUser,
			InitiatorPrincipalID:    f.owner.ID,
			InitiatorCredentialKind: store.InitiatorCredentialKindSession,
			AttributionVersion:      1,
			AuthorizationRevision:   1,
		},
		AuthorityCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, f.store.CreateSchedule(context.Background(), sc))
	return sc
}

// TestScheduledExecRecurringAuthoring_CreateUpdateResumeMatrix (AC2):
// create, dispatch-changing PATCH, and resume require live agent.attach on the
// target (same rule as live /exec). Denials leave stored rows, revisions, and
// attributions untouched and leak no argv-guessing oracle.
func TestScheduledExecRecurringAuthoring_CreateUpdateResumeMatrix(t *testing.T) {
	f := execAuthoringFixture(t)
	ctx := context.Background()

	projAdminID := tid("exec-rec-proj-admin")
	createTestUserWithProjectRole(t, f.store, projAdminID, "exec-rec-proj-admin@test", f.proj.ID, store.ProjectRoleAdmin)
	ensureHubMembership(ctx, f.store, projAdminID)
	projAdmin := NewAuthenticatedUser(projAdminID, "exec-rec-proj-admin@test", "PA", store.UserRoleMember, "api")

	hubAdminID := tid("exec-rec-hub-admin")
	createTestUserWithRole(t, f.store, hubAdminID, "exec-rec-hub-admin@test", store.UserRoleAdmin, store.SystemRoleHubAdmin)
	hubAdmin := NewAuthenticatedUser(hubAdminID, "exec-rec-hub-admin@test", "HA", store.UserRoleAdmin, "api")

	attachUAT := NewScopedUserIdentity(authUser(f.owner), f.proj.ID, append([]string{store.UATScopeAgentAttach, store.UATScopeAgentRead}, scheduledEventUATScopes...))
	manageUAT := NewScopedUserIdentity(authUser(f.owner), f.proj.ID, append([]string{store.UATScopeAgentManage, store.UATScopeAgentRead}, scheduledEventUATScopes...))

	// Resolve target by agentName on create + record attribution & ceiling.
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f), map[string]any{
		"name":           "owner-by-slug",
		"cronExpr":       "*/15 * * * *",
		"timeZone":       "UTC",
		"eventType":      "exec",
		"agentName":      f.sibling.Slug,
		"command":        []string{"echo", "hello"},
		"timeoutSeconds": 90,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	createdID, _ := decodeEventResponse(t, rec)["id"].(string)
	require.NotEmpty(t, createdID)
	stored, err := f.store.GetSchedule(ctx, createdID)
	require.NoError(t, err)
	decoded, err := decodeScheduledExecPayload(stored.Payload)
	require.NoError(t, err)
	assert.Equal(t, f.sibling.ID, decoded.AgentID)
	assert.Equal(t, f.sibling.Slug, decoded.AgentName)
	assert.Equal(t, 90, decoded.TimeoutSeconds)
	assert.Equal(t, 1, stored.AuthorizationRevision)
	assert.Equal(t, f.owner.ID, stored.InitiatorPrincipalID)

	userCases := []struct {
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

	for _, c := range userCases {
		// Reset the server's rate-limiter state per user case so owner,
		// attachUAT, and manageUAT (which share principal user:<owner.ID>) test
		// authorization rather than tripping the burst-10 rate limit.
		f.srv.scheduledExecAuthoringLimiter = newKeysRateLimiter(scheduledExecAuthoringRate, scheduledExecAuthoringBurst)
		t.Run("create/"+c.name, func(t *testing.T) {
			before := len(execRecurringStoredSchedules(t, f))
			rec := requestAsIdentity(t, f.srv, c.identity, http.MethodPost, execRecurringSchedulesPath(f),
				execRecurringCreateBody("create-"+uuid.NewString()[:8], f.sibling.ID, "true"))
			if c.allowed {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				assert.Len(t, execRecurringStoredSchedules(t, f), before+1)
				return
			}
			assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
			assert.Len(t, execRecurringStoredSchedules(t, f), before)
		})

		t.Run("patch-dispatch/"+c.name, func(t *testing.T) {
			sc := execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusActive)
			rec := requestAsIdentity(t, f.srv, c.identity, http.MethodPatch, execRecurringSchedulesPath(f)+"/"+sc.ID,
				map[string]any{"command": []string{"echo", "changed"}, "cronExpr": "5 * * * *"})
			after, err := f.store.GetSchedule(ctx, sc.ID)
			require.NoError(t, err)
			if c.allowed {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, 2, after.AuthorizationRevision, "dispatch edit bumps revision")
				return
			}
			assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
			assert.Equal(t, 1, after.AuthorizationRevision)
			assert.Equal(t, sc.Payload, after.Payload)

			// Anti-oracle: sending the exact stored command without attach must also be refused, never 200.
			recSame := requestAsIdentity(t, f.srv, c.identity, http.MethodPatch, execRecurringSchedulesPath(f)+"/"+sc.ID,
				map[string]any{"command": []string{"true"}})
			assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, recSame.Code, recSame.Body.String())
		})

		t.Run("resume/"+c.name, func(t *testing.T) {
			sc := execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusPaused)
			rec := requestAsIdentity(t, f.srv, c.identity, http.MethodPost, execRecurringSchedulesPath(f)+"/"+sc.ID+"/resume", nil)
			after, err := f.store.GetSchedule(ctx, sc.ID)
			require.NoError(t, err)
			if c.allowed {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, store.ScheduleStatusActive, after.Status)
				assert.Equal(t, 2, after.AuthorizationRevision)
				return
			}
			assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
			assert.Equal(t, store.ScheduleStatusPaused, after.Status)
			assert.Equal(t, 1, after.AuthorizationRevision)
		})
	}

	// Agent authoring matrix on POST /schedules.
	fAgent := execAuthoringFixture(t)
	markEdgeBackfillComplete(t, fAgent.store)
	addProjectEdge(t, fAgent.store, store.DelegationPrincipalUser, fAgent.owner.ID, fAgent.caller.ID, fAgent.proj.ID)
	foreign := foreignOwnedAgent(t, fAgent, "exec-rec-foreign")

	agentCases := []struct {
		name    string
		target  *store.Agent
		scopes  []AgentTokenScope
		allowed bool
	}{
		{"self", fAgent.caller, []AgentTokenScope{ScopeAgentLifecycle}, true},
		{"child", fAgent.child, []AgentTokenScope{ScopeAgentLifecycle}, true},
		{"sibling", fAgent.sibling, []AgentTokenScope{ScopeAgentLifecycle}, true},
		{"other user's agent", foreign, []AgentTokenScope{ScopeAgentLifecycle}, false},
		{"baseline (no lifecycle scope)", fAgent.sibling, nil, false},
	}
	for _, c := range agentCases {
		t.Run("agent/"+c.name, func(t *testing.T) {
			before := len(execRecurringStoredSchedules(t, fAgent))
			rec := fAgent.asAgent(t, http.MethodPost, execRecurringSchedulesPath(fAgent),
				execRecurringCreateBody("agent-"+uuid.NewString()[:8], c.target.ID, "true"), c.scopes...)
			if c.allowed {
				require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
				assert.Len(t, execRecurringStoredSchedules(t, fAgent), before+1)
				return
			}
			assert.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, rec.Code, rec.Body.String())
			assert.Len(t, execRecurringStoredSchedules(t, fAgent), before)
		})
	}

	t.Run("delegated agent credential refused", func(t *testing.T) {
		before := len(execRecurringStoredSchedules(t, fAgent))
		delegated := &DelegatedAgentIdentity{agentID: fAgent.caller.ID}
		rec := requestAsIdentity(t, fAgent.srv, delegated, http.MethodPost, execRecurringSchedulesPath(fAgent),
			execRecurringCreateBody("delegated", fAgent.caller.ID, "true"))
		assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
		assert.Len(t, execRecurringStoredSchedules(t, fAgent), before)
	})
}

// TestScheduledExecRecurringAuthoring_ManagedStoppedAndTargetDeletedRecovery (AC2, AC4, AC11):
// managed runtimes are rejected 422 on create/PATCH/resume; stopped agents are
// accepted at authoring; a schedule paused with target_deleted cannot be resumed
// until retargeted to a live agent, after which resume clears status_reason.
func TestScheduledExecRecurringAuthoring_ManagedStoppedAndTargetDeletedRecovery(t *testing.T) {
	f := execAuthoringFixture(t)
	ctx := context.Background()

	managed := &store.Agent{
		ID: tid("exec-rec-managed"), Slug: tid("exec-rec-managed"), Name: "exec-rec-managed", ProjectID: f.proj.ID,
		Phase: "running", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
		Runtime: "managed:test",
	}
	require.NoError(t, f.store.CreateAgent(ctx, managed))

	// Create on managed -> 422.
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f),
		execRecurringCreateBody("managed-create", managed.ID, "true"))
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Equal(t, ErrCodeUnsupportedCapability, decodeTargetAPIError(t, rec).Code)

	// Stopped agent -> 201 Created at authoring.
	stopped := &store.Agent{
		ID: tid("exec-rec-stopped"), Slug: tid("exec-rec-stopped"), Name: "exec-rec-stopped", ProjectID: f.proj.ID,
		Phase: "stopped", CreatedBy: f.owner.ID, OwnerID: f.owner.ID, Ancestry: []string{f.owner.ID},
		Runtime: "docker",
	}
	require.NoError(t, f.store.CreateAgent(ctx, stopped))
	rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f),
		execRecurringCreateBody("stopped-ok", stopped.ID, "true"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	scID, _ := decodeEventResponse(t, rec)["id"].(string)

	// Retargeting active schedule to managed -> 422.
	rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch, execRecurringSchedulesPath(f)+"/"+scID,
		map[string]any{"agentId": managed.ID})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	// Target deletion pauses with target_deleted; resume before retargeting fails 404.
	f.srv.cancelScheduledEventsForAgent(ctx, stopped)
	f.srv.pauseExecSchedulesTargetingAgent(ctx, stopped)
	require.NoError(t, f.store.DeleteAgent(ctx, stopped.ID))
	paused, err := f.store.GetSchedule(ctx, scID)
	require.NoError(t, err)
	require.Equal(t, store.ScheduleStatusPaused, paused.Status)
	require.Equal(t, store.ScheduleStatusReasonTargetDeleted, paused.StatusReason)

	rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f)+"/"+scID+"/resume", nil)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// PATCH while paused to point at live f.sibling, then POST /resume clears target_deleted and activates.
	rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch, execRecurringSchedulesPath(f)+"/"+scID,
		map[string]any{"agentId": f.sibling.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f)+"/"+scID+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resumed, err := f.store.GetSchedule(ctx, scID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, resumed.Status)
	assert.Equal(t, store.ScheduleStatusReasonNone, resumed.StatusReason)
	p, err := decodeScheduledExecPayload(resumed.Payload)
	require.NoError(t, err)
	assert.Equal(t, f.sibling.ID, p.AgentID)
	assert.Equal(t, f.sibling.Slug, p.AgentName)
}

// TestScheduledExecRecurringAuthoring_FlagOffAndFlagFlip (AC1):
// with either hub flag off, create/PATCH/resume reject exec schedules without
// advertising exec in the supported types list, while pause and delete of an
// existing exec schedule continue to succeed.
func TestScheduledExecRecurringAuthoring_FlagOffAndFlagFlip(t *testing.T) {
	for _, flags := range []map[string]bool{
		{conduitExperiment: true, experiments.ExecSchedules: false},
		{conduitExperiment: false, experiments.ExecSchedules: true},
	} {
		f := execAuthoringFixture(t)
		active := execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusActive)
		paused := execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusPaused)

		setHubExperiments(t, f.srv, flags)

		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f),
			execRecurringCreateBody("flag-off", f.sibling.ID, "true"))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "unsupported event type: exec")
		assert.NotContains(t, rec.Body.String(), "supported: message, dispatch_agent, exec")

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch, execRecurringSchedulesPath(f)+"/"+active.ID,
			map[string]any{"command": []string{"echo", "nope"}})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "unsupported event type: exec")

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f)+"/"+paused.ID+"/resume", nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "unsupported event type: exec")

		// Pause and delete still work after a flag flip.
		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f)+"/"+active.ID+"/pause", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodDelete, execRecurringSchedulesPath(f)+"/"+active.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}
}

// TestScheduledExecRecurringAuthoring_PayloadBoundsAndBodyLimit (AC13):
// enforces payload bounds, raw tombstone, field exclusivity, partial PATCH
// convenience merges, and the 64 KiB body limit on /schedules.
func TestScheduledExecRecurringAuthoring_PayloadBoundsAndBodyLimit(t *testing.T) {
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
	mk := func(mut func(m map[string]any)) map[string]any {
		m := execRecurringCreateBody("bound-"+uuid.NewString()[:8], f.sibling.ID, "true")
		mut(m)
		return m
	}

	badCreate := map[string]map[string]any{
		"65 args":            mk(func(m map[string]any) { m["command"] = many }),
		"NUL":                mk(func(m map[string]any) { m["command"] = []string{"a\x00b"} }),
		"element over 4096":  mk(func(m map[string]any) { m["command"] = []string{big} }),
		"total over 16384":   mk(func(m map[string]any) { m["command"] = total }),
		"timeout 3601":       mk(func(m map[string]any) { m["timeoutSeconds"] = 3601 }),
		"timeout -1":         mk(func(m map[string]any) { m["timeoutSeconds"] = -1 }),
		"empty command":      mk(func(m map[string]any) { m["command"] = []string{} }),
		"empty argv0":        mk(func(m map[string]any) { m["command"] = []string{""} }),
		"wake in payload":    {"name": "w1", "cronExpr": "0 * * * *", "eventType": "exec", "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"],"wake":true}`},
		"unknown field":      {"name": "u1", "cronExpr": "0 * * * *", "eventType": "exec", "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"],"cwd":"/"}`},
		"payload and fields": {"name": "pf", "cronExpr": "0 * * * *", "eventType": "exec", "agentId": f.sibling.ID, "payload": `{"agentId":"` + f.sibling.ID + `","command":["true"]}`},
		"message field":      mk(func(m map[string]any) { m["message"] = "hi" }),
		"wake field":         mk(func(m map[string]any) { m["wake"] = true }),
		"template field":     mk(func(m map[string]any) { m["template"] = "x" }),
		"no target":          {"name": "nt", "cronExpr": "0 * * * *", "eventType": "exec", "command": []string{"true"}},
		"exec on message":    {"name": "em", "cronExpr": "0 * * * *", "eventType": "message", "agentName": f.sibling.Slug, "message": "hi", "command": []string{"true"}},
		"agentId on message": {"name": "am", "cronExpr": "0 * * * *", "eventType": "message", "agentId": f.sibling.ID, "agentName": f.sibling.Slug, "message": "hi"},
	}
	for name, b := range badCreate {
		t.Run("create/"+name, func(t *testing.T) {
			rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f), b)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Empty(t, execRecurringStoredSchedules(t, f))
		})
	}

	// Raw tombstone -> 422 raw_input_removed on POST /schedules.
	rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f), map[string]any{
		"name": "raw-tomb", "cronExpr": "0 * * * *", "eventType": "exec",
		"payload": `{"agentId":"` + f.sibling.ID + `","command":["true"],"raw":true}`,
	})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())

	// Raw payload create + partial convenience PATCH merges + PATCH bounds + 64 KiB body limit.
	fClean := execAuthoringFixture(t)
	rec = requestAsIdentity(t, fClean.srv, authUser(fClean.owner), http.MethodPost, execRecurringSchedulesPath(fClean), map[string]any{
		"name":      "raw-ok",
		"cronExpr":  "0 * * * *",
		"eventType": "exec",
		"payload":   `{"agentId":"` + fClean.sibling.ID + `","command":["echo","v1"],"timeoutSeconds":300}`,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	scID, _ := decodeEventResponse(t, rec)["id"].(string)

	// Partial PATCH: command only -> preserves timeoutSeconds=300 and agentId.
	rec = requestAsIdentity(t, fClean.srv, authUser(fClean.owner), http.MethodPatch, execRecurringSchedulesPath(fClean)+"/"+scID,
		map[string]any{"command": []string{"echo", "v2"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sc, err := fClean.store.GetSchedule(context.Background(), scID)
	require.NoError(t, err)
	p, err := decodeScheduledExecPayload(sc.Payload)
	require.NoError(t, err)
	assert.Equal(t, []string{"echo", "v2"}, p.Command)
	assert.Equal(t, 300, p.TimeoutSeconds)

	// Partial PATCH: timeoutSeconds=0 resets timeout while preserving command.
	rec = requestAsIdentity(t, fClean.srv, authUser(fClean.owner), http.MethodPatch, execRecurringSchedulesPath(fClean)+"/"+scID,
		map[string]any{"timeoutSeconds": 0})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	sc, err = fClean.store.GetSchedule(context.Background(), scID)
	require.NoError(t, err)
	p, err = decodeScheduledExecPayload(sc.Payload)
	require.NoError(t, err)
	assert.Equal(t, []string{"echo", "v2"}, p.Command)
	assert.Equal(t, 0, p.TimeoutSeconds)

	// Invalid PATCH bounds -> 400 and stored row unchanged.
	rec = requestAsIdentity(t, fClean.srv, authUser(fClean.owner), http.MethodPatch, execRecurringSchedulesPath(fClean)+"/"+scID,
		map[string]any{"timeoutSeconds": 3601})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = requestAsIdentity(t, fClean.srv, authUser(fClean.owner), http.MethodPatch, execRecurringSchedulesPath(fClean)+"/"+scID,
		map[string]any{"payload": `{"agentId":"` + fClean.sibling.ID + `","command":["true"]}`, "command": []string{"true"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Over 64 KiB body rejected on POST /schedules and PATCH /schedules/{id}.
	hugeCreate := execRecurringCreateBody("huge", fClean.sibling.ID, "true")
	hugeCreate["name"] = strings.Repeat("n", scheduleRequestBodyLimit+1)
	rec = requestAsIdentity(t, fClean.srv, authUser(fClean.owner), http.MethodPost, execRecurringSchedulesPath(fClean), hugeCreate)
	assert.GreaterOrEqual(t, rec.Code, 400)
	assert.Less(t, rec.Code, 500)

	rec = requestAsIdentity(t, fClean.srv, authUser(fClean.owner), http.MethodPatch, execRecurringSchedulesPath(fClean)+"/"+scID,
		map[string]any{"name": strings.Repeat("n", scheduleRequestBodyLimit+1)})
	assert.GreaterOrEqual(t, rec.Code, 400)
	assert.Less(t, rec.Code, 500)
}

// TestScheduledExecRecurringAuthoring_CrossQuotaAndConcurrentLastSlot (AC8, D5):
// pending one-shot exec events and active recurring exec schedules share the
// 20-per-target and 200-per-project caps; paused schedules are excluded;
// self-PATCH at 20/20 succeeds; retargeting or resuming over 20/20 fails 429;
// concurrent HTTP writers racing for the last slot never exceed 20.
func TestScheduledExecRecurringAuthoring_CrossQuotaAndConcurrentLastSlot(t *testing.T) {
	t.Run("cross-type per-target self-edit retarget pause and resume", func(t *testing.T) {
		f := execAuthoringFixture(t)
		for i := 0; i < 10; i++ {
			seedExecEvent(t, f, f.sibling.ID)
		}
		var activeSchedules []*store.Schedule
		for i := 0; i < 10; i++ {
			activeSchedules = append(activeSchedules, execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusActive))
		}
		pausedOnSibling := execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusPaused)
		activeOnChild := execRecurringSeedSchedule(t, f, f.child.ID, store.ScheduleStatusActive)

		// 21st recurring on f.sibling -> 429 quota_exceeded.
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f),
			execRecurringCreateBody("over-target", f.sibling.ID, "true"))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)

		// 21st one-shot on f.sibling -> 429 quota_exceeded (counts active recurring too!).
		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execEventsPath(f),
			execCreateBody(f.sibling.ID, "true"))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)

		// Editing an already-active schedule on f.sibling at 20/20 succeeds (excludes itself).
		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch,
			execRecurringSchedulesPath(f)+"/"+activeSchedules[0].ID,
			map[string]any{"command": []string{"echo", "self-edit-at-cap"}})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Retargeting activeOnChild onto full f.sibling -> 429 quota_exceeded.
		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch,
			execRecurringSchedulesPath(f)+"/"+activeOnChild.ID,
			map[string]any{"agentId": f.sibling.ID})
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)

		// Resuming pausedOnSibling via POST /resume or PATCH status=active -> 429 quota_exceeded.
		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost,
			execRecurringSchedulesPath(f)+"/"+pausedOnSibling.ID+"/resume", nil)
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch,
			execRecurringSchedulesPath(f)+"/"+pausedOnSibling.ID,
			map[string]any{"status": store.ScheduleStatusActive})
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)

		// Pausing one active schedule frees 1 slot -> resuming pausedOnSibling now succeeds!
		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost,
			execRecurringSchedulesPath(f)+"/"+activeSchedules[1].ID+"/pause", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost,
			execRecurringSchedulesPath(f)+"/"+pausedOnSibling.ID+"/resume", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("cross-type per-project cap", func(t *testing.T) {
		f := execAuthoringFixture(t)
		for i := 0; i < 100; i++ {
			seedExecEvent(t, f, uuid.NewString())
			execRecurringSeedSchedule(t, f, uuid.NewString(), store.ScheduleStatusActive)
		}
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f),
			execRecurringCreateBody("over-proj", f.sibling.ID, "true"))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeQuotaExceeded, decodeTargetAPIError(t, rec).Code)
	})

	t.Run("concurrent HTTP last-slot race across one-shot create recurring create and resume", func(t *testing.T) {
		f := execAuthoringFixture(t)
		for i := 0; i < scheduledExecMaxPerTarget-1; i++ {
			seedExecEvent(t, f, f.sibling.ID)
		}
		paused1 := execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusPaused)
		paused2 := execRecurringSeedSchedule(t, f, f.sibling.ID, store.ScheduleStatusPaused)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var admitted atomic.Int32
		run := func(fn func() int) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code := fn()
				if code == http.StatusCreated || code == http.StatusOK {
					admitted.Add(1)
				}
			}()
		}

		// Use two distinct authorized principals so the burst-10 rate limiter doesn't mask quota.
		uat := NewScopedUserIdentity(authUser(f.owner), f.proj.ID, append([]string{store.UATScopeAgentAttach, store.UATScopeAgentRead}, scheduledEventUATScopes...))
		for i := 0; i < 3; i++ {
			name := "race-rec-" + uuid.NewString()[:8]
			run(func() int {
				return requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost,
					execRecurringSchedulesPath(f), execRecurringCreateBody(name, f.sibling.ID, "true")).Code
			})
			run(func() int {
				return requestAsIdentity(t, f.srv, uat, http.MethodPost,
					execEventsPath(f), execCreateBody(f.sibling.ID, "true")).Code
			})
		}
		run(func() int {
			return requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost,
				execRecurringSchedulesPath(f)+"/"+paused1.ID+"/resume", nil).Code
		})
		run(func() int {
			return requestAsIdentity(t, f.srv, uat, http.MethodPost,
				execRecurringSchedulesPath(f)+"/"+paused2.ID+"/resume", nil).Code
		})

		close(start)
		wg.Wait()

		assert.LessOrEqual(t, admitted.Load(), int32(1), "at most 1 concurrent writer may claim the 20th slot")

		ctx := context.Background()
		evts, err := f.store.ListScheduledEvents(ctx, store.ScheduledEventFilter{
			ProjectID: f.proj.ID, EventType: scheduledEventTypeExec, Status: store.ScheduledEventPending,
		}, store.ListOptions{Limit: 100})
		require.NoError(t, err)
		scs, err := f.store.ListSchedules(ctx, store.ScheduleFilter{
			ProjectID: f.proj.ID, EventType: scheduledEventTypeExec, Status: store.ScheduleStatusActive,
		}, store.ListOptions{Limit: 100})
		require.NoError(t, err)
		assert.LessOrEqual(t, len(evts.Items)+len(scs.Items), scheduledExecMaxPerTarget)
	})
}

// TestScheduledExecRecurringAuthoring_RateLimitAndAudit (AC8, AC12):
// burst-10 rate limit covers recurring authoring, and create/update/pause/resume/delete
// emit content-free schedule.exec.* audit records with schedule_id and no secret argv.
func TestScheduledExecRecurringAuthoring_RateLimitAndAudit(t *testing.T) {
	t.Run("rate limit shared across recurring authoring", func(t *testing.T) {
		f := execAuthoringFixture(t)
		for i := 0; i < scheduledExecAuthoringBurst; i++ {
			rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f),
				execRecurringCreateBody("rl-"+uuid.NewString()[:8], f.sibling.ID, "true"))
			require.Equal(t, http.StatusCreated, rec.Code, "attempt %d: %s", i, rec.Body.String())
		}
		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f),
			execRecurringCreateBody("rl-over", f.sibling.ID, "true"))
		require.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
		assert.Equal(t, ErrCodeRateLimited, decodeTargetAPIError(t, rec).Code)
		assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	})

	t.Run("content-free lifecycle audits", func(t *testing.T) {
		f := execAuthoringFixture(t)
		logs := auditCapture(t, f.srv)
		sentinel := "EXECRECAUDIT-" + uuid.NewString()

		rec := requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, execRecurringSchedulesPath(f), map[string]any{
			"name":           "audit-lifecycle",
			"cronExpr":       "0 * * * *",
			"eventType":      "exec",
			"agentId":        f.sibling.ID,
			"command":        []string{"/bin/sh", "-c", "echo " + sentinel},
			"timeoutSeconds": 60,
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		scID, _ := decodeEventResponse(t, rec)["id"].(string)
		require.NotEmpty(t, scID)
		schedURL := execRecurringSchedulesPath(f) + "/" + scID

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPatch, schedURL, map[string]any{
			"command": []string{"/usr/bin/env", "SECRET=" + sentinel, "true"},
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, schedURL+"/pause", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodPost, schedURL+"/resume", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = requestAsIdentity(t, f.srv, authUser(f.owner), http.MethodDelete, schedURL, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

		out := logs.String()
		assert.Contains(t, out, "audit_action=schedule.exec.create")
		assert.Contains(t, out, "audit_action=schedule.exec.update")
		assert.Contains(t, out, "audit_action=schedule.exec.pause")
		assert.Contains(t, out, "audit_action=schedule.exec.resume")
		assert.Contains(t, out, "audit_action=schedule.exec.delete")
		assert.Contains(t, out, "schedule_id="+scID)
		assert.Contains(t, out, "shell_form=true")
		assert.Contains(t, out, "argv0=sh")
		assert.Contains(t, out, "argv0=env")
		assert.Contains(t, out, "authorization_revision=1")
		assert.Contains(t, out, "authorization_revision=2")
		assert.Contains(t, out, "authorization_revision=3")
		assert.NotContains(t, out, sentinel, "audit must never contain argv beyond argv0")
		assert.NotContains(t, out, "/bin/sh", "argv0 is sanitized basename only")
		assert.NotContains(t, out, "/usr/bin/env", "argv0 is sanitized basename only")
	})
}
