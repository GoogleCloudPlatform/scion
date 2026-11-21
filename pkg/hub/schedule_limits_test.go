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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCronMinGap(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want time.Duration
	}{
		{"* * * * *", time.Minute},
		{"*/5 * * * *", 5 * time.Minute},
		{"*/7 * * * *", 4 * time.Minute}, // :56 -> :00 of the next hour
		{"0,1 * * * *", time.Minute},
		{"0 * * * *", time.Hour},
		{"0 */6 * * *", 6 * time.Hour},
		{"0 9 * * 1-5", 24 * time.Hour},
		{"0,55 0,23 * * *", 5 * time.Minute}, // 23:55 -> 00:00
		{"30 2 1 1 *", 24 * time.Hour},       // lower bound: day fields ignored
	} {
		sched, err := parseScheduleCron(tc.expr)
		require.NoError(t, err, tc.expr)
		assert.Equal(t, tc.want, cronMinGap(sched), tc.expr)
	}
}

func createScheduleReq(name, cron string) CreateScheduleRequest {
	return CreateScheduleRequest{Name: name, CronExpr: cron, EventType: "message", AgentName: "all", Message: "ping"}
}

// TestSchedule_MinIntervalEnforced: a cron expression that can fire more
// often than server.hub.schedules.min_interval is refused on create, on a
// cron change and on resume.
func TestSchedule_MinIntervalEnforced(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	srv.config.ScheduleMinInterval = 5 * time.Minute
	base := "/api/v1/projects/" + projectID + "/schedules"

	rec := doRequest(t, srv, http.MethodPost, base, createScheduleReq("every-minute", "* * * * *"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "min_interval")

	rec = doRequest(t, srv, http.MethodPost, base, createScheduleReq("burst", "0,1 * * * *"))
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	rec = doRequest(t, srv, http.MethodPost, base, createScheduleReq("every-five", "*/5 * * * *"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))

	rec = doRequest(t, srv, http.MethodPatch, base+"/"+created.ID, UpdateScheduleRequest{CronExpr: "* * * * *"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "min_interval")

	// A paused schedule from before the limit cannot be resumed as is.
	rec = doRequest(t, srv, http.MethodPost, base+"/"+created.ID+"/pause", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	stored, err := s.GetSchedule(context.Background(), created.ID)
	require.NoError(t, err)
	stored.CronExpr = "* * * * *"
	require.NoError(t, s.UpdateSchedule(context.Background(), stored, store.ScheduleFieldMask{CronExpr: true}, stored.AuthorizationRevision, stored.AuthorizationRevision != 0, nil))
	rec = doRequest(t, srv, http.MethodPost, base+"/"+created.ID+"/resume", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "min_interval")
	rec = doRequest(t, srv, http.MethodPatch, base+"/"+created.ID, UpdateScheduleRequest{Status: store.ScheduleStatusActive})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "min_interval")

	// Zero turns the check off.
	srv.config.ScheduleMinInterval = 0
	rec = doRequest(t, srv, http.MethodPost, base, createScheduleReq("every-minute-ok", "* * * * *"))
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

// TestSchedule_ProjectQuota: server.hub.schedules.max_active_per_project
// caps the active recurring schedules of every kind in one project, on
// create, enable and resume.
func TestSchedule_ProjectQuota(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)
	srv.config.ScheduleMaxActivePerProject = 2
	base := "/api/v1/projects/" + projectID + "/schedules"

	var ids []string
	for _, name := range []string{"q1", "q2"} {
		rec := doRequest(t, srv, http.MethodPost, base, createScheduleReq(name, "0 * * * *"))
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		var sc store.Schedule
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&sc))
		ids = append(ids, sc.ID)
	}

	rec := doRequest(t, srv, http.MethodPost, base, createScheduleReq("q3", "0 * * * *"))
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, base, CreateScheduleRequest{
		Name: "q3-dispatch", CronExpr: "0 * * * *", EventType: "dispatch_agent", AgentName: "worker-q3",
	})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, "dispatch_agent schedules count against the same cap: %s", rec.Body.String())

	// Pausing one frees a slot; resuming it while the slot is taken is refused.
	rec = doRequest(t, srv, http.MethodPost, base+"/"+ids[0]+"/pause", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, base, createScheduleReq("q3", "0 * * * *"))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, base+"/"+ids[0]+"/resume", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPatch, base+"/"+ids[0], UpdateScheduleRequest{Status: store.ScheduleStatusActive})
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())

	// An edit of an already-active schedule is not refused by the cap.
	rec = doRequest(t, srv, http.MethodPatch, base+"/"+ids[1], UpdateScheduleRequest{CronExpr: "30 * * * *"})
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
