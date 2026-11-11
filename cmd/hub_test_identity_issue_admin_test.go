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

	"github.com/spf13/cobra"
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

// resetAdminRoleFlags restores, when the test ends, every flag value and
// pflag Changed state these tests can set: the local flags of the hub
// test-identity, hub profile and hub status commands (including the
// variables they share with WS3 and A4: --admin-role, --purpose, --ttl,
// --lifetime, --json, --out, --purge), the root --hub-profile and the
// --as and --run flags of the hub and artifact trees. A later test that
// drives rootCmd without resetting flags then inherits nothing from them.
func resetAdminRoleFlags(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		for _, name := range []string{"hub-profile", "hub", "no-hub", "format", "global"} {
			if f := rootCmd.PersistentFlags().Lookup(name); f != nil {
				require.NoError(t, f.Value.Set(f.DefValue))
				f.Changed = false
			}
		}
		for _, c := range []*cobra.Command{hubCmd, artifactCmd} {
			for _, name := range []string{"as", "run"} {
				if f := c.PersistentFlags().Lookup(name); f != nil {
					require.NoError(t, f.Value.Set(f.DefValue))
					f.Changed = false
				}
			}
		}
		var walk func(c *cobra.Command)
		walk = func(c *cobra.Command) {
			resetCommandFlags(t, c)
			for _, sub := range c.Commands() {
				walk(sub)
			}
		}
		for _, p := range []string{"hub.test-identity", "hub.profile", "hub.status"} {
			walk(realCommand(t, p))
		}
		// Belt and braces: the shared variables themselves.
		testIdentityIssueAdminRole, testIdentityWebCodeAdminRole, testIdentityAdminRole = "", "", ""
		testIdentityPurpose, testIdentityOut, testIdentityJSON = "", "", false
		testIdentityTTL, testIdentityLifetime = 0, 0
	})
}

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
	resetAdminRoleFlags(t)
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
	resetAdminRoleFlags(t)
	e.saveAdminProfiles(t)
	base := []string{"hub", "test-identity", "issue-admin", "--purpose", "admin journey", "--run", "r1"}
	// Without a profile the gate refuses --as and --run first, so these
	// carry neither.
	noProfile := []string{"hub", "test-identity", "issue-admin", "--purpose", "admin journey", "--admin-role", "hub-admin"}
	for _, tc := range []struct {
		name string
		args []string
		is   error
		want string
	}{
		{"agent mode without a profile", noProfile, nil, "runs only against a target-hub profile"},
		{"--as without a profile", append(append([]string{}, base...), "--admin-role", "hub-admin", "--as", "a"), nil, "--as needs a hub profile"},
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
	_, err = e.run(t, noProfile...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue-admin needs a hub profile")
	_, err = e.run(t, append(append([]string{}, base...), "--admin-role", "hub-admin", "--as", "a")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--as needs a hub profile", "the profile gate refuses --as first")
	_, err = e.run(t, "hub", "test-identity", "web-code", "x", "--admin-role", "hub-admin", "--out", filepath.Join(t.TempDir(), "c.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--admin-role needs a hub profile")
	assert.Empty(t, e.target.Recorded())
	// With a profile, human mode issues a test super-admin as agent mode
	// would.
	r, err := e.run(t, append(append([]string{"--hub-profile", "t9"}, base...), "--admin-role", "super-admin", "--as", "s1")...)
	require.NoError(t, err, r.stdout+r.stderr)
	assert.Contains(t, r.stdout, "Issued test identity ")
	assert.Contains(t, r.stdout, "  Kind:          "+hubprofile.KindTestSuperAdmin+"\n", "the plain-text output names the kind")
	assertAdminIssuerAuthorization(t, e.lastTargetRequest(t, "POST", "/api/v1/test-identities/admin"), hubprofiletest.SuperAdminDelegation)
	e.assertClean(t)
}

// A4-style golden test for issue-admin: exact standard output (only the
// identity's ID, email, kind, role, expiries and the cache path), empty
// standard error, the cache file at mode 0600 holding the token, and an
// existing alias refused before any request, leaving the file unchanged.
func TestHubTestIdentityIssueAdmin_GoldenOutputAndFile(t *testing.T) {
	e := newX3bEnv(t)
	resetAdminRoleFlags(t)
	e.saveAdminProfiles(t)
	r, err := e.run(t, "--hub-profile", "t9", "hub", "test-identity", "issue-admin", "--admin-role", "hub-admin",
		"--purpose", "admin journey", "--as", "admin1", "--run", "r1")
	require.NoError(t, err, r.stdout+r.stderr)
	cache := filepath.Join(e.home, ".scion", "hub-profiles", "t9", "run-r1", "admin1.json")
	ent, err := hubprofile.ReadEntry(e.home, "t9", "r1", "admin1")
	require.NoError(t, err)
	want := "Issued test identity " + ent.IdentityID + " as \"admin1\" (run r1, profile t9)\n" +
		"  Email:         " + ent.Email + "\n" +
		"  Kind:          " + hubprofile.KindTestHubAdmin + "\n" +
		"  Role:          member\n" +
		"  Expires:       " + formatTestIdentityTime(ent.ExpiresAt) + "\n" +
		"  Token expires: " + formatTestIdentityTime(ent.TokenExpiresAt) + "\n" +
		"  Cache file:    " + cache + " (mode 0600)\n"
	assert.Equal(t, want, r.stdout)
	assert.Empty(t, r.stderr)
	fi, err := os.Lstat(cache)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	require.NotEmpty(t, ent.Token)
	assert.NotContains(t, r.stdout, ent.Token)
	before, err := os.ReadFile(cache)
	require.NoError(t, err)

	// The alias exists: refused before any request, the file unchanged.
	n := len(e.target.Recorded())
	_, err = e.run(t, "--hub-profile", "t9", "hub", "test-identity", "issue-admin", "--admin-role", "hub-admin",
		"--purpose", "admin journey", "--as", "admin1", "--run", "r1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	assert.Len(t, e.target.Recorded(), n, "no request for a taken alias")
	after, err := os.ReadFile(cache)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	e.assertClean(t)
}

// The two admin issuance commands in both directions (option 1, A4 kept):
// A4's issue --admin-role still refuses a hub profile (human mode, pointing
// to issue-admin) and agent mode, and issue-admin still refuses outside a
// target profile (without one in human and agent mode, and with a profile
// for the agent's own hub). No request, no file, no cache.
func TestHubTestIdentityAdminCommands_EachInItsContext(t *testing.T) {
	e := newX3bEnv(t)
	resetAdminRoleFlags(t)
	e.saveAdminProfiles(t)
	dir := resolvedTempDir(t)
	a4 := func(out string) []string {
		return []string{"hub", "test-identity", "issue", "--admin-role", "hub-admin", "--purpose", "admin journey", "--out", filepath.Join(dir, out)}
	}

	// Agent mode: A4's issue --admin-role is refused with and without a profile.
	_, err := e.run(t, append([]string{"--hub-profile", "t9"}, a4("agent-profile.token")...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not available in agent mode")
	_, err = e.run(t, a4("agent.token")...)
	require.Error(t, err)

	// Human mode: A4's issue --admin-role refuses a hub profile and names
	// the runner's command.
	t.Setenv("SCION_CLI_MODE", "human")
	_, err = e.run(t, append([]string{"--hub-profile", "t9"}, a4("human-profile.token")...)...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue --admin-role is not available with a hub profile")
	assert.Contains(t, err.Error(), "issue-admin")

	// issue-admin outside a target profile: refused in human mode without a
	// profile, and in agent mode without one or with the agent's own hub.
	noProfile := []string{"hub", "test-identity", "issue-admin", "--admin-role", "hub-admin", "--purpose", "admin journey"}
	_, err = e.run(t, noProfile...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "issue-admin needs a hub profile")
	t.Setenv("SCION_CLI_MODE", "agent")
	_, err = e.run(t, noProfile...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runs only against a target-hub profile")
	_, err = e.run(t, append([]string{"--hub-profile", "own9"}, append(noProfile, "--as", "a", "--run", "r1")...)...)
	require.ErrorIs(t, err, hubprofile.ErrOwnHub)

	for _, f := range []string{"agent-profile.token", "agent.token", "human-profile.token"} {
		_, statErr := os.Lstat(filepath.Join(dir, f))
		assert.True(t, os.IsNotExist(statErr), "%s must not exist", f)
	}
	assert.Empty(t, e.target.Recorded(), "no request in any refused context")
	_, statErr := os.Lstat(filepath.Join(e.home, ".scion", "hub-profiles", "t9"))
	assert.True(t, os.IsNotExist(statErr), "no run cache")
	e.assertClean(t)
}
