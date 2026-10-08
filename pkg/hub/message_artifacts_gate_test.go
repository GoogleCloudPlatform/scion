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

// resolveArtifactRefs resolves only for a request-authenticated caller:
// the credential context recorded by the authentication middleware must
// bind the context's current identity (requestCredentialBindsIdentity).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// middlewareCtx builds ctx exactly as each authentication middleware arm
// does: the identity first, then the credential context derived from it.
func middlewareCtx(identity Identity) context.Context {
	ctx := contextWithIdentity(context.Background(), identity)
	return contextWithCredentialContext(ctx, credentialContextForIdentity(identity))
}

func resolvesAvailable(t *testing.T, srv *Server, ctx context.Context, id string) bool {
	t.Helper()
	views := srv.resolveArtifactRefs(ctx, []artifacts.MessageRef{{ArtifactID: id}})
	require.Len(t, views, 1)
	if !views[0].Available {
		assert.Equal(t, artifacts.RefView{Ref: artifacts.FormatRef(id, 0), ID: id}, views[0], "an unresolved view carries only the reference")
	}
	return views[0].Available
}

// TestResolveArtifactRefs_EveryGenuineCredentialKindResolves: each
// authentication arm whose identity the artifact service serves resolves
// the caller's own artifact (no false deny), built the way that arm builds
// its context.
func TestResolveArtifactRefs_EveryGenuineCredentialKindResolves(t *testing.T) {
	srv, s, project, sender, _, _, _, _ := paritySetup(t)
	st, _ := enableArtifactsForTest(t, srv)
	owner, err := s.GetUser(context.Background(), project.OwnerID)
	require.NoError(t, err)
	userOwned := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindUser, owner.ID, "User doc")
	devOwned := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindUser, DevUserID, "Dev doc")
	agentOwned := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Agent doc")
	agent := tokenBackedSender(t, s, sender)

	cases := []struct {
		name     string
		identity Identity
		artifact string
	}{
		// Session JWT (AuthTypeJWT), trusted-proxy and proxy-authenticator
		// users, and external bearer users are all *AuthenticatedUser with
		// CredentialKindInteractive.
		{"interactive session", NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web"), userOwned},
		{"proxy user", NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "proxy"), userOwned},
		{"external bearer", NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "external"), userOwned},
		{"user access token", NewScopedUserIdentityWithCeiling(NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "api"), project.ID, nil, "uat-cred-1",
			permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{artifacts.PermissionRead}}), userOwned},
		{"dev token", NewDevUser(DevUserConfig{}), devOwned},
		{"agent token", agent, agentOwned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := middlewareCtx(tc.identity)
			require.True(t, requestCredentialBindsIdentity(ctx))
			assert.True(t, resolvesAvailable(t, srv, ctx, tc.artifact))
		})
	}

	// The broker arm sets the identity (the broker, or the user it acts for
	// after HMAC verification and on-behalf-of resolution) and then records
	// a broker credential. The on-behalf-of user binds and resolves like any
	// request-authenticated user; a broker identity itself is not served by
	// the artifact service.
	onBehalf := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web")
	brokerCtx := contextWithCredentialContext(contextWithIdentity(context.Background(), onBehalf),
		CredentialContext{Kind: CredentialKindBroker, ID: "broker-1", Type: "broker"})
	require.True(t, requestCredentialBindsIdentity(brokerCtx))
	assert.True(t, resolvesAvailable(t, srv, brokerCtx, userOwned))
}

// TestResolveArtifactRefs_ReplacedOrUnauthenticatedIdentityResolvesNothing
// is the swap-shape guard: a request context whose identity is replaced
// after authentication (contextWithIdentity, as in-process code does to act
// for a reconstructed principal) resolves nothing, even when the
// replacement names the same user or agent; and a context holding only an
// identity resolves nothing.
func TestResolveArtifactRefs_ReplacedOrUnauthenticatedIdentityResolvesNothing(t *testing.T) {
	srv, s, project, sender, target, _, _, _ := paritySetup(t)
	st, _ := enableArtifactsForTest(t, srv)
	owner, err := s.GetUser(context.Background(), project.OwnerID)
	require.NoError(t, err)
	userOwned := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindUser, owner.ID, "User doc")
	agentOwned := seedMessageArtifact(t, st, project.ID, artifacts.PrincipalKindAgent, sender.ID, "Agent doc")
	agent := tokenBackedSender(t, s, sender)

	session := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "web")
	reconstructedUser := NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "dispatch")
	reconstructedAgent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: sender.ID, ID: agent.Claims.ID}, ProjectID: sender.ProjectID,
		Scopes: ScopesForRole(AgentRoleBaseline), ScopeSchema: CurrentAgentScopeSchema, Ancestry: sender.Ancestry,
	}}
	otherAgent := artifactTestAgent(target.ID, target.ProjectID, ScopesForRole(AgentRoleBaseline)...)

	// Sanity: the genuine request contexts resolve.
	require.True(t, resolvesAvailable(t, srv, middlewareCtx(session), userOwned))
	require.True(t, resolvesAvailable(t, srv, middlewareCtx(agent), agentOwned))

	cases := []struct {
		name     string
		ctx      context.Context
		artifact string
	}{
		{"user request, identity replaced by a reconstructed user", contextWithIdentity(middlewareCtx(session), reconstructedUser), userOwned},
		{"user request, identity replaced by a reconstructed agent", contextWithIdentity(middlewareCtx(session), reconstructedAgent), agentOwned},
		{"agent request, identity replaced by a reconstructed agent", contextWithIdentity(middlewareCtx(agent), reconstructedAgent), agentOwned},
		{"agent request, identity replaced by a reconstructed user", contextWithIdentity(middlewareCtx(agent), reconstructedUser), userOwned},
		{"agent request, identity replaced by another agent", contextWithIdentity(middlewareCtx(otherAgent), agent), agentOwned},
		{"identity only, user", contextWithIdentity(context.Background(), session), userOwned},
		{"identity only, agent", contextWithIdentity(context.Background(), agent), agentOwned},
		{"no identity", context.Background(), userOwned},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, requestCredentialBindsIdentity(tc.ctx))
			assert.False(t, resolvesAvailable(t, srv, tc.ctx, tc.artifact))
			_, admitted, warning := srv.admitMessageArtifacts(tc.ctx, map[string]string{artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: tc.artifact})})
			assert.Empty(t, admitted)
			assert.Equal(t, artifactRefsWarning(1), warning)
		})
	}
}

// TestResolveArtifactRefs_ThroughHubMiddleware: an agent sends to a user
// through the full hub handler with its real token. The authentication
// middleware's context reaches admission unchanged, so the agent's own
// artifact is admitted and recorded.
func TestResolveArtifactRefs_ThroughHubMiddleware(t *testing.T) {
	srv, s := testServer(t)
	st, _ := enableArtifactsForTest(t, srv)
	p := artifactProject(t, s, "msgart-mw")
	a, tok := artifactAgent(t, srv, s, p.ID, "msgart-mw-agent", AgentRoleBaseline)
	user := &store.User{ID: tid("msgart-mw-user"), Email: "mw-user@test.example", DisplayName: "MW", Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), user))
	own := seedMessageArtifact(t, st, p.ID, artifacts.PrincipalKindAgent, a.ID, "Agent doc")

	body, err := json.Marshal(OutboundMessageRequest{
		Recipient: "user:" + user.Email, Msg: "see doc", Type: "instruction",
		Metadata: map[string]string{artifacts.MessageMetadataKey: refsValue(artifacts.MessageRef{ArtifactID: own})},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/outbound-message", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", tok)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp["artifact_warning"], "the agent's own artifact must be admitted through the real middleware")
}

// valueIdentity is a comparable value-typed identity. No authentication arm
// creates one; it stands for any future identity type that is not a
// pointer.
type valueIdentity struct{ id string }

func (v valueIdentity) ID() string   { return v.id }
func (v valueIdentity) Type() string { return "user" }

// TestRequestCredentialBindsIdentity_PointerIdentitiesOnly: the binding is
// by pointer. A value-typed identity never binds, neither the recorded value
// itself nor an identical reconstructed copy, and pointer identities bind
// only to the same address.
func TestRequestCredentialBindsIdentity_PointerIdentitiesOnly(t *testing.T) {
	recorded := valueIdentity{id: tid("value-user")}
	ctx := middlewareCtx(recorded)
	assert.False(t, requestCredentialBindsIdentity(ctx), "a value-typed identity must not bind")
	assert.False(t, requestCredentialBindsIdentity(contextWithIdentity(ctx, valueIdentity{id: recorded.id})),
		"an identical reconstructed value must not bind")

	u := NewAuthenticatedUser(tid("ptr-user"), "p@test.example", "P", "member", "web")
	copied := *u
	assert.True(t, requestCredentialBindsIdentity(middlewareCtx(u)))
	assert.False(t, requestCredentialBindsIdentity(contextWithIdentity(middlewareCtx(u), &copied)),
		"a field-identical copy at another address must not bind")
	assert.False(t, sameIdentityObject(nil, u))
	assert.False(t, sameIdentityObject((*AuthenticatedUser)(nil), (*AuthenticatedUser)(nil)))
}
