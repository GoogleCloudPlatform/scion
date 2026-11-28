package githubapp

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/fakes"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

var repo = Repo{Owner: "example-owner", Name: "example-repo"}

func newTestClient(t *testing.T, scope Scope) (*Client, *fakes.GitHub, *clock) {
	t.Helper()
	key := fakes.Key(0)
	ck := &clock{t: time.Now()}
	gh := fakes.NewGitHub(4242, &key.PublicKey)
	gh.Now = ck.now
	t.Cleanup(gh.Close)
	c := New(Options{
		BaseURL: gh.Server.URL, AppID: 4242, Scope: scope, Now: ck.now,
		PrivateKey: func(context.Context) (*rsa.PrivateKey, error) { return key, nil },
	})
	return c, gh, ck
}

func TestParsePrivateKey(t *testing.T) {
	key := fakes.Key(0)
	if _, err := ParsePrivateKey(fakes.PEM(key)); err != nil {
		t.Errorf("PKCS#1: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePrivateKey(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})); err != nil {
		t.Errorf("PKCS#8: %v", err)
	}
	for _, bad := range []string{"", "not pem", "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"} {
		if _, err := ParsePrivateKey([]byte(bad)); err == nil {
			t.Errorf("ParsePrivateKey(%q) succeeded", bad)
		}
	}
}

func TestAppJWTClaims(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	jwt, err := signAppJWT(fakes.Key(0), 99, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("jwt has %d parts", len(parts))
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iat, Exp int64
		Iss      string
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if c.Iss != "99" || c.Iat != now.Unix()-60 || c.Exp != now.Add(9*time.Minute).Unix() {
		t.Errorf("claims = %+v", c)
	}
}

// TestInstallationTokenCaching: one token is minted and reused, then
// refreshed once it is within five minutes of expiry.
func TestInstallationTokenCaching(t *testing.T) {
	c, gh, ck := newTestClient(t, ScopeRepo)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.ListRunners(ctx, repo); err != nil {
			t.Fatal(err)
		}
	}
	if mints, _, _ := gh.Stats(); mints != 1 {
		t.Fatalf("token mints = %d after 3 calls, want 1 (cached)", mints)
	}
	ck.add(54 * time.Minute) // 6 min left: still cached
	if _, err := c.ListRunners(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if mints, _, _ := gh.Stats(); mints != 1 {
		t.Fatalf("token mints = %d with 6 min left, want 1", mints)
	}
	ck.add(2 * time.Minute) // 4 min left: refresh
	if _, err := c.ListRunners(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if mints, _, _ := gh.Stats(); mints != 2 {
		t.Fatalf("token mints = %d with 4 min left, want 2 (refreshed)", mints)
	}
}

// TestUnauthorizedDropsCachedToken: a 401 discards the cached token so the
// next call mints a new one.
func TestUnauthorizedDropsCachedToken(t *testing.T) {
	c, gh, _ := newTestClient(t, ScopeRepo)
	ctx := context.Background()
	if _, err := c.ListRunners(ctx, repo); err != nil {
		t.Fatal(err)
	}
	// Corrupt the cached token.
	c.mu.Lock()
	for id, tok := range c.tokens {
		tok.token = "ghs_revoked"
		c.tokens[id] = tok
	}
	c.mu.Unlock()
	if _, err := c.ListRunners(ctx, repo); !IsStatus(err, 401) {
		t.Fatalf("err = %v, want 401", err)
	}
	if _, err := c.ListRunners(ctx, repo); err != nil {
		t.Fatalf("after 401: %v", err)
	}
	if mints, _, _ := gh.Stats(); mints != 2 {
		t.Errorf("token mints = %d, want 2", mints)
	}
}

func TestJITAndDeleteRunner(t *testing.T) {
	for _, scope := range []Scope{ScopeRepo, ScopeOrg} {
		t.Run(string(scope), func(t *testing.T) {
			c, gh, _ := newTestClient(t, scope)
			ctx := context.Background()
			id, jit, err := c.GenerateJITConfig(ctx, repo, JITRequest{Name: "cr-job-1", RunnerGroupID: 1, Labels: []string{"lbl"}, WorkFolder: "/home/runner/work"})
			if err != nil || id == 0 || jit == "" {
				t.Fatalf("GenerateJITConfig = %d %q %v", id, jit, err)
			}
			rn, ok := gh.RunnerByName("cr-job-1")
			want := "repo:example-owner/example-repo"
			if scope == ScopeOrg {
				want = "org:example-owner"
			}
			if !ok || rn.Scope != want {
				t.Fatalf("runner scope = %q, want %q", rn.Scope, want)
			}
			runners, err := c.ListRunners(ctx, repo)
			if err != nil || len(runners) != 1 {
				t.Fatalf("ListRunners = %v %v", runners, err)
			}
			if err := c.DeleteRunner(ctx, repo, id); err != nil {
				t.Fatal(err)
			}
			if err := c.DeleteRunner(ctx, repo, id); err != nil {
				t.Errorf("deleting a missing runner: %v, want nil", err)
			}
		})
	}
}

func TestRunsAndJobs(t *testing.T) {
	c, gh, _ := newTestClient(t, ScopeRepo)
	gh.AddRun(fakes.GitHubRun{ID: 5, Repo: repo.String(), HeadRepo: "fork/example-repo", Event: "pull_request", Status: "queued",
		Jobs: []fakes.GitHubJob{{ID: 50, Status: "queued", Labels: []string{"x"}}}})
	ctx := context.Background()
	run, err := c.GetRun(ctx, repo, 5)
	if err != nil || run.HeadRepository == nil || run.HeadRepository.FullName != "fork/example-repo" || run.Event != "pull_request" {
		t.Fatalf("GetRun = %+v %v", run, err)
	}
	runs, err := c.ListRuns(ctx, repo, "queued")
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListRuns = %v %v", runs, err)
	}
	jobs, err := c.ListJobs(ctx, repo, 5)
	if err != nil || len(jobs) != 1 || jobs[0].ID != 50 || jobs[0].RunID != 5 {
		t.Fatalf("ListJobs = %+v %v", jobs, err)
	}
	if _, err := c.GetRun(ctx, repo, 6); !IsStatus(err, 404) {
		t.Errorf("missing run: err = %v, want 404", err)
	}
}

func TestKeyErrorPropagates(t *testing.T) {
	gh := fakes.NewGitHub(1, &fakes.Key(0).PublicKey)
	defer gh.Close()
	c := New(Options{BaseURL: gh.Server.URL, AppID: 1, Scope: ScopeRepo,
		PrivateKey: func(context.Context) (*rsa.PrivateKey, error) { return nil, errors.New("secret manager down") }})
	if _, err := c.ListRunners(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "secret manager down") {
		t.Errorf("err = %v", err)
	}
}

// TestReinstallRelooksUpInstallation: after the App is reinstalled under a
// new installation ID, the cached ID is dropped and looked up again.
func TestReinstallRelooksUpInstallation(t *testing.T) {
	c, gh, ck := newTestClient(t, ScopeRepo)
	ctx := context.Background()
	if _, err := c.ListRunners(ctx, repo); err != nil {
		t.Fatal(err)
	}
	gh.InstallationID = 78
	ck.add(56 * time.Minute) // cached token needs a refresh
	if _, err := c.ListRunners(ctx, repo); !IsStatus(err, 404) {
		t.Fatalf("first call after reinstall: %v, want 404", err)
	}
	if _, err := c.ListRunners(ctx, repo); err != nil {
		t.Fatalf("installation ID not looked up again: %v", err)
	}
}

func TestDo_RateLimitClassification(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	resetEpoch := now.Add(25 * time.Minute).Unix()

	cases := []struct {
		name            string
		status          int
		headers         map[string]string
		body            string
		wantRateLimited bool
		wantTransient   bool
		wantRetryAfter  time.Time
	}{
		{
			name:   "403 with remaining 0 and reset",
			status: http.StatusForbidden,
			headers: map[string]string{
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Remaining": "0",
				"X-RateLimit-Reset":     strconv.FormatInt(resetEpoch, 10),
			},
			body:            `{"message":"API rate limit exceeded"}`,
			wantRateLimited: true,
			wantTransient:   true,
			wantRetryAfter:  time.Unix(resetEpoch, 0),
		},
		{
			name:   "429 with Retry-After seconds",
			status: http.StatusTooManyRequests,
			headers: map[string]string{
				"Retry-After": "90",
			},
			body:            `{"message":"secondary limit"}`,
			wantRateLimited: true,
			wantTransient:   true,
			wantRetryAfter:  now.Add(90 * time.Second),
		},
		{
			name:            "403 with rate limit body only",
			status:          http.StatusForbidden,
			body:            `{"message":"You have exceeded a secondary rate limit."}`,
			wantRateLimited: true,
			wantTransient:   true,
			wantRetryAfter:  now.Add(time.Minute),
		},
		{
			name:   "403 secondary rate limit with remaining > 0 ignores primary reset",
			status: http.StatusForbidden,
			headers: map[string]string{
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Remaining": "4000",
				"X-RateLimit-Reset":     strconv.FormatInt(now.Add(55*time.Minute).Unix(), 10),
			},
			body:            `{"message":"You have exceeded a secondary rate limit."}`,
			wantRateLimited: true,
			wantTransient:   true,
			wantRetryAfter:  now.Add(time.Minute),
		},
		{
			name:   "403 permission denied is not rate limited",
			status: http.StatusForbidden,
			headers: map[string]string{
				"X-RateLimit-Limit":     "5000",
				"X-RateLimit-Remaining": "4999",
			},
			body:            `{"message":"Resource not accessible by integration"}`,
			wantRateLimited: false,
			wantTransient:   false,
		},
		{
			name:            "404 not found is not transient",
			status:          http.StatusNotFound,
			body:            `{"message":"Not Found"}`,
			wantRateLimited: false,
			wantTransient:   false,
		},
		{
			name:            "502 bad gateway is transient but not rate limited",
			status:          http.StatusBadGateway,
			body:            `{"message":"Bad Gateway"}`,
			wantRateLimited: false,
			wantTransient:   true,
		},
		{
			name:            "200 invalid json decode error is not transient",
			status:          http.StatusOK,
			body:            `not-json`,
			wantRateLimited: false,
			wantTransient:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := New(Options{
				BaseURL: srv.URL,
				AppID:   1,
				Scope:   ScopeRepo,
				Now:     func() time.Time { return now },
			})
			var out map[string]any
			err := c.doInstall(context.Background(), 77, http.MethodGet, "/test", "token ghs_x", nil, &out)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if got := IsRateLimited(err); got != tc.wantRateLimited {
				t.Errorf("IsRateLimited = %v, want %v (err=%v)", got, tc.wantRateLimited, err)
			}
			if got := IsTransient(err); got != tc.wantTransient {
				t.Errorf("IsTransient = %v, want %v (err=%v)", got, tc.wantTransient, err)
			}
			if !tc.wantRetryAfter.IsZero() {
				var ae *APIError
				if !errors.As(err, &ae) {
					t.Fatalf("expected *APIError, got %T", err)
				}
				if !ae.RetryAfter.Equal(tc.wantRetryAfter) {
					t.Errorf("RetryAfter = %v, want %v", ae.RetryAfter, tc.wantRetryAfter)
				}
			}
		})
	}

	// Transport / context errors are transient and not rate-limited.
	if IsRateLimited(context.DeadlineExceeded) || !IsTransient(context.DeadlineExceeded) {
		t.Errorf("context.DeadlineExceeded: IsRateLimited=%v IsTransient=%v, want false/true",
			IsRateLimited(context.DeadlineExceeded), IsTransient(context.DeadlineExceeded))
	}
}

func TestConditionalGet_304UsesCache(t *testing.T) {
	c, gh, ck := newTestClient(t, ScopeRepo)
	gh.AddRun(fakes.GitHubRun{
		ID: 42, Repo: repo.String(), HeadRepo: repo.String(), Event: "push", Status: "queued",
		Jobs: []fakes.GitHubJob{{ID: 420, Status: "queued", Labels: []string{"cr-ci"}}},
	})
	ctx := context.Background()

	jobs1, err := c.ListJobs(ctx, repo, 42)
	if err != nil {
		t.Fatalf("first ListJobs: %v", err)
	}
	if len(jobs1) != 1 || jobs1[0].ID != 420 {
		t.Fatalf("jobs1 = %+v", jobs1)
	}
	rl1, ok := c.RateLimit(repo)
	if !ok || rl1.Limit != 5000 || rl1.Remaining != 4999 {
		t.Fatalf("rl1 = %+v ok=%v, want limit=5000 remaining=4999", rl1, ok)
	}

	// Second call: unchanged -> 304, identical jobs, remaining unchanged, At updated.
	ck.add(time.Minute)
	jobs2, err := c.ListJobs(ctx, repo, 42)
	if err != nil {
		t.Fatalf("second ListJobs: %v", err)
	}
	if len(jobs2) != 1 || jobs2[0].ID != 420 || jobs2[0].Status != "queued" {
		t.Fatalf("jobs2 = %+v", jobs2)
	}
	jobsPath := "/repos/" + repo.String() + "/actions/runs/42/jobs"
	ok200, notMod304 := gh.CallsByStatus(jobsPath)
	if ok200 != 1 || notMod304 != 1 {
		t.Fatalf("CallsByStatus = (%d, %d), want (1, 1)", ok200, notMod304)
	}
	rl2, ok := c.RateLimit(repo)
	if !ok || rl2.Remaining != rl1.Remaining || !rl2.At.After(rl1.At) {
		t.Fatalf("rl2 = %+v (rl1 = %+v), want same remaining and later At", rl2, rl1)
	}

	// Mutate job -> 200 with updated data.
	gh.SetJob(42, fakes.GitHubJob{ID: 420, Status: "in_progress", Labels: []string{"cr-ci"}, RunnerName: "cr-job-420"})
	jobs3, err := c.ListJobs(ctx, repo, 42)
	if err != nil {
		t.Fatalf("third ListJobs: %v", err)
	}
	if len(jobs3) != 1 || jobs3[0].Status != "in_progress" {
		t.Fatalf("jobs3 = %+v, want in_progress", jobs3)
	}
	ok200, notMod304 = gh.CallsByStatus(jobsPath)
	if ok200 != 2 || notMod304 != 1 {
		t.Fatalf("after mutation CallsByStatus = (%d, %d), want (2, 1)", ok200, notMod304)
	}

	// 401 drops the installation's ETag cache too.
	c.mu.Lock()
	for id, tok := range c.tokens {
		tok.token = "ghs_revoked"
		c.tokens[id] = tok
	}
	c.mu.Unlock()
	if _, err := c.ListJobs(ctx, repo, 42); !IsStatus(err, http.StatusUnauthorized) {
		t.Fatalf("after revoke: err = %v, want 401", err)
	}
	if _, err := c.ListJobs(ctx, repo, 42); err != nil {
		t.Fatalf("after 401 re-auth: %v", err)
	}
	ok200, notMod304 = gh.CallsByStatus(jobsPath)
	if ok200 != 3 || notMod304 != 1 {
		t.Fatalf("after 401 cache invalidation CallsByStatus = (%d, %d), want (3, 1)", ok200, notMod304)
	}
}

func TestBreaker_SkipsCallsUntilReset(t *testing.T) {
	c, gh, ck := newTestClient(t, ScopeRepo)
	gh.AddRun(fakes.GitHubRun{
		ID: 7, Repo: repo.String(), HeadRepo: repo.String(), Event: "push", Status: "queued",
		Jobs: []fakes.GitHubJob{{ID: 70, Status: "queued", Labels: []string{"cr-ci"}}},
	})
	ctx := context.Background()

	// Trip a secondary 429 rate limit after 1 counted call.
	gh.RateLimitMode = fakes.Secondary429
	gh.RateLimitRetryAfter = 2 * time.Minute
	gh.RateLimitAfter = 1

	// First call succeeds (1 counted call).
	if _, err := c.ListJobs(ctx, repo, 7); err != nil {
		t.Fatalf("initial ListJobs: %v", err)
	}
	// Mutate so the next ListJobs is not a 304; this triggers the 429 and opens the breaker.
	gh.SetJob(7, fakes.GitHubJob{ID: 70, Status: "in_progress", Labels: []string{"cr-ci"}})
	err := func() error { _, e := c.ListJobs(ctx, repo, 7); return e }()
	if !IsRateLimited(err) {
		t.Fatalf("second ListJobs err = %v, want rate limited", err)
	}
	jobsPath := "/repos/" + repo.String() + "/actions/runs/7/jobs"
	callsBefore := gh.Calls(jobsPath)

	// While breaker is open, ListJobs returns synthetic 429 without hitting GitHub.
	_, err = c.ListJobs(ctx, repo, 7)
	if !IsRateLimited(err) || !IsStatus(err, http.StatusTooManyRequests) {
		t.Fatalf("breaker-open ListJobs err = %v, want synthetic 429", err)
	}
	if got := gh.Calls(jobsPath); got != callsBefore {
		t.Fatalf("ListJobs hit GitHub while breaker open: calls=%d, want %d", got, callsBefore)
	}

	// Exempt endpoints (GenerateJITConfig and DeleteRunner) still call GitHub.
	jitPath := "/repos/" + repo.String() + "/actions/runners/generate-jitconfig"
	delPath := "/repos/" + repo.String() + "/actions/runners/999"
	_, _, _ = c.GenerateJITConfig(ctx, repo, JITRequest{Name: "cr-job-70", RunnerGroupID: 1, Labels: []string{"cr-ci"}, WorkFolder: "/home/runner/work"})
	_ = c.DeleteRunner(ctx, repo, 999)
	if gh.Calls(jitPath) != 1 {
		t.Errorf("GenerateJITConfig calls = %d, want 1 (exempt from breaker)", gh.Calls(jitPath))
	}
	if gh.Calls(delPath) != 1 {
		t.Errorf("DeleteRunner calls = %d, want 1 (exempt from breaker)", gh.Calls(delPath))
	}

	// Advance clock past RetryAfter and lift fake limit: ListJobs calls GitHub again.
	gh.ResetRateLimit()
	ck.add(3 * time.Minute)
	jobs, err := c.ListJobs(ctx, repo, 7)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "in_progress" {
		t.Fatalf("after reset ListJobs = %+v %v", jobs, err)
	}
	if got := gh.Calls(jobsPath); got != callsBefore+1 {
		t.Errorf("after reset calls = %d, want %d", got, callsBefore+1)
	}
}

func TestBreaker_SecondaryWithoutRetryAfterReopensAfterOneMinute(t *testing.T) {
	c, gh, ck := newTestClient(t, ScopeRepo)
	gh.AddRun(fakes.GitHubRun{
		ID: 8, Repo: repo.String(), HeadRepo: repo.String(), Event: "push", Status: "queued",
		Jobs: []fakes.GitHubJob{{ID: 80, Status: "queued", Labels: []string{"cr-ci"}}},
	})
	ctx := context.Background()

	gh.RateLimitMode = fakes.Secondary403NoRetryAfter
	gh.RateLimitAfter = 1

	// First call succeeds (1 counted call).
	if _, err := c.ListJobs(ctx, repo, 8); err != nil {
		t.Fatalf("initial ListJobs: %v", err)
	}
	// Mutate so the next ListJobs is not a 304; triggers secondary 403 (Remaining=4000, Reset=now+55m, no Retry-After).
	gh.SetJob(8, fakes.GitHubJob{ID: 80, Status: "in_progress", Labels: []string{"cr-ci"}})
	triggerAt := ck.now()
	_, err := c.ListJobs(ctx, repo, 8)
	if !IsRateLimited(err) {
		t.Fatalf("second ListJobs err = %v, want rate limited", err)
	}
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %T, want *APIError", err)
	}
	wantUntil := triggerAt.Add(time.Minute)
	if !ae.RetryAfter.Equal(wantUntil) {
		t.Fatalf("RetryAfter = %v (delta %v), want %v (60s, not 55m primary Reset)", ae.RetryAfter, ae.RetryAfter.Sub(triggerAt), wantUntil)
	}

	jobsPath := "/repos/" + repo.String() + "/actions/runs/8/jobs"
	callsAfterTrip := gh.Calls(jobsPath)

	// Lift fake limit; at +30s the local breaker is still open (no HTTP call).
	gh.ResetRateLimit()
	ck.add(30 * time.Second)
	if _, err := c.ListJobs(ctx, repo, 8); !IsRateLimited(err) || !IsStatus(err, http.StatusTooManyRequests) {
		t.Fatalf("at +30s err = %v, want synthetic 429 from breaker", err)
	}
	if got := gh.Calls(jobsPath); got != callsAfterTrip {
		t.Fatalf("at +30s calls = %d, want %d", got, callsAfterTrip)
	}

	// At +61s (long before the 55m primary Reset), the breaker reopens to real HTTP calls.
	ck.add(31 * time.Second)
	jobs, err := c.ListJobs(ctx, repo, 8)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "in_progress" {
		t.Fatalf("at +61s ListJobs = %+v %v", jobs, err)
	}
	if got := gh.Calls(jobsPath); got != callsAfterTrip+1 {
		t.Errorf("at +61s calls = %d, want %d", got, callsAfterTrip+1)
	}
}
