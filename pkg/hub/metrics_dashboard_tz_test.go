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
