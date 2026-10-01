package main

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// TestPickWeightedPhaseIsDeterministicForAFixedSeed confirms that two
// independent rand.Source(42) sequences produce identical pickWeightedPhase
// draws, which is what makes perf/bench/seed's --rand-seed flag meaningful
// for reproducibility.
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

// TestCheckDBNotExists covers checkDBNotExists's refusal behavior directly
// (extracted into its own function specifically so it is unit-testable
// without spawning the compiled binary), including the "file:" DSN /
// "?query" normalization that keeps the existence check from being
// bypassed by DSN-form variants of an existing path.
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

	t.Run("non-empty file is refused for a file: DSN with no query", func(t *testing.T) {
		p := filepath.Join(dir, "nonempty-filedsn.db")
		if err := os.WriteFile(p, []byte("not really sqlite but non-empty"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkDBNotExists("file:" + p); err == nil {
			t.Error("checkDBNotExists on file:<non-empty path>: want error, got nil")
		}
	})

	t.Run("non-empty file is refused for a file: DSN with a query suffix", func(t *testing.T) {
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

// TestValidateDBPathArg covers a list of specific DSN forms whose string
// content lets `--db`'s literal value resolve to a DIFFERENT filesystem
// path than what `checkDBNotExists` just confirmed doesn't exist under
// that same literal value ("file://localhost/<path>", "#fragment",
// "%XX"-escapes, a bare leading "//" authority). This is deliberately NOT
// claimed to be an exhaustive enumeration; main()'s `filepath.Clean`
// canonicalization is the actual defense-in-depth for whatever
// slash-count variant this list has not thought to add yet.
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
		"/tmp/hub.db#frag",       // URI fragment, silently dropped by the DSN parser
		"/tmp/%76ictim.db",       // percent-encoding, silently decoded by the DSN parser
		"/tmp/hub.db#cache=rw",   // fragment form disguised as a query-like suffix
		"/tmp/100%done/hub.db",   // "%" anywhere, not just a valid escape sequence
		"//localhost/tmp/hub.db", // same bypass class as file://localhost/, no "file:" prefix
		"//tmp/hub.db",           // leading "//" on its own, no "localhost" segment needed
	}
	for _, p := range rejected {
		if err := validateDBPathArg(p); err == nil {
			t.Errorf("validateDBPathArg(%q): want error, got nil", p)
		}
	}
}

// TestResolveDBPathArg covers two distinct slash-count variants, through
// `resolveDBPathArg` specifically -- the function `main()` actually calls
// -- rather than through `filepath.Clean` and `checkDBNotExists` directly.
//
// Calling `filepath.Clean` directly in a test would exercise Go's standard
// library, not this package's own logic, and would not detect `main()`'s
// call to `filepath.Clean` being removed entirely. `resolveDBPathArg` is
// the package-local function that owns "validate, then canonicalize, in
// that order" -- `main()` has nothing left to get wrong beyond calling it
// -- so a test against `resolveDBPathArg` is a test of the actual
// implementation `main()` delegates to, the same relationship
// `validateDBPathArg` and `checkDBNotExists` already have with their own
// tests above.
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

// TestFilepathCleanClosesSlashCountBypass reproduces two distinct
// slash-count variants end-to-end, at the `filepath.Clean` +
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
