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

// TestValidateDBPathArg covers bench-rev-3 O1, bench-rev-4 N1 and
// bench-rev-5 N-a: a growing list of specific DSN forms whose string
// content lets `--db`'s literal value resolve to a DIFFERENT filesystem
// path than what `checkDBNotExists` just confirmed doesn't exist under
// that same literal value ("file://localhost/<path>", "#fragment",
// "%XX"-escapes, a bare leading "//" authority). bench-rev-5 N-a: an
// earlier version of this function's doc comment claimed rejecting
// "#"/"%" "closes every such form" -- it did not (the bare "//" form,
// without "file:", was not caught at all) -- so this is deliberately NOT
// claimed to be an exhaustive enumeration here either; main()'s
// `filepath.Clean` canonicalization is the actual defense-in-depth for
// whatever slash-count variant this list has not thought to add yet.
func TestValidateDBPathArg(t *testing.T) {
	valid := []string{
		"/tmp/hub.db",
		"hub.db",
		"./relative/hub.db",
		"fileish:name.db", // "file" is a substring, but not the "file:" prefix
		"/tmp/a path with spaces/hub.db",
	}
	for _, p := range valid {
		if err := validateDBPathArg(p); err != nil {
			t.Errorf("validateDBPathArg(%q): want nil, got %v", p, err)
		}
	}

	rejected := []string{
		"file:/tmp/hub.db",
		"file:/tmp/hub.db?cache=shared",
		"file://localhost/tmp/hub.db",
		"file://localhost/tmp/hub.db?cache=shared",
		"/tmp/hub.db?cache=shared",
		"/tmp/hub.db?mode=rw",
		"/tmp/hub.db#frag",       // N1: URI fragment, silently dropped by the DSN parser
		"/tmp/%76ictim.db",       // N1: percent-encoding, silently decoded by the DSN parser
		"/tmp/hub.db#cache=rw",   // fragment form disguised as a query-like suffix
		"/tmp/100%done/hub.db",   // "%" anywhere, not just a valid escape sequence
		"//localhost/tmp/hub.db", // N-a: same bypass class as file://localhost/, no "file:" prefix
		"//tmp/hub.db",           // N-a: leading "//" on its own, no "localhost" segment needed
	}
	for _, p := range rejected {
		if err := validateDBPathArg(p); err == nil {
			t.Errorf("validateDBPathArg(%q): want error, got nil", p)
		}
	}
}

// TestResolveDBPathArg covers bench-rev-5 N-a's two distinct slash-count
// variants, through `resolveDBPathArg` specifically -- the function
// `main()` actually calls -- rather than through `filepath.Clean` and
// `checkDBNotExists` directly.
//
// bench-rev-6 N6-1: an earlier version of this test called `filepath.Clean`
// directly, so it exercised Go's standard library, not this package's own
// logic; deleting `main()`'s call to `filepath.Clean` entirely left this
// test passing (reviewer-confirmed mutation). `resolveDBPathArg` is the
// package-local function that owns "validate, then canonicalize, in that
// order" -- `main()` has nothing left to get wrong beyond calling it -- so
// a test against `resolveDBPathArg` is a test of the actual implementation
// `main()` delegates to, the same relationship `validateDBPathArg` and
// `checkDBNotExists` already have with their own tests above.
func TestResolveDBPathArg(t *testing.T) {
	t.Run("rejects what validateDBPathArg rejects, before ever cleaning", func(t *testing.T) {
		for _, bad := range []string{"//localhost/tmp/x.db", "file:/tmp/x.db", "/tmp/x.db?q=1"} {
			if _, err := resolveDBPathArg(bad); err == nil {
				t.Errorf("resolveDBPathArg(%q): want error, got nil", bad)
			}
		}
	})

	t.Run("canonicalizes a messy-but-valid path to match its clean equivalent", func(t *testing.T) {
		cases := []struct{ in, want string }{
			{"/tmp/sub/./x.db", "/tmp/sub/x.db"},
			{"/tmp/sub/other/../x.db", "/tmp/sub/x.db"},
			{"/tmp/sub//x.db", "/tmp/sub/x.db"}, // internal double slash, not a leading one
		}
		for _, c := range cases {
			got, err := resolveDBPathArg(c.in)
			if err != nil {
				t.Errorf("resolveDBPathArg(%q): %v", c.in, err)
				continue
			}
			if got != c.want {
				t.Errorf("resolveDBPathArg(%q) = %q, want %q", c.in, got, c.want)
			}
		}
	})
}

// TestFilepathCleanClosesSlashCountBypass reproduces bench-rev-5 N-a's two
// distinct slash-count variants end-to-end, at the `filepath.Clean` +
// `checkDBNotExists` layer specifically (as defense-in-depth independent
// of `validateDBPathArg`'s leading-"//" rejection, which already rejects
// both inputs below before `resolveDBPathArg` would ever reach `Clean` --
// see TestResolveDBPathArg and TestValidateDBPathArg for that layer).
//
// The two forms behave differently after cleaning, and both are safe:
//   - "//tmp/..." and "///tmp/..." (no authority-shaped segment) clean down
//     to the REAL existing path, so checkDBNotExists correctly detects and
//     refuses it.
//   - "//localhost/tmp/..." cleans to a LITERAL "/localhost/tmp/..." path
//     (filepath.Clean does not know about URI authorities; it only
//     collapses slash counts), which is a different, non-existent path --
//     not the victim. Because `run()` uses this SAME cleaned string for the
//     actual DSN open, the open would land on that harmless non-existent
//     path too, never on the real victim: the two operations agree, which
//     is the actual property that closes the bug, not "always rediscovers
//     the original attacker-intended target."
func TestFilepathCleanClosesSlashCountBypass(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.db")
	if err := os.WriteFile(victim, []byte("existing non-empty sqlite-shaped content"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("no-authority variants resolve to the real victim and are refused", func(t *testing.T) {
		for _, v := range []string{"//" + victim[1:], "///" + victim[1:]} {
			cleaned := filepath.Clean(v)
			if cleaned != victim {
				t.Errorf("filepath.Clean(%q) = %q, want the real victim path %q", v, cleaned, victim)
				continue
			}
			if err := checkDBNotExists(cleaned); err == nil {
				t.Errorf("checkDBNotExists(filepath.Clean(%q)): want error (existing non-empty "+
					"file), got nil", v)
			}
		}
	})

	t.Run("authority-shaped variant cleans to a different, non-colliding path", func(t *testing.T) {
		v := "//localhost" + victim
		cleaned := filepath.Clean(v)
		if cleaned == victim {
			t.Fatalf("filepath.Clean(%q) = %q, unexpectedly equals the victim path -- "+
				"re-check this test's assumptions", v, cleaned)
		}
		// The cleaned form must not itself already exist either -- otherwise
		// THIS test's setup would be the one leaking data across runs, not
		// a property of the fix.
		if err := checkDBNotExists(cleaned); err != nil {
			t.Errorf("checkDBNotExists(filepath.Clean(%q)) = %v, want nil (a fresh, "+
				"non-colliding path)", v, err)
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
