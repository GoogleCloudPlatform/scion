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
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

type harness struct {
	t      *testing.T
	home   string
	clk    *hubprofiletest.Clock
	gcp    *hubprofiletest.GCP
	target *hubprofiletest.Target
	env    map[string]string
}

func newHarness(t *testing.T, iap bool) *harness {
	t.Helper()
	clk := hubprofiletest.NewClock()
	h := &harness{t: t, home: t.TempDir(), clk: clk, gcp: hubprofiletest.NewGCP(t, clk), target: hubprofiletest.NewTarget(t, clk, iap), env: map[string]string{}}
	return h
}

func (h *harness) open(p *Profile, alias, run string) (*Session, error) {
	return Open(Options{Home: h.home, Profile: p, Alias: alias, Run: run}, testDeps(h.gcp, h.clk, h.env))
}

func (h *harness) mustOpen(p *Profile, alias, run string) *Session {
	h.t.Helper()
	s, err := h.open(p, alias, run)
	require.NoError(h.t, err)
	return s
}

func headerNames(hd http.Header) []string {
	var names []string
	for k := range hd {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// X3-1 (no fallback, golden): the issuer request carries exactly the
// profile's bearer, delegation and run label, and the IAP token in
// Proxy-Authorization; never an agent token, SCION_AUTH_TOKEN or
// SCION_HUB_TOKEN, although all of them are in the environment.
func TestSession_IssuerRequestCarriesOnlyProfileCredentials(t *testing.T) {
	h := newHarness(t, true)
	s := h.mustOpen(testProfile(h.target, TransportIAP), "", "r1")
	client, err := s.Client()
	require.NoError(t, err)
	_, err = client.TestIdentities().List(context.Background(), nil)
	require.NoError(t, err)

	reqs := h.target.Recorded()
	require.Len(t, reqs, 2, "status (issuer audience) then the list")
	status, list := reqs[0], reqs[1]
	assert.Equal(t, "/api/v1/test-infra/status", status.Path)
	assert.Empty(t, status.Header.Get("Authorization"), "the public status read carries no bearer")
	assert.NotEmpty(t, status.Header.Get("Proxy-Authorization"))

	assert.Equal(t, "/api/v1/test-identities", list.Path)
	assert.Equal(t, []string{"Authorization", "Proxy-Authorization", "X-Scion-Issuer-Delegation", "X-Scion-Run-Label"}, credentialHeaderNames(list.Header), "exactly the profile credentials")
	assert.Equal(t, testDelegation, list.Header.Get("X-Scion-Issuer-Delegation"))
	assert.Equal(t, "r1", list.Header.Get("X-Scion-Run-Label"))
	c, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(list.Header.Get("Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, testIssuerSA, c.Email)
	assert.Equal(t, []string{testIssuerAud}, c.Audiences)
	pc, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(list.Header.Get("Proxy-Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, testUATSA, pc.Email)
	assert.Equal(t, []string{testIAPAudience}, pc.Audiences)
	for _, r := range reqs {
		for k, vs := range r.Header {
			for _, v := range vs {
				assert.NotContains(t, v, "AMBIENT", "header %s", k)
			}
		}
	}
}

// credentialHeaderNames returns the sorted names of every header in hd that
// can carry a credential or identity claim.
func credentialHeaderNames(hd http.Header) []string {
	var out []string
	for _, n := range headerNames(hd) {
		ln := strings.ToLower(n)
		if strings.Contains(ln, "authorization") || strings.HasPrefix(ln, "x-scion-") || ln == "cookie" || ln == "x-api-key" || strings.Contains(ln, "token") {
			out = append(out, n)
		}
	}
	return out
}

// X3-1: a profile with no applicable credential fails at Open, and the
// target sees no request.
func TestSession_NoProfileCredentialFailsClosed(t *testing.T) {
	h := newHarness(t, false)
	p := testProfile(h.target, TransportNone)
	p.IssuerDelegations = nil
	_, err := h.open(p, "", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no profile credential applies")
	assert.Contains(t, err.Error(), "never falls back")

	_, err = h.open(testProfile(h.target, TransportNone), "owner", "r1")
	require.Error(t, err, "an alias that is not cached")
	_, err = h.open(testProfile(h.target, TransportNone), "owner", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--run")
	assert.Empty(t, h.target.Recorded())
}

// Fail closed on emulator or IAM errors: no request leaves for the target
// without its credential.
func TestSession_TokenFailuresSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *harness)
	}{
		{"IAM error", func(h *harness) { h.gcp.IAMFail.Store(true) }},
		{"emulator error", func(h *harness) { h.gcp.EmuFail.Store(true) }},
		{"not assign mode", func(h *harness) { h.env["SCION_METADATA_MODE"] = "passthrough" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, true)
			tc.set(h)
			s := h.mustOpen(testProfile(h.target, TransportIAP), "", "")
			client, err := s.Client()
			require.NoError(t, err)
			_, err = client.TestIdentities().List(context.Background(), nil)
			require.Error(t, err)
			assert.Empty(t, h.target.Recorded(), "the target saw no request")
		})
	}
	t.Run("issuer mint fails after the transport works", func(t *testing.T) {
		h := newHarness(t, false)
		h.gcp.EmailOverride[testIssuerSA] = "impostor@proj.iam.gserviceaccount.com"
		s := h.mustOpen(testProfile(h.target, TransportNone), "", "")
		client, _ := s.Client()
		_, err := client.TestIdentities().List(context.Background(), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "email does not match")
		for _, r := range h.target.Recorded() {
			assert.NotEqual(t, "/api/v1/test-identities", r.Path)
		}
	})
}

// X3-2: a profile whose endpoint is the agent's own hub is refused.
func TestSession_RefusesOwnHub(t *testing.T) {
	h := newHarness(t, false)
	h.env["SCION_HUB_ENDPOINT"] = h.target.Srv.URL + "/"
	_, err := h.open(testProfile(h.target, TransportNone), "", "")
	require.ErrorIs(t, err, ErrOwnHub)
	assert.Empty(t, h.target.Recorded())
}

// N3: the runner mints an issuer token only for an audience of the issuer
// form, never the IAP audience or anything else the target publishes.
func TestSession_RefusesUnexpectedIssuerAudience(t *testing.T) {
	for _, tc := range []struct {
		name, aud, iap string
	}{
		{"the IAP audience", testIAPAudience, ""},
		{"arbitrary", "https://storage.googleapis.com", ""},
		{"bare prefix", IssuerAudiencePrefix, ""},
		{"control character", IssuerAudiencePrefix + "hub\n1", ""},
		{"equal to the profile's IAP audience", IssuerAudiencePrefix + "x", IssuerAudiencePrefix + "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, false)
			aud := tc.aud
			h.target.AudienceOverride = &aud
			p := testProfile(h.target, TransportNone)
			if tc.iap != "" {
				p.Transport = TransportIAP
				p.IAPAudience = tc.iap
				p.UATServiceAccount = testUATSA
			}
			s := h.mustOpen(p, "", "")
			_, err := s.IssuerAudience(context.Background())
			require.Error(t, err)
			assert.ErrorContains(t, err, "refusing to mint")
		})
	}

	// Driven through a client request: the request fails, the target sees
	// only the public status read, and no issuer token is ever minted.
	h := newHarness(t, false)
	aud := "https://storage.googleapis.com"
	h.target.AudienceOverride = &aud
	s := h.mustOpen(testProfile(h.target, TransportNone), "", "")
	client, err := s.Client()
	require.NoError(t, err)
	_, err = client.TestIdentities().List(context.Background(), nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "refusing to mint")
	reqs := h.target.Recorded()
	require.Len(t, reqs, 1)
	assert.Equal(t, "/api/v1/test-infra/status", reqs[0].Path)
	for _, tok := range h.gcp.MintedTokens() {
		c, _ := transportauth.DecodeIDTokenClaims(tok)
		assert.NotEqual(t, testIssuerSA, c.Email, "no issuer token was minted")
	}
	assert.EqualValues(t, 0, h.gcp.IAMCalls.Load(), "IAM Credentials was not called")
}

// Profile credentials go only to the target host, never to another URL
// (for example a signed transfer URL).
func TestSession_NoCredentialOffTarget(t *testing.T) {
	h := newHarness(t, true)
	var got http.Header
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
	}))
	defer other.Close()
	s := h.mustOpen(testProfile(h.target, TransportIAP), "", "r1")
	resp, err := s.HTTPClient().Get(other.URL + "/upload")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.NotNil(t, got)
	for _, hd := range []string{"Authorization", "Proxy-Authorization", "X-Scion-Issuer-Delegation", "X-Scion-Run-Label"} {
		assert.Empty(t, got.Get(hd), hd)
	}
	assert.EqualValues(t, 0, h.gcp.IAMCalls.Load(), "no token is minted for another host")
}

func issueOwner(t *testing.T, h *harness, p *Profile, alias, role string) *CacheEntry {
	t.Helper()
	s := h.mustOpen(p, "", "r1")
	e, err := IssueToCache(context.Background(), s, IssueRequest{Alias: alias, Role: role, Purpose: "journey"})
	require.NoError(t, err)
	return e
}

// X3-3 (cache) and the alias bearer: issue stores the identity 0600; --as
// later uses the cached fixture token, never the issuer token.
func TestSession_IssueCachesAndAliasUsesFixtureToken(t *testing.T) {
	h := newHarness(t, true)
	p := testProfile(h.target, TransportIAP)
	e := issueOwner(t, h, p, "owner", "member")
	assert.Equal(t, KindTestFixture, e.Kind)
	assert.Equal(t, "member", e.Role)
	assert.Equal(t, testDelegation, e.DelegationID)

	path := filepath.Join(h.home, ".scion", "hub-profiles", "t1", "run-r1", "owner.json")
	fi, err := os.Lstat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// The identity's purpose names the run, for teardown.
	assert.Equal(t, "scion-run:r1/owner journey", h.target.Purpose(e.IdentityID))

	// A second issue of the same alias is refused before any request.
	before := len(h.target.Recorded())
	s := h.mustOpen(p, "", "r1")
	_, err = IssueToCache(context.Background(), s, IssueRequest{Alias: "owner"})
	require.ErrorIs(t, err, ErrEntryExists)
	assert.Len(t, h.target.Recorded(), before, "a taken alias costs no request")

	as := h.mustOpen(p, "owner", "r1")
	client, err := as.Client()
	require.NoError(t, err)
	me, err := client.Auth().Me(context.Background())
	require.NoError(t, err)
	assert.Equal(t, e.Email, me.Email)
	last := h.target.Recorded()[len(h.target.Recorded())-1]
	assert.Equal(t, "Bearer "+e.Token, last.Header.Get("Authorization"))
	assert.Empty(t, last.Header.Get("X-Scion-Issuer-Delegation"))
	assert.Equal(t, "profile:owner", as.BearerSource())
}

// N9: a cached identity is bound to the exact canonical host and effective
// port it was issued on; the broad own-hub rule (loopback and bridge hosts
// as one host, 80/443 as one port) does not apply here.
func TestSession_CacheEntryBoundToExactEndpoint(t *testing.T) {
	h := newHarness(t, false)
	now := h.clk.Now()
	write := func(alias, endpoint string) {
		res, err := ReserveEntry(h.home, "t1", "r1", alias)
		require.NoError(t, err)
		require.NoError(t, res.Fill(&CacheEntry{Alias: alias, Run: "r1", Profile: "t1", Endpoint: endpoint, IdentityID: "fx-" + alias, Email: "e@scion-fixture.invalid", Kind: KindTestFixture, Role: "member", ExpiresAt: now.Add(time.Hour), TokenExpiresAt: now.Add(30 * time.Minute), Token: "T-" + alias}))
	}
	write("a", "https://localhost:8443")
	write("b", "http://localhost")
	write("c", "https://T1.Example.com.:443/")

	profileAt := func(endpoint string) *Profile {
		return &Profile{Name: "t1", Endpoint: endpoint, Transport: TransportNone, IssuerServiceAccount: testIssuerSA, IssuerDelegations: map[string]string{TierMember: testDelegation}}
	}
	for _, tc := range []struct{ alias, endpoint string }{
		{"a", "https://host.docker.internal:8443"}, // bridge host vs loopback
		{"a", "https://127.0.0.1:8443"},            // another loopback name
		{"b", "https://localhost"},                 // 80 vs 443
	} {
		_, err := h.open(profileAt(tc.endpoint), tc.alias, "r1")
		require.Error(t, err, "%s at %s", tc.alias, tc.endpoint)
		assert.Contains(t, err.Error(), "was issued on another hub")
	}
	// The same canonical host and effective port is accepted.
	_, err := h.open(profileAt("https://t1.example.com"), "c", "r1")
	require.NoError(t, err)
	_, err = h.open(profileAt("https://localhost:8443/"), "a", "r1")
	require.NoError(t, err)
}

// Admin tiers are out of scope: refused with a clear message.
func TestIssueToCache_RefusesAdminRoles(t *testing.T) {
	h := newHarness(t, false)
	s := h.mustOpen(testProfile(h.target, TransportNone), "", "r1")
	for _, role := range []string{"hub-admin", "super-admin", "admin"} {
		_, err := IssueToCache(context.Background(), s, IssueRequest{Alias: "a", Role: role})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "member and viewer identities only")
	}
	assert.Empty(t, h.target.Recorded())
	_, err := IssueToCache(context.Background(), h.mustOpen(testProfile(h.target, TransportNone), "", ""), IssueRequest{Alias: "a"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--run")
}

// X3-5: a member token is re-issued through the token route before it
// expires (within ReissueMargin); an admin-tier token is not, and an
// expired identity is refused.
func TestSession_MemberAutoReissueBeforeExpiry(t *testing.T) {
	h := newHarness(t, false)
	p := testProfile(h.target, TransportNone)
	e := issueOwner(t, h, p, "owner", "member")

	// Outside the margin: the cached token is used, no re-issue.
	h.clk.Set(e.TokenExpiresAt.Add(-ReissueMargin - time.Minute))
	s := h.mustOpen(p, "owner", "r1")
	client, _ := s.Client()
	_, err := client.Auth().Me(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, h.target.TokenHits())

	// Inside the margin, before expiry: re-issued, cache replaced.
	h.clk.Set(e.TokenExpiresAt.Add(-4 * time.Minute))
	s = h.mustOpen(p, "owner", "r1")
	client, _ = s.Client()
	_, err = client.Auth().Me(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, h.target.TokenHits())
	got, err := ReadEntry(h.home, "t1", "r1", "owner")
	require.NoError(t, err)
	assert.NotEqual(t, e.Token, got.Token)
	assert.True(t, got.TokenExpiresAt.After(e.TokenExpiresAt))
	last := h.target.Recorded()[len(h.target.Recorded())-1]
	assert.Equal(t, "Bearer "+got.Token, last.Header.Get("Authorization"), "the request used the new token")
	fi, _ := os.Lstat(filepath.Join(h.home, ".scion", "hub-profiles", "t1", "run-r1", "owner.json"))
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// Expired identity: refused, nothing sent.
	h.clk.Set(got.ExpiresAt.Add(time.Second))
	s = h.mustOpen(p, "owner", "r1")
	client, _ = s.Client()
	before := len(h.target.Recorded())
	_, err = client.Auth().Me(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue a new one")
	assert.Len(t, h.target.Recorded(), before)
}

func TestSession_AdminTierTokenIsNotReissued(t *testing.T) {
	h := newHarness(t, false)
	p := testProfile(h.target, TransportNone)
	now := h.clk.Now()
	// An admin-tier entry (admin tiers cannot be issued through a profile;
	// the cache must still refuse to re-issue one).
	res, err := ReserveEntry(h.home, "t1", "r1", "admin")
	require.NoError(t, err)
	require.NoError(t, res.Fill(&CacheEntry{Alias: "admin", Run: "r1", Profile: "t1", Endpoint: h.target.Srv.URL, IdentityID: "fx-adm", Email: "a@scion-fixture.invalid", Kind: "test_admin", Role: "hub-admin", ExpiresAt: now.Add(time.Hour), TokenExpiresAt: now.Add(time.Minute), Token: "ADMIN-TOKEN"}))

	s := h.mustOpen(p, "admin", "r1")
	_, err = s.Reissue(context.Background(), 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin-tier tokens are not re-issued")

	h.clk.Set(now.Add(2 * time.Minute))
	s = h.mustOpen(p, "admin", "r1")
	client, _ := s.Client()
	_, err = client.Auth().Me(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "admin-tier tokens are not re-issued, issue a new identity")
	assert.Equal(t, 0, h.target.TokenHits())
}

// X3-6: teardown leaves 404 on every identity of the run, an empty list
// for the delegation, and no cache; it also removes an identity whose
// cache file was never written.
func TestTeardown_RemovesEveryIdentityOfTheRunAndTheCache(t *testing.T) {
	h := newHarness(t, true)
	p := testProfile(h.target, TransportIAP)
	owner := issueOwner(t, h, p, "owner", "member")
	viewer := issueOwner(t, h, p, "viewer", "viewer")
	// An orphan of the run: issued, but its cache file is gone.
	orphan := issueOwner(t, h, p, "lost", "member")
	require.NoError(t, RemoveEntry(h.home, "t1", "r1", "lost"))
	// An expired orphan of the run: no cache file, past its expiry, still
	// on the hub (the reaper has not run).
	expired := issueOwner(t, h, p, "old", "member")
	require.NoError(t, RemoveEntry(h.home, "t1", "r1", "old"))
	h.target.Expire(expired.IdentityID)
	// An identity of another run on the same delegation is kept.
	s2 := h.mustOpen(p, "", "r2")
	other, err := IssueToCache(context.Background(), s2, IssueRequest{Alias: "owner"})
	require.NoError(t, err)

	rep, err := Teardown(context.Background(), h.mustOpen(p, "", "r1"), false)
	require.NoError(t, err)
	assert.True(t, rep.Complete, "%+v", rep.Items)
	assert.True(t, rep.CacheRemoved)

	issuer := h.mustOpen(p, "", "r1")
	client, _ := issuer.IssuerClient()
	for _, id := range []string{owner.IdentityID, viewer.IdentityID, orphan.IdentityID, expired.IdentityID} {
		err := client.TestIdentities().Delete(context.Background(), id)
		assert.True(t, apiclient.IsNotFoundError(err), "identity %s is gone (404)", id)
	}
	list, err := client.TestIdentities().List(context.Background(), &hubclient.ListTestIdentitiesOptions{IncludeExpired: true})
	require.NoError(t, err)
	var ids []string
	for _, it := range list.Items {
		ids = append(ids, it.ID)
	}
	assert.Equal(t, []string{other.IdentityID}, ids, "only the other run's identity is left on the delegation")
	dir, _ := RunDir(h.home, "t1", "r1")
	_, err = os.Stat(dir)
	assert.True(t, os.IsNotExist(err))

	// Again: idempotent, still complete.
	rep, err = Teardown(context.Background(), h.mustOpen(p, "", "r1"), false)
	require.NoError(t, err)
	assert.True(t, rep.Complete)

	// The other run, purged.
	rep, err = Teardown(context.Background(), h.mustOpen(p, "", "r2"), true)
	require.NoError(t, err)
	assert.True(t, rep.Complete)
	list, _ = client.TestIdentities().List(context.Background(), nil)
	assert.Empty(t, list.Items)
}

// X3-4: hub status passes with the profile's credentials and fails when the
// request on the wire carries another bearer, another credential header, or
// another transport principal.
func TestStatus_GateChecks(t *testing.T) {
	h := newHarness(t, true)
	p := testProfile(h.target, TransportIAP)
	issueOwner(t, h, p, "owner", "member")

	t.Run("issuer ok", func(t *testing.T) {
		r := h.mustOpen(p, "", "r1").Status(context.Background())
		assert.True(t, r.OK, "%+v", r.Checks)
		assert.Equal(t, SourceIssuer, r.BearerSource)
		assert.Equal(t, testIssuerSA, r.Identity.Email)
		assert.Equal(t, testUATSA, r.Transport.PrincipalEmail)
		assert.Equal(t, testIAPAudience, r.Transport.Audience)
		assert.Equal(t, testIssuerAud, r.IssuerAudience)
	})
	t.Run("alias ok", func(t *testing.T) {
		r := h.mustOpen(p, "owner", "r1").Status(context.Background())
		assert.True(t, r.OK, "%+v", r.Checks)
		assert.Equal(t, "profile:owner", r.BearerSource)
		assert.Equal(t, KindTestFixture, r.Identity.Kind)
		assert.Equal(t, "member", r.Identity.Tier)
		assert.NotNil(t, r.Identity.TokenExpiresAt)
	})
	tamper := func(s *Session, f func(r *http.Request)) {
		pt := s.hc.Transport.(*profileTransport)
		pt.base = rtFunc(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			f(r)
			return s.recorder.RoundTrip(r)
		})
	}
	t.Run("another bearer on the wire", func(t *testing.T) {
		s := h.mustOpen(p, "owner", "r1")
		tamper(s, func(r *http.Request) { r.Header.Set("Authorization", "Bearer AMBIENT-SCION-AUTH-TOKEN") })
		r := s.Status(context.Background())
		assert.False(t, r.OK)
		assert.False(t, checkOK(r, "bearer from profile"))
	})
	t.Run("another credential header on the wire", func(t *testing.T) {
		s := h.mustOpen(p, "owner", "r1")
		tamper(s, func(r *http.Request) { r.Header.Set("X-Scion-Agent-Token", "AMBIENT") })
		r := s.Status(context.Background())
		assert.False(t, r.OK)
		assert.False(t, checkOK(r, "bearer from profile"))
	})
	t.Run("another transport principal", func(t *testing.T) {
		s := h.mustOpen(p, "owner", "r1")
		other := jwtOf(map[string]any{"email": "intruder@proj.iam.gserviceaccount.com", "aud": testIAPAudience, "exp": h.clk.Now().Add(time.Hour).Unix()})
		tamper(s, func(r *http.Request) { r.Header.Set("Proxy-Authorization", "Bearer "+other) })
		r := s.Status(context.Background())
		assert.False(t, r.OK)
		assert.False(t, checkOK(r, "transport principal"))
		assert.Equal(t, "intruder@proj.iam.gserviceaccount.com", r.Transport.PrincipalEmail)
	})
	t.Run("hub refuses the credential", func(t *testing.T) {
		pp := *p
		pp.IssuerDelegations = map[string]string{TierMember: "dlg-unknown"}
		r := h.mustOpen(&pp, "", "r1").Status(context.Background())
		assert.False(t, r.OK)
		assert.False(t, checkOK(r, "hub accepts the credential"))
	})
}

func checkOK(r *StatusReport, name string) bool {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.OK
		}
	}
	return false
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// X3-3 (golden): no token appears in logs or errors across a whole run:
// issue, alias request, re-issue, status, teardown, and the failure paths.
func TestNoTokenInLogsOrErrors(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	h := newHarness(t, true)
	p := testProfile(h.target, TransportIAP)
	var errs []string
	e := issueOwner(t, h, p, "owner", "member")
	s := h.mustOpen(p, "owner", "r1")
	c, _ := s.Client()
	_, _ = c.Auth().Me(context.Background())
	h.clk.Set(e.TokenExpiresAt.Add(-time.Minute))
	_, _ = h.mustOpen(p, "owner", "r1").Reissue(context.Background(), 0)
	r := h.mustOpen(p, "owner", "r1").Status(context.Background())
	for _, ch := range r.Checks {
		errs = append(errs, ch.Detail)
	}
	h.gcp.IAMFail.Store(true)
	sc, _ := h.mustOpen(p, "", "r1").Client()
	if _, err := sc.TestIdentities().List(context.Background(), nil); err != nil {
		errs = append(errs, err.Error())
	}
	h.gcp.IAMFail.Store(false)
	_, err := Teardown(context.Background(), h.mustOpen(p, "", "r1"), false)
	require.NoError(t, err)

	secrets := append(h.gcp.MintedTokens(), testAccessToken, "AMBIENT-SCION-AUTH-TOKEN", "AMBIENT-SCION-HUB-TOKEN")
	secrets = append(secrets, h.target.FixtureTokens()...)
	require.NotEmpty(t, secrets)
	all := logs.String() + strings.Join(errs, "\n")
	for _, sec := range secrets {
		assert.NotContains(t, all, sec)
		// No fragment of a JWT's payload either.
		if parts := strings.Split(sec, "."); len(parts) == 3 {
			assert.NotContains(t, all, parts[1])
		}
	}
	assert.NotContains(t, all, "FIXTURE-TOKEN")
}
