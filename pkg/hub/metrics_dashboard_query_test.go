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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricsQueryWindowFor(t *testing.T) {
	now := time.Date(2026, time.September, 14, 8, 30, 0, 0, time.FixedZone("test", 2*60*60))

	window := metricsQueryWindowFor(now, 7, &queryConfig{ProjectID: "project-1"})

	assert.Equal(t, now.UTC(), window.end)
	assert.Equal(t, now.UTC().AddDate(0, 0, -7), window.start)
	assert.Equal(t, []string{`metric.labels.project_id = "project-1"`}, window.extraFilter)

	global := metricsQueryWindowFor(now, 1, &queryConfig{})
	assert.Nil(t, global.extraFilter)
}

func TestQueryGroupedTimeSeriesSetPreservesPartialResults(t *testing.T) {
	queries := []groupedTimeSeriesQuery{
		{metricName: "first", groupBy: "model", errorLabel: "first series"},
		{metricName: "broken", groupBy: "harness", errorLabel: "broken series"},
		{metricName: "last", groupBy: "model", errorLabel: "last series"},
	}
	var calls []string

	results, err := queryGroupedTimeSeriesSet(queries, func(metricName, groupBy string) ([]LabeledTimeSeries, error) {
		calls = append(calls, metricName+":"+groupBy)
		if metricName == "broken" {
			return nil, errors.New("query failed")
		}
		return []LabeledTimeSeries{{Label: metricName}}, nil
	})

	require.EqualError(t, err, "partial query failures: broken series: query failed")
	assert.Equal(t, []string{"first:model", "broken:harness", "last:model"}, calls)
	assert.Equal(t, []LabeledTimeSeries{{Label: "first"}}, results[0])
	assert.Nil(t, results[1])
	assert.Equal(t, []LabeledTimeSeries{{Label: "last"}}, results[2])
}

func TestQueryGroupedTimeSeriesSetSuccess(t *testing.T) {
	results, err := queryGroupedTimeSeriesSet([]groupedTimeSeriesQuery{
		{metricName: "calls", groupBy: "model", errorLabel: "calls"},
	}, func(metricName, _ string) ([]LabeledTimeSeries, error) {
		return []LabeledTimeSeries{{Label: metricName}}, nil
	})

	require.NoError(t, err)
	assert.Equal(t, [][]LabeledTimeSeries{{{Label: "calls"}}}, results)
}

func TestQueryGroupedMetricsViewUsesConcreteCachedView(t *testing.T) {
	service := &MetricsDashboardService{cache: make(map[string]*cacheEntry)}
	want := &ModelCallsView{PeriodDays: 7}
	service.setCache("model-calls:7:project-1", want)

	got, err := queryGroupedMetricsView(
		service,
		t.Context(),
		"model-calls",
		7,
		[]QueryOption{WithProjectID("project-1")},
		nil,
		func([][]LabeledTimeSeries) *ModelCallsView {
			t.Fatal("cache hit unexpectedly queried metrics")
			return nil
		},
	)

	require.NoError(t, err)
	assert.Same(t, want, got)
}
