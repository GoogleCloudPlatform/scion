//go:build !no_sqlite

// This file holds the main_test.go cases that exercise a real SQLite-backed
// store end to end (run() -> entc.OpenSQLite -> hub.New / role lookups).
// They need the real modernc.org/sqlite driver registered, which is only
// compiled in under the default (non-no_sqlite) build -- see
// pkg/ent/entc/driver_sqlite.go / driver_nosqlite.go. `make test-fast`
// builds the whole module with `-tags no_sqlite` (pkg/hub's own tests that
// need a real store carry this same constraint, for the same reason; see
// .github/workflows/ci.yml's hub-sqlite-tests job), so without this
// constraint these two tests fail that job with "sql: unknown driver
// \"sqlite\"" rather than being skipped. The DB-free tests in main_test.go
// (pickWeightedPhase, path validation, etc.) stay there, ungated, since
// they do not need the driver and should keep running under -tags
// no_sqlite too.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// TestDeriveSharedSigningKeyMatchesHub is the parity check
// deriveSharedSigningKey's doc comment promises. It does
// not compare deriveSharedSigningKey's output against a copy-pasted
// expected byte string (which would drift silently along with any future
// copy-paste of pkg/hub/server.go's unexported version) -- it seeds a real
// database with run(), starts a real hub.Server against it with the same
// --session-secret, and confirms the hub actually accepts a bearer token
// minted with this file's deriveSharedSigningKey. If pkg/hub/server.go's
// deriveSharedSigningKey ever changes its key-derivation format, the
// minted token stops validating and this test fails with a 401 -- an
// end-to-end proof, not a narrow unit comparison.
func TestDeriveSharedSigningKeyMatchesHub(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	outPath := filepath.Join(dir, "seed.json")
	const secret = "test-parity-secret"

	if err := run(dbPath, 2, secret, "parity-project", "Parity Project", 42, outPath); err != nil {
		t.Fatalf("run: %v", err)
	}

	var meta struct {
		ProjectID   string `json:"projectId"`
		MemberToken string `json:"memberToken"`
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read seed metadata: %v", err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse seed metadata: %v", err)
	}

	// Re-open the freshly-seeded database and start a real hub.Server
	// against it with the same shared signing secret, exactly as the real
	// `scion server start --db ... --session-secret ...` subprocess would.
	client, err := openSQLiteForBench(dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	cfg := hub.DefaultServerConfig()
	cfg.SharedSigningSecret = secret
	srv, err := hub.New(cfg, entadapter.NewCompositeStore(client))
	if err != nil {
		t.Fatalf("hub.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+meta.ProjectID+"/agents", nil)
	req.Header.Set("Authorization", "Bearer "+meta.MemberToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("member token rejected by a hub started with the same --session-secret: "+
			"status=%d body=%s (this means deriveSharedSigningKey in this package has drifted "+
			"from pkg/hub/server.go's unexported version)", rec.Code, rec.Body.String())
	}
}

// TestRunSeedsNonAdminProjectMember checks that the principal every bench
// tool authenticates as is genuinely a project member, not the project
// owner and not any kind of admin.
func TestRunSeedsNonAdminProjectMember(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	outPath := filepath.Join(dir, "seed.json")
	const secret = "test-role-secret"

	if err := run(dbPath, 3, secret, "role-project", "Role Project", 7, outPath); err != nil {
		t.Fatalf("run: %v", err)
	}

	var meta struct {
		ProjectID    string `json:"projectId"`
		OwnerUserID  string `json:"ownerUserId"`
		MemberUserID string `json:"memberUserId"`
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read seed metadata: %v", err)
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("parse seed metadata: %v", err)
	}

	client, err := openSQLiteForBench(dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	s := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	bindingsFor := func(userID string) []*store.RoleBinding {
		all, err := s.ListRoleBindingsForPrincipals(ctx,
			[]store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: userID}},
			[]string{store.RoleScopeProject}, []string{meta.ProjectID})
		if err != nil {
			t.Fatalf("ListRoleBindingsForPrincipals(%s): %v", userID, err)
		}
		return all
	}
	roleNameFor := func(rb *store.RoleBinding) string {
		rd, err := s.GetRoleDefinitionsByIDs(ctx, []string{rb.RoleDefinitionID})
		if err != nil || rd[rb.RoleDefinitionID] == nil {
			t.Fatalf("resolve role definition %s: %v", rb.RoleDefinitionID, err)
		}
		return rd[rb.RoleDefinitionID].Name
	}

	memberBindings := bindingsFor(meta.MemberUserID)
	if len(memberBindings) != 1 {
		t.Fatalf("member has %d project-scoped role bindings, want exactly 1", len(memberBindings))
	}
	if got := roleNameFor(memberBindings[0]); got != store.ProjectRoleMember {
		t.Errorf("member's project role = %q, want %q (not owner/admin)", got, store.ProjectRoleMember)
	}

	ownerBindings := bindingsFor(meta.OwnerUserID)
	if len(ownerBindings) != 1 {
		t.Fatalf("owner has %d project-scoped role bindings, want exactly 1", len(ownerBindings))
	}
	if got := roleNameFor(ownerBindings[0]); got != store.ProjectRoleOwner {
		t.Errorf("owner's project role = %q, want %q", got, store.ProjectRoleOwner)
	}
}
