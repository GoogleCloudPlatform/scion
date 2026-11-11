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
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const scopeCredSourceURL = "https://github.com/acme-corp/private.templates/tree/main/.scion/templates"

// scopeCredCascadeCases are the GH_ convention cascade cases shared by the
// hub-scope and user-scope tests: the repo-specific key beats the owner-level
// key, which beats GITHUB_TOKEN; with none set the fetch is unauthenticated.
var scopeCredCascadeCases = []struct {
	name    string
	values  map[string]string
	want    string
	wantLog string
}{
	{
		name:    "repo key beats owner and default",
		values:  map[string]string{"GH_ACME_CORP__PRIVATE_TEMPLATES": "fake-repo-cred", "GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"},
		want:    "fake-repo-cred",
		wantLog: "GH_ACME_CORP__PRIVATE_TEMPLATES",
	},
	{
		name:    "owner key beats default",
		values:  map[string]string{"GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"},
		want:    "fake-owner-cred",
		wantLog: "GH_ACME_CORP",
	},
	{
		name:    "default when no convention key",
		values:  map[string]string{"GITHUB_TOKEN": "fake-default-cred"},
		want:    "fake-default-cred",
		wantLog: "GITHUB_TOKEN",
	},
	{
		name:   "keys for another owner are ignored",
		values: map[string]string{"GH_OTHER_ORG": "fake-other-cred"},
		want:   "",
	},
	{
		name:   "unauthenticated when nothing is set",
		values: map[string]string{},
		want:   "",
	},
}

// TestImportCredential_HubScopeCascade verifies that a global-scope import
// reads the GH_ convention names, then GITHUB_TOKEN, from hub credentials.
func TestImportCredential_HubScopeCascade(t *testing.T) {
	for _, tt := range scopeCredCascadeCases {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, backend, logBuf := newImportCredServer(t, "", tt.values)
			backend.scope = secret.ScopeHub
			backend.projectID = srv.HubID()
			got := srv.importCredential(context.Background(), store.TemplateScopeGlobal, "", scopeCredSourceURL)
			if got != tt.want {
				t.Errorf("importCredential = %q, want %q", got, tt.want)
			}
			if tt.wantLog != "" && !strings.Contains(logBuf.String(), "secret="+tt.wantLog) {
				t.Errorf("resource log does not name credential %s: %s", tt.wantLog, logBuf.String())
			}
			assertNoCredentialInLog(t, logBuf.String(), tt.values)
		})
	}
}

// TestImportCredential_UserScopeCascade verifies that a user-scope import by
// that user reads the GH_ convention names, then GITHUB_TOKEN, from the
// user's own credentials.
func TestImportCredential_UserScopeCascade(t *testing.T) {
	const userID = "user-import-cred"
	for _, tt := range scopeCredCascadeCases {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, backend, logBuf := newImportCredServer(t, userID, tt.values)
			backend.scope = secret.ScopeUser
			ctx := contextWithIdentity(context.Background(), NewAuthenticatedUser(userID, "u@test.com", "U", "member", "web"))
			got := srv.importCredential(ctx, store.TemplateScopeUser, userID, scopeCredSourceURL)
			if got != tt.want {
				t.Errorf("importCredential = %q, want %q", got, tt.want)
			}
			if tt.wantLog != "" && !strings.Contains(logBuf.String(), "secret="+tt.wantLog) {
				t.Errorf("resource log does not name credential %s: %s", tt.wantLog, logBuf.String())
			}
			assertNoCredentialInLog(t, logBuf.String(), tt.values)
		})
	}
}

// TestImportCredential_UserScopeOnlyForThatUser verifies that a user-scope
// import never spends a user's credential unless the caller is that user.
func TestImportCredential_UserScopeOnlyForThatUser(t *testing.T) {
	const userID = "user-import-cred-owner"
	values := map[string]string{"GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"}
	for name, ctx := range map[string]context.Context{
		"no caller":      context.Background(),
		"another caller": contextWithIdentity(context.Background(), NewAuthenticatedUser("user-someone-else", "e@test.com", "E", "member", "web")),
	} {
		t.Run(name, func(t *testing.T) {
			srv, _, backend, _ := newImportCredServer(t, userID, values)
			backend.scope = secret.ScopeUser
			if got := srv.importCredential(ctx, store.TemplateScopeUser, userID, scopeCredSourceURL); got != "" {
				t.Errorf("importCredential = %q, want unauthenticated", got)
			}
			if backend.gets != 0 {
				t.Errorf("credential backend Get called %d times, want 0", backend.gets)
			}
		})
	}
}

// TestImportCredential_ScopesReadOnlyTheirOwnCredentials verifies that each
// scope asks the backend only for its own credential scope: hub scope never
// reads project or user credentials, and user scope never reads hub ones.
func TestImportCredential_ScopesReadOnlyTheirOwnCredentials(t *testing.T) {
	const userID = "user-import-cred-iso"
	userCtx := contextWithIdentity(context.Background(), NewAuthenticatedUser(userID, "i@test.com", "I", "member", "web"))
	tests := []struct {
		name      string
		ctx       context.Context
		scope     string
		scopeID   string
		wantScope string
	}{
		{"global reads hub", context.Background(), store.TemplateScopeGlobal, "", secret.ScopeHub},
		{"user reads user", userCtx, store.TemplateScopeUser, userID, secret.ScopeUser},
		{"project reads project", context.Background(), store.TemplateScopeProject, "project-import-cred-iso", secret.ScopeProject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := testServer(t)
			rec := &scopeRecordingBackend{}
			srv.SetSecretBackend(rec)
			_ = srv.importCredential(tt.ctx, tt.scope, tt.scopeID, scopeCredSourceURL)
			if len(rec.scopes) == 0 {
				t.Fatalf("no credential lookups, want lookups in %s scope", tt.wantScope)
			}
			for _, got := range rec.scopes {
				if got != tt.wantScope {
					t.Errorf("credential lookup in %s scope, want only %s", got, tt.wantScope)
				}
			}
		})
	}
}

// scopeRecordingBackend records the credential scope of every Get and finds
// nothing. The embedded nil interface panics on any other call.
type scopeRecordingBackend struct {
	secret.SecretBackend
	scopes []string
}

func (b *scopeRecordingBackend) Get(_ context.Context, _, scope, _ string) (*secret.SecretWithValue, error) {
	b.scopes = append(b.scopes, scope)
	return nil, store.ErrNotFound
}

// TestImportCredential_HubAndUserScopeNonGitHubReadNothing verifies that
// sources which are not GitHub folders read no hub or user credential.
func TestImportCredential_HubAndUserScopeNonGitHubReadNothing(t *testing.T) {
	const userID = "user-import-cred-nongh"
	userCtx := contextWithIdentity(context.Background(), NewAuthenticatedUser(userID, "n@test.com", "N", "member", "web"))
	values := map[string]string{"GH_ACME_CORP__TEMPLATES": "fake-repo-cred", "GH_ACME_CORP": "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"}
	for _, sourceURL := range []string{
		"https://github.com/acme-corp/templates/archive/main.tar.gz",
		"https://example.com/acme-corp/templates.tgz",
		":gcs:bucket/acme-corp/templates",
	} {
		t.Run(sourceURL, func(t *testing.T) {
			srv, _, backend, _ := newImportCredServer(t, userID, values)
			backend.scope = secret.ScopeUser
			if got := srv.importCredential(userCtx, store.TemplateScopeUser, userID, sourceURL); got != "" {
				t.Errorf("user scope: importCredential = %q, want no credential", got)
			}
			if got := srv.importCredential(context.Background(), store.TemplateScopeGlobal, "", sourceURL); got != "" {
				t.Errorf("global scope: importCredential = %q, want no credential", got)
			}
			if backend.gets != 0 {
				t.Errorf("credential backend Get called %d times, want 0", backend.gets)
			}
		})
	}
}

// TestResourcesEndpoints_ScopeCredentialOnTheWire verifies end to end that
// the unified import and discover endpoints send the hub credential for
// global scope and the caller's own credential for user scope, each stored
// through the real local credential backend, as the Bearer credential, and that
// a hub credential is not sent for a user-scope import.
func TestResourcesEndpoints_ScopeCredentialOnTheWire(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		scope     string
		admin     bool
		credScope string // where the credential is stored
		wantAuth  string
	}{
		{"global import uses hub credential", "/api/v1/resources/import", "global", true, secret.ScopeHub, "Bearer fake-scope-cred"},
		{"global discover uses hub credential", "/api/v1/resources/discover", "global", true, secret.ScopeHub, "Bearer fake-scope-cred"},
		{"user import uses user credential", "/api/v1/resources/import", "user", false, secret.ScopeUser, "Bearer fake-scope-cred"},
		{"user discover uses user credential", "/api/v1/resources/discover", "user", false, secret.ScopeUser, "Bearer fake-scope-cred"},
		{"user import ignores hub credential", "/api/v1/resources/import", "user", false, secret.ScopeHub, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, s, _ := testTemplateBootstrapServer(t)
			ctx := context.Background()

			var user *store.User
			if tt.admin {
				user = skillDiscoverAdmin(t, s, "scope-cred-admin")
			} else {
				user = skillDiscoverMember(t, s, "scope-cred-member")
			}

			srv.SetSecretBackend(secret.NewLocalBackend(s, srv.HubID(), "test-secret"))
			scopeID := srv.HubID()
			if tt.credScope == secret.ScopeUser {
				scopeID = user.ID
			}
			if _, _, err := srv.GetSecretBackend().Set(ctx, &secret.SetSecretInput{
				Name: "GH_ACME_CORP", Value: "fake-scope-cred", SecretType: secret.TypeEnvironment,
				Scope: tt.credScope, ScopeID: scopeID,
			}); err != nil {
				t.Fatalf("failed to store credential: %v", err)
			}

			var capturedAuth string
			defer mockSkillTarballWithHook(t, map[string]string{
				"repo-main/templates/my-template/scion-agent.yaml": "harness: claude\n",
			}, func(req *http.Request) {
				capturedAuth = req.Header.Get("Authorization")
			})()

			rec := doRequestAsUser(t, srv, user, http.MethodPost, tt.path, map[string]string{
				"kind":      "template",
				"scope":     tt.scope,
				"sourceUrl": "https://github.com/acme-corp/templates/tree/main/templates",
			})
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			if capturedAuth != tt.wantAuth {
				t.Errorf("Authorization header = %q, want %q", capturedAuth, tt.wantAuth)
			}
		})
	}
}

// TestHarnessConfigReimport_ScopeCredentialOnTheWire verifies end to end that
// reimporting a stored harness config sends the hub credential for a global
// config and the owner's own credential for a user config, and that a user
// config whose ScopeID names another user is fetched anonymously, even though
// both users have a credential stored.
func TestHarnessConfigReimport_ScopeCredentialOnTheWire(t *testing.T) {
	tests := []struct {
		name         string
		scope        string
		admin        bool
		otherScopeID bool // store the config under another user's ScopeID
		credScope    string
		wantAuth     string
		want200      bool
	}{
		{"global reimport uses hub credential", store.HarnessConfigScopeGlobal, true, false, secret.ScopeHub, "Bearer fake-scope-cred", true},
		{"user reimport uses owner credential", store.HarnessConfigScopeUser, false, false, secret.ScopeUser, "Bearer fake-scope-cred", true},
		{"user reimport under another user's ScopeID is anonymous", store.HarnessConfigScopeUser, false, true, secret.ScopeUser, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, s, _ := testTemplateBootstrapServer(t)
			ctx := context.Background()

			var user *store.User
			if tt.admin {
				user = skillDiscoverAdmin(t, s, "reimport-cred-admin")
			} else {
				user = skillDiscoverMember(t, s, "reimport-cred-member")
			}
			otherID := tid("reimport-cred-other")

			srv.SetSecretBackend(secret.NewLocalBackend(s, srv.HubID(), "test-secret"))
			credIDs := []string{srv.HubID()}
			if tt.credScope == secret.ScopeUser {
				credIDs = []string{user.ID, otherID}
			}
			for _, id := range credIDs {
				if _, _, err := srv.GetSecretBackend().Set(ctx, &secret.SetSecretInput{
					Name: "GH_ACME_CORP", Value: "fake-scope-cred", SecretType: secret.TypeEnvironment,
					Scope: tt.credScope, ScopeID: id,
				}); err != nil {
					t.Fatalf("failed to store credential: %v", err)
				}
			}

			hc := &store.HarnessConfig{
				ID: tid("reimport-cred-hc"), Name: "my-config", Slug: "my-config", Harness: "claude",
				Scope: tt.scope, Status: store.HarnessConfigStatusActive,
			}
			if tt.scope == store.HarnessConfigScopeUser {
				hc.OwnerID, hc.CreatedBy, hc.ScopeID = user.ID, user.ID, user.ID
				if tt.otherScopeID {
					hc.ScopeID = otherID
				}
			}
			if err := s.CreateHarnessConfig(ctx, hc); err != nil {
				t.Fatalf("failed to create harness config: %v", err)
			}

			// Every outbound request's Authorization header, in order ("" for none).
			var auths []string
			defer mockSkillTarballWithHook(t, map[string]string{
				"repo-main/harness-configs/my-config/config.yaml": "harness: claude\n",
			}, func(req *http.Request) {
				auths = append(auths, req.Header.Get("Authorization"))
			})()

			rec := doRequestAsUser(t, srv, user, http.MethodPost,
				"/api/v1/harness-configs/"+hc.ID+"/reimport",
				ReimportHarnessConfigRequest{SourceURL: "https://github.com/acme-corp/configs/tree/main/harness-configs"})
			if tt.want200 && rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}
			if len(auths) != 1 || auths[0] != tt.wantAuth {
				t.Errorf("Authorization headers = %q, want exactly one fetch with %q (status %d: %s)", auths, tt.wantAuth, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestImportCredential_ScopeConstantsAgree pins that the harness-config scope
// values equal the template scope values importCredential matches on, so a
// rename of either set cannot silently make harness-config reimport
// anonymous.
func TestImportCredential_ScopeConstantsAgree(t *testing.T) {
	for _, pair := range [][2]string{
		{store.HarnessConfigScopeGlobal, store.TemplateScopeGlobal},
		{store.HarnessConfigScopeProject, store.TemplateScopeProject},
		{store.HarnessConfigScopeUser, store.TemplateScopeUser},
	} {
		if pair[0] != pair[1] {
			t.Errorf("harness-config scope %q != template scope %q", pair[0], pair[1])
		}
	}
}
