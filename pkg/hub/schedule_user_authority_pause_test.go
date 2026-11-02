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
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for pausing the schedules whose recorded authority is a deleted
// user, with status reason creator_deleted (ptone/scion#2143).

// userAuthoritySchedule returns a due schedule whose recorded authority is
// userID (a session revision, revision 1). CreatedBy is userID too, unless
// a test changes it.
func userAuthoritySchedule(name, projectID, userID, status string) *store.Schedule {
	rev := withSessionRevision(store.ScheduledEvent{}, userID)
	due := time.Now().Add(-time.Minute).UTC()
	return &store.Schedule{
		ID:                   tid("user-pause-" + name),
		ProjectID:            projectID,
		Name:                 name,
		CronExpr:             "0 9 * * *",
		EventType:            "message",
		Payload:              `{"agentName":"all","message":"hi"}`,
		Status:               status,
		NextRunAt:            &due,
		CreatedBy:            userID,
		InitiatorAttribution: rev.InitiatorAttribution,
		AuthorityCeiling:     rev.AuthorityCeiling,
	}
}

// requireCreatorDeletedPause asserts schedule id is paused with
// creator_deleted, with its authority recorded at revision 1 as before.
func requireCreatorDeletedPause(t *testing.T, s store.Store, id string) {
	t.Helper()
	got := mustGetSchedule(t, s, id)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status, got.Name)
	assert.Equal(t, store.ScheduleStatusReasonCreatorDeleted, got.StatusReason, got.Name)
	assert.Equal(t, 1, got.AuthorizationRevision, "%s: a system pause does not re-record authority", got.Name)
}

// requireScheduleUntouched asserts schedule id kept status and an empty
// reason.
func requireScheduleUntouched(t *testing.T, s store.Store, id, status string) {
	t.Helper()
	got := mustGetSchedule(t, s, id)
	assert.Equal(t, status, got.Status, got.Name)
	assert.Empty(t, got.StatusReason, got.Name)
}

// A plain user delete pauses, in every project, the active schedules whose
// recorded authority is the user. A schedule only created by the user
// (authority re-recorded to someone else), a legacy schedule with no
// recorded authority, a schedule whose authority is an agent, and one the
// user already paused are left as they are.
func TestScheduleUserAuthorityPause_PlainDeletePauses(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-pause-dave", "user-pause-dave@test.com")
	other := newActiveMember(t, s, "user-pause-erin", "user-pause-erin@test.com")
	projectB := &store.Project{ID: tid("project-user-pause-b"), Name: "User Pause B", Slug: "user-pause-b"}
	require.NoError(t, s.CreateProject(ctx, projectB))

	held := userAuthoritySchedule("held", projectID, dave.ID, store.ScheduleStatusActive)
	heldB := userAuthoritySchedule("held-b", projectB.ID, dave.ID, store.ScheduleStatusActive)
	userPaused := userAuthoritySchedule("user-paused", projectID, dave.ID, store.ScheduleStatusPaused)
	createdOnly := userAuthoritySchedule("created-only", projectID, other.ID, store.ScheduleStatusActive)
	createdOnly.CreatedBy = dave.ID
	legacy := userAuthoritySchedule("legacy", projectID, dave.ID, store.ScheduleStatusActive)
	legacy.InitiatorAttribution = store.InitiatorAttribution{}
	byAgent := creatorPauseSchedule("by-agent", projectID, authzHelperAgentID, store.ScheduleStatusActive)
	for _, sc := range []*store.Schedule{held, heldB, userPaused, createdOnly, legacy, byAgent} {
		require.NoError(t, s.CreateSchedule(ctx, sc))
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	requireCreatorDeletedPause(t, s, held.ID)
	requireCreatorDeletedPause(t, s, heldB.ID)
	requireScheduleUntouched(t, s, userPaused.ID, store.ScheduleStatusPaused)
	requireScheduleUntouched(t, s, createdOnly.ID, store.ScheduleStatusActive)
	requireScheduleUntouched(t, s, legacy.ID, store.ScheduleStatusActive)
	requireScheduleUntouched(t, s, byAgent.ID, store.ScheduleStatusActive)
	assert.Equal(t, 1, mustGetSchedule(t, s, byAgent.ID).AuthorizationRevision, "the agent schedule's authority is unchanged")
}

// A forced delete pauses the user's schedules as soon as its revoke
// commits, before the agents are stopped, and they stay paused after the
// deletion completes. A schedule whose authority is another user's agent
// is not touched.
func TestScheduleUserAuthorityPause_ForceDeletePauses(t *testing.T) {
	f := newForceDelFixture(t, time.Hour)
	ctx := context.Background()
	f.agent(t, "user-pause-a", nil)
	other := createOwnedAgent(t, f.s, "user-pause-other", f.project.ID, DevUserID)

	held := userAuthoritySchedule("force-held", f.project.ID, f.dave.ID, store.ScheduleStatusActive)
	byAgent := creatorPauseSchedule("force-by-agent", f.project.ID, other.ID, store.ScheduleStatusActive)
	for _, sc := range []*store.Schedule{held, byAgent} {
		require.NoError(t, f.s.CreateSchedule(ctx, sc))
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	f.disp.setFn(blockingDelete(entered, release, nil))
	rec := f.forceDelete(t)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, store.UserDeletionStateStopping, decodeDeletion(t, rec).State)

	// Paused while the user is deleting, before the agent is stopped.
	requireCreatorDeletedPause(t, f.s, held.ID)
	requireScheduleUntouched(t, f.s, byAgent.ID, store.ScheduleStatusActive)

	<-entered
	close(release)
	f.waitCompleted(t)

	requireCreatorDeletedPause(t, f.s, held.ID)
	requireScheduleUntouched(t, f.s, byAgent.ID, store.ScheduleStatusActive)
	assert.Equal(t, 1, mustGetSchedule(t, f.s, byAgent.ID).AuthorizationRevision, "the agent schedule's authority is unchanged")
}

// Resuming a schedule paused because its user was deleted clears the
// reason and re-records the authority to the resumer, so the evaluator
// fires it instead of pausing it again.
func TestScheduleUserAuthorityPause_ResumeReRecordsToResumer(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-pause-resume", "user-pause-resume@test.com")
	sc := userAuthoritySchedule("resume", projectID, dave.ID, store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, sc))
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/users/"+dave.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	requireCreatorDeletedPause(t, s, sc.ID)

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules/"+sc.ID+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	assert.Empty(t, got.StatusReason, "resume clears the reason")
	assert.Equal(t, 2, got.AuthorizationRevision)
	assert.NotEqual(t, dave.ID, got.InitiatorPrincipalID, "authority is re-recorded to the resumer")
	assert.Equal(t, dave.ID, got.CreatedBy, "CreatedBy stays history")

	srv.executeSchedule(ctx, *got, time.Now().UTC())
	got = mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status, "the resumed schedule is not paused again")
	assert.Empty(t, got.StatusReason)
	assert.Equal(t, 1, got.RunCount, "the resumed schedule fires")
}

// The evaluator backstop pauses a due schedule whose authority user has no
// row, or whose status is deleting, with creator_deleted: no event is
// created and no run is recorded. A schedule whose authority user is
// active or suspended is not paused: it fires and fire-time authority
// decides.
func TestScheduleUserAuthorityPause_Backstop(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, s store.Store) string
		gone  bool
	}{
		{"deleted", func(t *testing.T, s store.Store) string {
			u := newActiveMember(t, s, "user-pause-bs-deleted", "user-pause-bs-deleted@test.com")
			require.NoError(t, s.DeleteUser(context.Background(), u.ID))
			return u.ID
		}, true},
		{"never existed", func(t *testing.T, s store.Store) string {
			return tid("user-pause-bs-missing")
		}, true},
		{"deleting", func(t *testing.T, s store.Store) string {
			u := newActiveMember(t, s, "user-pause-bs-deleting", "user-pause-bs-deleting@test.com")
			require.NoError(t, s.MarkUserDeleting(context.Background(), u.ID))
			return u.ID
		}, true},
		{"active", func(t *testing.T, s store.Store) string {
			return newActiveMember(t, s, "user-pause-bs-active", "user-pause-bs-active@test.com").ID
		}, false},
		{"suspended", func(t *testing.T, s store.Store) string {
			u := newActiveMember(t, s, "user-pause-bs-suspended", "user-pause-bs-suspended@test.com")
			u.Status = store.UserStatusSuspended
			require.NoError(t, s.UpdateUser(context.Background(), u))
			return u.ID
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, projectID := setupScheduleTest(t)
			ctx := context.Background()
			userID := tc.setup(t, s)
			sc := userAuthoritySchedule("backstop", projectID, userID, store.ScheduleStatusActive)
			require.NoError(t, s.CreateSchedule(ctx, sc))

			srv.executeSchedule(ctx, *mustGetSchedule(t, s, sc.ID), time.Now().UTC())

			got := mustGetSchedule(t, s, sc.ID)
			if tc.gone {
				requireCreatorDeletedPause(t, s, sc.ID)
				assert.Zero(t, got.RunCount, "no run is recorded")
				assert.Zero(t, got.ErrorCount, "no refusal is recorded")
				assert.Zero(t, scheduleEventCount(t, s, projectID, sc.ID), "no event is created")
				return
			}
			requireScheduleUntouched(t, s, sc.ID, store.ScheduleStatusActive)
			assert.Equal(t, 1, got.RunCount, "the schedule fires")
			assert.Equal(t, 1, scheduleEventCount(t, s, projectID, sc.ID))
		})
	}
}

// reattributeOnListStore runs hook once, right after the first schedule
// list filtered by authority principal returns: a resume landing between
// the user delete's list and its pause. It is passed to the Server
// constructor and forwards DB and Dialect, as failingUpdateScheduleStore
// does.
type reattributeOnListStore struct {
	store.Store
	once sync.Once
	hook func(ctx context.Context)
}

func (r *reattributeOnListStore) DB() *sql.DB {
	if p, ok := r.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (r *reattributeOnListStore) Dialect() string {
	if p, ok := r.Store.(interface{ Dialect() string }); ok {
		return p.Dialect()
	}
	return ""
}

func (r *reattributeOnListStore) ListSchedules(ctx context.Context, filter store.ScheduleFilter, opts store.ListOptions) (*store.ListResult[store.Schedule], error) {
	res, err := r.Store.ListSchedules(ctx, filter, opts)
	if err == nil && !filter.AuthorityPrincipal.IsZero() && r.hook != nil {
		r.once.Do(func() { r.hook(ctx) })
	}
	return res, err
}

// reattributeSchedule re-records schedule id's authority to the dev user
// at revision 2, as a resume does.
func reattributeSchedule(t *testing.T, ctx context.Context, s store.Store, id string) {
	t.Helper()
	fresh := mustGetSchedule(t, s, id)
	rev := withSessionRevision(store.ScheduledEvent{}, DevUserID)
	attr := rev.InitiatorAttribution
	attr.AuthorizationRevision = 2
	fresh.AuthorityCeiling = rev.AuthorityCeiling
	require.NoError(t, s.UpdateSchedule(ctx, fresh, store.ScheduleFieldMask{}, 1, true, &attr))
}

// A schedule resumed (re-attributed) after the user delete listed it is not
// paused: the pause is conditioned on the revision it read.
func TestScheduleUserAuthorityPause_ConcurrentResumeNotRepaused(t *testing.T) {
	base, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	wrapped := &reattributeOnListStore{Store: base}
	srv, s := testServerOnMigratedStore(t, wrapped)
	srv, s, projectID := initScheduleTest(t, srv, s)
	ctx := context.Background()
	dave := newActiveMember(t, s, "user-pause-race", "user-pause-race@test.com")
	raced := userAuthoritySchedule("race", projectID, dave.ID, store.ScheduleStatusActive)
	plain := userAuthoritySchedule("race-plain", projectID, dave.ID, store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, raced))
	require.NoError(t, s.CreateSchedule(ctx, plain))
	wrapped.hook = func(ctx context.Context) { reattributeSchedule(t, ctx, base, raced.ID) }

	srv.pauseSchedulesAuthoredByUser(ctx, dave.ID)

	got := mustGetSchedule(t, s, raced.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status, "the resumed schedule is not paused")
	assert.Empty(t, got.StatusReason)
	assert.Equal(t, 2, got.AuthorizationRevision)
	assert.Equal(t, DevUserID, got.InitiatorPrincipalID)
	requireCreatorDeletedPause(t, s, plain.ID)
}

// The evaluator backstop, given a row listed before a resume landed, skips
// the tick without pausing or firing it.
func TestScheduleUserAuthorityPause_BackstopConcurrentResumeNotRepaused(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	sc := userAuthoritySchedule("backstop-race", projectID, tid("user-pause-race-missing"), store.ScheduleStatusActive)
	require.NoError(t, s.CreateSchedule(ctx, sc))
	stale := *mustGetSchedule(t, s, sc.ID)
	reattributeSchedule(t, ctx, s, sc.ID)

	srv.executeSchedule(ctx, stale, time.Now().UTC())

	got := mustGetSchedule(t, s, sc.ID)
	assert.Equal(t, store.ScheduleStatusActive, got.Status, "the resumed schedule is not paused")
	assert.Empty(t, got.StatusReason)
	assert.Equal(t, 2, got.AuthorizationRevision)
	assert.Zero(t, got.RunCount, "the stale row does not fire this tick")
	assert.Zero(t, scheduleEventCount(t, s, projectID, sc.ID))
}
