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
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const terminalWorkspacePath = "/api/v1/users/me/terminal-workspace"

// twResponse mirrors the wire shape for decoding test assertions.
type twResponse struct {
	AgentIDs         []string   `json:"agentIds"`
	FrontmostAgentID *string    `json:"frontmostAgentId"`
	Revision         int64      `json:"revision"`
	UpdatedAt        *time.Time `json:"updatedAt"`
	Pruned           int        `json:"pruned"`
}

func decodeTWResponse(t *testing.T, rec *httptest.ResponseRecorder) twResponse {
	t.Helper()
	var resp twResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp
}

// twUser creates and returns a plain user for terminal-workspace tests.
func twUser(t *testing.T, s store.Store, id string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID: id, Email: id + "@test.com", DisplayName: "TW User", Role: store.UserRoleMember, Status: "active",
	}
	require.NoError(t, s.CreateUser(ctx, u))
	return u
}

// twAgent creates an agent in a fresh project, owned by ownerID unless empty.
// Ownership is the simplest structural grant for ActionAttach (relationship
// grant: resource owner — see TestAuthz_OwnerBypass), so this is enough to
// make the agent attachable by its owner together with the access-only
// project binding added below: the owner relationship requires active
// project access (ptone/scion#2141), and that binding grants no permission
// itself.
func twAgent(t *testing.T, s store.Store, ownerID string) *store.Agent {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID:   api.NewUUID(),
		Name: "tw-test-project-" + api.NewUUID()[:8],
		Slug: "tw-test-project-" + api.NewUUID()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "tw-test-agent-" + api.NewUUID()[:8],
		Slug:      "tw-test-agent-" + api.NewUUID()[:8],
		ProjectID: project.ID,
		OwnerID:   ownerID,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	if ownerID != "" {
		grantProjectAccessOnly(t, s, ownerID, project.ID)
	}
	return agent
}

// doTWRequestWithCredential injects identity and credential context directly
// and calls the handler, bypassing auth middleware. Used for credential-kind
// boundary tests (UAT, agent JWT, broker) that cannot be produced through the
// normal login/token-issuance paths in a unit test.
func doTWRequestWithCredential(t *testing.T, srv *Server, method string, body interface{}, identity Identity, cred CredentialContext) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, terminalWorkspacePath, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	ctx := req.Context()
	if identity != nil {
		ctx = contextWithIdentity(ctx, identity)
	}
	ctx = contextWithCredentialContext(ctx, cred)
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.handleUserMeTerminalWorkspace(rec, req)
	return rec
}

// fakeAgentIdentity is a minimal AgentIdentity stub for negative credential
// tests: it exists only to make the "not a UserIdentity" branch of
// requireSessionCredential reachable directly.
type fakeAgentIdentity struct{}

func (fakeAgentIdentity) ID() string                    { return tid("tw-fake-agent") }
func (fakeAgentIdentity) Type() string                  { return "agent" }
func (fakeAgentIdentity) ProjectID() string             { return "" }
func (fakeAgentIdentity) Scopes() []AgentTokenScope     { return nil }
func (fakeAgentIdentity) HasScope(AgentTokenScope) bool { return false }
func (fakeAgentIdentity) Ancestry() []string            { return nil }
func (fakeAgentIdentity) OriginUserID() string          { return "" }
func (fakeAgentIdentity) TokenID() string               { return "" }

// fakeNonUserIdentity is a bare Identity (not a UserIdentity or AgentIdentity)
// used to exercise the broker-credential boundary directly.
type fakeNonUserIdentity struct{}

func (fakeNonUserIdentity) ID() string   { return tid("tw-fake-broker") }
func (fakeNonUserIdentity) Type() string { return "broker" }

// ---------------------------------------------------------------------------
// GET: no row, round trip, self-only
// ---------------------------------------------------------------------------

func TestTerminalWorkspace_GetNoRow_ReturnsEmptyShape(t *testing.T) {
	srv, s := testServer(t)
	alice := twUser(t, s, tid("tw-empty-alice"))

	rec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	resp := decodeTWResponse(t, rec)
	assert.Equal(t, []string{}, resp.AgentIDs)
	assert.Nil(t, resp.FrontmostAgentID)
	assert.Equal(t, int64(0), resp.Revision)
	assert.Nil(t, resp.UpdatedAt)
	assert.Equal(t, 0, resp.Pruned)
}

func TestTerminalWorkspace_PutThenGet_RoundTripLowercased(t *testing.T) {
	srv, s := testServer(t)
	alice := twUser(t, s, tid("tw-roundtrip-alice"))
	a1 := twAgent(t, s, alice.ID)
	a2 := twAgent(t, s, alice.ID)

	upper1 := strings.ToUpper(a1.ID)
	body := map[string]interface{}{
		"agentIds":         []string{upper1, a2.ID},
		"frontmostAgentId": strings.ToUpper(a2.ID),
	}
	putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath, body)
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())
	putResp := decodeTWResponse(t, putRec)
	assert.Equal(t, []string{strings.ToLower(a1.ID), strings.ToLower(a2.ID)}, putResp.AgentIDs)
	require.NotNil(t, putResp.FrontmostAgentID)
	assert.Equal(t, strings.ToLower(a2.ID), *putResp.FrontmostAgentID)
	assert.Equal(t, int64(1), putResp.Revision)
	assert.Equal(t, 0, putResp.Pruned)

	getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	getResp := decodeTWResponse(t, getRec)
	assert.Equal(t, []string{strings.ToLower(a1.ID), strings.ToLower(a2.ID)}, getResp.AgentIDs)
	require.NotNil(t, getResp.FrontmostAgentID)
	assert.Equal(t, strings.ToLower(a2.ID), *getResp.FrontmostAgentID)
	assert.Equal(t, int64(1), getResp.Revision)
	assert.Equal(t, 0, getResp.Pruned)

	// A second PUT replaces the list and increments the revision.
	a3 := twAgent(t, s, alice.ID)
	body2 := map[string]interface{}{"agentIds": []string{a3.ID}, "frontmostAgentId": a3.ID}
	putRec2 := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath, body2)
	require.Equal(t, http.StatusOK, putRec2.Code, putRec2.Body.String())
	putResp2 := decodeTWResponse(t, putRec2)
	assert.Equal(t, int64(2), putResp2.Revision)
	assert.Equal(t, []string{a3.ID}, putResp2.AgentIDs)
}

func TestTerminalWorkspace_SelfOnly(t *testing.T) {
	srv, s := testServer(t)
	alice := twUser(t, s, tid("tw-selfonly-alice"))
	bob := twUser(t, s, tid("tw-selfonly-bob"))
	a1 := twAgent(t, s, alice.ID)

	putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
		map[string]interface{}{"agentIds": []string{a1.ID}, "frontmostAgentId": a1.ID})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	getRec := doRequestAsUser(t, srv, bob, http.MethodGet, terminalWorkspacePath, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	resp := decodeTWResponse(t, getRec)
	assert.Equal(t, []string{}, resp.AgentIDs)
	assert.Equal(t, int64(0), resp.Revision, "bob must see no row: alice's write is not visible to him")
}

// ---------------------------------------------------------------------------
// PUT validation
// ---------------------------------------------------------------------------

func TestTerminalWorkspace_PutValidation(t *testing.T) {
	srv, s := testServer(t)
	alice := twUser(t, s, tid("tw-validation-alice"))
	a1 := twAgent(t, s, alice.ID)
	a2 := twAgent(t, s, alice.ID)

	t.Run("too many entries", func(t *testing.T) {
		ids := make([]string, 33)
		for i := range ids {
			ids[i] = api.NewUUID()
		}
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": ids})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("non-UUID entry", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{"not-a-uuid"}})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("duplicate entry", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{a1.ID, strings.ToUpper(a1.ID)}})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("frontmost not in list", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{a1.ID}, "frontmostAgentId": a2.ID})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("frontmost set with empty list", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{}, "frontmostAgentId": a1.ID})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("unknown field", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{a1.ID}, "unexpectedField": true})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("body over 16 KiB", func(t *testing.T) {
		huge := strings.Repeat("a", 20*1024)
		req := httptest.NewRequest(http.MethodPut, terminalWorkspacePath,
			strings.NewReader(`{"agentIds":[],"frontmostAgentId":"`+huge+`"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("well-formed but non-existent UUID is accepted (no existence oracle)", func(t *testing.T) {
		ghost := api.NewUUID()
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{ghost}, "frontmostAgentId": ghost})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}

// ---------------------------------------------------------------------------
// Credential boundary
// ---------------------------------------------------------------------------

func TestTerminalWorkspace_CredentialBoundary(t *testing.T) {
	t.Run("interactive session allowed", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-cred-interactive"))
		rec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("dev credential allowed", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doRequest(t, srv, http.MethodGet, terminalWorkspacePath, nil)
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("no credential unauthorized", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doRequestNoAuth(t, srv, http.MethodGet, terminalWorkspacePath, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})

	t.Run("UAT forbidden", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-cred-uat"))
		identity := NewAuthenticatedUser(alice.ID, alice.Email, alice.DisplayName, alice.Role, "api")
		rec := doTWRequestWithCredential(t, srv, http.MethodGet, nil, identity,
			CredentialContext{Kind: CredentialKindUAT, ProjectID: tid("tw-cred-uat-proj")})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("agent JWT forbidden", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doTWRequestWithCredential(t, srv, http.MethodGet, nil, fakeAgentIdentity{},
			CredentialContext{Kind: CredentialKindAgentJWT})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("broker forbidden", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doTWRequestWithCredential(t, srv, http.MethodGet, nil, fakeNonUserIdentity{},
			CredentialContext{Kind: CredentialKindBroker})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("no identity in context unauthorized", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doTWRequestWithCredential(t, srv, http.MethodGet, nil, nil,
			CredentialContext{Kind: CredentialKindBroker})
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})

	t.Run("POST not allowed", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doRequest(t, srv, http.MethodPost, terminalWorkspacePath, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
	})

	t.Run("DELETE not allowed", func(t *testing.T) {
		srv, _ := testServer(t)
		rec := doRequest(t, srv, http.MethodDelete, terminalWorkspacePath, nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
	})
}

// ---------------------------------------------------------------------------
// Pruning
// ---------------------------------------------------------------------------

func TestTerminalWorkspace_Pruning(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	alice := twUser(t, s, tid("tw-prune-alice"))

	agentA := twAgent(t, s, alice.ID) // kept: owned by alice
	agentB := twAgent(t, s, alice.ID) // soft-deleted
	agentB.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, agentB))
	agentCID := api.NewUUID()   // does not exist at all
	agentD := twAgent(t, s, "") // exists, but alice cannot attach (not owned by her, no binding)
	agentE := twAgent(t, s, alice.ID)
	agentE.Phase = "stopped" // stopped, but still owned/attachable: must be kept
	require.NoError(t, s.UpdateAgent(ctx, agentE))

	stored := []string{agentA.ID, agentB.ID, agentCID, agentD.ID, agentE.ID}
	putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
		map[string]interface{}{"agentIds": stored, "frontmostAgentId": agentB.ID})
	require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

	getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
	require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
	resp := decodeTWResponse(t, getRec)

	// Order preserved: Equal, not ElementsMatch, so a regression that
	// rebuilds the list from the agents map (unordered) would fail this.
	assert.Equal(t, []string{agentA.ID, agentE.ID}, resp.AgentIDs)
	assert.Equal(t, 3, resp.Pruned, "B (soft-deleted), C (missing) and D (denied) must be pruned")
	assert.Nil(t, resp.FrontmostAgentID, "the saved frontmost (B) was pruned")

	// GET has no side effects: the stored row still has the unpruned list, in
	// its original order.
	row, err := s.GetUserTerminalWorkspace(ctx, alice.ID)
	require.NoError(t, err)
	assert.Equal(t, stored, row.AgentIDs)
	assert.Equal(t, int64(1), row.Revision)
}

// ---------------------------------------------------------------------------
// Error-derived denies keep entries: an access check that could not be
// decided (a store/resolution fault on one of the four tagged paths, see
// authz_resolution_error_test.go) must not prune. These reuse the fail*Store
// wrappers defined there, binding a fresh AuthzService
// to the same underlying store so only the targeted authz sub-call fails.
// ---------------------------------------------------------------------------

func TestTerminalWorkspace_Pruning_KeepsErrorDerivedDenies(t *testing.T) {
	t.Run("principal resolution error", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-err-principal"))
		agent := twAgent(t, s, alice.ID)
		putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{agent.ID}, "frontmostAgentId": agent.ID})
		require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

		srv.authzService = NewAuthzService(
			&failEffectiveGroupsStore{Store: s, failErr: errors.New("injected principal resolution failure")},
			slog.Default(),
		)

		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
		require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
		resp := decodeTWResponse(t, getRec)
		assert.Equal(t, []string{agent.ID}, resp.AgentIDs)
		assert.Equal(t, 0, resp.Pruned)
	})

	t.Run("role-binding resolution error", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-err-binding"))
		agent := twAgent(t, s, alice.ID)
		putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{agent.ID}})
		require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

		srv.authzService = NewAuthzService(
			&failBindingsStore{Store: s, failErr: errors.New("injected binding resolution failure")},
			slog.Default(),
		)

		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
		require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
		resp := decodeTWResponse(t, getRec)
		assert.Equal(t, []string{agent.ID}, resp.AgentIDs)
		assert.Equal(t, 0, resp.Pruned)
	})

	t.Run("role-definition resolution error", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-err-roledef"))
		projectID := tid("tw-err-roledef-proj")
		createDelegateTestProject(t, s, projectID, "tw-err-roledef-proj", "test")
		// A role binding is required so loadRoleDefinitions is actually
		// called (it short-circuits on an empty role-definition ID list).
		createTestUserWithProjectRole(t, s, alice.ID, alice.Email, projectID, store.ProjectRoleMember)
		agent := twAgent(t, s, alice.ID)
		putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{agent.ID}})
		require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

		srv.authzService = NewAuthzService(
			&failRoleDefsStore{Store: s, failErr: errors.New("injected role-definition resolution failure")},
			slog.Default(),
		)

		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
		require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
		resp := decodeTWResponse(t, getRec)
		assert.Equal(t, []string{agent.ID}, resp.AgentIDs)
		assert.Equal(t, 0, resp.Pruned)
	})

	t.Run("access-constraint load error", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-err-constraint"))
		agent := twAgent(t, s, alice.ID)
		putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{agent.ID}})
		require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

		srv.authzService = NewAuthzService(
			&failConstraintsStore{Store: s, failErr: errors.New("injected access-constraint load failure")},
			slog.Default(),
		)

		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
		require.Equal(t, http.StatusOK, getRec.Code, getRec.Body.String())
		resp := decodeTWResponse(t, getRec)
		assert.Equal(t, []string{agent.ID}, resp.AgentIDs)
		assert.Equal(t, 0, resp.Pruned)
	})
}

// ---------------------------------------------------------------------------
// Store errors on GET → 500, row unchanged
// ---------------------------------------------------------------------------

// failGetAgentsByIDsStore fails GetAgentsByIDs.
type failGetAgentsByIDsStore struct {
	store.Store
	failErr error
}

func (s *failGetAgentsByIDsStore) GetAgentsByIDs(ctx context.Context, ids []string) (map[string]*store.Agent, error) {
	return nil, s.failErr
}

// failGetUserTerminalWorkspaceStore fails GetUserTerminalWorkspace with a
// non-NotFound error, simulating a row-load fault rather than "never saved".
type failGetUserTerminalWorkspaceStore struct {
	store.Store
	failErr error
}

func (s *failGetUserTerminalWorkspaceStore) GetUserTerminalWorkspace(ctx context.Context, userID string) (*store.UserTerminalWorkspace, error) {
	return nil, s.failErr
}

func TestTerminalWorkspace_Get_StoreErrors(t *testing.T) {
	t.Run("GetAgentsByIDs fails returns 500 and the row is unchanged", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-500-agents"))
		agent := twAgent(t, s, alice.ID)
		putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{agent.ID}})
		require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

		srv.store = &failGetAgentsByIDsStore{Store: s, failErr: errors.New("injected GetAgentsByIDs failure")}
		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
		assert.Equal(t, http.StatusInternalServerError, getRec.Code, getRec.Body.String())

		srv.store = s
		row, err := s.GetUserTerminalWorkspace(context.Background(), alice.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{agent.ID}, row.AgentIDs)
		assert.Equal(t, int64(1), row.Revision)
	})

	t.Run("row load fails returns 500 and the row is unchanged", func(t *testing.T) {
		srv, s := testServer(t)
		alice := twUser(t, s, tid("tw-500-row"))
		agent := twAgent(t, s, alice.ID)
		putRec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{agent.ID}})
		require.Equal(t, http.StatusOK, putRec.Code, putRec.Body.String())

		srv.store = &failGetUserTerminalWorkspaceStore{Store: s, failErr: errors.New("injected row load failure")}
		getRec := doRequestAsUser(t, srv, alice, http.MethodGet, terminalWorkspacePath, nil)
		assert.Equal(t, http.StatusInternalServerError, getRec.Code, getRec.Body.String())

		srv.store = s
		row, err := s.GetUserTerminalWorkspace(context.Background(), alice.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{agent.ID}, row.AgentIDs)
		assert.Equal(t, int64(1), row.Revision)
	})
}

// ---------------------------------------------------------------------------
// PUT body validation: agentIds required, no trailing content
// ---------------------------------------------------------------------------

func TestTerminalWorkspace_PutRequiresAgentIDs(t *testing.T) {
	srv, s := testServer(t)
	alice := twUser(t, s, tid("tw-put-required"))

	t.Run("missing agentIds field is rejected, not treated as empty", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath, map[string]interface{}{})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("JSON null body is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, terminalWorkspacePath, strings.NewReader("null"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("trailing content after the JSON object is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, terminalWorkspacePath,
			strings.NewReader(`{"agentIds":[]} {"agentIds":["`+api.NewUUID()+`"]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	})

	t.Run("an explicit empty list is still valid (saved empty)", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodPut, terminalWorkspacePath,
			map[string]interface{}{"agentIds": []string{}})
		assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}
