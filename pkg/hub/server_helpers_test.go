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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// testDevToken is the development token used for testing.
const testDevToken = "scion_dev_test_token_for_unit_tests_1234567890"

// testServer creates a test server with an in-memory SQLite store.
// The server is configured with dev auth enabled using testDevToken.
func testServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("failed to create test store: %v", err)
	}
	// newTestStore already migrated s; a second Migrate is a no-op that
	// costs several times a fresh one.
	return testServerOnMigratedStore(t, s)
}

// testServerWithStore is testServer on a store the caller opened. It migrates
// the store first, so callers may pass an unmigrated store or one holding
// data written since its last Migrate. The store is closed when the test ends.
func testServerWithStore(t *testing.T, s store.Store) (*Server, store.Store) {
	t.Helper()
	if err := migrateTestStore(context.Background(), s); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}
	return testServerOnMigratedStore(t, s)
}

// testServerOnMigratedStore is testServerWithStore on a store that is already
// migrated with nothing written since. The store is closed when the test ends.
func testServerOnMigratedStore(t *testing.T, s store.Store) (*Server, store.Store) {
	t.Helper()
	// Remove the delegation edge backfill marker. Migrate() sets it (the
	// backfill processes zero agents and writes the completion marker).
	// Tests that create agents directly via the store bypass the HTTP handler
	// which normally creates delegation edges, so they would fail the
	// post-backfill no-edge check. Tests that specifically exercise
	// post-backfill behavior re-create the marker explicitly.
	_ = s.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")

	return testServerWithStoreConfig(t, s, testServerConfig())
}

// testServerWithStoreConfig is testServerWithStore with the given server
// config, on a store that is already migrated. The store is closed when the
// test ends.
func testServerWithStoreConfig(t *testing.T, s store.Store, cfg ServerConfig) (*Server, store.Store) {
	t.Helper()
	// Release the in-memory SQLite database to avoid OOM across many
	// tests. Registered before newTestHubServer so that, cleanups being
	// LIFO, the server shuts down before its store closes.
	t.Cleanup(func() { _ = s.Close() })
	// newTestHubServer registers Shutdown, which runs CleanupResources even
	// though Start was never called and so stops every background goroutine
	// New() starts (see TestTestServerCleanupStopsBackgroundGoroutines).
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	waitUserScopedDataSweep(t, srv)
	return srv, s
}

// testServerConfig is the server config testServerWithStore passes to New().
func testServerConfig() ServerConfig {
	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken // Enable dev auth for testing
	// Never build real Cloud Logging clients from an ambient GCP project
	// env var (ptone/scion#3188).
	cfg.DisableCloudLogQuery = true
	cfg.DevUserConfig = DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}
	return cfg
}

// testServerWithBrokerAuth creates a test server with broker auth enabled.
func testServerWithBrokerAuth(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() {
		// Shutdown runs CleanupResources; see testServerWithStore.
		_ = srv.Shutdown(context.Background())
		_ = s.Close()
	})
	return srv, s
}

// waitUserScopedDataSweep waits for the startup sweep New() starts in the
// background to end. The sweep reads srv.store, so a test helper calls this
// before returning a server whose srv.store a test may replace.
func waitUserScopedDataSweep(t testing.TB, srv *Server) {
	t.Helper()
	select {
	case <-srv.userScopedDataSweepDone:
	case <-time.After(time.Minute):
		t.Fatal("startup sweep of user-scope data did not finish")
	}
}

// doRequest performs an HTTP request against the test server.
// It automatically includes the dev auth token for authenticated endpoints.
func doRequest(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Add dev auth token for authenticated endpoints
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doRequestNoAuth performs an HTTP request without authentication.
// Use this for testing unauthenticated access or auth endpoints themselves.
func doRequestNoAuth(t *testing.T, srv *Server, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("failed to marshal body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// doRequestRaw performs an HTTP request with raw bytes as the body.
// Useful for testing malformed request bodies.
func doRequestRaw(t *testing.T, srv *Server, method, path string, body []byte, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+testDevToken)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// grantDevUserRuntimeBrokerAccess creates a custom role with runtime_broker.*
// permissions and binds it to the dev user. This is needed because the
// inline authz checks in handleBrokerHeartbeat and handleBrokerSecretByKey
// use Resource{Type: "runtime_broker"} which does not match the canonical
// "broker.*" permissions in the registry (getRuntimeBroker and
// getBrokerProjects have already been aligned to the canonical type; these
// two remain to be aligned separately). The dev user's super-admin role only
// includes registry permissions.
func grantDevUserRuntimeBrokerAccess(t *testing.T, s store.Store) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:      "runtime-broker-compat",
		ScopeType: store.RoleScopeSystem,
		Permissions: []string{
			"runtime_broker.read",
			"runtime_broker.update",
			"runtime_broker.delete",
			"runtime_broker.list",
		},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      DevUserID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// grantSuperAdminRole binds the seeded super-admin role definition to the
// given user. Under the CO1 authorization cutover the AK1 kernel only
// evaluates role bindings, so User.Role = "admin" alone is insufficient.
func grantSuperAdminRole(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err, "super-admin role definition must exist")
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create super-admin role binding: %v", err)
	}
}
