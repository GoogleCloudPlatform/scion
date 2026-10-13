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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// User access credentials from a peer hub (ptone/scion#4603): the receiving
// hub reads a hub issuer's scion_token_kind claim. Tokens are signed by a
// real peer hub (its federation-token and identity-token handlers, or its
// OIDC signer for shapes those handlers never produce) and verified through
// the peer's published JWKS.

// peerTokFixture is a receiving hub (srv) whose federation configuration
// trusts a peer hub (peer) as an issuer_type hub issuer.
type peerTokFixture struct {
	srv       *Server
	st        store.Store
	peer      *Server
	peerAgent *AgentTokenClaims
	jwksURL   string
	// userJWKSURL and signUser serve and sign for a separate issuer_type
	// user issuer (peerTokUserIssuerURL) trusted by the same receiving hub.
	userJWKSURL string
	signUser    func(t *testing.T, claims interface{}) string
}

const peerTokUserIssuerURL = "https://idp-peer-tok.example.com"

// newPeerTokFixture builds the peer hub (federation.project_visitors on,
// minting allowed for federationTokenAudience), serves its JWKS, and
// installs the receiving hub's federation configuration with configure
// (mutate adjusts the peer issuer).
func newPeerTokFixture(t *testing.T, mutate func(*config.TrustedIssuerConfig)) *peerTokFixture {
	t.Helper()
	peer, _, agentClaims := testOIDCIdentityServer(t)
	federationTokenConfigure(t, peer)
	jwks := httptest.NewServer(http.HandlerFunc(peer.handleJWKS))
	t.Cleanup(jwks.Close)

	userKey, userJWKS, userKid := setupFederationTestServer(t)

	srv, st := testServer(t)
	visitorSetExperiment(t, srv, true)
	f := &peerTokFixture{
		srv: srv, st: st, peer: peer, peerAgent: agentClaims,
		jwksURL: jwks.URL, userJWKSURL: userJWKS.URL,
		signUser: func(t *testing.T, claims interface{}) string {
			return signGenericToken(t, userKey, userKid, claims)
		},
	}
	f.configure(t, mutate)
	return f
}

// configure replaces the receiving hub's federation configuration through
// ApplySnapshot, as a settings reload does, with no restart.
func (f *peerTokFixture) configure(t *testing.T, mutate func(*config.TrustedIssuerConfig)) {
	t.Helper()
	issuer := config.TrustedIssuerConfig{
		IssuerURL:        testOIDCIssuerURL,
		IssuerType:       string(IssuerTypeHub),
		JWKSURL:          f.jwksURL,
		ExpectedAudience: federationTokenAudience,
		AcceptUserTokens: true,
	}
	if mutate != nil {
		mutate(&issuer)
	}
	before := f.srv.federationAuth.Load()
	ApplySnapshot(f.srv, Layer1Snapshot{FederationConfig: &config.FederationConfig{
		Enabled: true,
		TrustedIssuers: []config.TrustedIssuerConfig{
			issuer,
			{IssuerURL: peerTokUserIssuerURL, IssuerType: string(IssuerTypeUser), JWKSURL: f.userJWKSURL, ExpectedAudience: federationTokenAudience},
		},
	}})
	after := f.srv.federationAuth.Load()
	require.NotNil(t, after, "federation configuration applied")
	require.NotSame(t, before, after, "a new authenticator is installed")
}

// mintUserToken returns a user access credential minted by the peer's
// federation-token handler for a new active local user of the peer.
func (f *peerTokFixture) mintUserToken(t *testing.T) (string, *store.User) {
	t.Helper()
	u := federationTokenUser(t, f.peer.store, store.UserKindHuman, store.UserStatusActive)
	w := callFederationToken(t, f.peer, sessionIdentityFor(u), federationTokenBody(federationTokenAudience))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp federationTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Token, u
}

// mintAgentToken returns an agent identity token minted by the peer's
// identity-token handler.
func (f *peerTokFixture) mintAgentToken(t *testing.T) string {
	t.Helper()
	w := doIdentityTokenRequest(t, f.peer, f.peerAgent, identityTokenRequest{Audience: federationTokenAudience})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp identityTokenResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	return resp.Token
}

// peerUserClaims returns the claim set of a valid peer user access
// credential as a map, for tests that change one claim.
func peerUserClaims(subject, email string) map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"iss":              testOIDCIssuerURL,
		"sub":              subject,
		"aud":              []string{federationTokenAudience},
		"email":            email,
		"email_verified":   true,
		"name":             "Peer " + subject,
		"iat":              now.Add(-time.Minute).Unix(),
		"nbf":              now.Add(-time.Minute).Unix(),
		"exp":              now.Add(10 * time.Minute).Unix(),
		"jti":              uuid.New().String(),
		"scion_token_kind": tokenKindUser,
	}
}

// signAsPeer signs claims with the peer hub's OIDC signer, the key its JWKS
// publishes.
func (f *peerTokFixture) signAsPeer(t *testing.T, claims interface{}) string {
	t.Helper()
	token, err := jwt.Signed(f.peer.oidcKeyManager.Signer()).Claims(claims).Serialize()
	require.NoError(t, err)
	return token
}

// through runs token through the receiving hub's auth middleware and
// returns the identity and credential context the next handler sees, with
// the response.
func (f *peerTokFixture) through(t *testing.T, token string) (Identity, CredentialContext, *httptest.ResponseRecorder) {
	t.Helper()
	var gotIdentity Identity
	var gotCredential CredentialContext
	h := UnifiedAuthMiddleware(f.srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIdentity = GetIdentityFromContext(r.Context())
		gotCredential = GetCredentialContextFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil)
	req.Header.Set(FederationTokenHeader, token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return gotIdentity, gotCredential, rec
}

// assertPeerTokRefused checks the refusal every rejected token gets: the
// federation verification failure response (401, "invalid federation
// token"), with no handler run.
func assertPeerTokRefused(t *testing.T, f *peerTokFixture, token string) {
	t.Helper()
	identity, _, rec := f.through(t, token)
	require.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	assert.Equal(t, "invalid federation token", federationTokenErrorMessage(t, rec))
	assert.Nil(t, identity, "the handler never runs")
}

// assertPeerTokUser checks that token yields the FederatedUserIdentity of
// subject and email on the peer issuer.
func assertPeerTokUser(t *testing.T, f *peerTokFixture, token, subject, email string) {
	t.Helper()
	identity, credential, rec := f.through(t, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fu, ok := identity.(*FederatedUserIdentity)
	require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
	assert.Equal(t, testOIDCIssuerURL+":"+subject, fu.ID())
	assert.Equal(t, email, fu.Email())
	assert.Equal(t, "viewer", fu.Role())
	assert.Empty(t, fu.Scopes(), "no agent scopes on a user identity")
	assert.Equal(t, CredentialKindFederation, credential.Kind)
}

// assertPeerTokAgent checks that token yields a FederatedAgentIdentity.
func assertPeerTokAgent(t *testing.T, f *peerTokFixture, token string) *FederatedAgentIdentity {
	t.Helper()
	identity, _, rec := f.through(t, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fa, ok := identity.(*FederatedAgentIdentity)
	require.True(t, ok, "identity = %T, want *FederatedAgentIdentity", identity)
	return fa
}

// A user access credential minted by the peer is accepted from an issuer
// with accept_user_tokens, and becomes the visitor identity once the
// (issuer, subject) pair is linked to an active federated account.
func TestFederationPeerUserToken_MintedTokenAccepted(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	token, u := f.mintUserToken(t)
	assertPeerTokUser(t, f, token, u.ID, u.Email)

	issuer := testOIDCIssuerURL
	email := u.Email
	acct := &store.User{
		DisplayName: "Peer visitor", Kind: store.UserKindFederated, Status: store.UserStatusActive,
		HomeIssuer: &issuer, FederatedEmail: &email,
	}
	ctx := context.Background()
	require.NoError(t, f.st.CreateFederatedUser(ctx, acct))
	_, err := f.st.LinkFederatedIdentity(ctx, acct.ID, testOIDCIssuerURL, u.ID, u.Email)
	require.NoError(t, err)

	identity, credential, rec := f.through(t, token)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	v, ok := identity.(*VisitorIdentity)
	require.True(t, ok, "identity = %T, want *VisitorIdentity", identity)
	assert.Equal(t, acct.ID, v.ID())
	assert.Equal(t, testOIDCIssuerURL, v.IssuerURL())
	assert.Equal(t, u.ID, v.Subject())
	assert.Equal(t, CredentialKindFederation, credential.Kind)
}

// A user access credential from an issuer without accept_user_tokens is
// refused.
func TestFederationPeerUserToken_RefusedWithoutAcceptUserTokens(t *testing.T) {
	f := newPeerTokFixture(t, func(c *config.TrustedIssuerConfig) { c.AcceptUserTokens = false })
	token, _ := f.mintUserToken(t)
	assertPeerTokRefused(t, f, token)
}

// A user-kind token carrying any agent claim claims both kinds and is
// refused.
func TestFederationPeerUserToken_AgentClaimsRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)

	base := peerUserClaims("sub-both", "both@corp.com")
	assertPeerTokUser(t, f, f.signAsPeer(t, base), "sub-both", "both@corp.com")

	for claim, value := range map[string]interface{}{
		"project_id": "project-alpha",
		"agent_name": "worker-1",
		"ancestry":   []string{"user:alice"},
		"root_user":  "user:alice",
	} {
		t.Run(claim, func(t *testing.T) {
			claims := peerUserClaims("sub-both", "both@corp.com")
			claims[claim] = value
			assertPeerTokRefused(t, f, f.signAsPeer(t, claims))
		})
	}
}

// A scion_token_kind other than "user" or "agent" is refused.
func TestFederationPeerUserToken_UnknownKindRefused(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	for _, kind := range []string{"service", "User", "user,agent"} {
		t.Run(kind, func(t *testing.T) {
			claims := peerUserClaims("sub-kind", "kind@corp.com")
			claims["scion_token_kind"] = kind
			assertPeerTokRefused(t, f, f.signAsPeer(t, claims))
		})
	}
}

// A user access credential needs a subject, an email, and email_verified
// true.
func TestFederationPeerUserToken_VerifiedEmailRequired(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	for name, mutate := range map[string]func(map[string]interface{}){
		"email_verified false":   func(c map[string]interface{}) { c["email_verified"] = false },
		"email_verified missing": func(c map[string]interface{}) { delete(c, "email_verified") },
		"email missing":          func(c map[string]interface{}) { delete(c, "email") },
		"email empty":            func(c map[string]interface{}) { c["email"] = "" },
		"sub missing":            func(c map[string]interface{}) { delete(c, "sub") },
	} {
		t.Run(name, func(t *testing.T) {
			claims := peerUserClaims("sub-email", "email@corp.com")
			mutate(claims)
			assertPeerTokRefused(t, f, f.signAsPeer(t, claims))
		})
	}
}

// The issuer's allowed_emails and allowed_domains apply to user access
// credentials.
func TestFederationPeerUserToken_AllowedEmailsAndDomains(t *testing.T) {
	t.Run("allowed_domains", func(t *testing.T) {
		f := newPeerTokFixture(t, func(c *config.TrustedIssuerConfig) { c.AllowedDomains = []string{"Corp.com"} })
		assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-in", "in@corp.com")), "sub-in", "in@corp.com")
		assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-out", "out@other.example")))
		assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-sub", "sub@eng.corp.com")))
	})
	t.Run("allowed_emails", func(t *testing.T) {
		f := newPeerTokFixture(t, func(c *config.TrustedIssuerConfig) { c.AllowedEmails = []string{"*@corp.com"} })
		assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-in", "in@corp.com")), "sub-in", "in@corp.com")
		assertPeerTokRefused(t, f, f.signAsPeer(t, peerUserClaims("sub-out", "out@other.example")))
	})
	t.Run("agent constraints do not apply to user tokens", func(t *testing.T) {
		f := newPeerTokFixture(t, func(c *config.TrustedIssuerConfig) {
			c.AllowedProjects = []string{"project-alpha"}
			c.AllowedRootUsers = []string{"user:alice"}
		})
		assertPeerTokUser(t, f, f.signAsPeer(t, peerUserClaims("sub-in", "in@corp.com")), "sub-in", "in@corp.com")
	})
}

// Agent tokens from a hub issuer behave as before: minted agent tokens
// (scion_token_kind "agent") and tokens from hubs that predate the claim
// both yield a FederatedAgentIdentity, whatever accept_user_tokens and the
// experiment say, and the agent constraints still apply.
func TestFederationPeerUserToken_AgentTokensUnchanged(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	minted := f.mintAgentToken(t)

	legacy := map[string]interface{}{
		"iss":        testOIDCIssuerURL,
		"sub":        "legacy-agent",
		"aud":        []string{federationTokenAudience},
		"iat":        time.Now().Add(-time.Minute).Unix(),
		"exp":        time.Now().Add(10 * time.Minute).Unix(),
		"project_id": "project-legacy",
		"agent_name": "legacy-worker",
		"ancestry":   []string{"user:root"},
		"root_user":  "user:root",
	}
	legacyToken := f.signAsPeer(t, legacy)

	check := func(t *testing.T) {
		t.Helper()
		fa := assertPeerTokAgent(t, f, minted)
		assert.Equal(t, f.peerAgent.Subject, fa.RemoteAgentID())
		assert.Equal(t, f.peerAgent.ProjectID, fa.RemoteProjectID())
		assert.Equal(t, f.peerAgent.Ancestry, fa.Ancestry())

		la := assertPeerTokAgent(t, f, legacyToken)
		assert.Equal(t, "legacy-agent", la.RemoteAgentID())
		assert.Equal(t, "project-legacy", la.RemoteProjectID())
		assert.Equal(t, "legacy-worker", la.AgentName())
	}

	t.Run("accept_user_tokens on", check)
	t.Run("accept_user_tokens off", func(t *testing.T) {
		f.configure(t, func(c *config.TrustedIssuerConfig) { c.AcceptUserTokens = false })
		check(t)
	})
	t.Run("experiment off", func(t *testing.T) {
		visitorSetExperiment(t, f.srv, false)
		check(t)
		visitorSetExperiment(t, f.srv, true)
	})
	t.Run("allowed_projects still applies", func(t *testing.T) {
		f.configure(t, func(c *config.TrustedIssuerConfig) { c.AllowedProjects = []string{f.peerAgent.ProjectID} })
		assertPeerTokAgent(t, f, minted)
		assertPeerTokRefused(t, f, legacyToken)
	})
}

// accept_user_tokens is read on every request: turning it off refuses the
// same credential on the next request and turning it on admits it again,
// with no restart.
func TestFederationPeerUserToken_FlagAppliesWithoutRestart(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	token, u := f.mintUserToken(t)

	assertPeerTokUser(t, f, token, u.ID, u.Email)
	f.configure(t, func(c *config.TrustedIssuerConfig) { c.AcceptUserTokens = false })
	assertPeerTokRefused(t, f, token)
	f.configure(t, nil)
	assertPeerTokUser(t, f, token, u.ID, u.Email)
}

// With federation.project_visitors off, a user access credential from a hub
// issuer is refused even with accept_user_tokens; turning it back on admits
// the same credential.
func TestFederationPeerUserToken_ExperimentOff(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	token, u := f.mintUserToken(t)

	visitorSetExperiment(t, f.srv, false)
	assertPeerTokRefused(t, f, token)
	visitorSetExperiment(t, f.srv, true)
	assertPeerTokUser(t, f, token, u.ID, u.Email)
}

// Authenticate, without options, refuses user access credentials from hub
// issuers.
func TestFederationPeerUserToken_AuthenticateWithoutOptionsRefuses(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	token, _ := f.mintUserToken(t)
	auth := f.srv.federationAuth.Load()
	_, err := auth.Authenticate(token)
	require.Error(t, err)
	identity, err := auth.AuthenticateWithOptions(token, FederationAuthOptions{AcceptHubUserTokens: true})
	require.NoError(t, err)
	_, ok := identity.(*FederatedUserIdentity)
	assert.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
}

// issuer_type user issuers are unchanged: the kind claim is not read, and
// email_verified is not required.
func TestFederationPeerUserToken_UserIssuerUnchanged(t *testing.T) {
	f := newPeerTokFixture(t, nil)
	for name, kind := range map[string]interface{}{"no kind": nil, "agent kind": tokenKindAgent, "unknown kind": "service"} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			claims := map[string]interface{}{
				"iss":   peerTokUserIssuerURL,
				"sub":   "idp-user",
				"aud":   []string{federationTokenAudience},
				"iat":   now.Add(-time.Minute).Unix(),
				"exp":   now.Add(10 * time.Minute).Unix(),
				"email": "idp@corp.com",
			}
			if kind != nil {
				claims["scion_token_kind"] = kind
			}
			identity, _, rec := f.through(t, f.signUser(t, claims))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			fu, ok := identity.(*FederatedUserIdentity)
			require.True(t, ok, "identity = %T, want *FederatedUserIdentity", identity)
			assert.Equal(t, peerTokUserIssuerURL+":idp-user", fu.ID())
		})
	}
}
