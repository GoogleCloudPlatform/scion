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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

func validProfile() *Profile {
	return &Profile{
		Name: "t1", Endpoint: "https://T1.example.com:443/", Transport: TransportIAP,
		IAPAudience: testIAPAudience, UATServiceAccount: testUATSA, IssuerServiceAccount: testIssuerSA,
		IssuerDelegations: map[string]string{TierMember: testDelegation},
	}
}

func TestProfileValidate(t *testing.T) {
	require.NoError(t, validProfile().Validate())
	cases := []struct {
		name   string
		mutate func(p *Profile)
		want   string
	}{
		{"bad name", func(p *Profile) { p.Name = "../x" }, "invalid profile name"},
		{"upper name", func(p *Profile) { p.Name = "T1" }, "invalid profile name"},
		{"no endpoint", func(p *Profile) { p.Endpoint = "" }, "endpoint is required"},
		{"http remote", func(p *Profile) { p.Endpoint = "http://t1.example.com" }, "https is required"},
		{"userinfo", func(p *Profile) { p.Endpoint = "https://u:p@t1.example.com" }, "no user info"},
		{"query", func(p *Profile) { p.Endpoint = "https://t1.example.com/?a=b" }, "no user info"},
		{"bad transport", func(p *Profile) { p.Transport = "cloudrun" }, "invalid transport"},
		{"iap without audience", func(p *Profile) { p.IAPAudience = "" }, "--iap-audience"},
		{"iap without uat sa", func(p *Profile) { p.UATServiceAccount = "" }, "--uat-sa"},
		{"none with audience", func(p *Profile) { p.Transport = TransportNone }, "needs transport iap"},
		{"bad issuer sa", func(p *Profile) { p.IssuerServiceAccount = "someone@example.com" }, "--issuer-sa"},
		{"admin tier without the admin issuer sa", func(p *Profile) { p.IssuerDelegations[TierHubAdmin] = "d" }, "--admin-issuer-sa"},
		{"super-admin tier without the admin issuer sa", func(p *Profile) { p.IssuerDelegations[TierSuperAdmin] = "d" }, "--admin-issuer-sa"},
		{"admin issuer sa equal to the issuer sa", func(p *Profile) {
			p.IssuerDelegations[TierHubAdmin] = "d"
			p.AdminIssuerServiceAccount = testIssuerSA
		}, "must differ"},
		{"bad admin issuer sa", func(p *Profile) { p.AdminIssuerServiceAccount = "someone@example.com" }, "--admin-issuer-sa"},
		{"legacy admin tier name", func(p *Profile) { p.IssuerDelegations = map[string]string{"admin": "d"} }, "invalid issuer delegation tier"},
		{"unknown tier", func(p *Profile) { p.IssuerDelegations = map[string]string{"viewer": "d"} }, "must be \"member\""},
		{"bad delegation id", func(p *Profile) { p.IssuerDelegations = map[string]string{TierMember: "a/b"} }, "invalid issuer delegation id"},
		{"delegation without issuer sa", func(p *Profile) { p.IssuerServiceAccount = "" }, "needs the target's issuer service account"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := validProfile()
			tc.mutate(p)
			err := p.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
	// Admin delegations (X4b) with the admin issuer account, with or
	// without a member delegation; each tier is read only by its own name.
	p0 := validProfile()
	p0.AdminIssuerServiceAccount = "t1-admin-issuer@proj.iam.gserviceaccount.com"
	p0.IssuerDelegations[TierHubAdmin] = "dh"
	p0.IssuerDelegations[TierSuperAdmin] = "ds"
	require.NoError(t, p0.Validate())
	assert.Equal(t, "dh", p0.Delegation(TierHubAdmin))
	assert.Equal(t, "ds", p0.Delegation(TierSuperAdmin))
	assert.Equal(t, testDelegation, p0.Delegation(TierMember))
	assert.Empty(t, p0.Delegation("admin"))
	adminOnly := validProfile()
	adminOnly.IssuerServiceAccount = ""
	adminOnly.AdminIssuerServiceAccount = "t1-admin-issuer@proj.iam.gserviceaccount.com"
	adminOnly.IssuerDelegations = map[string]string{TierHubAdmin: "dh"}
	require.NoError(t, adminOnly.Validate(), "an admin delegation needs no member issuer account")
	assert.Empty(t, adminOnly.MemberDelegation())

	// http is accepted on a loopback host (local test hubs).
	p := validProfile()
	p.Endpoint = "http://127.0.0.1:8080"
	assert.NoError(t, p.Validate())
}

func TestParseDelegations(t *testing.T) {
	got, err := ParseDelegations([]string{"member=d1"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"member": "d1"}, got)
	_, err = ParseDelegations([]string{"member=d1", "member=d2"})
	assert.Error(t, err)
	_, err = ParseDelegations([]string{"member"})
	assert.Error(t, err)
	got, err = ParseDelegations(nil)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestNormalizeEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://T1.Example.com:443/":     "https://t1.example.com",
		"https://t1.example.com:8443/api": "https://t1.example.com:8443/api",
		"http://localhost:80":             "http://localhost",
		"http://[::1]:9000/":              "http://[::1]:9000",
	} {
		got, err := NormalizeEndpoint(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
}

func TestCheckNotOwnHub(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	own := env(map[string]string{"SCION_HUB_ENDPOINT": "https://hub.example.com/"})
	for _, ep := range []string{
		"https://hub.example.com",
		"https://HUB.example.com:443",
		"http://hub.example.com:443", // same effective port
		"http://hub.example.com",     // both default ports: http and https of one host
		"https://hub.example.com/other/path",
		"https://hub.example.com.", // trailing dot
		"https://hub.example.com.:443/",
	} {
		assert.ErrorIs(t, CheckNotOwnHub(ep, own), ErrOwnHub, ep)
	}
	assert.NoError(t, CheckNotOwnHub("https://t1.example.com", own))
	assert.NoError(t, CheckNotOwnHub("https://hub.example.com:8443", own), "another explicit port is another hub")

	// Loopback aliases and IP literal forms are one host.
	for _, tc := range []struct{ own, target string }{
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080/"},
		{"http://localhost:8080", "http://127.0.0.1:8080"},
		{"http://127.0.0.1:8080", "http://localhost.:8080"},
		{"http://[::1]:8080", "http://[0:0:0:0:0:0:0:1]:8080"},
		{"http://127.0.0.1:8080", "http://[::ffff:127.0.0.1]:8080"},
		{"http://[::1]:8080", "http://127.0.0.2:8080"},
		// The container bridge address of the agent's own hub.
		{"http://host.docker.internal:8080", "http://localhost:8080"},
		{"http://host.containers.internal:8080", "http://127.0.0.1:8080"},
		{"http://HOST.DOCKER.INTERNAL.:8080", "http://[::1]:8080"},
	} {
		assert.ErrorIs(t, CheckNotOwnHub(tc.target, env(map[string]string{"SCION_HUB_ENDPOINT": tc.own})), ErrOwnHub, "%s vs %s", tc.target, tc.own)
	}
	local := env(map[string]string{"SCION_HUB_ENDPOINT": "http://127.0.0.1:8080"})
	assert.NoError(t, CheckNotOwnHub("http://127.0.0.1:9090", local), "local test hubs on two ports")
	assert.NoError(t, CheckNotOwnHub("http://localhost:9090", local))

	// SCION_HUB_URL counts too.
	assert.ErrorIs(t, CheckNotOwnHub("https://hub2.example.com", env(map[string]string{"SCION_HUB_URL": "https://hub2.example.com"})), ErrOwnHub)
	// No own hub (a workstation): nothing to refuse.
	assert.NoError(t, CheckNotOwnHub("https://hub.example.com", env(nil)))

	// Fail closed: an own-hub value that does not parse to an absolute URL
	// refuses every profile, instead of comparing a raw string.
	for _, bad := range []string{"hub.example.com:443", "hub.example.com", "//hub.example.com", "ftp://hub.example.com", "https://user@hub.example.com"} {
		err := CheckNotOwnHub("https://t1.example.com", env(map[string]string{"SCION_HUB_ENDPOINT": bad}))
		assert.ErrorIs(t, err, ErrOwnHubUnknown, bad)
	}
	// User info in the target is refused outright.
	assert.Error(t, CheckNotOwnHub("https://user@t1.example.com", own))
}

func TestSaveLoadListRemove(t *testing.T) {
	home := t.TempDir()
	p := validProfile()
	require.NoError(t, Save(home, p, false))

	path := filepath.Join(Dir(home), "t1.yaml")
	assertPrivateFile(t, path)
	assertPrivateDir(t, Dir(home))

	got, err := Load(home, "t1")
	require.NoError(t, err)
	assert.Equal(t, "https://t1.example.com", got.Endpoint, "stored normalized")
	assert.Equal(t, testDelegation, got.MemberDelegation())

	// The file holds only the non-secret fields.
	data, _ := os.ReadFile(path)
	for _, k := range []string{"token", "secret", "key"} {
		assert.NotContains(t, strings.ToLower(string(data)), k)
	}

	err = Save(home, validProfile(), false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	require.NoError(t, Save(home, validProfile(), true))

	names, err := List(home)
	require.NoError(t, err)
	assert.Equal(t, []string{"t1"}, names)

	require.NoError(t, Remove(home, "t1", false))
	_, err = Load(home, "t1")
	assert.Error(t, err)
}

func TestLoadRefusesUnknownFieldsSymlinksAndOpenModes(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, Save(home, validProfile(), false))
	path := filepath.Join(Dir(home), "t1.yaml")

	data, _ := os.ReadFile(path)
	require.NoError(t, os.WriteFile(path, append(data, []byte("token: abc\n")...), 0o600))
	_, err := Load(home, "t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid file")

	require.NoError(t, os.Remove(path))
	other := filepath.Join(home, "elsewhere.yaml")
	require.NoError(t, os.WriteFile(other, data, 0o600))
	wintest.MustSymlink(t, other, path)
	_, err = Load(home, "t1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
	err = Save(home, validProfile(), true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symlink")
}

func TestRemoveRefusesCachedRuns(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, Save(home, validProfile(), false))
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	require.NoError(t, res.Fill(&CacheEntry{Alias: "owner", Run: "r1", Profile: "t1", IdentityID: "fx-1", Token: "T", ExpiresAt: time.Now().Add(time.Hour)}))

	err = Remove(home, "t1", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cached runs (r1)")
	require.NoError(t, Remove(home, "t1", true))
	_, err = os.Stat(filepath.Join(Dir(home), "t1"))
	assert.True(t, os.IsNotExist(err))
}
