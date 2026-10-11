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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// newBareTestStore builds a fresh, migrated sqlite store without wiring a
// Server around it, so it can be wrapped before the first New() call. The
// store is closed on cleanup, matching testServer's "avoid OOM across many
// tests" convention, regardless of how many layers wrap it afterward.
func newBareTestStore(t *testing.T) store.Store {
	t.Helper()
	base, err := newTestStore(t, ":memory:")
	if err != nil {
		if strings.Contains(err.Error(), "sqlite driver not registered") {
			t.Skip("Skipping test because sqlite driver is not registered (build with -tags sqlite to enable)")
		}
		t.Fatalf("newTestStore: %v", err)
	}
	_ = base.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")
	return base
}

// newTestServerFromStore wires a full-chain server (dev super-admin auth,
// live OperationalSettings) around an arbitrary store.Store, so a test can
// substitute a wrapped store before wiring.
func newTestServerFromStore(t *testing.T, s store.Store, reg *experiments.Registry) *Server {
	t.Helper()
	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.DevUserConfig = DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	cfg.Experiments = reg
	srv, err := newTestHubServer(t, cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")

	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.SetOperationalSettings(ops)
	return srv
}

// testServerWithOps builds a full-chain-capable server (real sqlite store,
// dev super-admin auth) with a live OperationalSettings, matching how
// cmd/server_foreground.go wires every driver in production. When reg is
// non-nil, it replaces the production experiments registry.
func testServerWithOps(t *testing.T, reg *experiments.Registry) (*Server, store.Store) {
	t.Helper()
	s := newBareTestStore(t)
	return newTestServerFromStore(t, s, reg), s
}

// errorCode decodes the standard {"error":{"code":...}} envelope and returns
// the code, so tests can distinguish (for example) revision_conflict from
// experiments_malformed instead of asserting on HTTP status alone.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	code, _ := errorCodeAndDetails(t, rec)
	return code
}

// errorCodeAndDetails decodes the standard {"error":{"code":...,"details":
// {...}}} envelope, returning the code and the raw details object (nil when
// absent).
func errorCodeAndDetails(t *testing.T, rec *httptest.ResponseRecorder) (string, map[string]interface{}) {
	t.Helper()
	var body struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v (%s)", err, rec.Body.String())
	}
	if body.Error.Code == "" {
		t.Fatalf("expected a non-empty error code, got %s", rec.Body.String())
	}
	return body.Error.Code, body.Error.Details
}

// createTestUser creates and hub-memberships a plain member user.
func createTestUser(t *testing.T, s store.Store, id, email string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{ID: tid(id), Email: email, DisplayName: email, Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ensureHubMembership(ctx, s, u.ID)
	return u
}
