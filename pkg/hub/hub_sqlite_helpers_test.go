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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"
)

// setHubAgentDefaults sets the hub operational agent_defaults on a test server,
// simulating what ApplySnapshot does in Postgres mode. Writes under s.mu, the
// same lock hubAgentDefaults() reads under.
func setHubAgentDefaults(srv *Server, d opsettings.AgentDefaultsSettings) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	srv.config.AgentDefaults = d
}

// recordAttr reads a single attribute off a captured record.
func recordAttr(r slog.Record, key string) (slog.Value, bool) {
	var found slog.Value
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found, ok = a.Value, true
			return false
		}
		return true
	})
	return found, ok
}

// findMockAgent returns the agent with the given slug from the mock store.
func findMockAgent(ms *mockScheduledEventStore, slug string) *store.Agent {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	for _, a := range ms.agents {
		if a.Slug == slug {
			return a
		}
	}
	return nil
}

// hubDefaultsDispatchAgent returns a minimal agent record for the dispatch
// tests below. AppliedConfig must be non-nil: buildCreateRequest only builds
// req.Config when the agent has one.
func hubDefaultsDispatchAgent() *store.Agent {
	return &store.Agent{
		ID:              tid("agent-1"),
		Name:            "test-agent",
		Slug:            "test-agent",
		OwnerID:         tid("user-1"),
		RuntimeBrokerID: tid("host-1"),
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
}

// hubConfigTokenUser creates an active user holding the named system role
// and hub membership, and returns the user ID.
func hubConfigTokenUser(t *testing.T, s store.Store, name, systemRole string) string {
	t.Helper()
	id := tid(name)
	userRole := "member"
	if systemRole == store.SystemRoleSuperAdmin {
		userRole = "admin"
	}
	createTestUserWithRole(t, s, id, id+"@test.com", userRole, systemRole)
	ensureHubMembership(context.Background(), s, id)
	return id
}

// mintHubConfigToken mints a real token for userID through
// UserAccessTokenService over a session.
func mintHubConfigToken(t *testing.T, srv *Server, userID string, boundary TokenBoundary, scopes ...string) string {
	t.Helper()
	key, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
		UserID: userID, Name: "hct-" + tid("tok"), Boundary: boundary, Scopes: scopes,
	})
	require.NoError(t, err, "mint %s token with %v", boundary.Kind, scopes)
	return key
}

// gcpIdentitySettingsSA registers a service account for the settings tests.
func gcpIdentitySettingsSA(t *testing.T, s store.Store, scope, scopeID string, verified bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     scope,
		ScopeID:   scopeID,
		Email:     fmt.Sprintf("sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: "someone",
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// newGCPIdentitySettingsStore returns a migrated in-memory store.
func newGCPIdentitySettingsStore(t *testing.T) store.Store {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Skipf("skipping: test store unavailable (%v)", err)
	}
	return s
}

// fileModeGCPIdentityServer points the global settings directory at a temp
// HOME seeded with a settings.yaml that has a server key (as every hub's does)
// and returns a file-mode server backed by a real store.
func fileModeGCPIdentityServer(t *testing.T) (*Server, store.Store, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o755))
	settingsPath := filepath.Join(globalDir, "settings.yaml")
	require.NoError(t, os.WriteFile(settingsPath, []byte(
		"schema_version: \"1\"\ndefault_timezone: UTC\nserver:\n  hub:\n    port: 9810\n"), 0o644))

	s := newGCPIdentitySettingsStore(t)
	srv := &Server{
		dbDriver:    "sqlite",
		maintenance: NewMaintenanceState(false, ""),
		store:       s,
	}
	return srv, s, settingsPath
}

func readSettingsYAML(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var raw map[string]interface{}
	require.NoError(t, yamlv3.Unmarshal(data, &raw))
	return raw
}

// markBrokerEmbedded records the fixture's broker as the hub's embedded
// (co-located) broker — the one a single-node VM dispatches to — the same way
// server startup does, via the server-held embedded broker ID.
func markBrokerEmbedded(t *testing.T, f *bypassAgentsFixture) {
	t.Helper()
	f.srv.SetEmbeddedBrokerID(f.broker.ID)
}

// markBrokerRuntimeProfile records a single runtime profile of the given
// type on the fixture's broker, standing in for a broker whose settings
// define no profiles at all (buildStoreBrokerProfiles's single-"default"
// fallback, cmd/server_broker.go). Tests use this for the simple
// one-profile case; markBrokerStockProfiles below covers the realistic
// multi-profile embedded broker.
func markBrokerRuntimeProfile(t *testing.T, f *bypassAgentsFixture, profileType string) {
	t.Helper()
	ctx := context.Background()
	b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	b.Profiles = []store.BrokerProfile{{Name: "default", Type: profileType, Available: true}}
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
}

// markBrokerStockProfiles records the stock embedded-broker profile shape
// (pkg/config/embeds/default_settings.yaml): a "local" profile (docker) and
// a "remote" profile (kubernetes), plus DefaultProfile set to
// defaultProfileName — mirroring what a real single-node VM's embedded
// broker reports at registration (registerGlobalProjectAndBroker,
// cmd/server_broker.go). Every stock embedded broker looks like this: the
// hub default must keep working here with no explicit profile, not just on
// the single-profile shape markBrokerRuntimeProfile sets up.
func markBrokerStockProfiles(t *testing.T, f *bypassAgentsFixture, defaultProfileName string) {
	t.Helper()
	ctx := context.Background()
	b, err := f.store.GetRuntimeBroker(ctx, f.broker.ID)
	require.NoError(t, err)
	b.Profiles = []store.BrokerProfile{
		{Name: "local", Type: "docker", Available: true},
		{Name: "remote", Type: "kubernetes", Available: true},
	}
	b.DefaultProfile = defaultProfileName
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, b))
}

// createdAgentRecord creates an agent with the given request and returns the
// persisted record, for tests that need to inspect more than the resolved
// GCP identity — e.g. the pinned AppliedConfig.Profile.
func createdAgentRecord(t *testing.T, f *bypassAgentsFixture, req CreateAgentRequest) *store.Agent {
	t.Helper()
	rec := createAgentAsOwner(t, f, req)
	require.Equal(t, http.StatusCreated, rec.Code,
		"agent creation should succeed; got: %s", rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	got, err := f.store.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	return got
}

// captureDefaultSlog routes the default slog logger into a capturing handler
// for the duration of the test.
func captureDefaultSlog(t *testing.T) *levelCapturingHandler {
	t.Helper()
	h := &levelCapturingHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// newStartupNamedServer builds a Server through New with hubName as the
// startup-resolved ServerConfig.HubName, the name resolved at startup
// (LoadGlobalConfig(serverConfigPath)).
func newStartupNamedServer(t *testing.T, hubName string) *Server {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	cfg := DefaultServerConfig()
	cfg.HubName = hubName
	srv, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)
	return srv
}

const hubPSHPath = "/api/v1/pre-start-hooks"

// doRequestWithToken performs an HTTP request using an arbitrary bearer token.
func doRequestWithToken(t *testing.T, srv *Server, token, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
