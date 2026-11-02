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
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 4 of ptone/scion#1530 (design 79265b51 + addendum 01688eb9): skill
// file URLs, artifact views, list cursors and attachment deliveries signed
// by the keyring's signing ring. Each surface is exercised through the real
// HTTP handlers of two (or three) real hub Servers over one real SQLite
// store, which differ only in their keyring (which generation is active,
// deadlines), as replicas mid-rollout would. The ring deadline clock is
// controlled (ServerConfig.KeyringNow); capability expiry keeps the wall
// clock. Synthetic fixtures only (krLegacySecret, krGen1Secret,
// krGen2Secret from web_keyring_test.go).

// fixedKeyBytes returns the key of legacy-mode capability keys (nil when
// not configured).
func fixedKeyBytes(c capabilityKeys) []byte {
	if !c.configured() {
		return nil
	}
	k, err := c.ring.Active(c.purpose, time.Now())
	if err != nil {
		return nil
	}
	return k.Bytes()
}

// testSealerAEAD returns the AEAD of the sealer's active key.
func testSealerAEAD(t *testing.T, l *listCursorSealer) cipher.AEAD {
	t.Helper()
	require.NotNil(t, l)
	k, _, err := l.keys.issue()
	require.NoError(t, err)
	aead, err := listCursorAEAD(k.Bytes())
	require.NoError(t, err)
	return aead
}

// capKeyring parses a version-1 keyring file with the given signing
// generations, emit setting and legacy readers. It does not run the
// release check (keyringReleaseCheck): servers built on a versioned ring
// need allowVersionedRings.
func capKeyring(t *testing.T, active, emit string, legacyReads []string, entries ...krEntry) *keyring.Keyring {
	t.Helper()
	keys := make([]any, 0, len(entries))
	for _, e := range entries {
		m := map[string]any{"id": e.id, "secret": e.secret, "sign_until": nil, "verify_until": nil}
		if e.signUntil != nil {
			m["sign_until"] = e.signUntil.Format(time.RFC3339)
			m["verify_until"] = e.verifyUntl.Format(time.RFC3339)
		}
		keys = append(keys, m)
	}
	reads := make([]any, 0, len(legacyReads))
	for _, id := range legacyReads {
		reads = append(reads, id)
	}
	doc := map[string]any{
		"schema":   1,
		"revision": "test-rev-p4",
		"signing": map[string]any{
			"active": active, "emit": emit, "keys": keys, "legacy_read_ids": reads,
		},
		"encryption": map[string]any{
			"active": "legacy", "write_format": "v1",
			"keys":               []any{map[string]any{"id": "legacy", "key_b64": base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(krLegacySecret))}},
			"legacy_v1_read_ids": []any{"legacy"},
		},
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	k, err := keyring.Parse(data)
	require.NoError(t, err)
	return k
}

// allowVersionedRings lets New accept emit "versioned" for this test (the
// release check refuses it until phase 6). Tests using it do not run in
// parallel.
func allowVersionedRings(t *testing.T) {
	t.Helper()
	orig := keyringReleaseCheck
	keyringReleaseCheck = func(*keyring.Keyring) error { return nil }
	t.Cleanup(func() { keyringReleaseCheck = orig })
}

// capRollout returns the keyrings of replicas A and B mid-rollout, with
// compatible reader lists: A has gen1 active; B has gen2 active and keeps
// gen1 as the outgoing generation (sign_until T0+1h, verify_until T0+48h);
// both carry the imported legacy entry and the other generation. With emit
// "legacy" both list every generation that issues ID-less values (legacy,
// gen1, gen2) as legacy readers; with emit "versioned" nothing issues
// ID-less values any more and the legacy readers keep only the imported
// legacy generation, for values issued before the rollout.
func capRollout(t *testing.T, emit string) (a, b *keyring.Keyring) {
	t.Helper()
	reads := []string{"legacy"}
	if emit == "legacy" {
		reads = []string{"legacy", "gen1", "gen2"}
	}
	legacy := krEntry{id: "legacy", secret: krLegacySecret}
	a = capKeyring(t, "gen1", emit, reads, legacy,
		krEntry{id: "gen1", secret: krGen1Secret},
		krEntry{id: "gen2", secret: krGen2Secret})
	b = capKeyring(t, "gen2", emit, reads, legacy,
		krEntry{id: "gen1", secret: krGen1Secret, signUntil: krTime(time.Hour), verifyUntl: krTime(48 * time.Hour)},
		krEntry{id: "gen2", secret: krGen2Secret})
	return a, b
}

// capClosing returns a keyring whose active generation gen1 stops issuing
// at T0+1h.
func capClosing(t *testing.T) *keyring.Keyring {
	t.Helper()
	return capKeyring(t, "gen1", "versioned", []string{"legacy"},
		krEntry{id: "legacy", secret: krLegacySecret},
		krEntry{id: "gen1", secret: krGen1Secret, signUntil: krTime(time.Hour), verifyUntl: krTime(48 * time.Hour)},
		krEntry{id: "gen2", secret: krGen2Secret})
}

// newCapReplica builds a real hub Server over st with keyring k (nil: a
// legacy-mode replica of the same deployment), sharing the legacy secret
// as every other purpose requires, and the ring clock clk.
func newCapReplica(t *testing.T, st store.Store, k *keyring.Keyring, clk *krClock) *Server {
	t.Helper()
	cfg := testServerConfig()
	cfg.Keyring = k
	cfg.SharedSigningSecret = krLegacySecret
	cfg.KeyringNow = clk.now
	srv, err := newTestHubServer(t, cfg, st)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	waitUserScopedDataSweep(t, srv)
	return srv
}

// mirrorHubResources gives dst the shared resources of src that replicas of
// one deployment share besides the database: experiments, the artifact
// store, object storage, the attachment store and the web chat store.
func mirrorHubResources(dst, src *Server) {
	src.mu.RLock()
	exp, art, stor, att, wcs := src.experiments, src.artifactStore, src.storage, src.attachmentStore, src.webChatStore
	src.mu.RUnlock()
	dst.mu.Lock()
	dst.experiments = exp
	dst.mu.Unlock()
	dst.SetArtifactStore(art)
	dst.SetStorage(stor)
	if att != nil {
		dst.SetAttachmentStore(att)
	}
	if wcs != nil {
		dst.SetWebChatStore(wcs)
	}
}

// captureLogs sends the default logger to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// assertNoCapabilityKeyMaterial asserts out names no synthetic secret and no derived
// purpose key of any generation, in base64 or raw form.
func assertNoCapabilityKeyMaterial(t *testing.T, out string, extra ...string) {
	t.Helper()
	for _, secret := range []string{krLegacySecret, krGen1Secret, krGen2Secret} {
		assert.NotContains(t, out, secret)
		for _, name := range []string{SecretKeyDownloadSigningKey, SecretKeyArtifactViewKey, SecretKeyListCursorKey, SecretKeyAttachmentDeliveryKey, SecretKeyPortViewKey} {
			k := deriveSharedSigningKey(secret, name)
			assert.NotContains(t, out, base64.StdEncoding.EncodeToString(k))
			assert.NotContains(t, out, base64.RawURLEncoding.EncodeToString(k))
			assert.NotContains(t, out, string(k))
		}
	}
	for _, e := range extra {
		if e != "" {
			assert.NotContains(t, out, e)
		}
	}
}

func hmacB64(key []byte, msg string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// ---------------------------------------------------------------------------
// Seam, configuration and golden bytes
// ---------------------------------------------------------------------------

// TestKeyringReleaseCheck_DefaultIsCheckSessionPilot: production refuses
// what this release does not support.
func TestKeyringReleaseCheck_DefaultIsCheckSessionPilot(t *testing.T) {
	assert.Equal(t, reflect.ValueOf((*keyring.Keyring).CheckSessionPilot).Pointer(), reflect.ValueOf(keyringReleaseCheck).Pointer())
}

// TestValidateKeyringConfig_EmitLegacyActiveMustBeLegacyReader (A1): with
// emit legacy the active generation issues ID-less values, which verify
// only against legacy_read_ids, so it must be listed; the refusal names no
// key or ID.
func TestValidateKeyringConfig_EmitLegacyActiveMustBeLegacyReader(t *testing.T) {
	legacy := krEntry{id: "legacy", secret: krLegacySecret}
	gen1 := krEntry{id: "gen1", secret: krGen1Secret}

	bad := capKeyring(t, "gen1", "legacy", []string{"legacy"}, legacy, gen1)
	err := validateKeyringConfig(ServerConfig{Keyring: bad, SharedSigningSecret: krLegacySecret})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "legacy_read_ids")
	assert.NotContains(t, err.Error(), "gen1")
	assertNoCapabilityKeyMaterial(t, err.Error())
	_, err = New(ServerConfig{Keyring: bad, SharedSigningSecret: krLegacySecret}, nil)
	require.Error(t, err, "New refuses before serving")

	good := capKeyring(t, "gen1", "legacy", []string{"legacy", "gen1"}, legacy, gen1)
	assert.NoError(t, validateKeyringConfig(ServerConfig{Keyring: good, SharedSigningSecret: krLegacySecret}))
	imported := capKeyring(t, "legacy", "legacy", []string{"legacy"}, legacy, gen1)
	assert.NoError(t, validateKeyringConfig(ServerConfig{Keyring: imported, SharedSigningSecret: krLegacySecret}))

	// A deadline already passed does not change membership.
	expired := capKeyring(t, "gen1", "legacy", []string{"legacy", "gen1"}, legacy,
		krEntry{id: "gen1", secret: krGen1Secret, signUntil: krTime(-48 * time.Hour), verifyUntl: krTime(-time.Hour)})
	assert.NoError(t, validateKeyringConfig(ServerConfig{Keyring: expired, SharedSigningSecret: krLegacySecret}))

	// Versioned emission issues key-ID-carrying values: the rule does not
	// apply (the release check is bypassed only to reach it).
	allowVersionedRings(t)
	versioned := capKeyring(t, "gen1", "versioned", []string{"legacy"}, legacy, gen1)
	assert.NoError(t, validateKeyringConfig(ServerConfig{Keyring: versioned, SharedSigningSecret: krLegacySecret}))

	// Legacy mode is unaffected.
	assert.NoError(t, validateKeyringConfig(ServerConfig{Keyring: keyring.Legacy("s")}))
	assert.NoError(t, validateKeyringConfig(ServerConfig{}))
}

// TestNew_ReleaseCheckRefusesVersionedRing: without the test seam, New
// refuses a keyring file this release does not support (emit "versioned")
// before serving, and the refusal names no key material.
func TestNew_ReleaseCheckRefusesVersionedRing(t *testing.T) {
	k := capKeyring(t, "legacy", "versioned", []string{"legacy"}, krEntry{id: "legacy", secret: krLegacySecret})
	_, err := New(ServerConfig{Keyring: k, SharedSigningSecret: krLegacySecret}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported in this release")
	assertNoCapabilityKeyMaterial(t, err.Error())
}

// TestCapabilityKeys_GoldenBytes pins every wire form to formulas written
// out here: legacy-mode keys and the ring's imported legacy generation
// derive the bytes the shared secret did, ID-less forms are the pre-ring
// formats, and the versioned forms bind the generation ID.
func TestCapabilityKeys_GoldenBytes(t *testing.T) {
	k := capKeyring(t, "gen1", "versioned", []string{"legacy"},
		krEntry{id: "legacy", secret: krLegacySecret}, krEntry{id: "gen1", secret: krGen1Secret})
	for _, name := range []string{SecretKeyDownloadSigningKey, SecretKeyArtifactViewKey, SecretKeyListCursorKey, SecretKeyAttachmentDeliveryKey, SecretKeyPortViewKey} {
		c := capabilityKeys{ring: k.Signing(), purpose: sharedSecretPurpose(name)}
		legacyKeys := c.candidates("legacy")
		require.Len(t, legacyKeys, 1)
		assert.Equal(t, deriveSharedSigningKey(krLegacySecret, name), legacyKeys[0].Bytes(), "%s: legacy generation keeps the shared-secret bytes", name)
		active, versioned, err := c.issue()
		require.NoError(t, err)
		assert.True(t, versioned)
		assert.Equal(t, "gen1", active.ID)
		assert.Equal(t, deriveSharedSigningKey(krGen1Secret, name), active.Bytes(), name)
	}

	gen1Download := deriveSharedSigningKey(krGen1Secret, SecretKeyDownloadSigningKey)
	const id, ver, path = "11111111-2222-4333-8444-555555555555", "1.0.0", "SKILL.md"
	exp := time.Unix(1893456000, 0)

	// Skill URLs.
	legacyKey := []byte("test-only-golden-download-key-32")
	legacySrv := &Server{downloadKeys: fixedCapabilityKeys(legacyKey)}
	assert.Equal(t, "exp=1893456000&sig="+hmacB64(legacyKey, "skill-file\n"+id+"\n"+ver+"\n"+path+"\n1893456000"),
		legacySrv.signDownloadURL(id, ver, path, exp))
	ringSrv := &Server{downloadKeys: capabilityKeys{ring: k.Signing(), purpose: sharedSecretPurpose(SecretKeyDownloadSigningKey)}}
	assert.Equal(t, "kv=2&kid=gen1&exp=1893456000&sig="+hmacB64(gen1Download, "skill-file-v2\ngen1\n"+id+"\n"+ver+"\n"+path+"\n1893456000"),
		ringSrv.signDownloadURL(id, ver, path, exp))

	// Attachment delivery tokens (addendum §2).
	payload := []byte(`{"a":"art","m":"msg","g":"agent","b":"broker","e":1893456000,"n":"nonce"}`)
	enc := base64.RawURLEncoding.EncodeToString(payload)
	gen1Delivery := deriveSharedSigningKey(krGen1Secret, SecretKeyAttachmentDeliveryKey)
	assert.Equal(t, hmacB64(legacyKey, "attachment-delivery\n"+string(payload)),
		base64.RawURLEncoding.EncodeToString(deliveryMAC(legacyKey, payload)))
	assert.Equal(t, hmacB64(gen1Delivery, "attachment-delivery-v2\ngen1\n"+string(payload)),
		base64.RawURLEncoding.EncodeToString(deliveryMACv2(gen1Delivery, "gen1", payload)))
	c := deliveryCapability{ArtifactID: "art", MessageID: "msg", AgentID: "agent", BrokerID: "broker", Exp: 1893456000, Nonce: "nonce"}
	dLegacy := &Server{attachmentDeliveryKeys: fixedCapabilityKeys(legacyKey)}
	tok, err := dLegacy.signDeliveryCapability(c)
	require.NoError(t, err)
	assert.Equal(t, enc+"."+hmacB64(legacyKey, "attachment-delivery\n"+string(payload)), tok)
	dRing := &Server{attachmentDeliveryKeys: capabilityKeys{ring: k.Signing(), purpose: sharedSecretPurpose(SecretKeyAttachmentDeliveryKey)}}
	tok, err = dRing.signDeliveryCapability(c)
	require.NoError(t, err)
	assert.Equal(t, "ad2.gen1."+enc+"."+hmacB64(gen1Delivery, "attachment-delivery-v2\ngen1\n"+string(payload)), tok)

	// List cursors: c2 opens only under its own AAD.
	gen1Cursor := deriveSharedSigningKey(krGen1Secret, SecretKeyListCursorKey)
	sealer := newRingListCursorSealer(capabilityKeys{ring: k.Signing(), purpose: sharedSecretPurpose(SecretKeyListCursorKey)})
	const binding = "templates:golden-binding"
	inner := authorizedListCursor(time.Now(), "11111111-1111-1111-1111-111111111111", binding)
	sealCustom := func(prefix, aad string) string {
		aead, err := listCursorAEAD(gen1Cursor)
		require.NoError(t, err)
		nonce := bytes.Repeat([]byte{7}, aead.NonceSize())
		return prefix + base64.RawURLEncoding.EncodeToString(append(nonce, aead.Seal(nil, nonce, []byte(inner), []byte(aad))...))
	}
	got, err := sealer.Open(sealCustom("c2.gen1.", "scion-list-cursor-v2:gen1:"+binding), binding)
	require.NoError(t, err)
	assert.Equal(t, inner, got)
	for name, c := range map[string]string{
		"v1 AAD under c2":         sealCustom("c2.gen1.", "scion-list-cursor-v1:"+binding),
		"AAD without kid":         sealCustom("c2.gen1.", "scion-list-cursor-v2:"+binding),
		"kid in AAD, not in wire": sealCustom("c1.", "scion-list-cursor-v2:gen1:"+binding),
		"other kid in AAD":        sealCustom("c2.gen1.", "scion-list-cursor-v2:legacy:"+binding),
	} {
		_, err := sealer.Open(c, binding)
		assert.ErrorIs(t, err, errInvalidCursor, name)
	}
}

// TestSignSkillFileDownloadURLs_ProviderURLsUntouchedWhenVersioned: cloud
// storage presigned URLs are never changed, whatever the ring emits.
func TestSignSkillFileDownloadURLs_ProviderURLsUntouchedWhenVersioned(t *testing.T) {
	k := capKeyring(t, "gen1", "versioned", []string{"legacy"},
		krEntry{id: "legacy", secret: krLegacySecret}, krEntry{id: "gen1", secret: krGen1Secret})
	s := &Server{downloadKeys: capabilityKeys{ring: k.Signing(), purpose: sharedSecretPurpose(SecretKeyDownloadSigningKey)}}
	sk := "11111111-2222-4333-8444-555555555555"
	provider := "https://storage.googleapis.com/bucket/skills/" + sk + "/SKILL.md?X-Goog-Signature=abc"
	urls := s.signSkillFileDownloadURLs([]DownloadURLInfo{
		{Path: "SKILL.md", URL: provider},
		{Path: "SKILL.md", URL: "/api/v1/skills/" + sk + "/files/SKILL.md?raw=1&version=1.0.0"},
	}, sk, "1.0.0", time.Now())
	assert.Equal(t, provider, urls[0].URL)
	assert.Contains(t, urls[1].URL, "&kv=2&kid=gen1&exp=")
}

// ---------------------------------------------------------------------------
// Skill file URLs
// ---------------------------------------------------------------------------

// capDispatchURLs pre-resolves skill@version on srv as the dispatcher does
// and returns the file URLs by path.
func capDispatchURLs(t *testing.T, srv *Server, f *signedURLFixture, skill *store.Skill, version string) map[string]string {
	t.Helper()
	uri := "skill://scion/global/" + skill.Slug + "@" + version
	resp := srv.preResolveAgentSkills(context.Background(), dispatchTestAgent(f.alice.ID, f.project.ID, uri))
	require.NotNil(t, resp)
	require.Empty(t, resp.Errors)
	require.Len(t, resp.Resolved, 1)
	out := map[string]string{}
	for _, file := range resp.Resolved[0].Files {
		out[file.Path] = file.URL
	}
	return out
}

// capGet issues a credential-less GET through srv's full handler, as a
// broker downloading a skill file or a browser loading a view does.
func capGet(srv *Server, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func dropQuery(t *testing.T, target, key string) string {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	q := u.Query()
	q.Del(key)
	u.RawQuery = q.Encode()
	return u.RequestURI()
}

// TestCapabilityKeyring_SkillURLCrossInstance: a local-storage skill
// dispatch URL issued on either replica downloads the file on the other,
// in both directions, with versioned and with ID-less emission.
func TestCapabilityKeyring_SkillURLCrossInstance(t *testing.T) {
	for _, emit := range []string{"versioned", "legacy"} {
		t.Run(emit, func(t *testing.T) {
			allowVersionedRings(t)
			f := setupSignedURLFixture(t)
			clk := &krClock{t: krT0}
			ka, kb := capRollout(t, emit)
			a := newCapReplica(t, f.srv.store, ka, clk)
			b := newCapReplica(t, f.srv.store, kb, clk)
			mirrorHubResources(a, f.srv)
			mirrorHubResources(b, f.srv)
			for _, dir := range []struct {
				name     string
				from, to *Server
				kid      string
			}{{"A to B", a, b, "gen1"}, {"B to A", b, a, "gen2"}} {
				u := capDispatchURLs(t, dir.from, f, f.skillA, "1.0.0")["SKILL.md"]
				q := queryOf(t, u)
				require.NotEmpty(t, q.Get("sig"), dir.name)
				if emit == "versioned" {
					assert.Equal(t, "2", q.Get("kv"), dir.name)
					assert.Equal(t, dir.kid, q.Get("kid"), dir.name)
				} else {
					assert.False(t, q.Has("kv") || q.Has("kid"), "%s: ID-less form: %s", dir.name, u)
				}
				rec := capGet(dir.to, u)
				require.Equal(t, http.StatusOK, rec.Code, "%s: %s", dir.name, rec.Body.String())
				assert.Equal(t, f.secretA, rec.Body.Bytes(), dir.name)
				// The capability stays bound to its file and version.
				assertDenied(t, capGet(dir.to, strings.Replace(u, "/SKILL.md?", "/other.md?", 1)), "SECRET")
				assertDenied(t, capGet(dir.to, swapQuery(t, u, "version", "2.0.0")), "SECRET")
			}
		})
	}
}

// TestCapabilityKeyring_SkillURLLegacyReplicaUpgrade: a URL issued by a
// replica still in legacy mode (same shared secret) downloads on a ring
// replica whose legacy reader is the imported generation.
func TestCapabilityKeyring_SkillURLLegacyReplicaUpgrade(t *testing.T) {
	allowVersionedRings(t)
	f := setupSignedURLFixture(t)
	clk := &krClock{t: krT0}
	_, kb := capRollout(t, "versioned")
	legacy := newCapReplica(t, f.srv.store, nil, clk)
	b := newCapReplica(t, f.srv.store, kb, clk)
	mirrorHubResources(legacy, f.srv)
	mirrorHubResources(b, f.srv)
	u := capDispatchURLs(t, legacy, f, f.skillA, "1.0.0")["SKILL.md"]
	require.False(t, queryOf(t, u).Has("kid"))
	rec := capGet(b, u)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, f.secretA, rec.Body.Bytes())
}

// TestCapabilityKeyring_SkillURLUnknownKidAndPartialFields: unknown or
// swapped generations and partial, repeated or wrong version fields all get
// the same 401, without the file.
func TestCapabilityKeyring_SkillURLUnknownKidAndPartialFields(t *testing.T) {
	allowVersionedRings(t)
	f := setupSignedURLFixture(t)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := newCapReplica(t, f.srv.store, ka, clk)
	b := newCapReplica(t, f.srv.store, kb, clk)
	legacy := newCapReplica(t, f.srv.store, nil, clk)
	for _, s := range []*Server{a, b, legacy} {
		mirrorHubResources(s, f.srv)
	}
	u := capDispatchURLs(t, a, f, f.skillA, "1.0.0")["SKILL.md"]
	lu := capDispatchURLs(t, legacy, f, f.skillA, "1.0.0")["SKILL.md"]
	require.Equal(t, http.StatusOK, capGet(b, u).Code)
	for name, target := range map[string]string{
		"unknown kid":                swapQuery(t, u, "kid", "gen9"),
		"kid swapped to another gen": swapQuery(t, u, "kid", "gen2"),
		"kid with bad characters":    swapQuery(t, u, "kid", "gen!1"),
		"empty kid":                  swapQuery(t, u, "kid", ""),
		"kv only":                    dropQuery(t, u, "kid"),
		"kid only":                   dropQuery(t, u, "kv"),
		"neither (v2 MAC as legacy)": dropQuery(t, dropQuery(t, u, "kv"), "kid"),
		"kv=3":                       swapQuery(t, u, "kv", "3"),
		"empty kv":                   swapQuery(t, u, "kv", ""),
		"repeated kid":               u + "&kid=gen1",
		"repeated kv":                u + "&kv=2",
		"legacy URL plus kid":        lu + "&kid=legacy",
		"legacy URL plus kv":         lu + "&kv=2",
		"legacy URL as v2 of legacy": lu + "&kv=2&kid=legacy",
	} {
		rec := capGet(b, target)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, name)
		assert.NotContains(t, rec.Body.String(), "SECRET", name)
	}
	// kv or kid alone on a request with a principal commits it to signature
	// validation too: it never falls back to principal authorization.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/x/files/SKILL.md?kid=gen1", nil)
	assert.True(t, hasSkillFileSignature(req))
	req = httptest.NewRequest(http.MethodGet, "/api/v1/skills/x/files/SKILL.md?kv=2", nil)
	assert.True(t, hasSkillFileSignature(req))
}

// TestCapabilityKeyring_SkillURLRetirementBoundary: a gen1 URL downloads
// on B until gen1's verify_until and is refused from that instant (before
// its own expiry), in both forms.
func TestCapabilityKeyring_SkillURLRetirementBoundary(t *testing.T) {
	for _, emit := range []string{"versioned", "legacy"} {
		t.Run(emit, func(t *testing.T) {
			allowVersionedRings(t)
			f := setupSignedURLFixture(t)
			clk := &krClock{t: krT0}
			ka, kb := capRollout(t, emit)
			a := newCapReplica(t, f.srv.store, ka, clk)
			b := newCapReplica(t, f.srv.store, kb, clk)
			mirrorHubResources(a, f.srv)
			mirrorHubResources(b, f.srv)
			u := capDispatchURLs(t, a, f, f.skillA, "1.0.0")["SKILL.md"]
			clk.t = krT0.Add(48*time.Hour - time.Second)
			rec := capGet(b, u)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			clk.t = krT0.Add(48 * time.Hour)
			assertDenied(t, capGet(b, u), "SECRET")
		})
	}
}

// TestCapabilityKeyring_SkillURLIssuanceClosed: at the active generation's
// sign_until nothing is signed (URLs are issued unsigned and the broker's
// download is refused), the instance is not ready, and the single warning
// names no key material.
func TestCapabilityKeyring_SkillURLIssuanceClosed(t *testing.T) {
	allowVersionedRings(t)
	f := setupSignedURLFixture(t)
	clk := &krClock{t: krT0}
	c := newCapReplica(t, f.srv.store, capClosing(t), clk)
	mirrorHubResources(c, f.srv)
	require.NotEmpty(t, queryOf(t, capDispatchURLs(t, c, f, f.skillA, "1.0.0")["SKILL.md"]).Get("sig"))

	logs := captureLogs(t)
	skillURLIssuanceClosedWarned.Store(false)
	clk.t = krT0.Add(time.Hour)
	u := capDispatchURLs(t, c, f, f.skillA, "1.0.0")["SKILL.md"]
	q := queryOf(t, u)
	assert.False(t, q.Has("sig") || q.Has("exp") || q.Has("kid") || q.Has("kv"), "issued unsigned: %s", u)
	assert.Equal(t, http.StatusUnauthorized, capGet(c, u).Code)
	_ = capDispatchURLs(t, c, f, f.skillA, "1.0.0")
	assert.Equal(t, 1, strings.Count(logs.String(), "Skill file URLs are issued unsigned"), logs.String())
	assertNoCapabilityKeyMaterial(t, logs.String())

	rec := httptest.NewRecorder()
	c.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// ---------------------------------------------------------------------------
// Artifact views
// ---------------------------------------------------------------------------

type capViewFixture struct {
	a, b, base *Server
	owner      *store.User
	id         string
	links      string
	page       []byte
}

// setupCapViewFixture builds two replicas over one store, one artifact
// store and one blob store, and an HTML artifact owned by a project member.
func setupCapViewFixture(t *testing.T, ka, kb *keyring.Keyring, clk *krClock) *capViewFixture {
	t.Helper()
	base, s := testServer(t)
	enableArtifactsForTest(t, base)
	f := &capViewFixture{base: base, page: []byte("<p>capability view page</p>")}
	f.a = newCapReplica(t, s, ka, clk)
	f.b = newCapReplica(t, s, kb, clk)
	mirrorHubResources(f.a, base)
	mirrorHubResources(f.b, base)
	p1 := artifactProject(t, s, "capview-p1")
	createTestUserWithProjectRole(t, s, tid("capview-owner"), "capview-owner@test.com", p1.ID, store.ProjectRoleMember)
	owner, err := s.GetUser(context.Background(), tid("capview-owner"))
	require.NoError(t, err)
	f.owner = owner
	rec := userArtifactRequest(t, f.a, owner, http.MethodPost, "/api/v1/artifacts?name=index.html&scope="+p1.ID, f.page)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	f.id = decodeArtifactID(t, rec)
	f.links = "/api/v1/artifacts/" + f.id + "/links"
	return f
}

// mintView mints a session view on srv and returns its URL.
func (f *capViewFixture) mintView(t *testing.T, srv *Server) (string, int) {
	t.Helper()
	rec := userArtifactRequest(t, srv, f.owner, http.MethodPost, "/api/v1/artifacts/"+f.id+"/versions/1/view", nil)
	if rec.Code != http.StatusOK {
		return "", rec.Code
	}
	var v artifacts.ViewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v.URL, rec.Code
}

// shareLink creates a share link on srv and follows it there, returning the
// link and the view location it redirects to.
func (f *capViewFixture) shareLink(t *testing.T, srv *Server) (artifacts.CreateLinkResponse, string) {
	t.Helper()
	rec := userArtifactRequest(t, srv, f.owner, http.MethodPost, f.links, []byte(`{"ttlHours": 2}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created artifacts.CreateLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	shared := anonymous(srv, http.MethodGet, created.URL)
	require.Equal(t, http.StatusSeeOther, shared.Code, shared.Body.String())
	return created, shared.Header().Get("Location")
}

// TestCapabilityKeyring_ArtifactViewCrossInstance: a session view and a
// link-bound view minted on either replica load on the other, in both
// directions and both forms; revoking the link ends its view on the other
// replica (link liveness).
func TestCapabilityKeyring_ArtifactViewCrossInstance(t *testing.T) {
	for _, emit := range []string{"versioned", "legacy"} {
		t.Run(emit, func(t *testing.T) {
			allowVersionedRings(t)
			clk := &krClock{t: krT0}
			ka, kb := capRollout(t, emit)
			f := setupCapViewFixture(t, ka, kb, clk)
			for _, dir := range []struct {
				name     string
				from, to *Server
				kid      string
			}{{"A to B", f.a, f.b, "gen1"}, {"B to A", f.b, f.a, "gen2"}} {
				view, code := f.mintView(t, dir.from)
				require.Equal(t, http.StatusOK, code, dir.name)
				created, loc := f.shareLink(t, dir.from)
				for _, u := range []string{view, loc} {
					if emit == "versioned" {
						assert.True(t, strings.HasPrefix(u, artifacts.RouteView+"v2."+dir.kid+"."), "%s: %s", dir.name, u)
					} else {
						assert.False(t, strings.HasPrefix(u, artifacts.RouteView+"v2."), "%s: %s", dir.name, u)
					}
					rec := anonymous(dir.to, http.MethodGet, u)
					require.Equal(t, http.StatusOK, rec.Code, "%s: %s", dir.name, rec.Body.String())
					assert.Equal(t, string(f.page), rec.Body.String(), dir.name)
				}
				rec := userArtifactRequest(t, dir.from, f.owner, http.MethodDelete, f.links+"/"+created.Link.ID, nil)
				require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
				assert.Equal(t, http.StatusNotFound, anonymous(dir.to, http.MethodGet, loc).Code, "%s: revoked link's view", dir.name)
				assert.Equal(t, http.StatusOK, anonymous(dir.to, http.MethodGet, view).Code, "%s: session view unaffected", dir.name)
			}
		})
	}
}

// TestCapabilityKeyring_ArtifactViewUnknownKidAndRetirement: unknown,
// swapped and stripped generations get the uniform 404; a gen1 view loads
// on B until gen1's verify_until and not from that instant.
func TestCapabilityKeyring_ArtifactViewUnknownKidAndRetirement(t *testing.T) {
	allowVersionedRings(t)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	f := setupCapViewFixture(t, ka, kb, clk)
	view, code := f.mintView(t, f.a)
	require.Equal(t, http.StatusOK, code)
	prefix := artifacts.RouteView + "v2.gen1."
	require.True(t, strings.HasPrefix(view, prefix), view)
	rest := strings.TrimPrefix(view, prefix)
	for name, u := range map[string]string{
		"unknown kid":     artifacts.RouteView + "v2.gen9." + rest,
		"swapped kid":     artifacts.RouteView + "v2.gen2." + rest,
		"kid stripped":    artifacts.RouteView + rest,
		"no kid":          artifacts.RouteView + "v2.." + rest,
		"kid too long":    artifacts.RouteView + "v2." + strings.Repeat("k", 33) + "." + rest,
		"unknown version": artifacts.RouteView + "v3.gen1." + rest,
	} {
		assert.Equal(t, http.StatusNotFound, anonymous(f.b, http.MethodGet, u).Code, name)
	}
	clk.t = krT0.Add(48*time.Hour - time.Second)
	assert.Equal(t, http.StatusOK, anonymous(f.b, http.MethodGet, view).Code)
	clk.t = krT0.Add(48 * time.Hour)
	assert.Equal(t, http.StatusNotFound, anonymous(f.b, http.MethodGet, view).Code)
}

// TestCapabilityKeyring_ArtifactViewIssuanceClosed: at sign_until the mint
// route and a share link answer 503 and sign nothing; earlier views still
// load; the replica warns exactly once, naming no key material.
func TestCapabilityKeyring_ArtifactViewIssuanceClosed(t *testing.T) {
	allowVersionedRings(t)
	logs := captureLogs(t)
	clk := &krClock{t: krT0}
	f := setupCapViewFixture(t, capClosing(t), capClosing(t), clk)
	view, code := f.mintView(t, f.a)
	require.Equal(t, http.StatusOK, code)
	rec := userArtifactRequest(t, f.a, f.owner, http.MethodPost, f.links, []byte(`{"ttlHours": 2}`))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var created artifacts.CreateLinkResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	const warning = "view capability not issued"
	require.NotContains(t, logs.String(), warning)
	// An existing session, with capability issuance closed: the user JWT is
	// signed by the same ring, so it is minted just before sign_until (T0+1h)
	// on the ring clock and stays valid at T0+1h (15 min on the same clock).
	clk.t = krT0.Add(59 * time.Minute)
	session := keyringSessionToken(t, f.a, f.owner)
	clk.t = krT0.Add(time.Hour)
	viewPath := "/api/v1/artifacts/" + f.id + "/versions/1/view"
	code = requestWithToken(t, f.a, session, http.MethodPost, viewPath, nil).Code
	assert.Equal(t, http.StatusServiceUnavailable, code)
	shared := anonymous(f.a, http.MethodGet, created.URL)
	assert.Equal(t, http.StatusServiceUnavailable, shared.Code)
	assert.Empty(t, shared.Header().Get("Location"))
	code = requestWithToken(t, f.a, session, http.MethodPost, viewPath, nil).Code
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Equal(t, http.StatusOK, anonymous(f.b, http.MethodGet, view).Code)
	assert.Equal(t, 1, strings.Count(logs.String(), warning), "one warning for three refusals")
	assertNoCapabilityKeyMaterial(t, logs.String(), view, strings.TrimPrefix(created.URL, artifacts.RouteShared))
}

// ---------------------------------------------------------------------------
// List cursors
// ---------------------------------------------------------------------------

// TestCapabilityKeyring_ListCursorCrossInstance: a scoped templates
// continuation sealed on either replica resumes on the other, in both
// directions and both forms; it stays bound to its caller.
func TestCapabilityKeyring_ListCursorCrossInstance(t *testing.T) {
	for _, emit := range []string{"versioned", "legacy"} {
		t.Run(emit, func(t *testing.T) {
			allowVersionedRings(t)
			_, s, alice, carol, _ := setupTemplateScopeTest(t)
			setupCursorOpacityTemplates(t, s, carol.ID)
			clk := &krClock{t: krT0}
			ka, kb := capRollout(t, emit)
			a := newCapReplica(t, s, ka, clk)
			b := newCapReplica(t, s, kb, clk)
			for _, dir := range []struct {
				name     string
				from, to *Server
				kid      string
			}{{"A to B", a, b, "gen1"}, {"B to A", b, a, "gen2"}} {
				rec, first := getTemplatesPage(t, dir.from, carol, "scope=user&limit=1")
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Len(t, first.Templates, 1)
				require.NotEmpty(t, first.NextCursor)
				if emit == "versioned" {
					assert.True(t, strings.HasPrefix(first.NextCursor, "c2."+dir.kid+"."), "%s: %s", dir.name, first.NextCursor)
				} else {
					assert.True(t, strings.HasPrefix(first.NextCursor, listCursorPrefix), "%s: %s", dir.name, first.NextCursor)
				}
				rec, second := getTemplatesPage(t, dir.to, carol, "scope=user&limit=1&cursor="+first.NextCursor)
				require.Equal(t, http.StatusOK, rec.Code, "%s: %s", dir.name, rec.Body.String())
				require.Len(t, second.Templates, 1, dir.name)
				assert.NotEqual(t, first.Templates[0].ID, second.Templates[0].ID, dir.name)
				// Another caller cannot resume it (caller binding kept).
				rec, _ = getTemplatesPage(t, dir.to, alice, "scope=user&limit=1&cursor="+first.NextCursor)
				assertInvalidCursor(t, rec)
			}
		})
	}
}

// TestCapabilityKeyring_ListCursorLegacyReplicaUpgrade: a c1 cursor sealed
// by a legacy-mode replica resumes on a ring replica.
func TestCapabilityKeyring_ListCursorLegacyReplicaUpgrade(t *testing.T) {
	allowVersionedRings(t)
	_, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	clk := &krClock{t: krT0}
	_, kb := capRollout(t, "versioned")
	legacy := newCapReplica(t, s, nil, clk)
	b := newCapReplica(t, s, kb, clk)
	_, first := getTemplatesPage(t, legacy, carol, "scope=user&limit=1")
	require.True(t, strings.HasPrefix(first.NextCursor, listCursorPrefix), first.NextCursor)
	rec, second := getTemplatesPage(t, b, carol, "scope=user&limit=1&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, second.Templates, 1)
}

// TestCapabilityKeyring_ListCursorUnknownKidAndRetirement: unknown,
// swapped and stripped generations get the uniform invalid_cursor; a gen1
// cursor resumes on B until gen1's verify_until and not from that instant.
func TestCapabilityKeyring_ListCursorUnknownKidAndRetirement(t *testing.T) {
	allowVersionedRings(t)
	_, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := newCapReplica(t, s, ka, clk)
	b := newCapReplica(t, s, kb, clk)
	_, first := getTemplatesPage(t, a, carol, "scope=user&limit=1")
	require.True(t, strings.HasPrefix(first.NextCursor, "c2.gen1."), first.NextCursor)
	body := strings.TrimPrefix(first.NextCursor, "c2.gen1.")
	for name, c := range map[string]string{
		"unknown kid":       "c2.gen9." + body,
		"swapped kid":       "c2.gen2." + body,
		"stripped to c1":    "c1." + body,
		"no kid":            "c2." + body,
		"empty kid":         "c2.." + body,
		"unknown version":   "c3.gen1." + body,
		"bad kid character": "c2.gen%211." + body,
	} {
		rec, _ := getTemplatesPage(t, b, carol, "scope=user&limit=1&cursor="+c)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		assertInvalidCursor(t, rec)
	}
	clk.t = krT0.Add(48*time.Hour - time.Second)
	rec, _ := getTemplatesPage(t, b, carol, "scope=user&limit=1&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	clk.t = krT0.Add(48 * time.Hour)
	rec, _ = getTemplatesPage(t, b, carol, "scope=user&limit=1&cursor="+first.NextCursor)
	assertInvalidCursor(t, rec)
	// A fresh first page always works.
	rec, _ = getTemplatesPage(t, b, carol, "scope=user&limit=1")
	assert.Equal(t, http.StatusOK, rec.Code)
}

// TestCapabilityKeyring_ListCursorIssuanceClosed: at sign_until a page that
// needs a continuation is refused (500, no cursor); a complete first page
// still works.
func TestCapabilityKeyring_ListCursorIssuanceClosed(t *testing.T) {
	allowVersionedRings(t)
	_, s, _, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	logs := captureLogs(t) // before the replica: its loggers bind the default at New
	// An existing session, with capability issuance closed: the user JWT is
	// signed by the same ring, so it is minted just before sign_until (T0+1h)
	// on the ring clock and stays valid at T0+1h (15 min on the same clock).
	clk := &krClock{t: krT0.Add(59 * time.Minute)}
	c := newCapReplica(t, s, capClosing(t), clk)
	session := keyringSessionToken(t, c, carol)
	clk.t = krT0.Add(time.Hour)
	rec := requestWithToken(t, c, session, http.MethodGet, "/api/v1/templates?scope=user&limit=1", nil)
	assert.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "c2.")
	rec = requestWithToken(t, c, session, http.MethodGet, "/api/v1/templates?scope=user&limit=10", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var all ListTemplatesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all))
	assert.Len(t, all.Templates, 2)
	assert.Empty(t, all.NextCursor)
	assertNoCapabilityKeyMaterial(t, logs.String()+rec.Body.String())
}

// TestCapabilityKeyring_ListCursorFilterAndPerPageAuthorization: across
// replicas a cursor stays bound to its filter (another filter refuses it),
// and the resumed page is authorized live on the replica serving it: an
// item the caller lost access to between pages is not returned.
func TestCapabilityKeyring_ListCursorFilterAndPerPageAuthorization(t *testing.T) {
	allowVersionedRings(t)
	_, s, alice, carol, _ := setupTemplateScopeTest(t)
	setupCursorOpacityTemplates(t, s, carol.ID)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := newCapReplica(t, s, ka, clk)
	b := newCapReplica(t, s, kb, clk)

	rec, all := getTemplatesPage(t, a, carol, "scope=user&limit=10")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, all.Templates, 2)
	_, first := getTemplatesPage(t, a, carol, "scope=user&limit=1")
	require.Len(t, first.Templates, 1)
	require.True(t, strings.HasPrefix(first.NextCursor, "c2.gen1."), first.NextCursor)

	// Another filter on the other replica refuses the cursor.
	rec, _ = getTemplatesPage(t, b, carol, "scope=user&search=cursor-opacity&limit=1&cursor="+first.NextCursor)
	assertInvalidCursor(t, rec)

	// Carol loses read access to the remaining template between pages.
	var remaining string
	for _, tpl := range all.Templates {
		if tpl.ID != first.Templates[0].ID {
			remaining = tpl.ID
		}
	}
	require.NotEmpty(t, remaining)
	tpl, err := s.GetTemplate(context.Background(), remaining)
	require.NoError(t, err)
	tpl.OwnerID = alice.ID
	require.NoError(t, s.UpdateTemplate(context.Background(), tpl))
	rec, second := getTemplatesPage(t, b, carol, "scope=user&limit=1&cursor="+first.NextCursor)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	for _, got := range second.Templates {
		assert.NotEqual(t, remaining, got.ID, "a template the caller can no longer read was returned")
	}
}

// TestCapabilityKeyring_SkillURLExpiredAcrossInstances: a URL replica A
// signed with an expiry in the past is refused on B (expiry is still
// checked after the candidate keys).
func TestCapabilityKeyring_SkillURLExpiredAcrossInstances(t *testing.T) {
	allowVersionedRings(t)
	f := setupSignedURLFixture(t)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := newCapReplica(t, f.srv.store, ka, clk)
	b := newCapReplica(t, f.srv.store, kb, clk)
	mirrorHubResources(a, f.srv)
	mirrorHubResources(b, f.srv)
	u := capDispatchURLs(t, a, f, f.skillA, "1.0.0")["SKILL.md"]
	base := u[:strings.Index(u, "&kv=")]
	expired := base + "&" + a.signDownloadURL(f.skillA.ID, "1.0.0", "SKILL.md", time.Now().Add(-time.Second))
	require.Contains(t, expired, "&kid=gen1&")
	assertDenied(t, capGet(b, expired), "SECRET")
	assertDenied(t, capGet(a, expired), "SECRET")
	// The same shape with a live expiry downloads.
	live := base + "&" + a.signDownloadURL(f.skillA.ID, "1.0.0", "SKILL.md", time.Now().Add(time.Minute))
	require.Equal(t, http.StatusOK, capGet(b, live).Code)
}

// TestCapabilityKeyring_RingModeKeySourceLabel: in ring mode every
// capability key is labelled as coming from the keyring, which the test
// admin tier refuses (G7) like any deployment-wide source.
func TestCapabilityKeyring_RingModeKeySourceLabel(t *testing.T) {
	_, s := testServer(t)
	clk := &krClock{t: krT0}
	k := capKeyring(t, "legacy", "legacy", []string{"legacy"}, krEntry{id: "legacy", secret: krLegacySecret})
	srv := newCapReplica(t, s, k, clk)
	for _, name := range ringModeCapabilityKeys {
		assert.Equal(t, SigningKeySourceKeyring, srv.signingKeySource(name), name)
		assert.False(t, signingKeySourceIsHubLocal(srv.signingKeySource(name)), name)
	}
	legacy := newCapReplica(t, s, nil, clk)
	for _, name := range ringModeCapabilityKeys {
		assert.NotEqual(t, SigningKeySourceKeyring, legacy.signingKeySource(name), name)
	}
}

// ringModeCapabilityKeys are the capability keys ring mode derives.
var ringModeCapabilityKeys = []string{SecretKeyDownloadSigningKey, SecretKeyArtifactViewKey, SecretKeyListCursorKey, SecretKeyAttachmentDeliveryKey, SecretKeyPortViewKey}

// ---------------------------------------------------------------------------
// Attachment deliveries (addendum 01688eb9)
// ---------------------------------------------------------------------------

type capDeliveryFixture struct {
	*sendFixture
	call  dispatchCall
	msgID string
}

func setupCapDeliveryFixture(t *testing.T) *capDeliveryFixture {
	t.Helper()
	f := newSendFixture(t)
	f.enableBrokerDelivery(t)
	msgID, call := f.sendWithFile(t, "report.txt", "file bytes")
	return &capDeliveryFixture{sendFixture: f, call: call, msgID: msgID}
}

// replica builds a ring replica over the fixture's store and resources.
func (f *capDeliveryFixture) replica(t *testing.T, k *keyring.Keyring, clk *krClock) *Server {
	t.Helper()
	srv := newCapReplica(t, f.st, k, clk)
	mirrorHubResources(srv, f.srv)
	return srv
}

// issueOn issues the capability of the fixture's one attachment on srv, as
// srv's dispatcher would, and returns its token ("" when no URL).
func (f *capDeliveryFixture) issueOn(t *testing.T, srv *Server) string {
	t.Helper()
	wire := srv.attachmentDelivery().Issue(context.Background(), f.call.Agent, f.call.Agent.RuntimeBrokerID, f.call.StructuredMessage.AttachmentPlan)
	require.Len(t, wire, 1)
	if wire[0].URL == "" {
		return ""
	}
	return tokenOf(t, wire[0])
}

// redeemOn redeems token on srv as broker brokerID ("" for no broker
// identity) through the registered route handler.
func redeemOn(srv *Server, brokerID, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, attachmentDeliveriesPrefix+token, nil)
	if brokerID != "" {
		req = req.WithContext(contextWithIdentity(req.Context(), NewBrokerIdentity(brokerID)))
	}
	rec := httptest.NewRecorder()
	srv.guarded(attachmentDeliveriesPrefix, srv.handleAttachmentDelivery)(rec, req)
	return rec
}

// TestCapabilityKeyring_DeliveryCrossReplica: a delivery issued on either
// replica redeems on the other with the file bytes, in both directions and
// both forms, each with a fresh nonce; the same token never redeems twice,
// on either replica.
func TestCapabilityKeyring_DeliveryCrossReplica(t *testing.T) {
	for _, emit := range []string{"versioned", "legacy"} {
		t.Run(emit, func(t *testing.T) {
			allowVersionedRings(t)
			f := setupCapDeliveryFixture(t)
			clk := &krClock{t: krT0}
			ka, kb := capRollout(t, emit)
			a := f.replica(t, ka, clk)
			b := f.replica(t, kb, clk)
			for _, dir := range []struct {
				name     string
				from, to *Server
				kid      string
			}{{"A to B", a, b, "gen1"}, {"B to A", b, a, "gen2"}} {
				token := f.issueOn(t, dir.from)
				require.NotEmpty(t, token, dir.name)
				if emit == "versioned" {
					assert.True(t, strings.HasPrefix(token, "ad2."+dir.kid+"."), "%s: %s", dir.name, token)
				} else {
					assert.Len(t, strings.Split(token, "."), 2, dir.name)
				}
				ok := redeemOn(dir.to, f.brokerID, token)
				require.Equal(t, http.StatusOK, ok.Code, "%s: %s", dir.name, ok.Body.String())
				assert.Equal(t, "file bytes", ok.Body.String(), dir.name)
				assert.Equal(t, http.StatusNotFound, redeemOn(dir.to, f.brokerID, token).Code, "%s: second use, same replica", dir.name)
				assert.Equal(t, http.StatusNotFound, redeemOn(dir.from, f.brokerID, token).Code, "%s: second use, other replica", dir.name)
			}
		})
	}
}

// TestCapabilityKeyring_DeliveryLegacyReplicaUpgrade: a token issued by a
// legacy-mode replica (pre-ring bytes) redeems on a ring replica whose
// legacy reader is the imported generation.
func TestCapabilityKeyring_DeliveryLegacyReplicaUpgrade(t *testing.T) {
	allowVersionedRings(t)
	f := setupCapDeliveryFixture(t)
	clk := &krClock{t: krT0}
	_, kb := capRollout(t, "versioned")
	legacy := f.replica(t, nil, clk)
	b := f.replica(t, kb, clk)
	token := f.issueOn(t, legacy)
	require.Len(t, strings.Split(token, "."), 2)
	ok := redeemOn(b, f.brokerID, token)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, "file bytes", ok.Body.String())
}

// TestCapabilityKeyring_DeliveryNonceSharedAcrossFormats: the redemption
// namespace is the same for every format and generation: a nonce redeemed
// as a versioned token cannot be redeemed again re-signed in the legacy
// form under the imported generation.
func TestCapabilityKeyring_DeliveryNonceSharedAcrossFormats(t *testing.T) {
	allowVersionedRings(t)
	f := setupCapDeliveryFixture(t)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := f.replica(t, ka, clk)
	b := f.replica(t, kb, clk)
	token := f.issueOn(t, a)
	parts := strings.Split(token, ".")
	require.Len(t, parts, 4)
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	require.NoError(t, err)
	legacyKey := deriveSharedSigningKey(krLegacySecret, SecretKeyAttachmentDeliveryKey)
	resigned := parts[2] + "." + base64.RawURLEncoding.EncodeToString(deliveryMAC(legacyKey, payload))
	// The re-signed form is itself valid before the nonce is used...
	_, ok := b.verifyDeliveryCapability(resigned, time.Now())
	require.True(t, ok)
	require.Equal(t, http.StatusOK, redeemOn(b, f.brokerID, token).Code)
	// ...but its nonce is spent.
	assert.Equal(t, http.StatusNotFound, redeemOn(a, f.brokerID, resigned).Code)
}

// TestCapabilityKeyring_DeliveryStrictFormatDoesNotConsumeNonce: malformed,
// unknown-generation and wrongly bound tokens are refused with the uniform
// 404 before nonce redemption, so the genuine token still redeems once
// afterwards.
func TestCapabilityKeyring_DeliveryStrictFormatDoesNotConsumeNonce(t *testing.T) {
	allowVersionedRings(t)
	f := setupCapDeliveryFixture(t)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := f.replica(t, ka, clk)
	b := f.replica(t, kb, clk)
	token := f.issueOn(t, a)
	parts := strings.Split(token, ".")
	require.Len(t, parts, 4)
	unknown := redeemOn(b, f.brokerID, "not-a-token")
	altered := []byte(parts[2])
	altered[len(altered)/2] ^= 0x01
	gen1 := deriveSharedSigningKey(krGen1Secret, SecretKeyAttachmentDeliveryKey)
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	require.NoError(t, err)
	for name, tok := range map[string]string{
		"unknown kid (signed by gen1)": "ad2.gen9." + parts[2] + "." + base64.RawURLEncoding.EncodeToString(deliveryMACv2(gen1, "gen9", payload)),
		"swapped kid":                  "ad2.gen2." + parts[2] + "." + parts[3],
		"kid stripped (two parts)":     parts[2] + "." + parts[3],
		"empty kid":                    "ad2.." + parts[2] + "." + parts[3],
		"invalid kid":                  "ad2.gen!1." + parts[2] + "." + parts[3],
		"unknown version":              "ad3.gen1." + parts[2] + "." + parts[3],
		"uppercase version":            "AD2.gen1." + parts[2] + "." + parts[3],
		"three parts":                  "gen1." + parts[2] + "." + parts[3],
		"five parts":                   token + ".x",
		"one part":                     parts[2],
		"altered payload":              "ad2.gen1." + string(altered) + "." + parts[3],
		"padded base64":                token + "=",
		"standard base64 MAC":          "ad2.gen1." + parts[2] + "." + strings.ReplaceAll(strings.ReplaceAll(parts[3], "-", "+"), "_", "/") + "+",
	} {
		rec := redeemOn(b, f.brokerID, tok)
		assert.Equal(t, http.StatusNotFound, rec.Code, name)
		assert.Equal(t, unknown.Body.String(), rec.Body.String(), name)
	}
	// Binding refusals happen before redemption as well.
	assert.Equal(t, http.StatusNotFound, redeemOn(b, "", token).Code, "no broker identity")
	assert.Equal(t, http.StatusNotFound, redeemOn(b, "00000000-0000-4000-8000-000000000000", token).Code, "wrong broker")
	ok := redeemOn(b, f.brokerID, token)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, "file bytes", ok.Body.String())
}

// TestCapabilityKeyring_DeliveryRetirementBoundary: a gen1 token is
// refused on B at gen1's verify_until even though it has not expired, and
// redeems one second earlier (the refusal did not spend the nonce).
func TestCapabilityKeyring_DeliveryRetirementBoundary(t *testing.T) {
	for _, emit := range []string{"versioned", "legacy"} {
		t.Run(emit, func(t *testing.T) {
			allowVersionedRings(t)
			f := setupCapDeliveryFixture(t)
			clk := &krClock{t: krT0}
			ka, kb := capRollout(t, emit)
			a := f.replica(t, ka, clk)
			b := f.replica(t, kb, clk)
			token := f.issueOn(t, a)
			clk.t = krT0.Add(48 * time.Hour)
			assert.Equal(t, http.StatusNotFound, redeemOn(b, f.brokerID, token).Code)
			clk.t = krT0.Add(48*time.Hour - time.Second)
			ok := redeemOn(b, f.brokerID, token)
			require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
		})
	}
}

// TestCapabilityKeyring_DeliveryBindingAndLivenessRingMode: on a ring
// replica an expired capability, a deleted message and an unbound
// attachment each get the uniform 404 (the body of an unknown token). The
// expired refusal happens before redemption: the same nonce, validly
// signed, still redeems afterwards.
func TestCapabilityKeyring_DeliveryBindingAndLivenessRingMode(t *testing.T) {
	setup := func(t *testing.T) (*capDeliveryFixture, *Server, *Server) {
		allowVersionedRings(t)
		f := setupCapDeliveryFixture(t)
		clk := &krClock{t: krT0}
		ka, kb := capRollout(t, "versioned")
		return f, f.replica(t, ka, clk), f.replica(t, kb, clk)
	}
	t.Run("expired", func(t *testing.T) {
		f, a, b := setup(t)
		plan := f.call.StructuredMessage.AttachmentPlan[0]
		c := deliveryCapability{
			ArtifactID: plan.Delivery.ArtifactID, MessageID: plan.Delivery.MessageID, AgentID: f.call.Agent.ID,
			BrokerID: f.brokerID, Exp: time.Now().Add(-time.Second).Unix(), Nonce: "ring-expired-nonce",
		}
		expired, err := a.signDeliveryCapability(c)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(expired, "ad2.gen1."), expired)
		unknown := redeemOn(b, f.brokerID, "not-a-token")
		got := redeemOn(b, f.brokerID, expired)
		require.Equal(t, http.StatusNotFound, got.Code)
		assert.Equal(t, unknown.Body.String(), got.Body.String())
		c.Exp = time.Now().Add(attachmentDeliveryTTL).Unix()
		live, err := a.signDeliveryCapability(c)
		require.NoError(t, err)
		ok := redeemOn(b, f.brokerID, live)
		require.Equal(t, http.StatusOK, ok.Code, "the expired refusal must not spend the nonce: %s", ok.Body.String())
		assert.Equal(t, "file bytes", ok.Body.String())
	})
	t.Run("deleted message", func(t *testing.T) {
		f, a, b := setup(t)
		token := f.issueOn(t, a)
		require.NoError(t, f.wcs.SetMessageDeleted(context.Background(), f.msgID, time.Now()))
		unknown := redeemOn(b, f.brokerID, "not-a-token")
		got := redeemOn(b, f.brokerID, token)
		require.Equal(t, http.StatusNotFound, got.Code)
		assert.Equal(t, unknown.Body.String(), got.Body.String())
	})
	t.Run("unbound attachment", func(t *testing.T) {
		f, a, b := setup(t)
		token := f.issueOn(t, a)
		f.srv.retireMessageAttachments(context.Background(), f.msgID)
		unknown := redeemOn(b, f.brokerID, "not-a-token")
		got := redeemOn(b, f.brokerID, token)
		require.Equal(t, http.StatusNotFound, got.Code)
		assert.Equal(t, unknown.Body.String(), got.Body.String())
	})
}

// TestCapabilityKeyring_DeliveryCrossReplicaBrokerHMAC: a versioned token
// issued on A redeems on B through B's full handler chain with real broker
// HMAC authentication; a wrong broker secret gets nothing.
func TestCapabilityKeyring_DeliveryCrossReplicaBrokerHMAC(t *testing.T) {
	allowVersionedRings(t)
	f := setupCapDeliveryFixture(t)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := f.replica(t, ka, clk)
	b := f.replica(t, kb, clk)
	// As enableBrokerHMAC does for the fixture's server, before any
	// request builds B's handler chain.
	if b.brokerAuthService == nil {
		svc := NewBrokerAuthService(DefaultBrokerAuthConfig(), f.st)
		t.Cleanup(svc.Close)
		b.brokerAuthService = svc
		b.authConfig.BrokerAuthSvc = svc
	}
	key := seedBrokerSecret(t, f.st, f.brokerID)
	token := f.issueOn(t, a)
	require.True(t, strings.HasPrefix(token, "ad2.gen1."), token)
	path := attachmentDeliveriesPrefix + token
	forged := doBrokerSignedRequest(t, b, f.brokerID, []byte("test-only-not-the-broker-secret-0123"), "", http.MethodGet, path, nil)
	require.NotEqual(t, http.StatusOK, forged.Code)
	assert.NotContains(t, forged.Body.String(), "file bytes")
	ok := doBrokerSignedRequest(t, b, f.brokerID, key, "", http.MethodGet, path, nil)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, "file bytes", ok.Body.String())
}

// TestCapabilityKeyring_DeliveryIssuanceClosed: at sign_until Issue yields
// no URL (no old key, no unsigned token), the instance is not ready, and
// the signing failure is logged exactly once over repeated dispatch
// attempts, naming no key material.
func TestCapabilityKeyring_DeliveryIssuanceClosed(t *testing.T) {
	allowVersionedRings(t)
	f := setupCapDeliveryFixture(t)
	logs := captureLogs(t) // before the replica: its message logger binds the default at New
	deliveryIssuanceClosedLogged.Store(false)
	t.Cleanup(func() { deliveryIssuanceClosedLogged.Store(false) })
	clk := &krClock{t: krT0}
	c := f.replica(t, capClosing(t), clk)
	before := f.issueOn(t, c)
	require.NotEmpty(t, before)
	const failure = "Failed to sign an attachment delivery capability"
	require.NotContains(t, logs.String(), failure)
	clk.t = krT0.Add(time.Hour)
	for i := 0; i < 3; i++ {
		assert.Empty(t, f.issueOn(t, c), "attempt %d", i)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), failure), "one log line for three dispatch attempts:\n%s", logs.String())
	rec := httptest.NewRecorder()
	c.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	// A token issued before sign_until still redeems.
	ok := redeemOn(c, f.brokerID, before)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assertNoCapabilityKeyMaterial(t, logs.String(), before)
}

// TestCapabilityKeyring_DeliveryWithProjectionStillSigned: a deliverable
// plan entry that carries a scratchpad projection (applyProjection) is
// issued a versioned capability like any other: it redeems on the other
// replica with the file bytes, and the same token with its payload altered
// is refused without spending the nonce. The projection is plan metadata;
// the signed payload is unchanged by it.
func TestCapabilityKeyring_DeliveryWithProjectionStillSigned(t *testing.T) {
	allowVersionedRings(t)
	f := setupCapDeliveryFixture(t)
	clk := &krClock{t: krT0}
	ka, kb := capRollout(t, "versioned")
	a := f.replica(t, ka, clk)
	b := f.replica(t, kb, clk)
	plan := append([]messages.AttachmentPlanEntry(nil), f.call.StructuredMessage.AttachmentPlan...)
	require.Len(t, plan, 1)
	require.Equal(t, messages.AttachmentDeliverable, plan[0].State)
	plan[0].Delivery.ProjectedPath = "/test-only/shared/.attachments/" + plan[0].Delivery.ArtifactID + "/report.txt"
	plan[0].Delivery.ProjectedMount = "test-only-shared"
	applyProjection(&plan[0])
	require.Equal(t, plan[0].Delivery.ProjectedPath, plan[0].ProjectedPath)
	require.Equal(t, "test-only-shared", plan[0].ProjectedMount)
	wire := a.attachmentDelivery().Issue(context.Background(), f.call.Agent, f.call.Agent.RuntimeBrokerID, plan)
	require.Len(t, wire, 1)
	token := tokenOf(t, wire[0])
	require.True(t, strings.HasPrefix(token, "ad2.gen1."), token)
	parts := strings.Split(token, ".")
	require.Len(t, parts, 4)
	// Alter one byte of the decoded payload and re-encode it, so the altered
	// token stays well-formed and its refusal comes from the MAC check.
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[2])
	require.NoError(t, err)
	// The signed payload carries only the capability fields; the projection
	// is not part of it.
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &fields))
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"a", "m", "g", "b", "e", "n"}, keys)
	assert.NotContains(t, string(payload), "test-only-shared")
	// Flip a byte inside the artifact ID value (never the nonce), so the
	// refusal below cannot depend on the field lengths.
	at := bytes.Index(payload, []byte(`"a":"`))
	require.GreaterOrEqual(t, at, 0)
	payload[at+5] ^= 0x01
	altered := "ad2.gen1." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[3]
	ap := strings.Split(altered, ".")
	require.Len(t, ap, 4)
	require.NotEqual(t, parts[2], ap[2])
	_, err = base64.RawURLEncoding.Strict().DecodeString(ap[2])
	require.NoError(t, err, "the altered payload is valid base64url")
	unknown := redeemOn(b, f.brokerID, "not-a-token")
	got := redeemOn(b, f.brokerID, altered)
	assert.Equal(t, http.StatusNotFound, got.Code)
	assert.Equal(t, unknown.Body.String(), got.Body.String())
	ok := redeemOn(b, f.brokerID, token)
	require.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
	assert.Equal(t, "file bytes", ok.Body.String())
}

// TestCapabilityKeyring_DeliveryOtherSigningErrorsLogEveryAttempt: only
// the issuance-closed failure is logged once; any other signing failure
// (here: no delivery key at all) is still logged on every attempt, even
// with the once-flag already set.
func TestCapabilityKeyring_DeliveryOtherSigningErrorsLogEveryAttempt(t *testing.T) {
	allowVersionedRings(t)
	f := setupCapDeliveryFixture(t)
	logs := captureLogs(t) // before the replica: its message logger binds the default at New
	deliveryIssuanceClosedLogged.Store(true)
	t.Cleanup(func() { deliveryIssuanceClosedLogged.Store(false) })
	clk := &krClock{t: krT0}
	ka, _ := capRollout(t, "versioned")
	c := f.replica(t, ka, clk)
	c.mu.Lock()
	c.attachmentDeliveryKeys = capabilityKeys{}
	c.mu.Unlock()
	const failure = "Failed to sign an attachment delivery capability"
	require.NotContains(t, logs.String(), failure)
	assert.Empty(t, f.issueOn(t, c))
	assert.Empty(t, f.issueOn(t, c))
	assert.Equal(t, 2, strings.Count(logs.String(), failure), logs.String())
	assertNoCapabilityKeyMaterial(t, logs.String())
}

// ---------------------------------------------------------------------------
// No partial fallback
// ---------------------------------------------------------------------------

// aliasCountingStore counts reads of the retained single-key records of the
// capability purposes.
type aliasCountingStore struct {
	store.Store
	reads atomic.Int64
}

func capabilityKeyName(key string) bool {
	switch key {
	case SecretKeyDownloadSigningKey, SecretKeyArtifactViewKey, SecretKeyListCursorKey, SecretKeyAttachmentDeliveryKey, SecretKeyPortViewKey:
		return true
	}
	return false
}

func (s *aliasCountingStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	if capabilityKeyName(key) {
		s.reads.Add(1)
	}
	return s.Store.GetSecret(ctx, key, scope, scopeID)
}

func (s *aliasCountingStore) GetSecretValue(ctx context.Context, key, scope, scopeID string) (string, error) {
	if capabilityKeyName(key) {
		s.reads.Add(1)
	}
	return s.Store.GetSecretValue(ctx, key, scope, scopeID)
}

// TestCapabilityKeyring_UnusableRingRefusesWithoutAliasReads: a keyring
// that is not the legacy adapter but has no usable signing generation
// refuses each capability key at startup, without reading the retained
// single-key records that legacy mode persisted (they are present), and
// without switching to legacy mode. Legacy mode does read them.
func TestCapabilityKeyring_UnusableRingRefusesWithoutAliasReads(t *testing.T) {
	_, s := testServer(t) // legacy mode: persists the capability key records
	ctx := context.Background()
	counting := &aliasCountingStore{Store: s}
	srv := &Server{config: ServerConfig{Keyring: &keyring.Keyring{}}, store: counting, hubID: "test-hub-id"}
	for name, init := range map[string]func(context.Context) error{
		"download":   srv.initDownloadSigningKey,
		"view":       srv.initArtifactViewKey,
		"cursor":     srv.initListCursorSealer,
		"attachment": srv.initAttachmentDeliveryKey,
		"port view":  srv.initPortViewKey,
	} {
		err := init(ctx)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "no usable active generation", name)
	}
	assert.Zero(t, counting.reads.Load(), "ring mode read a retained single-key record")
	assert.False(t, srv.downloadKeys.configured() || srv.artifactViewKeys.configured() || srv.attachmentDeliveryKeys.configured() || srv.portViewKeys.configured())
	assert.Nil(t, srv.listCursorSealer)

	// The control: the same inits in legacy mode do read those records.
	legacy := &Server{config: ServerConfig{}, store: counting, hubID: "test-hub-id"}
	require.NoError(t, legacy.initDownloadSigningKey(ctx))
	assert.NotZero(t, counting.reads.Load(), "the counting store sees legacy-mode reads")

	// And New refuses the unusable keyring before serving.
	cfg := testServerConfig()
	cfg.Keyring = &keyring.Keyring{}
	_, err := New(cfg, &aliasCountingStore{Store: s})
	require.Error(t, err)
}
