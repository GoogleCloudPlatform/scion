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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for pausing a schedule whose authority agent is deleted, with
// status reason creator_deleted (ptone/scion#5403).

// creatorPauseSchedule returns a due schedule whose recorded authority is
// agentID (an agent revision, revision 1).
func creatorPauseSchedule(name, projectID, agentID, status string) *store.Schedule {
	evt := withMockAgentRevision(store.ScheduledEvent{}, agentID)
	due := time.Now().Add(-time.Minute).UTC()
	return &store.Schedule{
		ID:                   tid("creator-pause-" + name),
		ProjectID:            projectID,
		Name:                 name,
		CronExpr:             "0 9 * * *",
		EventType:            "message",
		Payload:              `{"agentName":"all","message":"hi"}`,
		Status:               status,
		NextRunAt:            &due,
		CreatedBy:            agentID,
		InitiatorAttribution: evt.InitiatorAttribution,
		AuthorityCeiling:     evt.AuthorityCeiling,
	}
}

func mustGetSchedule(t *testing.T, s store.Store, id string) *store.Schedule {
	t.Helper()
	sc, err := s.GetSchedule(context.Background(), id)
	require.NoError(t, err)
	return sc
}

func scheduleEventCount(t *testing.T, s store.Store, projectID, scheduleID string) int {
	t.Helper()
	res, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{
		ProjectID: projectID, ScheduleID: scheduleID,
	}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	return len(res.Items)
}

// Deleting an agent, hard or soft, pauses the active schedules whose
// authority is that agent with creator_deleted. A schedule authored by a
// user or by another agent stays active, and one a user already paused
// keeps no reason.
func TestScheduleCreatorPause_AgentDeletePausesAuthoredSchedules(t *testing.T) {
	cases := []struct {
		name      string
		retention time.Duration
	}{
		{"hard", 0},
		{"soft", time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, _, _ := engineTestServer(t)
			srv.config.SoftDeleteRetention = tc.retention
			ctx := context.Background()
			agent := setupBrokerAgentInPhase(t, s, "cpause-"+tc.name, state.PhaseStopped)
			other := &store.Agent{
				ID: tid("cpause-other-" + tc.name), Slug: "cpause-other-" + tc.name,
				Name: "cpause-other", ProjectID: agent.ProjectID, Phase: string(state.PhaseStopped),
			}
			require.NoError(t, s.CreateAgent(ctx, other))

			authored := creatorPauseSchedule(tc.name+"-authored", agent.ProjectID, agent.ID, store.ScheduleStatusActive)
			userPaused := creatorPauseSchedule(tc.name+"-user-paused", agent.ProjectID, agent.ID, store.ScheduleStatusPaused)
			byOther := creatorPauseSchedule(tc.name+"-other", agent.ProjectID, other.ID, store.ScheduleStatusActive)
			byUser := creatorPauseSchedule(tc.name+"-user", agent.ProjectID, agent.ID, store.ScheduleStatusActive)
			userRev := withSessionRevision(store.ScheduledEvent{}, DevUserID)
			byUser.InitiatorAttribution = userRev.InitiatorAttribution
			byUser.AuthorityCeiling = userRev.AuthorityCeiling
			for _, sc := range []*store.Schedule{authored, userPaused, byOther, byUser} {
				require.NoError(t, s.CreateSchedule(ctx, sc))
			}

			rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
			require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

			require.Eventually(t, func() bool {
				return mustGetSchedule(t, s, authored.ID).Status == store.ScheduleStatusPaused
			}, 5*time.Second, 10*time.Millisecond, "the agent's schedule is paused")
			got := mustGetSchedule(t, s, authored.ID)
			assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, got.StatusReason)
			assert.Equal(t, 1, got.AuthorizationRevision, "a system pause does not re-record authority")

			got = mustGetSchedule(t, s, userPaused.ID)
			assert.Equal(t, store.ScheduleStatusPaused, got.Status)
			assert.Empty(t, got.StatusReason, "a schedule already paused is not touched")

			for _, sc := range []*store.Schedule{byOther, byUser} {
				got = mustGetSchedule(t, s, sc.ID)
				assert.Equal(t, store.ScheduleStatusActive, got.Status, sc.Name)
				assert.Empty(t, got.StatusReason, sc.Name)
			}
		})
	}
}

// A due schedule whose authority agent is gone (hard-deleted or
// soft-deleted) is paused with creator_deleted when it comes due: no event
// is created and no run or error is recorded.
func TestScheduleCreatorPause_FireWithDeletedCreatorPauses(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s store.Store, projectID string) string
	}{
		{"hard deleted", func(t *testing.T, s store.Store, projectID string) string {
			require.NoError(t, s.DeleteAgent(context.Background(), authzHelperAgentID))
			return authzHelperAgentID
		}},
		{"soft deleted", func(t *testing.T, s store.Store, projectID string) string {
			id := tid("cpause-soft-deleted-agent")
			require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
				ID: id, Slug: "cpause-soft-deleted", Name: "cpause-soft-deleted",
				ProjectID: projectID, Phase: string(state.PhaseStopped), DeletedAt: time.Now(),
			}))
			return id
		}},
		{"never existed", func(t *testing.T, s store.Store, projectID string) string {
			return tid("cpause-missing-agent")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)
			ctx := context.Background()
			agentID := tc.setup(t, s, projectID)
			sc := creatorPauseSchedule("fire", projectID, agentID, store.ScheduleStatusActive)
			require.NoError(t, s.CreateSchedule(ctx, sc))

			srv.executeSchedule(ctx, *mustGetSchedule(t, s, sc.ID), time.Now().UTC())

			got := mustGetSchedule(t, s, sc.ID)
			assert.Equal(t, store.ScheduleStatusPaused, got.Status)
			assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, got.StatusReason)
			assert.Zero(t, got.RunCount, "no run is recorded")
			assert.Zero(t, got.ErrorCount, "no refusal is recorded")
			assert.Empty(t, got.LastRunError)
			assert.Zero(t, scheduleEventCount(t, s, projectID, sc.ID), "no event is created")
		})
	}
}

// Positive control for the backstop: a schedule whose authority agent is
// live fires (an event is created and a run recorded) and stays active.
func TestScheduleCreatorPause_FireWithLiveCreatorFires(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	sc := creatorPauseSchedule("live", projectID, authzHelperAgentID, store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, sc))

	srv.executeSchedule(ctx, *mustGetSchedule(t, s, sc.ID), time.Now().UTC())

	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Empty(t, got.StatusReason)
	assert.Equal(t, 1, got.RunCount)
	assert.Equal(t, 1, scheduleEventCount(t, s, projectID, sc.ID))
}

// Resuming a schedule paused with creator_deleted clears the reason and
// re-records the authority to the resumer in the same write.
func TestScheduleCreatorPause_ResumeClearsReasonAndReRecordsAuthority(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	sc := creatorPauseSchedule("resume", projectID, authzHelperAgentID, store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, sc))
	require.NoError(t, s.DeleteAgent(ctx, authzHelperAgentID))
	srv.executeSchedule(ctx, *mustGetSchedule(t, s, sc.ID), time.Now().UTC())
	require.Equal(t, store.ScheduleStatusReasonCreatorDeleted, mustGetSchedule(t, s, sc.ID).StatusReason)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+sc.ID+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, store.ScheduleStatusActive, body["status"])
	assert.NotContains(t, body, "statusReason", "an empty reason is omitted")

	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Empty(t, got.StatusReason, "resume clears the reason")
	assert.Equal(t, 2, got.AuthorizationRevision)
	assert.NotEqual(t, authzHelperAgentID, got.InitiatorPrincipalID, "authority is re-recorded to the resumer")
	assert.NotEqual(t, store.DelegationPrincipalAgent, got.InitiatorPrincipalKind)
}

// An enable through PATCH (status active) is a user status write: it
// clears the reason.
func TestScheduleCreatorPause_PatchEnableClearsReason(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	sc := creatorPauseSchedule("patch", projectID, authzHelperAgentID, store.ScheduleStatusPaused)
	sc.StatusReason = store.ScheduleStatusReasonCreatorDeleted
	require.NoError(t, s.CreateSchedule(ctx, sc))

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+sc.ID,
		UpdateScheduleRequest{Status: store.ScheduleStatusActive})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Empty(t, got.StatusReason)
}

// The system pause is conditioned on the revision it read: once a resume
// has re-recorded the authority (bumping the revision), a pause built from
// the earlier read does not land.
func TestScheduleCreatorPause_ConcurrentResumeWins(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	sc := creatorPauseSchedule("race", projectID, authzHelperAgentID, store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, sc))
	stale := *mustGetSchedule(t, s, sc.ID)

	// A re-attributing write lands first (as a resume's does).
	fresh := mustGetSchedule(t, s, sc.ID)
	userRev := withSessionRevision(store.ScheduledEvent{}, DevUserID)
	newAttr := userRev.InitiatorAttribution
	newAttr.AuthorizationRevision = 2
	fresh.AuthorityCeiling = userRev.AuthorityCeiling
	require.NoError(t, s.UpdateSchedule(ctx, fresh, store.ScheduleFieldMask{}, 1, true, &newAttr))

	err := srv.pauseScheduleWithReason(ctx, stale, store.ScheduleStatusReasonCreatorDeleted)
	require.ErrorIs(t, err, store.ErrRevisionConflict)
	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Empty(t, got.StatusReason)
}

// The schedule list API carries statusReason for a system-paused schedule
// and omits it otherwise.
func TestScheduleCreatorPause_ListShowsReason(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	paused := creatorPauseSchedule("list-paused", projectID, authzHelperAgentID, store.ScheduleStatusPaused)
	paused.StatusReason = store.ScheduleStatusReasonCreatorDeleted
	active := creatorPauseSchedule("list-active", projectID, authzHelperAgentID, store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, paused))
	require.NoError(t, s.CreateSchedule(ctx, active))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Schedules []map[string]any `json:"schedules"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	byName := map[string]map[string]any{}
	for _, sc := range resp.Schedules {
		byName[sc["name"].(string)] = sc
	}
	require.Contains(t, byName, "list-paused")
	require.Contains(t, byName, "list-active")
	assert.Equal(t, "paused", byName["list-paused"]["status"])
	assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, byName["list-paused"]["statusReason"])
	assert.NotContains(t, byName["list-active"], "statusReason")

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules/"+paused.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var one map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &one))
	assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, one["statusReason"])
}

// The existing zone-prefix auto-pause now records cron_zone_unsupported.
func TestScheduleCreatorPause_ZonePrefixPauseRecordsReason(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	sc := creatorPauseSchedule("zone", projectID, authzHelperAgentID, store.ScheduleStatusActive)
	sc.CronExpr = "CRON_TZ=Asia/Tokyo 0 9 * * *"
	require.NoError(t, s.CreateSchedule(ctx, sc))

	srv.executeSchedule(ctx, *mustGetSchedule(t, s, sc.ID), time.Now().UTC())

	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status)
	assert.Equal(t, store.ScheduleStatusReasonCronZoneUnsupported, got.StatusReason)
}

// failingUpdateScheduleStore fails every UpdateSchedule with an error that is
// neither a revision conflict nor not-found. It is passed to the Server
// constructor, so srv.store is never reassigned; it forwards the raw store's
// optional DB and Dialect methods, which New() and other Server code
// type-assert (see store_fault_helpers_test.go).
type failingUpdateScheduleStore struct {
	store.Store
}

func (f *failingUpdateScheduleStore) DB() *sql.DB {
	if p, ok := f.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (f *failingUpdateScheduleStore) Dialect() string {
	if p, ok := f.Store.(interface{ Dialect() string }); ok {
		return p.Dialect()
	}
	return ""
}

func (f *failingUpdateScheduleStore) UpdateSchedule(context.Context, *store.Schedule, store.ScheduleFieldMask, int, bool, *store.InitiatorAttribution) error {
	return errors.New("injected update failure")
}

// When the backstop's pause fails with any other error, the schedule fires as
// before: the fire is refused, recorded as a failed event and counted, and
// the schedule stays active with no reason, so the pause is retried at its
// next run.
func TestScheduleCreatorPause_BackstopPauseErrorFallsBackToFire(t *testing.T) {
	base, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	srv, s := testServerOnMigratedStore(t, &failingUpdateScheduleStore{Store: base})
	srv, s, projectID := initScheduleTest(t, srv, s)
	ctx := context.Background()
	sc := creatorPauseSchedule("fallback", projectID, authzHelperAgentID, store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, sc))
	require.NoError(t, s.DeleteAgent(ctx, authzHelperAgentID))

	srv.executeSchedule(ctx, *mustGetSchedule(t, s, sc.ID), time.Now().UTC())

	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status, "a failed pause leaves the schedule active")
	assert.Empty(t, got.StatusReason, "a failed pause records no reason")
	assert.Equal(t, 1, got.RunCount, "the fallback fire is recorded")
	assert.Equal(t, 1, got.ErrorCount, "the fallback fire is refused")
	res, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID, ScheduleID: sc.ID}, store.ListOptions{Limit: 10})
	require.NoError(t, err)
	require.Len(t, res.Items, 1, "the fallback fire creates one event")
	assert.Equal(t, store.ScheduledEventFailed, res.Items[0].Status, "the fallback event is failed")
}

// The delete hook pages through every active schedule in the project: with
// more than one page of them, the agent's schedules on every page are paused
// and the others are left active.
func TestScheduleCreatorPause_AgentDeletePagesAllSchedules(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "cpause-pages", state.PhaseStopped)
	total := scheduleCreatorPausePageSize + 5
	var authored, others []string
	for i := 0; i < total; i++ {
		sc := creatorPauseSchedule(fmt.Sprintf("pages-%03d", i), agent.ProjectID, agent.ID, store.ScheduleStatusActive)
		if i%2 == 1 {
			userRev := withSessionRevision(store.ScheduledEvent{}, DevUserID)
			sc.InitiatorAttribution = userRev.InitiatorAttribution
			sc.AuthorityCeiling = userRev.AuthorityCeiling
			others = append(others, sc.ID)
		} else {
			authored = append(authored, sc.ID)
		}
		require.NoError(t, s.CreateSchedule(ctx, sc))
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	last := authored[len(authored)-1]
	require.Eventually(t, func() bool {
		return mustGetSchedule(t, s, last).Status == store.ScheduleStatusPaused
	}, 10*time.Second, 10*time.Millisecond, "the last page's schedule is paused")
	for _, id := range authored {
		got := mustGetSchedule(t, s, id)
		assert.Equal(t, store.ScheduleStatusPaused, got.Status, "authored schedule %s is paused", id)
		assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, got.StatusReason, "authored schedule %s has the reason", id)
	}
	for _, id := range others {
		assert.Equal(t, store.ScheduleStatusActive, mustGetSchedule(t, s, id).Status, "user schedule %s stays active", id)
	}
}
