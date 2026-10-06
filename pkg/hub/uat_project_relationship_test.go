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

// Tests for ptone/scion#2092: project UATs require active project access
// for project targets, and a member may select relationship-scoped agent
// attach/port access before owning any agent.
//
// CreateToken's mint path calls AuthzService.CanMintSelector, and
// enforceUATConstraints calls AuthzService.ProjectTargetAdmission for
// project targets (both in pkg/hub/authz_boundary.go).
//
// Kept in its own file (not authz_cross_member_attach_test.go) to avoid
// contention with ptone/scion#2118, which edits that file's
// legacy-implication assertions.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

// uatpMember creates a project-member user, matching production shape: a
// project-scoped member role binding AND the seeded hub-members group
// (seed.go:462-480), which every logged-in user actually holds. Tests in
// this file deliberately do not use a minimal fixture that omits hub
// membership: the seeded hub-member system role interacts with the
// live-project-access gate (see
// TestProjectUAT_HubMembershipAloneDoesNotGrantProjectAccess and the
// seeded-role regressions below), and a fixture without that binding would
// hide the interaction instead of exposing it.
func uatpMember(t *testing.T, s store.Store, projectID, userID string) {
	t.Helper()
	createTestUserWithProjectRole(t, s, userID, userID+"@test.com", projectID, store.ProjectRoleMember)
	ensureHubMembership(context.Background(), s, userID)
}

// uatpAgent creates an agent directly via the store with Hub-recorded
// OwnerID/Ancestry, rather than through the HTTP handler.
func uatpAgent(t *testing.T, s store.Store, projectID, ownerID, idSuffix string, ancestry ...string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:        tid("uatp-agent-" + idSuffix),
		Slug:      "uatp-agent-" + idSuffix,
		Name:      "UATP Agent " + idSuffix,
		ProjectID: projectID,
		OwnerID:   ownerID,
		Phase:     string(state.PhaseStopped),
		Ancestry:  ancestry,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	return agent
}

// uatpExposePort registers an exposed port directly via the store, instead
// of the HTTP registration path (which scoped UATs cannot use --
// authorizePortRegistration always denies ScopedUserIdentity, by design).
func uatpExposePort(t *testing.T, s store.Store, agent *store.Agent, port int) {
	t.Helper()
	ports := append([]store.ExposedPort(nil), agent.ExposedPorts...)
	ports = append(ports, store.ExposedPort{
		Port: port, Label: "web", Host: "127.0.0.1", Mode: "rw",
		ExposedAt: time.Now().UTC(), ExposedBy: "agent",
	})
	require.NoError(t, s.UpdateAgentExposedPorts(context.Background(), agent.ID, ports))
	agent.ExposedPorts = ports
}

// assertAuthorizedPTY asserts rec is the "authorized, no runtime broker"
// signal for a /pty preflight: pty_handlers.go runs authorization first and
// only then checks for a runtime broker, returning 422 when the agent has
// none configured (as every agent in this file's fixtures does) -- so 422
// here is the positive authorization signal, not a validation failure.
// Asserting the exact code (rather than merely != 403) also catches a
// regression to 401/404/500, which a loose "not forbidden" check would miss.
func assertAuthorizedPTY(t *testing.T, rec *httptest.ResponseRecorder, msgAndArgs ...interface{}) {
	t.Helper()
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code, msgAndArgs...)
}

// requireAuthorizedPTY is assertAuthorizedPTY's require-semantics twin, for
// sanity preconditions a test cannot usefully continue past.
func requireAuthorizedPTY(t *testing.T, rec *httptest.ResponseRecorder, msgAndArgs ...interface{}) {
	t.Helper()
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, msgAndArgs...)
}

// assertAuthorizedPortProxy is assertAuthorizedPTY's port-proxy equivalent:
// authorization runs first, and an authorized request against a port with
// no active tunnel returns 503.
func assertAuthorizedPortProxy(t *testing.T, rec *httptest.ResponseRecorder, msgAndArgs ...interface{}) {
	t.Helper()
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, msgAndArgs...)
}

// uatpDeleteProjectBinding removes userID's direct project-scoped role
// binding(s) in projectID, modeling an admin removing the member.
func uatpDeleteProjectBinding(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
		}
	}
}

// uatpInsertLegacyToken inserts a project-boundary token row with the given
// scopes directly into the store, without minting, so a test can evaluate a
// stored scope that mint would refuse. Use-time evaluation applies the
// ceiling, boundary eligibility and current authority.
func uatpInsertLegacyToken(t *testing.T, s store.Store, userID, projectID string, scopes []string) string {
	t.Helper()
	return uatpInsertStoredToken(t, s, userID, "", projectID, scopes)
}

// uatpInsertHubToken inserts a hub-boundary token row with the given
// scopes and no ceiling version directly into the store, without minting.
func uatpInsertHubToken(t *testing.T, s store.Store, userID string, scopes []string) string {
	t.Helper()
	return uatpInsertStoredToken(t, s, userID, string(BoundaryKindHub), "", scopes)
}

// uatpInsertStoredToken inserts a token row with the given boundary kind,
// project ID and scopes, and returns its key. An empty boundary kind
// stores a project boundary.
func uatpInsertStoredToken(t *testing.T, s store.Store, userID, boundaryKind, projectID string, scopes []string) string {
	t.Helper()
	ctx := context.Background()
	keyBody := uuid.NewString()
	key := store.UATPrefix + keyBody
	hash := sha256.Sum256([]byte(key))
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{
		ID:           uuid.NewString(),
		UserID:       userID,
		Name:         "legacy",
		Prefix:       key[:len(store.UATPrefix)+UATPrefixLength],
		KeyHash:      hex.EncodeToString(hash[:]),
		BoundaryKind: boundaryKind,
		ProjectID:    projectID,
		Scopes:       scopes,
		Created:      time.Now(),
	}))
	return key
}

// ---------------------------------------------------------------------------
// Acceptance: a member can mint, then create an agent, and attach/access its
// ports; authorized descendants work.
// ---------------------------------------------------------------------------

func TestProjectUAT_MemberMintsAttachBeforeFirstAgent(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-mint-before-agent-project")
	ownerID := tid("uatp-mint-before-agent-owner")
	memberID := tid("uatp-mint-before-agent-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)

	// Design doc "Resource-relative minting for #2092": "Do not enumerate
	// owned agents at mint time or require that one already exists. A
	// member can mint a token before creating their first agent."
	uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "attach-before-agent",
		projectID, []string{"agent:attach", "agent:port_access"}, nil)
	require.NoError(t, err, "member should be able to select attach/port_access before owning any agent")

	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatpExposePort(t, s, agent, 8080)

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assertAuthorizedPTY(t, rec, "member should pass attach authorization on their own newly created agent: %s", rec.Body.String())

	rec = doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/8080/proxy/", nil)
	assertAuthorizedPortProxy(t, rec, "member should pass port_access authorization on their own agent: %s", rec.Body.String())
}

func TestProjectUAT_AttachReachesAuthorizedDescendants(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-descendant-project")
	ownerID := tid("uatp-descendant-owner")
	memberID := tid("uatp-descendant-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)

	uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "attach-descendant",
		projectID, []string{"agent:attach", "agent:port_access"}, nil)
	require.NoError(t, err, "member should be able to mint agent:attach/agent:port_access for their own agents and descendants")

	parent := uatpAgent(t, s, projectID, memberID, t.Name()+"-parent", memberID)
	child := uatpAgent(t, s, projectID, parent.ID, t.Name()+"-child", memberID, parent.ID)
	uatpExposePort(t, s, child, 8090)

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+child.ID+"/pty", nil)
	assertAuthorizedPTY(t, rec, "attach should reach an authorized multi-hop descendant: %s", rec.Body.String())

	rec = doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+child.ID+"/ports/8090/proxy/", nil)
	assertAuthorizedPortProxy(t, rec, "port_access should reach an authorized multi-hop descendant: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Acceptance: missing scope, another user's target, another project, and
// removed project access all deny.
// ---------------------------------------------------------------------------

func TestProjectUAT_OwnedAgentRequiresSelectedScope(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-scope-project")
	ownerID := tid("uatp-scope-owner")
	memberID := tid("uatp-scope-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatpExposePort(t, s, agent, 9090)

	t.Run("read-only token cannot attach", func(t *testing.T) {
		// agent:read is carried by the stock member role (seed.go), so
		// this mint succeeds via the flat project-role path.
		uatKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "agent:read token must not authorize attach: %s", rec.Body.String())
	})

	t.Run("attach-only token cannot reach ports", func(t *testing.T) {
		uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "attach-only",
			projectID, []string{"agent:attach"}, nil)
		require.NoError(t, err, "the owner's own agent:attach selector is relationship-eligible and mints")
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/9090/proxy/", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "attach must not imply port_access: %s", rec.Body.String())
	})

	t.Run("port-only token cannot attach", func(t *testing.T) {
		uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "port-only",
			projectID, []string{"agent:port_access"}, nil)
		require.NoError(t, err, "the owner's own agent:port_access selector is relationship-eligible and mints")
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "port_access must not imply attach: %s", rec.Body.String())
	})
}

func TestProjectUAT_AttachDeniedForOtherMembersAgents(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-crossmember-project")
	ownerID := tid("uatp-crossmember-owner")
	adminID := tid("uatp-crossmember-admin")
	memberID := tid("uatp-crossmember-member")
	otherID := tid("uatp-crossmember-other")
	createRS1Project(t, s, projectID, ownerID)
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)
	ensureHubMembership(context.Background(), s, adminID)
	uatpMember(t, s, projectID, memberID)
	uatpMember(t, s, projectID, otherID)

	otherAgent := uatpAgent(t, s, projectID, otherID, t.Name(), otherID)

	cases := []struct {
		name   string
		userID string
	}{
		{"member", memberID},
		{"project-admin", adminID},
		{"project-owner", ownerID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(tc.userID), tc.userID, "attach-cross",
				projectID, []string{"agent:attach"}, nil)
			require.NoError(t, err, "%s should be able to select attach for their own future agents", tc.name)
			rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+otherAgent.ID+"/pty", nil)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"%s must not attach to another member's agent: %s", tc.name, rec.Body.String())
		})
	}
}

// TestProjectUAT_OwnerAdminPortAccessOnMembersAgent pins use-time behaviour
// of owner and admin user access tokens on another member's agent. The
// project-owner and project-admin roles carry agent.port_access, so an
// agent:port_access token opens the member's already-exposed ports. The same
// token cannot attach, manage ports, open the tunnel, exec or read env; an
// agent:attach token stays denied on the member's agent; a token without
// agent:port_access (agent:manage excludes it) cannot open ports; and
// another member's port_access token is refused.
func TestProjectUAT_OwnerAdminPortAccessOnMembersAgent(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-oversight-project")
	ownerID := tid("uatp-oversight-owner")
	adminID := tid("uatp-oversight-admin")
	memberID := tid("uatp-oversight-member")
	otherID := tid("uatp-oversight-other")
	createRS1Project(t, s, projectID, ownerID)
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)
	ensureHubMembership(context.Background(), s, adminID)
	uatpMember(t, s, projectID, memberID)
	uatpMember(t, s, projectID, otherID)

	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatpExposePort(t, s, agent, 8080)
	base := "/api/v1/agents/" + agent.ID

	for _, tc := range []struct{ name, userID string }{
		{"project-owner", ownerID},
		{"project-admin", adminID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			portKey, _, err := srv.uatService.CreateToken(rs4MintContext(tc.userID), tc.userID, "oversight-ports",
				projectID, []string{"agent:port_access"}, nil)
			require.NoError(t, err)

			rec := doRequestWithUAT(t, srv, portKey, http.MethodGet, base+"/ports/8080/proxy/", nil)
			assertAuthorizedPortProxy(t, rec, "%s port_access token should open a member's exposed port: %s", tc.name, rec.Body.String())

			rec = doRequestWithUAT(t, srv, portKey, http.MethodGet, base+"/pty", nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s port_access token must not attach: %s", tc.name, rec.Body.String())

			for _, m := range []struct{ method, path string }{
				{http.MethodPost, base + "/ports"},
				{http.MethodDelete, base + "/ports"},
				{http.MethodDelete, base + "/ports/8080"},
			} {
				rec := doRequestWithUAT(t, srv, portKey, m.method, m.path, map[string]any{"port": 8080})
				assert.Equal(t, http.StatusForbidden, rec.Code,
					"%s port_access token must not manage a member's ports (%s %s): %s", tc.name, m.method, m.path, rec.Body.String())
			}

			req := httptest.NewRequest(http.MethodGet, base+"/ports/tunnel", nil)
			req.Header.Set("Authorization", "Bearer "+portKey)
			req.Header.Set("Connection", "Upgrade")
			req.Header.Set("Upgrade", "websocket")
			req.Header.Set("Sec-WebSocket-Version", "13")
			req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
			tunnel := httptest.NewRecorder()
			srv.Handler().ServeHTTP(tunnel, req)
			assert.Equal(t, http.StatusForbidden, tunnel.Code, "%s port_access token must not open a member's port tunnel: %s", tc.name, tunnel.Body.String())

			for _, action := range []string{"exec", "env"} {
				rec := doRequestWithUAT(t, srv, portKey, http.MethodPost, base+"/"+action, map[string]any{})
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s port_access token must not %s a member's agent: %s", tc.name, action, rec.Body.String())
			}

			attachKey, _, err := srv.uatService.CreateToken(rs4MintContext(tc.userID), tc.userID, "oversight-attach",
				projectID, []string{"agent:attach"}, nil)
			require.NoError(t, err, "agent:attach is relationship-eligible and mints")
			rec = doRequestWithUAT(t, srv, attachKey, http.MethodGet, base+"/pty", nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s attach token must not attach to a member's agent: %s", tc.name, rec.Body.String())
			for _, action := range []string{"exec", "env"} {
				rec := doRequestWithUAT(t, srv, attachKey, http.MethodPost, base+"/"+action, map[string]any{})
				assert.Equal(t, http.StatusForbidden, rec.Code, "%s attach token must not %s a member's agent: %s", tc.name, action, rec.Body.String())
			}

			// agent:manage excludes port access. project-admin lacks
			// agent.delete and so cannot mint agent:manage; it uses the
			// agent scopes it does carry instead.
			otherScopes := []string{store.UATScopeAgentManage}
			if tc.userID == adminID {
				otherScopes = []string{"agent:read", "agent:lifecycle", "agent:message"}
			}
			otherKey := mintScopedUAT(t, srv, tc.userID, projectID, otherScopes)
			rec = doRequestWithUAT(t, srv, otherKey, http.MethodGet, base+"/ports/8080/proxy/", nil)
			assert.Equal(t, http.StatusForbidden, rec.Code, "%s token without agent:port_access must not open ports: %s", tc.name, rec.Body.String())
		})
	}

	t.Run("other member", func(t *testing.T) {
		portKey, _, err := srv.uatService.CreateToken(rs4MintContext(otherID), otherID, "other-ports",
			projectID, []string{"agent:port_access"}, nil)
		require.NoError(t, err, "agent:port_access is relationship-eligible and mints")
		rec := doRequestWithUAT(t, srv, portKey, http.MethodGet, base+"/ports/8080/proxy/", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "a member must not open another member's port: %s", rec.Body.String())
	})
}

func TestProjectUAT_AttachConfinedToTokenProject(t *testing.T) {
	srv, s := testServer(t)
	projectP := tid("uatp-confine-p")
	projectQ := tid("uatp-confine-q")
	ownerP := tid("uatp-confine-owner-p")
	ownerQ := tid("uatp-confine-owner-q")
	memberID := tid("uatp-confine-member")
	createRS1Project(t, s, projectP, ownerP)
	createRS1Project(t, s, projectQ, ownerQ)
	uatpMember(t, s, projectP, memberID)
	uatpMember(t, s, projectQ, memberID)

	uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "attach-p",
		projectP, []string{"agent:attach"}, nil)
	require.NoError(t, err, "the owner's own agent:attach selector is relationship-eligible and mints")

	ownAgentInQ := uatpAgent(t, s, projectQ, memberID, t.Name(), memberID)
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+ownAgentInQ.ID+"/pty", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a project-P token must not reach the member's own agent in project Q: %s", rec.Body.String())
}

// TestProjectUAT_RequiresActiveProjectAccessAtUse pins that use-time active
// project membership is rechecked on every request (ptone/scion#2092). Each
// subcase mints "agent:read" (already carried by the stock member role, so
// the mint itself does not depend on CanMintSelector's relationship-eligible
// path) and then removes project access through a different mechanism, to
// isolate this from the separate mint-eligibility coverage in the
// attach/port_access tests above.
func TestProjectUAT_RequiresActiveProjectAccessAtUse(t *testing.T) {
	t.Run("direct binding deleted", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("uatp-removed-direct-project")
		ownerID := tid("uatp-removed-direct-owner")
		memberID := tid("uatp-removed-direct-member")
		createRS1Project(t, s, projectID, ownerID)
		uatpMember(t, s, projectID, memberID)
		agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)

		uatKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "sanity: member can read own agent before removal")

		uatpDeleteProjectBinding(t, s, memberID, projectID)

		rec = doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		// The owner relationship grant (authz.go's checkRelationshipGrants)
		// has no project-membership precondition of its own -- it is
		// ProjectTargetAdmission, evaluated first in enforceUATConstraints,
		// that denies here regardless of retained ownership.
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"removed project access should deny a project UAT even on the holder's own agent: %s", rec.Body.String())
	})

	t.Run("group membership removed", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		projectID := tid("uatp-removed-group-project")
		ownerID := tid("uatp-removed-group-owner")
		memberID := tid("uatp-removed-group-member")
		groupID := tid("uatp-removed-group-grp")
		createRS1Project(t, s, projectID, ownerID)
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: memberID, Email: memberID + "@test.com", DisplayName: "Member", Role: "member", Status: "active",
		}))
		ensureHubMembership(ctx, s, memberID)
		require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupID, Slug: "uatp-removed-group", Name: "UATP Removed Group"}))
		rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: groupID,
			ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
		})
		require.NoError(t, err)
		require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: memberID, Role: store.GroupMemberRoleMember,
		}))

		agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
		uatKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "sanity: group-granted access works before removal")

		require.NoError(t, s.RemoveGroupMember(ctx, groupID, store.GroupMemberTypeUser, memberID))

		rec = doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"removing the group membership that carried project access should deny use-time access: %s", rec.Body.String())
	})

	t.Run("binding expired", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		projectID := tid("uatp-removed-expired-project")
		ownerID := tid("uatp-removed-expired-owner")
		memberID := tid("uatp-removed-expired-member")
		createRS1Project(t, s, projectID, ownerID)
		uatpMember(t, s, projectID, memberID)
		agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)

		uatKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, "sanity: active binding works before expiry")

		// Model the binding lapsing: delete the active binding and replace
		// it with an otherwise-identical one whose ExpiresAt is in the
		// past (there is no RoleBinding update method; role bindings are
		// immutable once created).
		uatpDeleteProjectBinding(t, s, memberID, projectID)
		rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
		require.NoError(t, err)
		past := time.Now().Add(-time.Hour)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: memberID,
			ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test", ExpiresAt: &past,
		})
		require.NoError(t, err)

		rec = doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"an expired binding must not count as active project access: %s", rec.Body.String())
	})

	t.Run("ancestry retained on agent row", func(t *testing.T) {
		srv, s := testServer(t)
		projectID := tid("uatp-removed-ancestry-project")
		ownerID := tid("uatp-removed-ancestry-owner")
		memberID := tid("uatp-removed-ancestry-member")
		createRS1Project(t, s, projectID, ownerID)
		uatpMember(t, s, projectID, memberID)
		parent := uatpAgent(t, s, projectID, memberID, t.Name()+"-parent", memberID)
		child := uatpAgent(t, s, projectID, memberID, t.Name()+"-child", memberID, parent.ID)

		uatKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
		uatpDeleteProjectBinding(t, s, memberID, projectID)

		// The agent row's Ancestry/OwnerID still names the member; that
		// historical fact must not substitute for current project access
		// (design doc: "retained creation ancestry alone cannot authorize
		// a UAT request after project access is removed").
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+child.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"retained ancestry must not substitute for current project access: %s", rec.Body.String())
	})
}

// TestProjectUAT_HubMembershipAloneDoesNotGrantProjectAccess pins that
// runtime project admission is project membership OR actual system
// authority for the EXACT requested permission/target -- not "the principal
// holds some system-scope permission that is merely applicable to project
// targets in general" (ptone/scion#2092).
//
// The seeded hub-members group (seed.go:462-480) gives every hub member a
// system-scoped hub-member role binding whose permission set
// (hubMemberPermissionIDs, seed.go:203-241) includes template.read/list,
// harness_config.read/list, skill.read/list, and quota.read -- none of them
// agent.*, but several reviewed "applies to an existing project target in
// general" in permissions.ProjectTargetApplicability. An aggregate
// implementation of the project-access gate ("holds any system permission
// applicable to some project target") would incorrectly treat hub
// membership alone as access to every project, since hub membership is
// close to universal among authenticated users. This test pins that
// SystemAuthorityProof instead requires the exact requested permission.
func TestProjectUAT_HubMembershipAloneDoesNotGrantProjectAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("uatp-hubonly-project")
	ownerID := tid("uatp-hubonly-owner")
	memberID := tid("uatp-hubonly-member")
	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: memberID, Email: memberID + "@test.com", DisplayName: "Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, memberID) // the only access source left standing, below

	// Grant project membership transiently, purely so a scoped UAT can be
	// minted against this project (the flat agent:read mint path still
	// requires an actual project-scoped permission, which is not itself
	// under test here) -- then revoke it. What remains is exactly the
	// seeded hub-members binding.
	createTestUserWithProjectRole(t, s, memberID, memberID+"@test.com", projectID, store.ProjectRoleMember)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
	uatpDeleteProjectBinding(t, s, memberID, projectID)

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub membership alone (no project binding, no agent.* system authority) must not satisfy project admission: %s", rec.Body.String())
}

// TestProjectUAT_ProjectAccessCheckedBeforeRelationshipGrants documents the
// gate placement: the project access check runs in Decide step 1, inside
// enforceUATConstraints, so a UAT without active project access is denied
// before the kernel or the owner/ancestor relationship grants (Decide step
// 9) ever run -- the response never comes from "relationship grant:
// resource owner" admitting a request project access should have blocked.
func TestProjectUAT_ProjectAccessCheckedBeforeRelationshipGrants(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-gateplacement-project")
	ownerID := tid("uatp-gateplacement-owner")
	memberID := tid("uatp-gateplacement-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)

	uatKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
	uatpDeleteProjectBinding(t, s, memberID, projectID)

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	// The UAT-only pre-kernel check in Decide step 1 denies this before the
	// owner relationship grant in step 9 ever runs.
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"project access must be checked before relationship grants fire: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Acceptance: current system authority still admits; flat scope-wide
// eligibility keeps the project-binding ceiling (ptone/scion#2092).
// ---------------------------------------------------------------------------

func TestProjectUAT_SystemAuthorityCountsAsProjectAccess(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-sysauth-project")
	ownerID := tid("uatp-sysauth-owner")
	superAdminID := tid("uatp-sysauth-admin")
	createRS1Project(t, s, projectID, ownerID)
	createTestUserWithRole(t, s, superAdminID, superAdminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	// Deliberately no project-scoped binding for superAdminID in projectID.

	t.Run("relationship-eligible selector is mintable without a project binding", func(t *testing.T) {
		// A super-admin's current system authority admits the project even
		// with no membership row, and agent:attach is relationship-eligible
		// (needs no existing target).
		_, _, err := srv.uatService.CreateToken(rs4MintContext(superAdminID), superAdminID, "sysauth-attach",
			projectID, []string{"agent:attach"}, nil)
		require.NoError(t, err, "system authority should admit a relationship-eligible selector without a project binding")
	})

	t.Run("flat scope-wide selector still requires the project-binding ceiling", func(t *testing.T) {
		// System authority must never inflate the flat project-role
		// ceiling (matches TestRS4_A2_SystemPermDoesNotInflateCeiling) --
		// only the denial reason should change from "no project access at
		// all" (ErrUATProjectForbidden) to "admitted, but this specific
		// flat selector exceeds the project role" (ErrUATScopeViolation).
		_, _, err := srv.uatService.CreateToken(rs4MintContext(superAdminID), superAdminID, "sysauth-delete",
			projectID, []string{"agent:delete"}, nil)
		assert.ErrorIs(t, err, ErrUATScopeViolation,
			"flat agent:delete must be denied as a scope violation, not as a blanket project-forbidden, once system authority admits the project")
	})
}

// ---------------------------------------------------------------------------
// Runtime project admission (ptone/scion#2092) is current membership OR a
// system grant applicable to the EXACT requested canonical permission AND
// the actual project target -- not "holds any system permission applicable
// to project targets in general," which the seeded hub-member/hub-admin
// roles would make vacuous. These four tests use real seeded roles (not
// synthetic minimal fixtures) to pin that requirement.
// ---------------------------------------------------------------------------

// TestProjectUAT_FormerMemberRetainedAncestryOnlyCatalogRoles: seeded
// catalog-only roles case (1). A former project member retains ONLY the
// seeded hub-member group binding
// plus a direct hub-viewer binding (covering "and viewer, if seeded") --
// both catalog-only system roles -- after their project membership is
// removed. Neither may restore attach, port access, or read on their former
// project's agents, even though the agent row's ancestry/ownership still
// names them.
func TestProjectUAT_FormerMemberRetainedAncestryOnlyCatalogRoles(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("uatp-formermember-project")
	ownerID := tid("uatp-formermember-owner")
	memberID := tid("uatp-formermember-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID) // hub-member group + project-member binding
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatpExposePort(t, s, agent, 6060)

	readKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
	attachKey, _, attachErr := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "former-attach",
		projectID, []string{"agent:attach"}, nil)
	portKey, _, portErr := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "former-port",
		projectID, []string{"agent:port_access"}, nil)

	// Add a direct hub-viewer binding too -- both catalog-only roles must
	// fail the project-access gate the same way.
	viewerRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubViewer, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: viewerRD.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: memberID,
		ScopeType: store.RoleScopeSystem, ScopeID: "", CreatedBy: "test",
	})
	require.NoError(t, err)

	// Remove project membership. Hub-member (group) and hub-viewer (direct)
	// bindings remain -- both catalog-only, neither carries agent.*.
	uatpDeleteProjectBinding(t, s, memberID, projectID)

	rec := doRequestWithUAT(t, srv, readKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"former member's catalog-only roles must not restore read: %s", rec.Body.String())

	require.NoError(t, attachErr, "the member's own agent:attach selector was relationship-eligible and minted before removal")
	rec = doRequestWithUAT(t, srv, attachKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"former member's catalog-only roles must not restore attach: %s", rec.Body.String())

	require.NoError(t, portErr, "the member's own agent:port_access selector was relationship-eligible and minted before removal")
	rec = doRequestWithUAT(t, srv, portKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/6060/proxy/", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"former member's catalog-only roles must not restore port access: %s", rec.Body.String())
}

// TestProjectUAT_HubAdminScheduledEventGrantsDoNotUnlockAgents: seeded
// catalog-only roles case (2). Hub-admin (seed.go hubAdminPermissionIDs)
// carries scheduled_event.* at
// system scope, all reviewed project-target-applicable in general -- but
// none of it is agent.*. A hub-admin with no project membership must not
// gain agent access through it.
func TestProjectUAT_HubAdminScheduledEventGrantsDoNotUnlockAgents(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-hubadmin-project")
	ownerID := tid("uatp-hubadmin-owner")
	hubAdminID := tid("uatp-hubadmin-admin")
	createRS1Project(t, s, projectID, ownerID)
	createTestUserWithRole(t, s, hubAdminID, hubAdminID+"@test.com", "admin", store.SystemRoleHubAdmin)

	// Grant project membership transiently to mint a token against this
	// project (the flat agent:read mint path is not itself under test
	// here), then revoke it. Only the system-scoped hub-admin binding
	// (scheduled_event.*, no agent.*) remains.
	createTestUserWithProjectRole(t, s, hubAdminID, hubAdminID+"@test.com", projectID, store.ProjectRoleMember)
	agent := uatpAgent(t, s, projectID, hubAdminID, t.Name(), hubAdminID)
	readKey := mintScopedUAT(t, srv, hubAdminID, projectID, []string{"agent:read"})
	uatpDeleteProjectBinding(t, s, hubAdminID, projectID)

	rec := doRequestWithUAT(t, srv, readKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"hub-admin's scheduled_event authority must not unlock an unrelated agent target: %s", rec.Body.String())
}

// TestProjectUAT_CatalogOnlySystemGrantsDoNotCountForAgentTargets: seeded
// catalog-only roles case (3). A system-scope role holding only
// skill.read/template.read/harness_config.read -- all reviewed
// project-target-applicable in general, none of them agent.* -- must not
// admit an agent target.
func TestProjectUAT_CatalogOnlySystemGrantsDoNotCountForAgentTargets(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("uatp-catalogonly-project")
	ownerID := tid("uatp-catalogonly-owner")
	catalogID := tid("uatp-catalogonly-user")
	createRS1Project(t, s, projectID, ownerID)

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: catalogID, Email: catalogID + "@test.com", DisplayName: "Catalog Only", Role: "member", Status: "active",
	}))
	rd := createTestRoleDefinition(t, s, "uatp-catalog-only-role", store.RoleScopeSystem,
		[]string{"skill.read", "template.read", "harness_config.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: catalogID,
		ScopeType: store.RoleScopeSystem, ScopeID: "", CreatedBy: "test",
	})
	require.NoError(t, err)

	createTestUserWithProjectRole(t, s, catalogID, catalogID+"@test.com", projectID, store.ProjectRoleMember)
	agent := uatpAgent(t, s, projectID, catalogID, t.Name(), catalogID)
	readKey := mintScopedUAT(t, srv, catalogID, projectID, []string{"agent:read"})
	uatpDeleteProjectBinding(t, s, catalogID, projectID)

	rec := doRequestWithUAT(t, srv, readKey, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"catalog-only system grants (skill/template/harness_config read) must not admit an agent target: %s", rec.Body.String())
}

// TestProjectUAT_SuperAdminNoMembershipCanMintAndAttach: seeded catalog-only
// roles case (4). A super-admin's system authority is the EXACT requested
// permission
// (agent.attach is a real, held permission via allPermissionIDs(), not
// merely "applicable in general"), so it must admit both minting and
// use-time attach with no project binding at all.
func TestProjectUAT_SuperAdminNoMembershipCanMintAndAttach(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-superadmin-project")
	ownerID := tid("uatp-superadmin-owner")
	superAdminID := tid("uatp-superadmin-admin")
	createRS1Project(t, s, projectID, ownerID)
	createTestUserWithRole(t, s, superAdminID, superAdminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	// Deliberately no project-scoped binding for superAdminID.

	uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(superAdminID), superAdminID, "superadmin-attach",
		projectID, []string{"agent:attach"}, nil)
	require.NoError(t, err, "super-admin's exact agent.attach authority should admit minting without a project binding")

	agent := uatpAgent(t, s, projectID, superAdminID, t.Name(), superAdminID)
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assertAuthorizedPTY(t, rec, "super-admin's exact-permission system authority should admit use-time attach without a project binding: %s", rec.Body.String())
}

// TestProjectUAT_PTYTicketPathFailsClosed pins that the /pty ticket
// fallback for browser clients (pty_handlers.go validatePTYTicket) is an
// unimplemented stub that always returns nil, so a request bearing only a
// ticket query parameter -- no Authorization header, no session cookie --
// has no identity at all and must fail closed with 401. Ticket
// storage/redemption is not implemented; this only pins the current
// fail-closed behavior so a future implementation cannot silently regress
// to fail-open.
func TestProjectUAT_PTYTicketPathFailsClosed(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-ticket-project")
	ownerID := tid("uatp-ticket-owner")
	createRS1Project(t, s, projectID, ownerID)
	agent := uatpAgent(t, s, projectID, ownerID, t.Name(), ownerID)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty?ticket=not-a-real-ticket", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code,
		"a request with only an unredeemable ticket parameter must fail closed: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Session characterization (ptone/scion#2141).
// ---------------------------------------------------------------------------

// TestSessionOwnerAttach_DeniedAfterProjectAccessRemoved pins the rule for
// interactive sessions: an owner/ancestor relationship grant on a project
// target requires the user's active project access at use time, so a
// session user whose project binding was removed is denied attach and
// port access on their own agent. The row-level coverage is in
// authz_relationship_project_access_test.go.
func TestSessionOwnerAttach_DeniedAfterProjectAccessRemoved(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-session-project")
	ownerID := tid("uatp-session-owner")
	memberID := tid("uatp-session-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatpExposePort(t, s, agent, 7070)

	memberUser, err := s.GetUser(context.Background(), memberID)
	require.NoError(t, err)

	rec := doRequestAsUser(t, srv, memberUser, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	requireAuthorizedPTY(t, rec, "sanity: session attaches to own agent before removal")

	uatpDeleteProjectBinding(t, s, memberID, projectID)

	// ptone/scion#2141: an interactive owner relationship on a project
	// target requires active project access at use time (relationship
	// stage 2c), the same as a project UAT.
	rec = doRequestAsUser(t, srv, memberUser, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"session owner attach requires active project access: %s", rec.Body.String())

	rec = doRequestAsUser(t, srv, memberUser, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/7070/proxy/", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"session owner port access requires active project access: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Acceptance: access constraints deny.
// ---------------------------------------------------------------------------

func TestProjectUAT_AccessConstraintsRestrictRelationshipAttach(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("uatp-constraint-project")
	ownerID := tid("uatp-constraint-owner")
	memberID := tid("uatp-constraint-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)
	uatpExposePort(t, s, agent, 6161)

	// Mint before any constraint exists, so the already-issued UAT carries
	// agent:attach/agent:port_access when the constraint below is added.
	uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "constrained-attach",
		projectID, []string{"agent:attach", "agent:port_access"}, nil)
	require.NoError(t, err, "attach/port_access mint should succeed before any constraint exists")

	recPTY := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	require.Equal(t, http.StatusUnprocessableEntity, recPTY.Code, "sanity: attach authorized before the constraint exists")
	recPort := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/6161/proxy/", nil)
	require.Equal(t, http.StatusServiceUnavailable, recPort.Code, "sanity: port access authorized before the constraint exists")

	// A constraint on the member restricts their MAXIMUM permissions in
	// this project to agent.read -- agent.attach and agent.port_access are
	// excluded. Project MEMBERSHIP is unaffected (ProjectMembershipEvidence
	// is permission-agnostic), so this exercises the kernel/relationship
	// restriction path (Decide step 7c), not the project-access gate.
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:                 "uatp-deny-attach",
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr("user"),
		SubjectPrincipalID:   pvStrPtr(memberID),
		ScopeType:            store.RoleScopeProject,
		ScopeID:              projectID,
		MaximumPermissions:   []string{"agent.read"},
		Purpose:              "uatp test: deny relationship attach",
		CreatedBy:            ownerID,
	})
	require.NoError(t, err)

	t.Run("mint is rejected once the constraint exists", func(t *testing.T) {
		_, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "constrained-attach-2",
			projectID, []string{"agent:attach"}, nil)
		assert.ErrorIs(t, err, ErrUATScopeViolation,
			"an access constraint excluding agent.attach must block minting it, even though attach is relationship-eligible: %v", err)
		assert.NotErrorIs(t, err, ErrUATProjectForbidden,
			"the member still has project access; denial must be the per-selector constraint, not the admission gate: %v", err)
		var violation *UATScopeViolationError
		if assert.ErrorAs(t, err, &violation) {
			assert.Equal(t, "agent:attach", violation.Selector)
		}
	})

	t.Run("already-minted attach UAT is denied at the real endpoint", func(t *testing.T) {
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "access constraint must deny attach at use time: %s", rec.Body.String())
	})

	t.Run("already-minted port-access UAT is denied at the real endpoint", func(t *testing.T) {
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/ports/6161/proxy/", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "access constraint must deny port access at use time: %s", rec.Body.String())
	})

	t.Run("session identity sees the same restriction (characterization, not UAT coverage)", func(t *testing.T) {
		// Access-constraint reduction already applies to relationship
		// grants for every credential kind, independent of the project-
		// access gate; this documents that a session identity is
		// restricted the same way, without standing in for the UAT
		// real-endpoint coverage above.
		owner := NewAuthenticatedUser(memberID, memberID+"@test.com", "Member", "member", "api")
		decision := srv.authzService.CheckAccess(ctx, owner, agentResource(agent), ActionAttach)
		assert.False(t, decision.Allowed, "access constraint must restrict the owner relationship grant's attach: %s", decision.Reason)
	})
}

// ---------------------------------------------------------------------------
// Acceptance: new attach-only token cannot start/stop agents.
// ---------------------------------------------------------------------------

func TestProjectUAT_AttachOnlyTokenCannotManageLifecycle(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-lifecycle-project")
	ownerID := tid("uatp-lifecycle-owner")
	memberID := tid("uatp-lifecycle-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)

	uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "lifecycle-attach",
		projectID, []string{"agent:attach"}, nil)
	require.NoError(t, err, "the owner's own agent:attach selector is relationship-eligible and mints")

	// enforceUATConstraints denies by exact scope match before
	// LegacyUATScopeImplications (agent:attach -> lifecycle) is ever
	// consulted (ruling ptone/scion#2092), so an attach-only token cannot
	// manage lifecycle actions.
	for _, action := range []string{"start", "stop", "restart"} {
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"attach-only token must not manage lifecycle (%s): %s", action, rec.Body.String())
	}

	// Control: exec is an attach-class action. A real command body is
	// required so the request reaches authorization instead of failing
	// request validation first (handleAgentExec rejects an empty command
	// with 400 before loading the agent or checking authorization at all,
	// so an empty body would pass this assertion even under a blanket
	// deny). Past authorization, the handler has no runtime broker to
	// dispatch to, so it returns 503 -- the same "authorized, no runtime"
	// signal used for /pty and port-proxy elsewhere in this file.
	rec := doRequestWithUAT(t, srv, uatKey, http.MethodPost, "/api/v1/agents/"+agent.ID+"/exec",
		map[string]any{"command": []string{"true"}})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"attach-only token should pass authorization for exec (no broker configured): %s", rec.Body.String())

	// Negative twin: an agent:read-only token must be denied the same exec
	// request, showing the 503 above comes from authorization succeeding
	// and not from a blanket allow that would mask a missing check.
	readOnlyKey := mintScopedUAT(t, srv, memberID, projectID, []string{"agent:read"})
	rec = doRequestWithUAT(t, srv, readOnlyKey, http.MethodPost, "/api/v1/agents/"+agent.ID+"/exec",
		map[string]any{"command": []string{"true"}})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"agent:read-only token must be denied exec: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Revocation takes effect on the next handshake.
// ---------------------------------------------------------------------------

func TestProjectUAT_AttachRecheckedOnEachHandshake(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-recheck-project")
	ownerID := tid("uatp-recheck-owner")
	memberID := tid("uatp-recheck-member")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	agent := uatpAgent(t, s, projectID, memberID, t.Name(), memberID)

	uatKey, token, err := srv.uatService.CreateToken(rs4MintContext(memberID), memberID, "recheck-attach",
		projectID, []string{"agent:attach"}, nil)
	require.NoError(t, err, "the owner's own agent:attach selector is relationship-eligible and mints")

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	requireAuthorizedPTY(t, rec, "preflight should be authorized before revocation: %s", rec.Body.String())

	require.NoError(t, srv.uatService.RevokeToken(rs4MintContext(memberID), memberID, token.ID))

	rec = doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "a revoked token must be rejected on the next handshake: %s", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Acceptance: stock project role attach/port_access lock-in; no dependency on B.
// ---------------------------------------------------------------------------

// TestProjectRoles_AttachAndPortAccessLockIn locks in the stock project
// role permission lists so a future edit cannot silently change cross-member
// attach/port_access. No stock role grants agent.attach. project-owner and
// project-admin grant agent.port_access (opening a member's already-exposed
// ports); project-member does not.
func TestProjectRoles_AttachAndPortAccessLockIn(t *testing.T) {
	// R6/R6/R5 added artifact.read and artifact.create.
	revisions := map[string]int{
		store.ProjectRoleOwner:  6,
		store.ProjectRoleAdmin:  6,
		store.ProjectRoleMember: 5,
	}
	portAccess := map[string]bool{
		store.ProjectRoleOwner: true,
		store.ProjectRoleAdmin: true,
	}
	for _, role := range BuiltInRoles() {
		if role.ScopeType != store.RoleScopeProject {
			continue
		}
		t.Run(role.Name, func(t *testing.T) {
			assert.NotContains(t, role.Permissions, "agent.attach",
				"no stock project role should grant agent.attach to other members' agents")
			if portAccess[role.Name] {
				assert.Contains(t, role.Permissions, "agent.port_access",
					"role %s should grant agent.port_access", role.Name)
			} else {
				assert.NotContains(t, role.Permissions, "agent.port_access",
					"role %s should not grant agent.port_access to other members' agents", role.Name)
			}
			if want, ok := revisions[role.Name]; ok {
				assert.Equal(t, want, role.Revision,
					"role %s revision must be bumped deliberately if its permission list changes", role.Name)
			}
		})
	}
}

// TestCanMintSelector_RelationshipEligibleWithoutTarget exercises
// AuthzService.CanMintSelector directly, covering the mint-eligibility unit
// contract CreateToken's mint path relies on: relationship-eligible
// selectors (agent:attach/agent:port_access) are mintable before any agent
// exists, a non-member gets the uniform MintDenialProjectAccessRequired
// reason for every requested selector in one call, and a flat-only selector
// the member's role doesn't carry gets MintDenialFlatRoleInsufficient.
func TestCanMintSelector_RelationshipEligibleWithoutTarget(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("uatp-canmint-project")
	ownerID := tid("uatp-canmint-owner")
	memberID := tid("uatp-canmint-member")
	nonMemberID := tid("uatp-canmint-nonmember")
	createRS1Project(t, s, projectID, ownerID)
	uatpMember(t, s, projectID, memberID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: nonMemberID, Email: nonMemberID + "@test.com", DisplayName: "Non Member", Role: "member", Status: "active",
	}))
	ensureHubMembership(ctx, s, nonMemberID)

	boundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}

	t.Run("owner eligible for attach and port_access without an existing agent", func(t *testing.T) {
		principal := PrincipalContext{Kind: PrincipalKindUser, ID: ownerID}
		results, err := srv.authzService.CanMintSelector(ctx, principal, boundary, []string{"agent:attach", "agent:port_access"})
		require.NoError(t, err)
		for _, r := range results {
			assert.True(t, r.OK, "owner should be eligible for %s (RequiresExistingTarget: false): reason %s", r.Selector, r.Reason)
		}
	})

	t.Run("member eligible for attach and port_access", func(t *testing.T) {
		principal := PrincipalContext{Kind: PrincipalKindUser, ID: memberID}
		results, err := srv.authzService.CanMintSelector(ctx, principal, boundary, []string{"agent:attach", "agent:port_access"})
		require.NoError(t, err)
		for _, r := range results {
			assert.True(t, r.OK, "member should be eligible for %s: reason %s", r.Selector, r.Reason)
		}
	})

	t.Run("non-member gets a uniform reason for every selector", func(t *testing.T) {
		principal := PrincipalContext{Kind: PrincipalKindUser, ID: nonMemberID}
		results, err := srv.authzService.CanMintSelector(ctx, principal, boundary, []string{"agent:attach", "agent:read"})
		require.NoError(t, err)
		require.Len(t, results, 2)
		for _, r := range results {
			assert.False(t, r.OK, "selector %s", r.Selector)
			assert.Equal(t, MintDenialProjectAccessRequired, r.Reason,
				"a non-member gets the same reason regardless of selector (oracle resistance): %s", r.Selector)
		}
	})

	t.Run("member ineligible for a flat permission their role does not hold", func(t *testing.T) {
		principal := PrincipalContext{Kind: PrincipalKindUser, ID: memberID}
		results, err := srv.authzService.CanMintSelector(ctx, principal, boundary, []string{"agent:delete"})
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.False(t, results[0].OK)
		assert.Equal(t, MintDenialFlatRoleInsufficient, results[0].Reason)
	})

	t.Run("unresolvable selector", func(t *testing.T) {
		principal := PrincipalContext{Kind: PrincipalKindUser, ID: memberID}
		results, err := srv.authzService.CanMintSelector(ctx, principal, boundary, []string{"not-a-real-selector"})
		require.NoError(t, err)
		require.Len(t, results, 1)
		assert.False(t, results[0].OK)
		assert.Equal(t, MintDenialUnknownSelector, results[0].Reason)
	})
}

// ---------------------------------------------------------------------------
// System-role runtime authority for the EXACT requested permission on a
// project target (ptone/scion#2092): permissions.ProjectTargetApplicability
// reviews group.read, gcp_service_account.read, and group.addMember true
// (ptone/scion#2117), so both the ALLOW and DENY subtests below run
// unskipped.
// ---------------------------------------------------------------------------

// TestUATProjectAdmission_SystemAuthorityForExactPermission covers
// group.read and gcp_service_account.read through the public
// AuthzService.CheckAccess/Decide entry point rather than a real HTTP
// route: neither GET /api/v1/groups/{id} (handlers_groups.go getGroup) nor
// GET /api/v1/projects/{id}/gcp-service-accounts/{id}
// (handlers_gcp_identity.go getGCPServiceAccount, whose own comment cites
// this) calls Decide/CheckAccess at all for a project-scoped read -- a
// pre-existing gap tracked as ptone/scion#598, unrelated to this task and
// out of scope to fix here. CheckAccess is the same public facade the
// pty/port/agent-GET tests in this file reach through Decide, so this
// coverage survives a future refactor of enforceUATConstraints's body.
func TestUATProjectAdmission_SystemAuthorityForExactPermission(t *testing.T) {
	cases := []struct {
		permissionID string
		uatScope     string
		action       Action
		resource     func(projectID string) Resource
	}{
		{
			permissionID: "group.read", uatScope: "group:read", action: ActionRead,
			resource: func(projectID string) Resource {
				return groupResource(&store.Group{ID: tid("uatp-exactperm-group"), ProjectID: projectID})
			},
		},
		{
			permissionID: "gcp_service_account.read", uatScope: "gcp_service_account:read", action: ActionRead,
			resource: func(projectID string) Resource {
				return gcpServiceAccountResource(&store.GCPServiceAccount{
					ID: tid("uatp-exactperm-sa"), Scope: store.ScopeProject, ScopeID: projectID,
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.permissionID, func(t *testing.T) {
			t.Run("system-scope binding with the exact permission allows use", func(t *testing.T) {
				srv, s := testServer(t)
				ctx := context.Background()
				projectID := tid("uatp-exactperm-allow-project-" + tc.permissionID)
				ownerID := tid("uatp-exactperm-allow-owner-" + tc.permissionID)
				userID := tid("uatp-exactperm-allow-user-" + tc.permissionID)
				createRS1Project(t, s, projectID, ownerID)
				require.NoError(t, s.CreateUser(ctx, &store.User{
					ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
				}))
				grantPermissionViaRoleBinding(t, s, userID, tc.permissionID, store.RoleScopeSystem, "")

				user := NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "api")
				// The permission is hub-only: a project token is not
				// eligible for it.
				scoped := NewScopedUserIdentity(user, projectID, []string{tc.uatScope})
				decision := srv.authzService.CheckAccess(ctx, scoped, tc.resource(projectID), tc.action)
				assert.False(t, decision.Allowed)
				assert.Equal(t, "permission is not eligible for this token boundary", decision.Reason)

				// A hub token with the selector reaches the project target
				// through system authority for the exact permission.
				ceiling, ok := permissions.BuildCeilingFromSelectors([]string{tc.uatScope})
				require.True(t, ok)
				hub := NewScopedUserIdentityWithBoundaryAndDecoration(user, TokenBoundary{Kind: BoundaryKindHub}, []string{tc.uatScope}, "", ceiling, nil)
				decision = srv.authzService.CheckAccess(ctx, hub, tc.resource(projectID), tc.action)
				assert.True(t, decision.Allowed, "system authority for the exact permission should admit a project target: %s", decision.Reason)
			})

			t.Run("missing binding denies", func(t *testing.T) {
				srv, s := testServer(t)
				ctx := context.Background()
				projectID := tid("uatp-exactperm-nobind-project-" + tc.permissionID)
				ownerID := tid("uatp-exactperm-nobind-owner-" + tc.permissionID)
				userID := tid("uatp-exactperm-nobind-user-" + tc.permissionID)
				createRS1Project(t, s, projectID, ownerID)
				require.NoError(t, s.CreateUser(ctx, &store.User{
					ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
				}))

				scoped := NewScopedUserIdentity(NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "api"), projectID, []string{tc.uatScope})
				decision := srv.authzService.CheckAccess(ctx, scoped, tc.resource(projectID), tc.action)
				assert.False(t, decision.Allowed, "no authority at all must deny: %s", decision.Reason)
			})

			t.Run("system role holding a different permission denies", func(t *testing.T) {
				srv, s := testServer(t)
				ctx := context.Background()
				projectID := tid("uatp-exactperm-wrong-project-" + tc.permissionID)
				ownerID := tid("uatp-exactperm-wrong-owner-" + tc.permissionID)
				userID := tid("uatp-exactperm-wrong-user-" + tc.permissionID)
				createRS1Project(t, s, projectID, ownerID)
				require.NoError(t, s.CreateUser(ctx, &store.User{
					ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
				}))
				// A system role granting an unrelated permission must not
				// admit this different, unheld permission -- holding *some*
				// system authority is not enough.
				grantPermissionViaRoleBinding(t, s, userID, "skill.read", store.RoleScopeSystem, "")

				scoped := NewScopedUserIdentity(NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "api"), projectID, []string{tc.uatScope})
				decision := srv.authzService.CheckAccess(ctx, scoped, tc.resource(projectID), tc.action)
				assert.False(t, decision.Allowed, "holding a different system permission must not admit this one: %s", decision.Reason)
			})

			t.Run("access constraint denies despite the exact permission", func(t *testing.T) {
				srv, s := testServer(t)
				ctx := context.Background()
				projectID := tid("uatp-exactperm-constraint-project-" + tc.permissionID)
				ownerID := tid("uatp-exactperm-constraint-owner-" + tc.permissionID)
				userID := tid("uatp-exactperm-constraint-user-" + tc.permissionID)
				createRS1Project(t, s, projectID, ownerID)
				require.NoError(t, s.CreateUser(ctx, &store.User{
					ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
				}))
				grantPermissionViaRoleBinding(t, s, userID, tc.permissionID, store.RoleScopeSystem, "")
				_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
					Name:                 "uatp-deny-" + tc.permissionID,
					SubjectKind:          store.ConstraintSubjectPrincipal,
					SubjectPrincipalType: pvStrPtr("user"),
					SubjectPrincipalID:   pvStrPtr(userID),
					ScopeType:            store.RoleScopeProject,
					ScopeID:              projectID,
					MaximumPermissions:   []string{},
					Purpose:              "uatp test: deny " + tc.permissionID,
					CreatedBy:            ownerID,
				})
				require.NoError(t, err)

				scoped := NewScopedUserIdentity(NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "api"), projectID, []string{tc.uatScope})
				decision := srv.authzService.CheckAccess(ctx, scoped, tc.resource(projectID), tc.action)
				assert.False(t, decision.Allowed, "a governing constraint must deny even with the exact permission held: %s", decision.Reason)
			})
		})
	}
}

// TestUATProjectAdmission_CrossPermissionMemoIsolation pins that project
// admission for one permission (group.read) does not leak into admission
// for a different permission (agent.read, agent.attach, agent.port_access)
// on the same project, even when both checks share the same request-scoped
// ProjectAdmissionCache -- the cache key
// (authz_boundary.go projectAdmissionCacheKey) includes the permission ID,
// so this is a regression test pinning that key shape rather than assuming
// it safe by inspection alone.
func TestUATProjectAdmission_CrossPermissionMemoIsolation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("uatp-memo-isolation-project")
	ownerID := tid("uatp-memo-isolation-owner")
	userID := tid("uatp-memo-isolation-user")
	createRS1Project(t, s, projectID, ownerID)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
	}))
	// System authority for the exact permission group.read only -- no
	// agent.* permission at all, and no project membership.
	grantPermissionViaRoleBinding(t, s, userID, "group.read", store.RoleScopeSystem, "")

	principal := PrincipalContext{Kind: PrincipalKindUser, ID: userID}
	memo := NewProjectAdmissionCache()

	groupClass := ProjectTargetClass{ResourceType: "group"}
	_, err := srv.authzService.ProjectAdmissionForClass(ctx, principal, projectID, "group.read", groupClass, memo)
	require.NoError(t, err)
	// (The group.read result itself is not asserted here -- whichever way
	// it resolves, this test's only concern is that the memo does not leak
	// across permissions.)

	agentClass := ProjectTargetClass{ResourceType: "agent"}
	for _, permID := range []string{"agent.read", "agent.attach", "agent.port_access"} {
		result, err := srv.authzService.ProjectAdmissionForClass(ctx, principal, projectID, permID, agentClass, memo)
		require.NoError(t, err)
		assert.False(t, result.Admitted,
			"admission for group.read must not leak into %s via the shared request-scoped memo: source %s", permID, result.Source)
	}
}

// TestProjectUAT_GroupAddMemberExactSystemPermissionAtRealRoute exercises
// the group family through a real HTTP route + middleware (POST
// /api/v1/groups/{id}/members -> addGroupMember -> s.authorize -> Decide),
// rather than a direct CheckAccess call, so this coverage survives a future
// refactor that moves enforceUATConstraints's body elsewhere.
//
// group.addMember is hub-only (permissions.PermissionAllowedBoundaries).
// A project token carrying it, inserted directly as a stored row
// (uatpInsertLegacyToken), is not eligible for the permission's boundary
// and is denied at use time. A hub token minted with the selector is
// admitted through system authority for the exact permission. This also
// doubles as the group family's real-HTTP exercise.
func TestProjectUAT_GroupAddMemberExactSystemPermissionAtRealRoute(t *testing.T) {
	t.Run("system-scope binding admits a hub token and not a project token", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()
		projectID := tid("uatp-groupaddmember-project")
		ownerID := tid("uatp-groupaddmember-owner")
		userID := tid("uatp-groupaddmember-user")
		targetID := tid("uatp-groupaddmember-target")
		createRS1Project(t, s, projectID, ownerID)
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
		}))
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: targetID, Email: targetID + "@test.com", DisplayName: "Target", Role: "member", Status: "active",
		}))
		grantPermissionViaRoleBinding(t, s, userID, "group.addMember", store.RoleScopeSystem, "")
		// OwnerID is set to userID so this request also clears
		// addGroupMember's separate group-ownership role-hierarchy guard
		// (handlers_groups.go: "Only group owners or admins can add
		// members"), which is unrelated to the project-access question this
		// test is actually about.
		group := &store.Group{
			ID: tid("uatp-groupaddmember-group"), Slug: "uatp-groupaddmember-group", Name: "G",
			ProjectID: projectID, OwnerID: userID,
		}
		require.NoError(t, s.CreateGroup(ctx, group))

		// group.addMember is hub-only: a project token carrying it is not
		// eligible for the permission's boundary.
		uatKey := uatpInsertLegacyToken(t, s, userID, projectID, []string{"group:addMember"})
		rec := doRequestWithUAT(t, srv, uatKey, http.MethodPost, "/api/v1/groups/"+group.ID+"/members",
			map[string]any{"memberType": "user", "memberId": targetID, "role": "member"})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"a project token is not eligible for a hub-only permission: %s", rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "Only group owners or admins can add members")
		projectToken, err := srv.uatService.ValidateToken(ctx, uatKey)
		require.NoError(t, err)
		decision := srv.authzService.CheckAccess(ctx, projectToken, groupResource(group), ActionAddMember)
		assert.False(t, decision.Allowed)
		assert.Equal(t, bearerReasonBoundaryIneligible, decision.Reason)
		eval := srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(bearerUser(userID)), projectToken.Boundary(), projectToken.Ceiling(), "group.addMember", groupResource(group), BearerOptions{})
		assert.Equal(t, BearerStageBoundaryEligibility, eval.Stage, eval.Decision.Reason)

		// A hub token with the selector is admitted through system authority
		// for the exact permission.
		hubKey, _, err := srv.uatService.CreateTokenWithParams(rs4MintContext(userID), CreateTokenParams{
			UserID: userID, Name: "hub", Boundary: TokenBoundary{Kind: BoundaryKindHub}, Scopes: []string{"group:addMember"},
		})
		require.NoError(t, err)
		rec = doRequestWithUAT(t, srv, hubKey, http.MethodPost, "/api/v1/groups/"+group.ID+"/members",
			map[string]any{"memberType": "user", "memberId": targetID, "role": "member"})
		assert.Equal(t, http.StatusCreated, rec.Code,
			"system authority for the exact permission should admit a hub token: %s", rec.Body.String())
	})

	t.Run("hub token of a group owner without the system binding is denied at project access", func(t *testing.T) {
		// The fixture matches the positive case above (OwnerID == caller)
		// except that the caller holds no group.addMember system binding.
		// The token is a hub-boundary row carrying group:addMember, so it
		// passes the ceiling and boundary eligibility stages. The group is
		// homed in a project the caller is not a member of, so the bearer
		// gate denies at the project access stage, before group ownership
		// is considered by the kernel, relationship grants or the handler's
		// own role-hierarchy check. The response therefore must not carry
		// the handler's "Only group owners or admins can add members" text.
		srv, s := testServer(t)
		ctx := context.Background()
		projectID := tid("uatp-groupaddmember-nobind-project")
		ownerID := tid("uatp-groupaddmember-nobind-owner")
		userID := tid("uatp-groupaddmember-nobind-user")
		targetID := tid("uatp-groupaddmember-nobind-target")
		createRS1Project(t, s, projectID, ownerID)
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: userID + "@test.com", DisplayName: "User", Role: "member", Status: "active",
		}))
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: targetID, Email: targetID + "@test.com", DisplayName: "Target", Role: "member", Status: "active",
		}))
		group := &store.Group{
			ID: tid("uatp-groupaddmember-nobind-group"), Slug: "uatp-groupaddmember-nobind-group", Name: "G",
			ProjectID: projectID, OwnerID: userID,
		}
		require.NoError(t, s.CreateGroup(ctx, group))

		hubKey := uatpInsertHubToken(t, s, userID, []string{"group:addMember"})
		rec := doRequestWithUAT(t, srv, hubKey, http.MethodPost, "/api/v1/groups/"+group.ID+"/members",
			map[string]any{"memberType": "user", "memberId": targetID, "role": "member"})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"group ownership alone does not satisfy project access: %s", rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "Only group owners or admins can add members",
			"the bearer gate denies before the handler's role-hierarchy check: %s", rec.Body.String())
		hubToken, err := srv.uatService.ValidateToken(ctx, hubKey)
		require.NoError(t, err)
		require.Equal(t, BoundaryKindHub, hubToken.Boundary().Kind)
		eval := srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(bearerUser(userID)), hubToken.Boundary(), hubToken.Ceiling(), "group.addMember", groupResource(group), BearerOptions{})
		assert.False(t, eval.Decision.Allowed)
		assert.Equal(t, BearerStageProjectAccess, eval.Stage, eval.Decision.Reason)
	})
}

// TestProjectUAT_MintForbiddenIsOracleResistant pins that CanMintSelector's
// admission check is ONE check for the whole request, with a single reason
// (MintDenialProjectAccessRequired) whenever it fails -- CreateToken must
// map that reason to the existing, bare ErrUATProjectForbidden (no
// selector, no reason code in the response), so a non-member of a real
// project and a caller naming a project that does not exist at all get the
// byte-identical HTTP response (ptone/scion#2092). Neither case may be
// distinguishable from the other, from "the project doesn't exist," or from
// a per-selector eligibility denial, by the response alone: this asserts
// the exact error code and message rather than only the HTTP status, and
// checks the body carries neither the selector name nor the
// project_access_required reason code, so a future change that leaks either
// into the message is caught here rather than only by the RS4 tests that
// happen to assert the sentinel error type. Covers both a flat selector
// (agent:read) and a relationship-eligible selector (agent:attach), since
// the two take different internal paths inside CanMintSelector before
// reaching the same uniform admission failure.
func TestProjectUAT_MintForbiddenIsOracleResistant(t *testing.T) {
	for _, scope := range []string{"agent:read", "agent:attach"} {
		t.Run(scope, func(t *testing.T) {
			srv, s := testServer(t)
			ctx := context.Background()
			projectID := tid("uatp-oracle-project-" + scope)
			ownerID := tid("uatp-oracle-owner-" + scope)
			outsiderID := tid("uatp-oracle-outsider-" + scope)
			createRS1Project(t, s, projectID, ownerID)
			outsider := &store.User{
				ID: outsiderID, Email: outsiderID + "@test.com", DisplayName: "Outsider", Role: "member", Status: "active",
			}
			require.NoError(t, s.CreateUser(ctx, outsider))
			ensureHubMembership(ctx, s, outsiderID)

			body := map[string]any{"name": "oracle-test", "projectId": projectID, "scopes": []string{scope}}
			recNonMember := doRequestAsUser(t, srv, outsider, http.MethodPost, "/api/v1/auth/tokens", body)

			nonexistentProjectID := tid("uatp-oracle-nonexistent-" + scope)
			bodyNonexistent := map[string]any{"name": "oracle-test-2", "projectId": nonexistentProjectID, "scopes": []string{scope}}
			recNonexistent := doRequestAsUser(t, srv, outsider, http.MethodPost, "/api/v1/auth/tokens", bodyNonexistent)

			require.Equal(t, http.StatusForbidden, recNonMember.Code, "non-member mint: %s", recNonMember.Body.String())
			assert.Equal(t, recNonMember.Code, recNonexistent.Code,
				"non-member and nonexistent-project mint must return the same HTTP status")
			assert.JSONEq(t, recNonMember.Body.String(), recNonexistent.Body.String(),
				"non-member and nonexistent-project mint must return the byte-identical error body")

			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(recNonMember.Body.Bytes(), &resp))
			assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
			assert.Equal(t, "forbidden", resp.Error.Message)
			assert.Nil(t, resp.Error.Details)
			assert.NotContains(t, recNonMember.Body.String(), scope,
				"the oracle-resistant response must not name the selector")
			assert.NotContains(t, recNonMember.Body.String(), string(MintDenialProjectAccessRequired),
				"the oracle-resistant response must not name the internal reason code")
		})
	}
}

// TestProjectUAT_EnforceUATConstraintsFailsClosedOnUnsupportedPrincipal pins
// the fail-closed branch in enforceUATConstraints: any error from
// ProjectTargetAdmission (including ErrUnsupportedPrincipalKind for a
// principal kind other than PrincipalKindUser/PrincipalKindDev) denies, the
// same as an explicit !admission.Admitted. This kind mismatch cannot occur
// for a real ScopedUserIdentity today (Decide only reaches
// enforceUATConstraints for CredentialKindUAT, which is always a local
// user), but the gate must still fail closed rather than panic or silently
// admit if principal resolution is ever widened.
func TestProjectUAT_EnforceUATConstraintsFailsClosedOnUnsupportedPrincipal(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	projectID := tid("uatp-failclosed-project")
	ownerID := tid("uatp-failclosed-owner")
	createRS1Project(t, s, projectID, ownerID)
	agent := uatpAgent(t, s, projectID, ownerID, t.Name(), ownerID)

	scoped := NewScopedUserIdentity(NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "api"), projectID, []string{"agent:read"})
	principal := PrincipalContext{Kind: PrincipalKindAgent, ID: ownerID}

	decision := srv.authzService.enforceUATConstraints(ctx, principal, scoped, agentResource(agent), ActionRead, "agent.read")
	require.NotNil(t, decision, "an unsupported principal kind must deny, not pass through as nil")
	assert.False(t, decision.Allowed)
	assert.Equal(t, "token holder lacks active access to the target project", decision.Reason)
	assert.False(t, decision.IsIndeterminate(),
		"an unsupported principal kind is a policy fact, not a store fault, so it must not be tagged as a resolution error")
}

// TestProjectUAT_EnforceUATConstraintsFailsClosedOnConstraintLoadError pins
// the same fail-closed contract as
// TestProjectUAT_EnforceUATConstraintsFailsClosedOnUnsupportedPrincipal for a
// realistic error source: a transient access-constraint-table load failure
// inside SystemAuthorityProof, reached from ProjectTargetAdmission when the
// UAT holder has no project membership and relies on system authority (the
// same fixture as TestProjectUAT_SuperAdminNoMembershipCanMintAndAttach).
// Two layers are pinned here, deliberately kept separate: the HTTP assertion
// exercises the request end-to-end and denies via defence in depth (the
// kernel's own constraint load also hits the injected failure), while the
// direct enforceUATConstraints call at the end pins the specific branch this
// test is named for -- that ProjectTargetAdmission's own error, and not just
// its Admitted field, is what enforceUATConstraints treats as a denial.
func TestProjectUAT_EnforceUATConstraintsFailsClosedOnConstraintLoadError(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-failclosed-cle-project")
	ownerID := tid("uatp-failclosed-cle-owner")
	superAdminID := tid("uatp-failclosed-cle-admin")
	createRS1Project(t, s, projectID, ownerID)
	createTestUserWithRole(t, s, superAdminID, superAdminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	// Deliberately no project-scoped binding for superAdminID: the use-time
	// gate below must go through SystemAuthorityProof, not
	// ProjectMembershipEvidence, so it actually reaches constraint loading.

	uatKey, _, err := srv.uatService.CreateToken(rs4MintContext(superAdminID), superAdminID, "cle-attach",
		projectID, []string{"agent:attach"}, nil)
	require.NoError(t, err, "super-admin's exact agent.attach authority should admit minting without a project binding")

	agent := uatpAgent(t, s, projectID, superAdminID, t.Name(), superAdminID)

	// Sanity: without any injected failure, the request is authorized.
	sanity := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assertAuthorizedPTY(t, sanity, "sanity check before injecting the failure: %s", sanity.Body.String())

	failing := &r2FailingStore{failListConstraints: fmt.Errorf("injected: constraint load failure")}
	restore := installFailStore(srv, failing)
	defer restore()

	rec := doRequestWithUAT(t, srv, uatKey, http.MethodGet, "/api/v1/agents/"+agent.ID+"/pty", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"a transient access-constraint load failure inside the use-time gate must deny, not silently pass through: %s", rec.Body.String())

	// The HTTP assertion above is satisfied by the kernel's own constraint
	// load hitting the same injected failure independently, so it alone does
	// not pin enforceUATConstraints's own ProjectTargetAdmission-error branch.
	// Call the gate directly, with the failing store still installed, to pin
	// that specific branch: it denies, and because the error is a store
	// fault the deny is tagged as a resolution error (IsIndeterminate).
	scoped := NewScopedUserIdentity(NewAuthenticatedUser(superAdminID, superAdminID+"@test.com", "Admin", "admin", "api"), projectID, []string{"agent:attach"})
	decision := srv.authzService.enforceUATConstraints(context.Background(), principalContextForIdentity(scoped), scoped, agentResource(agent), ActionAttach, "agent.attach")
	require.NotNil(t, decision, "step-1 gate must deny on a ProjectTargetAdmission error")
	assert.False(t, decision.Allowed)
	assert.Equal(t, "token holder lacks active access to the target project", decision.Reason)
	assert.Equal(t, DenyCauseResolutionError, decision.DenyCause)
	assert.True(t, decision.IsIndeterminate(), "a store fault in the live project-access lookup must be reported as indeterminate")
}

// TestProjectUAT_EnforceUATConstraintsBindingLookupErrorIsIndeterminate pins
// the resolution-error tag on the membership path: a project member's UAT,
// with the role-binding lookup failing inside ProjectMembershipEvidence,
// denies (fail closed) and the deny is tagged DenyCauseResolutionError.
func TestProjectUAT_EnforceUATConstraintsBindingLookupErrorIsIndeterminate(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-bindfail-project")
	ownerID := tid("uatp-bindfail-owner")
	createRS1Project(t, s, projectID, ownerID)
	agent := uatpAgent(t, s, projectID, ownerID, t.Name(), ownerID)

	scoped := NewScopedUserIdentity(NewAuthenticatedUser(ownerID, ownerID+"@test.com", "Owner", "member", "api"), projectID, []string{"agent:attach"})
	principal := principalContextForIdentity(scoped)

	// Sanity: without an injected failure the gate admits.
	require.Nil(t, srv.authzService.enforceUATConstraints(context.Background(), principal, scoped, agentResource(agent), ActionAttach, "agent.attach"),
		"sanity: a project owner's attach-scoped UAT should pass the gate")

	failing := &r2FailingStore{failListBindings: fmt.Errorf("injected: binding lookup failure")}
	restore := installFailStore(srv, failing)
	defer restore()

	decision := srv.authzService.enforceUATConstraints(context.Background(), principal, scoped, agentResource(agent), ActionAttach, "agent.attach")
	require.NotNil(t, decision, "a binding lookup failure must deny")
	assert.False(t, decision.Allowed)
	assert.Equal(t, DenyCauseResolutionError, decision.DenyCause)
	assert.True(t, decision.IsIndeterminate())
}

// TestProjectUAT_EnforceUATConstraintsMissingUserIsPolicyDeny pins that a
// UAT whose holder has no user record denies at the live project-access
// gate as a policy deny, the same as an inactive user: GetUser returning
// store.ErrNotFound is not a store fault, so the deny is not tagged
// DenyCauseResolutionError and IsIndeterminate reports false.
func TestProjectUAT_EnforceUATConstraintsMissingUserIsPolicyDeny(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-missinguser-project")
	ownerID := tid("uatp-missinguser-owner")
	missingID := tid("uatp-missinguser-holder")
	createRS1Project(t, s, projectID, ownerID)
	agent := uatpAgent(t, s, projectID, ownerID, t.Name(), ownerID)

	_, err := s.GetUser(context.Background(), missingID)
	require.ErrorIs(t, err, store.ErrNotFound, "fixture: the holder must have no user record")

	scoped := NewScopedUserIdentity(NewAuthenticatedUser(missingID, missingID+"@test.com", "Missing", "member", "api"), projectID, []string{"agent:attach"})
	principal := principalContextForIdentity(scoped)

	_, _, evidenceErr := srv.authzService.ProjectMembershipEvidence(context.Background(), principal, projectID)
	require.ErrorIs(t, evidenceErr, ErrProjectAccessDenied)
	assert.False(t, isProjectAccessLookupFault(evidenceErr), "a missing user must not be classified as a lookup fault")

	decision := srv.authzService.enforceUATConstraints(context.Background(), principal, scoped, agentResource(agent), ActionAttach, "agent.attach")
	require.NotNil(t, decision, "a missing holder must deny")
	assert.False(t, decision.Allowed)
	assert.Equal(t, "token holder lacks active access to the target project", decision.Reason)
	assert.Empty(t, decision.DenyCause)
	assert.False(t, decision.IsIndeterminate(), "a missing user is a policy deny, not an indeterminate result")

	full := srv.authzService.CheckAccess(context.Background(), scoped, agentResource(agent), ActionAttach)
	assert.False(t, full.Allowed)
	assert.False(t, full.IsIndeterminate(), "end-to-end decision for a missing holder must not be indeterminate")
}

// TestProjectUAT_CreateTokenMapsCanMintSelectorErrorToForbidden pins the
// fail-closed mapping at CreateToken's own call site: when CanMintSelector
// itself returns an error (as opposed to a per-selector denial), CreateToken
// must map it to the same bare, oracle-resistant ErrUATProjectForbidden used
// for "no admission at all" -- never a different, more specific error that
// would let a caller distinguish "the check itself failed" from "the check
// ran and denied." Uses the same injected access-constraint-table load
// failure that TestCanMintSelector_ConstraintLoadErrorReturnsError/
// project_boundary_flat_role_descriptor pins at the CanMintSelector level
// directly, here observed through the actual mint call site and a real HTTP
// request.
func TestProjectUAT_CreateTokenMapsCanMintSelectorErrorToForbidden(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-cle-mint-project")
	ownerID := tid("uatp-cle-mint-owner")
	adminID := tid("uatp-cle-mint-admin")
	createRS1Project(t, s, projectID, ownerID)
	// project-admin carries agent.delete, a permission with no
	// MintEligibilityRegistry descriptor (flat-role default), so mint
	// eligibility reaches hasProjectRoleFlatPermission, which loads the
	// access-constraint table.
	createTestUserWithProjectRole(t, s, adminID, adminID+"@test.com", projectID, store.ProjectRoleAdmin)
	ensureHubMembership(context.Background(), s, adminID)

	failing := &r2FailingStore{failListConstraints: fmt.Errorf("injected: constraint load failure")}
	restore := installFailStore(srv, failing)
	defer restore()

	_, _, err := srv.uatService.CreateToken(rs4MintContext(adminID), adminID, "cle-mint",
		projectID, []string{"agent:delete"}, nil)
	require.ErrorIs(t, err, ErrUATProjectForbidden,
		"a CanMintSelector error must map to the bare, oracle-resistant ErrUATProjectForbidden, not a distinguishable error")

	adminUser, getErr := s.GetUser(context.Background(), adminID)
	require.NoError(t, getErr)
	body := map[string]any{"name": "cle-mint-http", "projectId": projectID, "scopes": []string{"agent:delete"}}
	rec := doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/auth/tokens", body)
	require.Equal(t, http.StatusForbidden, rec.Code, "mint over HTTP: %s", rec.Body.String())

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeForbidden, resp.Error.Code)
	assert.Equal(t, "forbidden", resp.Error.Message)
	assert.Nil(t, resp.Error.Details,
		"a CanMintSelector-level error must produce the same detail-free body as any other admission failure")
}

// TestProjectUAT_CreateTokenMissingIdentityIsForbidden pins that minting
// with an interactive credential but no usable identity in the context (nil
// or typed-nil) returns ErrUATProjectForbidden rather than panicking or
// reaching CanMintSelector with an empty principal. The session-credential
// check rejects this first; CreateTokenWithParams also checks the identity
// again right before the mint-eligibility call, so neither check depends on
// the other's ordering.
func TestProjectUAT_CreateTokenMissingIdentityIsForbidden(t *testing.T) {
	srv, s := testServer(t)
	projectID := tid("uatp-nilident-project")
	ownerID := tid("uatp-nilident-owner")
	createRS1Project(t, s, projectID, ownerID)

	cases := map[string]Identity{
		"nil identity":       nil,
		"typed-nil identity": (*AuthenticatedUser)(nil),
	}
	for name, identity := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := contextWithIdentity(context.Background(), identity)
			ctx = contextWithCredentialContext(ctx, CredentialContext{Kind: CredentialKindInteractive, ID: "test-session"})
			key, token, err := srv.uatService.CreateTokenWithParams(ctx, CreateTokenParams{
				UserID:    ownerID,
				Name:      "nil-identity",
				ProjectID: projectID,
				Scopes:    []string{"agent:attach"},
			})
			require.ErrorIs(t, err, ErrUATProjectForbidden)
			assert.Empty(t, key)
			assert.Nil(t, token)
		})
	}
}
