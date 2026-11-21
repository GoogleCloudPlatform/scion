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
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var (
	discoverTestKeyOnce sync.Once
	discoverTestKeyPEM  string
)

func discoverTestPrivateKeyPEM(t *testing.T) string {
	t.Helper()
	discoverTestKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		block := &pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(key),
		}
		discoverTestKeyPEM = string(pem.EncodeToMemory(block))
	})
	return discoverTestKeyPEM
}

func discoverTestServer(t *testing.T, ghAPIURL string) (*Server, store.Store) {
	t.Helper()
	srv, s := webhookTestServer(t)
	srv.mu.Lock()
	srv.config.GitHubAppConfig.AppID = 42
	srv.config.GitHubAppConfig.PrivateKey = discoverTestPrivateKeyPEM(t)
	srv.config.GitHubAppConfig.APIBaseURL = ghAPIURL
	srv.mu.Unlock()
	return srv, s
}

func TestHandleGitHubAppDiscover_RepoListingFailurePreservesStoredRepos(t *testing.T) {
	const (
		secretToken    = "ghs_SUPER_SECRET_INSTALL_TOKEN_8613"
		upstreamSecret = "UPSTREAM_INTERNAL_ERROR_BODY_8613"
	)

	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":12345,"app_id":42,"account":{"login":"acme","type":"Organization"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/12345/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":{"metadata":"read"}}`,
				secretToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"message":%q}`, upstreamSecret)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGH.Close()

	srv, s := discoverTestServer(t, fakeGH.URL)
	ctx := context.Background()

	wantRepos := []string{"acme/widgets", "acme/api"}
	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   slices.Clone(wantRepos),
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	rec := httptest.NewRecorder()
	srv.handleGitHubAppDiscover(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d on repo listing failure, got %d: %s", http.StatusBadGateway, rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, upstreamSecret) {
		t.Errorf("response leaked upstream error body: %s", body)
	}
	if strings.Contains(body, secretToken) {
		t.Errorf("response leaked installation token: %s", body)
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to decode error response: %v (body: %s)", err, body)
	}
	if errResp.Error.Message == "" {
		t.Errorf("expected non-empty error message in response: %s", body)
	}
	if failed, _ := errResp.Error.Details["failed"].(float64); int(failed) != 1 {
		t.Errorf("expected error.details.failed == 1, got %v", errResp.Error.Details["failed"])
	}
	insts, _ := errResp.Error.Details["installations"].([]interface{})
	if len(insts) != 1 {
		t.Fatalf("expected 1 installation in error.details.installations, got %v", errResp.Error.Details["installations"])
	}
	instMap, _ := insts[0].(map[string]interface{})
	if instErr, _ := instMap["error"].(string); instErr == "" {
		t.Errorf("expected per-installation error message, got %v", instMap)
	}

	stored, err := s.GetGitHubInstallation(ctx, 12345)
	if err != nil {
		t.Fatalf("GetGitHubInstallation: %v", err)
	}
	if !slices.Equal(stored.Repositories, wantRepos) {
		t.Fatalf("stored repositories were modified on listing failure: got %v, want %v", stored.Repositories, wantRepos)
	}
}

func TestHandleGitHubAppDiscover_PartialMultiPageFailurePreservesStoredRepos(t *testing.T) {
	const (
		secretToken    = "ghs_MULTIPAGE_TOKEN_SECRET_8613"
		upstreamSecret = "PAGE2_UPSTREAM_FAILURE_DETAIL_8613"
	)

	// Build a compact 100-repo page 1 payload (< 4 KiB) so page 1 succeeds and
	// ListInstallationRepos advances to page 2.
	var page1Repos []string
	for i := 0; i < 100; i++ {
		page1Repos = append(page1Repos, fmt.Sprintf(`{"full_name":"a/%d"}`, i))
	}
	page1Body := `{"repositories":[` + strings.Join(page1Repos, ",") + `]}`

	var pagesRequested []string
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":12345,"app_id":42,"account":{"login":"acme","type":"Organization"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/12345/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":{"metadata":"read"}}`,
				secretToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			page := r.URL.Query().Get("page")
			pagesRequested = append(pagesRequested, page)
			w.Header().Set("Content-Type", "application/json")
			if page == "1" {
				_, _ = w.Write([]byte(page1Body))
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprintf(w, `{"message":%q}`, upstreamSecret)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGH.Close()

	srv, s := discoverTestServer(t, fakeGH.URL)
	ctx := context.Background()

	wantRepos := []string{"acme/existing-a", "acme/existing-b"}
	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   slices.Clone(wantRepos),
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	rec := httptest.NewRecorder()
	srv.handleGitHubAppDiscover(rec, req)

	if !slices.Equal(pagesRequested, []string{"1", "2"}) {
		t.Fatalf("expected pages [1 2] to be requested, got %v", pagesRequested)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d on page-2 listing failure, got %d: %s", http.StatusBadGateway, rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, upstreamSecret) {
		t.Errorf("response leaked upstream page-2 error body: %s", body)
	}
	if strings.Contains(body, secretToken) {
		t.Errorf("response leaked installation token: %s", body)
	}

	stored, err := s.GetGitHubInstallation(ctx, 12345)
	if err != nil {
		t.Fatalf("GetGitHubInstallation: %v", err)
	}
	if !slices.Equal(stored.Repositories, wantRepos) {
		t.Fatalf("stored repositories changed on page-2 failure: got %v, want %v", stored.Repositories, wantRepos)
	}
}

func TestHandleGitHubAppDiscover_PartialMultiInstallationFailure(t *testing.T) {
	const (
		okToken        = "ghs_OK_INST_TOKEN_8613"
		failToken      = "ghs_FAIL_INST_TOKEN_8613"
		upstreamSecret = "BETA_LISTING_UPSTREAM_SECRET_8613"
	)

	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{"id":1001,"app_id":42,"account":{"login":"acme","type":"Organization"}},
				{"id":1002,"app_id":42,"account":{"login":"beta","type":"Organization"}}
			]`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/1001/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":{"metadata":"read"}}`,
				okToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/1002/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":%q,"permissions":{"metadata":"read"}}`,
				failToken, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("Authorization") == "Bearer "+okToken {
				_, _ = w.Write([]byte(`{"repositories":[{"id":1,"name":"new-1","full_name":"acme/new-1"},{"id":2,"name":"new-2","full_name":"acme/new-2"}]}`))
				return
			}
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, `{"message":%q}`, upstreamSecret)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGH.Close()

	srv, s := discoverTestServer(t, fakeGH.URL)
	ctx := context.Background()

	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 1001,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/old"},
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation 1001: %v", err)
	}
	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 1002,
		AccountLogin:   "beta",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"beta/keep-me"},
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation 1002: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	rec := httptest.NewRecorder()
	srv.handleGitHubAppDiscover(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected status %d on partial failure, got %d: %s", http.StatusBadGateway, rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, upstreamSecret) || strings.Contains(body, okToken) || strings.Contains(body, failToken) {
		t.Errorf("response leaked upstream detail or token: %s", body)
	}

	inst1, err := s.GetGitHubInstallation(ctx, 1001)
	if err != nil {
		t.Fatalf("GetGitHubInstallation 1001: %v", err)
	}
	if !slices.Equal(inst1.Repositories, []string{"acme/new-1", "acme/new-2"}) {
		t.Errorf("installation 1001 repos = %v, want [acme/new-1 acme/new-2]", inst1.Repositories)
	}

	inst2, err := s.GetGitHubInstallation(ctx, 1002)
	if err != nil {
		t.Fatalf("GetGitHubInstallation 1002: %v", err)
	}
	if !slices.Equal(inst2.Repositories, []string{"beta/keep-me"}) {
		t.Errorf("installation 1002 repos = %v, want [beta/keep-me]", inst2.Repositories)
	}
}

func TestHandleGitHubAppDiscover_SuccessReplacesStoredRepos(t *testing.T) {
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":12345,"app_id":42,"account":{"login":"acme","type":"Organization"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/12345/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"ghs_ok","expires_at":%q,"permissions":{"metadata":"read"}}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"repositories":[{"id":1,"name":"fresh-1","full_name":"acme/fresh-1"},{"id":2,"name":"fresh-2","full_name":"acme/fresh-2"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGH.Close()

	srv, s := discoverTestServer(t, fakeGH.URL)
	ctx := context.Background()

	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/old"},
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	rec := httptest.NewRecorder()
	srv.handleGitHubAppDiscover(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on success, got %d: %s", rec.Code, rec.Body.String())
	}

	stored, err := s.GetGitHubInstallation(ctx, 12345)
	if err != nil {
		t.Fatalf("GetGitHubInstallation: %v", err)
	}
	wantRepos := []string{"acme/fresh-1", "acme/fresh-2"}
	if !slices.Equal(stored.Repositories, wantRepos) {
		t.Fatalf("stored repositories = %v, want %v", stored.Repositories, wantRepos)
	}
}

func TestHandleGitHubAppDiscover_ListInstallationsFailureIsNonLeaky(t *testing.T) {
	const upstreamSecret = "UPSTREAM_INSTALLATIONS_LIST_SECRET_8613"

	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"message":%q}`, upstreamSecret)
	}))
	defer fakeGH.Close()

	srv, _ := discoverTestServer(t, fakeGH.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	rec := httptest.NewRecorder()
	srv.handleGitHubAppDiscover(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, upstreamSecret) || strings.Contains(body, fakeGH.URL) {
		t.Errorf("response leaked upstream detail or URL: %s", body)
	}
}

func TestHandleGitHubAppDiscover_AuthorizationUnchanged(t *testing.T) {
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGH.Close()

	srv, s := discoverTestServer(t, fakeGH.URL)
	ctx := context.Background()

	adminUser := &store.User{
		ID: tid("admin-discover-1"), Email: "admin-discover@example.com", DisplayName: "Admin",
		Role: "admin", Status: "active",
	}
	memberUser := &store.User{
		ID: tid("member-discover-1"), Email: "member-discover@example.com", DisplayName: "Member",
		Role: "member", Status: "active",
	}
	if err := s.CreateUser(ctx, adminUser); err != nil {
		t.Fatalf("CreateUser(admin): %v", err)
	}
	if err := s.CreateUser(ctx, memberUser); err != nil {
		t.Fatalf("CreateUser(member): %v", err)
	}
	superAdminRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		t.Fatalf("GetRoleDefinitionByName(super-admin): %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: superAdminRD.ID,
		PrincipalType:    "user",
		PrincipalID:      adminUser.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}

	// Unauthenticated -> 401
	unauthReq := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	unauthRec := httptest.NewRecorder()
	srv.mux.ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST discover: got %d, want 401", unauthRec.Code)
	}

	// Non-admin member -> 403
	member := NewAuthenticatedUser(memberUser.ID, memberUser.Email, memberUser.DisplayName, "member", "api")
	memberReq := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	memberReq = memberReq.WithContext(contextWithCredentialContext(contextWithIdentity(ctx, member), CredentialContext{Kind: CredentialKindInteractive}))
	memberRec := httptest.NewRecorder()
	srv.mux.ServeHTTP(memberRec, memberReq)
	if memberRec.Code != http.StatusForbidden {
		t.Errorf("member POST discover: got %d, want 403", memberRec.Code)
	}

	// Admin -> 200
	admin := NewAuthenticatedUser(adminUser.ID, adminUser.Email, adminUser.DisplayName, "admin", "api")
	adminReq := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	adminReq = adminReq.WithContext(contextWithCredentialContext(contextWithIdentity(ctx, admin), CredentialContext{Kind: CredentialKindInteractive}))
	adminRec := httptest.NewRecorder()
	srv.mux.ServeHTTP(adminRec, adminReq)
	if adminRec.Code != http.StatusOK {
		t.Errorf("admin POST discover: got %d, want 200: %s", adminRec.Code, adminRec.Body.String())
	}
}

func TestHandleGitHubAppDiscover_OtherCallersAfterFailure(t *testing.T) {
	fakeGH := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":12345,"app_id":42,"account":{"login":"acme","type":"Organization"}}]`))
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/12345/access_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprintf(w, `{"token":"ghs_minted_ok","expires_at":%q,"permissions":{"contents":"write","metadata":"read"}}`,
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer fakeGH.Close()

	srv, s := discoverTestServer(t, fakeGH.URL)
	ctx := context.Background()

	// Seed installation with known repo.
	if err := s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          42,
		Repositories:   []string{"acme/widgets"},
		Status:         store.GitHubInstallationStatusActive,
	}); err != nil {
		t.Fatalf("CreateGitHubInstallation: %v", err)
	}

	// Trigger failing discovery — stored repo list must be preserved.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/github-app/installations/discover", nil)
	rec := httptest.NewRecorder()
	srv.handleGitHubAppDiscover(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on failing discovery, got %d", rec.Code)
	}

	// Auto-association on a new project still sees the preserved repo list, and
	// still enforces creator authorization.
	giveUserGitHubToken(t, srv, s, "creator-ok", "ghp_creator_ok")
	fakeCreatorAccess(srv, map[string]githubapp.RepoAccess{
		"ghp_creator_ok|acme/widgets": {Push: true, Pull: true},
	})
	remoteURL := "https://" + srv.githubAppWebHost() + "/acme/widgets.git"
	authzProj := &store.Project{
		ID:        tid("proj-authz"),
		Name:      "Authz Project",
		Slug:      tid("proj-authz"),
		CreatedBy: "creator-ok",
		GitRemote: remoteURL,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, authzProj); err != nil {
		t.Fatalf("CreateProject authz: %v", err)
	}
	srv.autoAssociateGitHubInstallation(ctx, authzProj)
	gotAuthz, err := s.GetProject(ctx, authzProj.ID)
	if err != nil {
		t.Fatalf("GetProject authz: %v", err)
	}
	if gotAuthz.GitHubInstallationID == nil || *gotAuthz.GitHubInstallationID != 12345 {
		t.Fatalf("expected authorized project to auto-associate via preserved repo list, got %v", gotAuthz.GitHubInstallationID)
	}

	// An unauthorized creator is still refused even with the preserved repo list.
	unauthzProj := &store.Project{
		ID:        tid("proj-unauthz"),
		Name:      "Unauthz Project",
		Slug:      tid("proj-unauthz"),
		CreatedBy: "creator-no-access",
		GitRemote: remoteURL,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	if err := s.CreateProject(ctx, unauthzProj); err != nil {
		t.Fatalf("CreateProject unauthz: %v", err)
	}
	srv.autoAssociateGitHubInstallation(ctx, unauthzProj)
	gotUnauthz, err := s.GetProject(ctx, unauthzProj.ID)
	if err != nil {
		t.Fatalf("GetProject unauthz: %v", err)
	}
	if gotUnauthz.GitHubInstallationID != nil {
		t.Fatalf("expected unauthorized project NOT to auto-associate, got %v", *gotUnauthz.GitHubInstallationID)
	}

	// Token mint on the associated project continues to work normally.
	token, _, err := srv.mintGitHubAppToken(ctx, gotAuthz)
	if err != nil {
		t.Fatalf("mintGitHubAppToken after discovery failure: %v", err)
	}
	if token != "ghs_minted_ok" {
		t.Fatalf("minted token = %q, want ghs_minted_ok", token)
	}
}
