package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/claim"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/fakes"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/gcp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/githubapp"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/launcher"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/logging"
	"github.com/GoogleCloudPlatform/scion/cr-ci/mediator/internal/oidc"
)

const (
	testRepo        = "example-owner/example-repo"
	testLabel       = "scion-cr-8c32g"
	testMediatorURL = "https://mediator.example.com"
	testProject     = "example-project"
	testJob         = "projects/example-project/locations/us-central1/jobs/cr-gh-runner-8c32g"
	schedulerEmail  = "cr-runner-scheduler-sa@example-project.iam.gserviceaccount.com"
	execEmail       = "cr-runner-exec-sa@example-project.iam.gserviceaccount.com"
	webhookSecretID = "cr-runner-gh-webhook-secret"
	appKeySecretID  = "cr-runner-gh-app-private-key"
	fakeHookSecret  = "FAKE-webhook-secret-6d1f0c"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// harness wires a real Server, with real REST clients, to httptest fakes of
// GitHub, Cloud Run, the metadata server, Secret Manager and Google JWKS.
type harness struct {
	t      *testing.T
	clock  *testClock
	gh     *fakes.GitHub
	cr     *fakes.CloudRun
	gapis  *fakes.GoogleAPIs
	google *fakes.Google
	logs   *syncBuffer
	claims *claim.Store
	srv    *Server
	http   *httptest.Server

	oidcTokens []string
}

type harnessOpts struct {
	max       int
	claimTTL  time.Duration
	orgScope  bool
	mediator  string
	reuseFrom *harness // share the fakes (simulates a mediator restart)
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	if o.max == 0 {
		o.max = 5
	}
	if o.claimTTL == 0 {
		o.claimTTL = 300 * time.Second
	}
	if o.mediator == "" {
		o.mediator = testMediatorURL
	}
	h := &harness{t: t, logs: &syncBuffer{}}
	if p := o.reuseFrom; p != nil {
		h.clock, h.gh, h.cr, h.gapis, h.google = p.clock, p.gh, p.cr, p.gapis, p.google
	} else {
		h.clock = &testClock{t: time.Now().Truncate(time.Second)}
		appKey := fakes.Key(0)
		h.gh = fakes.NewGitHub(4242, &appKey.PublicKey)
		h.gh.Now = h.clock.Now
		h.cr = fakes.NewCloudRun(testJob)
		h.cr.Now = h.clock.Now
		h.gapis = fakes.NewGoogleAPIs(testProject, map[string][]byte{
			webhookSecretID: []byte(fakeHookSecret),
			appKeySecretID:  fakes.PEM(appKey),
		})
		h.google = fakes.NewGoogle(fakes.Key(1))
		t.Cleanup(func() { h.gh.Close(); h.cr.Close(); h.gapis.Close(); h.google.Close() })
	}

	tokens := &gcp.MetadataTokenSource{BaseURL: h.gapis.Server.URL, Now: h.clock.Now}
	sm := &gcp.SecretManager{BaseURL: h.gapis.Server.URL, Tokens: tokens, Project: testProject}
	scope := githubapp.ScopeRepo
	if o.orgScope {
		scope = githubapp.ScopeOrg
	}
	gh := githubapp.New(githubapp.Options{
		BaseURL: h.gh.Server.URL, AppID: 4242, Scope: scope, Now: h.clock.Now,
		PrivateKey: func(ctx context.Context) (*rsa.PrivateKey, error) {
			b, err := sm.Access(ctx, appKeySecretID)
			if err != nil {
				return nil, err
			}
			return githubapp.ParsePrivateKey(b)
		},
	})
	h.claims = claim.NewStore(o.claimTTL, h.clock.Now)
	h.srv = New(Options{
		TargetRepos:   []githubapp.Repo{{Owner: "example-owner", Name: "example-repo"}},
		OrgScope:      o.orgScope,
		RunnerLabel:   testLabel,
		MaxConcurrent: o.max,
		RunnerGroupID: 1,
		MediatorURL:   o.mediator,
		TaskTimeout:   45 * time.Minute,
		WebhookSecret: func(ctx context.Context) ([]byte, error) { return sm.Access(ctx, webhookSecretID) },
		GitHub:        gh,
		Launcher: &launcher.JobsLauncher{
			BaseURL: h.cr.Server.URL, Tokens: tokens, Project: testProject, Region: "us-central1",
			Job: "cr-gh-runner-8c32g", MediatorURL: o.mediator, Now: h.clock.Now,
		},
		Claims:        h.claims,
		ReconcileOIDC: &oidc.Verifier{CertsURL: h.google.Server.URL, Audience: o.mediator, Email: schedulerEmail, Now: h.clock.Now},
		RunnerOIDC:    &oidc.Verifier{CertsURL: h.google.Server.URL, Audience: o.mediator, Email: execEmail, Now: h.clock.Now},
		Logger:        logging.New(h.logs, slog.LevelDebug),
		Now:           h.clock.Now,
	})
	h.http = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.http.Close)
	return h
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func jobPayload(action string, jobID, runID int64, repo string, labels []string, conclusion string) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": action,
		"workflow_job": map[string]any{
			"id": jobID, "run_id": runID, "labels": labels, "conclusion": conclusion, "status": action,
		},
		"repository":   map[string]any{"full_name": repo},
		"installation": map[string]any{"id": 77},
	})
	return b
}

func (h *harness) post(path string, headers map[string]string, body []byte) (int, string, http.Header) {
	h.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.http.URL+path, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (h *harness) webhookWith(secret, event string, body []byte) (int, string) {
	code, out, _ := h.post("/webhook", map[string]string{
		"X-GitHub-Event": event, "X-GitHub-Delivery": "delivery-1", "X-Hub-Signature-256": sign(secret, body),
	}, body)
	return code, out
}

func (h *harness) webhook(body []byte) (int, string) {
	return h.webhookWith(fakeHookSecret, "workflow_job", body)
}

func (h *harness) queued(jobID, runID int64) (int, string) {
	h.gh.EnsureJob(runID, fakes.GitHubJob{ID: jobID, Status: "queued", Labels: []string{testLabel}, CreatedAt: h.clock.Now()})
	return h.webhook(jobPayload("queued", jobID, runID, testRepo, []string{testLabel}, ""))
}

func (h *harness) addSameRepoRun(runID int64, jobs ...fakes.GitHubJob) {
	h.gh.AddRun(fakes.GitHubRun{ID: runID, Repo: testRepo, HeadRepo: testRepo, Event: "pull_request", Status: "queued", Jobs: jobs})
}

// claim presents a valid runner identity and the claim token (contract v2).
func (h *harness) claim(token string) (int, string, http.Header) {
	return h.claimAs(h.google.IDToken(testMediatorURL, execEmail, h.clock.Now()), token)
}

func (h *harness) claimAs(idToken, token string) (int, string, http.Header) {
	hdr := map[string]string{}
	if idToken != "" {
		hdr["Authorization"] = "Bearer " + idToken
		h.oidcTokens = append(h.oidcTokens, idToken)
	}
	if token != "" {
		hdr[ClaimHeader] = token
	}
	return h.post("/internal/claim", hdr, nil)
}

func (h *harness) reconcileWith(token string) (int, ReconcileResult) {
	h.t.Helper()
	h.oidcTokens = append(h.oidcTokens, token)
	code, body, _ := h.post("/reconcile", map[string]string{"Authorization": "Bearer " + token}, nil)
	var res ReconcileResult
	if code == http.StatusOK || code == http.StatusInternalServerError {
		if err := json.Unmarshal([]byte(body), &res); err != nil {
			h.t.Fatalf("reconcile body %q: %v", body, err)
		}
	}
	return code, res
}

func (h *harness) reconcile() ReconcileResult {
	h.t.Helper()
	code, res := h.reconcileWith(h.google.IDToken(testMediatorURL, schedulerEmail, h.clock.Now()))
	if code != http.StatusOK {
		h.t.Fatalf("reconcile = %d %+v", code, res)
	}
	return res
}

func (h *harness) jitCount() int { _, n, _ := h.gh.Stats(); return n }

func (h *harness) executions() []fakes.Execution { return h.cr.Executions() }

func (h *harness) activeExecutions() int {
	n := 0
	for _, e := range h.cr.Executions() {
		if !e.Completed {
			n++
		}
	}
	return n
}

func (h *harness) wantLog(event string) {
	h.t.Helper()
	if !strings.Contains(h.logs.String(), `"event":"`+event+`"`) {
		h.t.Errorf("no %s log line in:\n%s", event, h.logs.String())
	}
}

// ---- HMAC, event, label and allowlist filtering ------------------------------

func TestWebhookHMAC(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}})
	body := jobPayload("queued", 100, 10, testRepo, []string{testLabel}, "")

	for name, hdr := range map[string]string{
		"missing":       "",
		"wrong secret":  sign("not-the-secret", body),
		"tampered body": sign(fakeHookSecret, append([]byte(" "), body...)),
		"sha1 prefix":   strings.Replace(sign(fakeHookSecret, body), "sha256=", "sha1=", 1),
		"not hex":       "sha256=zz",
	} {
		code, _, _ := h.post("/webhook", map[string]string{"X-GitHub-Event": "workflow_job", "X-Hub-Signature-256": hdr}, body)
		if code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
	if h.jitCount() != 0 || h.gh.RunGets != 0 {
		t.Fatal("rejected webhook reached GitHub")
	}
	h.wantLog(logging.EventWebhookRejected)

	if code, _ := h.webhook(body); code != http.StatusOK {
		t.Fatalf("valid signature: status %d", code)
	}
	if h.jitCount() != 1 {
		t.Errorf("valid webhook did not dispatch")
	}
}

func TestWebhookSecretRotation(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ping := []byte(`{"zen":"hi"}`)
	if code, _ := h.webhookWith(fakeHookSecret, "ping", ping); code != http.StatusOK {
		t.Fatalf("ping: %d", code)
	}
	h.gapis.SetSecret(webhookSecretID, []byte("FAKE-rotated-secret"))
	if code, _ := h.webhookWith("FAKE-rotated-secret", "ping", ping); code != http.StatusUnauthorized {
		t.Errorf("new secret accepted within the refetch backoff: %d", code)
	}
	h.clock.Add(61 * time.Second)
	if code, _ := h.webhookWith("FAKE-rotated-secret", "ping", ping); code != http.StatusOK {
		t.Errorf("rotated secret not picked up after the backoff: %d", code)
	}
	// The replaced secret stays valid for the grace period, so the order of
	// the two rotation steps does not matter.
	if code, _ := h.webhookWith(fakeHookSecret, "ping", ping); code != http.StatusOK {
		t.Errorf("old secret rejected within the grace period: %d", code)
	}
	h.clock.Add(time.Hour)
	if code, _ := h.webhookWith(fakeHookSecret, "ping", ping); code != http.StatusUnauthorized {
		t.Errorf("old secret still accepted after the grace period: %d", code)
	}
	if code, _ := h.webhookWith("FAKE-rotated-secret", "ping", ping); code != http.StatusOK {
		t.Errorf("current secret rejected: %d", code)
	}
}

// TestWebhookSecretUnavailable: while no secret could be loaded, deliveries
// get 503, and Secret Manager is retried at most every 10 s.
func TestWebhookSecretUnavailable(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.gapis.DeleteSecret(webhookSecretID)
	ping := []byte(`{"zen":"hi"}`)
	for i := 0; i < 3; i++ {
		if code, _ := h.webhookWith(fakeHookSecret, "ping", ping); code != http.StatusServiceUnavailable {
			t.Fatalf("no secret: %d, want 503", code)
		}
	}
	if n := h.gapis.AccessCount(webhookSecretID); n != 0 {
		t.Fatalf("missing secret counted as accessed: %d", n)
	}
	if n := h.gapis.MissCount(webhookSecretID); n != 1 {
		t.Errorf("Secret Manager asked %d times within 10 s, want 1", n)
	}
	h.gapis.SetSecret(webhookSecretID, []byte(fakeHookSecret))
	h.clock.Add(11 * time.Second)
	if code, _ := h.webhookWith(fakeHookSecret, "ping", ping); code != http.StatusOK {
		t.Errorf("after the secret appeared: %d", code)
	}
}

func TestWebhookIgnoresOtherEvents(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	body := jobPayload("queued", 100, 10, testRepo, []string{testLabel}, "")
	for _, ev := range []string{"ping", "workflow_run", "push"} {
		if code, out := h.webhookWith(fakeHookSecret, ev, body); code != http.StatusOK || !strings.Contains(out, "ignored") {
			t.Errorf("%s: %d %q", ev, code, out)
		}
	}
	if h.jitCount() != 0 || h.gh.RunGets != 0 {
		t.Error("non-workflow_job event reached GitHub")
	}
}

func TestWebhookLabelFiltering(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	for _, labels := range [][]string{{"ubuntu-latest"}, {"self-hosted", "linux"}, {}} {
		if code, _ := h.webhook(jobPayload("queued", 100, 10, testRepo, labels, "")); code != http.StatusOK {
			t.Errorf("labels %v: %d", labels, code)
		}
	}
	if h.jitCount() != 0 || h.gh.RunGets != 0 {
		t.Error("unlabelled job dispatched")
	}
	// Label match is case-insensitive, like GitHub's.
	h.gh.SetJob(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{"SCION-CR-8C32G"}})
	if code, _ := h.webhook(jobPayload("queued", 100, 10, testRepo, []string{"SCION-CR-8C32G"}, "")); code != http.StatusOK || h.jitCount() != 1 {
		t.Errorf("label match should be case-insensitive: %d jit=%d", code, h.jitCount())
	}
}

func TestWebhookAllowlist(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.gh.AddRun(fakes.GitHubRun{ID: 10, Repo: "other-owner/example-repo", HeadRepo: "other-owner/example-repo", Event: "push", Status: "queued"})
	if code, _ := h.webhook(jobPayload("queued", 100, 10, "other-owner/example-repo", []string{testLabel}, "")); code != http.StatusOK {
		t.Errorf("status %d", code)
	}
	if h.jitCount() != 0 || h.gh.RunGets != 0 {
		t.Error("repo outside TARGET_REPOS dispatched")
	}
	h.wantLog(logging.EventWebhookRejected)
}

func TestMediatorURLUnset(t *testing.T) {
	h := newHarness(t, harnessOpts{mediator: "-"})
	h.srv.o.MediatorURL = ""
	h.addSameRepoRun(10)
	if code, _ := h.queued(100, 10); code != http.StatusServiceUnavailable {
		t.Errorf("webhook without MEDIATOR_URL: %d, want 503", code)
	}
	if code, _ := h.reconcileWith("x"); code != http.StatusServiceUnavailable {
		t.Errorf("reconcile without MEDIATOR_URL: %d, want 503", code)
	}
	if h.jitCount() != 0 {
		t.Error("dispatched without MEDIATOR_URL")
	}
}

// ---- fork guard --------------------------------------------------------------

func TestForkGuard(t *testing.T) {
	tests := []struct {
		name, head, event string
	}{
		{"head repository differs", "contributor/example-repo", "pull_request"},
		{"pull_request_target", testRepo, "pull_request_target"},
		{"workflow_run", testRepo, "workflow_run"},
		{"head repository missing", "", "pull_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{})
			h.gh.AddRun(fakes.GitHubRun{ID: 10, Repo: testRepo, HeadRepo: tt.head, Event: tt.event, Status: "queued"})
			// The payload looks harmless; the guard must use the API.
			code, out := h.queued(100, 10)
			if code != http.StatusOK || !strings.Contains(out, "rejected") {
				t.Errorf("status %d %q, want 200 rejected", code, out)
			}
			if h.gh.RunGets != 1 {
				t.Errorf("run fetched %d times, want 1 (API check)", h.gh.RunGets)
			}
			if h.jitCount() != 0 || len(h.executions()) != 0 {
				t.Error("fork run got a runner")
			}
			h.wantLog(logging.EventRejectFork)
		})
	}
}

// ---- queued: launch, idempotency, capacity -----------------------------------

func TestQueuedLaunchAndClaim(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	code, out := h.queued(100, 10)
	if code != http.StatusOK || !strings.Contains(out, "launched") {
		t.Fatalf("queued: %d %q", code, out)
	}
	req := h.gh.JITRequests[0]
	if req["name"] != "cr-job-100" || req["runner_group_id"] != float64(1) || req["work_folder"] != "/home/runner/work" {
		t.Errorf("jit request = %v", req)
	}
	if ls, _ := req["labels"].([]any); len(ls) != 1 || ls[0] != testLabel {
		t.Errorf("jit labels = %v, want only %q", req["labels"], testLabel)
	}
	ex := h.executions()
	if len(ex) != 1 {
		t.Fatalf("executions = %d", len(ex))
	}
	env := ex[0].Env
	if env["GH_JOB_ID"] != "100" || env["RUNNER_NAME"] != "cr-job-100" || env["MEDIATOR_URL"] != testMediatorURL {
		t.Errorf("env = %v", env)
	}
	jit := h.gh.IssuedJIT[0]
	for k, v := range env {
		if strings.Contains(v, jit) || strings.Contains(strings.ToUpper(k), "JIT") {
			t.Errorf("JIT config in env override %s", k)
		}
	}
	if strings.Contains(h.cr.RunBodies[0], jit) {
		t.Error("JIT config in the RunJob request")
	}
	h.wantLog(logging.EventDispatch)

	code, body, hdr := h.claim(env["RUNNER_CLAIM_TOKEN"])
	if code != http.StatusOK || hdr.Get("Content-Type") != "application/json" || hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("claim: %d %v", code, hdr)
	}
	var got struct {
		JITConfig string `json:"jit_config"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil || got.JITConfig != jit {
		t.Fatalf("claim body %q", body)
	}
	if code, _, _ := h.claim(env["RUNNER_CLAIM_TOKEN"]); code != http.StatusGone {
		t.Errorf("second claim: %d, want 410 (single use)", code)
	}
}

func TestIdempotency(t *testing.T) {
	t.Run("duplicate delivery", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.queued(100, 10)
		code, out := h.queued(100, 10)
		if code != http.StatusOK || !strings.Contains(out, "already") {
			t.Errorf("second delivery: %d %q", code, out)
		}
		if h.jitCount() != 1 || len(h.executions()) != 1 {
			t.Errorf("jit=%d executions=%d, want 1 and 1", h.jitCount(), len(h.executions()))
		}
		h.wantLog(logging.EventDispatchSkip)
	})
	t.Run("concurrent deliveries", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); h.queued(100, 10) }()
		}
		wg.Wait()
		if h.jitCount() != 1 || len(h.executions()) != 1 {
			t.Errorf("jit=%d executions=%d, want 1 and 1", h.jitCount(), len(h.executions()))
		}
	})
	t.Run("runner already registered", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.gh.AddRunner(fakes.GitHubRunner{Name: "cr-job-100", Status: "online", Scope: "repo:" + testRepo})
		h.queued(100, 10)
		if h.jitCount() != 0 || len(h.executions()) != 0 {
			t.Error("dispatched although cr-job-100 is registered")
		}
	})
	t.Run("execution already launched (after restart)", func(t *testing.T) {
		h := newHarness(t, harnessOpts{})
		h.addSameRepoRun(10)
		h.cr.AddExecution(fakes.Execution{Created: h.clock.Now(), Env: map[string]string{"GH_JOB_ID": "100"}})
		h.queued(100, 10)
		if h.jitCount() != 0 || len(h.executions()) != 1 {
			t.Error("dispatched although an execution for job 100 is running")
		}
	})
}

func TestCapacityBackpressure(t *testing.T) {
	h := newHarness(t, harnessOpts{max: 2})
	h.addSameRepoRun(10)
	h.cr.AddExecution(fakes.Execution{Created: h.clock.Now(), Env: map[string]string{"GH_JOB_ID": "1"}})
	h.cr.AddExecution(fakes.Execution{Created: h.clock.Now(), Env: map[string]string{"GH_JOB_ID": "2"}})
	h.cr.AddExecution(fakes.Execution{Created: h.clock.Now(), Completed: true, Env: map[string]string{"GH_JOB_ID": "3"}})
	code, out := h.queued(100, 10)
	if code != http.StatusOK || !strings.Contains(out, "capacity") {
		t.Errorf("queued at capacity: %d %q", code, out)
	}
	if h.jitCount() != 0 {
		t.Error("JIT config minted while at capacity")
	}
	if len(h.gh.Runners()) != 0 {
		t.Error("registration created while at capacity")
	}
	h.wantLog(logging.EventCapacityFull)

	h.cr.Complete("1")
	if code, out := h.queued(100, 10); code != http.StatusOK || !strings.Contains(out, "launched") {
		t.Errorf("after a slot frees: %d %q", code, out)
	}
}

func TestRunJobFailureDeletesRegistration(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.cr.FailRun = true
	if code, _ := h.queued(100, 10); code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", code)
	}
	if h.jitCount() != 1 {
		t.Fatalf("jit = %d, want 1", h.jitCount())
	}
	if len(h.gh.Runners()) != 0 || len(h.gh.Deleted) != 1 {
		t.Errorf("registration not deleted after RunJob failure: runners=%v deleted=%v", h.gh.Runners(), h.gh.Deleted)
	}
	if h.claims.Len() != 0 {
		t.Errorf("claim not dropped: %d left", h.claims.Len())
	}
	h.wantLog(logging.EventLaunchFailed)

	// The job is retried cleanly once Cloud Run recovers.
	h.cr.FailRun = false
	if code, out := h.queued(100, 10); code != http.StatusOK || !strings.Contains(out, "launched") {
		t.Errorf("retry: %d %q", code, out)
	}
}

// ---- claim contract ----------------------------------------------------------

func TestClaimContract(t *testing.T) {
	h := newHarness(t, harnessOpts{claimTTL: 300 * time.Second})
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.queued(100, 10)
	h.queued(101, 11)
	ex := h.executions()
	tokA, tokB := ex[0].Env["RUNNER_CLAIM_TOKEN"], ex[1].Env["RUNNER_CLAIM_TOKEN"]
	for _, tok := range []string{tokA, tokB} {
		if len(tok) != 32 || strings.Trim(tok, "0123456789abcdef") != "" {
			t.Fatalf("claim token %q is not 32 lowercase hex chars", tok)
		}
	}
	now := h.clock.Now()

	// Identity first: a missing or invalid runner identity is 401 and does
	// not consume the claim.
	for name, id := range map[string]string{
		"no identity":     "",
		"wrong audience":  h.google.IDToken("https://elsewhere.example.com", execEmail, now),
		"wrong email":     h.google.IDToken(testMediatorURL, schedulerEmail, now),
		"expired":         h.google.IDToken(testMediatorURL, execEmail, now.Add(-3*time.Hour)),
		"forged":          fakes.Mint(fakes.Key(2), h.google.Kid, map[string]any{"iss": "https://accounts.google.com", "aud": testMediatorURL, "email": execEmail, "email_verified": true, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}),
		"not a jwt token": "0123456789abcdef0123456789abcdef",
	} {
		if code, _, _ := h.claimAs(id, tokA); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	h.wantLog(logging.EventClaimRejected)

	// Valid identity, bad claim header: 410.
	id := h.google.IDToken(testMediatorURL, execEmail, now)
	for name, tok := range map[string]string{
		"no claim header": "",
		"unknown token":   "0123456789abcdef0123456789abcdef",
		"short":           "abc",
	} {
		if code, _, _ := h.claimAs(id, tok); code != http.StatusGone {
			t.Errorf("%s: %d, want 410", name, code)
		}
	}

	// The claim survived every rejected attempt above.
	h.clock.Add(299 * time.Second)
	code, body, hdr := h.claim(tokA)
	if code != http.StatusOK || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("claim within TTL after rejected attempts: %d %q", code, body)
	}
	if code, _, _ := h.claim(tokA); code != http.StatusGone {
		t.Errorf("used claim: %d, want 410", code)
	}
	h.clock.Add(time.Second)
	if code, _, _ := h.claim(tokB); code != http.StatusGone {
		t.Errorf("claim at TTL: %d, want 410", code)
	}
	h.wantLog(logging.EventClaimConsumed)
}

// ---- completed / cancellation ------------------------------------------------

func TestCancellation(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.queued(100, 10)
	h.queued(101, 11)

	// Normal completion on its runner: nothing to cancel.
	if code, _ := h.webhook(jobEvent("completed", 101, 11, testRepo, "success", "cr-job-101")); code != http.StatusOK {
		t.Fatalf("completed: %d", code)
	}
	if len(h.cr.CancelCalls) != 0 || len(h.gh.Deleted) != 0 {
		t.Error("successful completion cancelled or deleted something")
	}

	// Cancelled while queued (no runner_name): its idle runner is stopped.
	code, out := h.webhook(jobPayload("completed", 100, 10, testRepo, []string{testLabel}, "cancelled"))
	if code != http.StatusOK {
		t.Fatalf("cancelled: %d %q", code, out)
	}
	if len(h.cr.CancelCalls) != 1 || !strings.HasSuffix(h.cr.CancelCalls[0], "/exec-1") {
		t.Errorf("cancel calls = %v", h.cr.CancelCalls)
	}
	if _, ok := h.gh.RunnerByName("cr-job-100"); ok {
		t.Error("leftover registration not deleted")
	}
	if h.claims.Pending(100) {
		t.Error("claim for a cancelled job still pending")
	}
	h.wantLog(logging.EventCancel)
}

func TestCancellationAfterRestart(t *testing.T) {
	first := newHarness(t, harnessOpts{})
	first.addSameRepoRun(10)
	first.queued(100, 10)
	h := newHarness(t, harnessOpts{reuseFrom: first}) // fresh memory, same world
	if code, _ := h.webhook(jobPayload("completed", 100, 10, testRepo, []string{testLabel}, "cancelled")); code != http.StatusOK {
		t.Fatalf("cancelled: %d", code)
	}
	if h.activeExecutions() != 0 {
		t.Error("execution not found and cancelled by GH_JOB_ID")
	}
	if _, ok := h.gh.RunnerByName("cr-job-100"); ok {
		t.Error("registration not found and deleted by name")
	}
}

// ---- reconcile ---------------------------------------------------------------

func TestReconcileAuth(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	now := h.clock.Now()
	bad := map[string]string{
		"none":           "",
		"garbage":        "not-a-jwt",
		"wrong audience": h.google.IDToken("https://elsewhere.example.com", schedulerEmail, now),
		"wrong email":    h.google.IDToken(testMediatorURL, "someone@example-project.iam.gserviceaccount.com", now),
		"expired":        h.google.IDToken(testMediatorURL, schedulerEmail, now.Add(-3*time.Hour)),
		"forged":         fakes.Mint(fakes.Key(2), h.google.Kid, map[string]any{"iss": "https://accounts.google.com", "aud": testMediatorURL, "email": schedulerEmail, "email_verified": true, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}),
	}
	for name, tok := range bad {
		if code, _ := h.reconcileWith(tok); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	h.wantLog(logging.EventReconcileDenied)
	if code, _ := h.reconcileWith(h.google.IDToken(testMediatorURL, schedulerEmail, now)); code != http.StatusOK {
		t.Errorf("valid token: %d", code)
	}
	h.wantLog(logging.EventReconcile)
}

func TestReconcileReapsStaleRunners(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	scope := "repo:" + testRepo
	h.gh.AddRunner(fakes.GitHubRunner{Name: "cr-job-1", Status: "offline", Scope: scope})            // orphan
	h.gh.AddRunner(fakes.GitHubRunner{Name: "cr-job-2", Status: "online", Busy: true, Scope: scope}) // working
	h.gh.AddRunner(fakes.GitHubRunner{Name: "someone-else", Status: "offline", Scope: scope})        // not ours
	h.gh.AddRunner(fakes.GitHubRunner{Name: "cr-job-x", Status: "offline", Scope: scope})            // not a job name

	if res := h.reconcile(); res.RunnersReaped != 0 {
		t.Fatalf("reaped %d on first sighting, want 0", res.RunnersReaped)
	}
	h.clock.Add(4 * time.Minute)
	if res := h.reconcile(); res.RunnersReaped != 0 {
		t.Fatalf("reaped %d after 4 min offline, want 0", res.RunnersReaped)
	}
	h.clock.Add(time.Minute)
	if res := h.reconcile(); res.RunnersReaped != 1 {
		t.Fatalf("reaped %d after 5 min offline, want 1", res.RunnersReaped)
	}
	var names []string
	for _, r := range h.gh.Runners() {
		names = append(names, r.Name)
	}
	if strings.Join(names, ",") != "cr-job-2,someone-else,cr-job-x" {
		t.Errorf("remaining runners = %v", names)
	}
	h.wantLog(logging.EventRunnerReaped)
}

// TestReconcileKeepsColdStartingRunners: a runner launched by this process is
// aged from its launch, but not reaped while its claim is still pending.
func TestReconcileKeepsColdStartingRunners(t *testing.T) {
	h := newHarness(t, harnessOpts{claimTTL: 10 * time.Minute})
	h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}})
	h.queued(100, 10)
	h.gh.SetJob(10, fakes.GitHubJob{ID: 100, Status: "in_progress", Labels: []string{testLabel}})
	h.reconcile()
	h.clock.Add(6 * time.Minute)
	if res := h.reconcile(); res.RunnersReaped != 0 {
		t.Fatal("reaped a runner whose claim is still pending")
	}
	h.clock.Add(5 * time.Minute) // claim expired; launched 11 min ago
	if res := h.reconcile(); res.RunnersReaped != 1 {
		t.Fatalf("reaped %d, want 1 once the claim expired", res.RunnersReaped)
	}
}

// TestReconcileReapsIdleStoppedRunnerOnFirstPass: a runner the entrypoint stopped
// for its idle timeout (RUNNER_IDLE_TIMEOUT, default 10m, longer than ReapAfter)
// leaves an offline, never-busy registration; it is aged from its launch, so the
// first reconcile after the stop deletes it.
func TestReconcileReapsIdleStoppedRunnerOnFirstPass(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}})
	h.queued(100, 10)
	tok := h.executions()[0].Env["RUNNER_CLAIM_TOKEN"]
	if code, _, _ := h.claim(tok); code != http.StatusOK {
		t.Fatalf("claim: %d", code)
	}
	// Job 100 is cancelled before GitHub hands it to the runner, which then sits idle.
	h.gh.SetJob(10, fakes.GitHubJob{ID: 100, Status: "completed", Labels: []string{testLabel}})
	h.gh.SetRunnerState("cr-job-100", "online", false)
	h.clock.Add(10 * time.Minute)
	h.gh.SetRunnerState("cr-job-100", "offline", false) // idle timeout: run.sh stopped
	h.cr.Complete("100")
	if res := h.reconcile(); res.RunnersReaped != 1 {
		t.Fatalf("reaped %d on the first pass after the idle stop, want 1", res.RunnersReaped)
	}
	h.wantLog(logging.EventRunnerReaped)
}

func TestReconcileBackfill(t *testing.T) {
	h := newHarness(t, harnessOpts{max: 2})
	t0 := h.clock.Now().Add(-10 * time.Minute)
	h.addSameRepoRun(10,
		fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}, CreatedAt: t0.Add(2 * time.Minute)},
		fakes.GitHubJob{ID: 101, Status: "queued", Labels: []string{"ubuntu-latest"}, CreatedAt: t0},
		fakes.GitHubJob{ID: 102, Status: "completed", Labels: []string{testLabel}, CreatedAt: t0},
	)
	h.gh.AddRun(fakes.GitHubRun{ID: 11, Repo: testRepo, HeadRepo: testRepo, Event: "push", Status: "in_progress",
		Jobs: []fakes.GitHubJob{{ID: 110, Status: "queued", Labels: []string{testLabel}, CreatedAt: t0.Add(time.Minute)}}})
	h.gh.AddRun(fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: testRepo, Event: "push", Status: "queued",
		Jobs: []fakes.GitHubJob{{ID: 120, Status: "queued", Labels: []string{testLabel}, CreatedAt: t0.Add(3 * time.Minute)}}})
	h.gh.AddRun(fakes.GitHubRun{ID: 13, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "queued",
		Jobs: []fakes.GitHubJob{{ID: 130, Status: "queued", Labels: []string{testLabel}, CreatedAt: t0}}})

	res := h.reconcile()
	if res.Backfilled != 2 || res.LeftQueued != 1 || !res.CapacityFullAtStop || res.ForkRunsRejected != 1 {
		t.Fatalf("result = %+v, want 2 backfilled, 1 left queued, 1 fork rejected", res)
	}
	got := map[string]bool{}
	for _, e := range h.executions() {
		got[e.Env["GH_JOB_ID"]] = true
	}
	if !got["110"] || !got["100"] || got["120"] || got["130"] {
		t.Errorf("launched %v, want the two oldest (110, 100) and never the fork job 130", got)
	}
	h.wantLog(logging.EventRejectFork)

	// A slot frees: the next reconcile launches the remaining job, and does
	// not relaunch jobs that already have a runner.
	h.cr.Complete("110")
	res = h.reconcile()
	if res.Backfilled != 1 || h.jitCount() != 3 {
		t.Errorf("second pass: %+v jit=%d, want 1 backfilled, 3 mints total", res, h.jitCount())
	}
}

// TestRestartRecovery: a mediator restart loses the pending claim; the runner
// exits 3, reconcile reaps the orphaned registration and relaunches the job.
func TestRestartRecovery(t *testing.T) {
	first := newHarness(t, harnessOpts{})
	first.addSameRepoRun(10, fakes.GitHubJob{ID: 100, Status: "queued", Labels: []string{testLabel}, CreatedAt: first.clock.Now()})
	first.queued(100, 10)
	lost := first.executions()[0].Env["RUNNER_CLAIM_TOKEN"]

	h := newHarness(t, harnessOpts{reuseFrom: first})
	if code, _, _ := h.claim(lost); code != http.StatusGone {
		t.Fatalf("claim after restart: %d, want 410", code)
	}
	h.cr.Complete("100") // runner exited 3
	h.reconcile()
	h.clock.Add(5 * time.Minute)
	res := h.reconcile()
	if res.RunnersReaped != 1 || res.Backfilled != 1 {
		t.Fatalf("result = %+v, want the orphan reaped and the job relaunched", res)
	}
	if len(h.executions()) != 2 {
		t.Errorf("executions = %d, want 2", len(h.executions()))
	}
}

func TestOrgScopeListsRunnersOncePerOwner(t *testing.T) {
	h := newHarness(t, harnessOpts{orgScope: true})
	h.addSameRepoRun(10)
	h.queued(100, 10)
	rn, ok := h.gh.RunnerByName("cr-job-100")
	if !ok || rn.Scope != "org:example-owner" {
		t.Errorf("org scope registered %+v", rn)
	}
}

// ---- no secrets in logs ------------------------------------------------------

// TestNoSecretsInLogs drives every path (accepted and rejected webhooks,
// launch, launch failure, claim, cancellation, reconcile accept and deny) at
// debug level and asserts that no credential appears in any log line.
func TestNoSecretsInLogs(t *testing.T) {
	h := newHarness(t, harnessOpts{max: 3})
	t0 := h.clock.Now()
	h.addSameRepoRun(10)
	h.addSameRepoRun(11)
	h.gh.AddRun(fakes.GitHubRun{ID: 12, Repo: testRepo, HeadRepo: "contributor/example-repo", Event: "pull_request", Status: "queued"})
	h.addSameRepoRun(13, fakes.GitHubJob{ID: 130, Status: "queued", Labels: []string{testLabel}, CreatedAt: t0})

	h.webhookWith("wrong-secret", "workflow_job", jobPayload("queued", 100, 10, testRepo, []string{testLabel}, ""))
	h.queued(100, 10)
	h.queued(100, 10)
	h.webhook(jobPayload("queued", 120, 12, testRepo, []string{testLabel}, ""))
	h.cr.FailRun = true
	h.queued(110, 11)
	h.cr.FailRun = false
	h.queued(110, 11)
	for _, e := range h.executions() {
		h.claim(e.Env["RUNNER_CLAIM_TOKEN"])
		h.claim(e.Env["RUNNER_CLAIM_TOKEN"])
	}
	h.webhook(jobPayload("completed", 100, 10, testRepo, []string{testLabel}, "cancelled"))
	h.reconcileWith(h.google.IDToken("https://elsewhere.example.com", schedulerEmail, t0))
	h.reconcile()
	h.clock.Add(6 * time.Minute)
	h.reconcile()
	h.gh.FailDelete = true
	h.webhook(jobPayload("completed", 110, 11, testRepo, []string{testLabel}, "cancelled"))

	logs := h.logs.String()
	if !strings.Contains(logs, logging.EventDispatch) || !strings.Contains(logs, logging.EventLaunchFailed) {
		t.Fatalf("scenario did not exercise the expected paths:\n%s", logs)
	}

	secrets := map[string]string{"webhook secret": fakeHookSecret}
	pemText := string(fakes.PEM(fakes.Key(0)))
	secrets["app private key (PEM)"] = pemText
	for i, line := range strings.Split(pemText, "\n") {
		if len(line) >= 32 && !strings.HasPrefix(line, "-----") {
			secrets["app private key line "+strconv.Itoa(i)] = line
		}
	}
	for i, s := range h.gh.Secrets() { // JWTs, installation tokens, JIT configs
		secrets["github credential "+strconv.Itoa(i)] = s
		if parts := strings.Split(s, "."); len(parts) == 3 {
			secrets["jwt signature "+strconv.Itoa(i)] = parts[2]
		}
	}
	for i, s := range h.gapis.Secrets() {
		secrets["gcp access token "+strconv.Itoa(i)] = s
	}
	for i, e := range h.executions() {
		secrets["claim token "+strconv.Itoa(i)] = e.Env["RUNNER_CLAIM_TOKEN"]
	}
	for i, s := range h.oidcTokens {
		secrets["oidc token "+strconv.Itoa(i)] = s
	}
	if len(h.gh.IssuedJIT) < 3 || len(h.gh.IssuedTokens) == 0 || len(h.gh.IssuedJWTs) == 0 || len(h.executions()) < 3 {
		t.Fatalf("scenario issued too few credentials to be meaningful: jit=%d tokens=%d jwts=%d executions=%d",
			len(h.gh.IssuedJIT), len(h.gh.IssuedTokens), len(h.gh.IssuedJWTs), len(h.executions()))
	}
	for name, s := range secrets {
		if s == "" {
			t.Errorf("%s is empty; the scenario is broken", name)
			continue
		}
		if strings.Contains(logs, s) {
			t.Errorf("%s appears in the logs", name)
		}
	}
}
