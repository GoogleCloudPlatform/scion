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

package hubprofile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// Admin delegations through a profile (ptone/scion#4240, X4b). The target
// is hubprofiletest's fake, which accepts an admin delegation only with an
// ID token of the admin issuer account and serves it like the hub: admin
// issuance of its kind, token re-issue and web-login codes for its own
// identities, nothing else.

// adminProfile is testProfile with the admin issuer account and both
// admin delegations.
func adminProfile(target *hubprofiletest.Target, transport string) *Profile {
	p := testProfile(target, transport)
	p.AdminIssuerServiceAccount = hubprofiletest.AdminIssuerSA
	p.IssuerDelegations[TierHubAdmin] = hubprofiletest.HubAdminDelegation
	p.IssuerDelegations[TierSuperAdmin] = hubprofiletest.SuperAdminDelegation
	return p
}

func issueAdmin(t *testing.T, h *harness, p *Profile, alias, role string) *CacheEntry {
	t.Helper()
	s := h.mustOpen(p, "", "r1")
	e, err := IssueAdminToCache(context.Background(), s, AdminIssueRequest{Alias: alias, AdminRole: role, Purpose: "admin journey"})
	require.NoError(t, err)
	return e
}

// lastRequestTo returns the last recorded request whose path ends in
// suffix.
func lastRequestTo(t *testing.T, h *harness, suffix string) hubprofiletest.Request {
	t.Helper()
	reqs := h.target.Recorded()
	for i := len(reqs) - 1; i >= 0; i-- {
		if strings.HasSuffix(reqs[i].Path, suffix) {
			return reqs[i]
		}
	}
	t.Fatalf("no request to %s", suffix)
	return hubprofiletest.Request{}
}

// assertAdminIssuerRequest checks that req carried an ID token of the
// admin issuer account for the issuer audience, the delegation of tier,
// the transport token, and no other credential.
func assertAdminIssuerRequest(t *testing.T, req hubprofiletest.Request, delegation string) {
	t.Helper()
	assert.Equal(t, delegation, req.Header.Get(HeaderIssuerDelegation))
	c, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, hubprofiletest.AdminIssuerSA, c.Email, "the admin issuer account, never the member issuer account")
	assert.Equal(t, []string{testIssuerAud}, c.Audiences)
	pc, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(req.Header.Get("Proxy-Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, testUATSA, pc.Email)
	assert.Equal(t, []string{"Authorization", "Proxy-Authorization", "X-Scion-Issuer-Delegation", "X-Scion-Run-Label"}, credentialHeaderNames(req.Header))
}

// IssueAdminToCache issues each admin kind through the profile's admin
// delegation of that tier, with an ID token of the admin issuer account,
// and caches it 0600 with its kind and delegation.
func TestIssueAdminToCache_ThroughAdminDelegation(t *testing.T) {
	h := newHarness(t, true)
	p := adminProfile(h.target, TransportIAP)
	for _, tc := range []struct{ role, kind, delegation string }{
		{TierHubAdmin, KindTestHubAdmin, hubprofiletest.HubAdminDelegation},
		{TierSuperAdmin, KindTestSuperAdmin, hubprofiletest.SuperAdminDelegation},
	} {
		e := issueAdmin(t, h, p, "a-"+tc.role, tc.role)
		assert.Equal(t, tc.kind, e.Kind)
		assert.Equal(t, tc.delegation, e.DelegationID)
		assert.Equal(t, tc.role, e.AdminTier())
		assert.False(t, e.IsMemberTier())
		assert.NotEmpty(t, e.Token)
		kind, delegation := h.target.IdentityKind(e.IdentityID)
		assert.Equal(t, tc.kind, kind)
		assert.Equal(t, tc.delegation, delegation)
		assert.Equal(t, RunPurpose("r1", "a-"+tc.role, "admin journey"), h.target.Purpose(e.IdentityID))

		req := lastRequestTo(t, h, "/api/v1/test-identities/admin")
		assertAdminIssuerRequest(t, req, tc.delegation)
		assert.Equal(t, "r1", req.Header.Get(HeaderRunLabel))

		assertPrivateFile(t, filepath.Join(h.home, ".scion", "hub-profiles", "t1", "run-r1", "a-"+tc.role+".json"))
		got, err := ReadEntry(h.home, "t1", "r1", "a-"+tc.role)
		require.NoError(t, err)
		assert.Equal(t, e.Token, got.Token)
	}
	for _, tok := range h.gcp.MintedTokens() {
		c, err := transportauth.DecodeIDTokenClaims(tok)
		require.NoError(t, err)
		assert.NotEqual(t, testIssuerSA, c.Email, "no member issuer token was minted for admin issuance")
	}
}

// Admin issuance is refused before any request without the tier's
// delegation, for a role that is not an admin tier, without --run, and
// for a taken alias; it never falls back to the member delegation.
func TestIssueAdminToCache_RefusedBeforeAnyRequest(t *testing.T) {
	h := newHarness(t, false)
	memberOnly := testProfile(h.target, TransportNone)
	_, err := IssueAdminToCache(context.Background(), h.mustOpen(memberOnly, "", "r1"), AdminIssueRequest{Alias: "a", AdminRole: TierHubAdmin, Purpose: "admin journey"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no hub-admin issuer delegation")

	p := adminProfile(h.target, TransportNone)
	delete(p.IssuerDelegations, TierSuperAdmin)
	s := h.mustOpen(p, "", "r1")
	_, err = IssueAdminToCache(context.Background(), s, AdminIssueRequest{Alias: "a", AdminRole: TierSuperAdmin, Purpose: "admin journey"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no super-admin issuer delegation")
	for _, role := range []string{"member", "admin", "viewer", ""} {
		_, err = IssueAdminToCache(context.Background(), s, AdminIssueRequest{Alias: "a", AdminRole: role, Purpose: "admin journey"})
		require.Error(t, err, role)
		assert.Contains(t, err.Error(), "invalid admin role", role)
	}
	_, err = IssueAdminToCache(context.Background(), h.mustOpen(p, "", ""), AdminIssueRequest{Alias: "a", AdminRole: TierHubAdmin, Purpose: "admin journey"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--run")
	assert.Empty(t, h.target.Recorded(), "no request")

	// A taken alias costs no identity.
	issueOwner(t, h, p, "owner", "member")
	n := len(h.target.LiveIdentityIDs())
	_, err = IssueAdminToCache(context.Background(), s, AdminIssueRequest{Alias: "owner", AdminRole: TierHubAdmin, Purpose: "admin journey"})
	require.Error(t, err)
	assert.Len(t, h.target.LiveIdentityIDs(), n)
}

// The token of a cached admin identity is re-issued only through the
// profile's admin delegation that issued it, explicitly and within
// ReissueMargin of its expiry; a profile whose delegation of that tier is
// missing or another one re-issues nothing and sends nothing.
func TestSession_AdminReissueThroughItsDelegation(t *testing.T) {
	h := newHarness(t, true)
	p := adminProfile(h.target, TransportIAP)
	e := issueAdmin(t, h, p, "admin1", TierHubAdmin)

	s := h.mustOpen(p, "admin1", "r1")
	ne, err := s.Reissue(context.Background(), 0)
	require.NoError(t, err)
	assert.Equal(t, 1, h.target.TokenHits())
	assert.NotEqual(t, e.Token, ne.Token)
	assertAdminIssuerRequest(t, lastRequestTo(t, h, "/api/v1/test-identities/"+e.IdentityID+"/token"), hubprofiletest.HubAdminDelegation)
	got, err := ReadEntry(h.home, "t1", "r1", "admin1")
	require.NoError(t, err)
	assert.Equal(t, ne.Token, got.Token)
	assert.Equal(t, KindTestHubAdmin, got.Kind)

	// Within ReissueMargin of the token's expiry, an alias request re-issues
	// first, through the admin delegation.
	h.clk.Set(got.TokenExpiresAt.Add(-time.Minute))
	s = h.mustOpen(p, "admin1", "r1")
	client, _ := s.Client()
	_, err = client.Auth().Me(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, h.target.TokenHits())

	// The profile's hub-admin delegation is another one, or gone: refused,
	// nothing sent.
	before := len(h.target.Recorded())
	other := adminProfile(h.target, TransportIAP)
	other.IssuerDelegations[TierHubAdmin] = "dlg-hub-admin-2"
	_, err = h.mustOpen(other, "admin1", "r1").Reissue(context.Background(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another hub-admin delegation")
	none := adminProfile(h.target, TransportIAP)
	delete(none.IssuerDelegations, TierHubAdmin)
	_, err = h.mustOpen(none, "admin1", "r1").Reissue(context.Background(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no hub-admin issuer delegation")
	assert.Len(t, h.target.Recorded(), before, "no request")
	assert.Equal(t, 2, h.target.TokenHits())

	// IssuerClientFor picks the delegation of each entry's tier.
	owner := issueOwner(t, h, p, "owner", "member")
	s = h.mustOpen(p, "", "r1")
	c, err := s.IssuerClientFor(owner)
	require.NoError(t, err)
	_, err = c.TestIdentities().Token(context.Background(), owner.IdentityID, nil)
	require.NoError(t, err)
	assert.Equal(t, testDelegation, lastRequestTo(t, h, "/token").Header.Get(HeaderIssuerDelegation))
	c, err = s.IssuerClientFor(got)
	require.NoError(t, err)
	_, err = c.TestIdentities().WebLoginCode(context.Background(), got.IdentityID)
	require.Error(t, err, "the target's admin web session setting is off")
	h.target.AdminWebSessions = true
	_, err = c.TestIdentities().WebLoginCode(context.Background(), got.IdentityID)
	require.NoError(t, err)
	assertAdminIssuerRequest(t, lastRequestTo(t, h, "/web-login-code"), hubprofiletest.HubAdminDelegation)
}

// Teardown deletes member identities as before and leaves admin-kind
// identities to expire: each is reported left-to-expire and dropped from
// the cache with its token. A profile with only admin delegations tears
// its admin aliases down the same way.
func TestTeardown_AdminAliasesLeftToExpire(t *testing.T) {
	h := newHarness(t, false)
	p := adminProfile(h.target, TransportNone)
	owner := issueOwner(t, h, p, "owner", "member")
	adm := issueAdmin(t, h, p, "admin1", TierHubAdmin)

	rep, err := Teardown(context.Background(), h.mustOpen(p, "", "r1"), false)
	require.NoError(t, err)
	assert.True(t, rep.Complete, "%+v", rep.Items)
	assert.True(t, rep.CacheRemoved)
	outcomes := map[string]string{}
	for _, it := range rep.Items {
		outcomes[it.IdentityID] = it.Outcome
	}
	assert.Equal(t, "deleted", outcomes[owner.IdentityID])
	assert.Equal(t, "left-to-expire", outcomes[adm.IdentityID])
	assert.Contains(t, h.target.LiveIdentityIDs(), adm.IdentityID, "a delegation deletes no admin identity")
	assert.NotContains(t, h.target.LiveIdentityIDs(), owner.IdentityID)
	_, err = os.Stat(filepath.Join(h.home, ".scion", "hub-profiles", "t1", "run-r1"))
	assert.True(t, os.IsNotExist(err))
	for _, r := range h.target.Recorded() {
		if r.Method == "DELETE" {
			assert.NotContains(t, r.Path, adm.IdentityID, "no delete was sent for the admin identity")
		}
	}

	adminOnly := adminProfile(h.target, TransportNone)
	adminOnly.IssuerServiceAccount = ""
	delete(adminOnly.IssuerDelegations, TierMember)
	adm2 := issueAdmin(t, h, adminOnly, "admin2", TierSuperAdmin)
	rep, err = Teardown(context.Background(), h.mustOpen(adminOnly, "", "r1"), false)
	require.NoError(t, err)
	require.Len(t, rep.Items, 1)
	assert.Equal(t, adm2.IdentityID, rep.Items[0].IdentityID)
	assert.Equal(t, "left-to-expire", rep.Items[0].Outcome)
	assert.True(t, rep.Complete)
	assert.True(t, rep.CacheRemoved)
}

// A4-style token handling for admin issuance through a profile: when the
// identity was issued but its cache file could not be written, the error
// names the identity and its expiry and never carries the token; when the
// hub refuses the request, the error carries no token either. No cache
// file is left behind in either case.
func TestIssueAdminToCache_ErrorPathsCarryNoToken(t *testing.T) {
	h := newHarness(t, false)
	p := adminProfile(h.target, TransportNone)
	s := h.mustOpen(p, "", "r1")

	orig := fillAdminEntry
	t.Cleanup(func() { fillAdminEntry = orig })
	fillAdminEntry = func(r *Reservation, e *CacheEntry) error {
		_ = r.f.Close()
		return errors.New("simulated write failure")
	}
	_, err := IssueAdminToCache(context.Background(), s, AdminIssueRequest{Alias: "a1", AdminRole: TierHubAdmin, Purpose: "admin journey"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was issued but could not be cached")
	assert.Contains(t, err.Error(), "expires at")
	for _, tok := range h.target.FixtureTokens() {
		assert.NotContains(t, err.Error(), tok)
		assert.NotContains(t, err.Error(), strings.Split(tok, ".")[1])
	}
	_, statErr := os.Lstat(filepath.Join(h.home, ".scion", "hub-profiles", "t1", "run-r1", "a1.json"))
	assert.True(t, os.IsNotExist(statErr), "no cache file is left")
	fillAdminEntry = orig

	// The hub refuses (the delegation does not cover the requested kind):
	// nothing cached, no token.
	wrong := adminProfile(h.target, TransportNone)
	wrong.IssuerDelegations[TierSuperAdmin] = hubprofiletest.HubAdminDelegation
	s2 := h.mustOpen(wrong, "", "r1")
	_, err = IssueAdminToCache(context.Background(), s2, AdminIssueRequest{Alias: "a2", AdminRole: TierSuperAdmin, Purpose: "admin journey"})
	require.Error(t, err)
	for _, tok := range h.target.FixtureTokens() {
		assert.NotContains(t, err.Error(), tok)
	}
	_, statErr = os.Lstat(filepath.Join(h.home, ".scion", "hub-profiles", "t1", "run-r1", "a2.json"))
	assert.True(t, os.IsNotExist(statErr))
}

// X5 + X4b: every admin profile request runs the own-hub check by hub id
// before any bearer or IAM issuer-token mint. With a target that reports
// this agent's own hub id (under another name), admin issuance, admin
// token re-issue and an admin web-login code are refused with ErrOwnHub:
// no issuer ID token is minted for either issuer account, no bearer or
// delegation reaches the target, and no cache entry is written or changed.
func TestOwnHubByID_AdminPathsRefusedBeforeAnyMint(t *testing.T) {
	h := newHarness(t, true)
	p := adminProfile(h.target, TransportIAP)
	// A cached admin identity, issued while no own hub endpoint is set.
	cached := issueAdmin(t, h, p, "admin1", TierHubAdmin)
	before, err := ReadEntry(h.home, "t1", "r1", "admin1")
	require.NoError(t, err)

	h.target.SetHubID("hub-own")
	own := hubprofiletest.NewHealthHub(t, "hub-own")
	h.env["SCION_HUB_ENDPOINT"] = own.Srv.URL
	minted := len(h.gcp.MintedTokens())
	recorded := len(h.target.Recorded())
	hits := h.target.TokenHits()

	// Admin issuance.
	_, err = IssueAdminToCache(context.Background(), h.mustOpen(p, "", "r1"), AdminIssueRequest{Alias: "admin2", AdminRole: TierHubAdmin, Purpose: "admin journey"})
	require.ErrorIs(t, err, ErrOwnHub)
	_, statErr := os.Lstat(filepath.Join(h.home, ".scion", "hub-profiles", "t1", "run-r1", "admin2.json"))
	assert.True(t, os.IsNotExist(statErr), "no cache file for a refused issuance")

	// Admin token re-issue of the cached identity.
	_, err = h.mustOpen(p, "admin1", "r1").Reissue(context.Background(), 0)
	require.ErrorIs(t, err, ErrOwnHub)

	// Admin web-login code through the admin delegation.
	c, err := h.mustOpen(p, "", "r1").AdminIssuerClient(TierHubAdmin)
	require.NoError(t, err)
	_, err = c.TestIdentities().WebLoginCode(context.Background(), cached.IdentityID)
	require.ErrorIs(t, err, ErrOwnHub)

	for _, tok := range h.gcp.MintedTokens()[minted:] {
		cl, err := transportauth.DecodeIDTokenClaims(tok)
		require.NoError(t, err)
		assert.NotEqual(t, hubprofiletest.AdminIssuerSA, cl.Email, "no admin issuer ID token is minted")
		assert.NotEqual(t, testIssuerSA, cl.Email, "no member issuer ID token is minted")
	}
	for _, r := range h.target.Recorded()[recorded:] {
		assert.Empty(t, r.Header.Get("Authorization"), r.Path)
		assert.Empty(t, r.Header.Get(HeaderIssuerDelegation), r.Path)
		assert.NotEqual(t, "/api/v1/test-identities/admin", r.Path)
	}
	assert.Equal(t, hits, h.target.TokenHits(), "no token re-issued")
	assert.Empty(t, h.target.WebCodes(), "no web-login code minted")
	after, err := ReadEntry(h.home, "t1", "r1", "admin1")
	require.NoError(t, err)
	assert.Equal(t, before.Token, after.Token, "the cached entry is unchanged")
}
