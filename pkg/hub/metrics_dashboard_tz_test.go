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

package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	_ "time/tzdata" // the zone cases below must not depend on the host's zoneinfo

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func chicago(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	require.NoError(t, err)
	return loc
}

// TestDailyBucketsUseViewerTimeZone: an evening in US Central is already the
// next day in UTC; bucketed in the viewer's zone it stays on that evening's date.
func TestDailyBucketsUseViewerTimeZone(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)

	// One epoch starting 10:00 CDT Sep 29; running totals at 11:00 CDT (1),
	// 20:00 CDT (3) and 00:30 CDT Sep 30 (7) — the last two are Sep 30 in UTC.
	epoch := time.Date(2026, 9, 29, 15, 0, 0, 0, time.UTC)
	point := func(end time.Time, v int64) *monitoringpb.Point {
		return &monitoringpb.Point{
			Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(epoch), EndTime: timestamppb.New(end)},
			Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: v}},
		}
	}
	client.seriesByFilter[`metric.type = "`+metricPrefix+telemetrycontract.MetricAPICalls+`"`] = []*monitoringpb.TimeSeries{{
		Metric:     &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricAPICalls, Labels: map[string]string{"model": "m"}},
		MetricKind: googlemetricpb.MetricDescriptor_CUMULATIVE,
		Points: []*monitoringpb.Point{
			point(time.Date(2026, 9, 29, 16, 0, 0, 0, time.UTC), 1),
			point(time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC), 3),
			point(time.Date(2026, 9, 30, 5, 30, 0, 0, time.UTC), 7),
		},
	}}
	m := telemetrycontract.MetricAPICalls
	start, end := epoch.Add(-time.Hour), time.Date(2026, 9, 30, 6, 0, 0, 0, time.UTC)

	utc, err := svc.queryDailyTimeSeries(context.Background(), m, start, end, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []TimeSeriesPoint{{Timestamp: "2026-09-29", Value: 1}, {Timestamp: "2026-09-30", Value: 6}}, utc)

	local, err := svc.queryDailyTimeSeries(context.Background(), m, start, end, nil, chicago(t))
	require.NoError(t, err)
	assert.Equal(t, []TimeSeriesPoint{{Timestamp: "2026-09-29", Value: 3}, {Timestamp: "2026-09-30", Value: 4}}, local)

	grouped, err := svc.queryGroupedTimeSeries(context.Background(), m, "metric.labels.model", start, end, nil, chicago(t))
	require.NoError(t, err)
	require.Len(t, grouped, 1)
	assert.Equal(t, local, grouped[0].Points)

	// Active Agents per Day (queryDailyUniqueCount): agent a1 is seen at
	// 11:00 CDT Sep 29, a2 at 20:00 CDT Sep 29 (Sep 30 in UTC) and a3 at
	// 00:30 CDT Sep 30.
	sessions := telemetrycontract.MetricSessionCount
	agent := func(name string, end time.Time) *monitoringpb.TimeSeries {
		return &monitoringpb.TimeSeries{
			Metric:     &googlemetricpb.Metric{Type: metricPrefix + sessions, Labels: map[string]string{telemetrycontract.AgentLabel: name}},
			MetricKind: googlemetricpb.MetricDescriptor_CUMULATIVE,
			Points:     []*monitoringpb.Point{point(end, 1)},
		}
	}
	client.seriesByFilter[`metric.type = "`+metricPrefix+sessions+`"`] = []*monitoringpb.TimeSeries{
		agent("a1", time.Date(2026, 9, 29, 16, 0, 0, 0, time.UTC)),
		agent("a2", time.Date(2026, 9, 30, 1, 0, 0, 0, time.UTC)),
		agent("a3", time.Date(2026, 9, 30, 5, 30, 0, 0, time.UTC)),
	}
	groupBy := "metric.labels." + telemetrycontract.AgentLabel
	utcAgents, err := svc.queryDailyUniqueCount(context.Background(), sessions, groupBy, start, end, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []TimeSeriesPoint{{Timestamp: "2026-09-29", Value: 1}, {Timestamp: "2026-09-30", Value: 2}}, utcAgents)
	localAgents, err := svc.queryDailyUniqueCount(context.Background(), sessions, groupBy, start, end, nil, chicago(t))
	require.NoError(t, err)
	assert.Equal(t, []TimeSeriesPoint{{Timestamp: "2026-09-29", Value: 2}, {Timestamp: "2026-09-30", Value: 1}}, localAgents)
}

// TestDailyBucketsAcrossDSTFallBack: US Central falls back on 2026-11-01, so
// that day runs 05:00Z to 06:00Z the next day (25 hours).
func TestDailyBucketsAcrossDSTFallBack(t *testing.T) {
	loc := chicago(t)
	assert.Equal(t, "2026-10-31", dayKey(time.Date(2026, 11, 1, 4, 59, 0, 0, time.UTC), loc))
	assert.Equal(t, "2026-11-01", dayKey(time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC), loc))
	assert.Equal(t, "2026-11-01", dayKey(time.Date(2026, 11, 2, 5, 59, 0, 0, time.UTC), loc))
	assert.Equal(t, "2026-11-02", dayKey(time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC), loc))
}

// TestServeMetricsDashboardBucketsInRequestedZone drives the HTTP handler so
// the tz query parameter -> WithLocation wiring is covered end to end: the
// day-bucketed views come back bucketed, and labelled, in the requested zone,
// and fall back to UTC for an absent or rejected zone.
func TestServeMetricsDashboardBucketsInRequestedZone(t *testing.T) {
	// 01:00Z three days ago is the previous evening in US Central.
	seen := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -3).Add(time.Hour)
	utcDay := seen.Format("2006-01-02")
	chicagoDay := seen.In(chicago(t)).Format("2006-01-02")
	require.NotEqual(t, utcDay, chicagoDay)

	serve := func(t *testing.T, query string) *httptest.ResponseRecorder {
		t.Helper()
		client := newFakeMetricsClient()
		client.seriesByFilter[`metric.type = "`+metricPrefix+telemetrycontract.MetricSessionCount+`"`] = []*monitoringpb.TimeSeries{{
			Metric:     &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricSessionCount, Labels: map[string]string{telemetrycontract.AgentLabel: "a1"}},
			MetricKind: googlemetricpb.MetricDescriptor_CUMULATIVE,
			Points: []*monitoringpb.Point{{
				Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(seen.Add(-time.Minute)), EndTime: timestamppb.New(seen)},
				Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 1}},
			}},
		}}
		client.seriesByFilter[`metric.type = "`+metricPrefix+telemetrycontract.MetricAPICalls+`"`] = []*monitoringpb.TimeSeries{{
			Metric:     &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricAPICalls, Labels: map[string]string{"model": "m", "harness": "h"}},
			MetricKind: googlemetricpb.MetricDescriptor_CUMULATIVE,
			Points: []*monitoringpb.Point{{
				Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(seen.Add(-time.Minute)), EndTime: timestamppb.New(seen)},
				Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 2}},
			}},
		}}
		s := &Server{metricsDashboard: newContractTestService(client)}
		rec := httptest.NewRecorder()
		s.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, "/api/v1/metrics/dashboard?"+query, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec
	}

	for _, tc := range []struct {
		tz, wantZone, wantDay string
	}{
		{"America%2FChicago", "America/Chicago", chicagoDay},
		{"", "UTC", utcDay},
		{"Local", "UTC", utcDay},
		{"Not%2FA_Zone", "UTC", utcDay},
	} {
		t.Run(fmt.Sprintf("tz=%s", tc.tz), func(t *testing.T) {
			var sessions SessionsView
			require.NoError(t, json.Unmarshal(serve(t, "view=sessions&period=7&tz="+tc.tz).Body.Bytes(), &sessions))
			assert.Equal(t, tc.wantZone, sessions.Zone)
			assert.Equal(t, []TimeSeriesPoint{{Timestamp: tc.wantDay, Value: 1}}, sessions.ActiveAgents)
			assert.Equal(t, []TimeSeriesPoint{{Timestamp: tc.wantDay, Value: 1}}, sessions.DailyCounts)

			var calls ModelCallsView
			require.NoError(t, json.Unmarshal(serve(t, "view=model-calls&period=7&tz="+tc.tz).Body.Bytes(), &calls))
			assert.Equal(t, tc.wantZone, calls.Zone)
			require.Len(t, calls.ByModel, 1)
			assert.Equal(t, []TimeSeriesPoint{{Timestamp: tc.wantDay, Value: 2}}, calls.ByModel[0].Points)

			var tokens TokensView
			require.NoError(t, json.Unmarshal(serve(t, "view=tokens&period=7&tz="+tc.tz).Body.Bytes(), &tokens))
			assert.Equal(t, tc.wantZone, tokens.Zone)
		})
	}
}

// TestSetCacheSweepsExpiredEntries: expired entries are evicted once the
// cache reaches the sweep threshold, live ones are kept, and the next sweep
// waits for the map to grow again.
func TestSetCacheSweepsExpiredEntries(t *testing.T) {
	svc := newContractTestService(newFakeMetricsClient())
	stale := time.Now().Add(-2 * cacheTTL)
	for i := 0; i < cacheSweepThreshold-2; i++ {
		svc.cache[fmt.Sprintf("old:%d", i)] = &cacheEntry{data: i, fetchedAt: stale}
	}
	svc.setCache("live:0", 0)
	assert.Len(t, svc.cache, cacheSweepThreshold-1, "below the threshold nothing is swept")

	svc.setCache("live:1", 1)
	assert.Len(t, svc.cache, 2, "reaching the threshold sweeps every expired entry")
	for _, k := range []string{"live:0", "live:1"} {
		_, ok := svc.getCached(k)
		assert.True(t, ok, "live entry %s survives the sweep", k)
	}
	assert.Equal(t, cacheSweepThreshold, svc.sweepAt)
}

func TestQueryConfigLocation(t *testing.T) {
	loc := chicago(t)
	cfg := applyQueryOptions([]QueryOption{WithProjectID("p1"), WithLocation(loc)})
	assert.Equal(t, loc, metricsQueryWindowFor(time.Now(), 7, cfg).loc)
	assert.Equal(t, ":p1@America/Chicago", cfg.cacheKeySuffix(), "zones must not share cached day buckets")

	assert.Equal(t, ":p1", applyQueryOptions([]QueryOption{WithProjectID("p1")}).cacheKeySuffix())
	assert.Equal(t, ":p1", applyQueryOptions([]QueryOption{WithProjectID("p1"), WithLocation(time.UTC)}).cacheKeySuffix())
	assert.Equal(t, ":p1", cfg.projectCacheKeySuffix(), "the summary has no day buckets and is shared across zones")
	assert.Equal(t, "@America/Chicago", applyQueryOptions([]QueryOption{WithLocation(loc)}).cacheKeySuffix())
	assert.Equal(t, time.UTC, metricsQueryWindowFor(time.Now(), 7, &queryConfig{}).loc, "days default to UTC")
}

func TestDashboardLocation(t *testing.T) {
	loc := func(query string) *time.Location {
		return dashboardLocation(httptest.NewRequest(http.MethodGet, "/api/v1/metrics/?"+query, nil))
	}
	if got := loc("tz=America%2FChicago"); assert.NotNil(t, got) {
		assert.Equal(t, "America/Chicago", got.String())
	}
	assert.Nil(t, loc(""), "absent")
	assert.Nil(t, loc("tz=Not%2FA_Zone"), "unknown zone")
	assert.Nil(t, loc("tz=Local"), "server-local zone is not the viewer's")
	assert.Nil(t, loc("tz=../../etc/passwd"), "not a zone name")
	assert.Nil(t, loc("tz=localtime"), "tzdata implementation file, not a zone")
	assert.Nil(t, loc("tz=Factory"), "tzdata placeholder, not a zone")
	assert.Nil(t, loc("tz=right%2FAmerica%2FChicago"), "host-dependent right/ tree")
	assert.Nil(t, loc("tz=%2Fetc%2Flocaltime"), "absolute path")
}
