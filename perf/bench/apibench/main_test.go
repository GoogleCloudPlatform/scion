package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/perf/bench/internal/benchout"
)

func makeTestSeed() benchout.SeedMetadata {
	return benchout.SeedMetadata{
		DBPath:        "/tmp/hub.db",
		SessionSecret: "top-secret",
		ProjectID:     "proj-1",
		ProjectSlug:   "proj-1-slug",
		OwnerUserID:   "owner-1",
		OwnerToken:    "owner-token-value",
		MemberUserID:  "member-1",
		MemberToken:   "member-token-value",
		AgentCount:    25,
	}
}

func floatsEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestMedian(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{5}, 5},
		{"odd", []float64{3, 1, 2}, 2},
		{"even", []float64{1, 2, 3, 4}, 2.5},
		{"unsorted duplicates", []float64{5, 1, 5, 1, 3}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := median(c.in)
			if !floatsEqual(got, c.want) {
				t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestMedianDoesNotMutateInput(t *testing.T) {
	in := []float64{3, 1, 2}
	orig := append([]float64(nil), in...)
	_ = median(in)
	for i := range in {
		if in[i] != orig[i] {
			t.Fatalf("median mutated its input: got %v, want %v", in, orig)
		}
	}
}

func TestMinMax(t *testing.T) {
	cases := []struct {
		name    string
		in      []float64
		wantMin float64
		wantMax float64
	}{
		{"empty", nil, 0, 0},
		{"single", []float64{7}, 7, 7},
		{"unsorted", []float64{3, 1, 4, 1, 5, 9, 2, 6}, 1, 9},
		{"all equal", []float64{2, 2, 2}, 2, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lo, hi := minMax(c.in)
			if !floatsEqual(lo, c.wantMin) || !floatsEqual(hi, c.wantMax) {
				t.Errorf("minMax(%v) = (%v, %v), want (%v, %v)", c.in, lo, hi, c.wantMin, c.wantMax)
			}
		})
	}
}

func TestStdDev(t *testing.T) {
	cases := []struct {
		name string
		in   []float64
		want float64
	}{
		{"empty", nil, 0},
		{"single", []float64{5}, 0},
		// Sample (n-1) stddev of {2,4,4,4,5,5,7,9} is 2.13809...
		{"textbook", []float64{2, 4, 4, 4, 5, 5, 7, 9}, 2.1380899352993947},
		{"two equal values", []float64{3, 3}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stddev(c.in)
			if math.Abs(got-c.want) > 1e-9 {
				t.Errorf("stddev(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestRedactSeedStripsCredentials(t *testing.T) {
	seed := makeTestSeed()
	redacted := redactSeed(seed)

	if redacted.SessionSecret == seed.SessionSecret {
		t.Error("redactSeed did not change SessionSecret")
	}
	if redacted.OwnerToken == seed.OwnerToken {
		t.Error("redactSeed did not change OwnerToken")
	}
	if redacted.MemberToken == seed.MemberToken {
		t.Error("redactSeed did not change MemberToken")
	}
	// Non-credential fields must survive unchanged.
	if redacted.ProjectID != seed.ProjectID {
		t.Errorf("redactSeed changed ProjectID: got %q, want %q", redacted.ProjectID, seed.ProjectID)
	}
	if redacted.AgentCount != seed.AgentCount {
		t.Errorf("redactSeed changed AgentCount: got %d, want %d", redacted.AgentCount, seed.AgentCount)
	}
}

// harnessBuildInfo must read the BINARY's own VCS stamp, not
// `git rev-parse HEAD` in the caller's cwd -- there is no way to assert
// the exact commit from inside `go test` (it depends on the checkout this
// suite happens to run from), but the actual, checkable requirement is
// "record empty with an explicit source rather than guessing": the source
// string must never be empty, and must never claim a VCS stamp while
// leaving the commit blank.
func TestHarnessBuildInfoNeverGuesses(t *testing.T) {
	commit, dirty, source := harnessBuildInfo()
	if source == "" {
		t.Fatal("harnessBuildInfo: source must never be empty")
	}
	if source == "go build VCS stamp" {
		if commit == "" {
			t.Fatal("harnessBuildInfo: source claims a VCS stamp but commit is empty")
		}
		if dirty == nil {
			t.Fatal("harnessBuildInfo: source claims a VCS stamp but dirty is nil")
		}
	} else {
		if commit != "" {
			t.Fatalf("harnessBuildInfo: commit %q set without claiming a VCS-stamp source (got %q)", commit, source)
		}
		// dirty must be nil (unknown), not false, when the commit itself is
		// unknown -- a bare `false` would be indistinguishable from "known
		// clean".
		if dirty != nil {
			t.Fatalf("harnessBuildInfo: dirty = %v, want nil when commit is unknown (source %q)", *dirty, source)
		}
	}
}

// fetchHubVersion does not return the hub's hard-coded `/health.version`
// placeholder -- see its doc comment and hubHealth's.
func TestFetchHubVersionParsesHealthResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]string{
			"version":      "hub-1.2.3",
			"scionVersion": "0.9.0",
			"status":       "ok",
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer srv.Close()

	scionVersion := fetchHubVersion(srv.Client(), srv.URL)
	if scionVersion != "0.9.0" {
		t.Fatalf("fetchHubVersion = %q, want 0.9.0", scionVersion)
	}
}

func TestFetchHubVersionReturnsEmptyOnUnreachableHost(t *testing.T) {
	scionVersion := fetchHubVersion(&http.Client{Timeout: 2e9}, "http://127.0.0.1:1")
	if scionVersion != "" {
		t.Fatalf("fetchHubVersion on unreachable host = %q, want empty", scionVersion)
	}
}

func TestFetchHubVersionReturnsEmptyOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	scionVersion := fetchHubVersion(srv.Client(), srv.URL)
	if scionVersion != "" {
		t.Fatalf("fetchHubVersion on 404 = %q, want empty", scionVersion)
	}
}
