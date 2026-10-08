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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))

	require.Len(t, resp.Integrations, 1, "a managed plugin with a record is listed once")
	assert.Equal(t, HealthSummaryIntegration{
		Name: "telegram", Platform: "telegram", Health: "healthy", Connected: true, Version: "v1.2.3",
	}, resp.Integrations[0])
	for _, b := range resp.Brokers.Items {
		assert.NotEqual(t, "plugin-telegram", b.Name, "plugin must appear only under integrations")
	}
}

func TestHandleHealthSummary_IntegrationStoppedChangesNextRefresh(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("chat-app")
	srv.SetPluginManager(mgr)

	list, _ := getHealthSummaryIntegrations(t, srv)
	got := findHealthSummaryIntegration(t, list, "chat-app")
	assert.Equal(t, "healthy", got.Health)
	assert.Equal(t, "gchat", got.Platform)
	assert.True(t, got.Connected)

	mgr.stopped["chat-app"] = true
	list, body := getHealthSummaryIntegrations(t, srv)
	got = findHealthSummaryIntegration(t, list, "chat-app")
	assert.Equal(t, "unknown", got.Health)
	assert.False(t, got.Connected)
	assert.Empty(t, got.Reason, "a managed plugin gets no not-managed reason")
	assert.NotContains(t, string(body), "connection refused", "raw errors must not leak")
}

func TestHandleHealthSummary_IntegrationUnhealthyNotConnected(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("slack")
	mgr.health["slack"] = "unhealthy"
	srv.SetPluginManager(mgr)

	list, _ := getHealthSummaryIntegrations(t, srv)
	got := findHealthSummaryIntegration(t, list, "slack")
	assert.Equal(t, "unhealthy", got.Health)
	assert.False(t, got.Connected)
}

func TestHandleHealthSummary_IntegrationNotManaged(t *testing.T) {
	srv, s := testServer(t)
	srv.SetPluginManager(newHealthSummaryPluginDouble("telegram"))
	createHealthSummaryPluginRecord(t, s, "discord")

	list, _ := getHealthSummaryIntegrations(t, srv)
	require.Len(t, list, 2)
	assert.Equal(t, "discord", list[0].Name, "sorted by name")
	assert.Equal(t, HealthSummaryIntegration{
		Name: "discord", Platform: "discord", Health: "unknown", Reason: "not managed by this hub instance",
	}, list[0])
	assert.Equal(t, "healthy", list[1].Health)
}

func TestHandleHealthSummary_IntegrationNotManagedWithoutManager(t *testing.T) {
	srv, s := testServer(t)
	createHealthSummaryPluginRecord(t, s, "teams")

	list, _ := getHealthSummaryIntegrations(t, srv)
	require.Len(t, list, 1)
	assert.Equal(t, "unknown", list[0].Health)
	assert.Equal(t, "not managed by this hub instance", list[0].Reason)
}

func TestHandleHealthSummary_IntegrationsOmitMessageAndDetails(t *testing.T) {
	srv, _ := testServer(t)
	mgr := newHealthSummaryPluginDouble("telegram")
	mgr.health["telegram"] = "degraded"
	mgr.message["telegram"] = "token sk-live-SECRETVALUE rejected"
	mgr.details["telegram"] = map[string]string{"bot_token": "SECRETDETAIL"}
	srv.SetPluginManager(mgr)

	_, body := getHealthSummaryIntegrations(t, srv)
	var raw struct {
		Integrations []map[string]json.RawMessage `json:"integrations"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Len(t, raw.Integrations, 1)
	allowed := map[string]bool{"name": true, "platform": true, "health": true, "connected": true, "version": true, "reason": true}
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
}

// TestHandleHealthSummary_IntegrationIdentityRequiresIntegrationsRead: a
// caller with hub.health.read but not hub.integrations.read gets only the
// aggregate (counts and status), with no integration names, platforms,
// versions or links anywhere in the payload. A caller with both sees the
// full detail.
func TestHandleHealthSummary_IntegrationIdentityRequiresIntegrationsRead(t *testing.T) {
	srv, s := testServer(t)
	seedHealthSummaryIntegrations(srv, s, t)
	healthOnly := healthSummaryRoleUser(t, s, "hs-health-only", []string{"hub.health.read"})
	withIntegrations := healthSummaryRoleUser(t, s, "hs-health-integrations", []string{"hub.health.read", "hub.integrations.read"})

	wantCounts := HealthSummaryIntegrationCounts{Total: 4, Healthy: 1, Degraded: 1, Unhealthy: 1, Unknown: 1}
	identity := []string{"telegram", "slack", "teams", "discord", "gchat", "v1.2.3", "plugin-", "/integrations", "not managed by this hub instance"}

	t.Run("health.read only: aggregate", func(t *testing.T) {
		resp, body := healthSummaryAsUser(t, srv, healthOnly)
		for _, frag := range identity {
			assert.NotContains(t, strings.ToLower(body), strings.ToLower(frag), "identity %q in the restricted payload", frag)
		}
		assert.Contains(t, body, `"integrations":[]`)
		assert.False(t, resp.IntegrationsDetail)
		assert.Equal(t, wantCounts, resp.IntegrationCounts)
		assert.Equal(t, HealthStatusDegraded, resp.Status, "the status does not depend on the caller")
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
		assert.Equal(t, "v1.2.3", findHealthSummaryIntegration(t, resp.Integrations, "telegram").Version)
		assert.Contains(t, body, "discord")
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
		u := healthSummaryRoleUser(t, s, "hs-anti-oracle", []string{"hub.health.read"})
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

// TestHandleHealthSummary_SlowIntegrationNotReported: a plugin that does
// not answer within the per-plugin timeout is reported as not reported
// (unknown, neutral), the summary does not wait for it, the other plugins
// are unaffected, and the hung plugin is not queried again until its first
// query returns.
func TestHandleHealthSummary_SlowIntegrationNotReported(t *testing.T) {
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

	for i := 0; i < 2; i++ {
		start := time.Now()
		rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Less(t, time.Since(start), 5*time.Second, "the summary must not block on a slow plugin")
		var resp HealthSummaryResponse
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
		assert.Equal(t, HealthSummaryIntegration{
			Name: "fast", Platform: "fast", Health: "healthy", Connected: true, Version: "v1.2.3",
		}, findHealthSummaryIntegration(t, resp.Integrations, "fast"))
		assert.Equal(t, HealthSummaryIntegration{
			Name: "slow", Platform: "slow", Health: "unknown", Reason: "health not reported in time",
		}, findHealthSummaryIntegration(t, resp.Integrations, "slow"))
		assert.Equal(t, HealthStatusHealthy, resp.Status, "not reported is neutral")
		for _, it := range resp.Attention {
			assert.NotEqual(t, HealthAttentionIntegration, it.Kind)
		}
	}
	assert.Equal(t, int32(1), mgr.calls.Load(), "a plugin whose query is still running is not queried again")

	// Once the hung query returns, a later poll queries the plugin again
	// and reports its real health.
	release()
	require.Eventually(t, func() bool {
		list, _ := getHealthSummaryIntegrations(t, srv)
		return findHealthSummaryIntegration(t, list, "slow").Health == "healthy"
	}, 5*time.Second, 20*time.Millisecond)
	assert.Equal(t, int32(2), mgr.calls.Load())
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

// TestHandleHealthSummary_OverlappingSummariesShareIntegrationHealth: two
// summaries that overlap while an unhealthy plugin's query is running both
// get its real health (and so both read degraded), from one shared query.
func TestHandleHealthSummary_OverlappingSummariesShareIntegrationHealth(t *testing.T) {
	srv, _ := testServer(t)
	mgr := &gatedHealthSummaryPluginDouble{
		healthSummaryPluginDouble: newHealthSummaryPluginDouble("slack"),
		name:                      "slack",
		gate:                      make(chan struct{}),
	}
	mgr.health["slack"] = "unhealthy"
	srv.SetPluginManager(mgr)

	type out struct {
		resp HealthSummaryResponse
		code int
	}
	results := make(chan out, 2)
	fetch := func() {
		rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
		var resp HealthSummaryResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		results <- out{resp: resp, code: rr.Code}
	}
	go fetch()
	require.Eventually(t, func() bool { return mgr.calls.Load() == 1 }, 5*time.Second, 5*time.Millisecond,
		"the first summary's query must be running")
	go fetch()
	time.Sleep(100 * time.Millisecond) // let the second summary join the running query
	close(mgr.gate)

	for i := 0; i < 2; i++ {
		select {
		case o := <-results:
			require.Equal(t, http.StatusOK, o.code)
			require.Len(t, o.resp.Integrations, 1)
			assert.Equal(t, "unhealthy", o.resp.Integrations[0].Health, "summary %d", i)
			assert.Empty(t, o.resp.Integrations[0].Reason)
			assert.Equal(t, HealthStatusDegraded, o.resp.Status, "summary %d", i)
		case <-time.After(10 * time.Second):
			t.Fatal("summary did not return")
		}
	}
	assert.Equal(t, int32(1), mgr.calls.Load(), "overlapping summaries share one query")
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
		"discord:unknown:not managed by this hub instance",
		"teams:unknown:not managed by this hub instance",
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
