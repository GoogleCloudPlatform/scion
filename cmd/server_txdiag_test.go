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
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
)

// TestAttachTxDiagRecorder_ExportsHoldAndCommitMetrics: with the default
// tx_diagnostics config and the hub DB metrics recorder, the production
// wiring (txDiagConfig, attachTxDiagRecorder) makes transaction holds and
// commits observable as metrics. If the recorder stopped satisfying
// txdiag.Recorder, attachTxDiagRecorder would silently attach nothing and
// this test would fail.
func TestAttachTxDiagRecorder_ExportsHoldAndCommitMetrics(t *testing.T) {
	ctx := context.Background()
	diag, err := txDiagConfig(config.TxDiagnosticsConfig{})
	require.NoError(t, err)
	require.NotNil(t, diag, "diagnostics are on by default")

	client, err := entc.OpenSQLite("file:"+filepath.Join(t.TempDir(), "hub.db"), entc.PoolConfig{MaxOpenConns: 1, Diag: diag})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(ctx, client))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(ctx) })
	rec, err := dbmetrics.New(mp)
	require.NoError(t, err)
	attachTxDiagRecorder(client, rec)

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	_, err = tx.HubSetting.Create().SetSection("wiring").SetValue(json.RawMessage(`{}`)).Save(ctx)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(ctx, &rm))
	seen := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			seen[m.Name] = true
		}
	}
	for _, name := range []string{dbmetrics.MetricTxHoldDuration, dbmetrics.MetricTxCommitDuration, dbmetrics.MetricTxLockWaitDuration} {
		require.True(t, seen[name], "metric %s not exported after attachTxDiagRecorder", name)
	}
}

// TestTxDiagConfig_DisabledAndNoOps: enabled=false yields no Diag (the pool
// opens unwrapped), invalid durations fail, and attachTxDiagRecorder is a
// no-op on an unwrapped client or a nil recorder.
func TestTxDiagConfig_DisabledAndNoOps(t *testing.T) {
	off := false
	diag, err := txDiagConfig(config.TxDiagnosticsConfig{Enabled: &off})
	require.NoError(t, err)
	require.Nil(t, diag)

	_, err = txDiagConfig(config.TxDiagnosticsConfig{WarnInterval: "0"})
	require.Error(t, err)

	client, err := entc.OpenSQLite("file:"+filepath.Join(t.TempDir(), "plain.db"), entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	attachTxDiagRecorder(client, dbmetrics.NewDisabled())
	attachTxDiagRecorder(client, nil)
	attachTxDiagRecorder(nil, nil)
}
