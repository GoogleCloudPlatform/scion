package githubapp

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
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
			id, jit, err := c.GenerateJITConfig(ctx, repo, JITRequest{Name: "cr-job-1", RunnerGroupID: 1, Labels: []string{"lbl"}, WorkFolder: "/scratch/_work"})
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
