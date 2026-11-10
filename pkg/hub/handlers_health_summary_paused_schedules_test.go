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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pauseHealthSchedule pauses sc with the given status reason ("" is a
// user pause).
func pauseHealthSchedule(t *testing.T, s store.Store, sc *store.Schedule, reason string) {
	t.Helper()
	require.NoError(t, s.UpdateScheduleStatus(context.Background(), sc.ID, store.ScheduleStatusPaused, reason))
}

func pausedScheduleIDs(sec *HealthSummarySchedules) []string {
	ids := []string{}
	for _, p := range sec.Paused {
		ids = append(ids, p.ID)
	}
	return ids
}

func healthSummaryAsAdmin(t *testing.T, srv *Server) (HealthSummaryResponse, string) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp, rr.Body.String()
}

// A schedule paused by the hub is listed with its reason code and a fixed
// attention sentence per reason; an unknown stored reason is returned as
// "other" and never as the stored text. A user pause (empty reason) and
// an active schedule are not listed.
func TestHandleHealthSummary_HubPausedSchedulesListed(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "paused-alpha")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)

	creator := createHealthSchedule(t, s, p.ID, "creator-gone", ranAt)
	pauseHealthSchedule(t, s, creator, store.ScheduleStatusReasonCreatorDeleted)
	zone := createHealthSchedule(t, s, p.ID, "zoned", ranAt)
	pauseHealthSchedule(t, s, zone, store.ScheduleStatusReasonCronZoneUnsupported)
	later := createHealthSchedule(t, s, p.ID, "later-reason", ranAt)
	pauseHealthSchedule(t, s, later, "stored_reason_text_not_for_summary")
	user := createHealthSchedule(t, s, p.ID, "user-paused", ranAt, "an error")
	pauseHealthSchedule(t, s, user, store.ScheduleStatusReasonNone)
	createHealthSchedule(t, s, p.ID, "active", ranAt)

	resp, body := healthSummaryAsAdmin(t, srv)
	require.NotNil(t, resp.Schedules)
	assert.Equal(t, 3, resp.Schedules.PausedTotal)
	assert.False(t, resp.Schedules.PausedTruncated)
	assert.False(t, resp.Schedules.PausedScanLimited)
	assert.ElementsMatch(t, []string{creator.ID, zone.ID, later.ID}, pausedScheduleIDs(resp.Schedules))
	assert.Equal(t, 0, resp.Schedules.Total, "a paused schedule is not a failing one")

	byID := map[string]HealthPausedSchedule{}
	for _, row := range resp.Schedules.Paused {
		require.NotNil(t, row.UpdatedAt)
		row.UpdatedAt = nil
		byID[row.ID] = row
	}
	assert.Equal(t, HealthPausedSchedule{
		ID: creator.ID, Name: "creator-gone", ProjectID: p.ID, ProjectSlug: "paused-alpha",
		EventType: "message", Target: "worker", Reason: "creator_deleted",
	}, byID[creator.ID])
	assert.Equal(t, "cron_zone_unsupported", byID[zone.ID].Reason)
	assert.Equal(t, "other", byID[later.ID].Reason)

	item := func(sc *store.Schedule, name, msg string) HealthAttentionItem {
		return HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
			Subject: HealthAttentionSubject{Type: HealthSubjectSchedule, ID: sc.ID, Name: name, ProjectID: p.ID},
			Message: msg,
		}
	}
	assert.Contains(t, resp.Attention, item(creator, "creator-gone",
		"Scheduled event paused-alpha / creator-gone was paused by the hub: the agent or user it runs under was deleted"))
	assert.Contains(t, resp.Attention, item(zone, "zoned",
		"Scheduled event paused-alpha / zoned was paused by the hub: its cron expression has a zone prefix, which is not supported: set its time zone field instead"))
	assert.Contains(t, resp.Attention, item(later, "later-reason",
		"Scheduled event paused-alpha / later-reason was paused by the hub: a reason this hub does not recognize"))
	for _, it := range resp.Attention {
		assert.NotEqual(t, user.ID, it.Subject.ID, "a user pause is not an attention item")
	}
	assert.NotContains(t, body, "user-paused")
	assert.NotContains(t, body, "stored_reason_text_not_for_summary")
	assert.NotContains(t, body, "message-body-not-for-summary")
	assert.Equal(t, HealthStatusHealthy, resp.Status, "a paused schedule does not change the status")
}

// Hub-paused schedules follow the same per-project read rule as failing
// ones: health.read alone sees none, a project member sees that project's
// only, a hub admin sees all.
func TestHandleHealthSummary_HubPausedSchedulesFollowScheduleReadAccess(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	alpha := createHealthScheduleProject(t, s, "paused-acc-alpha")
	beta := createHealthScheduleProject(t, s, "paused-acc-beta")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	inAlpha := createHealthSchedule(t, s, alpha.ID, "pacc-alpha", ranAt)
	pauseHealthSchedule(t, s, inAlpha, store.ScheduleStatusReasonCreatorDeleted)
	inBeta := createHealthSchedule(t, s, beta.ID, "pacc-beta", ranAt)
	pauseHealthSchedule(t, s, inBeta, store.ScheduleStatusReasonCreatorDeleted)

	healthOnly := healthSummaryRoleUser(t, s, "hs-paused-health-only", []string{"hub.health.read"})
	alphaMember := healthSummaryRoleUser(t, s, "hs-paused-alpha-member", []string{"hub.health.read"})
	grantUserProjectAccess(t, s, alphaMember.ID, alpha.ID)

	t.Run("health.read only: none", func(t *testing.T) {
		resp, body := healthSummaryAsUser(t, srv, healthOnly)
		require.NotNil(t, resp.Schedules)
		assert.Equal(t, 0, resp.Schedules.PausedTotal)
		assert.Empty(t, resp.Schedules.Paused)
		assert.NotContains(t, body, "pacc-alpha")
		assert.NotContains(t, body, "pacc-beta")
		assert.Equal(t, HealthStatusHealthy, resp.Status)
	})

	t.Run("member of alpha: alpha only", func(t *testing.T) {
		resp, body := healthSummaryAsUser(t, srv, alphaMember)
		require.NotNil(t, resp.Schedules)
		assert.Equal(t, 1, resp.Schedules.PausedTotal)
		assert.Equal(t, []string{inAlpha.ID}, pausedScheduleIDs(resp.Schedules))
		assert.NotContains(t, body, inBeta.ID)
		assert.NotContains(t, body, "pacc-beta")
		assert.Equal(t, HealthStatusHealthy, resp.Status)
	})

	t.Run("hub admin: both", func(t *testing.T) {
		resp, _ := healthSummaryAsAdmin(t, srv)
		require.NotNil(t, resp.Schedules)
		assert.ElementsMatch(t, []string{inAlpha.ID, inBeta.ID}, pausedScheduleIDs(resp.Schedules))
	})
}

// Past the row cap the paused total stays true and one item covers the
// rest.
func TestHandleHealthSummary_HubPausedSchedulesCap(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "paused-cap")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	for _, n := range []string{"pcap-a", "pcap-b", "pcap-c"} {
		pauseHealthSchedule(t, s, createHealthSchedule(t, s, p.ID, n, ranAt), store.ScheduleStatusReasonCreatorDeleted)
	}
	prevLimit, prevPage := healthSummaryScheduleLimit, healthSummarySchedulePageSize
	healthSummaryScheduleLimit, healthSummarySchedulePageSize = 2, 1
	t.Cleanup(func() { healthSummaryScheduleLimit, healthSummarySchedulePageSize = prevLimit, prevPage })

	resp, _ := healthSummaryAsAdmin(t, srv)
	require.NotNil(t, resp.Schedules)
	assert.Equal(t, 3, resp.Schedules.PausedTotal)
	assert.True(t, resp.Schedules.PausedTruncated)
	assert.False(t, resp.Schedules.PausedScanLimited)
	assert.Len(t, resp.Schedules.Paused, 2)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
		Message: "1 more scheduled event was paused by the hub",
	})
}

// Past the scan limit only the first rows in store order are read: the
// paused list says so, its total is a lower bound and one item notes the
// rest.
func TestHandleHealthSummary_HubPausedSchedulesScanLimited(t *testing.T) {
	srv, s := testServer(t)
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "paused-scan")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	for _, n := range []string{"pscan-a", "pscan-b", "pscan-c"} {
		pauseHealthSchedule(t, s, createHealthSchedule(t, s, p.ID, n, ranAt), store.ScheduleStatusReasonCronZoneUnsupported)
	}
	prevScan, prevPage := healthSummaryScheduleScanLimit, healthSummarySchedulePageSize
	healthSummaryScheduleScanLimit, healthSummarySchedulePageSize = 2, 1
	t.Cleanup(func() { healthSummaryScheduleScanLimit, healthSummarySchedulePageSize = prevScan, prevPage })

	resp, _ := healthSummaryAsAdmin(t, srv)
	require.NotNil(t, resp.Schedules)
	assert.True(t, resp.Schedules.PausedScanLimited)
	assert.True(t, resp.Schedules.PausedTruncated)
	assert.False(t, resp.Schedules.ScanLimited, "the failing list has its own scan")
	assert.Equal(t, 2, resp.Schedules.PausedTotal)
	assert.Len(t, resp.Schedules.Paused, 2)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
		Message: "More scheduled events may have been paused by the hub",
	})
}

// pausedListFailStore fails ListSchedules only for the hub-paused filter.
type pausedListFailStore struct{ store.Store }

func (f pausedListFailStore) ListSchedules(ctx context.Context, filter store.ScheduleFilter, opts store.ListOptions) (*store.ListResult[store.Schedule], error) {
	if filter.SystemPausedOnly {
		return nil, errors.New("paused list failed: dial tcp 10.0.0.9:5432")
	}
	return f.Store.ListSchedules(ctx, filter, opts)
}

// A failed hub-paused read marks only the paused list as not reported:
// the failing list is still listed, paused is null with
// paused_unavailable true, one warning says so, and no store error text
// is returned.
func TestHandleHealthSummary_HubPausedSchedulesNotReported(t *testing.T) {
	srv, s, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) pausedListFailStore {
		return pausedListFailStore{Store: inner}
	})
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "paused-fail")
	failing := createHealthSchedule(t, s, p.ID, "still-failing", time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC), "e")

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var raw struct {
		Schedules map[string]json.RawMessage `json:"schedules"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &raw))
	require.NotNil(t, raw.Schedules, "the section stays reported")
	assert.Equal(t, "null", string(raw.Schedules["paused"]))
	assert.Equal(t, "true", string(raw.Schedules["paused_unavailable"]))
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Schedules)
	assert.Equal(t, 1, resp.Schedules.Total)
	assert.Equal(t, []string{failing.ID}, failingScheduleIDs(resp.Schedules))
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionSchedule,
		Subject: HealthAttentionSubject{Type: HealthSubjectSchedules},
		Message: "Hub-paused scheduled event data not available",
	})
	for _, it := range resp.Attention {
		assert.NotEqual(t, "Scheduled event data not available", it.Message, "the failing list was read")
	}
	assert.NotContains(t, rr.Body.String(), "10.0.0.9")
}

// pausedFilterIgnoredStore drops SystemPausedOnly and Status from the
// hub-paused schedule list, as a store that ignored both would, so every
// non-deleted schedule reaches the hub's re-check.
type pausedFilterIgnoredStore struct{ store.Store }

func (f pausedFilterIgnoredStore) ListSchedules(ctx context.Context, filter store.ScheduleFilter, opts store.ListOptions) (*store.ListResult[store.Schedule], error) {
	if filter.SystemPausedOnly {
		filter.SystemPausedOnly = false
		filter.Status = ""
	}
	return f.Store.ListSchedules(ctx, filter, opts)
}

// When the store ignores the paused filter, the hub's per-row re-check
// still leaves out a user-paused schedule (empty reason) and an active
// schedule that carries a reason.
func TestHandleHealthSummary_HubPausedSchedulesRecheckedPerRow(t *testing.T) {
	srv, s, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) pausedFilterIgnoredStore {
		return pausedFilterIgnoredStore{Store: inner}
	})
	tickHubInstance(t, srv)
	p := createHealthScheduleProject(t, s, "paused-recheck")
	ranAt := time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC)
	hub := createHealthSchedule(t, s, p.ID, "recheck-hub", ranAt)
	pauseHealthSchedule(t, s, hub, store.ScheduleStatusReasonCreatorDeleted)
	pauseHealthSchedule(t, s, createHealthSchedule(t, s, p.ID, "recheck-user", ranAt), store.ScheduleStatusReasonNone)
	activeWithReason := createHealthSchedule(t, s, p.ID, "recheck-active", ranAt)
	require.NoError(t, s.UpdateScheduleStatus(context.Background(), activeWithReason.ID,
		store.ScheduleStatusActive, store.ScheduleStatusReasonCreatorDeleted))

	resp, body := healthSummaryAsAdmin(t, srv)
	require.NotNil(t, resp.Schedules)
	assert.Equal(t, 1, resp.Schedules.PausedTotal)
	assert.Equal(t, []string{hub.ID}, pausedScheduleIDs(resp.Schedules))
	assert.NotContains(t, body, "recheck-user")
	assert.NotContains(t, body, "recheck-active")
}
