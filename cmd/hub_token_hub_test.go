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

package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubTokenTestState captures and restores package-level vars used by
// cmd/hub_token.go, for test isolation between subtests that set flags via
// their backing package vars directly.
type hubTokenTestState struct {
	home                string
	projectPath         string
	tokenOutputJSON     bool
	tokenCreateName     string
	tokenCreateProject  string
	tokenCreateBoundary string
	tokenCreateScopes   []string
	tokenCreateExpires  string
	tokenScopesProject  string
	tokenScopesBoundary string
	tokenListProject    string
	tokenListBoundary   string
	tokenListWide       bool
}

func saveHubTokenTestState() hubTokenTestState {
	return hubTokenTestState{
		home:                os.Getenv("HOME"),
		projectPath:         projectPath,
		tokenOutputJSON:     tokenOutputJSON,
		tokenCreateName:     tokenCreateName,
		tokenCreateProject:  tokenCreateProject,
		tokenCreateBoundary: tokenCreateBoundary,
		tokenCreateScopes:   tokenCreateScopes,
		tokenCreateExpires:  tokenCreateExpires,
		tokenScopesProject:  tokenScopesProject,
		tokenScopesBoundary: tokenScopesBoundary,
		tokenListProject:    tokenListProject,
		tokenListBoundary:   tokenListBoundary,
		tokenListWide:       tokenListWide,
	}
}

func (s hubTokenTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	tokenOutputJSON = s.tokenOutputJSON
	tokenCreateName = s.tokenCreateName
	tokenCreateProject = s.tokenCreateProject
	tokenCreateBoundary = s.tokenCreateBoundary
	tokenCreateScopes = s.tokenCreateScopes
	tokenCreateExpires = s.tokenCreateExpires
	tokenScopesProject = s.tokenScopesProject
	tokenScopesBoundary = s.tokenScopesBoundary
	tokenListProject = s.tokenListProject
	tokenListBoundary = s.tokenListBoundary
	tokenListWide = s.tokenListWide
}

// setupHubTokenProject points the CLI's project resolution and Hub
// connection at a test server, following the pattern in hub_scope_test.go /
// hub_secret_test.go's setupSecretProject.
func setupHubTokenProject(t *testing.T, endpoint string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_HUB_ENDPOINT", endpoint)
	projectPath = setupSecretProject(t, home, endpoint)
}

// newHubTokenScopesMockServer serves GET /api/v1/projects/{id} and
// GET /api/v1/auth/scopes for the CLI scopes-command tests.
func newHubTokenScopesMockServer(t *testing.T, projectID string, scopesResp hubclient.ScopesResponse) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "my-project"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/scopes":
			_ = json.NewEncoder(w).Encode(scopesResp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func fixtureScopesResponse(projectID string, eligible bool) hubclient.ScopesResponse {
	var elig *hubclient.ScopeEligibility
	if projectID != "" {
		elig = &hubclient.ScopeEligibility{
			Boundary: hubclient.TokenBoundary{Kind: "project", ProjectID: projectID},
			Eligible: eligible,
		}
		if !eligible {
			elig.Reason = "flat_role_insufficient"
		}
	}
	return hubclient.ScopesResponse{
		Scopes: []hubclient.ScopeInfo{
			{ID: "agent:read", Resource: "agent", Action: "read", Description: "Read agents", PermissionID: "agent.read", Eligibility: elig},
		},
	}
}

func TestRunTokenScopes_TextOutput_NoProject(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	server := newHubTokenScopesMockServer(t, "", fixtureScopesResponse("", false))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenScopesProject = ""
	tokenOutputJSON = false

	out := captureStdout(t, func() {
		require.NoError(t, runTokenScopes(nil, nil))
	})

	assert.Contains(t, out, "SCOPE")
	assert.Contains(t, out, "DESCRIPTION")
	assert.Contains(t, out, "agent:read")
	assert.NotContains(t, out, "ELIGIBLE", "no --project: table must be the plain catalog, no eligibility column")
}

func TestRunTokenScopes_TextOutput_WithProject(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "11111111-1111-1111-1111-111111111111"
	server := newHubTokenScopesMockServer(t, projectID, fixtureScopesResponse(projectID, false))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenScopesProject = projectID
	tokenOutputJSON = false

	out := captureStdout(t, func() {
		require.NoError(t, runTokenScopes(nil, nil))
	})

	assert.Contains(t, out, "ELIGIBLE")
	assert.Contains(t, out, "REASON")
	assert.Contains(t, out, "agent:read")
	assert.Contains(t, out, "false")
	assert.Contains(t, out, "flat_role_insufficient")
}

func TestRunTokenScopes_JSONOutput(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "22222222-2222-2222-2222-222222222222"
	server := newHubTokenScopesMockServer(t, projectID, fixtureScopesResponse(projectID, true))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenScopesProject = projectID
	tokenOutputJSON = true

	out := captureStdout(t, func() {
		require.NoError(t, runTokenScopes(nil, nil))
	})

	var resp hubclient.ScopesResponse
	require.NoError(t, json.Unmarshal([]byte(out), &resp), "output must be valid JSON: %s", out)
	require.Len(t, resp.Scopes, 1)
	require.NotNil(t, resp.Scopes[0].Eligibility)
	assert.True(t, resp.Scopes[0].Eligibility.Eligible)
}

func TestRunTokenCreate_ScopeViolationHint(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "33333333-3333-3333-3333-333333333333"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "my-project"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/tokens":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "scope_violation",
					"message": `requested scopes exceed issuer authority: selector "agent:delete" denied (flat_role_insufficient)`,
					"details": map[string]interface{}{"selector": "agent:delete", "reason": "flat_role_insufficient"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenCreateName = "ci-token"
	tokenCreateProject = projectID
	tokenCreateScopes = []string{"agent:delete"}
	tokenCreateExpires = ""
	tokenOutputJSON = false

	var runErr error
	errOut := captureStderr(t, func() {
		runErr = runTokenCreate(nil, nil)
	})

	require.Error(t, runErr)
	assert.Contains(t, errOut, "agent:delete")
	assert.Contains(t, errOut, "flat_role_insufficient")
	assert.Contains(t, errOut, "hub token scopes")
}

func TestRunTokenCreate_ScopeViolationWithoutDetails_NoEmptySelector(t *testing.T) {
	// An older hub's scope_violation body may carry no details. The
	// "Denied scope" line must not print an empty selector/reason, but the
	// hint to check eligibility is still useful.
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "44444444-4444-4444-4444-444444444444"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "my-project"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/tokens":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "scope_violation",
					"message": "requested scopes exceed issuer authority",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenCreateName = "ci-token"
	tokenCreateProject = projectID
	tokenCreateScopes = []string{"agent:delete"}
	tokenCreateExpires = ""
	tokenOutputJSON = false

	var runErr error
	errOut := captureStderr(t, func() {
		runErr = runTokenCreate(nil, nil)
	})

	require.Error(t, runErr)
	assert.NotContains(t, errOut, `Denied scope ""`)
	assert.Contains(t, errOut, "hub token scopes")
}

// TestHubTokenCreateCmd_JSONFlagRegistered guards against a regression back
// to "checked in runTokenCreate but never registered" (the --json fix): a
// flag lookup, not a network round trip, is what would have caught it.
func TestHubTokenCreateCmd_JSONFlagRegistered(t *testing.T) {
	assert.NotNil(t, hubTokenCreateCmd.Flags().Lookup("json"),
		"hub token create must register --json, not merely check it in runTokenCreate")
}

func TestHubTokenScopesCmd_ProjectFlagRegistered(t *testing.T) {
	assert.NotNil(t, hubTokenScopesCmd.Flags().Lookup("project"))
	assert.NotNil(t, hubTokenScopesCmd.Flags().Lookup("json"))
}

// TestPrintScopeTable_AliasIneligibleMembers verifies the text renderer
// names the ineligible members for a denied alias, not just its own flag.
func TestPrintScopeTable_AliasIneligibleMembers(t *testing.T) {
	resp := &hubclient.ScopesResponse{
		Aliases: []hubclient.ScopeAliasInfo{
			{
				ID: "agent:manage", Description: "All agent management operations",
				ExpandsTo: []string{"agent:delete", "agent:read"},
				Eligibility: &hubclient.ScopeEligibility{
					Boundary:          hubclient.TokenBoundary{Kind: "project", ProjectID: "p1"},
					Eligible:          false,
					IneligibleMembers: []string{"agent:delete"},
				},
			},
		},
	}

	out := captureStdout(t, func() {
		printScopeTable(resp, true)
	})

	assert.True(t, strings.Contains(out, "agent:manage"))
	assert.True(t, strings.Contains(out, "agent:delete"))
}

func TestRunTokenCreate_HubBoundary(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	var body map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/tokens":
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.CreateTokenResponse{
				Token: "synthetic-fixture-value",
				AccessToken: &hubclient.TokenInfo{
					ID: "t1", Name: "health-check", Scopes: []string{"hub_health:read", "broker:read"},
					Boundary: &hubclient.TokenBoundary{Kind: "hub"},
				},
			})
		default:
			// A hub-boundary token resolves no project.
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenCreateName = "health-check"
	tokenCreateProject = ""
	tokenCreateBoundary = "hub"
	tokenCreateScopes = []string{"hub_health:read,broker:read"}
	tokenCreateExpires = ""
	tokenOutputJSON = false

	var runErr error
	out := captureStdout(t, func() {
		runErr = runTokenCreate(nil, nil)
	})
	require.NoError(t, runErr)

	assert.Equal(t, map[string]interface{}{"kind": "hub"}, body["boundary"])
	_, hasProject := body["projectId"]
	assert.False(t, hasProject, "a hub-boundary request must not send projectId")
	assert.Equal(t, []interface{}{"hub_health:read", "broker:read"}, body["scopes"])
	assert.Contains(t, out, "Boundary: "+hubBoundaryDescription)
	assert.NotContains(t, out, "Project:")
}

func TestValidateTokenCreateBoundary(t *testing.T) {
	tests := []struct {
		name     string
		boundary string
		project  string
		wantErr  string
	}{
		{name: "project boundary with project", boundary: "project", project: "p1"},
		{name: "empty boundary means project", boundary: "", project: "p1"},
		{name: "hub boundary without project", boundary: "hub"},
		{name: "project boundary needs project", boundary: "project", wantErr: "--project is required"},
		{name: "hub boundary refuses project", boundary: "hub", project: "p1", wantErr: "cannot be used with --boundary hub"},
		{name: "unknown boundary", boundary: "global", project: "p1", wantErr: "invalid --boundary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTokenCreateBoundary(tt.boundary, tt.project)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestHubTokenCreateCmd_BoundaryFlag(t *testing.T) {
	f := hubTokenCreateCmd.Flags().Lookup("boundary")
	require.NotNil(t, f, "--boundary must be registered on hub token create")
	assert.Equal(t, "project", f.DefValue)
	// --project is validated against --boundary in RunE, so cobra must
	// not require it on its own.
	p := hubTokenCreateCmd.Flags().Lookup("project")
	require.NotNil(t, p)
	_, required := p.Annotations[cobra.BashCompOneRequiredFlag]
	assert.False(t, required)
}

// newNoRequestHubServer is a hub that fails the test on any request: usage
// errors are reported before the CLI contacts the hub.
func newNoRequestHubServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s: a usage error must be reported before any request", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server
}

// fixtureTokenList is a token list with one token of each boundary kind as
// the current hub reports them, plus one from a hub that reports neither a
// boundary nor a project.
func fixtureTokenList(projectID string) hubclient.ListTokensResponse {
	return hubclient.ListTokensResponse{Items: []hubclient.TokenInfo{
		{
			ID: "tok-project", Name: "ci", Prefix: "scion_pat_aaaa", ProjectID: projectID,
			Boundary: &hubclient.TokenBoundary{Kind: "project", ProjectID: projectID},
			Scopes:   []string{"agent:read"}, Purpose: "nightly build",
		},
		{
			ID: "tok-hub", Name: "health", Prefix: "scion_pat_bbbb",
			Boundary: &hubclient.TokenBoundary{Kind: "hub"},
			Scopes:   []string{"hub_health:read"}, Purpose: "platform health",
		},
		{ID: "tok-unknown", Name: "plain", Prefix: "scion_pat_cccc", Scopes: []string{"agent:list"}},
	}}
}

// newHubTokenListMockServer serves GET /api/v1/projects/{id} and
// GET /api/v1/auth/tokens with the given list body for the list tests.
func newHubTokenListMockServer(t *testing.T, projectID string, body interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && projectID != "" && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "my-project"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/auth/tokens":
			_ = json.NewEncoder(w).Encode(body)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// outputLine returns the first text output line containing id, or "" when
// no line does.
func outputLine(out, id string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, id) {
			return line
		}
	}
	return ""
}

func runTokenListForTest(t *testing.T, projectID string, body interface{}) (string, error) {
	t.Helper()
	server := newHubTokenListMockServer(t, projectID, body)
	t.Cleanup(server.Close)
	setupHubTokenProject(t, server.URL)
	var runErr error
	out := captureStdout(t, func() {
		runErr = runTokenList(nil, nil)
	})
	return out, runErr
}

func TestRunTokenList_ShowsBoundaryColumn(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "55555555-5555-5555-5555-555555555555"
	tokenListProject, tokenListBoundary, tokenListWide, tokenOutputJSON = "", "", false, false

	out, err := runTokenListForTest(t, "", fixtureTokenList(projectID))
	require.NoError(t, err)

	assert.Contains(t, out, "BOUNDARY")
	assert.NotContains(t, out, "PURPOSE", "PURPOSE is shown only under --wide")
	assert.Contains(t, outputLine(out, "tok-project"), "project:"+projectID)
	assert.Contains(t, outputLine(out, "tok-hub"), "  hub  ")
	assert.Contains(t, outputLine(out, "tok-unknown"), "unknown")
	assert.NotContains(t, outputLine(out, "tok-unknown"), " hub ", "an unknown boundary is never shown as hub")
	assert.NotContains(t, out, "nightly build")

	t.Run("wide adds purpose", func(t *testing.T) {
		tokenListWide = true
		out, err := runTokenListForTest(t, "", fixtureTokenList(projectID))
		require.NoError(t, err)
		assert.Contains(t, out, "PURPOSE")
		assert.Contains(t, outputLine(out, "tok-project"), "nightly build")
		assert.Contains(t, outputLine(out, "tok-hub"), "platform health")
	})
}

func TestRunTokenList_BoundaryFilter(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "66666666-6666-6666-6666-666666666666"
	tokenListProject, tokenListWide, tokenOutputJSON = "", false, false

	tests := []struct {
		boundary string
		want     string
		absent   []string
	}{
		{boundary: "hub", want: "tok-hub", absent: []string{"tok-project", "tok-unknown"}},
		{boundary: "project", want: "tok-project", absent: []string{"tok-hub", "tok-unknown"}},
	}
	for _, tt := range tests {
		t.Run(tt.boundary, func(t *testing.T) {
			tokenListBoundary = tt.boundary
			out, err := runTokenListForTest(t, "", fixtureTokenList(projectID))
			require.NoError(t, err)
			assert.Contains(t, out, tt.want)
			for _, id := range tt.absent {
				assert.NotContains(t, out, id)
			}
		})
	}

	t.Run("invalid boundary is a usage error", func(t *testing.T) {
		setupHubTokenProject(t, newNoRequestHubServer(t).URL)
		tokenListBoundary = "global"
		err := runTokenList(nil, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid --boundary")
	})

	t.Run("hub boundary with project is a usage error", func(t *testing.T) {
		setupHubTokenProject(t, newNoRequestHubServer(t).URL)
		tokenListBoundary, tokenListProject = "hub", projectID
		err := runTokenList(nil, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--project cannot be used with --boundary hub")
	})
}

func TestRunTokenList_ProjectFilterNeverMatchesHubTokens(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "77777777-7777-7777-7777-777777777777"
	list := fixtureTokenList(projectID)
	// A hub token whose response also carries a projectId still is not a
	// token of that project.
	list.Items = append(list.Items, hubclient.TokenInfo{
		ID: "tok-hub-with-project", Name: "odd", ProjectID: projectID,
		Boundary: &hubclient.TokenBoundary{Kind: "hub"},
	})
	tokenListProject, tokenListBoundary, tokenListWide, tokenOutputJSON = projectID, "", false, false

	out, err := runTokenListForTest(t, projectID, list)
	require.NoError(t, err)

	assert.Contains(t, out, "tok-project")
	assert.NotContains(t, out, "tok-hub")
	assert.NotContains(t, out, "tok-unknown")
}

func TestRunTokenList_JSONOmitsProjectIDForHubToken(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "88888888-8888-8888-8888-888888888888"
	tokenListProject, tokenListBoundary, tokenListWide, tokenOutputJSON = "", "", false, true

	// The hub's own JSON: projectId absent for the hub token.
	body := map[string]interface{}{"items": []map[string]interface{}{
		{"id": "tok-project", "name": "ci", "projectId": projectID, "boundary": map[string]interface{}{"kind": "project", "projectId": projectID}},
		{"id": "tok-hub", "name": "health", "boundary": map[string]interface{}{"kind": "hub"}},
	}}
	out, err := runTokenListForTest(t, "", body)
	require.NoError(t, err)

	var got struct {
		Items []map[string]interface{} `json:"items"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got), "output must be valid JSON: %s", out)
	require.Len(t, got.Items, 2)
	assert.Equal(t, projectID, got.Items[0]["projectId"])
	_, hasProject := got.Items[1]["projectId"]
	assert.False(t, hasProject, "hub token JSON must not carry projectId: %v", got.Items[1])
	assert.Equal(t, map[string]interface{}{"kind": "hub"}, got.Items[1]["boundary"])
}

func TestRunTokenScopes_HubBoundarySendsBoundaryParam(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	var query url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/auth/scopes" {
			// A hub boundary resolves no project.
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		query = r.URL.Query()
		_ = json.NewEncoder(w).Encode(hubclient.ScopesResponse{Scopes: []hubclient.ScopeInfo{
			{ID: "hub_health:read", AllowedBoundaries: []string{"hub"}, Eligibility: &hubclient.ScopeEligibility{
				Boundary: hubclient.TokenBoundary{Kind: "hub"}, Eligible: true,
			}},
			{ID: "broker:create", AllowedBoundaries: []string{"hub"}, Eligibility: &hubclient.ScopeEligibility{
				Boundary: hubclient.TokenBoundary{Kind: "hub"}, Reason: "authority_required",
			}},
		}})
	}))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenScopesBoundary, tokenScopesProject, tokenOutputJSON = "hub", "", false

	out := captureStdout(t, func() {
		require.NoError(t, runTokenScopes(nil, nil))
	})

	assert.Equal(t, "hub", query.Get("boundary"))
	_, hasProject := query["projectId"]
	assert.False(t, hasProject, "a hub boundary must not send projectId")
	assert.Contains(t, out, "ELIGIBLE")
	assert.Contains(t, outputLine(out, "hub_health:read"), "true")
	row := outputLine(out, "broker:create")
	assert.Contains(t, row, "false")
	assert.Contains(t, row, "your account has no current access that includes this permission")
}

func TestRunTokenScopes_BoundaryUsageErrors(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	tests := []struct {
		name, boundary, project, wantErr string
	}{
		{name: "hub with project", boundary: "hub", project: "p1", wantErr: "--project cannot be used with --boundary hub"},
		{name: "project without project", boundary: "project", wantErr: "--project is required with --boundary project"},
		{name: "unknown boundary", boundary: "global", wantErr: "invalid --boundary"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setupHubTokenProject(t, newNoRequestHubServer(t).URL)
			tokenScopesBoundary, tokenScopesProject = tt.boundary, tt.project
			err := runTokenScopes(nil, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestRunTokenScopes_UnsupportedBoundaryFallsBackToCatalog(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	var catalogCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/auth/scopes" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Query().Get("boundary") == "hub" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "unsupported_boundary", "message": "hub boundary eligibility is not available"},
			})
			return
		}
		catalogCalls++
		if r.URL.RawQuery != "" {
			t.Errorf("fallback must request the plain catalog, got %q", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(hubclient.ScopesResponse{
			Scopes: []hubclient.ScopeInfo{
				{ID: "agent:read", Description: "Read agents", AllowedBoundaries: []string{"project", "hub"}},
				{ID: "broker:read", Description: "Read brokers", AllowedBoundaries: []string{"hub"}},
				{ID: "project:only", Description: "Project only", AllowedBoundaries: []string{"project"}},
			},
			Aliases: []hubclient.ScopeAliasInfo{
				{ID: "agent:view", Description: "View agents", ExpandsTo: []string{"agent:read"}},
				{ID: "mixed:manage", Description: "Mixed", ExpandsTo: []string{"agent:read", "project:only"}},
			},
		})
	}))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenScopesBoundary, tokenScopesProject, tokenOutputJSON = "hub", "", false

	var runErr error
	out, errOut := captureStdoutStderr(t, func() {
		runErr = runTokenScopes(nil, nil)
	})
	require.NoError(t, runErr)

	assert.Equal(t, 1, catalogCalls)
	assert.Contains(t, errOut, unsupportedBoundaryNotice)
	assert.NotContains(t, out, "ELIGIBLE", "the fallback has no eligibility to show")
	assert.Contains(t, out, "BOUNDARIES")
	assert.Contains(t, outputLine(out, "broker:read"), "  hub  ", "hub-only scopes are marked")
	assert.Contains(t, out, "agent:read")
	assert.Contains(t, out, "agent:view")
	assert.NotContains(t, out, "project:only", "scopes a hub token may not select are left out")
	assert.NotContains(t, out, "mixed:manage", "aliases that expand to such scopes are left out")

	t.Run("json stays valid", func(t *testing.T) {
		tokenOutputJSON = true
		var jsonErr error
		jsonOut, _ := captureStdoutStderr(t, func() {
			jsonErr = runTokenScopes(nil, nil)
		})
		require.NoError(t, jsonErr)
		var resp hubclient.ScopesResponse
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &resp), "output must be valid JSON: %s", jsonOut)
		assert.Len(t, resp.Scopes, 2)
		assert.Len(t, resp.Aliases, 1)
	})
}

func TestRunTokenCreate_HubDenialHintNamesHubScopesCommand(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/auth/tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "scope_violation",
				"message": "requested scopes exceed issuer authority",
				"details": map[string]interface{}{"selector": "broker:create", "reason": "authority_required"},
			},
		})
	}))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenCreateName = "broker-reg"
	tokenCreateProject = ""
	tokenCreateBoundary = "hub"
	tokenCreateScopes = []string{"broker:create"}
	tokenCreateExpires = ""
	tokenOutputJSON = false

	var runErr error
	errOut := captureStderr(t, func() {
		runErr = runTokenCreate(nil, nil)
	})

	require.Error(t, runErr)
	assert.Contains(t, errOut, `"broker:create"`)
	assert.Contains(t, errOut, "your account has no current access that includes this permission")
	assert.Contains(t, errOut, "scion hub token scopes --boundary hub")
}

func TestRunTokenCreate_ProjectSendsExplicitBoundary(t *testing.T) {
	state := saveHubTokenTestState()
	t.Cleanup(state.restore)

	projectID := "99999999-9999-9999-9999-999999999999"
	var body map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
			_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "my-project"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/tokens":
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.CreateTokenResponse{
				Token: "synthetic-fixture-value",
				AccessToken: &hubclient.TokenInfo{
					ID: "t1", Name: "ci", ProjectID: projectID, Scopes: []string{"agent:read"},
					Boundary: &hubclient.TokenBoundary{Kind: "project", ProjectID: projectID},
				},
			})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	setupHubTokenProject(t, server.URL)

	tokenCreateName = "ci"
	tokenCreateProject = projectID
	tokenCreateBoundary = "project"
	tokenCreateScopes = []string{"agent:read"}
	tokenCreateExpires = ""
	tokenOutputJSON = false

	var runErr error
	out := captureStdout(t, func() {
		runErr = runTokenCreate(nil, nil)
	})
	require.NoError(t, runErr)

	assert.Equal(t, map[string]interface{}{"kind": "project", "projectId": projectID}, body["boundary"])
	assert.Equal(t, projectID, body["projectId"], "a project request also sends the shorthand, naming the same project")
	assert.Contains(t, out, "Project:")
}

func TestTokenBoundaryLabel(t *testing.T) {
	tests := []struct {
		name string
		info hubclient.TokenInfo
		want string
	}{
		{name: "hub", info: hubclient.TokenInfo{Boundary: &hubclient.TokenBoundary{Kind: "hub"}}, want: "hub"},
		{name: "project from boundary", info: hubclient.TokenInfo{Boundary: &hubclient.TokenBoundary{Kind: "project", ProjectID: "p1"}}, want: "project:p1"},
		{name: "project from projectId only", info: hubclient.TokenInfo{ProjectID: "p2"}, want: "project:p2"},
		{name: "project without a project ID", info: hubclient.TokenInfo{Boundary: &hubclient.TokenBoundary{Kind: "project"}}, want: "project"},
		{name: "neither is unknown", info: hubclient.TokenInfo{}, want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tokenBoundaryLabel(tt.info))
		})
	}
}
