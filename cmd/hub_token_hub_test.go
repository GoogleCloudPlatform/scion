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
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubTokenTestState captures and restores package-level vars used by
// cmd/hub_token.go, for test isolation between subtests that set flags via
// their backing package vars directly.
type hubTokenTestState struct {
	home               string
	projectPath        string
	tokenOutputJSON    bool
	tokenCreateName    string
	tokenCreateProject string
	tokenCreateScopes  []string
	tokenCreateExpires string
	tokenScopesProject string
}

func saveHubTokenTestState() hubTokenTestState {
	return hubTokenTestState{
		home:               os.Getenv("HOME"),
		projectPath:        projectPath,
		tokenOutputJSON:    tokenOutputJSON,
		tokenCreateName:    tokenCreateName,
		tokenCreateProject: tokenCreateProject,
		tokenCreateScopes:  tokenCreateScopes,
		tokenCreateExpires: tokenCreateExpires,
		tokenScopesProject: tokenScopesProject,
	}
}

func (s hubTokenTestState) restore() {
	_ = os.Setenv("HOME", s.home)
	projectPath = s.projectPath
	tokenOutputJSON = s.tokenOutputJSON
	tokenCreateName = s.tokenCreateName
	tokenCreateProject = s.tokenCreateProject
	tokenCreateScopes = s.tokenCreateScopes
	tokenCreateExpires = s.tokenCreateExpires
	tokenScopesProject = s.tokenScopesProject
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
