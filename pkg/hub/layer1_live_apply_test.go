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
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/rawbytes"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3904: saved Layer-1 settings that take effect on the
// running hub, and clears that return to the startup value.

// A saved soft-delete retention applies to the next delete, and a shorter
// retention applies to the next purge, without a restart. Clearing it
// returns to the startup value (no retention: hard delete).
func TestSoftDeleteRetention_SavedValueAppliesToNextDeleteAndPurge(t *testing.T) {
	srv, s, _, _ := engineTestServer(t)
	srv.mu.RLock()
	startupRetention := srv.config.SoftDeleteRetention
	srv.mu.RUnlock()
	require.Zero(t, startupRetention, "the test server starts with no retention")

	ApplySnapshot(srv, dbSnap(Layer1Snapshot{SoftDeleteRetention: "1h"}))
	soft := setupBrokerAgentInPhase(t, s, "retention-soft", state.PhaseStopped)
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+soft.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	got := mustGetAgent(t, s, soft.ID)
	require.False(t, got.DeletedAt.IsZero(), "a saved retention makes the next delete soft")

	// Under the 1h retention the purge keeps the agent.
	srv.purgeHandler()(context.Background())
	require.False(t, agentGone(t, s, soft.ID))

	// A shorter retention applies to the next purge.
	time.Sleep(10 * time.Millisecond)
	res := ApplySnapshot(srv, dbSnap(Layer1Snapshot{SoftDeleteRetention: "1ms"}))
	assert.Contains(t, res["applied"], "soft_delete_retention")
	srv.purgeHandler()(context.Background())
	assert.True(t, agentGone(t, s, soft.ID), "the shorter retention purges the agent")

	// Clearing the retention returns to the startup value: hard delete.
	ApplySnapshot(srv, dbSnap(Layer1Snapshot{}))
	retention, _ := srv.softDeleteSettings()
	assert.Equal(t, startupRetention, retention)
	hard := setupBrokerAgentInPhase(t, s, "retention-hard", state.PhaseStopped)
	rec = doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+hard.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, s, hard.ID), "a cleared retention makes the next delete hard")
}

// soft_delete_retain_files applies live; unset returns to the startup value,
// and an invalid retention keeps the running one.
func TestSoftDeleteSettings_RetainFilesAndInvalidRetention(t *testing.T) {
	srv := &Server{}
	srv.config.SoftDeleteRetention = 2 * time.Hour
	srv.config.SoftDeleteRetainFiles = true
	srv.recordStartupLayer1Values()

	f := false
	ApplySnapshot(srv, dbSnap(Layer1Snapshot{SoftDeleteRetention: "30m", SoftDeleteRetainFiles: &f}))
	r, keep := srv.softDeleteSettings()
	assert.Equal(t, 30*time.Minute, r)
	assert.False(t, keep)

	ApplySnapshot(srv, dbSnap(Layer1Snapshot{SoftDeleteRetention: "not-a-duration", SoftDeleteRetainFiles: &f}))
	r, _ = srv.softDeleteSettings()
	assert.Equal(t, 30*time.Minute, r, "an invalid retention keeps the running value")

	ApplySnapshot(srv, dbSnap(Layer1Snapshot{}))
	r, keep = srv.softDeleteSettings()
	assert.Equal(t, 2*time.Hour, r, "unset returns to the startup retention")
	assert.True(t, keep, "unset returns to the startup retain-files value")
}

// webhookReceiver is a local notification receiver.
type webhookReceiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []string
}

func newWebhookReceiver(t *testing.T) *webhookReceiver {
	r := &webhookReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, string(body))
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *webhookReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

// A notification channel added through a save receives a notification
// without a restart; a cleared channel stops receiving. The notification
// dispatcher, started before the save, follows the swap.
func TestNotificationChannels_SaveRebuildsRegistry(t *testing.T) {
	ctx := context.Background()
	recv := newWebhookReceiver(t)
	st := newFakeHubSettingStore()
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	srv.SetOperationalSettings(ops)
	nd := &NotificationDispatcher{channelRegistryFn: srv.currentChannelRegistry}

	apply := func() map[string]interface{} {
		return ApplySnapshot(srv, ops.Snapshot())
	}
	notify := func() {
		msg := messages.NewNotification("agent:probe", "user:u1", "probe", messages.TypeStateChange)
		if cr := nd.currentChannelRegistry(); cr != nil {
			cr.Dispatch(ctx, msg)
		}
	}

	apply()
	notify()
	require.Equal(t, 0, recv.count(), "no channel configured yet")

	doc := `{"notification_channels":[{"type":"webhook","params":{"url":"` + recv.srv.URL + `"}}]}`
	_, err := ops.Update(ctx, "notifications", json.RawMessage(doc), "admin", -1, "managed")
	require.NoError(t, err)
	res := apply()
	assert.Contains(t, res["applied"], "notification_channels")
	before := srv.currentChannelRegistry()
	notify()
	assert.Equal(t, 1, recv.count(), "the saved channel receives the notification")

	// Re-applying the same list keeps the registry.
	res = apply()
	assert.NotContains(t, res["applied"], "notification_channels")
	assert.Same(t, before, srv.currentChannelRegistry())

	_, err = ops.Update(ctx, "notifications", json.RawMessage(`{"notification_channels":[]}`), "admin", -1, "managed")
	require.NoError(t, err)
	apply()
	assert.Nil(t, srv.currentChannelRegistry())
	notify()
	assert.Equal(t, 1, recv.count(), "a cleared channel receives nothing")
}

// A snapshot built from the file does not carry the channels, so applying
// it leaves the registry configured at startup in place.
func TestNotificationChannels_FileSnapshotKeepsRegistry(t *testing.T) {
	srv := &Server{}
	reg := NewChannelRegistry([]ChannelConfig{{Type: "webhook", Params: map[string]string{"url": "http://127.0.0.1:9/x"}}}, slog.Default())
	srv.SetChannelRegistry(reg)
	ApplySnapshot(srv, BuildLayer1SnapshotFromFile(&config.GlobalConfig{}))
	assert.Same(t, reg, srv.currentChannelRegistry())
}

// Clearing image_registry makes the next agent use the startup registry.
func TestImageRegistry_ClearRevertsToStartupValue(t *testing.T) {
	srv, _ := testServer(t)
	srv.mu.Lock()
	srv.config.MaintenanceConfig.ImageRegistry = "boot.example.com/scion"
	srv.mu.Unlock()
	srv.recordStartupLayer1Values()
	dispatcher := srv.CreateAuthenticatedDispatcher()

	ApplySnapshot(srv, dbSnap(Layer1Snapshot{ImageRegistry: "db.example.com/scion"}))
	assert.Equal(t, "db.example.com/scion", dispatcher.ImageRegistry(), "a saved registry applies to the next dispatch")

	res := ApplySnapshot(srv, dbSnap(Layer1Snapshot{}))
	assert.Contains(t, res["applied"], "image_registry")
	assert.Equal(t, "boot.example.com/scion", dispatcher.ImageRegistry(), "a cleared registry returns to the startup value")
	assert.Equal(t, "boot.example.com/scion", srv.resolveImageRegistry())
}

// Without OperationalSettings, a registry set through /system/registry
// survives a file-mode reload: a snapshot built from the file does not
// return an unset value to the startup value.
func TestImageRegistry_FileModeReloadKeepsSystemRegistry(t *testing.T) {
	tempSettingsHome(t)
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	srv.config.MaintenanceConfig.ImageRegistry = "boot.example.com/scion"
	srv.recordStartupLayer1Values()

	rr := httptest.NewRecorder()
	req := adminRequest(http.MethodPut, "/api/v1/system/registry", `{"image_registry":"set.example.com/scion"}`)
	req.RemoteAddr = "127.0.0.1:1234"
	srv.handleSystemRegistry(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.Equal(t, "set.example.com/scion", srv.resolveImageRegistry())

	srv.reloadSettings()
	assert.Equal(t, "set.example.com/scion", srv.resolveImageRegistry(), "a file-mode reload keeps the registry")
}

// Clearing the GitHub App section returns to the startup app; the secrets
// loaded at startup are kept.
func TestGitHubApp_ClearRevertsToStartupValue(t *testing.T) {
	srv := &Server{}
	srv.config.GitHubAppConfig = GitHubAppServerConfig{
		AppID: 7, APIBaseURL: "https://api.github.com", PrivateKeyPath: "/etc/boot.pem",
		PrivateKey: "secret-key", WebhookSecret: "secret-hook",
	}
	srv.recordStartupLayer1Values()

	ApplySnapshot(srv, dbSnap(Layer1Snapshot{GitHubAppID: 42, GitHubAPIBaseURL: "https://ghe.example.com/api/v3", GitHubWebhooksEnabled: true}))
	srv.mu.RLock()
	assert.Equal(t, int64(42), srv.config.GitHubAppConfig.AppID)
	assert.Equal(t, "https://ghe.example.com/api/v3", srv.config.GitHubAppConfig.APIBaseURL)
	assert.Equal(t, "/etc/boot.pem", srv.config.GitHubAppConfig.PrivateKeyPath, "an unset key path keeps the startup path")
	srv.mu.RUnlock()

	res := ApplySnapshot(srv, dbSnap(Layer1Snapshot{}))
	assert.Contains(t, res["applied"], "github_app")
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	assert.Equal(t, int64(7), srv.config.GitHubAppConfig.AppID)
	assert.Equal(t, "https://api.github.com", srv.config.GitHubAppConfig.APIBaseURL)
	assert.False(t, srv.config.GitHubAppConfig.WebhooksEnabled)
	assert.Equal(t, "secret-key", srv.config.GitHubAppConfig.PrivateKey)
	assert.Equal(t, "secret-hook", srv.config.GitHubAppConfig.WebhookSecret)
}

// Clearing telemetry returns to the startup telemetry default and config.
func TestTelemetry_ClearRevertsToStartupValue(t *testing.T) {
	on := true
	srv := &Server{}
	srv.config.TelemetryDefault = &on
	srv.config.TelemetryConfig = &api.TelemetryConfig{Resource: map[string]string{"boot": "yes"}}
	srv.recordStartupLayer1Values()

	off := false
	ApplySnapshot(srv, dbSnap(Layer1Snapshot{
		TelemetryEnabled: &off,
		TelemetryConfig:  &config.V1TelemetryConfig{Resource: map[string]string{"db": "yes"}},
	}))
	srv.mu.RLock()
	assert.False(t, *srv.config.TelemetryDefault)
	assert.Equal(t, map[string]string{"db": "yes"}, srv.config.TelemetryConfig.Resource)
	srv.mu.RUnlock()

	res := ApplySnapshot(srv, dbSnap(Layer1Snapshot{}))
	assert.Contains(t, res["applied"], "telemetry_default")
	assert.Contains(t, res["applied"], "telemetry_config")
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	require.NotNil(t, srv.config.TelemetryDefault)
	assert.True(t, *srv.config.TelemetryDefault)
	assert.Equal(t, map[string]string{"boot": "yes"}, srv.config.TelemetryConfig.Resource)
}

// Clearing auto_expose_ports.enabled returns to the startup value.
func TestAutoExposePorts_ClearRevertsToStartupValue(t *testing.T) {
	ctx := context.Background()
	st := newFakeHubSettingStore()
	ops := NewOperationalSettings(st, emptyKoanf(), emptyKoanf())
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	on := true
	srv.config.AutoExposePortsDefault = &on
	srv.recordStartupLayer1Values()
	srv.SetOperationalSettings(ops)

	_, err := ops.Update(ctx, "auto_expose_ports", json.RawMessage(`{"enabled":false}`), "admin", -1, "managed")
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
	got := srv.autoExposePortsDefault()
	require.NotNil(t, got)
	assert.False(t, *got, "the saved value applies")

	_, err = ops.Update(ctx, "auto_expose_ports", json.RawMessage(`{}`), "admin", -1, "managed")
	require.NoError(t, err)
	res := ApplySnapshot(srv, ops.Snapshot())
	assert.Contains(t, res["applied"], "auto_expose_ports_default")
	got = srv.autoExposePortsDefault()
	require.NotNil(t, got)
	assert.True(t, *got, "a cleared value returns to the startup value")
}

// The save response lists the changed keys applied to the running hub and
// the changed keys pending a restart.
func TestPutServerConfigDB_ReportsAppliedAndPendingRestartKeys(t *testing.T) {
	srv, _, ops := newTestDBServer(t)
	rr := putHubNameServerConfigDB(t, srv, ops,
		`{"server":{"hub":{"hub_name":"report-hub","public_url":"https://report.example.com","soft_delete_retention":"2h"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var resp struct {
		Reload struct {
			AppliedKeys    []string `json:"applied_keys"`
			PendingRestart []string `json:"pending_restart"`
		} `json:"reload"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Contains(t, resp.Reload.AppliedKeys, "server.hub.hub_name")
	assert.Contains(t, resp.Reload.AppliedKeys, "server.hub.soft_delete_retention")
	assert.Equal(t, []string{"server.hub.public_url"}, resp.Reload.PendingRestart)

	// Saving the same values again changes nothing.
	rr = putHubNameServerConfigDB(t, srv, ops,
		`{"server":{"hub":{"hub_name":"report-hub","public_url":"https://report.example.com","soft_delete_retention":"2h"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Empty(t, resp.Reload.AppliedKeys)
	assert.Empty(t, resp.Reload.PendingRestart)
}

// ptone/scion#4108: a telemetry map key containing a dot saves, reloads and
// applies with the key intact, and GET keeps the telemetry section.
func TestTelemetry_DottedMapKeysRoundTrip(t *testing.T) {
	ctx := context.Background()
	srv, fakeStore, ops := newTestDBServer(t)
	doc := `{"enabled":true,"resource":{"service.namespace":"team-a"},"filter":{"sampling":{"rates":{"agent.tool.call":0.5}}},"cloud":{"headers":{"x-api.key":"v"}}}`
	_, err := ops.Update(ctx, "telemetry", json.RawMessage(doc), "admin", -1, "managed")
	require.NoError(t, err)

	// Reload from the store, as a restart or another replica would.
	ops2 := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err = ops2.Refresh(ctx)
	require.NoError(t, err)
	for _, o := range []*OperationalSettings{ops, ops2} {
		snap := o.Snapshot()
		require.NotNil(t, snap.TelemetryConfig, "the telemetry section must not vanish")
		assert.Equal(t, map[string]string{"service.namespace": "team-a"}, snap.TelemetryConfig.Resource)
		require.NotNil(t, snap.TelemetryConfig.Filter)
		require.NotNil(t, snap.TelemetryConfig.Filter.Sampling)
		assert.Equal(t, map[string]float64{"agent.tool.call": 0.5}, snap.TelemetryConfig.Filter.Sampling.Rates)
		require.NotNil(t, snap.TelemetryConfig.Cloud)
		assert.Equal(t, map[string]string{"x-api.key": "v"}, snap.TelemetryConfig.Cloud.Headers)
	}

	resp := getServerConfigDB(t, srv, ops)
	require.NotNil(t, resp.Telemetry, "GET keeps the telemetry section")
	assert.Equal(t, "team-a", resp.Telemetry.Resource["service.namespace"])

	ApplySnapshot(srv, ops.Snapshot())
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	require.NotNil(t, srv.config.TelemetryConfig)
	assert.Equal(t, "team-a", srv.config.TelemetryConfig.Resource["service.namespace"])
}

// ptone/scion#4108: with no stored telemetry row, the bootstrap value from
// settings.yaml keeps its dotted map keys in the snapshot.
func TestTelemetry_BootstrapDottedMapKeys(t *testing.T) {
	boot := koanf.New(".")
	require.NoError(t, boot.Load(rawbytes.Provider([]byte("telemetry:\n  resource:\n    service.namespace: team-b\n")), yaml.Parser()))
	ops := NewOperationalSettings(newFakeHubSettingStore(), boot, emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	snap := ops.Snapshot()
	require.NotNil(t, snap.TelemetryConfig)
	assert.Equal(t, map[string]string{"service.namespace": "team-b"}, snap.TelemetryConfig.Resource)
}

// ptone/scion#4108: a telemetry document that does not decode is logged
// rather than dropped silently.
func TestTelemetry_DecodeFailureIsLogged(t *testing.T) {
	ctx := context.Background()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seedWithOrigin("telemetry", json.RawMessage(`{"resource":{"service":{"namespace":1}}}`), "managed")
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(ctx)
	require.NoError(t, err)

	buf := captureSlogDefault(t)
	snap := ops.Snapshot()
	assert.Nil(t, snap.TelemetryConfig)
	assert.Contains(t, buf.String(), "cannot decode telemetry settings")
}

// dbSnap marks s as a DB-backed snapshot, as OperationalSettings.Snapshot
// builds it.
func dbSnap(s Layer1Snapshot) Layer1Snapshot {
	s.FromOperationalSettings = true
	return s
}
