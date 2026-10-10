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

package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// awaitServerExit (runServerStart step 16) closes the audit writer only
// after the server wait has returned, i.e. after each HTTP drain
// (remaining-audit P1-9, architect ruling C'). The test's wait blocks on a
// channel and records that it returned; the close asserts it, so a missing
// wait fails deterministically.
func TestAwaitServerExit_ClosesAuditAfterServersDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	waitEntered := make(chan struct{})
	releaseWait := make(chan struct{})
	waited := false
	wait := func() {
		close(waitEntered)
		<-releaseWait // the servers are still draining
		waited = true
	}
	closed := make(chan bool, 1)
	done := make(chan error, 1)
	go func() {
		done <- awaitServerExit(ctx, make(chan error), cancel, wait, func(context.Context) error {
			closed <- waited
			return nil
		})
	}()
	cancel()
	<-waitEntered
	select {
	case <-closed:
		t.Fatal("audit writer closed while the servers were still draining")
	default:
	}
	close(releaseWait)
	require.True(t, <-closed, "closeAudit must run after wait returns")
	require.NoError(t, <-done)
}

// On a server error, the writer is closed after cancel without waiting
// for drains (their records are counted as closed).
func TestAwaitServerExit_ErrorPathCancelsThenClosesAudit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	boom := errors.New("server failed")
	errCh <- boom
	closed := false
	err := awaitServerExit(ctx, errCh, cancel, func() { t.Error("error path must not wait for drains") }, func(context.Context) error {
		require.Error(t, ctx.Err(), "cancel runs before the audit close")
		closed = true
		return nil
	})
	require.ErrorIs(t, err, boom)
	require.True(t, closed)
}

func TestAwaitServerExit_NoHub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waited := false
	require.NoError(t, awaitServerExit(ctx, make(chan error), cancel, func() { waited = true }, nil))
	require.True(t, waited)
}

// P1-10: with a ManualReader MeterProvider passed to the real
// wireHubCoreMetrics, the decision-log and logging-writer instruments are
// exported with only their declared attributes.
func TestWireHubCoreMetrics_DecisionLogInstruments(t *testing.T) {
	ctx := context.Background()
	inner := newTestStore(t)
	srv, err := hub.New(hub.ServerConfig{}, inner)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	wireHubCoreMetrics(srv, mp)

	ops := hub.NewOperationalSettings(inner, koanf.New("."), koanf.New("."))
	_, err = ops.Update(ctx, "experiments",
		json.RawMessage(`{"overrides":{"hub.authorization_decision_audit_v2":true}}`), "test", 0, "managed")
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	identity := hub.NewAuthenticatedUser("metrics-user", "metrics@example.com", "Metrics", "member", "web")
	decide := func(id string) {
		reqCtx := logging.ContextWithRequestMeta(ctx, &logging.RequestMeta{RequestID: id})
		srv.GetAuthzService().CheckAccess(reqCtx, identity, hub.Resource{Type: "project", ID: "metrics-project"}, hub.ActionRead)
	}
	decide("metrics-req-1")
	require.NoError(t, srv.CloseAuditWriter(ctx)) // drains: written
	decide("metrics-req-2")                       // rejected: closed

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	points := map[string][]map[string]string{}
	values := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			var dps []metricdata.DataPoint[int64]
			switch d := m.Data.(type) {
			case metricdata.Sum[int64]:
				dps = d.DataPoints
			case metricdata.Gauge[int64]:
				dps = d.DataPoints
			default:
				continue
			}
			for _, dp := range dps {
				attrs := map[string]string{}
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.Emit()
				}
				points[m.Name] = append(points[m.Name], attrs)
				values[m.Name] += dp.Value
			}
		}
	}
	keysOf := func(attrs map[string]string) []string {
		var keys []string
		for k := range attrs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return keys
	}

	require.NotEmpty(t, points[hub.MetricDecisionAuditRecords], "decision records counter exported")
	for _, attrs := range points[hub.MetricDecisionAuditRecords] {
		require.Equal(t, []string{"disposition", "result"}, keysOf(attrs))
	}
	require.GreaterOrEqual(t, values[hub.MetricDecisionAuditRecords], int64(2))

	require.NotEmpty(t, points[logging.MetricWriteFailures], "write failures exported")
	sawClosed := false
	for _, attrs := range points[logging.MetricWriteFailures] {
		require.Equal(t, []string{"reason", "writer"}, keysOf(attrs))
		require.Equal(t, "audit", attrs["writer"])
		sawClosed = sawClosed || attrs["reason"] == "closed"
	}
	require.True(t, sawClosed, "closed rejection exported: %v", points[logging.MetricWriteFailures])

	require.Equal(t, int64(1), values[logging.MetricWriteRecords])
	for _, name := range []string{logging.MetricWriteRecords, logging.MetricQueueDepth, logging.MetricWriterStalled} {
		require.NotEmpty(t, points[name], name)
		for _, attrs := range points[name] {
			require.Equal(t, map[string]string{"writer": "audit"}, attrs, name)
		}
	}
}
