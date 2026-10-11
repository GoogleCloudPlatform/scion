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

package hub

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// renderSPAShell executes spaShellTemplate with placeholder data and returns
// the resulting HTML, without needing a running WebServer or HTTP request.
func renderSPAShell(t *testing.T) string {
	t.Helper()
	ws := NewWebServer(WebServerConfig{})
	require.NotNil(t, ws.shellTmpl, "spaShellTemplate must parse")
	var buf bytes.Buffer
	require.NoError(t, ws.shellTmpl.Execute(&buf, spaShellData{}))
	return buf.String()
}

// extractBetween returns the substring from the first occurrence of start up
// to (not including) the first occurrence of end found after start.
func extractBetween(t *testing.T, html, start, end string) string {
	t.Helper()
	startIdx := strings.Index(html, start)
	require.NotEqual(t, -1, startIdx, "start marker %q not found", start)
	endIdx := strings.Index(html[startIdx:], end)
	require.NotEqual(t, -1, endIdx, "end marker %q not found after start", end)
	return html[startIdx : startIdx+endIdx]
}

// mockProxyAuthenticator is a test double for ProxyAuthenticator.
type mockProxyAuthenticator struct {
	user *ProxyUserInfo
	err  error
}

// proxyAuthStore is a minimal store that supports the proxy auth user provisioning path.
type proxyAuthStore struct {
	store.Store     // embed interface to satisfy all methods
	users           map[string]*store.User
	roleBindings    map[string]*store.RoleBinding
	roleDefinitions map[string]*store.RoleDefinition
}

func newProxyAuthStore() *proxyAuthStore {
	return &proxyAuthStore{
		users:           make(map[string]*store.User),
		roleBindings:    make(map[string]*store.RoleBinding),
		roleDefinitions: make(map[string]*store.RoleDefinition),
	}
}

// newProxyAuthStoreWithRoles returns a proxyAuthStore pre-seeded with the
// super-admin role definition so that ensureSuperAdminRoleBinding /
// deleteSuperAdminRoleBinding can create and remove bindings.
func newProxyAuthStoreWithRoles() *proxyAuthStore {
	s := newProxyAuthStore()
	s.roleDefinitions["rd-super-admin"] = &store.RoleDefinition{
		ID:        "rd-super-admin",
		Name:      store.SystemRoleSuperAdmin,
		ScopeType: store.RoleScopeSystem,
		System:    true,
	}
	return s
}

// staticAccessSettings is a test implementation of AccessSettingsProvider
// with mutable fields for simulating live config changes in tests.
type staticAccessSettings struct {
	adminEmails       []string
	authorizedDomains []string
	userAccessMode    string
	defaultUserRole   string
}

func newTestWebServer(t *testing.T, cfg WebServerConfig) *WebServer {
	t.Helper()
	ws := NewWebServer(cfg)
	if ws.assets == nil && ws.assetsDisk == "" && cfg.AssetsDir == "" {
		ws.assets = fstest.MapFS{
			"assets/main.js": &fstest.MapFile{Data: []byte("// test stub")},
		}
		ws.hasAssets = ws.detectWebAssets()
	}
	return ws
}

// newDevAuthWebServer creates a web server with dev-auth enabled for testing
// authenticated routes without requiring OAuth.
//
// By default a minimal authoritative store is installed containing an active
// DevUserID record so that the suspendedUserMiddleware (which correctly fails
// closed when ws.store is nil) passes through to the handler under test.
// Tests that intentionally exercise nil-store or error-store paths can
// override ws.store after this call returns.
func newDevAuthWebServer(t *testing.T, overrides ...func(*WebServerConfig)) *WebServer {
	t.Helper()
	cfg := WebServerConfig{
		Host:         "127.0.0.1",
		DevAuthToken: "test-dev-token-12345",
	}
	for _, fn := range overrides {
		fn(&cfg)
	}
	ws := NewWebServer(cfg)
	if ws.assets == nil && ws.assetsDisk == "" {
		ws.assets = fstest.MapFS{
			"assets/main.js": &fstest.MapFile{Data: []byte("// test stub")},
		}
	}
	// When a disk-based assets dir is used, provision assets/main.js so that
	// detectWebAssets() detects valid assets on disk.
	if ws.assetsDisk != "" {
		assetsSubDir := filepath.Join(ws.assetsDisk, "assets")
		if err := os.MkdirAll(assetsSubDir, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(assetsSubDir, "main.js"), []byte("// test stub"), 0o644)
		}
	}
	ws.hasAssets = ws.detectWebAssets()

	// Install a minimal authoritative store with an active dev user so the
	// suspended-user middleware does not fail closed on every authenticated
	// request.  This mirrors production where the store is always present.
	devStore := newProxyAuthStore()
	devStore.users[DevUserID] = &store.User{
		ID:     DevUserID,
		Email:  "dev@localhost",
		Role:   "admin",
		Status: store.UserStatusActive,
	}
	ws.store = devStore

	return ws
}

// mockOAuthTransport intercepts HTTP requests to Google OAuth endpoints and
// returns canned responses. This lets us drive handleOAuthCallback through
// the full handler path without hitting real Google servers.
type mockOAuthTransport struct {
	tokenJSON    string // response body for the token endpoint
	userinfoJSON string // response body for the userinfo endpoint
}

func (m *mockProxyAuthenticator) Authenticate(_ *http.Request) (*ProxyUserInfo, error) {
	return m.user, m.err
}
func (m *mockProxyAuthenticator) Name() string { return "mock" }

func (s *proxyAuthStore) GetUserByEmail(_ context.Context, email string) (*store.User, error) {
	for _, u := range s.users {
		if u.Email == email {
			return u, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *proxyAuthStore) CreateUser(_ context.Context, user *store.User) error {
	s.users[user.ID] = user
	return nil
}

func (s *proxyAuthStore) UpdateUser(_ context.Context, user *store.User) error {
	s.users[user.ID] = user
	return nil
}

func (s *proxyAuthStore) GetGroupBySlug(_ context.Context, _ string) (*store.Group, error) {
	return nil, store.ErrNotFound // hub-members group not found is gracefully handled
}

func (s *proxyAuthStore) AddGroupMember(_ context.Context, _ *store.GroupMember) error {
	return nil
}

func (s *proxyAuthStore) GetUser(_ context.Context, id string) (*store.User, error) {
	if u, ok := s.users[id]; ok {
		return u, nil
	}
	return nil, store.ErrNotFound
}

func (s *proxyAuthStore) GetRoleDefinitionByName(_ context.Context, name string, scopeType string) (*store.RoleDefinition, error) {
	for _, rd := range s.roleDefinitions {
		if rd.Name == name && rd.ScopeType == scopeType {
			return rd, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *proxyAuthStore) CreateRoleBinding(_ context.Context, rb *store.RoleBinding) (*store.RoleBinding, error) {
	// Check for duplicate (same role definition + principal + scope)
	for _, existing := range s.roleBindings {
		if existing.RoleDefinitionID == rb.RoleDefinitionID &&
			existing.PrincipalType == rb.PrincipalType &&
			existing.PrincipalID == rb.PrincipalID &&
			existing.ScopeType == rb.ScopeType &&
			existing.ScopeID == rb.ScopeID {
			return nil, store.ErrAlreadyExists
		}
	}
	id := generateID()
	rb.ID = id
	s.roleBindings[id] = rb
	return rb, nil
}

func (s *proxyAuthStore) ListRoleBindingsForPrincipal(_ context.Context, principalType, principalID string) ([]*store.RoleBinding, error) {
	var result []*store.RoleBinding
	for _, rb := range s.roleBindings {
		if rb.PrincipalType == principalType && rb.PrincipalID == principalID {
			result = append(result, rb)
		}
	}
	return result, nil
}

func (s *proxyAuthStore) DeleteRoleBinding(_ context.Context, id string) error {
	if _, ok := s.roleBindings[id]; !ok {
		return store.ErrNotFound
	}
	delete(s.roleBindings, id)
	return nil
}

// hasSuperAdminBinding returns true if the store contains a system-scoped
// super-admin role binding for the given user.
func (s *proxyAuthStore) hasSuperAdminBinding(userID string) bool {
	for _, rb := range s.roleBindings {
		if rb.PrincipalType == store.RoleBindingPrincipalUser &&
			rb.PrincipalID == userID &&
			rb.ScopeType == store.RoleScopeSystem {
			// Check if this binding references the super-admin role definition
			if rd, ok := s.roleDefinitions[rb.RoleDefinitionID]; ok && rd.Name == store.SystemRoleSuperAdmin {
				return true
			}
		}
	}
	return false
}

func (s *staticAccessSettings) AdminEmails() []string       { return s.adminEmails }
func (s *staticAccessSettings) AuthorizedDomains() []string { return s.authorizedDomains }
func (s *staticAccessSettings) UserAccessMode() string      { return s.userAccessMode }
func (s *staticAccessSettings) DefaultUserRole() string {
	if s.defaultUserRole == "" {
		return "member"
	}
	return s.defaultUserRole
}

func (t *mockOAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body string
	switch {
	case strings.Contains(req.URL.String(), "oauth2.googleapis.com/token"):
		body = t.tokenJSON
	case strings.Contains(req.URL.String(), "googleapis.com/oauth2"):
		body = t.userinfoJSON
	default:
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found"))}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}, nil
}
