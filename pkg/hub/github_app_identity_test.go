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
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var (
	sharedGitHubTestKeyOnce sync.Once
	sharedGitHubTestKeyPEM  string
)

func testGitHubAppPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	sharedGitHubTestKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		block := &pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		}
		sharedGitHubTestKeyPEM = string(pem.EncodeToMemory(block))
	})
	return sharedGitHubTestKeyPEM
}

func setGitHubTriggersExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.GitHubTriggers {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, experiments.Default().Retired())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	srv.mu.Lock()
	srv.experiments = reg
	srv.mu.Unlock()
	if got := srv.githubTriggersEnabled(); got != on {
		t.Fatalf("githubTriggersEnabled() = %v, want %v", got, on)
	}
}

type mockAppEndpoint struct {
	server *httptest.Server
	hits   atomic.Int64

	mu     sync.Mutex
	status int
	body   string
}

func newMockAppEndpoint(t *testing.T, status int, payload any) *mockAppEndpoint {
	t.Helper()
	m := &mockAppEndpoint{status: status}
	m.setPayload(t, status, payload)
	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app" || r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		m.hits.Add(1)
		m.mu.Lock()
		code := m.status
		body := m.body
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockAppEndpoint) setPayload(t *testing.T, status int, payload any) {
	t.Helper()
	var raw string
	switch v := payload.(type) {
	case string:
		raw = v
	default:
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal mock /app payload: %v", err)
		}
		raw = string(b)
	}
	m.mu.Lock()
	m.status = status
	m.body = raw
	m.mu.Unlock()
}

func TestGitHubAppIdentity_StartupAndLazyPopulation(t *testing.T) {
	t.Run("startup_populates_when_key_present", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":   42,
			"slug": "scion-bot",
			"events": []string{
				"pull_request",
				"issues",
			},
			"permissions": map[string]string{
				"contents": "write",
				"metadata": "read",
			},
		})

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      42,
			PrivateKey: testGitHubAppPrivateKeyPEM(t),
			APIBaseURL: mockGH.server.URL,
		}
		srv.mu.Unlock()

		ctx := context.Background()
		srv.initGitHubAppIdentity(ctx)

		if got := mockGH.hits.Load(); got != 1 {
			t.Fatalf("expected 1 GET /app on startup, got %d", got)
		}

		id, ok := srv.GitHubAppIdentity(ctx)
		if !ok || id == nil {
			t.Fatal("expected cached GitHubAppIdentity after startup")
		}
		if id.Slug != "scion-bot" || id.BotLogin != "scion-bot[bot]" {
			t.Errorf("unexpected identity: %+v", id)
		}
		if got := mockGH.hits.Load(); got != 1 {
			t.Errorf("expected GitHubAppIdentity to serve from cache (hits=1), got %d", got)
		}
	})

	t.Run("lazy_populates_when_startup_had_no_key", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":          77,
			"slug":        "lazy-scion",
			"events":      []string{"issue_comment"},
			"permissions": map[string]string{"issues": "write"},
		})

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      77,
			APIBaseURL: mockGH.server.URL,
		}
		srv.mu.Unlock()

		ctx := context.Background()
		srv.initGitHubAppIdentity(ctx)
		if got := mockGH.hits.Load(); got != 0 {
			t.Fatalf("expected 0 GET /app on startup without private key, got %d", got)
		}
		if _, ok := srv.GitHubAppIdentity(ctx); ok {
			t.Fatal("expected no cached identity before private key is configured")
		}

		key := testGitHubAppPrivateKeyPEM(t)
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
			PrivateKey: &key,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT /api/v1/github-app failed: %d %s", rec.Code, rec.Body.String())
		}

		id, ok := srv.GitHubAppIdentity(ctx)
		if !ok || id == nil {
			t.Fatal("expected lazy GitHubAppIdentity fetch after key configuration")
		}
		if id.Slug != "lazy-scion" || id.BotLogin != "lazy-scion[bot]" {
			t.Errorf("unexpected lazy identity: %+v", id)
		}
		if got := mockGH.hits.Load(); got != 1 {
			t.Errorf("expected exactly 1 lazy GET /app call, got %d", got)
		}

		// Subsequent calls hit the cache rather than repeating GET /app.
		_, _ = srv.GitHubAppIdentity(ctx)
		if got := mockGH.hits.Load(); got != 1 {
			t.Errorf("expected cached follow-up read (hits=1), got %d", got)
		}
	})

	t.Run("startup_background_skips_when_off_and_runs_async_when_on", func(t *testing.T) {
		ctx := context.Background()

		// 1. Flag off -> startGitHubAppIdentityInit skips GET /app completely.
		offGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":   42,
			"slug": "off-at-start",
		})
		srvOff, _ := testServer(t)
		srvOff.mu.Lock()
		srvOff.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      42,
			PrivateKey: testGitHubAppPrivateKeyPEM(t),
			APIBaseURL: offGH.server.URL,
		}
		srvOff.mu.Unlock()

		srvOff.startGitHubAppIdentityInit(ctx)
		srvOff.stopGitHubAppIdentityInit()
		if got := offGH.hits.Load(); got != 0 {
			t.Fatalf("expected 0 GET /app calls at startup while hub.github_triggers is off, got %d", got)
		}

		// 2. Flag on -> startGitHubAppIdentityInit returns immediately without blocking on GET /app.
		entered := make(chan struct{}, 1)
		release := make(chan struct{})
		var asyncHits atomic.Int64
		blockingGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/app" || r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			asyncHits.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"slug":"async-scion","events":["issues"]}`))
		}))
		t.Cleanup(func() {
			select {
			case <-release:
			default:
				close(release)
			}
			blockingGH.Close()
		})

		srvOn, _ := testServer(t)
		t.Cleanup(srvOn.stopGitHubAppIdentityInit)
		setGitHubTriggersExperiment(t, srvOn, true)
		srvOn.mu.Lock()
		srvOn.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      42,
			PrivateKey: testGitHubAppPrivateKeyPEM(t),
			APIBaseURL: blockingGH.URL,
		}
		srvOn.mu.Unlock()

		// Must return without waiting for release.
		srvOn.startGitHubAppIdentityInit(ctx)
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("background startup identity fetch never started")
		}

		// Release the mock response and wait for the background fetch to finish
		// before invoking shutdown cancellation (stopGitHubAppIdentityInit).
		close(release)
		srvOn.githubAppIdentity.startupWG.Wait()
		srvOn.stopGitHubAppIdentityInit()

		id, ok := srvOn.GitHubAppIdentity(ctx)
		if !ok || id == nil || id.Slug != "async-scion" {
			t.Fatalf("expected async startup identity async-scion, got %+v (ok=%v)", id, ok)
		}
		if got := asyncHits.Load(); got != 1 {
			t.Errorf("expected 1 GET /app call across async startup + cached read, got %d", got)
		}
	})

	t.Run("reset_during_in_flight_refresh_discards_stale_result", func(t *testing.T) {
		ctx := context.Background()

		enteredOld := make(chan struct{}, 1)
		releaseOld := make(chan struct{})
		oldGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/app" || r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			select {
			case enteredOld <- struct{}{}:
			default:
			}
			<-releaseOld
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"slug":"stale-old-app","events":["issues"]}`))
		}))
		t.Cleanup(func() {
			select {
			case <-releaseOld:
			default:
				close(releaseOld)
			}
			oldGH.Close()
		})

		newGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":     42,
			"slug":   "fresh-new-app",
			"events": []string{"pull_request"},
		})

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      42,
			PrivateKey: testGitHubAppPrivateKeyPEM(t),
			APIBaseURL: oldGH.URL,
		}
		srv.mu.Unlock()

		var refreshDone sync.WaitGroup
		refreshDone.Add(1)
		go func() {
			defer refreshDone.Done()
			_, _ = srv.refreshGitHubAppIdentity(ctx)
		}()

		select {
		case <-enteredOld:
		case <-time.After(5 * time.Second):
			t.Fatal("in-flight refresh against oldGH never started")
		}

		// Update APIBaseURL on the SAME AppID while oldGH's GET /app is in flight.
		newURL := newGH.server.URL
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
			APIBaseURL: &newURL,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT /api/v1/github-app failed: %d %s", rec.Code, rec.Body.String())
		}

		// Let the stale in-flight refresh finish AFTER reset(): its generation is
		// stale so it must not populate the cache with "stale-old-app".
		close(releaseOld)
		refreshDone.Wait()

		id, ok := srv.GitHubAppIdentity(ctx)
		if !ok || id == nil {
			t.Fatal("expected GitHubAppIdentity to lazily fetch from newGH after stale refresh was discarded")
		}
		if id.Slug != "fresh-new-app" {
			t.Fatalf("expected fresh-new-app after config reset, got %q", id.Slug)
		}
		if got := newGH.hits.Load(); got != 1 {
			t.Errorf("expected 1 GET /app call to newGH, got %d", got)
		}
	})
}

func TestGitHubAppIdentity_LazyFetchRetryAndFailureCaching(t *testing.T) {
	t.Run("failure_cached_without_second_get_app", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusBadGateway, `{"message":"bad gateway"}`)

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      42,
			PrivateKey: testGitHubAppPrivateKeyPEM(t),
			APIBaseURL: mockGH.server.URL,
		}
		srv.mu.Unlock()

		ctx := context.Background()
		id, checkedAt, lastErr := srv.ensureGitHubAppIdentity(ctx)
		if id != nil {
			t.Fatalf("expected nil identity on GET /app failure, got %+v", id)
		}
		if checkedAt.IsZero() {
			t.Fatal("expected non-zero checkedAt when GET /app failure is cached")
		}
		if lastErr == "" {
			t.Fatal("expected cached content-free error message on GET /app failure")
		}
		if got := mockGH.hits.Load(); got != 1 {
			t.Fatalf("expected ordinary failure to be cached without retry (1 GET /app), got %d", got)
		}

		// Follow-up lazy lookups read the cached failure rather than issuing a second GET /app.
		if _, ok := srv.GitHubAppIdentity(ctx); ok {
			t.Fatal("expected cached failure to return ok=false")
		}
		if got := mockGH.hits.Load(); got != 1 {
			t.Fatalf("expected no second GET /app after cached failure, got %d", got)
		}
	})

	t.Run("generation_change_mid_flight_retried_once", func(t *testing.T) {
		ctx := context.Background()

		enteredOld := make(chan struct{}, 1)
		releaseOld := make(chan struct{})
		var oldHits atomic.Int64
		oldGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/app" || r.Method != http.MethodGet {
				http.NotFound(w, r)
				return
			}
			oldHits.Add(1)
			select {
			case enteredOld <- struct{}{}:
			default:
			}
			<-releaseOld
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":42,"slug":"stale-old-app","events":["issues"]}`))
		}))
		t.Cleanup(func() {
			select {
			case <-releaseOld:
			default:
				close(releaseOld)
			}
			oldGH.Close()
		})

		newGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":     42,
			"slug":   "retried-after-reset",
			"events": []string{"pull_request"},
		})

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:      42,
			PrivateKey: testGitHubAppPrivateKeyPEM(t),
			APIBaseURL: oldGH.URL,
		}
		srv.mu.Unlock()

		var (
			gotID *githubapp.AppIdentity
			gotOK bool
			wg    sync.WaitGroup
		)
		wg.Add(1)
		go func() {
			defer wg.Done()
			gotID, gotOK = srv.GitHubAppIdentity(ctx)
		}()

		select {
		case <-enteredOld:
		case <-time.After(5 * time.Second):
			t.Fatal("lazy GET /app attempt 0 against oldGH never started")
		}

		// Reset App config (bumps generation) while ensureGitHubAppIdentity's
		// first attempt is still in flight.
		newURL := newGH.server.URL
		rec := doRequest(t, srv, http.MethodPut, "/api/v1/github-app", GitHubAppConfigUpdateRequest{
			APIBaseURL: &newURL,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT /api/v1/github-app failed: %d %s", rec.Code, rec.Body.String())
		}

		// Release attempt 0; its stale result is discarded (checkedAt stays zero)
		// and the same ensureGitHubAppIdentity call retries once against newGH.
		close(releaseOld)
		wg.Wait()

		if !gotOK || gotID == nil {
			t.Fatal("expected in-flight lazy identity fetch to retry once after generation change")
		}
		if gotID.Slug != "retried-after-reset" {
			t.Fatalf("got slug %q, want %q", gotID.Slug, "retried-after-reset")
		}
		if got := oldHits.Load(); got != 1 {
			t.Errorf("expected 1 GET /app to oldGH, got %d", got)
		}
		if got := newGH.hits.Load(); got != 1 {
			t.Errorf("expected 1 retry GET /app to newGH, got %d", got)
		}
	})
}

func TestGitHubAppIdentity_SyncPermissionsAndHealthCheckShareSingleGetApp(t *testing.T) {
	mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
		"id":     42,
		"slug":   "sync-scion",
		"events": []string{"issues", "issue_comment"},
		"permissions": map[string]string{
			"contents": "write",
			"issues":   "read",
			"metadata": "read",
		},
	})

	srv, s := testServer(t)
	srv.mu.Lock()
	srv.config.GitHubAppConfig = GitHubAppServerConfig{
		AppID:      42,
		PrivateKey: testGitHubAppPrivateKeyPEM(t),
		APIBaseURL: mockGH.server.URL,
	}
	srv.mu.Unlock()

	ctx := context.Background()
	instID := int64(5001)
	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}
	if err := s.CreateProject(ctx, &store.Project{
		ID:                   tid("proj-sync-1"),
		Name:                 "Sync Project",
		Slug:                 "sync-project",
		GitRemote:            "https://github.com/acme/widgets.git",
		GitHubInstallationID: &instID,
		GitHubPermissions: &store.GitHubTokenPermissions{
			Contents: "write",
			Issues:   "write",
		},
		GitHubAppStatus: &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK},
		Created:         time.Now(),
		Updated:         time.Now(),
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/github-app/sync-permissions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/github-app/sync-permissions: %d %s", rec.Code, rec.Body.String())
	}
	if got := mockGH.hits.Load(); got != 1 {
		t.Fatalf("expected sync-permissions to issue exactly 1 GET /app (shared with identity cache), got %d", got)
	}

	id, ok := srv.GitHubAppIdentity(ctx)
	if !ok || id.Slug != "sync-scion" {
		t.Fatalf("expected identity cache populated by sync-permissions, got %+v (ok=%v)", id, ok)
	}
	proj, err := s.GetProject(ctx, tid("proj-sync-1"))
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if proj.GitHubAppStatus == nil || proj.GitHubAppStatus.State != store.GitHubAppStateDegraded {
		t.Fatalf("expected under-privileged project to become degraded, got %+v", proj.GitHubAppStatus)
	}

	// Update upstream app slug and trigger the periodic health check handler:
	// it must also share a single GET /app call and refresh the identity cache.
	mockGH.setPayload(t, http.StatusOK, map[string]any{
		"id":     42,
		"slug":   "sync-scion-v2",
		"events": []string{"issues", "issue_comment"},
		"permissions": map[string]string{
			"contents": "write",
			"issues":   "write",
			"metadata": "read",
		},
	})
	srv.githubAppHealthCheckHandler()(ctx)
	if got := mockGH.hits.Load(); got != 2 {
		t.Fatalf("expected health check to add exactly 1 GET /app (total 2), got %d", got)
	}
	id, ok = srv.GitHubAppIdentity(ctx)
	if !ok || id.Slug != "sync-scion-v2" {
		t.Fatalf("expected health check to refresh cached slug to sync-scion-v2, got %+v", id)
	}
}

func TestGitHubAppIdentity_MentionIdentitiesAndSlugWarnRateLimit(t *testing.T) {
	t.Run("fetched_slug_and_normalized_aliases", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":     42,
			"slug":   "scion-app",
			"events": []string{"issue_comment"},
		})

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:                 42,
			PrivateKey:            testGitHubAppPrivateKeyPEM(t),
			APIBaseURL:            mockGH.server.URL,
			CommandMentionAliases: []string{"@scion", "SCION", "  helper-bot  ", ""},
		}
		srv.mu.Unlock()

		ctx := context.Background()
		slug, aliases, ok := srv.GitHubAppMentionIdentities(ctx)
		if !ok {
			t.Fatal("expected GitHubAppMentionIdentities ok=true")
		}
		if slug != "scion-app" {
			t.Errorf("slug = %q, want %q", slug, "scion-app")
		}
		wantAliases := []string{"scion", "helper-bot"}
		if !reflect.DeepEqual(aliases, wantAliases) {
			t.Errorf("aliases = %v, want %v", aliases, wantAliases)
		}
		if bot := srv.GitHubAppBotLogin(ctx); bot != "scion-app[bot]" {
			t.Errorf("GitHubAppBotLogin = %q, want %q", bot, "scion-app[bot]")
		}
	})

	t.Run("app_slug_override_skips_get_app", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusInternalServerError, `{"message":"boom"}`)

		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:                 42,
			APIBaseURL:            mockGH.server.URL,
			AppSlug:               "@custom-scion",
			CommandMentionAliases: []string{"@team-bot"},
		}
		srv.mu.Unlock()

		ctx := context.Background()
		slug, aliases, ok := srv.GitHubAppMentionIdentities(ctx)
		if !ok || slug != "custom-scion" {
			t.Fatalf("expected override slug custom-scion, got slug=%q ok=%v", slug, ok)
		}
		if !reflect.DeepEqual(aliases, []string{"team-bot"}) {
			t.Errorf("aliases = %v, want [team-bot]", aliases)
		}
		if bot := srv.GitHubAppBotLogin(ctx); bot != "custom-scion[bot]" {
			t.Errorf("GitHubAppBotLogin = %q, want custom-scion[bot]", bot)
		}
		if got := mockGH.hits.Load(); got != 0 {
			t.Errorf("expected 0 GET /app calls when app_slug override is set, got %d", got)
		}
	})

	t.Run("unavailable_warns_at_most_once_per_hour", func(t *testing.T) {
		var logBuf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		srv, _ := testServer(t)
		ctx := context.Background()

		for i := 0; i < 3; i++ {
			if slug, _, ok := srv.GitHubAppMentionIdentities(ctx); ok || slug != "" {
				t.Fatalf("expected unavailable slug, got slug=%q ok=%v", slug, ok)
			}
		}

		const warnSubstr = "GitHub App slug unavailable; skipping command mention detection"
		if count := strings.Count(logBuf.String(), warnSubstr); count != 1 {
			t.Errorf("expected exactly 1 rate-limited warning across 3 calls, got %d; log:\n%s", count, logBuf.String())
		}
		if srv.githubAppIdentity.shouldWarnSlugUnavailable(time.Now().UTC().Add(61*time.Minute)) != true {
			t.Error("expected warning limiter to allow another log after 1 hour")
		}
	})
}

func TestHandleGetGitHubApp_TriggersReadiness(t *testing.T) {
	t.Run("omitted_when_experiment_off", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":     42,
			"slug":   "scion-app",
			"events": githubTriggersRequiredEvents,
		})
		srv, _ := testServer(t)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:           42,
			PrivateKey:      testGitHubAppPrivateKeyPEM(t),
			WebhookSecret:   "whsec",
			WebhooksEnabled: true,
			APIBaseURL:      mockGH.server.URL,
		}
		srv.mu.Unlock()

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/github-app: %d %s", rec.Code, rec.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if _, present := raw["triggers"]; present {
			t.Fatalf("expected triggers field omitted when hub.github_triggers is off, got: %s", rec.Body.String())
		}
		if got := mockGH.hits.Load(); got != 0 {
			t.Errorf("expected 0 GET /app calls when hub.github_triggers is off, got %d", got)
		}
	})

	t.Run("reports_missing_events_and_permissions", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":     42,
			"slug":   "scion-app",
			"events": []string{"pull_request", "issue_comment"}, // missing issues, pull_request_review, pull_request_review_comment (push is NOT required)
			"permissions": map[string]string{
				"contents": "write",
				"issues":   "read", // insufficient: write required
				"metadata": "read",
				// pull_requests missing
			},
		})
		srv, _ := testServer(t)
		setGitHubTriggersExperiment(t, srv, true)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:           42,
			PrivateKey:      testGitHubAppPrivateKeyPEM(t),
			WebhookSecret:   "whsec",
			WebhooksEnabled: true,
			APIBaseURL:      mockGH.server.URL,
		}
		srv.mu.Unlock()

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/github-app: %d %s", rec.Code, rec.Body.String())
		}
		var resp GitHubAppConfigResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if resp.Triggers == nil {
			t.Fatal("expected triggers readiness block when hub.github_triggers is on")
		}
		if resp.Triggers.Ready {
			t.Error("expected ready=false when events and permissions are missing")
		}
		if !resp.Triggers.WebhooksEnabled || !resp.Triggers.HasWebhookSecret {
			t.Errorf("expected webhooks_enabled=true and has_webhook_secret=true, got %+v", resp.Triggers)
		}
		if resp.Triggers.AppSlug != "scion-app" {
			t.Errorf("app_slug = %q, want scion-app", resp.Triggers.AppSlug)
		}
		wantMissingEvents := []string{"issues", "pull_request_review", "pull_request_review_comment"}
		if !reflect.DeepEqual(resp.Triggers.MissingEvents, wantMissingEvents) {
			t.Errorf("missing_events = %v, want %v", resp.Triggers.MissingEvents, wantMissingEvents)
		}
		wantMissingPerms := []string{"issues:write", "pull_requests:write"}
		if !reflect.DeepEqual(resp.Triggers.MissingPermissions, wantMissingPerms) {
			t.Errorf("missing_permissions = %v, want %v", resp.Triggers.MissingPermissions, wantMissingPerms)
		}
		if resp.Triggers.CheckedAt == nil || resp.Triggers.CheckedAt.IsZero() {
			t.Errorf("expected non-zero checked_at after fetch, got %v", resp.Triggers.CheckedAt)
		}
		if resp.Triggers.Error != "" {
			t.Errorf("expected empty error, got %q", resp.Triggers.Error)
		}

		var rawResp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &rawResp); err != nil {
			t.Fatalf("unmarshal raw response: %v", err)
		}
		if triggersRaw, ok := rawResp["triggers"].(map[string]any); !ok {
			t.Fatalf("expected triggers JSON object, got %T", rawResp["triggers"])
		} else if _, has := triggersRaw["checked_at"]; !has {
			t.Errorf("expected checked_at in triggers JSON after fetch: %s", rec.Body.String())
		}

		// Project-level readiness omits hub-level webhook config fields.
		projView := srv.GitHubTriggersProjectReadiness(context.Background())
		if projView.CheckedAt == nil || projView.CheckedAt.IsZero() {
			t.Errorf("expected non-zero project readiness checked_at after fetch, got %v", projView.CheckedAt)
		}
		projJSON, err := json.Marshal(projView)
		if err != nil {
			t.Fatalf("marshal project readiness: %v", err)
		}
		var projMap map[string]any
		if err := json.Unmarshal(projJSON, &projMap); err != nil {
			t.Fatalf("unmarshal project readiness: %v", err)
		}
		if _, has := projMap["webhooks_enabled"]; has {
			t.Errorf("project readiness must omit webhooks_enabled: %s", string(projJSON))
		}
		if _, has := projMap["has_webhook_secret"]; has {
			t.Errorf("project readiness must omit has_webhook_secret: %s", string(projJSON))
		}
		if _, has := projMap["checked_at"]; !has {
			t.Errorf("project readiness must include checked_at after fetch: %s", string(projJSON))
		}
	})

	t.Run("ready_when_all_met_and_false_without_webhook_config", func(t *testing.T) {
		mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
			"id":   42,
			"slug": "ready-scion",
			"events": []string{
				"issue_comment",
				"issues",
				"pull_request",
				"pull_request_review",
				"pull_request_review_comment",
			},
			"permissions": map[string]string{
				"contents":      "write",
				"issues":        "write",
				"metadata":      "read",
				"pull_requests": "write",
			},
		})
		srv, _ := testServer(t)
		setGitHubTriggersExperiment(t, srv, true)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:           42,
			PrivateKey:      testGitHubAppPrivateKeyPEM(t),
			WebhookSecret:   "whsec",
			WebhooksEnabled: true,
			APIBaseURL:      mockGH.server.URL,
		}
		srv.mu.Unlock()

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/github-app: %d %s", rec.Code, rec.Body.String())
		}
		var resp GitHubAppConfigResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Triggers == nil || !resp.Triggers.Ready {
			t.Fatalf("expected ready=true, got %+v", resp.Triggers)
		}
		if len(resp.Triggers.MissingEvents) != 0 || len(resp.Triggers.MissingPermissions) != 0 {
			t.Errorf("expected empty missing slices, got events=%v perms=%v",
				resp.Triggers.MissingEvents, resp.Triggers.MissingPermissions)
		}

		// Turning off WebhooksEnabled flips Ready to false even with complete permissions/events.
		srv.mu.Lock()
		srv.config.GitHubAppConfig.WebhooksEnabled = false
		srv.mu.Unlock()
		rec = doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Triggers == nil || resp.Triggers.Ready {
			t.Fatalf("expected ready=false when webhooks_enabled=false, got %+v", resp.Triggers)
		}
	})

	t.Run("content_free_error_when_get_app_fails", func(t *testing.T) {
		const secretLeak = "SECRET_UPSTREAM_BODY_DO_NOT_LEAK"
		mockGH := newMockAppEndpoint(t, http.StatusUnauthorized, `{"message":"Bad credentials `+secretLeak+`"}`)

		srv, _ := testServer(t)
		setGitHubTriggersExperiment(t, srv, true)
		srv.mu.Lock()
		srv.config.GitHubAppConfig = GitHubAppServerConfig{
			AppID:           42,
			PrivateKey:      testGitHubAppPrivateKeyPEM(t),
			WebhookSecret:   "whsec",
			WebhooksEnabled: true,
			APIBaseURL:      mockGH.server.URL,
		}
		srv.mu.Unlock()

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/github-app: %d %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, secretLeak) || strings.Contains(body, "Bad credentials") {
			t.Fatalf("response leaked upstream error text: %s", body)
		}
		var resp GitHubAppConfigResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Triggers == nil {
			t.Fatal("expected triggers block")
		}
		if resp.Triggers.Ready {
			t.Error("expected ready=false when GET /app fails")
		}
		if resp.Triggers.Error == "" {
			t.Error("expected content-free error string when GET /app fails")
		}
	})

	t.Run("unconfigured_app_reports_content_free_error", func(t *testing.T) {
		srv, _ := testServer(t)
		setGitHubTriggersExperiment(t, srv, true)

		rec := doRequest(t, srv, http.MethodGet, "/api/v1/github-app", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/github-app: %d %s", rec.Code, rec.Body.String())
		}
		var resp GitHubAppConfigResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Triggers == nil {
			t.Fatal("expected triggers block when hub.github_triggers is on")
		}
		if resp.Triggers.Ready {
			t.Error("expected ready=false when GitHub App is not configured")
		}
		if resp.Triggers.Error != "GitHub App is not configured." {
			t.Errorf("triggers.error = %q, want %q", resp.Triggers.Error, "GitHub App is not configured.")
		}
		if len(resp.Triggers.MissingEvents) != 0 || len(resp.Triggers.MissingPermissions) != 0 {
			t.Errorf("expected empty missing slices when unconfigured, got events=%v perms=%v",
				resp.Triggers.MissingEvents, resp.Triggers.MissingPermissions)
		}
		if resp.Triggers.CheckedAt != nil {
			t.Errorf("expected nil checked_at before first fetch, got %v", resp.Triggers.CheckedAt)
		}
		if strings.Contains(rec.Body.String(), "0001-01-01") {
			t.Errorf("response must omit checked_at rather than emitting Go zero time: %s", rec.Body.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("unmarshal raw response: %v", err)
		}
		triggersMap, ok := raw["triggers"].(map[string]any)
		if !ok {
			t.Fatalf("expected triggers JSON object, got %T", raw["triggers"])
		}
		if _, has := triggersMap["checked_at"]; has {
			t.Errorf("expected checked_at omitted before first fetch, got %v in %s", triggersMap["checked_at"], rec.Body.String())
		}

		projView := srv.GitHubTriggersProjectReadiness(context.Background())
		if projView.Ready || projView.Error != "GitHub App is not configured." {
			t.Errorf("project readiness = %+v, want ready=false error=%q", projView, "GitHub App is not configured.")
		}
		if projView.CheckedAt != nil {
			t.Errorf("expected nil project readiness checked_at before first fetch, got %v", projView.CheckedAt)
		}
		projJSON, err := json.Marshal(projView)
		if err != nil {
			t.Fatalf("marshal project readiness: %v", err)
		}
		var projMap map[string]any
		if err := json.Unmarshal(projJSON, &projMap); err != nil {
			t.Fatalf("unmarshal project readiness: %v", err)
		}
		if _, has := projMap["checked_at"]; has {
			t.Errorf("expected project readiness checked_at omitted before first fetch: %s", string(projJSON))
		}
	})
}

func TestWebhook_InstallationNewPermissionsAccepted(t *testing.T) {
	mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
		"id":     42,
		"slug":   "scion-app",
		"events": []string{"pull_request"},
		"permissions": map[string]string{
			"contents": "write",
			"issues":   "read",
		},
	})

	srv, s := webhookTestServer(t)
	srv.mu.Lock()
	srv.config.GitHubAppConfig.PrivateKey = testGitHubAppPrivateKeyPEM(t)
	srv.config.GitHubAppConfig.APIBaseURL = mockGH.server.URL
	srv.mu.Unlock()

	ctx := context.Background()
	srv.initGitHubAppIdentity(ctx)
	if id, ok := srv.GitHubAppIdentity(ctx); !ok || id.Permissions["issues"] != "read" {
		t.Fatalf("expected initial issues:read in cache, got %+v", id)
	}

	instID := int64(12345)
	otherInstID := int64(99999)
	for _, id := range []int64{instID, otherInstID} {
		if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: id,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          42,
			Repositories:   []string{"acme/widgets"},
			Status:         store.GitHubInstallationStatusActive,
		}); err != nil {
			t.Fatalf("CreateGitHubInstallation(%d): %v", id, err)
		}
	}

	mintTime := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	projects := []*store.Project{
		{
			ID:                   tid("proj-np-degraded"),
			Name:                 "Degraded Project",
			Slug:                 "degraded-project",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:         store.GitHubAppStateDegraded,
				ErrorCode:     githubapp.ErrCodePermissionDenied,
				ErrorMessage:  "missing issues:write",
				LastTokenMint: &mintTime,
				LastChecked:   mintTime,
			},
			Created: mintTime,
			Updated: mintTime,
		},
		{
			ID:                   tid("proj-np-err-perm"),
			Name:                 "Perm Error Project",
			Slug:                 "perm-error-project",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:        store.GitHubAppStateError,
				ErrorCode:    githubapp.ErrCodePermissionDenied,
				ErrorMessage: "403 forbidden",
				LastChecked:  mintTime,
			},
			Created: mintTime,
			Updated: mintTime,
		},
		{
			ID:                   tid("proj-np-suspended"),
			Name:                 "Suspended Error Project",
			Slug:                 "suspended-error-project",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:        store.GitHubAppStateError,
				ErrorCode:    githubapp.ErrCodeInstallationSuspended,
				ErrorMessage: "suspended",
				LastChecked:  mintTime,
			},
			Created: mintTime,
			Updated: mintTime,
		},
		{
			ID:                   tid("proj-np-other-inst"),
			Name:                 "Other Installation Project",
			Slug:                 "other-inst-project",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &otherInstID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:     store.GitHubAppStateDegraded,
				ErrorCode: githubapp.ErrCodePermissionDenied,
			},
			Created: mintTime,
			Updated: mintTime,
		},
	}
	for _, p := range projects {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.ID, err)
		}
	}

	// Simulate the GitHub App owner adding issues:write and required events,
	// followed by the installation owner accepting the new permissions.
	mockGH.setPayload(t, http.StatusOK, map[string]any{
		"id":     42,
		"slug":   "scion-app",
		"events": githubTriggersRequiredEvents,
		"permissions": map[string]string{
			"contents":      "write",
			"issues":        "write",
			"metadata":      "read",
			"pull_requests": "write",
		},
	})

	payload := mustJSON(t, map[string]any{
		"action": "new_permissions_accepted",
		"installation": map[string]any{
			"id":     instID,
			"app_id": 42,
			"account": map[string]any{
				"login": "acme",
				"type":  "Organization",
			},
		},
	})
	sig := signWebhookPayload(payload, "test-webhook-secret")

	// 1. While hub.github_triggers is off (§11 G12), new_permissions_accepted is a no-op:
	// no GET /app refresh and no project status reset.
	reqOff := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	reqOff.Header.Set("X-GitHub-Event", "installation")
	reqOff.Header.Set("X-GitHub-Delivery", "delivery-new-perms-off")
	reqOff.Header.Set("X-Hub-Signature-256", sig)

	recOff := httptest.NewRecorder()
	srv.handleGitHubWebhook(recOff, reqOff)
	if recOff.Code != http.StatusOK {
		t.Fatalf("webhook (flag off) returned %d: %s", recOff.Code, recOff.Body.String())
	}
	if got := mockGH.hits.Load(); got != 1 {
		t.Fatalf("expected 0 webhook GET /app calls while experiment off (total 1), got %d", got)
	}
	if id, ok := srv.GitHubAppIdentity(ctx); !ok || id.Permissions["issues"] != "read" {
		t.Fatalf("expected cached identity untouched while experiment off, got %+v", id)
	}
	pDegOff, err := s.GetProject(ctx, tid("proj-np-degraded"))
	if err != nil {
		t.Fatalf("GetProject(degraded, off): %v", err)
	}
	if pDegOff.GitHubAppStatus == nil || pDegOff.GitHubAppStatus.State != store.GitHubAppStateDegraded {
		t.Fatalf("expected degraded project untouched while experiment off, got %+v", pDegOff.GitHubAppStatus)
	}

	// 2. Enable hub.github_triggers and redeliver: refreshes identity and resets degraded/permission_denied.
	setGitHubTriggersExperiment(t, srv, true)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-GitHub-Delivery", "delivery-new-perms-1")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", rec.Code, rec.Body.String())
	}

	if got := mockGH.hits.Load(); got != 2 {
		t.Errorf("expected 2 total GET /app calls (1 startup + 1 webhook refresh), got %d", got)
	}
	id, ok := srv.GitHubAppIdentity(ctx)
	if !ok || id.Permissions["issues"] != "write" {
		t.Fatalf("expected refreshed issues:write in cache, got %+v", id)
	}

	// Both degraded and permission_denied projects on installation 12345 reset to unchecked.
	pDeg, err := s.GetProject(ctx, tid("proj-np-degraded"))
	if err != nil {
		t.Fatalf("GetProject(degraded): %v", err)
	}
	if pDeg.GitHubAppStatus == nil || pDeg.GitHubAppStatus.State != store.GitHubAppStateUnchecked {
		t.Errorf("expected degraded project reset to unchecked, got %+v", pDeg.GitHubAppStatus)
	}
	if pDeg.GitHubAppStatus.LastTokenMint == nil || !pDeg.GitHubAppStatus.LastTokenMint.Equal(mintTime) {
		t.Errorf("expected LastTokenMint preserved on reset, got %+v", pDeg.GitHubAppStatus.LastTokenMint)
	}

	pPerm, err := s.GetProject(ctx, tid("proj-np-err-perm"))
	if err != nil {
		t.Fatalf("GetProject(err-perm): %v", err)
	}
	if pPerm.GitHubAppStatus == nil || pPerm.GitHubAppStatus.State != store.GitHubAppStateUnchecked {
		t.Errorf("expected permission_denied error project reset to unchecked, got %+v", pPerm.GitHubAppStatus)
	}

	// Non-permission errors and other installations remain untouched.
	pSusp, err := s.GetProject(ctx, tid("proj-np-suspended"))
	if err != nil {
		t.Fatalf("GetProject(suspended): %v", err)
	}
	if pSusp.GitHubAppStatus == nil || pSusp.GitHubAppStatus.State != store.GitHubAppStateError {
		t.Errorf("expected installation_suspended project untouched, got %+v", pSusp.GitHubAppStatus)
	}

	pOther, err := s.GetProject(ctx, tid("proj-np-other-inst"))
	if err != nil {
		t.Fatalf("GetProject(other-inst): %v", err)
	}
	if pOther.GitHubAppStatus == nil || pOther.GitHubAppStatus.State != store.GitHubAppStateDegraded {
		t.Errorf("expected project on other installation untouched, got %+v", pOther.GitHubAppStatus)
	}
}

func TestWebhook_InstallationNewPermissionsAccepted_PaginatesAllProjects(t *testing.T) {
	prevPageSize := resetPermissionDeniedProjectsPageSize
	resetPermissionDeniedProjectsPageSize = 2
	t.Cleanup(func() { resetPermissionDeniedProjectsPageSize = prevPageSize })

	mockGH := newMockAppEndpoint(t, http.StatusOK, map[string]any{
		"id":     42,
		"slug":   "scion-app",
		"events": githubTriggersRequiredEvents,
		"permissions": map[string]string{
			"contents":      "write",
			"issues":        "write",
			"metadata":      "read",
			"pull_requests": "write",
		},
	})

	srv, s := webhookTestServer(t)
	setGitHubTriggersExperiment(t, srv, true)
	srv.mu.Lock()
	srv.config.GitHubAppConfig.PrivateKey = testGitHubAppPrivateKeyPEM(t)
	srv.config.GitHubAppConfig.APIBaseURL = mockGH.server.URL
	srv.mu.Unlock()

	ctx := context.Background()
	instID := int64(22345)
	otherInstID := int64(88888)
	for _, id := range []int64{instID, otherInstID} {
		if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
			InstallationID: id,
			AccountLogin:   "acme",
			AccountType:    "Organization",
			AppID:          42,
			Repositories:   []string{"acme/widgets"},
			Status:         store.GitHubInstallationStatusActive,
		}); err != nil {
			t.Fatalf("CreateGitHubInstallation(%d): %v", id, err)
		}
	}

	baseTime := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	mintTime := baseTime.Add(-time.Hour)

	// ListProjects orders by (Created DESC, ID DESC). Create 6 projects so that
	// with pageSize=2 they span 3 pages and the target degraded / permission_denied
	// projects land on page 3 (the last page).
	fixtures := []*store.Project{
		{
			ID:                   tid("proj-np-page3-degraded"),
			Name:                 "Page 3 Degraded",
			Slug:                 "page3-degraded",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:         store.GitHubAppStateDegraded,
				ErrorCode:     githubapp.ErrCodePermissionDenied,
				ErrorMessage:  "missing issues:write",
				LastTokenMint: &mintTime,
				LastChecked:   mintTime,
			},
			Created: baseTime,
			Updated: baseTime,
		},
		{
			ID:                   tid("proj-np-page3-perm-err"),
			Name:                 "Page 3 Perm Error",
			Slug:                 "page3-perm-err",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:        store.GitHubAppStateError,
				ErrorCode:    githubapp.ErrCodePermissionDenied,
				ErrorMessage: "403 forbidden",
				LastChecked:  mintTime,
			},
			Created: baseTime.Add(1 * time.Minute),
			Updated: baseTime.Add(1 * time.Minute),
		},
		{
			ID:                   tid("proj-np-page2-suspended"),
			Name:                 "Page 2 Suspended",
			Slug:                 "page2-suspended",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:        store.GitHubAppStateError,
				ErrorCode:    githubapp.ErrCodeInstallationSuspended,
				ErrorMessage: "suspended",
				LastChecked:  mintTime,
			},
			Created: baseTime.Add(2 * time.Minute),
			Updated: baseTime.Add(2 * time.Minute),
		},
		{
			ID:                   tid("proj-np-page2-other-inst"),
			Name:                 "Page 2 Other Inst",
			Slug:                 "page2-other-inst",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &otherInstID,
			GitHubAppStatus: &store.GitHubAppProjectStatus{
				State:     store.GitHubAppStateDegraded,
				ErrorCode: githubapp.ErrCodePermissionDenied,
			},
			Created: baseTime.Add(3 * time.Minute),
			Updated: baseTime.Add(3 * time.Minute),
		},
		{
			ID:                   tid("proj-np-page1-ok-a"),
			Name:                 "Page 1 OK A",
			Slug:                 "page1-ok-a",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: mintTime},
			Created:              baseTime.Add(4 * time.Minute),
			Updated:              baseTime.Add(4 * time.Minute),
		},
		{
			ID:                   tid("proj-np-page1-ok-b"),
			Name:                 "Page 1 OK B",
			Slug:                 "page1-ok-b",
			GitRemote:            "https://github.com/acme/widgets.git",
			GitHubInstallationID: &instID,
			GitHubAppStatus:      &store.GitHubAppProjectStatus{State: store.GitHubAppStateOK, LastChecked: mintTime},
			Created:              baseTime.Add(5 * time.Minute),
			Updated:              baseTime.Add(5 * time.Minute),
		},
	}
	for _, p := range fixtures {
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("CreateProject(%s): %v", p.ID, err)
		}
	}

	firstPage, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 2, SkipTotalCount: true})
	if err != nil {
		t.Fatalf("ListProjects(first page): %v", err)
	}
	if firstPage.NextCursor == "" {
		t.Fatal("expected NextCursor on first page with Limit=2 and 6 projects")
	}
	for _, item := range firstPage.Items {
		if item.ID == tid("proj-np-page3-degraded") || item.ID == tid("proj-np-page3-perm-err") {
			t.Fatalf("target project %s unexpectedly landed on page 1", item.ID)
		}
	}

	payload := mustJSON(t, map[string]any{
		"action": "new_permissions_accepted",
		"installation": map[string]any{
			"id":     instID,
			"app_id": 42,
			"account": map[string]any{
				"login": "acme",
				"type":  "Organization",
			},
		},
	})
	sig := signWebhookPayload(payload, "test-webhook-secret")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", bytes.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "installation")
	req.Header.Set("X-GitHub-Delivery", "delivery-new-perms-paged")
	req.Header.Set("X-Hub-Signature-256", sig)

	rec := httptest.NewRecorder()
	srv.handleGitHubWebhook(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook returned %d: %s", rec.Code, rec.Body.String())
	}

	pDeg, err := s.GetProject(ctx, tid("proj-np-page3-degraded"))
	if err != nil {
		t.Fatalf("GetProject(page3-degraded): %v", err)
	}
	if pDeg.GitHubAppStatus == nil || pDeg.GitHubAppStatus.State != store.GitHubAppStateUnchecked {
		t.Errorf("expected page-3 degraded project reset to unchecked, got %+v", pDeg.GitHubAppStatus)
	}
	if pDeg.GitHubAppStatus != nil && (pDeg.GitHubAppStatus.LastTokenMint == nil || !pDeg.GitHubAppStatus.LastTokenMint.Equal(mintTime)) {
		t.Errorf("expected LastTokenMint preserved on page-3 reset, got %+v", pDeg.GitHubAppStatus.LastTokenMint)
	}

	pPerm, err := s.GetProject(ctx, tid("proj-np-page3-perm-err"))
	if err != nil {
		t.Fatalf("GetProject(page3-perm-err): %v", err)
	}
	if pPerm.GitHubAppStatus == nil || pPerm.GitHubAppStatus.State != store.GitHubAppStateUnchecked {
		t.Errorf("expected page-3 permission_denied project reset to unchecked, got %+v", pPerm.GitHubAppStatus)
	}

	pSusp, err := s.GetProject(ctx, tid("proj-np-page2-suspended"))
	if err != nil {
		t.Fatalf("GetProject(page2-suspended): %v", err)
	}
	if pSusp.GitHubAppStatus == nil || pSusp.GitHubAppStatus.State != store.GitHubAppStateError {
		t.Errorf("expected page-2 suspended project untouched, got %+v", pSusp.GitHubAppStatus)
	}

	pOther, err := s.GetProject(ctx, tid("proj-np-page2-other-inst"))
	if err != nil {
		t.Fatalf("GetProject(page2-other-inst): %v", err)
	}
	if pOther.GitHubAppStatus == nil || pOther.GitHubAppStatus.State != store.GitHubAppStateDegraded {
		t.Errorf("expected page-2 other-installation project untouched, got %+v", pOther.GitHubAppStatus)
	}

	for _, okID := range []string{"proj-np-page1-ok-a", "proj-np-page1-ok-b"} {
		pOK, err := s.GetProject(ctx, tid(okID))
		if err != nil {
			t.Fatalf("GetProject(%s): %v", okID, err)
		}
		if pOK.GitHubAppStatus == nil || pOK.GitHubAppStatus.State != store.GitHubAppStateOK {
			t.Errorf("expected %s untouched in ok state, got %+v", okID, pOK.GitHubAppStatus)
		}
	}
}
