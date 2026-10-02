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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var zonePrefixedExprs = []string{
	"CRON_TZ=Asia/Tokyo 0 9 * * *",
	"TZ=Asia/Tokyo 0 9 * * *",
}

// seedSchedule writes a schedule row straight to the store, bypassing the
// handler's validation, the way a row created before UTC-only cron looks.
func seedSchedule(t *testing.T, s store.Store, projectID, name, cronExpr, status string) store.Schedule {
	t.Helper()
	next := time.Now().UTC().Add(-time.Minute)
	sc := store.Schedule{
		ID:        api.NewUUID(),
		ProjectID: projectID,
		Name:      name,
		CronExpr:  cronExpr,
		EventType: "message",
		Payload:   `{"agentName":"worker","message":"hello"}`,
		Status:    status,
		NextRunAt: &next,
	}
	require.NoError(t, s.CreateSchedule(context.Background(), &sc))
	return sc
}

func errorMessage(t *testing.T, body []byte) string {
	t.Helper()
	var resp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &resp), string(body))
	return resp.Error.Message
}

// zonePrefixWarnings returns the captured "schedule paused" warning records.
func zonePrefixWarnings(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec["level"] == "WARN" && strings.HasPrefix(fmt.Sprint(rec["msg"]), "schedule paused: cron zone prefixes are not supported") {
			out = append(out, rec)
		}
	}
	return out
}

func TestParseScheduleCron(t *testing.T) {
	for _, expr := range zonePrefixedExprs {
		_, err := parseScheduleCron(expr)
		assert.ErrorIs(t, err, errCronZonePrefix, expr)
	}

	// robfig's own prefix check is case-sensitive; a lowercase prefix is not
	// a zone prefix, just an invalid expression.
	_, err := parseScheduleCron("cron_tz=Asia/Tokyo 0 9 * * *")
	require.Error(t, err)
	assert.NotErrorIs(t, err, errCronZonePrefix)

	// Descriptors have never been enabled at the hub parse sites; that is
	// unchanged.
	for _, expr := range []string{"@every 1h", "@daily"} {
		_, err := parseScheduleCron(expr)
		require.Error(t, err, expr)
		assert.NotErrorIs(t, err, errCronZonePrefix, expr)
		assert.Contains(t, err.Error(), "parser does not accept descriptors", expr)
	}

	// Plain expressions evaluate in UTC whatever the zone of the time passed
	// to Next (robfig would otherwise use that zone for a time.Local spec).
	sched, err := parseScheduleCron("0 9 * * *")
	require.NoError(t, err)
	for _, zone := range []string{"UTC", "Asia/Tokyo", "Asia/Kathmandu", "America/New_York"} {
		loc, err := time.LoadLocation(zone)
		require.NoError(t, err)
		from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).In(loc)
		next := sched.Next(from).UTC()
		assert.Equal(t, time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC), next, zone)
	}
}

func TestSchedule_CreateRejectsZonePrefix(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	for i, expr := range zonePrefixedExprs {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
			CreateScheduleRequest{
				Name: fmt.Sprintf("prefixed-%d", i), CronExpr: expr, EventType: "message",
				AgentName: "worker", Message: "hello",
			})
		require.Equal(t, http.StatusBadRequest, rec.Code, expr)
		assert.Equal(t, cronZonePrefixMessage, errorMessage(t, rec.Body.Bytes()), expr)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+projectID+"/schedules", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp ListSchedulesResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 0, resp.TotalCount, "nothing is stored for a rejected create")
}

func TestSchedule_CreateDescriptorsStillRejected(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	for i, expr := range []string{"@every 1h", "@daily"} {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
			CreateScheduleRequest{
				Name: fmt.Sprintf("descriptor-%d", i), CronExpr: expr, EventType: "message",
				AgentName: "worker", Message: "hello",
			})
		require.Equal(t, http.StatusBadRequest, rec.Code, expr)
		msg := errorMessage(t, rec.Body.Bytes())
		assert.Contains(t, msg, "invalid cron expression: parser does not accept descriptors", expr)
	}
}

func TestSchedule_CreatePlainEvaluatesInUTC(t *testing.T) {
	srv, _, projectID := setupScheduleTest(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+projectID+"/schedules",
		CreateScheduleRequest{
			Name: "plain", CronExpr: "17 9 * * *", EventType: "message",
			AgentName: "worker", Message: "hello",
		})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created store.Schedule
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&created))
	require.NotNil(t, created.NextRunAt)
	next := created.NextRunAt.UTC()
	assert.Equal(t, 9, next.Hour())
	assert.Equal(t, 17, next.Minute())
}

func TestSchedule_UpdateRejectsZonePrefix(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	plain := seedSchedule(t, s, projectID, "plain", "0 * * * *", store.ScheduleStatusActive)

	for _, expr := range zonePrefixedExprs {
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/projects/"+projectID+"/schedules/"+plain.ID,
			UpdateScheduleRequest{CronExpr: expr})
		require.Equal(t, http.StatusBadRequest, rec.Code, expr)
		assert.Equal(t, cronZonePrefixMessage, errorMessage(t, rec.Body.Bytes()), expr)
	}

	got, err := s.GetSchedule(context.Background(), plain.ID)
	require.NoError(t, err)
	assert.Equal(t, "0 * * * *", got.CronExpr, "a rejected update stores nothing")
}

func TestSchedule_PrefixedRowEnableResume(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()

	for i, expr := range zonePrefixedExprs {
		row := seedSchedule(t, s, projectID, fmt.Sprintf("legacy-%d", i), expr, store.ScheduleStatusPaused)
		base := "/api/v1/projects/" + projectID + "/schedules/" + row.ID

		// Resume: 400 with the UTC message, not 500.
		rec := doRequest(t, srv, http.MethodPost, base+"/resume", nil)
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, cronZonePrefixMessage, errorMessage(t, rec.Body.Bytes()))

		// Enable through update: 400 too.
		rec = doRequest(t, srv, http.MethodPatch, base, UpdateScheduleRequest{Status: store.ScheduleStatusActive})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		assert.Equal(t, cronZonePrefixMessage, errorMessage(t, rec.Body.Bytes()))

		got, err := s.GetSchedule(ctx, row.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduleStatusPaused, got.Status)

		// A metadata-only update (name, or the unchanged expression resent)
		// does not re-parse and is still allowed.
		rec = doRequest(t, srv, http.MethodPatch, base,
			UpdateScheduleRequest{Name: fmt.Sprintf("renamed-%d", i), CronExpr: expr})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Editing the expression to UTC and enabling in one request works.
		rec = doRequest(t, srv, http.MethodPatch, base,
			UpdateScheduleRequest{CronExpr: "0 0 * * *", Status: store.ScheduleStatusActive})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err = s.GetSchedule(ctx, row.ID)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduleStatusActive, got.Status)
		assert.Equal(t, "0 0 * * *", got.CronExpr)
	}

	// Edit to UTC, then resume as a separate step.
	row := seedSchedule(t, s, projectID, "legacy-resume", zonePrefixedExprs[0], store.ScheduleStatusPaused)
	base := "/api/v1/projects/" + projectID + "/schedules/" + row.ID
	rec := doRequest(t, srv, http.MethodPatch, base, UpdateScheduleRequest{CronExpr: "0 0 * * *"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, base+"/resume", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestPauseZonePrefixedSchedules_SeededRow(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	logs := authzHelperCaptureLogs(t)

	prefixed := seedSchedule(t, s, projectID, "prefixed", zonePrefixedExprs[0], store.ScheduleStatusActive)
	plain := seedSchedule(t, s, projectID, "plain", "0 * * * *", store.ScheduleStatusActive)
	pausedPlain := seedSchedule(t, s, projectID, "paused-plain", "0 * * * *", store.ScheduleStatusPaused)

	srv.pauseZonePrefixedSchedules(ctx)

	got, err := s.GetSchedule(ctx, prefixed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status)
	got, err = s.GetSchedule(ctx, plain.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, got.Status)
	got, err = s.GetSchedule(ctx, pausedPlain.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status)

	warnings := zonePrefixWarnings(t, logs)
	require.Len(t, warnings, 1)
	assert.Equal(t, prefixed.ID, warnings[0]["schedule_id"])
	assert.Equal(t, projectID, warnings[0]["project_id"])
	assert.Equal(t, zonePrefixedExprs[0], warnings[0]["cron_expr"])

	// Idempotent: a second start finds nothing to do.
	logs.Reset()
	srv.pauseZonePrefixedSchedules(ctx)
	assert.Empty(t, zonePrefixWarnings(t, logs))
}

func TestPauseZonePrefixedSchedules_ManyPages(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	logs := authzHelperCaptureLogs(t)

	// 450 active rows, more than two pages of 200. Rows are listed newest
	// first; k is the 0-based position in list order. Every third row is
	// prefixed, plus the rows at list positions 200 and 400 (k = 199, 399),
	// the last row of pages one and two, so the pass pauses each page's
	// boundary row before fetching the next page.
	const n = 450
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	isPrefixed := func(k int) bool { return k%3 == 2 || k == 199 || k == 399 }
	ids := make([]string, n) // indexed by k
	want := 0
	for k := 0; k < n; k++ {
		expr := "0 * * * *"
		if isPrefixed(k) {
			expr = zonePrefixedExprs[k%2]
			want++
		}
		sc := store.Schedule{
			ID:        api.NewUUID(),
			ProjectID: projectID,
			Name:      fmt.Sprintf("row-%03d", k),
			CronExpr:  expr,
			EventType: "message",
			Payload:   `{"agentName":"worker","message":"hello"}`,
			Status:    store.ScheduleStatusActive,
			CreatedAt: base.Add(time.Duration(n-k) * time.Second),
		}
		require.NoError(t, s.CreateSchedule(ctx, &sc))
		ids[k] = sc.ID
	}

	// Sanity: the boundary rows really are the last row of pages one and two.
	page1, err := s.ListSchedules(ctx, store.ScheduleFilter{Status: store.ScheduleStatusActive}, store.ListOptions{Limit: 200})
	require.NoError(t, err)
	require.Len(t, page1.Items, 200)
	require.Equal(t, ids[199], page1.Items[199].ID)

	srv.pauseZonePrefixedSchedules(ctx) // returns, so paging terminated

	for k := 0; k < n; k++ {
		got, err := s.GetSchedule(ctx, ids[k])
		require.NoError(t, err)
		if isPrefixed(k) {
			assert.Equal(t, store.ScheduleStatusPaused, got.Status, "prefixed row k=%d", k)
		} else {
			assert.Equal(t, store.ScheduleStatusActive, got.Status, "plain row k=%d", k)
		}
	}
	assert.Len(t, zonePrefixWarnings(t, logs), want)

	logs.Reset()
	srv.pauseZonePrefixedSchedules(ctx)
	assert.Empty(t, zonePrefixWarnings(t, logs))
}

func TestExecuteSchedule_ZonePrefixBackstop(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	logs := authzHelperCaptureLogs(t)

	// A row that reaches the evaluator after the startup pass (for example
	// inserted directly into the DB).
	row := seedSchedule(t, s, projectID, "late-prefixed", zonePrefixedExprs[1], store.ScheduleStatusActive)
	srv.executeSchedule(ctx, row, time.Now().UTC())

	got, err := s.GetSchedule(ctx, row.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status)
	assert.Equal(t, 0, got.RunCount, "the row is not run")
	assert.Equal(t, 0, got.ErrorCount, "the row is not errored")
	assert.Empty(t, got.LastRunError)

	events, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, events.Items, "no event is materialized")

	warnings := zonePrefixWarnings(t, logs)
	require.Len(t, warnings, 1)
	assert.Equal(t, row.ID, warnings[0]["schedule_id"])

	// Once paused it is no longer due, so the evaluator never sees it again.
	due, err := s.ListDueSchedules(ctx, time.Now().UTC())
	require.NoError(t, err)
	for _, d := range due {
		assert.NotEqual(t, row.ID, d.ID)
	}
}

func TestSchedule_ListCursorPaging(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)

	created := make([]string, 0, 5)
	for i := 0; i < 5; i++ {
		created = append(created, seedSchedule(t, s, projectID, fmt.Sprintf("page-%d", i), "0 * * * *", store.ScheduleStatusActive).ID)
	}

	listPath := "/api/v1/projects/" + projectID + "/schedules"
	var seen []string
	cursor := ""
	for i := 0; i < 10; i++ {
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		rec := doRequest(t, srv, http.MethodGet, listPath+"?"+q.Encode(), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListSchedulesResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.Equal(t, 5, resp.TotalCount)
		for _, sc := range resp.Schedules {
			seen = append(seen, sc.ID)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	assert.ElementsMatch(t, created, seen, "every row once, no page repeated")
	assert.Len(t, seen, 5)

	for _, bad := range []string{"not-a-cursor!!", created[0]} {
		rec := doRequest(t, srv, http.MethodGet, listPath+"?cursor="+url.QueryEscape(bad), nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "cursor %q: %s", bad, rec.Body.String())
	}
}
