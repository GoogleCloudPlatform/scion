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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile"
	"github.com/GoogleCloudPlatform/scion/pkg/hubprofile/hubprofiletest"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// scion hub test-identity issue-admin and the admin credential paths with
// a target-hub profile (ptone/scion#4240, X4b). The target is
// hubprofiletest's fake, which accepts an admin delegation only with an ID
// token of the admin issuer account.

// saveAdminProfiles adds profile t9 (the fake target, with its member and
// both admin delegations and the admin issuer account) and own9 (the same
// delegations for the agent's own hub) to the x3b environment.
func (e *x3bEnv) saveAdminProfiles(t *testing.T) {
	t.Helper()
	delegations := map[string]string{
		hubprofile.TierMember:     hubprofiletest.Delegation,
		hubprofile.TierHubAdmin:   hubprofiletest.HubAdminDelegation,
		hubprofile.TierSuperAdmin: hubprofiletest.SuperAdminDelegation,
	}
	require.NoError(t, hubprofile.Save(e.home, &hubprofile.Profile{Name: "t9", Endpoint: e.target.Srv.URL, Transport: hubprofile.TransportIAP,
		IAPAudience: hubprofiletest.IAPAudience, UATServiceAccount: hubprofiletest.UATSA, IssuerServiceAccount: hubprofiletest.IssuerSA,
		AdminIssuerServiceAccount: hubprofiletest.AdminIssuerSA, IssuerDelegations: delegations}, false))
	own, err := hubprofile.Load(e.home, "own")
	require.NoError(t, err)
	require.NoError(t, hubprofile.Save(e.home, &hubprofile.Profile{Name: "own9", Endpoint: own.Endpoint, Transport: hubprofile.TransportNone,
		IssuerServiceAccount: hubprofiletest.IssuerSA, AdminIssuerServiceAccount: hubprofiletest.AdminIssuerSA, IssuerDelegations: delegations}, false))
}

// lastTargetRequest returns the last recorded target request whose path
// ends in suffix.
func (e *x3bEnv) lastTargetRequest(t *testing.T, method, suffix string) hubprofiletest.Request {
	t.Helper()
	recs := e.target.Recorded()
	for i := len(recs) - 1; i >= 0; i-- {
		if recs[i].Method == method && strings.HasSuffix(recs[i].Path, suffix) {
			return recs[i]
		}
	}
	t.Fatalf("no %s request to %s", method, suffix)
	return hubprofiletest.Request{}
}

// assertAdminIssuerAuthorization checks the credentials the target saw on
// req: an ID token of the admin issuer account for the issuer audience in
// Authorization, the admin delegation, and the IAP transport token.
func assertAdminIssuerAuthorization(t *testing.T, req hubprofiletest.Request, delegation string) {
	t.Helper()
	assert.Equal(t, delegation, req.Header.Get("X-Scion-Issuer-Delegation"))
	c, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, hubprofiletest.AdminIssuerSA, c.Email)
	assert.True(t, c.HasAudience(hubprofiletest.IssuerAudience))
	pc, err := transportauth.DecodeIDTokenClaims(strings.TrimPrefix(req.Header.Get("Proxy-Authorization"), "Bearer "))
	require.NoError(t, err)
	assert.Equal(t, hubprofiletest.UATSA, pc.Email)
}

// C1: in agent mode, issue-admin with a profile for another hub issues
// through the profile's admin delegation (the target sees the admin
// issuer's ID token and the delegation), caches the identity 0600 and
// prints no token; token --as re-issues through the same delegation;
// web-code --admin-role mints through it once the target's admin web
// session setting is on; delete --as refuses an admin identity without a
// request; teardown leaves it to expire and drops it from the cache.
func TestHubTestIdentityIssueAdmin_Profile(t *testing.T) {
	e := newX3bEnv(t)
	e.saveAdminProfiles(t)
	projBefore, homeBefore := e.snapshot(t)

	r, err := e.run(t, "--hub-profile", "t9", "hub", "test-identity", "issue-admin", "--admin-role", "hub-admin",
		"--purpose", "admin journey", "--as", "admin1", "--run", "r1", "--json")
	require.NoError(t, err, r.stdout+r.stderr)
	var out map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &out), r.stdout)
	assert.NotContains(t, out, "token")
	assert.NotContains(t, out, "accessToken")
	var view struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
	}
	require.NoError(t, json.Unmarshal(out["identity"], &view))
	var identity map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(out["identity"], &identity))
	assert.NotContains(t, identity, "token")
	assert.Equal(t, hubprofile.KindTestHubAdmin, view.Kind)
	kind, delegation := e.target.IdentityKind(view.ID)
	assert.Equal(t, hubprofile.KindTestHubAdmin, kind)
	assert.Equal(t, hubprofiletest.HubAdminDelegation, delegation)
	issue := e.lastTargetRequest(t, "POST", "/api/v1/test-identities/admin")
	assertAdminIssuerAuthorization(t, issue, hubprofiletest.HubAdminDelegation)
	assert.Equal(t, "r1", issue.Header.Get("X-Scion-Run-Label"))
	cache := filepath.Join(e.home, ".scion", "hub-profiles", "t9", "run-r1", "admin1.json")
	fi, err := os.Lstat(cache)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// Token re-issue through the admin delegation, into the cache.
	hits := e.target.TokenHits()
	r, err = e.run(t, "--hub-profile", "t9", "hub", "test-identity", "token", "--as", "admin1", "--run", "r1")
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Equal(t, hits+1, e.target.TokenHits())
	assertAdminIssuerAuthorization(t, e.lastTargetRequest(t, "POST", "/api/v1/test-identities/"+view.ID+"/token"), hubprofiletest.HubAdminDelegation)

	// Web-login code through the admin delegation: the target refuses it
	// while its admin web session setting is off, and mints it once on.
	dir := resolvedTempDir(t)
	_, err = e.run(t, "--hub-profile", "t9", "hub", "test-identity", "web-code", view.ID, "--admin-role", "hub-admin", "--out", filepath.Join(dir, "off.txt"))
	require.Error(t, err)
	_, statErr := os.Lstat(filepath.Join(dir, "off.txt"))
	assert.True(t, os.IsNotExist(statErr), "a refused mint leaves no file")
	e.target.AdminWebSessions = true
	codeFile := filepath.Join(dir, "admin-code.txt")
	r, err = e.run(t, "--hub-profile", "t9", "hub", "test-identity", "web-code", view.ID, "--admin-role", "hub-admin", "--out", codeFile)
	require.NoError(t, err, r.stdout+r.stderr)
	codes := e.target.WebCodes()
	require.Len(t, codes, 1)
	data, err := os.ReadFile(codeFile)
	require.NoError(t, err)
	assert.Equal(t, codes[0].Code, string(data))
	assertAdminIssuerAuthorization(t, e.lastTargetRequest(t, "POST", "/web-login-code"), hubprofiletest.HubAdminDelegation)
	// Without --admin-role the member delegation is used, and the target
	// finds no such identity for it.
	_, err = e.run(t, "--hub-profile", "t9", "hub", "test-identity", "web-code", view.ID, "--out", filepath.Join(dir, "member.txt"))
	require.Error(t, err)
	assert.Equal(t, hubprofiletest.Delegation, e.lastTargetRequest(t, "POST", "/web-login-code").Header.Get("X-Scion-Issuer-Delegation"))

	// delete --as refuses an admin identity before any request.
	before := len(e.target.Recorded())
	_, err = e.run(t, "--hub-profile", "t9", "hub", "test-identity", "delete", "--as", "admin1", "--run", "r1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not deleted through a delegation")
	assert.Len(t, e.target.Recorded(), before)

	// Teardown: left to expire, dropped from the cache.
	r, err = e.run(t, "--hub-profile", "t9", "hub", "test-identity", "teardown", "--run", "r1")
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, view.ID+" (admin1): left-to-expire")
	_, statErr = os.Lstat(cache)
	assert.True(t, os.IsNotExist(statErr), "the cache file is gone")
	assert.Contains(t, e.target.LiveIdentityIDs(), view.ID, "the delegation deleted nothing")

	e.assertClean(t)
	projAfter, homeAfter := e.snapshot(t)
	assert.Equal(t, projBefore, projAfter)
	assert.Equal(t, homeBefore, homeAfter)
}

// C1 refusals: in agent mode issue-admin without a profile, and with a
// profile for the agent's own hub, is refused before any request; so are a
// missing or invalid --admin-role, a missing --as or --purpose, and a
// profile without that tier's delegation. In human mode it needs a profile
// too, and with one it issues as in agent mode.
func TestHubTestIdentityIssueAdmin_Refusals(t *testing.T) {
	e := newX3bEnv(t)
	e.saveAdminProfiles(t)
	base := []string{"hub", "test-identity", "issue-admin", "--purpose", "admin journey", "--run", "r1"}
	for _, tc := range []struct {
		name string
		args []string
		is   error
		want string
	}{
		{"agent mode without a profile", append(append([]string{}, base...), "--admin-role", "hub-admin", "--as", "a"), nil, "runs only against a target-hub profile"},
		{"a profile for the agent's own hub", append(append([]string{"--hub-profile", "own9"}, base...), "--admin-role", "hub-admin", "--as", "a"), hubprofile.ErrOwnHub, ""},
		{"no --admin-role", append(append([]string{"--hub-profile", "t9"}, base...), "--as", "a"), nil, "admin-role"},
		{"an invalid --admin-role", append(append([]string{"--hub-profile", "t9"}, base...), "--admin-role", "admin", "--as", "a"), nil, "invalid --admin-role"},
		{"member is not an admin role", append(append([]string{"--hub-profile", "t9"}, base...), "--admin-role", "member", "--as", "a"), nil, "invalid --admin-role"},
		{"no --as", append(append([]string{"--hub-profile", "t9"}, base...), "--admin-role", "hub-admin"), nil, "--as <alias>"},
		{"no --purpose", []string{"--hub-profile", "t9", "hub", "test-identity", "issue-admin", "--admin-role", "hub-admin", "--as", "a", "--run", "r1"}, nil, "--purpose is required"},
		{"a member-only profile", append(append([]string{"--hub-profile", "t1"}, base...), "--admin-role", "hub-admin", "--as", "a"), nil, "no hub-admin issuer delegation"},
		{"--admin-role on web-code without a profile", []string{"hub", "test-identity", "web-code", "x", "--admin-role", "hub-admin", "--out", filepath.Join(t.TempDir(), "c.txt")}, nil, "runs only against a target-hub profile"},
	} {
		_, err := e.run(t, tc.args...)
		require.Error(t, err, tc.name)
		if tc.is != nil {
			assert.ErrorIs(t, err, tc.is, tc.name)
		} else {
			assert.Contains(t, err.Error(), tc.want, tc.name)
		}
	}
	assert.Empty(t, e.target.Recorded(), "refusals send no request")
	_, err := os.Lstat(filepath.Join(e.home, ".scion", "hub-profiles", "t9", "run-r1"))
	assert.True(t, os.IsNotExist(err), "no cache was written")

	// Human mode: still no path without a profile.
	t.Setenv("SCION_CLI_MODE", "human")
	_, err = e.run(t, append(append([]string{}, base...), "--admin-role", "hub-admin", "--as", "a")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue-admin needs a hub profile")
	_, err = e.run(t, "hub", "test-identity", "web-code", "x", "--admin-role", "hub-admin", "--out", filepath.Join(t.TempDir(), "c.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--admin-role needs a hub profile")
	assert.Empty(t, e.target.Recorded())
	// With a profile, human mode issues a test super-admin as agent mode
	// would.
	r, err := e.run(t, append(append([]string{"--hub-profile", "t9"}, base...), "--admin-role", "super-admin", "--as", "s1")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, "Issued test identity ")
	assertAdminIssuerAuthorization(t, e.lastTargetRequest(t, "POST", "/api/v1/test-identities/admin"), hubprofiletest.SuperAdminDelegation)
	e.assertClean(t)
}
