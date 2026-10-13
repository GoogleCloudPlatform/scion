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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/hubexport"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Saving a hub observability switch writes the hub_observability row as
// the admin, and the save report lists the key as pending a restart, not
// as applied: the exporters are created once at startup (ptone/scion#4914).
func TestHubObservabilityPut_SavesRowAndReportsPendingRestart(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"observability":{"metrics_enabled":true,"tracing_enabled":false}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	doc, rev := rowDoc(t, fakeStore, hubObservabilitySection)
	assert.Equal(t, map[string]interface{}{"metrics_enabled": true, "tracing_enabled": false}, doc)
	assert.Equal(t, int64(1), rev)

	// The settings audit fields of the row name the admin who saved it.
	row, err := fakeStore.GetHubSetting(context.Background(), hubObservabilitySection)
	require.NoError(t, err)
	assert.Equal(t, "admin@example.com", row.UpdatedBy)
	assert.Equal(t, "managed", row.Origin)

	applied, pending := savedKeys(t, rr.Body.Bytes())
	assert.Equal(t, []string{config.HubMetricsEnabledKey, config.HubTracingEnabledKey}, pending)
	assert.NotContains(t, applied, config.HubMetricsEnabledKey)
	assert.NotContains(t, applied, config.HubTracingEnabledKey)

	snap := ops.Snapshot()
	require.NotNil(t, snap.HubMetricsEnabled)
	require.NotNil(t, snap.HubTracingEnabled)
	assert.True(t, *snap.HubMetricsEnabled)
	assert.False(t, *snap.HubTracingEnabled)
}

// An omitted switch keeps its stored value; an explicit null clears it, so
// the switch is unset again and the earlier rule applies at the next start.
func TestHubObservabilityPut_OmittedKeepsNullClears(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)

	rr := putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"observability":{"metrics_enabled":false,"tracing_enabled":true}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"observability":{"metrics_enabled":true}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _ := rowDoc(t, fakeStore, hubObservabilitySection)
	assert.Equal(t, map[string]interface{}{"metrics_enabled": true, "tracing_enabled": true}, doc, "an omitted switch keeps its value")
	_, pending := savedKeys(t, rr.Body.Bytes())
	assert.Equal(t, []string{config.HubMetricsEnabledKey}, pending, "only the changed key is reported")

	rr = putHubNameServerConfigDB(t, srv, ops, `{"server":{"hub":{"observability":{"tracing_enabled":null}}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	doc, _ = rowDoc(t, fakeStore, hubObservabilitySection)
	assert.Equal(t, map[string]interface{}{"metrics_enabled": true}, doc, "null clears the switch")
	_, pending = savedKeys(t, rr.Body.Bytes())
	assert.Equal(t, []string{config.HubTracingEnabledKey}, pending, "a cleared key is pending a restart too")
	assert.Nil(t, ops.Snapshot().HubTracingEnabled)
}

// A non-boolean switch is rejected and nothing is written.
func TestHubObservabilityPut_RejectsNonBoolean(t *testing.T) {
	srv, fakeStore, ops := newTestDBServer(t)
	rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"observability":{"metrics_enabled":"yes"}}}}`)
	assert.GreaterOrEqual(t, rr.Code, 400, rr.Body.String())
	assert.Less(t, rr.Code, 500, rr.Body.String())
	_, err := fakeStore.GetHubSetting(context.Background(), hubObservabilitySection)
	assert.Error(t, err, "no row is written")
}

// GET shows the saved switches and, read-only, the exporters as started
// with the export project and its source.
func TestHubObservabilityGet_SwitchesAndExportStatus(t *testing.T) {
	ctx := context.Background()
	srv, _, ops := newTestDBServer(t)

	resp := getServerConfigDB(t, srv, ops)
	assert.Nil(t, resp.Server.Hub.Observability, "unset switches are omitted")
	assert.Nil(t, resp.ObservabilityExport, "no status before startup records it")

	_, err := ops.Update(ctx, hubObservabilitySection, json.RawMessage(`{"metrics_enabled":true}`), "admin", -1, "managed")
	require.NoError(t, err)
	project := hubexport.Project{ID: "meta-project", Source: hubexport.SourceMetadata}
	on := true
	srv.SetObservabilityExportStatus(hubexport.Status{
		Project: project,
		Metrics: hubexport.DecideMetrics(&on, project),
		Tracing: hubexport.DecideTracing(nil, false, project),
	})

	resp = getServerConfigDB(t, srv, ops)
	require.NotNil(t, resp.Server.Hub.Observability)
	require.NotNil(t, resp.Server.Hub.Observability.MetricsEnabled)
	assert.True(t, *resp.Server.Hub.Observability.MetricsEnabled)
	assert.Nil(t, resp.Server.Hub.Observability.TracingEnabled)

	require.NotNil(t, resp.ObservabilityExport)
	assert.Equal(t, "meta-project", resp.ObservabilityExport.Project.ID)
	assert.Equal(t, hubexport.SourceMetadata, resp.ObservabilityExport.Project.Source)
	assert.True(t, resp.ObservabilityExport.Metrics.Enabled)
	assert.Equal(t, hubexport.BasisSetting, resp.ObservabilityExport.Metrics.Basis)
	assert.False(t, resp.ObservabilityExport.Tracing.Enabled)
	assert.Equal(t, hubexport.BasisLegacy, resp.ObservabilityExport.Tracing.Basis)
}

// A managed DB row wins over the settings file and the environment (the
// bootstrap merge) for the switches, and with no row the bootstrap value
// applies. The env override is listed for the admin view's env badge.
func TestHubObservabilitySnapshot_DBRowWinsOverBootstrap(t *testing.T) {
	ctx := context.Background()
	boot := koanf.New(".")
	require.NoError(t, boot.Load(confmap.Provider(map[string]interface{}{
		config.HubMetricsEnabledKey: "true",
	}, "."), nil))
	env := koanf.New(".")
	require.NoError(t, env.Load(confmap.Provider(map[string]interface{}{
		config.HubMetricsEnabledKey: "true",
	}, "."), nil))
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, boot, env)

	snap := ops.Snapshot()
	require.NotNil(t, snap.HubMetricsEnabled)
	assert.True(t, *snap.HubMetricsEnabled, "with no row the bootstrap value applies")
	assert.Contains(t, ops.EnvOverriddenKeys(), config.HubMetricsEnabledKey)

	_, err := ops.Update(ctx, hubObservabilitySection, json.RawMessage(`{"metrics_enabled":false}`), "admin", -1, "managed")
	require.NoError(t, err)
	snap = ops.Snapshot()
	require.NotNil(t, snap.HubMetricsEnabled)
	assert.False(t, *snap.HubMetricsEnabled, "the managed row wins over the env value")
}

// The switches are read from the settings file when the hub has no
// operational settings service, and ApplySnapshot does not touch the
// running exporters (restart_required).
func TestHubObservability_FileSnapshotAndNoLiveApply(t *testing.T) {
	on, off := true, false
	gc := &config.GlobalConfig{}
	gc.Hub.Observability = config.HubObservabilityConfig{MetricsEnabled: &on, TracingEnabled: &off}
	snap := BuildLayer1SnapshotFromFile(gc)
	assert.Equal(t, &on, snap.HubMetricsEnabled)
	assert.Equal(t, &off, snap.HubTracingEnabled)

	srv, _, _ := newTestDBServer(t)
	st := hubexport.Status{Metrics: hubexport.DecideMetrics(nil, hubexport.Project{})}
	srv.SetObservabilityExportStatus(st)
	applied := ApplySnapshot(srv, Layer1Snapshot{HubMetricsEnabled: &on, HubTracingEnabled: &on, FromOperationalSettings: true})
	for k := range applied {
		assert.NotContains(t, k, "observability", "the switches are not applied live")
	}
	assert.False(t, srv.ObservabilityExportStatus().Metrics.Enabled, "the startup status is unchanged")
}

// Only an interactive hub admin writes the switches: a user access token
// is refused for the section, and the section cannot be reset by a token.
func TestHubObservability_TokenRefused(t *testing.T) {
	assert.Equal(t, settingsTokenRefused, serverConfigTokenSections[hubObservabilitySection])
	assert.Equal(t, settingsTokenRefused, serverConfigKeyTokenClass(config.HubMetricsEnabledKey))
	assert.Equal(t, settingsTokenRefused, serverConfigKeyTokenClass(config.HubTracingEnabledKey))
	assert.False(t, serverConfigSectionTokenResettable(hubObservabilitySection))
}

// The status accessor returns a copy, so a caller cannot change the
// recorded startup state.
func TestHubObservability_StatusIsCopied(t *testing.T) {
	srv := &Server{}
	assert.Nil(t, srv.ObservabilityExportStatus())
	srv.SetObservabilityExportStatus(hubexport.Status{Project: hubexport.Project{ID: "p", Source: hubexport.SourceFile}})
	got := srv.ObservabilityExportStatus()
	got.Project.ID = "changed"
	assert.Equal(t, "p", srv.ObservabilityExportStatus().Project.ID)
}
