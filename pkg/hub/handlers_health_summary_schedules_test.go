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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createHealthSchedule stores an active schedule in projectID whose payload
// addresses agent "worker" with a message body that must never reach the
// summary. Each errs entry is one completed run: "" for success, otherwise
// the run's error.
func createHealthSchedule(t *testing.T, s store.Store, projectID, name string, ranAt time.Time, errs ...string) *store.Schedule {
	t.Helper()
	ctx := context.Background()
	next := ranAt.Add(time.Hour)
	sc := &store.Schedule{
		ID:        tid("health-sched-" + name),
		ProjectID: projectID,
		Name:      name,
		CronExpr:  "0 * * * *",
		EventType: "message",
		Payload:   `{"agentName":"worker","message":"message-body-not-for-summary"}`,
		NextRunAt: &next,
		CreatedBy: "test",
	}
	require.NoError(t, s.CreateSchedule(ctx, sc))
	for _, e := range errs {
		require.NoError(t, s.UpdateScheduleAfterRun(ctx, sc.ID, ranAt, next, e))
	}
	return sc
}

func createHealthScheduleProject(t *testing.T, s store.Store, slug string) *store.Project {
	t.Helper()
	p := &store.Project{ID: tid("health-sched-project-" + slug), Name: slug, Slug: slug}
	require.NoError(t, s.CreateProject(context.Background(), p))
	return p
}

func failingScheduleIDs(sec *HealthSummarySchedules) []string {
	ids := []string{}
	for _, f := range sec.Failing {
		ids = append(ids, f.ID)
	}
	return ids
}

// A schedule is listed when it is active and its last run failed. A
// schedule whose last run succeeded, one that never ran, and a paused
// schedule whose last run failed are not.
func TestHandleHealthSummary_FailingSchedulesListed(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "sched-alpha")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

	failing := createHealthSchedule(t, s, p.ID, "failing", ranAt, "", "first error", "schedule authority denied")
	createHealthSchedule(t, s, p.ID, "recovered", ranAt, "an error", "")
	createHealthSchedule(t, s, p.ID, "never-run", ranAt)
	paused := createHealthSchedule(t, s, p.ID, "paused", ranAt, "an error")
	require.NoError(t, s.UpdateScheduleStatus(context.Background(), paused.ID, store.ScheduleStatusPaused))

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	require.NotNil(t, resp.Schedules)
	assert.Equal(t, 1, resp.Schedules.Total)
	assert.False(t, resp.Schedules.Truncated)
	require.Len(t, resp.Schedules.Failing, 1)
	got := resp.Schedules.Failing[0]
	require.NotNil(t, got.LastFailureAt)
	assert.True(t, got.LastFailureAt.Equal(ranAt), "last_failure_at %v, want %v", got.LastFailureAt, ranAt)
	got.LastFailureAt = nil
	assert.Equal(t, HealthFailingSchedule{
		ID: failing.ID, Name: "failing", ProjectID: p.ID, ProjectSlug: "sched-alpha",
		EventType: "message", Target: "worker", RunCount: 3, ErrorCount: 2,
		LastError: "schedule authority denied",
	}, got)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: failing.ID, Name: "failing", ProjectID: p.ID},
		Message: "Scheduled event sched-alpha / failing failed its last run",
	})
	assert.Equal(t, HealthStatusHealthy, resp.Status, "a failing schedule does not change the status")
	assert.NotContains(t, rr.Body.String(), "message-body-not-for-summary")
}

// The stored last error is capped in runes; the rows are most recent
// failure first; past the row cap the total stays true and one item covers
// the rest.
func TestHandleHealthSummary_FailingSchedulesCapAndOrder(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "sched-cap")
	old := createHealthSchedule(t, s, p.ID, "old", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "e")
	newer := createHealthSchedule(t, s, p.ID, "newer", time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC), strings.Repeat("é", healthScheduleLastErrorMaxRunes+10))
	createHealthSchedule(t, s, p.ID, "oldest", time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), "e")

	prevLimit, prevPage := healthSummaryScheduleLimit, healthSummarySchedulePageSize
	healthSummaryScheduleLimit, healthSummarySchedulePageSize = 2, 1
	t.Cleanup(func() { healthSummaryScheduleLimit, healthSummarySchedulePageSize = prevLimit, prevPage })

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Schedules)
	assert.Equal(t, 3, resp.Schedules.Total)
	assert.True(t, resp.Schedules.Truncated)
	assert.Equal(t, []string{newer.ID, old.ID}, failingScheduleIDs(resp.Schedules))
	assert.Equal(t, strings.Repeat("é", healthScheduleLastErrorMaxRunes)+"…", resp.Schedules.Failing[0].LastError)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
		Message: "1 more scheduled event failed its last run",
	})
}

// The schedules section follows the schedules API's read rule: a caller
// with hub.health.read alone sees no schedule, and a project member sees
// only that project's schedules. The status is the same for every caller.
func TestHandleHealthSummary_FailingSchedulesFollowScheduleReadAccess(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	alpha := createHealthScheduleProject(t, s, "sched-acc-alpha")
	beta := createHealthScheduleProject(t, s, "sched-acc-beta")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	inAlpha := createHealthSchedule(t, s, alpha.ID, "acc-alpha", ranAt, "e")
	inBeta := createHealthSchedule(t, s, beta.ID, "acc-beta", ranAt, "e")

	healthOnly := healthSummaryRoleUser(t, s, "hs-sched-health-only", []string{"hub.health.read"})
	alphaMember := healthSummaryRoleUser(t, s, "hs-sched-alpha-member", []string{"hub.health.read"})
	grantUserProjectAccess(t, s, alphaMember.ID, alpha.ID)

	t.Run("health.read only: none", func(t *testing.T) {
		resp, body := healthSummaryAsUser(t, srv, healthOnly)
		require.NotNil(t, resp.Schedules)
		assert.Equal(t, 0, resp.Schedules.Total)
		assert.Empty(t, resp.Schedules.Failing)
		assert.NotContains(t, body, "acc-alpha")
		assert.NotContains(t, body, "acc-beta")
		assert.Equal(t, HealthStatusHealthy, resp.Status)
	})

	t.Run("member of alpha: alpha only", func(t *testing.T) {
		resp, body := healthSummaryAsUser(t, srv, alphaMember)
		require.NotNil(t, resp.Schedules)
		assert.Equal(t, 1, resp.Schedules.Total)
		assert.Equal(t, []string{inAlpha.ID}, failingScheduleIDs(resp.Schedules))
		assert.NotContains(t, body, inBeta.ID)
		assert.NotContains(t, body, "acc-beta")
		assert.Equal(t, HealthStatusHealthy, resp.Status)
	})

	t.Run("hub admin: both", func(t *testing.T) {
		rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var resp HealthSummaryResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		require.NotNil(t, resp.Schedules)
		assert.ElementsMatch(t, []string{inAlpha.ID, inBeta.ID}, failingScheduleIDs(resp.Schedules))
	})
}

// scheduleListFailStore fails ListSchedules.
type scheduleListFailStore struct{ store.Store }

func (scheduleListFailStore) ListSchedules(context.Context, store.ScheduleFilter, store.ListOptions) (*store.ListResult[store.Schedule], error) {
	return nil, errors.New("schedule list failed: dial tcp 10.0.0.9:5432")
}

// A failed schedule read leaves the section null with one warning, and no
// store error text in the response.
func TestHandleHealthSummary_FailingSchedulesNotReported(t *testing.T) {
	srv, _, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) scheduleListFailStore {
		return scheduleListFailStore{Store: inner}
	})
	tickHubInstance(t, srv)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	assert.Equal(t, "null", string(raw["schedules"]))
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
		Message: "Scheduled event data not available",
	})
	assert.NotContains(t, rr.Body.String(), "10.0.0.9")
}

// Past the scan limit only the first rows in store order are read: the
// section says so, Total is a lower bound and one item notes the rest.
func TestHandleHealthSummary_FailingSchedulesScanLimited(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "sched-scan")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	for _, n := range []string{"scan-a", "scan-b", "scan-c"} {
		createHealthSchedule(t, s, p.ID, n, ranAt, "e")
	}
	prevScan, prevPage := healthSummaryScheduleScanLimit, healthSummarySchedulePageSize
	healthSummaryScheduleScanLimit, healthSummarySchedulePageSize = 2, 1
	t.Cleanup(func() { healthSummaryScheduleScanLimit, healthSummarySchedulePageSize = prevScan, prevPage })

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Schedules)
	assert.True(t, resp.Schedules.ScanLimited)
	assert.True(t, resp.Schedules.Truncated)
	assert.Equal(t, 2, resp.Schedules.Total)
	assert.Len(t, resp.Schedules.Failing, 2)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
		Message: "More scheduled events may have failed their last run",
	})
}

// healthSchedAgentTypeIdentity reports Type() "agent" without being an
// AgentIdentity, so its project cannot be checked.
type healthSchedAgentTypeIdentity struct{}

func (healthSchedAgentTypeIdentity) ID() string   { return "hs-agent-type-only" }
func (healthSchedAgentTypeIdentity) Type() string { return "agent" }

// An agent caller sees the schedules of its own project only, and only
// with project:read. An agent-type identity without agent claims, and an
// empty project, are denied.
func TestHealthSummaryCanReadSchedules_AgentCaller(t *testing.T) {
	srv, _ := testServer(t)
	own, other := tid("hs-agent-own"), tid("hs-agent-other")
	req := func(id Identity) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/admin/health/summary", nil)
		return r.WithContext(contextWithIdentity(r.Context(), id))
	}
	assert.True(t, srv.healthSummaryCanReadSchedules(req(authzHelperAgent(own, ScopeProjectRead)), own))
	assert.False(t, srv.healthSummaryCanReadSchedules(req(authzHelperAgent(own, ScopeProjectRead)), other))
	assert.False(t, srv.healthSummaryCanReadSchedules(req(authzHelperAgent(own)), own))
	assert.False(t, srv.healthSummaryCanReadSchedules(req(healthSchedAgentTypeIdentity{}), own))
	assert.False(t, srv.healthSummaryCanReadSchedules(req(authzHelperAgent(own, ScopeProjectRead)), ""))
}

// The schedules API's denials keep their status and body after the access
// decision moved into scheduledEventAccessDecision: a user without access
// gets the explained 403, an identity kind the API does not serve gets the
// generic 403.
func TestScheduleAPI_DenialStatusAndBody(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	userID := tid("sched-deny-body-user")
	user := NewAuthenticatedUser(userID, "scheddenybody@test.com", "Deny Body", "member", "api")
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: userID, Email: user.Email(), DisplayName: user.DisplayName(), Role: "member", Status: "active",
	}))

	rec := doScheduleUserRequest(t, srv, user, http.MethodGet, projectID, "", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":{"code":"forbidden","message":"You don't have permission to access scheduled events in this project"}}`, rec.Body.String())

	rec = doScheduleUserRequest(t, srv, healthSchedServiceIdentity{}, http.MethodGet, projectID, "", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":{"code":"forbidden","message":"Insufficient permissions"}}`, rec.Body.String())
}

// healthSchedServiceIdentity is an identity kind the schedules API does
// not serve.
type healthSchedServiceIdentity struct{}

func (healthSchedServiceIdentity) ID() string   { return "hs-service" }
func (healthSchedServiceIdentity) Type() string { return "service" }
