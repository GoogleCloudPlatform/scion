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
	"testing"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	googlemetricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func intPoint(start, end time.Time, value int64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
		Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: value}},
	}
}

func doublePoint(start, end time.Time, value float64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
		Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: value}},
	}
}

func sumIncrements(incs []seriesIncrement) int64 {
	var total int64
	for _, inc := range incs {
		total += inc.Value
	}
	return total
}

// TestSeriesIncreasesSingleEpochMultipleFlushes pins AC-1.3: the total for a
// single-epoch series with N flushes equals the last value, not the sum of
// every raw point.
func TestSeriesIncreasesSingleEpochMultipleFlushes(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fetchStart := epoch.Add(-time.Hour)
	points := []*monitoringpb.Point{
		intPoint(epoch, epoch.Add(1*time.Minute), 5),
		intPoint(epoch, epoch.Add(2*time.Minute), 12),
		intPoint(epoch, epoch.Add(3*time.Minute), 20),
	}
	incs := seriesIncreases(points, fetchStart)
	require.Len(t, incs, 3)
	assert.Equal(t, []int64{5, 7, 8}, []int64{incs[0].Value, incs[1].Value, incs[2].Value})
	assert.Equal(t, int64(20), sumIncrements(incs), "must equal the last value, not the raw-point sum of 37")
}

// TestSeriesIncreasesResetMidWindow pins the "a new start is a reset" rule:
// a later point whose interval start moves forward begins a fresh epoch.
func TestSeriesIncreasesResetMidWindow(t *testing.T) {
	epoch1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	epoch2 := epoch1.Add(time.Hour) // process restarted; counter resets to 0
	fetchStart := epoch1.Add(-time.Hour)
	points := []*monitoringpb.Point{
		intPoint(epoch1, epoch1.Add(time.Minute), 5),
		intPoint(epoch2, epoch2.Add(time.Minute), 3),
	}
	incs := seriesIncreases(points, fetchStart)
	require.Len(t, incs, 2)
	assert.Equal(t, int64(5), incs[0].Value, "first epoch's first point contributes its full value")
	assert.Equal(t, int64(3), incs[1].Value, "the reset epoch's first point also contributes its full value, not a diff against the old epoch")
}

// TestSeriesIncreasesEpochBeforeLookback pins: an epoch that began before the
// fetch window is a baseline and contributes 0 for its first point.
func TestSeriesIncreasesEpochBeforeLookback(t *testing.T) {
	epoch := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) // long before the window
	fetchStart := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	points := []*monitoringpb.Point{
		intPoint(epoch, fetchStart.Add(time.Hour), 42),
	}
	incs := seriesIncreases(points, fetchStart)
	require.Len(t, incs, 1)
	assert.Equal(t, int64(0), incs[0].Value, "a pre-window epoch is a baseline, not a windfall of 42")

	// A later point in the same (pre-window) epoch still yields a normal delta.
	points = append(points, intPoint(epoch, fetchStart.Add(2*time.Hour), 50))
	incs = seriesIncreases(points, fetchStart)
	require.Len(t, incs, 2)
	assert.Equal(t, int64(0), incs[0].Value)
	assert.Equal(t, int64(8), incs[1].Value)
}

// TestSeriesIncreasesDoubleValueRounds pins: a DoubleValue point is accepted
// by rounding, defending against a future double producer.
func TestSeriesIncreasesDoubleValueRounds(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fetchStart := epoch.Add(-time.Hour)
	points := []*monitoringpb.Point{
		doublePoint(epoch, epoch.Add(time.Minute), 5.4),
		doublePoint(epoch, epoch.Add(2*time.Minute), 12.6),
	}
	incs := seriesIncreases(points, fetchStart)
	require.Len(t, incs, 2)
	assert.Equal(t, int64(5), incs[0].Value) // round(5.4) = 5
	assert.Equal(t, int64(8), incs[1].Value) // round(12.6)=13, 13-5=8
}

// TestSeriesIncreasesIgnoresDistributionPoints pins: canonical usage metrics
// are never distributions, so a histogram-shaped point (no TypedValue) is
// dropped rather than misread as zero.
func TestSeriesIncreasesIgnoresDistributionPoints(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	points := []*monitoringpb.Point{
		{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(epoch), EndTime: timestamppb.New(epoch.Add(time.Minute))}, Value: &monitoringpb.TypedValue{}},
		intPoint(epoch, epoch.Add(2*time.Minute), 9),
	}
	incs := seriesIncreases(points, epoch.Add(-time.Hour))
	require.Len(t, incs, 1)
	assert.Equal(t, int64(9), incs[0].Value)
}

// TestSeriesIncreasesSkipsNilPoints pins the nil handling in seriesIncreases:
// a nil point, a point with a nil Value, and a point with a nil Interval are
// all skipped without panicking, and a valid point mixed in among them still
// contributes normally. The nil-Value case is a pure no-op (the getters are
// nil-safe on their own), but the nil-Interval case is a real behavior fix:
// without it, such a point bucketed at the Unix epoch instead of being
// skipped.
func TestSeriesIncreasesSkipsNilPoints(t *testing.T) {
	epoch := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	points := []*monitoringpb.Point{
		nil,
		{Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(epoch), EndTime: timestamppb.New(epoch.Add(time.Minute))}}, // nil Value
		{Value: &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: 7}}},                                // nil Interval
		intPoint(epoch, epoch.Add(2*time.Minute), 9),
	}
	incs := seriesIncreases(points, epoch.Add(-time.Hour))
	require.Len(t, incs, 1)
	assert.Equal(t, int64(9), incs[0].Value)
}

// TestPointValueNil pins pointValue(nil) as ok=false rather than panicking.
func TestPointValueNil(t *testing.T) {
	value, ok := pointValue(nil)
	assert.False(t, ok)
	assert.Equal(t, int64(0), value)
}

// TestQueryDailyTimeSeriesBucketsByEndDay pins the day-bucketing rule: two
// flushes landing on different UTC days go to different buckets, and each
// bucket holds the increment (not the raw cumulative value).
func TestQueryDailyTimeSeriesBucketsByEndDay(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	day1 := time.Date(2026, 3, 10, 23, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 3, 11, 1, 0, 0, 0, time.UTC)
	epoch := day1.Add(-time.Hour)
	filter := `metric.type = "` + metricPrefix + telemetrycontract.MetricAPICalls + `"`
	client.seriesByFilter[filter] = []*monitoringpb.TimeSeries{{
		Metric: &googlemetricpb.Metric{Type: metricPrefix + telemetrycontract.MetricAPICalls},
		Points: []*monitoringpb.Point{
			intPoint(epoch, day1, 4),
			intPoint(epoch, day2, 10),
		},
	}}

	points, err := svc.queryDailyTimeSeries(context.Background(), telemetrycontract.MetricAPICalls, day1.Add(-2*time.Hour), day2.Add(time.Hour), nil)
	require.NoError(t, err)
	byDay := map[string]int64{}
	for _, p := range points {
		byDay[p.Timestamp] = p.Value
	}
	assert.Equal(t, int64(4), byDay["2026-03-10"])
	assert.Equal(t, int64(6), byDay["2026-03-11"], "second day's increment is 10-4=6, not the raw value 10")
}

// TestQuerySumTreatsNotFoundAsZero pins the "NotFound-as-zero" rule at the
// querySum level directly (the contract test exercises it through
// QuerySummary).
func TestQuerySumTreatsNotFoundAsZero(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	filter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `"`
	client.errByFilter[filter] = status.Error(codes.NotFound, "no descriptor yet")

	total, err := svc.querySum(context.Background(), telemetrycontract.MetricUsageTokens, time.Now().Add(-time.Hour), time.Now(), nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), total)
}

// TestQuerySumPropagatesOtherErrors pins that only NotFound is swallowed;
// any other error is still a partial failure.
func TestQuerySumPropagatesOtherErrors(t *testing.T) {
	client := newFakeMetricsClient()
	svc := newContractTestService(client)
	filter := `metric.type = "` + metricPrefix + telemetrycontract.MetricUsageTokens + `"`
	client.errByFilter[filter] = status.Error(codes.PermissionDenied, "no access")

	_, err := svc.querySum(context.Background(), telemetrycontract.MetricUsageTokens, time.Now().Add(-time.Hour), time.Now(), nil)
	assert.Error(t, err)
}
