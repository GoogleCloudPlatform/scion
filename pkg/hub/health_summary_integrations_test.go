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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// healthSummaryPluginDouble is a test double for the plugin manager with
// per-plugin health. A plugin listed in stopped fails its info query, as a
// plugin whose process has exited does.
type healthSummaryPluginDouble struct {
	*mockIntegrationManager
	health  map[string]string
	message map[string]string
	details map[string]map[string]string
	stopped map[string]bool
}

func newHealthSummaryPluginDouble(names ...string) *healthSummaryPluginDouble {
	d := &healthSummaryPluginDouble{
		mockIntegrationManager: newMockIntegrationManager(),
		health:                 map[string]string{},
		message:                map[string]string{},
		details:                map[string]map[string]string{},
		stopped:                map[string]bool{},
	}
	for _, n := range names {
		d.plugins[n] = map[string]string{}
		d.health[n] = "healthy"
	}
	return d
}

func (d *healthSummaryPluginDouble) BrokerInfo(name string) (string, string, []string, error) {
	if d.stopped[name] {
		return "", "", nil, errors.New("plugin process exited: connection refused")
	}
	return "v1.2.3", "chan-secret-id", []string{"send"}, nil
}

func (d *healthSummaryPluginDouble) BrokerHealthCheck(name string) (string, string, map[string]string, error) {
	if d.stopped[name] {
		return "", "", nil, errors.New("plugin process exited")
	}
	return d.health[name], d.message[name], d.details[name], nil
}

func getHealthSummaryIntegrations(t *testing.T, srv *Server) ([]HealthSummaryIntegration, []byte) {
	t.Helper()
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	require.NotNil(t, resp.Integrations, "integrations must be a list, never null")
	return resp.Integrations, rr.Body.Bytes()
}

func findHealthSummaryIntegration(t *testing.T, list []HealthSummaryIntegration, name string) HealthSummaryIntegration {
	t.Helper()
	for _, it := range list {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("integration %q not in %+v", name, list)
	return HealthSummaryIntegration{}
}

func createHealthSummaryPluginRecord(t *testing.T, s store.Store, name string) {
	t.Helper()
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID:              tid("plugin-record-" + name),
		Name:            "plugin-" + name,
		Slug:            "plugin-" + name,
		Status:          store.BrokerStatusOnline,
		ConnectionState: "embedded",
		Labels:          map[string]string{pluginBrokerLabel: name},
		Created:         time.Now(),
		Updated:         time.Now(),
	}))
}

// tickHubInstance runs one registry tick for srv, writing its row, with its
// plugins' health, as the registry loop does every 15 s.
func tickHubInstance(t *testing.T, srv *Server) {
	t.Helper()
	srv.newHubInstanceRegistry().tick(context.Background())
}

// newHubReplica returns a second hub server on the same store s, as a
// second replica behind a load balancer. Its own instance ID and plugin
// manager are independent of the first server's.
func newHubReplica(t *testing.T, s store.Store) *Server {
	t.Helper()
	srv, err := newTestHubServer(t, testServerConfig(), s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	return srv
}

func TestHandleHealthSummary_IntegrationsEmptyWithoutPlugins(t *testing.T) {
	srv, _ := testServer(t)
	list, body := getHealthSummaryIntegrations(t, srv)
	assert.Empty(t, list)
	assert.Contains(t, string(body), `"integrations":[]`)
}

func TestHandleHealthSummary_IntegrationHealthy(t *testing.T) {
	srv, s := testServer(t)
	srv.SetPluginManager(newHealthSummaryPluginDouble("telegram"))
	// The plugin's own record must not appear as a runtime broker.
	createHealthSummaryPluginRecord(t, s, "telegram")
	tickHubInstance(t, srv)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	require.Len(t, resp.Integrations, 1, "a managed plugin with a record is listed once")
	got := resp.Integrations[0]
	require.NotNil(t, got.ReportedAt)
	got.ReportedAt = nil
	assert.Equal(t, HealthSummaryIntegration{
		Name: "telegram", Platform: "telegram", Health: "healthy", Connected: true, Version: "v1.2.3",
		ManagedBy: []string{srv.InstanceID()},
	}, got)
	for _, b := range resp.Brokers.Items {
		assert.NotEqual(t, "plugin-telegram", b.Name, "plugin must appear only under integrations")
	}
}

// A plugin that stops shows as not reported after the next registry tick;
// until then the summary keeps the last written report, since it makes no
// plugin call itself.
func TestHandleHealthSummary_IntegrationStoppedChangesNextTick(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("chat-app")
	srv.SetPluginManager(mgr)
	tickHubInstance(t, srv)

	list, _ := getHealthSummaryIntegrations(t, srv)
	got := findHealthSummaryIntegration(t, list, "chat-app")
	assert.Equal(t, "healthy", got.Health)
	assert.Equal(t, "gchat", got.Platform)
	assert.True(t, got.Connected)

	mgr.stopped["chat-app"] = true
	list, _ = getHealthSummaryIntegrations(t, srv)
	assert.Equal(t, "healthy", findHealthSummaryIntegration(t, list, "chat-app").Health,
		"the summary reads the row, not the plugin")

	tickHubInstance(t, srv)
	list, body := getHealthSummaryIntegrations(t, srv)
	got = findHealthSummaryIntegration(t, list, "chat-app")
	assert.Equal(t, "unknown", got.Health)
	assert.False(t, got.Connected)
	assert.Empty(t, got.Reason, "a reported plugin gets no not-run reason")
	assert.NotContains(t, string(body), "connection refused", "raw errors must not leak")
}

func TestHandleHealthSummary_IntegrationUnhealthyNotConnected(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("slack")
	mgr.health["slack"] = "unhealthy"
	srv.SetPluginManager(mgr)
	tickHubInstance(t, srv)

	list, _ := getHealthSummaryIntegrations(t, srv)
	got := findHealthSummaryIntegration(t, list, "slack")
	assert.Equal(t, "unhealthy", got.Health)
	assert.False(t, got.Connected)
}

func TestHandleHealthSummary_IntegrationNotRunByAnyInstance(t *testing.T) {
	srv, s := testServer(t)
	srv.SetPluginManager(newHealthSummaryPluginDouble("telegram"))
	createHealthSummaryPluginRecord(t, s, "discord")
	tickHubInstance(t, srv)

	list, _ := getHealthSummaryIntegrations(t, srv)
	require.Len(t, list, 2)
	assert.Equal(t, "discord", list[0].Name, "sorted by name")
	assert.Equal(t, HealthSummaryIntegration{
		Name: "discord", Platform: "discord", Health: "unknown", Reason: "not run by any running hub instance",
	}, list[0])
	assert.Equal(t, "healthy", list[1].Health)
}

func TestHandleHealthSummary_IntegrationNotRunWithoutManager(t *testing.T) {
	srv, s := testServer(t)
	createHealthSummaryPluginRecord(t, s, "teams")
	tickHubInstance(t, srv)

	list, _ := getHealthSummaryIntegrations(t, srv)
	require.Len(t, list, 1)
	assert.Equal(t, "unknown", list[0].Health)
	assert.Equal(t, "not run by any running hub instance", list[0].Reason)
}

func TestHandleHealthSummary_IntegrationsOmitMessageAndDetails(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("telegram")
	mgr.health["telegram"] = "degraded"
	mgr.message["telegram"] = "token sk-live-SECRETVALUE rejected"
	mgr.details["telegram"] = map[string]string{"bot_token": "SECRETDETAIL"}
	srv.SetPluginManager(mgr)
	tickHubInstance(t, srv)

	// The registry row keeps only the allow-listed fields.
	rows, _, err := srv.store.ListHubInstances(context.Background(), time.Hour)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	for _, frag := range []string{"SECRETVALUE", "SECRETDETAIL", "chan-secret-id", "message", "details"} {
		assert.NotContains(t, string(rows[0].Stats), frag, "registry row holds %q", frag)
	}

	_, body := getHealthSummaryIntegrations(t, srv)
	var raw struct {
		Integrations []map[string]json.RawMessage `json:"integrations"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Len(t, raw.Integrations, 1)
	allowed := map[string]bool{
		"name": true, "platform": true, "health": true, "connected": true, "version": true, "reason": true,
		"managed_by": true, "reported_at": true,
	}
	for k := range raw.Integrations[0] {
		assert.True(t, allowed[k], "unexpected integration field %q", k)
	}
	assert.NotContains(t, raw.Integrations[0], "message")
	assert.NotContains(t, raw.Integrations[0], "details")
	for _, leak := range []string{"SECRETVALUE", "SECRETDETAIL", "chan-secret-id"} {
		assert.False(t, strings.Contains(string(body), leak), "response leaks %q", leak)
	}
	assert.Contains(t, string(body), `"health":"degraded"`)
}

// healthSummaryRoleUser creates a member user bound at system scope to a
// custom role holding exactly perms.
func healthSummaryRoleUser(t *testing.T, s store.Store, name string, perms []string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{ID: tid(name), Email: name + "@test.com", DisplayName: name, Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(ctx, u))
	ensureHubMembership(ctx, s, u.ID)
	rd := createTestRoleDefinition(t, s, name+"-role", store.RoleScopeSystem, perms)
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: "user", PrincipalID: u.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)
	return u
}

// healthSummaryAsUser fetches the summary through the full router as user.
func healthSummaryAsUser(t *testing.T, srv *Server, u *store.User) (HealthSummaryResponse, string) {
	t.Helper()
	rr := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	return resp, rr.Body.String()
}

// seedHealthSummaryIntegrations configures, on srv, a healthy managed
// plugin, an unhealthy managed plugin, a degraded managed plugin and a
// plugin record this instance does not run.
func seedHealthSummaryIntegrations(srv *Server, s store.Store, t *testing.T) {
	mgr := newHealthSummaryPluginDouble("telegram", "slack", "teams")
	mgr.health["slack"] = "unhealthy"
	mgr.health["teams"] = "degraded"
	srv.SetPluginManager(mgr)
	createHealthSummaryPluginRecord(t, s, "discord")
	tickHubInstance(t, srv)
}

// TestHandleHealthSummary_IntegrationIdentityRequiresIntegrationsRead: a
// caller with hub.health.read but not hub.integrations.read gets only the
// aggregate (counts and status), with no integration names, platforms,
// versions or links anywhere in the response. A caller with both sees the
// full detail.
func TestHandleHealthSummary_IntegrationIdentityRequiresIntegrationsRead(t *testing.T) {
	srv, s := testServer(t)
	seedHealthSummaryIntegrations(srv, s, t)
	healthOnly := healthSummaryRoleUser(t, s, "hs-health-only", []string{"hub.health.read"})
	withIntegrations := healthSummaryRoleUser(t, s, "hs-health-integrations", []string{"hub.health.read", "hub.integrations.read"})

	wantCounts := HealthSummaryIntegrationCounts{Total: 4, Healthy: 1, Degraded: 1, Unhealthy: 1, Unknown: 1}
	identity := []string{"telegram", "slack", "teams", "discord", "gchat", "v1.2.3", "plugin-", "/integrations",
		"not run by any running hub instance", "managed_by", "reported_at"}
	// This instance runs three plugins: one each healthy, unhealthy and
	// degraded.
	wantInstanceCounts := HealthSummaryIntegrationCounts{Total: 3, Healthy: 1, Degraded: 1, Unhealthy: 1}

	t.Run("health.read only: aggregate", func(t *testing.T) {
		resp, body := healthSummaryAsUser(t, srv, healthOnly)
		for _, frag := range identity {
			assert.NotContains(t, strings.ToLower(body), strings.ToLower(frag), "identity %q in the restricted response", frag)
		}
		assert.Contains(t, body, `"integrations":[]`)
		assert.False(t, resp.IntegrationsDetail)
		assert.Equal(t, wantCounts, resp.IntegrationCounts)
		assert.Equal(t, HealthStatusDegraded, resp.Status, "the status does not depend on the caller")
		// Each hub instance keeps its counts but loses its list.
		require.NotNil(t, resp.HubInstances)
		require.Len(t, resp.HubInstances.Items, 1)
		assert.Equal(t, wantInstanceCounts, resp.HubInstances.Items[0].IntegrationCounts)
		assert.Nil(t, resp.HubInstances.Items[0].Integrations)
		assert.Contains(t, body, `"integration_counts":{"total":3,`)
		var integrationItems []HealthAttentionItem
		for _, it := range resp.Attention {
			if it.Kind == HealthAttentionIntegration {
				integrationItems = append(integrationItems, it)
			}
		}
		assert.Equal(t, []HealthAttentionItem{
			{Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration, Subject: HealthAttentionSubject{Type: HealthSubjectIntegration}, Message: "1 integration unhealthy"},
			{Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration, Subject: HealthAttentionSubject{Type: HealthSubjectIntegration}, Message: "1 integration degraded"},
		}, integrationItems)
	})

	t.Run("health.read and integrations.read: full detail", func(t *testing.T) {
		resp, body := healthSummaryAsUser(t, srv, withIntegrations)
		assert.True(t, resp.IntegrationsDetail)
		assert.Equal(t, wantCounts, resp.IntegrationCounts)
		assert.Equal(t, HealthStatusDegraded, resp.Status)
		require.Len(t, resp.Integrations, 4)
		telegram := findHealthSummaryIntegration(t, resp.Integrations, "telegram")
		assert.Equal(t, "v1.2.3", telegram.Version)
		assert.Equal(t, []string{srv.InstanceID()}, telegram.ManagedBy)
		assert.NotNil(t, telegram.ReportedAt)
		assert.Contains(t, body, "discord")
		require.NotNil(t, resp.HubInstances)
		require.Len(t, resp.HubInstances.Items, 1)
		inst := resp.HubInstances.Items[0]
		assert.Equal(t, wantInstanceCounts, inst.IntegrationCounts)
		assert.Equal(t, []HealthHubInstanceIntegration{
			{Name: "slack", Health: "unhealthy", Connected: false, Version: "v1.2.3"},
			{Name: "teams", Health: "degraded", Connected: true, Version: "v1.2.3"},
			{Name: "telegram", Health: "healthy", Connected: true, Version: "v1.2.3"},
		}, inst.Integrations)
		assert.Contains(t, resp.Attention, HealthAttentionItem{
			Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration,
			Subject: HealthAttentionSubject{Type: HealthSubjectIntegration, ID: "slack", Name: "slack"},
			Message: "Integration slack is unhealthy",
		})
	})
}

// TestHandleHealthSummary_RestrictedIntegrationsMatchEmptyHub: for a
// caller without hub.integrations.read, the integration part of the
// response on a hub with integrations differs from a hub with none only in
// the counts and the aggregate items those counts produce.
func TestHandleHealthSummary_RestrictedIntegrationsMatchEmptyHub(t *testing.T) {
	type integrationView struct {
		Integrations json.RawMessage `json:"integrations"`
		Detail       json.RawMessage `json:"integrations_detail"`
		Attention    []struct {
			Kind    string          `json:"kind"`
			Subject json.RawMessage `json:"subject"`
			Message string          `json:"message"`
		} `json:"attention"`
	}
	view := func(withIntegrations bool) integrationView {
		srv, s := testServer(t)
		if withIntegrations {
			mgr := newHealthSummaryPluginDouble("telegram")
			mgr.health["telegram"] = "unhealthy"
			srv.SetPluginManager(mgr)
		}
		tickHubInstance(t, srv)
		u := healthSummaryRoleUser(t, s, "hs-restricted-view", []string{"hub.health.read"})
		rr := doRequestAsUser(t, srv, u, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var v integrationView
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &v))
		return v
	}
	empty, populated := view(false), view(true)
	assert.Equal(t, string(empty.Integrations), string(populated.Integrations))
	assert.Equal(t, "[]", string(populated.Integrations))
	assert.Equal(t, string(empty.Detail), string(populated.Detail))
	for _, it := range empty.Attention {
		assert.NotEqual(t, HealthAttentionIntegration, it.Kind)
	}
	var got []string
	for _, it := range populated.Attention {
		if it.Kind == HealthAttentionIntegration {
			assert.JSONEq(t, `{"type":"integration"}`, string(it.Subject))
			got = append(got, it.Message)
		}
	}
	assert.Equal(t, []string{"1 integration unhealthy"}, got)
}

// TestHandleHealthSummary_IntegrationUnhealthyDegrades: an unhealthy
// managed plugin turns the status degraded and raises an item; health is
// lower-cased at the source, whatever the plugin sends.
func TestHandleHealthSummary_IntegrationUnhealthyDegrades(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("slack")
	mgr.health["slack"] = " Unhealthy "
	srv.SetPluginManager(mgr)
	tickHubInstance(t, srv)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, HealthStatusDegraded, resp.Status)
	require.Len(t, resp.Integrations, 1)
	assert.Equal(t, "unhealthy", resp.Integrations[0].Health)
	assert.False(t, resp.Integrations[0].Connected)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionIntegration,
		Subject: HealthAttentionSubject{Type: HealthSubjectIntegration, ID: "slack", Name: "slack"},
		Message: "Integration slack is unhealthy",
	})
}

func TestGetIntegrationStatus_NormalisesHealthCasing(t *testing.T) {
	mgr := newHealthSummaryPluginDouble("chat")
	for in, want := range map[string]string{"Healthy": "healthy", "DEGRADED": "degraded", " unhealthy\n": "unhealthy"} {
		mgr.health["chat"] = in
		st := getIntegrationStatus(mgr, "chat")
		assert.Equal(t, want, st.Health, in)
		assert.Equal(t, want != "unhealthy", st.Connected, in)
	}
}

// slowHealthSummaryPluginDouble blocks the named plugin's info query until
// release is closed, and counts the queries it receives.
type slowHealthSummaryPluginDouble struct {
	*healthSummaryPluginDouble
	slow    string
	release chan struct{}
	calls   atomic.Int32
}

func (d *slowHealthSummaryPluginDouble) BrokerInfo(name string) (string, string, []string, error) {
	if name == d.slow {
		d.calls.Add(1)
		<-d.release
	}
	return d.healthSummaryPluginDouble.BrokerInfo(name)
}

// findHubInstanceIntegration returns the named entry of list.
func findHubInstanceIntegration(t *testing.T, list []api.HubInstanceIntegration, name string) api.HubInstanceIntegration {
	t.Helper()
	for _, it := range list {
		if it.Name == name {
			return it
		}
	}
	t.Fatalf("integration %q not in %+v", name, list)
	return api.HubInstanceIntegration{}
}

// TestHubInstanceIntegrations_SlowIntegrationNotReported: a plugin that
// does not answer within the tick's integration budget is reported as
// unknown, the tick does not wait for it, the other plugins are
// unaffected, and the hung plugin is not queried again until its first
// query returns.
func TestHubInstanceIntegrations_SlowIntegrationNotReported(t *testing.T) {
	orig := healthIntegrationQueryTimeout
	healthIntegrationQueryTimeout = 50 * time.Millisecond
	t.Cleanup(func() { healthIntegrationQueryTimeout = orig })

	srv, _ := testServer(t)
	mgr := &slowHealthSummaryPluginDouble{
		healthSummaryPluginDouble: newHealthSummaryPluginDouble("fast", "slow"),
		slow:                      "slow",
		release:                   make(chan struct{}),
	}
	released := false
	release := func() {
		if !released {
			released = true
			close(mgr.release)
		}
	}
	t.Cleanup(release)
	srv.SetPluginManager(mgr)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		start := time.Now()
		list := srv.hubInstanceIntegrations(ctx)
		assert.Less(t, time.Since(start), 5*time.Second, "the tick must not block on a slow plugin")
		assert.Equal(t, []api.HubInstanceIntegration{
			{Name: "fast", Health: "healthy", Connected: true, Version: "v1.2.3"},
			{Name: "slow", Health: "unknown"},
		}, list)
	}
	assert.Equal(t, int32(1), mgr.calls.Load(), "a plugin whose query is still running is not queried again")

	// Once the hung query returns, a later tick queries the plugin again
	// and reports its real health.
	release()
	require.Eventually(t, func() bool {
		return findHubInstanceIntegration(t, srv.hubInstanceIntegrations(ctx), "slow").Health == "healthy"
	}, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, int32(2), mgr.calls.Load())
}

// TestHubInstanceIntegrations_SlowIntegrationUnknownInSummary: a plugin
// that times out in the tick is stored as unknown and shown as unknown,
// neutral, with no reason (it is reported, just not known).
func TestHubInstanceIntegrations_SlowIntegrationUnknownInSummary(t *testing.T) {
	orig := healthIntegrationQueryTimeout
	healthIntegrationQueryTimeout = 20 * time.Millisecond
	t.Cleanup(func() { healthIntegrationQueryTimeout = orig })

	srv, _ := testServer(t)
	mgr := &slowHealthSummaryPluginDouble{
		healthSummaryPluginDouble: newHealthSummaryPluginDouble("slow"),
		slow:                      "slow",
		release:                   make(chan struct{}),
	}
	t.Cleanup(func() { close(mgr.release) })
	srv.SetPluginManager(mgr)
	tickHubInstance(t, srv)

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	got := findHealthSummaryIntegration(t, resp.Integrations, "slow")
	assert.Equal(t, "unknown", got.Health)
	assert.Empty(t, got.Reason)
	assert.Equal(t, []string{srv.InstanceID()}, got.ManagedBy)
	assert.Equal(t, HealthStatusHealthy, resp.Status, "unknown is neutral")
}

// TestHubInstanceIntegrations_HungIntegrationNoGoroutineGrowth: ticks
// against a hung plugin leave no goroutine behind; only the plugin's
// single running query remains until it returns.
func TestHubInstanceIntegrations_HungIntegrationNoGoroutineGrowth(t *testing.T) {
	orig := healthIntegrationQueryTimeout
	healthIntegrationQueryTimeout = 20 * time.Millisecond
	t.Cleanup(func() { healthIntegrationQueryTimeout = orig })

	srv, _ := testServer(t)
	mgr := &slowHealthSummaryPluginDouble{
		healthSummaryPluginDouble: newHealthSummaryPluginDouble("hung"),
		slow:                      "hung",
		release:                   make(chan struct{}),
	}
	t.Cleanup(func() { close(mgr.release) })
	srv.SetPluginManager(mgr)
	ctx := context.Background()

	settled := func() int {
		// Let short-lived goroutines finish; take the lowest reading.
		low := runtime.NumGoroutine()
		for i := 0; i < 20; i++ {
			time.Sleep(10 * time.Millisecond)
			if n := runtime.NumGoroutine(); n < low {
				low = n
			}
		}
		return low
	}

	// The first call starts the plugin's one query, which then hangs.
	srv.hubInstanceIntegrations(ctx)
	require.Equal(t, int32(1), mgr.calls.Load())
	base := settled()

	for i := 0; i < 10; i++ {
		assert.Equal(t, "unknown", findHubInstanceIntegration(t, srv.hubInstanceIntegrations(ctx), "hung").Health)
	}
	// A per-call waiter would add 10; allow a little slack for unrelated
	// background goroutines.
	assert.LessOrEqual(t, settled(), base+3, "calls against a hung plugin must not leave goroutines behind")
	assert.Equal(t, int32(1), mgr.calls.Load(), "still one query for the hung plugin")
}

// panickingHealthSummaryPluginDouble panics on the first info query for
// the named plugin and answers normally afterwards.
type panickingHealthSummaryPluginDouble struct {
	*healthSummaryPluginDouble
	name  string
	calls atomic.Int32
}

func (d *panickingHealthSummaryPluginDouble) BrokerInfo(name string) (string, string, []string, error) {
	if name == d.name && d.calls.Add(1) == 1 {
		panic("plugin RPC blew up")
	}
	return d.healthSummaryPluginDouble.BrokerInfo(name)
}

// TestHubInstanceIntegrations_PanickingIntegrationRecovered: a plugin call
// that panics does not take the hub down. The plugin is reported as
// unknown, its query is closed and removed, and a later call starts a
// fresh query that reports the real health.
func TestHubInstanceIntegrations_PanickingIntegrationRecovered(t *testing.T) {
	srv, _ := testServer(t)
	mgr := &panickingHealthSummaryPluginDouble{
		healthSummaryPluginDouble: newHealthSummaryPluginDouble("boom"),
		name:                      "boom",
	}
	srv.SetPluginManager(mgr)
	ctx := context.Background()

	assert.Equal(t, []api.HubInstanceIntegration{{Name: "boom", Health: "unknown"}}, srv.hubInstanceIntegrations(ctx))
	require.Eventually(t, func() bool {
		srv.healthIntegrationMu.Lock()
		defer srv.healthIntegrationMu.Unlock()
		return len(srv.healthIntegrationFlights) == 0
	}, 5*time.Second, 5*time.Millisecond, "the panicked query must be removed")

	assert.Equal(t, "healthy", findHubInstanceIntegration(t, srv.hubInstanceIntegrations(ctx), "boom").Health)
	assert.Equal(t, int32(2), mgr.calls.Load(), "the later call starts a fresh query")
}

// TestIntegrationHealthQuery_RemovesOnlyItsOwnFlight: a finishing query
// does not remove a different flight registered for the same plugin.
func TestIntegrationHealthQuery_RemovesOnlyItsOwnFlight(t *testing.T) {
	srv, _ := testServer(t)
	mgr := &gatedHealthSummaryPluginDouble{
		healthSummaryPluginDouble: newHealthSummaryPluginDouble("chat"),
		name:                      "chat",
		gate:                      make(chan struct{}),
	}
	old := srv.integrationHealthQuery(mgr, "chat")
	require.Eventually(t, func() bool { return mgr.calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)

	// Replace the running flight, as an eviction or reset would.
	replacement := &integrationHealthFlight{done: make(chan struct{})}
	srv.healthIntegrationMu.Lock()
	srv.healthIntegrationFlights["chat"] = replacement
	srv.healthIntegrationMu.Unlock()

	close(mgr.gate)
	select {
	case <-old.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the original query did not finish")
	}
	assert.Equal(t, "healthy", old.row.Health)
	srv.healthIntegrationMu.Lock()
	defer srv.healthIntegrationMu.Unlock()
	assert.Same(t, replacement, srv.healthIntegrationFlights["chat"], "the finished query must not remove the replacement")
}

// gatedHealthSummaryPluginDouble holds every info query for the named
// plugin until gate is closed, and counts the queries.
type gatedHealthSummaryPluginDouble struct {
	*healthSummaryPluginDouble
	name  string
	gate  chan struct{}
	calls atomic.Int32
}

func (d *gatedHealthSummaryPluginDouble) BrokerInfo(name string) (string, string, []string, error) {
	if name == d.name {
		d.calls.Add(1)
		<-d.gate
	}
	return d.healthSummaryPluginDouble.BrokerInfo(name)
}

// TestHubInstanceIntegrations_OverlappingCallsShareQuery: two calls that
// overlap while a plugin's query is running both get its real health from
// one shared query.
func TestHubInstanceIntegrations_OverlappingCallsShareQuery(t *testing.T) {
	srv, _ := testServer(t)
	mgr := &gatedHealthSummaryPluginDouble{
		healthSummaryPluginDouble: newHealthSummaryPluginDouble("slack"),
		name:                      "slack",
		gate:                      make(chan struct{}),
	}
	mgr.health["slack"] = "unhealthy"
	srv.SetPluginManager(mgr)

	results := make(chan []api.HubInstanceIntegration, 2)
	fetch := func() { results <- srv.hubInstanceIntegrations(context.Background()) }
	go fetch()
	require.Eventually(t, func() bool { return mgr.calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond,
		"the first call's query must be running")
	go fetch()
	time.Sleep(100 * time.Millisecond) // let the second call join the running query
	close(mgr.gate)

	for i := 0; i < 2; i++ {
		select {
		case list := <-results:
			assert.Equal(t, []api.HubInstanceIntegration{{Name: "slack", Health: "unhealthy", Version: "v1.2.3"}}, list, "call %d", i)
		case <-time.After(10 * time.Second):
			t.Fatal("call did not return")
		}
	}
	assert.Equal(t, int32(1), mgr.calls.Load(), "overlapping calls share one query")
}

// TestHealthSummaryCanReadIntegrations_AgreesWithRouteGuard: the summary's
// integration-detail check gives the same answer as the route guard on
// GET /api/v1/admin/integrations, for every kind of caller.
func TestHealthSummaryCanReadIntegrations_AgreesWithRouteGuard(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	seedRoleDefinitions(ctx, s)
	superRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	super := &store.User{ID: tid("hs-agree-super"), Email: "hs-agree-super@test.com", DisplayName: "s", Role: "admin", Status: "active"}
	require.NoError(t, s.CreateUser(ctx, super))
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superRD.ID, PrincipalType: "user", PrincipalID: super.ID,
		ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
	withRead := healthSummaryRoleUser(t, s, "hs-agree-with", []string{"hub.health.read", "hub.integrations.read"})
	without := healthSummaryRoleUser(t, s, "hs-agree-without", []string{"hub.health.read"})

	identity := func(u *store.User) Identity {
		return NewAuthenticatedUser(u.ID, u.Email, u.DisplayName, u.Role, "web")
	}
	cases := []struct {
		name string
		id   Identity
		cred *CredentialKind
		want bool
	}{
		{"super-admin session", identity(super), credKind(CredentialKindInteractive), true},
		{"custom role with integrations.read", identity(withRead), nil, true},
		{"custom role without integrations.read", identity(without), nil, false},
		{"no identity", nil, nil, false},
		{"broker identity (non-user)", NewBrokerIdentity(tid("hs-agree-broker")), nil, false},
		{"custom role with integrations.read, user access token", identity(withRead), credKind(CredentialKindUAT), true},
		{"custom role without integrations.read, user access token", identity(without), credKind(CredentialKindUAT), false},
	}
	meta := routeMetadataTable[healthSummaryIntegrationsRoute]
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqCtx := ctx
			if tc.id != nil {
				reqCtx = contextWithIdentity(reqCtx, tc.id)
			}
			if tc.cred != nil {
				reqCtx = contextWithCredentialContext(reqCtx, CredentialContext{Kind: *tc.cred})
			}
			req := httptest.NewRequest(http.MethodGet, healthSummaryIntegrationsRoute, nil).WithContext(reqCtx)
			rr := httptest.NewRecorder()
			srv.routeGuard(meta, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })(rr, req)
			guardAllowed := rr.Code == http.StatusOK

			got := srv.healthSummaryCanReadIntegrations(req)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, guardAllowed, got, "summary check and route guard disagree (guard status %d)", rr.Code)
		})
	}
}

func credKind(k CredentialKind) *CredentialKind { return &k }

// healthSummaryListCountingStore counts ListRuntimeBrokers calls made with
// the health summary's page size.
type healthSummaryListCountingStore struct {
	store.Store
	calls int
}

func (c *healthSummaryListCountingStore) ListRuntimeBrokers(ctx context.Context, f store.RuntimeBrokerFilter, o store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	if o.Limit == healthSummaryBrokerPageSize {
		c.calls++
	}
	return c.Store.ListRuntimeBrokers(ctx, f, o)
}

// TestHandleHealthSummary_OneBrokerPassForIntegrations: the runtime broker
// rows and the plugin record names come from one paged pass over the
// runtime broker table, across several pages, with the same results as
// before.
func TestHandleHealthSummary_OneBrokerPassForIntegrations(t *testing.T) {
	origPage := healthSummaryBrokerPageSize
	healthSummaryBrokerPageSize = 1
	t.Cleanup(func() { healthSummaryBrokerPageSize = origPage })

	srv, s := testServer(t)
	ctx := context.Background()
	for _, name := range []string{"pass-a", "pass-b"} {
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
			ID: tid(name), Name: name, Slug: name, Status: store.BrokerStatusOnline, LastHeartbeat: time.Now(),
		}))
	}
	createHealthSummaryPluginRecord(t, s, "discord")
	createHealthSummaryPluginRecord(t, s, "teams")
	srv.SetPluginManager(newHealthSummaryPluginDouble("telegram"))
	tickHubInstance(t, srv)

	counting := &healthSummaryListCountingStore{Store: srv.store}
	srv.store = counting

	// One standalone pass, for the expected page count.
	list, pluginNames, err := srv.healthSummaryBrokers(ctx, nil)
	require.NoError(t, err)
	onePass := counting.calls
	require.Greater(t, onePass, 1, "the listing must span several pages")
	assert.Len(t, list.Items, 2)
	assert.ElementsMatch(t, []string{"discord", "teams"}, pluginNames)

	counting.calls = 0
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.Equal(t, onePass, counting.calls, "the summary lists the runtime broker table once")

	assert.Equal(t, 2, resp.Brokers.Total)
	names := []string{}
	for _, it := range resp.Integrations {
		names = append(names, it.Name+":"+it.Health+":"+it.Reason)
	}
	assert.Equal(t, []string{
		"discord:unknown:not run by any running hub instance",
		"teams:unknown:not run by any running hub instance",
		"telegram:healthy:",
	}, names)
}

// TestHandleHealthSummary_BrokerListFailure: when the broker table cannot
// be read, the section is marked not reported, the status is unchanged and
// a fixed warning explains it.
func TestHandleHealthSummary_BrokerListFailure(t *testing.T) {
	srv, _ := testServer(t)
	srv.store = brokerListFailStore{srv.store}
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.True(t, resp.Brokers.NotReported)
	assert.Equal(t, []HealthSummaryBroker{}, resp.Brokers.Items)
	assert.Contains(t, resp.Attention, HealthAttentionItem{
		Severity: HealthAttentionWarning, Kind: HealthAttentionHubCheck,
		Subject: HealthAttentionSubject{Type: HealthSubjectHub, ID: srv.InstanceID()},
		Message: "Runtime broker data not available",
	})
	assert.NotContains(t, rr.Body.String(), "broker list exploded")
}

type brokerListFailStore struct{ store.Store }

func (brokerListFailStore) ListRuntimeBrokers(context.Context, store.RuntimeBrokerFilter, store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	return nil, errors.New("broker list exploded")
}

// TestHandleHealthSummary_GeneratedAtAndInstance: the summary carries when
// it was built and which hub instance built it.
func TestHandleHealthSummary_GeneratedAtAndInstance(t *testing.T) {
	srv, _ := testServer(t)
	before := time.Now().UTC().Add(-time.Second)
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	assert.False(t, resp.GeneratedAt.Before(before))
	assert.False(t, resp.GeneratedAt.After(time.Now().UTC().Add(time.Second)))
	assert.Equal(t, srv.InstanceID(), resp.Hub.InstanceID)
	assert.NotEmpty(t, resp.Hub.InstanceID)
	assert.Contains(t, rr.Body.String(), `"attention":[]`, "attention is a list, never null")
}

// pluginCallCountingManager counts every plugin manager call that reaches
// a plugin or lists plugins.
type pluginCallCountingManager struct {
	*healthSummaryPluginDouble
	calls atomic.Int32
}

func (m *pluginCallCountingManager) ListPlugins() []string {
	m.calls.Add(1)
	return m.healthSummaryPluginDouble.ListPlugins()
}

func (m *pluginCallCountingManager) BrokerInfo(name string) (string, string, []string, error) {
	m.calls.Add(1)
	return m.healthSummaryPluginDouble.BrokerInfo(name)
}

func (m *pluginCallCountingManager) BrokerHealthCheck(name string) (string, string, map[string]string, error) {
	m.calls.Add(1)
	return m.healthSummaryPluginDouble.BrokerHealthCheck(name)
}

// TestHandleHealthSummary_MakesNoPluginCalls: the summary handler makes no
// plugin manager call at all; the integration health it shows comes from
// the registry rows the tick wrote.
func TestHandleHealthSummary_MakesNoPluginCalls(t *testing.T) {
	srv, s := testServer(t)
	mgr := &pluginCallCountingManager{healthSummaryPluginDouble: newHealthSummaryPluginDouble("telegram", "slack")}
	mgr.health["slack"] = "unhealthy"
	srv.SetPluginManager(mgr)
	createHealthSummaryPluginRecord(t, s, "telegram")

	tickHubInstance(t, srv)
	require.Positive(t, mgr.calls.Load(), "the tick queries the plugins")
	mgr.calls.Store(0)

	for i := 0; i < 3; i++ {
		resp, _ := healthSummaryAsUser(t, srv, healthSummaryRoleUser(t, s, fmt.Sprintf("hs-nocall-%d", i), []string{"hub.health.read", "hub.integrations.read"}))
		assert.Equal(t, "unhealthy", findHealthSummaryIntegration(t, resp.Integrations, "slack").Health)
		assert.Equal(t, HealthStatusDegraded, resp.Status)
	}
	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Zero(t, mgr.calls.Load(), "the summary handler must make no plugin call")
}

// TestHandleHealthSummary_IntegrationFromOtherReplica: two hub replicas on
// one store. An integration run only by replica B shows B's real health
// when replica A serves, and a change on B shows after B's next tick.
// Either replica serves the same integration list.
func TestHandleHealthSummary_IntegrationFromOtherReplica(t *testing.T) {
	a, s := testServer(t)
	b := newHubReplica(t, s)
	require.NotEqual(t, a.InstanceID(), b.InstanceID())
	mgrB := newHealthSummaryPluginDouble("telegram")
	b.SetPluginManager(mgrB)
	createHealthSummaryPluginRecord(t, s, "telegram")

	tickHubInstance(t, a) // A runs no plugins
	tickHubInstance(t, b)

	list, _ := getHealthSummaryIntegrations(t, a)
	got := findHealthSummaryIntegration(t, list, "telegram")
	assert.Equal(t, "healthy", got.Health)
	assert.True(t, got.Connected)
	assert.Equal(t, "v1.2.3", got.Version)
	assert.Empty(t, got.Reason)
	assert.Equal(t, []string{b.InstanceID()}, got.ManagedBy)

	// B's plugin turns unhealthy: A's summary shows it after B's next
	// tick, and the status degrades on both replicas.
	mgrB.health["telegram"] = "unhealthy"
	tickHubInstance(t, b)
	for _, serving := range []*Server{a, b} {
		rr := doRequest(t, serving, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		var resp HealthSummaryResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		got := findHealthSummaryIntegration(t, resp.Integrations, "telegram")
		assert.Equal(t, "unhealthy", got.Health)
		assert.False(t, got.Connected)
		assert.Equal(t, HealthStatusDegraded, resp.Status)
		require.NotNil(t, resp.HubInstances)
		for _, it := range resp.HubInstances.Items {
			if it.ID == b.InstanceID() {
				assert.Equal(t, HealthSummaryIntegrationCounts{Total: 1, Unhealthy: 1}, it.IntegrationCounts)
			} else {
				assert.Equal(t, HealthSummaryIntegrationCounts{}, it.IntegrationCounts)
			}
		}
	}
}

// TestHandleHealthSummary_StaleReplicaReportNotUsed: a replica whose row
// is stale (no write for more than three ticks) is not used for
// integration health: its plugin is "not run by any running hub
// instance", and a plugin it reported without a plugin record is not
// listed at all.
func TestHandleHealthSummary_StaleReplicaReportNotUsed(t *testing.T) {
	now := hubInstanceT0
	rows := []store.HubInstance{
		{
			ID: "hub-a", Label: "a", Status: "healthy", StartedAt: now.Add(-time.Hour), LastSeen: now.Add(-5 * time.Second),
			Stats: json.RawMessage(`{"integrations":[{"name":"slack","health":"healthy","connected":true,"version":"1"}]}`),
		},
		{
			ID: "hub-b", Label: "b", Status: "healthy", StartedAt: now.Add(-time.Hour), LastSeen: now.Add(-46 * time.Second),
			Stats: json.RawMessage(`{"integrations":[{"name":"telegram","health":"unhealthy","connected":false,"version":"2"},` +
				`{"name":"slack","health":"unhealthy","connected":false,"version":"9"},{"name":"orphan","health":"unhealthy"}]}`),
		},
	}
	srv, s, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *fakeClockHubInstanceStore {
		return &fakeClockHubInstanceStore{Store: inner, rows: rows, now: now}
	})
	createHealthSummaryPluginRecord(t, s, "telegram")

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	names := []string{}
	for _, it := range resp.Integrations {
		names = append(names, it.Name)
	}
	assert.Equal(t, []string{"slack", "telegram"}, names, "a plugin only a stale replica reports, with no record, is not listed")
	slack := findHealthSummaryIntegration(t, resp.Integrations, "slack")
	assert.Equal(t, "healthy", slack.Health, "the stale replica's unhealthy report is not used")
	assert.Equal(t, "1", slack.Version)
	assert.Equal(t, []string{"hub-a"}, slack.ManagedBy)
	telegram := findHealthSummaryIntegration(t, resp.Integrations, "telegram")
	assert.Equal(t, HealthSummaryIntegration{
		Name: "telegram", Platform: "telegram", Health: "unknown", Reason: "not run by any running hub instance",
	}, telegram)
	assert.Equal(t, HealthStatusHealthy, resp.Status)
}

// TestHandleHealthSummary_IntegrationsWhenRegistryReadFails: when the
// registry cannot be read, plugin records are unknown with a fixed reason
// that says so, and the status does not change.
func TestHandleHealthSummary_IntegrationsWhenRegistryReadFails(t *testing.T) {
	srv, s, _, _ := testServerWithStoreFault(t, func(inner store.Store, _ *storeFaultSwitch) *fakeClockHubInstanceStore {
		return &fakeClockHubInstanceStore{Store: inner, err: errors.New("registry read failed")}
	})
	createHealthSummaryPluginRecord(t, s, "teams")

	list, body := getHealthSummaryIntegrations(t, srv)
	assert.Equal(t, []HealthSummaryIntegration{{
		Name: "teams", Platform: "teams", Health: "unknown", Reason: "hub instance data not available",
	}}, list)
	assert.NotContains(t, string(body), "registry read failed")
}

// TestMergeHealthSummaryIntegrations: the merge rules across live rows.
func TestMergeHealthSummaryIntegrations(t *testing.T) {
	now := hubInstanceT0
	row := func(id, label string, age time.Duration, integrations string) store.HubInstance {
		return store.HubInstance{
			ID: id, Label: label, StartedAt: now.Add(-time.Hour), LastSeen: now.Add(-age),
			Stats: json.RawMessage(`{"integrations":` + integrations + `}`),
		}
	}
	stopped := row("hub-s", "s", time.Second, `[{"name":"chat","health":"unhealthy"}]`)
	stoppedAt := now.Add(-time.Second)
	stopped.StoppedAt = &stoppedAt
	rows := []store.HubInstance{
		row("hub-z", "a-label", 10*time.Second, `[{"name":"chat","health":"degraded","connected":true,"version":"old"},{"name":"mail","health":"unknown","connected":true}]`),
		row("hub-y", "b-label", 2*time.Second, `[{"name":"chat","health":"healthy","connected":true,"version":"new"},{"name":"mail","health":"unknown","connected":true}]`),
		row("hub-x", "c-label", 5*time.Second, `[{"name":"chat","health":"unknown","connected":false,"version":"mid"},{"name":"ping","health":"healthy","connected":true}]`),
		stopped,
		{ID: "hub-bad", Label: "bad", LastSeen: now, Stats: json.RawMessage(`not json`)},
	}
	got := mergeHealthSummaryIntegrations(rows, now, true, []string{"chat", "zulip"})

	ts := func(age time.Duration) *time.Time { t := now.Add(-age).UTC(); return &t }
	assert.Equal(t, []HealthSummaryIntegration{
		{
			// Worst known value wins over healthy; unknown is neutral.
			// One replica not connected makes it not connected. The
			// version is the freshest report's. managed_by by label;
			// reported_at is the oldest report used.
			Name: "chat", Platform: "chat", Health: "degraded", Connected: false, Version: "new",
			ManagedBy: []string{"hub-z", "hub-y", "hub-x"}, ReportedAt: ts(10 * time.Second),
		},
		{
			// Only unknown reports: unknown, no reason.
			Name: "mail", Platform: "mail", Health: "unknown", Connected: true,
			ManagedBy: []string{"hub-z", "hub-y"}, ReportedAt: ts(10 * time.Second),
		},
		{
			Name: "ping", Platform: "ping", Health: "healthy", Connected: true,
			ManagedBy: []string{"hub-x"}, ReportedAt: ts(5 * time.Second),
		},
		{Name: "zulip", Platform: "zulip", Health: "unknown", Reason: healthIntegrationNotRunReason},
	}, got)

	// The result does not depend on row order.
	reversed := make([]store.HubInstance, len(rows))
	for i := range rows {
		reversed[len(rows)-1-i] = rows[i]
	}
	assert.Equal(t, got, mergeHealthSummaryIntegrations(reversed, now, true, []string{"zulip", "chat"}))

	// Never nil.
	assert.Equal(t, []HealthSummaryIntegration{}, mergeHealthSummaryIntegrations(nil, now, true, nil))
}
