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
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
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

func TestPauseZonePrefixedSchedules_ManyBatches(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx := context.Background()
	logs := authzHelperCaptureLogs(t)
	setZonePrefixBatchSize(t, 50)

	// 450 active rows across the project, 152 of them prefixed (every
	// third row, plus k = 199 and 399): four batches of 50 with a partial
	// last batch, mixed with plain rows the pass must leave alone.
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
	require.Greater(t, want, 3*50, "more than three batches of prefixed rows")

	runZonePrefixPass(t, srv)

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

// setZonePrefixBatchSize lowers the startup pass batch size for one test.
func setZonePrefixBatchSize(t *testing.T, n int) {
	t.Helper()
	prev := zonePrefixPassBatchSize
	zonePrefixPassBatchSize = n
	t.Cleanup(func() { zonePrefixPassBatchSize = prev })
}

// runZonePrefixPass runs the startup pass and fails if it does not return.
func runZonePrefixPass(t *testing.T, srv *Server) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.pauseZonePrefixedSchedules(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("pauseZonePrefixedSchedules did not terminate")
	}
}

// capturedErrors returns the captured ERROR records whose message starts
// with prefix.
func capturedErrors(t *testing.T, buf *bytes.Buffer, prefix string) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(bytes.NewReader(buf.Bytes()))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec map[string]any
		if json.Unmarshal(sc.Bytes(), &rec) != nil {
			continue
		}
		if rec["level"] == "ERROR" && strings.HasPrefix(fmt.Sprint(rec["msg"]), prefix) {
			out = append(out, rec)
		}
	}
	return out
}

// setupLegacyScheduleTest is setupScheduleTest on an in-memory SQLite
// database with a known DSN, plus a second raw connection to it, so a test
// can write created text the way a pre-normalization hub did.
func setupLegacyScheduleTest(t *testing.T) (*Server, store.Store, string, *sql.DB) {
	t.Helper()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	dsn := "file:legacy_" + name + "?mode=memory&cache=shared"
	st, err := newTestStoreAt(t, dsn)
	require.NoError(t, err)
	srv, s := testServerWithStore(t, st)
	db, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	srv, s, projectID := initScheduleTest(t, srv, s)
	return srv, s, projectID, db
}

// TestPauseZonePrefixedSchedules_LegacyCreatedText covers SQLite rows whose
// created column still holds local-zone text from a pre-normalization hub.
// That text breaks the created keyset (east of UTC it skips rows, west of
// UTC it stops advancing); the pass must not depend on it.
func TestPauseZonePrefixedSchedules_LegacyCreatedText(t *testing.T) {
	for _, zone := range []string{"Asia/Tokyo", "America/New_York"} {
		t.Run(zone, func(t *testing.T) {
			loc, err := time.LoadLocation(zone)
			require.NoError(t, err)
			srv, s, projectID, db := setupLegacyScheduleTest(t)
			setZonePrefixBatchSize(t, 3)
			logs := authzHelperCaptureLogs(t)

			// 13 rows, 7 prefixed: more than two batches of 3.
			const n = 13
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			isPrefixed := func(k int) bool { return k%2 == 0 }
			ids := make([]string, n)
			for k := 0; k < n; k++ {
				expr := "0 * * * *"
				if isPrefixed(k) {
					expr = zonePrefixedExprs[k%len(zonePrefixedExprs)]
				}
				ids[k] = seedSchedule(t, s, projectID, fmt.Sprintf("legacy-%02d", k), expr, store.ScheduleStatusActive).ID
				legacy := base.Add(time.Duration(k) * time.Minute).In(loc).String()
				_, err := db.Exec("UPDATE schedules SET created = ? WHERE id = ?", legacy, ids[k])
				require.NoError(t, err)
			}

			runZonePrefixPass(t, srv)

			want := 0
			for k := 0; k < n; k++ {
				got, err := s.GetSchedule(context.Background(), ids[k])
				require.NoError(t, err)
				if isPrefixed(k) {
					want++
					assert.Equal(t, store.ScheduleStatusPaused, got.Status, "prefixed row k=%d", k)
				} else {
					assert.Equal(t, store.ScheduleStatusActive, got.Status, "plain row k=%d", k)
				}
			}
			assert.Len(t, zonePrefixWarnings(t, logs), want)
		})
	}
}

// failingPauseStore fails UpdateScheduleStatus for one schedule ID.
type failingPauseStore struct {
	store.Store
	failID string
}

func (f *failingPauseStore) UpdateScheduleStatus(ctx context.Context, id, status string) error {
	if id == f.failID {
		return fmt.Errorf("injected pause failure")
	}
	return f.Store.UpdateScheduleStatus(ctx, id, status)
}

func TestPauseZonePrefixedSchedules_PauseFailureTerminates(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	setZonePrefixBatchSize(t, 1)
	logs := authzHelperCaptureLogs(t)

	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, seedSchedule(t, s, projectID, fmt.Sprintf("fail-%d", i), zonePrefixedExprs[0], store.ScheduleStatusActive).ID)
	}
	srv.store = &failingPauseStore{Store: s, failID: ids[1]}

	runZonePrefixPass(t, srv)

	for i, id := range ids {
		got, err := s.GetSchedule(context.Background(), id)
		require.NoError(t, err)
		if i == 1 {
			assert.Equal(t, store.ScheduleStatusActive, got.Status, "the row whose pause failed stays active")
		} else {
			assert.Equal(t, store.ScheduleStatusPaused, got.Status, "row %d", i)
		}
	}
	assert.Len(t, zonePrefixWarnings(t, logs), 3)
	require.Len(t, capturedErrors(t, logs, "schedule zone-prefix check: failed to pause schedule"), 1)
	summary := capturedErrors(t, logs, "schedule zone-prefix check: some prefixed schedules could not be paused")
	require.Len(t, summary, 1)
	assert.EqualValues(t, 1, summary[0]["count"])
}

// The store's prefix match is LIKE, which SQLite matches case-insensitively.
// Rows it returns that are not zone-prefixed must not stall the pass or hide
// real prefixed rows behind them.
func TestPauseZonePrefixedSchedules_LooseStoreMatchTerminates(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	setZonePrefixBatchSize(t, 1)

	var loose, real []string
	for i := 0; i < 3; i++ {
		loose = append(loose, seedSchedule(t, s, projectID, fmt.Sprintf("loose-%d", i), "cron_tz=Asia/Tokyo 0 9 * * *", store.ScheduleStatusActive).ID)
		real = append(real, seedSchedule(t, s, projectID, fmt.Sprintf("real-%d", i), zonePrefixedExprs[i%2], store.ScheduleStatusActive).ID)
	}

	runZonePrefixPass(t, srv)

	for _, id := range real {
		got, err := s.GetSchedule(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduleStatusPaused, got.Status)
	}
	for _, id := range loose {
		got, err := s.GetSchedule(context.Background(), id)
		require.NoError(t, err)
		assert.Equal(t, store.ScheduleStatusActive, got.Status, "not a zone prefix; the evaluator reports it as an invalid expression")
	}
}

// TestStartScheduler_PausesBeforeFirstTick checks the startup wiring: the
// pass runs before the scheduler starts, so the evaluator's first tick (tick
// 0, run immediately by Start) already sees the row paused.
func TestStartScheduler_PausesBeforeFirstTick(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	row := seedSchedule(t, s, projectID, "due-prefixed", zonePrefixedExprs[0], store.ScheduleStatusActive)

	srv.scheduler.MaxJitter = 0
	statusAtFirstTick := make(chan string, 1)
	evaluate := srv.evaluateSchedulesHandler()
	srv.scheduler.RegisterRecurring("schedule-evaluator", 1, func(ctx context.Context) {
		got, err := s.GetSchedule(ctx, row.ID)
		status := "error: "
		if err == nil {
			status = got.Status
		} else {
			status += err.Error()
		}
		select {
		case statusAtFirstTick <- status:
		default:
		}
		evaluate(ctx)
	})

	srv.startScheduler(ctx)
	var status string
	select {
	case status = <-statusAtFirstTick:
	case <-time.After(30 * time.Second):
		t.Fatal("the evaluator did not run")
	}
	srv.scheduler.Stop()

	assert.Equal(t, store.ScheduleStatusPaused, status, "paused before the evaluator's first tick")
	got, err := s.GetSchedule(context.Background(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusPaused, got.Status)
	assert.Equal(t, 0, got.RunCount)
	events, err := s.ListScheduledEvents(context.Background(), store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, events.Items, "no event is materialized")
}

// noopPauseStore reports success from UpdateScheduleStatus without writing,
// so a "paused" row stays active and keeps coming back from the store.
type noopPauseStore struct {
	store.Store
	calls int
}

func (n *noopPauseStore) UpdateScheduleStatus(context.Context, string, string) error {
	n.calls++
	return nil
}

func TestPauseZonePrefixedSchedules_NoopPauseTerminates(t *testing.T) {
	srv, s, projectID := setupScheduleTest(t)
	logs := authzHelperCaptureLogs(t)

	row := seedSchedule(t, s, projectID, "noop-pause", zonePrefixedExprs[0], store.ScheduleStatusActive)
	noop := &noopPauseStore{Store: s}
	srv.store = noop

	runZonePrefixPass(t, srv)

	assert.Equal(t, 1, noop.calls, "the row is handled once")
	assert.Len(t, zonePrefixWarnings(t, logs), 1, "one warning, not a stream")
	require.Len(t, capturedErrors(t, logs, "schedule zone-prefix check: stopped, the store returned no new rows"), 1)
	got, err := s.GetSchedule(context.Background(), row.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduleStatusActive, got.Status, "the no-op store did not write")
}

// A recurring dispatch_agent schedule whose name is held by a row left by a
// refused create-failure cleanup (ptone/scion#3701): each fire is recorded
// as an error with the actionable text, and each notifies the owner.
func TestExecuteSchedule_ErroredRowBlocksEveryFire(t *testing.T) {
	f := newSchedFire(t, "sched-blocked")
	ctx := context.Background()
	pub := NewChannelEventPublisher()
	t.Cleanup(pub.Close)
	f.srv.SetEventPublisher(pub)
	f.srv.scheduler = NewScheduler(f.store, slog.Default())
	f.srv.scheduler.RegisterEventHandler("dispatch_agent", f.srv.dispatchAgentEventHandler())

	errored := &store.Agent{ID: api.NewUUID(), Slug: "sched-blocked-r", Name: "sched-blocked-r", ProjectID: f.proj.ID}
	require.NoError(t, f.store.CreateAgent(ctx, errored))
	refused := createCleanupRefusedMessage(&DeleteRunMismatchError{RequestedRunID: "run-hub", CurrentRunID: "run-broker"})
	require.NoError(t, f.store.UpdateAgentStatus(ctx, errored.ID, store.AgentStatusUpdate{Phase: "error", Message: refused}))

	revision := withSessionRevision(store.ScheduledEvent{}, f.creator.ID)
	sched := &store.Schedule{
		ID: api.NewUUID(), ProjectID: f.proj.ID, Name: "blocked-nightly", CronExpr: "0 * * * *",
		EventType: "dispatch_agent", Payload: `{"agentName":"sched-blocked-r"}`,
		Status: store.ScheduleStatusActive, CreatedBy: f.creator.ID,
		InitiatorAttribution: revision.InitiatorAttribution,
		AuthorityCeiling:     revision.AuthorityCeiling,
	}
	require.NoError(t, f.store.CreateSchedule(ctx, sched))

	owner, unsub := pub.Subscribe("user." + f.creator.ID + ".notification")
	t.Cleanup(unsub)
	for i := 0; i < 2; i++ {
		sc, err := f.store.GetSchedule(ctx, sched.ID)
		require.NoError(t, err)
		f.srv.executeSchedule(ctx, *sc, time.Now())
	}

	got, err := f.store.GetSchedule(ctx, sched.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, got.ErrorCount)
	assert.Contains(t, got.LastRunError, `agent "sched-blocked-r" already exists in project in phase error`)
	assert.Contains(t, got.LastRunError, refused)
	assert.Contains(t, got.LastRunError, "delete the agent to resume this schedule")

	notifs, err := f.store.GetNotifications(ctx, store.SubscriberTypeUser, f.creator.ID, false)
	require.NoError(t, err)
	require.Len(t, notifs, 2, "one notification per blocked fire")
	for _, n := range notifs {
		assert.Equal(t, NotificationScheduleBlocked, n.Status)
		assert.Equal(t, errored.ID, n.AgentID)
		assert.Contains(t, n.Message, `Schedule "blocked-nightly" is blocked`)
	}
	assert.Len(t, owner, 2, "each notification published to the owner")

	// The row is untouched: still in phase error, not deleted or retried.
	row, err := f.store.GetAgent(ctx, errored.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", row.Phase)
}
