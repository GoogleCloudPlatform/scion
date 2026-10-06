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

// API tests for GET /api/v1/experiments and GET|PUT|DELETE
// /api/v1/admin/experiments (ptone/scion#2217). The HTTP tests in this file
// run through srv.Handler(), including UnifiedAuthMiddleware and the route
// guard, using a real sqlite-backed store and OperationalSettings. A few unit
// tests call the route table, the response builder, or the handler directly
// where the chain adds nothing (nil OperationalSettings, the post-write
// attribution fallback).
package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
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
	if err := base.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
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
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.SetOperationalSettings(ops)
	return srv
}

// failingUpsertStore wraps a real store.Store and forces UpsertHubSetting to
// return store.ErrRevisionConflict for one section, regardless of the
// requested revision. It exercises the handler's error-mapping path
// (errors.Is(err, store.ErrRevisionConflict) → 409) independent of whether a
// real revision mismatch occurred.
type failingUpsertStore struct {
	store.Store
	failSection string
}

func (f *failingUpsertStore) UpsertHubSetting(ctx context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	if section == f.failSection {
		return nil, store.ErrRevisionConflict
	}
	return f.Store.UpsertHubSetting(ctx, section, value, updatedBy, expectedRevision, origin)
}

// DB forwards to the wrapped store's raw *sql.DB when it exposes one, so
// New()'s D4 membership-index migration (which type-asserts for it) still
// runs against the real store instead of failing closed on the wrapper.
func (f *failingUpsertStore) DB() *sql.DB {
	if p, ok := f.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

// newTestServerWithFailingUpsert builds a full-chain server whose store
// always reports a revision conflict when writing to failSection, to exercise
// the store-race 409 mapping (ptone/scion#2217).
func newTestServerWithFailingUpsert(t *testing.T, failSection string) (*Server, store.Store) {
	t.Helper()
	base := newBareTestStore(t)
	wrapped := &failingUpsertStore{Store: base, failSection: failSection}
	srv := newTestServerFromStore(t, wrapped, nil)
	return srv, wrapped
}

// malformedSectionStore wraps a real store.Store and reports a fixed
// document for one section on every read, regardless of what (if anything)
// is really stored there. The real store's write path validates JSON before
// persisting it (the same as any other repository), so genuinely malformed
// bytes cannot be seeded through UpsertHubSetting; this fakes the read side
// instead, without needing the real store to hold invalid JSON.
type malformedSectionStore struct {
	store.Store
	section  string
	raw      json.RawMessage
	revision int64
}

func (m *malformedSectionStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if section == m.section {
		return &store.HubSetting{ID: section, Section: section, Value: m.raw, Revision: m.revision}, nil
	}
	return m.Store.GetHubSetting(ctx, section)
}

func (m *malformedSectionStore) ListHubSettings(ctx context.Context) ([]store.HubSetting, error) {
	rows, err := m.Store.ListHubSettings(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]store.HubSetting, 0, len(rows)+1)
	for _, r := range rows {
		if r.Section == m.section {
			continue
		}
		out = append(out, r)
	}
	out = append(out, store.HubSetting{ID: m.section, Section: m.section, Value: m.raw, Revision: m.revision})
	return out, nil
}

// DB forwards to the wrapped store's raw *sql.DB when it exposes one, so
// New()'s D4 membership-index migration (which type-asserts for it) still
// runs against the real store instead of failing closed on the wrapper.
func (m *malformedSectionStore) DB() *sql.DB {
	if p, ok := m.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

// newTestServerWithMalformedSection builds a full-chain server that reports
// section as malformed (revision 0, matching the real store's true "absent"
// state) on every read, so a compare-and-set recovery write lands as a
// genuine create against the underlying store.
func newTestServerWithMalformedSection(t *testing.T, reg *experiments.Registry, section string, raw json.RawMessage) (*Server, *malformedSectionStore) {
	t.Helper()
	base := newBareTestStore(t)
	wrapped := &malformedSectionStore{Store: base, section: section, raw: raw, revision: 0}
	srv := newTestServerFromStore(t, wrapped, reg)
	return srv, wrapped
}

// erroringGetStore wraps a real store.Store and forces GetHubSetting to
// return a generic error (not store.ErrNotFound) for one section, to
// exercise the "authoritative read failed" 503 path independent of the
// malformed-row and stale-replica paths, which both need a real or faked row.
type erroringGetStore struct {
	store.Store
	failSection string
}

var errSimulatedStoreFailure = errors.New("simulated store failure")

func (e *erroringGetStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if section == e.failSection {
		return nil, errSimulatedStoreFailure
	}
	return e.Store.GetHubSetting(ctx, section)
}

func (e *erroringGetStore) DB() *sql.DB {
	if p, ok := e.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

// newTestServerWithErroringGet builds a full-chain server whose store always
// fails to read failSection, to exercise the 503 settings_unavailable path
// on the authoritative (store) read used by PUT and DELETE.
func newTestServerWithErroringGet(t *testing.T, failSection string) *Server {
	t.Helper()
	base := newBareTestStore(t)
	wrapped := &erroringGetStore{Store: base, failSection: failSection}
	return newTestServerFromStore(t, wrapped, nil)
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

// refreshOps re-reads the store into the cache after a test writes a row
// directly through the store, bypassing the handler.
func refreshOps(t *testing.T, srv *Server) {
	t.Helper()
	if _, err := srv.GetOperationalSettings().Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
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

// findEntry returns a pointer to the named entry in resp.Experiments, or nil.
func findEntry(resp adminExperimentsResponse, name string) *adminExperimentEntry {
	for i := range resp.Experiments {
		if resp.Experiments[i].Name == name {
			return &resp.Experiments[i]
		}
	}
	return nil
}

// grantSystemPermissions creates a system-scoped role holding exactly the
// given permissions and binds it to userID, for exercising the authorization
// matrix independent of the curated hub-admin role.
func grantSystemPermissions(t *testing.T, s store.Store, userID, roleName string, perms []string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        roleName,
		Description: "test role for experiments authorization",
		ScopeType:   store.RoleScopeSystem,
		Permissions: perms,
	})
	if err != nil {
		t.Fatalf("CreateRoleDefinition: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}
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

// --- GET /api/v1/experiments ---

func TestHandleExperiments_FullChain(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)

	t.Run("real credential returns the resolved map", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/experiments", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body experimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if v, ok := body.Experiments["web.terminal_workspace"]; !ok || !v {
			t.Errorf("expected web.terminal_workspace=true, got %v (present=%v)", v, ok)
		}
	})

	t.Run("no credential is unauthorized", func(t *testing.T) {
		rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/experiments", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("invalid token is unauthorized", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/experiments", nil)
		req.Header.Set("Authorization", "Bearer not-a-real-token")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("method not allowed", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/experiments", nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("expected 405, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("settings/public is unchanged for anonymous and invalid-token callers", func(t *testing.T) {
		for _, rec := range []*httptest.ResponseRecorder{
			doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/settings/public", nil),
			func() *httptest.ResponseRecorder {
				req := httptest.NewRequest(http.MethodGet, "/api/v1/settings/public", nil)
				req.Header.Set("Authorization", "Bearer not-a-real-token")
				rr := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rr, req)
				return rr
			}(),
		} {
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var body map[string]interface{}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, ok := body["experiments"]; ok {
				t.Error("settings/public must not carry an experiments field")
			}
		}
	})
}

// --- GET /api/v1/admin/experiments ---

func TestHandleAdminExperiments_NoRowDefaults(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body adminExperimentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Malformed {
		t.Error("expected malformed=false with no row")
	}
	if body.UpdatedAt != nil || body.UpdatedBy != nil {
		t.Errorf("expected nil updated_at/updated_by with no row, got %v / %v", body.UpdatedAt, body.UpdatedBy)
	}
	var found *adminExperimentEntry
	for i := range body.Experiments {
		if body.Experiments[i].Name == "web.terminal_workspace" {
			found = &body.Experiments[i]
		}
	}
	if found == nil {
		t.Fatal("expected web.terminal_workspace in the listing")
	}
	if !found.Default || found.Override != nil || !found.Enabled {
		t.Errorf("expected default=true, override=nil, enabled=true, got default=%v override=%v enabled=%v",
			found.Default, found.Override, found.Enabled)
	}
}

func TestHandleAdminExperiments_AuthorizationMatrix(t *testing.T) {
	srv, s := testServerWithOps(t, nil)

	withPerm := createTestUser(t, s, "exp-with-perm", "exp-with-perm@test.com")
	grantSystemPermissions(t, s, withPerm.ID, "test-experiments-holder", []string{"hub.experiments.update"})

	withoutPerm := createTestUser(t, s, "exp-without-perm", "exp-without-perm@test.com")
	grantSystemPermissions(t, s, withoutPerm.ID, "test-experiments-non-holder", []string{"hub.config.read"})

	plainMember := createTestUser(t, s, "exp-plain-member", "exp-plain-member@test.com")

	t.Run("super-admin allowed", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("scoped admin with hub.experiments.update allowed", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, withPerm, http.MethodGet, "/api/v1/admin/experiments", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("scoped admin without hub.experiments.update forbidden", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, withoutPerm, http.MethodGet, "/api/v1/admin/experiments", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("non-admin forbidden", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, plainMember, http.MethodGet, "/api/v1/admin/experiments", nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unauthenticated unauthorized", func(t *testing.T) {
		rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAdminExperimentsRouteMetadataExists(t *testing.T) {
	meta, ok := routeMetadataTable["/api/v1/admin/experiments"]
	if !ok {
		t.Fatal("route metadata entry for /api/v1/admin/experiments not found")
	}
	if meta.Classification != RouteHubAdmin {
		t.Errorf("expected RouteHubAdmin classification, got %v", meta.Classification)
	}
	if meta.Permission != "hub.experiments.update" {
		t.Errorf("expected permission hub.experiments.update, got %q", meta.Permission)
	}
}

// --- PUT /api/v1/admin/experiments ---

func TestHandleAdminExperiments_RevisionChecks(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()

	t.Run("missing expected_revision is rejected", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "validation_failed" {
			t.Errorf("expected code=validation_failed, got %q", code)
		}
	})

	t.Run("stale expected_revision is a conflict", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 999})
		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "revision_conflict" {
			t.Errorf("expected code=revision_conflict, got %q", code)
		}
	})

	var lastRevision int64
	t.Run("successful write returns 200 and increases the revision, and persists without a restart", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 0})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Revision <= 0 {
			t.Errorf("expected revision > 0, got %d", body.Revision)
		}
		if body.UpdatedAt == nil || body.UpdatedBy == nil || *body.UpdatedBy != "dev@localhost" {
			t.Errorf("expected updated_by=dev@localhost, got %v", body.UpdatedBy)
		}
		lastRevision = body.Revision

		// The response's updated_at/updated_by must be the write's own,
		// not merely non-nil: compare against the row the write produced.
		row, err := s.GetHubSetting(ctx, "experiments")
		if err != nil {
			t.Fatalf("GetHubSetting: %v", err)
		}
		if body.UpdatedAt == nil || !body.UpdatedAt.Equal(row.UpdatedAt) {
			t.Errorf("expected updated_at=%v (the write's own), got %v", row.UpdatedAt, body.UpdatedAt)
		}
		if body.UpdatedBy == nil || *body.UpdatedBy != row.UpdatedBy {
			t.Errorf("expected updated_by=%q (the write's own), got %v", row.UpdatedBy, body.UpdatedBy)
		}

		// override and enabled must reflect the stored override, not the
		// registry default, in both the PUT response and a later admin GET.
		putEntry := findEntry(body, "web.terminal_workspace")
		if putEntry == nil {
			t.Fatal("expected web.terminal_workspace in the PUT response")
		}
		if putEntry.Override == nil || *putEntry.Override != false {
			t.Errorf("expected override=false in the PUT response, got %v", putEntry.Override)
		}
		if putEntry.Enabled {
			t.Error("expected enabled=false in the PUT response, reflecting the stored override")
		}

		get := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
		var getBody adminExperimentsResponse
		if err := json.Unmarshal(get.Body.Bytes(), &getBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		getEntry := findEntry(getBody, "web.terminal_workspace")
		if getEntry == nil {
			t.Fatal("expected web.terminal_workspace in the admin GET response")
		}
		if getEntry.Override == nil || *getEntry.Override != false {
			t.Errorf("expected override=false in the admin GET response, got %v", getEntry.Override)
		}
		if getEntry.Enabled {
			t.Error("expected enabled=false in the admin GET response, reflecting the stored override")
		}

		// GET /api/v1/experiments reflects the change without a restart.
		exp := doRequest(t, srv, http.MethodGet, "/api/v1/experiments", nil)
		var expBody experimentsResponse
		if err := json.Unmarshal(exp.Body.Bytes(), &expBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if expBody.Experiments["web.terminal_workspace"] {
			t.Error("expected web.terminal_workspace=false after the PUT")
		}
	})

	t.Run("null for a registered name resets it to the default", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": nil}, "expected_revision": lastRevision})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		var found *adminExperimentEntry
		for i := range body.Experiments {
			if body.Experiments[i].Name == "web.terminal_workspace" {
				found = &body.Experiments[i]
			}
		}
		if found == nil {
			t.Fatal("expected web.terminal_workspace in the listing")
		}
		if found.Override != nil {
			t.Errorf("expected override=nil after resetting to the default, got %v", *found.Override)
		}
		if !found.Enabled {
			t.Error("expected enabled=true (the registry default) after resetting to the default")
		}

		exp := doRequest(t, srv, http.MethodGet, "/api/v1/experiments", nil)
		var expBody experimentsResponse
		if err := json.Unmarshal(exp.Body.Bytes(), &expBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !expBody.Experiments["web.terminal_workspace"] {
			t.Error("expected web.terminal_workspace=true (the default) after resetting to the default")
		}
	})
}

// TestHandleAdminExperiments_ServerLayerOverrideReflectedInEnabled covers a
// server-layer test-registry experiment (ptone/scion#2217): an override on
// hub.test_gate (default on) must show up as override/enabled in the admin
// PUT and GET responses, the same as the web-only case in
// TestHandleAdminExperiments_RevisionChecks.
func TestHandleAdminExperiments_ServerLayerOverrideReflectedInEnabled(t *testing.T) {
	srv, _ := testServerWithOps(t, testRegistry(t))

	put := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
		map[string]interface{}{"overrides": map[string]interface{}{"hub.test_gate": false}, "expected_revision": 0})
	if put.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", put.Code, put.Body.String())
	}
	var putBody adminExperimentsResponse
	if err := json.Unmarshal(put.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	putEntry := findEntry(putBody, "hub.test_gate")
	if putEntry == nil {
		t.Fatal("expected hub.test_gate in the PUT response")
	}
	if putEntry.Override == nil || *putEntry.Override != false {
		t.Errorf("expected override=false in the PUT response, got %v", putEntry.Override)
	}
	if putEntry.Enabled {
		t.Error("expected enabled=false in the PUT response, reflecting the stored override")
	}

	get := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
	var getBody adminExperimentsResponse
	if err := json.Unmarshal(get.Body.Bytes(), &getBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	getEntry := findEntry(getBody, "hub.test_gate")
	if getEntry == nil {
		t.Fatal("expected hub.test_gate in the admin GET response")
	}
	if getEntry.Override == nil || *getEntry.Override != false {
		t.Errorf("expected override=false in the admin GET response, got %v", getEntry.Override)
	}
	if getEntry.Enabled {
		t.Error("expected enabled=false in the admin GET response, reflecting the stored override")
	}

	// requireExperiment must observe the same override at its decision point.
	if srv.experimentEnabled("hub.test_gate") {
		t.Error("expected hub.test_gate to resolve to false via experimentEnabled after the override")
	}
}

// TestBuildAdminExperimentsResponseAfterWrite_FallbackAttribution covers the
// caller/now fallback in buildAdminExperimentsResponseAfterWrite directly,
// without a race harness: the branch depends only on the cache state and the
// revision argument, both of which this test controls.
//
// Every full-chain write test in this file calls buildAdminExperimentsResponseAfterWrite
// with the exact revision Update() just produced, so they all take the
// refreshed-snapshot branch. In production the fallback fires when something
// else (a concurrent write on the same replica, or a subscription/poll
// refresh) lands in the cache between Update()'s cache write and this
// function's own ExperimentsSnapshot() call — a race no full-chain test can
// force deterministically. Calling the function directly with a revision
// that does not match the cache reproduces exactly that condition.
func TestBuildAdminExperimentsResponseAfterWrite_FallbackAttribution(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)

	// Establish a cached snapshot attributed to a different, distinguishable
	// writer at a known revision, so the assertions below can tell the
	// fallback's caller/now values apart from anything the snapshot holds.
	put := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
		map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 0})
	if put.Code != http.StatusOK {
		t.Fatalf("setup PUT expected 200, got %d: %s", put.Code, put.Body.String())
	}
	var putBody adminExperimentsResponse
	if err := json.Unmarshal(put.Body.Bytes(), &putBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cachedRevision := putBody.Revision // written by dev@localhost

	t.Run("revision mismatch falls back to the caller and now", func(t *testing.T) {
		before := time.Now()
		resp := srv.buildAdminExperimentsResponseAfterWrite(map[string]bool{}, cachedRevision+1, "caller@example.com", srv.GetOperationalSettings())
		after := time.Now()

		if resp.Revision != cachedRevision+1 {
			t.Errorf("expected revision=%d, got %d", cachedRevision+1, resp.Revision)
		}
		if resp.UpdatedBy == nil || *resp.UpdatedBy != "caller@example.com" {
			t.Fatalf("expected updated_by=caller@example.com (the fallback), got %v (mutant: always uses the mismatched snapshot's writer, dev@localhost)", resp.UpdatedBy)
		}
		if resp.UpdatedAt == nil {
			t.Fatal("expected a non-nil updated_at")
		}
		if resp.UpdatedAt.Before(before) || resp.UpdatedAt.After(after) {
			t.Errorf("expected updated_at within [%v, %v] (the fallback's time.Now()), got %v (mutant: a zero time or the mismatched snapshot's timestamp)", before, after, *resp.UpdatedAt)
		}
	})

	t.Run("no row also falls back to the caller and now", func(t *testing.T) {
		emptySrv, _ := testServerWithOps(t, nil) // fresh server: no row, Present=false
		before := time.Now()
		resp := emptySrv.buildAdminExperimentsResponseAfterWrite(map[string]bool{}, 1, "caller@example.com", emptySrv.GetOperationalSettings())
		after := time.Now()

		if resp.UpdatedBy == nil || *resp.UpdatedBy != "caller@example.com" {
			t.Errorf("expected updated_by=caller@example.com, got %v", resp.UpdatedBy)
		}
		if resp.UpdatedAt == nil || resp.UpdatedAt.Before(before) || resp.UpdatedAt.After(after) {
			t.Errorf("expected updated_at within [%v, %v], got %v", before, after, resp.UpdatedAt)
		}
	})
}

func TestHandleAdminExperiments_StaleReplica(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()

	// A second replica writes directly through the store, bypassing srv's
	// cache. srv's cache still believes revision 0 (no row).
	if _, err := s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(`{"overrides":{"web.terminal_workspace":false}}`), "other-replica", 0, "managed"); err != nil {
		t.Fatalf("UpsertHubSetting: %v", err)
	}

	t.Run("PUT with the cache's stale revision is a conflict", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": true}, "expected_revision": 0})
		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "revision_conflict" {
			t.Errorf("expected code=revision_conflict, got %q", code)
		}
	})

	t.Run("PUT with the store's current revision succeeds", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": true}, "expected_revision": 1})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

func TestHandleAdminExperiments_StoreRace(t *testing.T) {
	srv, _ := newTestServerWithFailingUpsert(t, "experiments")

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
		map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 0})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 from a store race, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "revision_conflict" {
		t.Errorf("expected code=revision_conflict, got %q", code)
	}
}

// TestHandleAdminExperiments_StoreReadError covers the 503 path on the
// authoritative store read used by PUT and DELETE, distinct from the
// malformed-row (409) and stale-replica (409) paths: here the read itself
// fails, not just its content or its revision.
func TestHandleAdminExperiments_StoreReadError(t *testing.T) {
	srv := newTestServerWithErroringGet(t, "experiments")

	t.Run("PUT", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 0})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "settings_unavailable" {
			t.Errorf("expected code=settings_unavailable, got %q", code)
		}
	})

	t.Run("DELETE", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/experiments",
			map[string]interface{}{"expected_revision": 0})
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "settings_unavailable" {
			t.Errorf("expected code=settings_unavailable, got %q", code)
		}
	})
}

// TestHandleAdminExperiments_NilOperationalSettings covers the 503 path taken
// when OperationalSettings itself is unavailable (initialization failed),
// for all three methods. A bare &Server{} has a nil OperationalSettings by
// construction (GetOperationalSettings loads a zero-value atomic.Pointer),
// so this needs no store at all, unlike TestHandleAdminExperiments_StoreReadError
// (store reachable, read fails) or the malformed-row tests (store reachable,
// content unreadable).
func TestHandleAdminExperiments_NilOperationalSettings(t *testing.T) {
	srv := &Server{}

	tests := []struct {
		name   string
		method string
		body   string
	}{
		{"GET", http.MethodGet, ""},
		{"PUT", http.MethodPut, `{"overrides":{"web.terminal_workspace":false},"expected_revision":0}`},
		{"DELETE", http.MethodDelete, `{"expected_revision":0}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/v1/admin/experiments", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			srv.handleAdminExperiments(rec, req)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
			}
			if code := errorCode(t, rec); code != "settings_unavailable" {
				t.Errorf("expected code=settings_unavailable, got %q", code)
			}
		})
	}
}

// TestHandleAdminExperiments_ReviewOverdueField asserts review_overdue at the
// response level (pkg/experiments' TestReviewOverdue covers the method
// itself, but nothing before this test called it through an admin response).
func TestHandleAdminExperiments_ReviewOverdueField(t *testing.T) {
	overdue := testExperiment("web.overdue_thing", true, experiments.LayerWeb)
	overdue.ReviewBy = "2020-01-01"
	fresh := testExperiment("web.fresh_thing", true, experiments.LayerWeb)
	fresh.ReviewBy = "2099-01-01"
	reg, err := experiments.NewRegistry([]experiments.Experiment{overdue, fresh}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv, _ := testServerWithOps(t, reg)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body adminExperimentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	overdueEntry := findEntry(body, "web.overdue_thing")
	if overdueEntry == nil || !overdueEntry.ReviewOverdue {
		t.Errorf("expected web.overdue_thing review_overdue=true, got %v", overdueEntry)
	}
	freshEntry := findEntry(body, "web.fresh_thing")
	if freshEntry == nil || freshEntry.ReviewOverdue {
		t.Errorf("expected web.fresh_thing review_overdue=false, got %v", freshEntry)
	}
}

func TestHandleAdminExperiments_NameValidation(t *testing.T) {
	srv, _ := testServerWithOps(t, nil)

	tests := []struct {
		name       string
		overrides  map[string]interface{}
		wantStatus int
		// wantCode and wantNameDetail are checked only when wantStatus is a
		// 4xx; both empty means "don't check" (used for the 200 case).
		wantCode       string
		wantNameDetail string
	}{
		{"true for unknown name", map[string]interface{}{"hub.future_thing": true}, http.StatusBadRequest, "validation_failed", "hub.future_thing"},
		{"false for retired name", map[string]interface{}{"web.access_boundaries_read": false}, http.StatusBadRequest, "validation_failed", "web.access_boundaries_read"},
		{"null for pattern-valid unknown name is accepted (no-op)", map[string]interface{}{"hub.future_thing": nil}, http.StatusOK, "", ""},
		{"null for pattern-invalid key", map[string]interface{}{"not a valid name": nil}, http.StatusBadRequest, "validation_failed", "not a valid name"},
		{"non-boolean value", map[string]interface{}{"web.terminal_workspace": "nope"}, http.StatusBadRequest, "validation_failed", ""},
		{"empty overrides", map[string]interface{}{}, http.StatusBadRequest, "validation_failed", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
				map[string]interface{}{"overrides": tt.overrides, "expected_revision": 0})
			if rec.Code != tt.wantStatus {
				t.Fatalf("expected %d, got %d: %s", tt.wantStatus, rec.Code, rec.Body.String())
			}
			if tt.wantCode == "" {
				return
			}
			code, details := errorCodeAndDetails(t, rec)
			if code != tt.wantCode {
				t.Errorf("expected code=%s, got %q", tt.wantCode, code)
			}
			if tt.wantNameDetail != "" {
				if got, _ := details["name"].(string); got != tt.wantNameDetail {
					t.Errorf("expected details.name=%q, got %v", tt.wantNameDetail, details["name"])
				}
			}
		})
	}
}

func TestHandleAdminExperiments_KeyRetentionAndUnknownOverrides(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()

	// Seed a row with a retired key, a pattern-valid unknown key, and a
	// pattern-invalid key, bypassing the handler.
	if _, err := s.UpsertHubSetting(ctx, "experiments",
		json.RawMessage(`{"overrides":{"web.access_boundaries_read":true,"hub.future_thing":false,"not a valid name":true}}`),
		"seed", 0, "managed"); err != nil {
		t.Fatalf("UpsertHubSetting: %v", err)
	}
	refreshOps(t, srv)

	t.Run("GET excludes retired and pattern-invalid keys from unknown_overrides", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
		var body adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := body.UnknownOverrides["web.access_boundaries_read"]; ok {
			t.Error("retired key must not appear in unknown_overrides")
		}
		if _, ok := body.UnknownOverrides["not a valid name"]; ok {
			t.Error("pattern-invalid key must not appear in unknown_overrides")
		}
		if v, ok := body.UnknownOverrides["hub.future_thing"]; !ok || v != false {
			t.Errorf("expected hub.future_thing=false in unknown_overrides, got %v (present=%v)", v, ok)
		}
	})

	t.Run("a PUT prunes retired and pattern-invalid keys and preserves the unknown key", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 1})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.UnknownOverrides) != 1 {
			t.Fatalf("expected exactly one preserved unknown override, got %v", body.UnknownOverrides)
		}
		if v, ok := body.UnknownOverrides["hub.future_thing"]; !ok || v != false {
			t.Errorf("expected hub.future_thing=false preserved, got %v (present=%v)", v, ok)
		}

		raw, err := s.GetHubSetting(ctx, "experiments")
		if err != nil {
			t.Fatalf("GetHubSetting: %v", err)
		}
		if string(raw.Value) == "" {
			t.Fatal("expected a non-empty stored document")
		}
		var doc struct {
			Overrides map[string]bool `json:"overrides"`
		}
		if err := json.Unmarshal(raw.Value, &doc); err != nil {
			t.Fatalf("unmarshal stored doc: %v", err)
		}
		if _, ok := doc.Overrides["web.access_boundaries_read"]; ok {
			t.Error("retired key must be pruned from the stored document")
		}
		if _, ok := doc.Overrides["not a valid name"]; ok {
			t.Error("pattern-invalid key must be dropped from the stored document")
		}
		if v, ok := doc.Overrides["hub.future_thing"]; !ok || v != false {
			t.Error("pattern-valid unknown key must be preserved verbatim in the stored document")
		}
	})

	t.Run("single unknown removal", func(t *testing.T) {
		get := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
		var before adminExperimentsResponse
		if err := json.Unmarshal(get.Body.Bytes(), &before); err != nil {
			t.Fatalf("decode: %v", err)
		}

		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"hub.future_thing": nil}, "expected_revision": before.Revision})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var after adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(after.UnknownOverrides) != 0 {
			t.Errorf("expected hub.future_thing removed, got %v", after.UnknownOverrides)
		}
		for _, e := range after.Experiments {
			if e.Name == "web.terminal_workspace" && (e.Override == nil || *e.Override != false) {
				t.Error("web.terminal_workspace override must be unchanged by the unrelated removal")
			}
		}
	})
}

func TestHandleAdminExperiments_MalformedRow(t *testing.T) {
	reg := testRegistry(t)
	srv, wrapped := newTestServerWithMalformedSection(t, reg, "experiments", json.RawMessage(`not valid json`))
	ctx := context.Background()

	t.Run("GET reports malformed with the fail-closed policy", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.Malformed {
			t.Fatal("expected malformed=true")
		}
		for _, e := range body.Experiments {
			if e.Override != nil {
				t.Errorf("expected override=nil for %s with a malformed row, got %v", e.Name, *e.Override)
			}
			switch e.Name {
			case "hub.test_gate": // server layer: fails closed to OFF
				if e.Enabled {
					t.Error("expected hub.test_gate enabled=false with a malformed row")
				}
			case "web.only_thing": // web-only: falls back to its registry default
				if !e.Enabled {
					t.Error("expected web.only_thing enabled=true (its default) with a malformed row")
				}
			}
		}
	})

	t.Run("GET /api/v1/experiments also fails closed", func(t *testing.T) {
		// The endpoint the web client actually reads must agree with the
		// admin GET's per-entry enabled values (ptone/scion#2217).
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/experiments", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body experimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Experiments["hub.test_gate"] {
			t.Error("expected hub.test_gate=false (server layer fails closed) with a malformed row")
		}
		if !body.Experiments["web.only_thing"] {
			t.Error("expected web.only_thing=true (its registry default) with a malformed row")
		}
		if _, ok := body.Experiments["hub.server_only"]; ok {
			t.Error("hub.server_only has no web layer and must not appear in the resolved map")
		}
	})

	t.Run("PUT is rejected and nothing is written", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"hub.test_gate": true}, "expected_revision": 0})
		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "experiments_malformed" {
			t.Errorf("expected code=experiments_malformed, got %q", code)
		}
		if _, err := wrapped.Store.GetHubSetting(ctx, "experiments"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expected the real store to still have no experiments row, got err=%v", err)
		}
	})

	t.Run("DELETE without confirm_reset_malformed is rejected", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/experiments",
			map[string]interface{}{"expected_revision": 0})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "validation_failed" {
			t.Errorf("expected code=validation_failed, got %q", code)
		}
	})

	t.Run("DELETE with confirm_reset_malformed recovers", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/experiments",
			map[string]interface{}{"confirm_reset_malformed": true})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Malformed {
			t.Error("expected malformed=false after recovery")
		}
		if body.UpdatedBy == nil || *body.UpdatedBy != "dev@localhost" {
			t.Errorf("expected updated_by=dev@localhost, got %v", body.UpdatedBy)
		}
		// Update() populates the cache directly from its own write result, so
		// the refreshed-snapshot path in buildAdminExperimentsResponseAfterWrite
		// applies here despite wrapped's read-side override (which only
		// affects GetHubSetting/ListHubSettings calls, not Update()'s own
		// cache write). Confirm that exactly, by comparing against the real
		// underlying store rather than trusting non-nil.
		row, err := wrapped.Store.GetHubSetting(ctx, "experiments")
		if err != nil {
			t.Fatalf("GetHubSetting: %v", err)
		}
		if body.UpdatedAt == nil || !body.UpdatedAt.Equal(row.UpdatedAt) {
			t.Errorf("expected updated_at=%v (the write's own), got %v", row.UpdatedAt, body.UpdatedAt)
		}
		for _, e := range body.Experiments {
			if e.Override != nil {
				t.Errorf("expected no overrides after reset-all, got %s=%v", e.Name, *e.Override)
			}
		}
	})
}

func TestHandleAdminExperiments_HealthyRowDeleteChecks(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	ctx := context.Background()

	t.Run("DELETE on a healthy row without expected_revision is rejected", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/experiments", map[string]interface{}{})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "validation_failed" {
			t.Errorf("expected code=validation_failed, got %q", code)
		}
	})

	t.Run("confirm_reset_malformed on a healthy row is a conflict and writes nothing", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/experiments",
			map[string]interface{}{"confirm_reset_malformed": true})
		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "revision_conflict" {
			t.Errorf("expected code=revision_conflict, got %q", code)
		}
		// Nothing is written: the row is still absent.
		if _, err := s.GetHubSetting(ctx, "experiments"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expected the row to still be absent, got err=%v", err)
		}
	})

	t.Run("DELETE on a healthy row with a stale expected_revision is a conflict", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/experiments",
			map[string]interface{}{"expected_revision": 999})
		if rec.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
		}
		if code := errorCode(t, rec); code != "revision_conflict" {
			t.Errorf("expected code=revision_conflict, got %q", code)
		}
		if _, err := s.GetHubSetting(ctx, "experiments"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expected the row to still be absent, got err=%v", err)
		}
	})

	t.Run("reset-all with the current revision clears overrides, including unknown keys", func(t *testing.T) {
		// Seed a stored unknown pattern-valid key directly, alongside a
		// registered override written through the handler, so the reset-all
		// clears both kinds of stored entry.
		put := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
			map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 0})
		var putBody adminExperimentsResponse
		if err := json.Unmarshal(put.Body.Bytes(), &putBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, err := s.UpsertHubSetting(ctx, "experiments",
			json.RawMessage(`{"overrides":{"web.terminal_workspace":false,"hub.future_thing":false}}`),
			"seed", putBody.Revision, "managed"); err != nil {
			t.Fatalf("UpsertHubSetting: %v", err)
		}
		refreshOps(t, srv)
		get := doRequest(t, srv, http.MethodGet, "/api/v1/admin/experiments", nil)
		var getBody adminExperimentsResponse
		if err := json.Unmarshal(get.Body.Bytes(), &getBody); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(getBody.UnknownOverrides) != 1 {
			t.Fatalf("setup: expected one unknown override before reset, got %v", getBody.UnknownOverrides)
		}

		rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/experiments",
			map[string]interface{}{"expected_revision": getBody.Revision})
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var body adminExperimentsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}

		raw, err := s.GetHubSetting(ctx, "experiments")
		if err != nil {
			t.Fatalf("GetHubSetting: %v", err)
		}
		// The response's updated_at/updated_by must be this write's own, not
		// merely non-nil: compare against the row the write produced.
		if body.UpdatedAt == nil || !body.UpdatedAt.Equal(raw.UpdatedAt) {
			t.Errorf("expected updated_at=%v (the write's own), got %v", raw.UpdatedAt, body.UpdatedAt)
		}
		if body.UpdatedBy == nil || *body.UpdatedBy != raw.UpdatedBy {
			t.Errorf("expected updated_by=%q (the write's own), got %v", raw.UpdatedBy, body.UpdatedBy)
		}
		if len(body.UnknownOverrides) != 0 {
			t.Errorf("expected unknown_overrides cleared by reset-all, got %v", body.UnknownOverrides)
		}
		for _, e := range body.Experiments {
			if e.Override != nil {
				t.Errorf("expected no overrides after reset-all, got %s=%v", e.Name, *e.Override)
			}
		}

		var doc struct {
			Overrides map[string]bool `json:"overrides"`
		}
		if err := json.Unmarshal(raw.Value, &doc); err != nil {
			t.Fatalf("unmarshal stored doc: %v", err)
		}
		if len(doc.Overrides) != 0 {
			t.Errorf("expected the stored document to have no overrides after reset-all, got %v", doc.Overrides)
		}
	})
}

// --- Generic section-reset rejection (ptone/scion#2217) ---

func TestHandleAdminServerConfigSectionReset_RejectsExperiments(t *testing.T) {
	srv, s := testServerWithOps(t, nil)
	srv.dbDriver = "postgres" // unlock the postgres-only generic-reset route for this test

	put := doRequest(t, srv, http.MethodPut, "/api/v1/admin/experiments",
		map[string]interface{}{"overrides": map[string]interface{}{"web.terminal_workspace": false}, "expected_revision": 0})
	if put.Code != http.StatusOK {
		t.Fatalf("setup PUT expected 200, got %d: %s", put.Code, put.Body.String())
	}
	before, err := s.GetHubSetting(context.Background(), "experiments")
	if err != nil {
		t.Fatalf("GetHubSetting: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/server-config/sections/experiments", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "validation_failed" {
		t.Errorf("expected code=validation_failed, got %q", code)
	}

	after, err := s.GetHubSetting(context.Background(), "experiments")
	if err != nil {
		t.Fatalf("GetHubSetting: %v", err)
	}
	if string(after.Value) != string(before.Value) || after.Revision != before.Revision {
		t.Errorf("expected the experiments row to be unchanged, before=%s/%d after=%s/%d",
			before.Value, before.Revision, after.Value, after.Revision)
	}

	// A different section still resets through the same route: the rejection
	// is specific to "experiments", not a blanket change to the route.
	msgStore := json.RawMessage(`{"conversation_envelope_switch":true}`)
	if _, err := s.UpsertHubSetting(context.Background(), "messaging", msgStore, "seed", 0, "managed"); err != nil {
		t.Fatalf("UpsertHubSetting: %v", err)
	}
	other := doRequest(t, srv, http.MethodDelete, "/api/v1/admin/server-config/sections/messaging", nil)
	if other.Code != http.StatusOK {
		t.Fatalf("expected the messaging section reset to succeed, got %d: %s", other.Code, other.Body.String())
	}
	if _, err := s.GetHubSetting(context.Background(), "messaging"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected the messaging row to be gone after reset, got err=%v", err)
	}
}
