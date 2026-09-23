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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Security tests for HMAC capability URLs on local-storage skill file
// downloads (#1792).

// ---------------------------------------------------------------------------
// Unit tests: signing primitive
// ---------------------------------------------------------------------------

func testSigningServer() *Server {
	return &Server{downloadSigningKey: bytes.Repeat([]byte{0x42}, 32)}
}

// signedFileRequest builds a GET for skillID/filePath@version carrying the
// given exp/sig query values.
func signedFileRequest(skillID, version, filePath, exp, sig string) *http.Request {
	q := url.Values{}
	q.Set("raw", "1")
	q.Set("version", version)
	q.Set("exp", exp)
	q.Set("sig", sig)
	return httptest.NewRequest(http.MethodGet, "/api/v1/skills/"+skillID+"/files/"+filePath+"?"+q.Encode(), nil)
}

// requestWithSuffix builds a GET for skillID/filePath@version followed by the
// "exp=...&sig=..." suffix produced by signDownloadURL.
func requestWithSuffix(skillID, version, filePath, suffix string) *http.Request {
	return httptest.NewRequest(http.MethodGet,
		"/api/v1/skills/"+skillID+"/files/"+filePath+"?raw=1&version="+url.QueryEscape(version)+"&"+suffix, nil)
}

func TestSkillFileSignature_Verify(t *testing.T) {
	s := testSigningServer()
	now := time.Unix(1_800_000_000, 0)
	const skillID, version, path = "skill-a", "1.0.0", "scripts/run.sh"
	suffix := s.signDownloadURL(skillID, version, path, now.Add(skillFileURLTTL))
	require.NotEmpty(t, suffix)
	valid := requestWithSuffix(skillID, version, path, suffix)
	q := valid.URL.Query()
	exp, sig := q.Get("exp"), q.Get("sig")

	t.Run("valid signature", func(t *testing.T) {
		assert.True(t, s.verifySkillFileSignature(valid, skillID, version, path, now))
	})
	t.Run("replay within TTL", func(t *testing.T) {
		assert.True(t, s.verifySkillFileSignature(valid, skillID, version, path, now.Add(time.Minute)))
		assert.True(t, s.verifySkillFileSignature(valid, skillID, version, path, now.Add(skillFileURLTTL)))
	})
	t.Run("expired", func(t *testing.T) {
		assert.False(t, s.verifySkillFileSignature(valid, skillID, version, path, now.Add(skillFileURLTTL+time.Second)))
	})
	t.Run("tampered signature", func(t *testing.T) {
		b := []byte(sig)
		if b[0] == 'A' {
			b[0] = 'B'
		} else {
			b[0] = 'A'
		}
		r := signedFileRequest(skillID, version, path, exp, string(b))
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("truncated signature", func(t *testing.T) {
		r := signedFileRequest(skillID, version, path, exp, sig[:len(sig)-2])
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("empty signature", func(t *testing.T) {
		r := signedFileRequest(skillID, version, path, exp, "")
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("non-base64 signature", func(t *testing.T) {
		r := signedFileRequest(skillID, version, path, exp, "!!!not-base64!!!")
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("non-canonical base64 encoding of the valid MAC", func(t *testing.T) {
		// A 32-byte MAC is 43 base64 chars; the last char carries 2 unused
		// low bits. Setting one yields a string a lenient decoder maps to the
		// same MAC. Only the canonical encoding is accepted.
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
		require.Len(t, sig, 43)
		last := strings.IndexByte(alphabet, sig[len(sig)-1])
		require.GreaterOrEqual(t, last, 0)
		alt := sig[:len(sig)-1] + string(alphabet[last^1])
		r := signedFileRequest(skillID, version, path, exp, alt)
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("tampered exp (extended)", func(t *testing.T) {
		e, _ := strconv.ParseInt(exp, 10, 64)
		r := signedFileRequest(skillID, version, path, strconv.FormatInt(e+60, 10), sig)
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("non-canonical exp encodings", func(t *testing.T) {
		for _, bad := range []string{"+" + exp, "0" + exp, exp + ".0", " " + exp, "abc", "", "-1"} {
			r := signedFileRequest(skillID, version, path, bad, sig)
			assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now), "exp=%q", bad)
		}
	})
	t.Run("cross-skill", func(t *testing.T) {
		assert.False(t, s.verifySkillFileSignature(valid, "skill-b", version, path, now))
	})
	t.Run("cross-file", func(t *testing.T) {
		assert.False(t, s.verifySkillFileSignature(valid, skillID, version, "SKILL.md", now))
	})
	t.Run("cross-version", func(t *testing.T) {
		assert.False(t, s.verifySkillFileSignature(valid, skillID, "1.0.1", path, now))
	})
	t.Run("different key", func(t *testing.T) {
		other := &Server{downloadSigningKey: bytes.Repeat([]byte{0x43}, 32)}
		assert.False(t, other.verifySkillFileSignature(valid, skillID, version, path, now))
	})
	t.Run("no key configured", func(t *testing.T) {
		assert.False(t, (&Server{}).verifySkillFileSignature(valid, skillID, version, path, now))
		assert.Empty(t, (&Server{}).signDownloadURL(skillID, version, path, now.Add(skillFileURLTTL)))
	})
	t.Run("repeated parameters rejected", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, valid.URL.RequestURI()+"&sig="+url.QueryEscape(sig), nil)
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
		r = httptest.NewRequest(http.MethodGet, valid.URL.RequestURI()+"&exp="+exp, nil)
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("exp beyond TTL rejected even if correctly signed", func(t *testing.T) {
		far := s.signDownloadURL(skillID, version, path, now.Add(24*time.Hour))
		r := requestWithSuffix(skillID, version, path, far)
		assert.False(t, s.verifySkillFileSignature(r, skillID, version, path, now))
	})
	t.Run("field boundaries are unambiguous", func(t *testing.T) {
		// Moving a character across the id/version or version/path boundary
		// must not produce the same MAC.
		assert.NotEqual(t,
			skillFileSignature(s.downloadSigningKey, "ab", "c", "d", 1),
			skillFileSignature(s.downloadSigningKey, "a", "bc", "d", 1))
		assert.NotEqual(t,
			skillFileSignature(s.downloadSigningKey, "a", "b", "cd", 1),
			skillFileSignature(s.downloadSigningKey, "a", "bc", "d", 1))
	})
}

func TestIsSignedSkillFileRequest(t *testing.T) {
	const q = "?raw=1&version=1.0.0&exp=1&sig=x"
	const id = "0b6d3c1e-5a4f-4c2b-9e8d-7f6a5b4c3d2e"
	cases := []struct {
		method, target string
		want           bool
	}{
		{http.MethodGet, "/api/v1/skills/" + id + "/files/SKILL.md" + q, true},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/a/b/c.sh" + q, true},
		{http.MethodPut, "/api/v1/skills/" + id + "/files/SKILL.md" + q, false},
		{http.MethodPost, "/api/v1/skills/" + id + "/files/SKILL.md" + q, false},
		{http.MethodDelete, "/api/v1/skills/" + id + "/files/SKILL.md" + q, false},
		{http.MethodHead, "/api/v1/skills/" + id + "/files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/" + q, false},
		{http.MethodGet, "/api/v1/skills//files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/download" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/versions/v" + q, false},
		{http.MethodGet, "/api/v1/templates/" + id + "/files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/agents/x/api/v1/skills/" + id + "/files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/SKILL.md?raw=1&version=1.0.0&exp=1", false},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/SKILL.md?raw=1&version=1.0.0&sig=x", false},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/SKILL.md?raw=1&version=1.0.0&exp=&sig=", false},
		// Reserved / non-UUID IDs are never admitted anonymously.
		{http.MethodGet, "/api/v1/skills/resolve/files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/id/files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/" + strings.ToUpper(id) + "/files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/{" + id + "}/files/SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/urn:uuid:" + id + "/files/SKILL.md" + q, false},
		// Unclean paths (the mux would redirect or reinterpret them).
		{http.MethodGet, "/api/v1/skills/" + id + "/files/a/../SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/./SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/a//SKILL.md" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/files/SKILL.md/" + q, false},
		{http.MethodGet, "/api/v1/skills/" + id + "/../resolve/files/SKILL.md" + q, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.target, nil)
		assert.Equal(t, c.want, isSignedSkillFileRequest(r), "%s %s", c.method, c.target)
	}
}

func TestSignSkillFileDownloadURLs(t *testing.T) {
	const sk = "0b6d3c1e-5a4f-4c2b-9e8d-7f6a5b4c3d2e"
	s := testSigningServer()
	now := time.Now()
	urls := s.signSkillFileDownloadURLs([]DownloadURLInfo{
		{Path: "SKILL.md", URL: "/api/v1/skills/"+sk+"/files/SKILL.md?raw=1&version=1.0.0"},
		{Path: "a/b.sh", URL: "https://hub.example.com/api/v1/skills/"+sk+"/files/a/b.sh?raw=1&version=1.0.0"},
		{Path: "gcs.md", URL: "https://storage.googleapis.com/b/o?X-Goog-Signature=abc"},
		{Path: "other.md", URL: "/api/v1/skills/other-skill/files/other.md?raw=1&version=1.0.0"},
	}, sk, "1.0.0", now)

	for _, i := range []int{0, 1} {
		u, err := url.Parse(urls[i].URL)
		require.NoError(t, err)
		assert.Equal(t, "1", u.Query().Get("raw"))
		assert.Equal(t, "1.0.0", u.Query().Get("version"))
		assert.Equal(t, strconv.FormatInt(now.Add(skillFileURLTTL).Unix(), 10), u.Query().Get("exp"))
		r := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
		assert.True(t, s.verifySkillFileSignature(r, sk, "1.0.0", urls[i].Path, now), urls[i].URL)
		assert.True(t, isSignedSkillFileRequest(r), urls[i].URL)
	}
	// Order of parameters matches the documented URL format.
	assert.Regexp(t, `\?raw=1&version=1\.0\.0&exp=\d+&sig=[A-Za-z0-9_-]+$`, urls[0].URL)
	// Cloud presigned URLs and other skills' URLs are left untouched.
	assert.Equal(t, "https://storage.googleapis.com/b/o?X-Goog-Signature=abc", urls[2].URL)
	assert.Equal(t, "/api/v1/skills/other-skill/files/other.md?raw=1&version=1.0.0", urls[3].URL)

	// Without a key, URLs are issued unsigned.
	unsigned := (&Server{}).signSkillFileDownloadURLs([]DownloadURLInfo{
		{Path: "SKILL.md", URL: "/api/v1/skills/"+sk+"/files/SKILL.md?raw=1&version=1.0.0"},
	}, sk, "1.0.0", now)
	assert.Equal(t, "/api/v1/skills/"+sk+"/files/SKILL.md?raw=1&version=1.0.0", unsigned[0].URL)
}

// ---------------------------------------------------------------------------
// Key provisioning
// ---------------------------------------------------------------------------

func TestDownloadSigningKey_ProvisionedAndPersisted(t *testing.T) {
	srv, _ := testServer(t)
	require.Len(t, srv.downloadSigningKey, 32)

	// The key is dedicated: distinct from the agent and user token keys.
	require.NotNil(t, srv.agentTokenService)
	require.NotNil(t, srv.userTokenService)
	assert.NotEqual(t, srv.agentTokenService.config.SigningKey, srv.downloadSigningKey)
	assert.NotEqual(t, srv.userTokenService.config.SigningKey, srv.downloadSigningKey)

	// It is persisted: resolving the key again yields the same bytes, so
	// URLs survive a Hub restart.
	again, err := srv.ensureSigningKey(context.Background(), SecretKeyDownloadSigningKey, nil)
	require.NoError(t, err)
	assert.Equal(t, srv.downloadSigningKey, again)
}

func TestDownloadSigningKey_DerivedFromSharedSecret(t *testing.T) {
	a := &Server{config: ServerConfig{SharedSigningSecret: "shared"}}
	b := &Server{config: ServerConfig{SharedSigningSecret: "shared"}}
	require.NoError(t, a.initDownloadSigningKey(context.Background()))
	require.NoError(t, b.initDownloadSigningKey(context.Background()))
	require.Len(t, a.downloadSigningKey, 32)
	// Replicas sharing the secret validate each other's URLs...
	assert.Equal(t, a.downloadSigningKey, b.downloadSigningKey)
	// ...and the key is domain-separated from the token keys.
	assert.NotEqual(t, deriveSharedSigningKey("shared", SecretKeyAgentSigningKey), a.downloadSigningKey)
	assert.NotEqual(t, deriveSharedSigningKey("shared", SecretKeyUserSigningKey), a.downloadSigningKey)
}

// When the deployment requires stable keys, a missing download key fails
// startup (like the agent/user keys) instead of silently using a per-replica
// ephemeral key that would cause intermittent 401s.
func TestDownloadSigningKey_FailFastWhenStableKeysRequired(t *testing.T) {
	srv, _ := testServer(t)
	// Remove the key persisted at startup so none can be found.
	for _, scopeID := range []string{srv.hubID, "hub", ""} {
		_ = srv.store.DeleteSecret(context.Background(), SecretKeyDownloadSigningKey, store.ScopeHub, scopeID)
	}
	srv.downloadSigningKey = nil
	srv.config.RequireStableSigningKey = true
	err := srv.initDownloadSigningKey(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "download signing key")
	assert.Nil(t, srv.downloadSigningKey)

	// Without the stable-key requirement the same situation provisions a key.
	srv.config.RequireStableSigningKey = false
	require.NoError(t, srv.initDownloadSigningKey(context.Background()))
	assert.Len(t, srv.downloadSigningKey, 32)
}

// ---------------------------------------------------------------------------
// End-to-end through the full middleware chain
// ---------------------------------------------------------------------------

// signedURLFixture is a local-storage Hub with production auth and two
// private skills (A with versions 1.0.0 and 2.0.0, B with 1.0.0), each
// containing SKILL.md and other.md.
type signedURLFixture struct {
	srv          *Server
	alice, bob   *store.User
	project      *store.Project
	skillA       *store.Skill
	skillB       *store.Skill
	secretA      []byte
	secretOtherA []byte
}

func addLocalSkillVersion(t *testing.T, srv *Server, stor storage.Storage, skill *store.Skill, version string, files map[string][]byte) {
	t.Helper()
	ctx := context.Background()
	var manifest []store.TemplateFile
	for p, c := range files {
		_, err := stor.Upload(ctx, skill.StoragePath+"/"+version+"/"+p, bytes.NewReader(c), storage.UploadOptions{})
		require.NoError(t, err)
		manifest = append(manifest, store.TemplateFile{Path: p, Size: int64(len(c)), Hash: sha256Hex(c)})
	}
	require.NoError(t, srv.store.CreateSkillVersion(ctx, &store.SkillVersion{
		ID:          api.NewUUID(),
		SkillID:     skill.ID,
		Version:     version,
		ContentHash: "sha256:" + version,
		Status:      store.SkillVersionStatusPublished,
		Files:       manifest,
		Created:     time.Now(),
	}))
}

func setupSignedURLFixture(t *testing.T) *signedURLFixture {
	t.Helper()
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, Bucket: "b", LocalPath: t.TempDir()})
	require.NoError(t, err)
	srv.SetStorage(stor)

	f := &signedURLFixture{
		srv: srv, alice: alice, bob: bob, project: project,
		secretA:      []byte("# skill A v1 SECRET-A\n"),
		secretOtherA: []byte("# skill A v1 other SECRET-OTHER-A\n"),
	}
	f.skillA = createTestSkill(t, s, "signed-a-"+api.NewUUID()[:8], store.SkillScopeGlobal, "", alice.ID)
	f.skillB = createTestSkill(t, s, "signed-b-"+api.NewUUID()[:8], store.SkillScopeGlobal, "", alice.ID)
	addLocalSkillVersion(t, srv, stor, f.skillA, "1.0.0", map[string][]byte{
		"SKILL.md": f.secretA, "other.md": f.secretOtherA,
	})
	addLocalSkillVersion(t, srv, stor, f.skillA, "2.0.0", map[string][]byte{
		"SKILL.md": []byte("# skill A v2 SECRET-A2\n"), "other.md": []byte("# A v2 other\n"),
	})
	addLocalSkillVersion(t, srv, stor, f.skillB, "1.0.0", map[string][]byte{
		"SKILL.md": []byte("# skill B SECRET-B\n"), "other.md": []byte("# B other SECRET-OTHER-B\n"),
	})
	return f
}

// dispatchURLs pre-resolves skill@version as the agent creator alice, exactly
// as the dispatcher does, and returns the Hub-relative file URLs by path.
func (f *signedURLFixture) dispatchURLs(t *testing.T, skill *store.Skill, version string) map[string]string {
	t.Helper()
	uri := "skill://scion/global/" + skill.Slug + "@" + version
	resp := f.srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(f.alice.ID, f.project.ID, uri))
	require.NotNil(t, resp)
	require.Empty(t, resp.Errors)
	require.Len(t, resp.Resolved, 1)
	out := map[string]string{}
	for _, file := range resp.Resolved[0].Files {
		out[file.Path] = file.URL
	}
	return out
}

// brokerGet issues a credential-less GET through the full Hub handler — the
// shape of the broker's downloadSkillFile request.
func (f *signedURLFixture) brokerGet(target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// swapQuery returns target with query parameter key replaced by value.
func swapQuery(t *testing.T, target, key, value string) string {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.RequestURI()
}

func queryOf(t *testing.T, target string) url.Values {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	return u.Query()
}

func assertDenied(t *testing.T, rec *httptest.ResponseRecorder, leak ...string) {
	t.Helper()
	assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	for _, l := range leak {
		assert.NotContains(t, rec.Body.String(), l)
	}
}

func TestSignedSkillFileURL_EndToEnd(t *testing.T) {
	f := setupSignedURLFixture(t)
	urlsA := f.dispatchURLs(t, f.skillA, "1.0.0")
	urlsB := f.dispatchURLs(t, f.skillB, "1.0.0")
	signedA := urlsA["SKILL.md"]
	require.Contains(t, signedA, "&exp=")
	require.Contains(t, signedA, "&sig=")
	sigA := queryOf(t, signedA).Get("sig")
	expA := queryOf(t, signedA).Get("exp")

	t.Run("valid signature serves private file without credentials", func(t *testing.T) {
		rec := f.brokerGet(signedA)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, f.secretA, rec.Body.Bytes())

		rec = f.brokerGet(urlsA["other.md"])
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, f.secretOtherA, rec.Body.Bytes())
	})

	t.Run("replay within TTL", func(t *testing.T) {
		for i := 0; i < 3; i++ {
			rec := f.brokerGet(signedA)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, f.secretA, rec.Body.Bytes())
		}
	})

	t.Run("tampered signature", func(t *testing.T) {
		b := []byte(sigA)
		b[len(b)/2] ^= 0x01
		assertDenied(t, f.brokerGet(swapQuery(t, signedA, "sig", string(b))), "SECRET")
	})

	t.Run("tampered exp", func(t *testing.T) {
		e, _ := strconv.ParseInt(expA, 10, 64)
		assertDenied(t, f.brokerGet(swapQuery(t, signedA, "exp", strconv.FormatInt(e+1, 10))), "SECRET")
	})

	t.Run("expired token", func(t *testing.T) {
		expired := f.srv.signDownloadURL(f.skillA.ID, "1.0.0", "SKILL.md", time.Now().Add(-time.Second))
		target := "/api/v1/skills/" + f.skillA.ID + "/files/SKILL.md?raw=1&version=1.0.0&" + expired
		assertDenied(t, f.brokerGet(target), "SECRET")
	})

	t.Run("cross-skill token (A's signature on B's file)", func(t *testing.T) {
		target := strings.Replace(signedA, f.skillA.ID, f.skillB.ID, 1)
		require.NotEqual(t, signedA, target)
		assertDenied(t, f.brokerGet(target), "SECRET")
	})

	t.Run("cross-skill token (B's signature on A's file)", func(t *testing.T) {
		target := swapQuery(t, signedA, "sig", queryOf(t, urlsB["SKILL.md"]).Get("sig"))
		target = swapQuery(t, target, "exp", queryOf(t, urlsB["SKILL.md"]).Get("exp"))
		assertDenied(t, f.brokerGet(target), "SECRET")
	})

	t.Run("cross-file token (SKILL.md signature on other.md)", func(t *testing.T) {
		target := strings.Replace(signedA, "/files/SKILL.md", "/files/other.md", 1)
		require.NotEqual(t, signedA, target)
		assertDenied(t, f.brokerGet(target), "SECRET")
	})

	t.Run("cross-version token (1.0.0 signature on 2.0.0)", func(t *testing.T) {
		assertDenied(t, f.brokerGet(swapQuery(t, signedA, "version", "2.0.0")), "SECRET")
	})

	t.Run("signature for nonexistent skill is 401 not 404", func(t *testing.T) {
		target := strings.Replace(signedA, f.skillA.ID, api.NewUUID(), 1)
		assertDenied(t, f.brokerGet(target))
	})

	t.Run("signature does not authorize writes", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPut, signedA, bytes.NewReader([]byte("overwrite")))
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		// Content unchanged.
		rec = f.brokerGet(signedA)
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, f.secretA, rec.Body.Bytes())
	})

	t.Run("signature does not authorize other skill routes", func(t *testing.T) {
		suffix := "?exp=" + expA + "&sig=" + url.QueryEscape(sigA)
		for _, p := range []string{
			"/api/v1/skills/" + f.skillA.ID,
			"/api/v1/skills/" + f.skillA.ID + "/download",
			"/api/v1/skills/" + f.skillA.ID + "/versions",
			"/api/v1/skills/" + f.skillA.ID + "/resolve",
		} {
			rec := f.brokerGet(p + suffix)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s: %s", p, rec.Body.String())
		}
	})

	t.Run("sig without exp (and vice versa) is rejected, no fallback", func(t *testing.T) {
		base := "/api/v1/skills/" + f.skillA.ID + "/files/SKILL.md?raw=1&version=1.0.0"
		assertDenied(t, f.brokerGet(base+"&sig="+url.QueryEscape(sigA)), "SECRET")
		assertDenied(t, f.brokerGet(base+"&exp="+expA), "SECRET")
		// Even for an authorized user, a present-but-invalid signature does not
		// fall back to principal authorization.
		rec := doRawRequestAsUser(t, f.srv, f.alice, http.MethodGet, base+"&exp="+expA+"&sig=bogus", nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
	})

	t.Run("unsigned private-file request uses normal authz (unchanged)", func(t *testing.T) {
		base := "/api/v1/skills/" + f.skillA.ID + "/files/SKILL.md?raw=1&version=1.0.0"
		// No credentials: rejected by the auth middleware.
		assertDenied(t, f.brokerGet(base), "SECRET")
		// Authenticated but without access: hidden as not found.
		rec := doRawRequestAsUser(t, f.srv, f.bob, http.MethodGet, base, nil)
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "SECRET")
		// Authorized user: allowed.
		rec = doRawRequestAsUser(t, f.srv, f.alice, http.MethodGet, base, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, f.secretA, rec.Body.Bytes())
	})

	t.Run("unsigned anonymous handler call on private skill still denied", func(t *testing.T) {
		// Bypass the middleware entirely (defense in depth).
		rec := doAnonymousRequest(f.srv, "/api/v1/skills/"+f.skillA.ID+"/files/SKILL.md?version=1.0.0")
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("request log redacts the signature and labels auth type", func(t *testing.T) {
		var buf bytes.Buffer
		f.srv.SetRequestLogger(slog.New(slog.NewJSONHandler(&buf, nil)))
		defer f.srv.SetRequestLogger(nil)
		rec := f.brokerGet(signedA)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		out := buf.String()
		require.NotEmpty(t, out)
		assert.NotContains(t, out, sigA)
		assert.NotContains(t, out, url.QueryEscape(sigA))
		assert.Contains(t, out, "sig=REDACTED")
		assert.Contains(t, out, `"auth_type":"`+AuthTypeSignedURL+`"`)
	})

	t.Run("reserved skill IDs are not reachable anonymously", func(t *testing.T) {
		suffix := "/files/SKILL.md?raw=1&version=1.0.0&exp=" + expA + "&sig=" + url.QueryEscape(sigA)
		for _, id := range []string{"resolve", strings.ToUpper(f.skillA.ID)} {
			rec := f.brokerGet("/api/v1/skills/" + id + suffix)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s: %s", id, rec.Body.String())
		}
	})

	t.Run("dot-segment paths are rejected, not redirected", func(t *testing.T) {
		target := strings.Replace(signedA, "/files/SKILL.md", "/files/x/../SKILL.md", 1)
		rec := f.brokerGet(target)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
		assert.Empty(t, rec.Header().Get("Location"))
	})

	t.Run("signature from a different hub key is rejected", func(t *testing.T) {
		other := &Server{downloadSigningKey: bytes.Repeat([]byte{0x01}, 32)}
		forged := other.signDownloadURL(f.skillA.ID, "1.0.0", "SKILL.md", time.Now().Add(skillFileURLTTL))
		target := "/api/v1/skills/" + f.skillA.ID + "/files/SKILL.md?raw=1&version=1.0.0&" + forged
		assertDenied(t, f.brokerGet(target), "SECRET")
	})
}

// A creator without read access gets no URLs at all, so there is nothing to
// sign: issuance happens only after CheckAccess passes.
func TestSignedSkillFileURL_NotIssuedWhenCreatorDenied(t *testing.T) {
	f := setupSignedURLFixture(t)
	uri := "skill://scion/global/" + f.skillA.Slug + "@1.0.0"
	resp := f.srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(f.bob.ID, f.project.ID, uri))
	require.NotNil(t, resp)
	require.Empty(t, resp.Resolved)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, "forbidden", resp.Errors[0].Code)
}

// The /skills/resolve endpoint (absolute URLs) signs as well, and those URLs
// work without credentials.
func TestSignedSkillFileURL_ResolveEndpointAbsoluteURLs(t *testing.T) {
	f := setupSignedURLFixture(t)
	rec := doRequestAsUser(t, f.srv, f.alice, http.MethodPost, "/api/v1/skills/resolve", ResolveSkillsRequest{
		Skills: []ResolveSkillRef{{URI: "skill://scion/global/" + f.skillA.Slug + "@1.0.0"}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ResolveSkillsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Resolved, 1)
	for _, file := range resp.Resolved[0].Files {
		u, err := url.Parse(file.URL)
		require.NoError(t, err)
		require.True(t, u.IsAbs(), file.URL)
		require.NotEmpty(t, u.Query().Get("sig"), file.URL)
		got := f.brokerGet(u.RequestURI())
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	}
}
