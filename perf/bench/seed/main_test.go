package main

import (
	"context"
	"encoding/json"
	"math/rand"
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
// deriveSharedSigningKey's doc comment promises (bench-rev-1 B4). It does
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

// TestPickWeightedPhaseIsDeterministicForAFixedSeed confirms that two
// independent rand.Source(42) sequences produce identical pickWeightedPhase
// draws, which is what makes perf/bench/seed's --rand-seed flag meaningful
// for reproducibility (bench-rev-1 B4).
func TestPickWeightedPhaseIsDeterministicForAFixedSeed(t *testing.T) {
	draw := func() []string {
		rng := rand.New(rand.NewSource(42))
		out := make([]string, 200)
		for i := range out {
			out[i] = pickWeightedPhase(rng)
		}
		return out
	}
	a, b := draw(), draw()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("draw %d differs between two rand.NewSource(42) sequences: %q vs %q -- "+
				"pickWeightedPhase must be a pure function of the passed *rand.Rand", i, a[i], b[i])
		}
	}
}

// TestPickWeightedPhaseDistributionRoughlyMatchesWeights checks the
// distribution is in the right ballpark (not an exact statistical test,
// which would be flaky) -- "running" is weighted 45/102 (~44%) and
// "cloning" is weighted 2/102 (~2%), so over a large sample the former
// should be clearly the most common and the latter clearly rare.
func TestPickWeightedPhaseDistributionRoughlyMatchesWeights(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	counts := map[string]int{}
	const n = 20000
	for i := 0; i < n; i++ {
		counts[pickWeightedPhase(rng)]++
	}

	runningFrac := float64(counts["running"]) / n
	if runningFrac < 0.35 || runningFrac > 0.55 {
		t.Errorf("running fraction = %.3f, want roughly 0.45 (weight 45/102)", runningFrac)
	}
	cloningFrac := float64(counts["cloning"]) / n
	if cloningFrac > 0.06 {
		t.Errorf("cloning fraction = %.3f, want roughly 0.02 (weight 2/102), clearly rare", cloningFrac)
	}
	for _, phase := range []string{"running", "stopped", "error", "created", "provisioning", "cloning", "starting", "suspended", "stopping"} {
		if counts[phase] == 0 {
			t.Errorf("phase %q was never drawn in %d samples", phase, n)
		}
	}
}

// TestRunSeedsNonAdminProjectMember is the B4-required check that the
// principal every bench tool authenticates as is genuinely a project
// member, not the project owner and not any kind of admin.
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

// TestCheckDBNotExists covers bench-rev-1 B6's refusal behavior directly
// (extracted into its own function specifically so it is unit-testable
// without spawning the compiled binary), including bench-rev-2 R4's "file:"
// DSN / "?query" bypass.
func TestCheckDBNotExists(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing path is fine", func(t *testing.T) {
		if err := checkDBNotExists(filepath.Join(dir, "does-not-exist.db")); err != nil {
			t.Errorf("checkDBNotExists on a missing path: %v", err)
		}
	})

	t.Run("empty file is fine", func(t *testing.T) {
		p := filepath.Join(dir, "empty.db")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkDBNotExists(p); err != nil {
			t.Errorf("checkDBNotExists on an empty file: %v", err)
		}
	})

	t.Run("non-empty file is refused for a plain path", func(t *testing.T) {
		p := filepath.Join(dir, "nonempty.db")
		if err := os.WriteFile(p, []byte("not really sqlite but non-empty"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkDBNotExists(p); err == nil {
			t.Error("checkDBNotExists on a non-empty file: want error, got nil")
		}
	})

	t.Run("non-empty file is refused for a file: DSN with no query (R4)", func(t *testing.T) {
		p := filepath.Join(dir, "nonempty-filedsn.db")
		if err := os.WriteFile(p, []byte("not really sqlite but non-empty"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkDBNotExists("file:" + p); err == nil {
			t.Error("checkDBNotExists on file:<non-empty path>: want error, got nil")
		}
	})

	t.Run("non-empty file is refused for a file: DSN with a query suffix (R4)", func(t *testing.T) {
		p := filepath.Join(dir, "nonempty-filedsn-query.db")
		if err := os.WriteFile(p, []byte("not really sqlite but non-empty"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkDBNotExists("file:" + p + "?cache=shared"); err == nil {
			t.Error("checkDBNotExists on file:<non-empty path>?cache=shared: want error, got nil")
		}
	})

	t.Run("missing path is fine even as a file: DSN with a query suffix", func(t *testing.T) {
		p := filepath.Join(dir, "does-not-exist-filedsn.db")
		if err := checkDBNotExists("file:" + p + "?cache=shared"); err != nil {
			t.Errorf("checkDBNotExists on a missing file: DSN: %v", err)
		}
	})
}

func TestNormalizeDBPathForStat(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"/tmp/hub.db", "/tmp/hub.db"},
		{"file:/tmp/hub.db", "/tmp/hub.db"},
		{"file:/tmp/hub.db?cache=shared", "/tmp/hub.db"},
		{"/tmp/hub.db?cache=shared", "/tmp/hub.db"},
	}
	for _, c := range cases {
		if got := normalizeDBPathForStat(c.in); got != c.want {
			t.Errorf("normalizeDBPathForStat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
