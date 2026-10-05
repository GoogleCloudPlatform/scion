// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // the zones below must not depend on the host's zoneinfo

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
)

// Tests for viewer-zone day bucketing on the metrics dashboard
// (ptone/scion#3370): the tz query parameter, the local-midnight window, the
// three bucketing sites, and the zone-aware cache.

func mustLoadLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	require.NoError(t, err)
	return loc
}

func TestResolveDashboardTimeZone(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "UTC"},
		{"UTC", "UTC"},
		{"Etc/UTC", "UTC"},
		{"Local", "UTC"},
		{"America/Chicago", "America/Chicago"},
		{"Asia/Kathmandu", "Asia/Kathmandu"},
		{"America/Argentina/Buenos_Aires", "America/Argentina/Buenos_Aires"},
		{"America/Port-au-Prince", "America/Port-au-Prince"},
		{"Etc/GMT+5", "Etc/GMT+5"},
		// Path-shaped input never reaches time.LoadLocation.
		{"../etc", "UTC"},
		{"/etc/localtime", "UTC"},
		{"Asia/../../etc/passwd", "UTC"},
		{"Asia\\Tokyo", "UTC"},
		{"Asia/Tokyo/", "UTC"},
		{"./Asia/Tokyo", "UTC"},
		// Garbage, unknown and non-portable names.
		{"not a zone; drop table", "UTC"},
		{"Not/A_Zone", "UTC"},
		{"asia/tokyo", "UTC"},
		{"+05:45", "UTC"},
		{"localtime", "UTC"},
		{"posixrules", "UTC"},
		{"Factory", "UTC"},
		{"right/Asia/Tokyo", "UTC"},
		{"posix/Asia/Tokyo", "UTC"},
		{"A/B/C/D", "UTC"},
		{strings.Repeat("A", maxTimeZoneParamLen+1), "UTC"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.in), func(t *testing.T) {
			got := resolveDashboardTimeZone(tt.in)
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got.String())
		})
	}
	assert.Same(t, time.UTC, resolveDashboardTimeZone("Local"), "Local must be UTC, never the hub process zone")
}

func TestMetricsQueryWindowStartsAtLocalMidnight(t *testing.T) {
	chicago := mustLoadLocation(t, "America/Chicago")
	kathmandu := mustLoadLocation(t, "Asia/Kathmandu")
	santiago := mustLoadLocation(t, "America/Santiago")

	tests := []struct {
		name       string
		now        time.Time
		periodDays int
		loc        *time.Location
		wantStart  time.Time
	}{
		{
			// 20:30 CDT on 10 March is 01:30Z on 11 March. The window still
			// ends on the local date: seven days back from 10 March is 4
			// March, which is CST (-6), before the 8 March DST change.
			name:       "Chicago evening, window spans spring-forward",
			now:        time.Date(2026, 3, 10, 20, 30, 0, 0, chicago),
			periodDays: 7,
			loc:        chicago,
			wantStart:  time.Date(2026, 3, 4, 6, 0, 0, 0, time.UTC),
		},
		{
			name:       "Kathmandu +05:45",
			now:        time.Date(2026, 3, 11, 0, 5, 0, 0, kathmandu),
			periodDays: 1,
			loc:        kathmandu,
			wantStart:  time.Date(2026, 3, 10, 18, 15, 0, 0, time.UTC),
		},
		{
			// America/Santiago skips 00:00-01:00 on 6 September 2026, and
			// time.Date resolves that missing midnight to 23:00 on the 5th.
			// The window must start at the first instant of the 6th instead.
			name:       "Santiago DST gap at midnight",
			now:        time.Date(2026, 9, 6, 12, 0, 0, 0, santiago),
			periodDays: 1,
			loc:        santiago,
			wantStart:  time.Date(2026, 9, 6, 4, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			window := metricsQueryWindowFor(tt.now, tt.periodDays, &queryConfig{Location: tt.loc})
			assert.Equal(t, tt.wantStart, window.start)
			assert.Equal(t, tt.now.UTC(), window.end)
			// The start is a local day boundary: the instant before it is on
			// the previous local date.
			assert.NotEqual(t, dayKey(window.start, tt.loc), dayKey(window.start.Add(-time.Nanosecond), tt.loc))
		})
	}
}

func TestStartOfLocalDayDSTDayLengths(t *testing.T) {
	chicago := mustLoadLocation(t, "America/Chicago")
	dayLength := func(y int, m time.Month, d int) time.Duration {
		return startOfLocalDay(y, m, d+1, chicago).Sub(startOfLocalDay(y, m, d, chicago))
	}
	assert.Equal(t, 23*time.Hour, dayLength(2026, time.March, 8), "spring-forward day")
	assert.Equal(t, 25*time.Hour, dayLength(2026, time.November, 1), "fall-back day")
	assert.Equal(t, 24*time.Hour, dayLength(2026, time.March, 10))
}

// bucketPoint is one cumulative flush on a single stream: the epoch is
// shared, so each point's increment is its value minus the previous one.
type bucketPoint struct {
	end   time.Time
	value int64
}

// bucketAtAllSites runs one series through each of the three day-bucketing
// sites (queryDailyTimeSeries: Daily Sessions; queryGroupedTimeSeries: API
// calls and tokens by model/harness; queryDailyUniqueCount: Active Agents per
// Day) in loc, and returns each site's day -> value map.
func bucketAtAllSites(t *testing.T, loc *time.Location, points []bucketPoint) map[string]map[string]int64 {
	t.Helper()
	require.NotEmpty(t, points)
	epoch := points[0].end.Add(-time.Hour)
	queryStart := epoch.Add(-time.Hour)
	queryEnd := points[len(points)-1].end.Add(time.Hour)
	var raw []*monitoringpb.Point
	for _, p := range points {
		raw = append(raw, intPoint(epoch, p.end, p.value))
	}
	ctx := context.Background()
	newSvc := func(metric string, labels map[string]string) *MetricsDashboardService {
		client := newFakeMetricsClient()
		client.seriesByFilter[`metric.type = "`+metricPrefix+metric+`"`] = []*monitoringpb.TimeSeries{{
			Metric: &googlemetricpb.Metric{Type: metricPrefix + metric, Labels: labels},
			Points: raw,
		}}
		return newContractTestService(client)
	}

	out := map[string]map[string]int64{}

	daily, err := newSvc(telemetrycontract.MetricAPICalls, nil).queryDailyTimeSeries(ctx, telemetrycontract.MetricAPICalls, queryStart, queryEnd, nil, loc)
	require.NoError(t, err)
	out["daily"] = pointsByDay(daily)

	grouped, err := newSvc(telemetrycontract.MetricAPICalls, map[string]string{telemetrycontract.ModelLabel: "m1"}).
		queryGroupedTimeSeries(ctx, telemetrycontract.MetricAPICalls, "metric.labels."+telemetrycontract.ModelLabel, queryStart, queryEnd, nil, loc)
	require.NoError(t, err)
	require.Len(t, grouped, 1)
	out["grouped"] = pointsByDay(grouped[0].Points)

	unique, err := newSvc(telemetrycontract.MetricSessionCount, map[string]string{telemetrycontract.AgentLabel: "agent-1"}).
		queryDailyUniqueCount(ctx, telemetrycontract.MetricSessionCount, "metric.labels."+telemetrycontract.AgentLabel, queryStart, queryEnd, nil, loc)
	require.NoError(t, err)
	out["unique"] = pointsByDay(unique)
	return out
}

// TestDailyBucketsChicagoEvening: 20:30 CDT on 10 March is 01:30Z on 11
// March. In America/Chicago it belongs to 10 March, not the next UTC date.
func TestDailyBucketsChicagoEvening(t *testing.T) {
	chicago := mustLoadLocation(t, "America/Chicago")
	evening := time.Date(2026, 3, 10, 20, 30, 0, 0, chicago)

	got := bucketAtAllSites(t, chicago, []bucketPoint{{evening, 5}})
	assert.Equal(t, map[string]int64{"2026-03-10": 5}, got["daily"])
	assert.Equal(t, map[string]int64{"2026-03-10": 5}, got["grouped"])
	assert.Equal(t, map[string]int64{"2026-03-10": 1}, got["unique"])
	// The same instant in the UTC default lands on the next UTC date.
	for site, byDay := range bucketAtAllSites(t, time.UTC, []bucketPoint{{evening, 5}}) {
		assert.Contains(t, byDay, "2026-03-11", site)
		assert.Len(t, byDay, 1, site)
	}
}

// TestDailyBucketsDSTTransitionDays: on Chicago's 23-hour spring-forward day
// and 25-hour fall-back day, a point just after local midnight and one just
// before the next local midnight both land on the transition day, and the
// points on either side land on the neighbouring days.
func TestDailyBucketsDSTTransitionDays(t *testing.T) {
	chicago := mustLoadLocation(t, "America/Chicago")
	tests := []struct {
		name    string
		day     time.Time
		wantLen time.Duration
		prev    string
		dayKey  string
		next    string
	}{
		{"spring forward (23h)", time.Date(2026, 3, 8, 0, 0, 0, 0, chicago), 23 * time.Hour, "2026-03-07", "2026-03-08", "2026-03-09"},
		{"fall back (25h)", time.Date(2026, 11, 1, 0, 0, 0, 0, chicago), 25 * time.Hour, "2026-10-31", "2026-11-01", "2026-11-02"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := startOfLocalDay(tt.day.Year(), tt.day.Month(), tt.day.Day()+1, chicago)
			require.Equal(t, tt.wantLen, next.Sub(tt.day))
			points := []bucketPoint{
				{tt.day.Add(-time.Minute), 1}, // previous local day
				{tt.day.Add(time.Minute), 2},  // +1, first minute of the transition day
				{next.Add(-time.Minute), 4},   // +2, last minute of the transition day
				{next.Add(time.Minute), 8},    // +4, following day
			}
			got := bucketAtAllSites(t, chicago, points)
			assert.Equal(t, map[string]int64{tt.prev: 1, tt.dayKey: 3, tt.next: 4}, got["daily"])
			assert.Equal(t, map[string]int64{tt.prev: 1, tt.dayKey: 3, tt.next: 4}, got["grouped"])
			assert.Equal(t, map[string]int64{tt.prev: 1, tt.dayKey: 1, tt.next: 1}, got["unique"])

			// 24h arithmetic would be wrong on these days: midnight plus 24h
			// is 01:00 the next day after spring-forward and 23:00 the same
			// day after fall-back.
			plus24 := dayKey(tt.day.Add(24*time.Hour), chicago)
			if tt.wantLen == 23*time.Hour {
				assert.Equal(t, tt.next, plus24)
			} else {
				assert.Equal(t, tt.dayKey, plus24)
			}
		})
	}
}

// TestDailyBucketsKathmanduFractionalOffset: Asia/Kathmandu is +05:45, so
// 18:10Z is 23:55 on the same date and 18:20Z is 00:05 on the next.
func TestDailyBucketsKathmanduFractionalOffset(t *testing.T) {
	kathmandu := mustLoadLocation(t, "Asia/Kathmandu")
	points := []bucketPoint{
		{time.Date(2026, 3, 10, 18, 10, 0, 0, time.UTC), 2},
		{time.Date(2026, 3, 10, 18, 20, 0, 0, time.UTC), 5},
	}
	got := bucketAtAllSites(t, kathmandu, points)
	assert.Equal(t, map[string]int64{"2026-03-10": 2, "2026-03-11": 3}, got["daily"])
	assert.Equal(t, map[string]int64{"2026-03-10": 2, "2026-03-11": 3}, got["grouped"])
	assert.Equal(t, map[string]int64{"2026-03-10": 1, "2026-03-11": 1}, got["unique"])

	// In UTC both flushes are on 10 March.
	utc := bucketAtAllSites(t, time.UTC, points)
	assert.Equal(t, map[string]int64{"2026-03-10": 5}, utc["daily"])
}

// TestDashboardCacheKeyDistinguishesZones: the same view and period for two
// zones are two cache entries with their own buckets, and a repeat request
// for a zone is served from its own entry.
func TestDashboardCacheKeyDistinguishesZones(t *testing.T) {
	chicago := mustLoadLocation(t, "America/Chicago")

	cfgUTC := applyQueryOptions([]QueryOption{WithProjectID("p1")})
	cfgChicago := applyQueryOptions([]QueryOption{WithProjectID("p1"), WithTimeZone(chicago)})
	assert.Equal(t, "sessions:7:tz=UTC:p1", cfgUTC.cacheKey("sessions", 7))
	assert.Equal(t, "sessions:7:tz=America/Chicago:p1", cfgChicago.cacheKey("sessions", 7))
	assert.Equal(t, cfgUTC.cacheKey("sessions", 7), applyQueryOptions([]QueryOption{WithProjectID("p1"), WithTimeZone(resolveDashboardTimeZone("../etc"))}).cacheKey("sessions", 7),
		"an invalid zone resolves to UTC and shares the UTC entry")

	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	// A flush 30 minutes ago keeps the point inside the 1-day window for
	// both zones, whatever the wall clock.
	end := time.Now().Add(-30 * time.Minute)
	client.seriesByFilter[`metric.type = "`+metricPrefix+telemetrycontract.MetricSessionCount+`"`] = []*monitoringpb.TimeSeries{{
		Metric: &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricSessionCount, Labels: map[string]string{telemetrycontract.AgentLabel: "a1"}},
		Points: []*monitoringpb.Point{intPoint(end.Add(-time.Minute), end, 3)},
	}}
	ctx := context.Background()

	utcView, err := svc.QuerySessions(ctx, 2)
	require.NoError(t, err)
	afterUTC := len(client.requestFilters())
	chicagoView, err := svc.QuerySessions(ctx, 2, WithTimeZone(chicago))
	require.NoError(t, err)
	afterChicago := len(client.requestFilters())
	assert.Greater(t, afterChicago, afterUTC, "a different zone must not be served from the UTC entry")

	assert.Equal(t, "UTC", utcView.TimeZone)
	assert.Equal(t, "America/Chicago", chicagoView.TimeZone)
	require.Len(t, utcView.DailyCounts, 1)
	require.Len(t, chicagoView.DailyCounts, 1)
	assert.Equal(t, dayKey(end, time.UTC), utcView.DailyCounts[0].Timestamp)
	assert.Equal(t, dayKey(end, chicago), chicagoView.DailyCounts[0].Timestamp)

	again, err := svc.QuerySessions(ctx, 2, WithTimeZone(chicago))
	require.NoError(t, err)
	assert.Same(t, chicagoView, again)
	assert.Equal(t, afterChicago, len(client.requestFilters()), "a repeat request for the same zone is a cache hit")
}

// TestDashboardCacheBoundsDistinctZones: at most maxCachedZones distinct
// non-UTC zones are cached; a further zone is computed but not cached, UTC
// is always cached, and expired entries are pruned so their zones free up.
func TestDashboardCacheBoundsDistinctZones(t *testing.T) {
	svc := &MetricsDashboardService{cache: make(map[string]*cacheEntry)}
	for i := 0; i < maxCachedZones; i++ {
		zone := fmt.Sprintf("Zone/%d", i)
		require.True(t, svc.setZonedCache("sessions:7:tz="+zone, zone, i))
	}
	assert.True(t, svc.setZonedCache("tokens:7:tz=Zone/0", "Zone/0", "x"), "a zone already cached may add views")
	assert.False(t, svc.setZonedCache("sessions:7:tz=Zone/extra", "Zone/extra", "x"), "a new zone past the cap is not cached")
	_, ok := svc.getCached("sessions:7:tz=Zone/extra")
	assert.False(t, ok)
	assert.True(t, svc.setZonedCache("sessions:7:tz=UTC", "UTC", "x"), "UTC is always cached")

	// Expire Zone/1's only entry: it is pruned on the next set, which frees
	// a slot.
	svc.mu.Lock()
	svc.cache["sessions:7:tz=Zone/1"].fetchedAt = time.Now().Add(-2 * cacheTTL)
	svc.mu.Unlock()
	assert.True(t, svc.setZonedCache("sessions:7:tz=Zone/extra", "Zone/extra", "x"))
	svc.mu.RLock()
	_, stillThere := svc.cache["sessions:7:tz=Zone/1"]
	svc.mu.RUnlock()
	assert.False(t, stillThere, "expired entries are pruned")
}

// TestServeMetricsDashboardEchoesResolvedZone: the API echoes the zone it
// bucketed by, and a bad tz never fails the request.
func TestServeMetricsDashboardEchoesResolvedZone(t *testing.T) {
	tests := []struct {
		tz   string
		want string
	}{
		{"", "UTC"},
		{"America/Chicago", "America/Chicago"},
		{"Asia/Kathmandu", "Asia/Kathmandu"},
		{"../etc", "UTC"},
		{"/etc/localtime", "UTC"},
		{"Local", "UTC"},
		{"garbage zone", "UTC"},
	}
	for _, view := range []string{"summary", "sessions", "model-calls", "tokens"} {
		for _, tt := range tests {
			t.Run(view+"/"+tt.tz, func(t *testing.T) {
				srv := &Server{metricsDashboard: newContractTestService(newFakeMetricsClient())}
				target := "/api/v1/metrics/?view=" + view + "&period=7&tz=" + strings.ReplaceAll(tt.tz, " ", "%20")
				rec := httptest.NewRecorder()
				srv.serveMetricsDashboard(rec, httptest.NewRequest(http.MethodGet, target, nil))
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var body struct {
					TimeZone string `json:"timeZone"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, tt.want, body.TimeZone)
			})
		}
	}
}
