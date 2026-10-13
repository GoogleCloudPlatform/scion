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
	"sync"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newFakeMetricsClient() *fakeMetricsClient {
	return &fakeMetricsClient{seriesByFilter: map[string][]*monitoringpb.TimeSeries{}, errByFilter: map[string]error{}}
}

func newContractTestService(client *fakeMetricsClient) *MetricsDashboardService {
	return &MetricsDashboardService{client: client, projectID: "test-project", cache: make(map[string]*cacheEntry)}
}

func intPoint(start, end time.Time, value int64) *monitoringpb.Point {
	return &monitoringpb.Point{
		Interval: &monitoringpb.TimeInterval{StartTime: timestamppb.New(start), EndTime: timestamppb.New(end)},
		Value:    &monitoringpb.TypedValue{Value: &monitoringpb.TypedValue_Int64Value{Int64Value: value}},
	}
}

func pointsByDay(points []TimeSeriesPoint) map[string]int64 {
	byDay := make(map[string]int64, len(points))
	for _, p := range points {
		byDay[p.Timestamp] = p.Value
	}
	return byDay
}

// fakeMetricsClient is the "narrow interface over monitoring.MetricClient"
// fake used by the dashboard contract test (design §7.2): it records every
// request filter, and returns canned series or errors keyed by the exact
// filter string a query produced.
type fakeMetricsClient struct {
	mu       sync.Mutex
	requests []*monitoringpb.ListTimeSeriesRequest

	seriesByFilter map[string][]*monitoringpb.TimeSeries
	errByFilter    map[string]error
}

func (c *fakeMetricsClient) ListTimeSeries(_ context.Context, req *monitoringpb.ListTimeSeriesRequest, _ ...gax.CallOption) timeSeriesIterator {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	if err, ok := c.errByFilter[req.Filter]; ok {
		return &fakeTimeSeriesIterator{err: err}
	}
	return &fakeTimeSeriesIterator{series: c.seriesByFilter[req.Filter]}
}

func (c *fakeMetricsClient) requestFilters() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	filters := make([]string, len(c.requests))
	for i, r := range c.requests {
		filters[i] = r.Filter
	}
	return filters
}

// fakeTimeSeriesIterator is a canned timeSeriesIterator.
type fakeTimeSeriesIterator struct {
	series []*monitoringpb.TimeSeries
	err    error
	idx    int
}

func (it *fakeTimeSeriesIterator) Next() (*monitoringpb.TimeSeries, error) {
	if it.err != nil {
		return nil, it.err
	}
	if it.idx >= len(it.series) {
		return nil, iterator.Done
	}
	ts := it.series[it.idx]
	it.idx++
	return ts, nil
}
