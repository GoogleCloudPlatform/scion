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
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Conduit admission tokens under the signing ring (ptone/scion#1530
// Phase 5). Synthetic fixtures only; the secrets are the web keyring
// test's (krLegacySecret, krGen1Secret, krGen2Secret).

// admT0 is the start of every admission keyring scenario; generation
// deadlines are relative to it.
var admT0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

// admSignUntil and admVerifyUntil bound the outgoing gen1 on the replica
// that activated gen2 (and on the closing replica).
var (
	admSignUntil   = admT0.Add(30 * time.Minute)
	admVerifyUntil = admT0.Add(3 * time.Hour)
)

// admEntry is one signing generation of admKeyring.
type admEntry struct {
	id, secret string
	bounded    bool // sign_until admSignUntil, verify_until admVerifyUntil
}

// admKeyring parses a keyring file with the given signing ring and the
// imported legacy encryption key.
func admKeyring(t *testing.T, active string, emit keyring.Emit, readers []string, entries ...admEntry) *keyring.Keyring {
	t.Helper()
	keys := make([]any, 0, len(entries))
	for _, e := range entries {
		m := map[string]any{"id": e.id, "secret": e.secret, "sign_until": nil, "verify_until": nil}
		if e.bounded {
			m["sign_until"] = admSignUntil.Format(time.RFC3339)
			m["verify_until"] = admVerifyUntil.Format(time.RFC3339)
		}
		keys = append(keys, m)
	}
	reads := make([]any, 0, len(readers))
	for _, id := range readers {
		reads = append(reads, id)
	}
	doc := map[string]any{
		"schema":   1,
		"revision": "test-rev-p5",
		"signing": map[string]any{
			"active": active, "emit": string(emit), "keys": keys, "legacy_read_ids": reads,
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

// admAllowVersioned lets New accept emit "versioned" for the test (the
// release check refuses it until phase 6).
func admAllowVersioned(t *testing.T) {
	t.Helper()
	orig := keyringReleaseCheck
	keyringReleaseCheck = func(*keyring.Keyring) error { return nil }
	t.Cleanup(func() { keyringReleaseCheck = orig })
}

// admRollout returns the keyrings of two replicas mid-rollout: A still
// issues with gen1, B has activated gen2 and bounds gen1. Both hold all
// generations; with emit legacy every issuing generation is a legacy
// reader on both (fleet preparation, design addendum §3).
func admRollout(t *testing.T, emit keyring.Emit) (a, b *keyring.Keyring) {
	t.Helper()
	readers := []string{"legacy", "gen1", "gen2"}
	legacy := admEntry{id: "legacy", secret: krLegacySecret}
	a = admKeyring(t, "gen1", emit, readers, legacy, admEntry{id: "gen1", secret: krGen1Secret}, admEntry{id: "gen2", secret: krGen2Secret})
	b = admKeyring(t, "gen2", emit, readers, legacy, admEntry{id: "gen1", secret: krGen1Secret, bounded: true}, admEntry{id: "gen2", secret: krGen2Secret})
	return a, b
}

// admReplica builds a real hub Server over st with keyring k, through New.
// Its keyring deadline clock follows the relay admission clock, so one
// relayClock drives claim expiry and generation deadlines together.
func admReplica(t *testing.T, st store.Store, k *keyring.Keyring) *Server {
	t.Helper()
	cfg := testServerConfig()
	cfg.Keyring = k
	cfg.SharedSigningSecret = krLegacySecret
	cfg.KeyringNow = func() time.Time { return conduitRelayNow() }
	srv, err := newTestHubServer(t, cfg, st)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	waitUserScopedDataSweep(t, srv)
	return srv
}

// admPair is two hub replicas in external relay mode over one store and
// one conduit registry, with relay-0 registered once and calling both.
type admPair struct {
	cf     *conduitFixture
	a, b   *relayOpsFixture
	clock  *time.Time
	relayG int64
}

func newAdmPair(t *testing.T, ka, kb *keyring.Keyring) *admPair {
	t.Helper()
	now := relayClock(t, admT0)
	st, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	// As testServerOnMigratedStore: the fixture creates agents directly,
	// and the store is closed after both replicas shut down (LIFO).
	_ = st.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")
	t.Cleanup(func() { _ = st.Close() })
	srvA := admReplica(t, st, ka)
	srvB := admReplica(t, st, kb)
	cf := newConduitFixtureOn(t, srvA, st)
	srvB.conduitGrants = newConduitGrantKeys(&memoryConduitGrantKeyStore{}, cf.clock.Now)
	setConduitExperiment(t, srvB, true)

	regStore := entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	fa := startRelayOpsHubOn(t, srvA, st, regStore, registry.New(regStore, registry.Config{}), true)
	fb := startRelayOpsHubOn(t, srvB, st, regStore, registry.New(regStore, registry.Config{}), false)
	fb.relayGen = fa.relayGen
	return &admPair{cf: cf, a: fa, b: fb, clock: now, relayG: fa.relayGen}
}

func (p *admPair) at(d time.Duration) { *p.clock = admT0.Add(d) }

// agentToken is an agent credential of the fixture's agent, minted on
// replica A at the current test clock. Agent JWTs are signed by the
// signing ring since Phase 3 of ptone/scion#1530, so A signs with its
// active generation, and minting fails once that generation reaches its
// sign_until. B verifies the JWT while the signing generation verifies
// there: an ID-less JWT (emit legacy) through B's legacy readers, a
// versioned one through its key ID, each before that generation's
// verify_until on B.
func (p *admPair) agentToken(t *testing.T) string {
	t.Helper()
	tok, err := p.cf.srv.GenerateAgentToken(p.cf.agent.ID, p.cf.agent.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	return tok
}

// agentHeader carries agentToken as the agent credential header.
func (p *admPair) agentHeader(t *testing.T) http.Header {
	t.Helper()
	return http.Header{"X-Scion-Agent-Token": {p.agentToken(t)}}
}

// refresh refreshes the session on f with a credential minted now.
func (p *admPair) refresh(t *testing.T, f *relayOpsFixture, sessionID, token string) (string, error) {
	t.Helper()
	return p.refreshWith(t, f, sessionID, token, p.agentToken(t))
}

// refreshWith refreshes the session on f with the credential cred.
func (p *admPair) refreshWith(t *testing.T, f *relayOpsFixture, sessionID, token, cred string) (string, error) {
	t.Helper()
	var out relay.HubRefreshResponse
	err := f.hub.Call(context.Background(), relay.HubSessionPath(sessionID, "refresh"),
		relay.HubRefreshRequest{AdmissionToken: token, Credential: []byte(cred)}, &out)
	return out.AdmissionToken, err
}

// assertTokenFormat checks a token's wire format: versioned under kid, or
// the legacy ID-less form.
func assertTokenFormat(t *testing.T, emit keyring.Emit, kid, token string) {
	t.Helper()
	if emit == keyring.EmitVersioned {
		assert.True(t, strings.HasPrefix(token, "ca3."+kid+"."), "a versioned token under %s", kid)
		return
	}
	assert.NotContains(t, token, ".", "a legacy (ID-less) token")
}

// TestAdmissionKeyring_CrossReplicaMixedActive (P5 acceptance): with two
// replicas on different active generations, a session admitted on one is
// served by the other (RPC forward), re-checked there with a re-issue
// under that replica's active generation that the first accepts, and its
// expired token refreshed there; in both wire formats.
func TestAdmissionKeyring_CrossReplicaMixedActive(t *testing.T) {
	admAllowVersioned(t)
	for _, emit := range []keyring.Emit{keyring.EmitLegacy, keyring.EmitVersioned} {
		t.Run(string(emit), func(t *testing.T) {
			ka, kb := admRollout(t, emit)
			p := newAdmPair(t, ka, kb)

			adm := p.a.mustAdmit(t, bearer(testDevToken), "s1")
			assertTokenFormat(t, emit, "gen1", adm.AdmissionToken)
			_, err := rpcCall(t, p.b.hub, "s1", adm.AdmissionToken, "/v1/unknown")
			require.NoError(t, err, "a token admitted on A is accepted by B")

			// Re-check on B past half the lifetime re-issues under gen2,
			// which A accepts.
			p.at(35 * time.Minute)
			res := sessionRecheck(t, p.b.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: adm.AdmissionToken})
			require.True(t, res[0].OK, "%+v", res[0])
			require.NotEmpty(t, res[0].AdmissionToken, "re-issued")
			assertTokenFormat(t, emit, "gen2", res[0].AdmissionToken)
			_, err = rpcCall(t, p.a.hub, "s1", res[0].AdmissionToken, "/v1/unknown")
			require.NoError(t, err, "a token re-issued on B is accepted by A")

			// And the reverse: A re-checks B's token and re-issues under gen1.
			p.at(70 * time.Minute)
			res = sessionRecheck(t, p.a.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: res[0].AdmissionToken})
			require.True(t, res[0].OK, "%+v", res[0])
			require.NotEmpty(t, res[0].AdmissionToken)
			assertTokenFormat(t, emit, "gen1", res[0].AdmissionToken)

			// Refresh on B of an expired token admitted on A.
			p.at(0)
			agentAdm := p.a.mustAdmit(t, p.agentHeader(t), "s-agent")
			assertTokenFormat(t, emit, "gen1", agentAdm.AdmissionToken)
			p.at(conduitAdmissionMaxAge + time.Minute)
			fresh, err := p.refresh(t, p.b, "s-agent", agentAdm.AdmissionToken)
			require.NoError(t, err, "refresh on B of an expired A token")
			assertTokenFormat(t, emit, "gen2", fresh)
			// Agent sessions may not forward this RPC (403 after the token
			// check), so A's acceptance is shown by a re-check instead.
			res = sessionRecheck(t, p.a.hub, relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: fresh})
			require.True(t, res[0].OK && !res[0].Close, "the refreshed token is accepted by A: %+v", res[0])
		})
	}
}

// TestAdmissionKeyring_RetiredGenerationAndExpiry: an expired token stays
// usable for re-check and refresh while its generation verifies; at and
// after verify_until it opens nowhere, whatever the operation accepts. An
// active generation at or after its sign_until mints nothing: admission
// fails, re-check keeps a healthy session without a re-issue, and refresh
// fails.
func TestAdmissionKeyring_RetiredGenerationAndExpiry(t *testing.T) {
	admAllowVersioned(t)
	for _, emit := range []keyring.Emit{keyring.EmitLegacy, keyring.EmitVersioned} {
		t.Run(string(emit)+" verify_until", func(t *testing.T) {
			ka, kb := admRollout(t, emit)
			p := newAdmPair(t, ka, kb)
			adm := p.a.mustAdmit(t, p.agentHeader(t), "s-agent")

			// Expired claims, gen1 still verifying on B.
			p.at(conduitAdmissionMaxAge + time.Minute)
			res := sessionRecheck(t, p.b.hub, relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: adm.AdmissionToken})
			require.True(t, res[0].OK, "expired claims, generation still verifying: %+v", res[0])

			// A fresh gen1 token from A (whose gen1 is unbounded), its claims
			// unexpired throughout: B's rpc accepts it just before gen1's
			// verify_until and refuses it at and after. A user session, which
			// may forward the RPC (an agent session gets 403 after the token
			// check).
			*p.clock = admVerifyUntil.Add(-10 * time.Minute)
			fresh := p.a.mustAdmit(t, bearer(testDevToken), "s-fresh")
			assertTokenFormat(t, emit, "gen1", fresh.AdmissionToken)
			*p.clock = admVerifyUntil.Add(-time.Second)
			_, err := rpcCall(t, p.b.hub, "s-fresh", fresh.AdmissionToken, "/v1/unknown")
			require.NoError(t, err, "rpc just before verify_until")

			for _, at := range []time.Time{admVerifyUntil, admVerifyUntil.Add(time.Second)} {
				*p.clock = at
				_, err := rpcCall(t, p.b.hub, "s-fresh", fresh.AdmissionToken, "/v1/unknown")
				assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "rpc of an unexpired token at verify_until%+v", at.Sub(admVerifyUntil))
				res := sessionRecheck(t, p.b.hub, relay.HubSessionRecheckItem{SessionID: "s-agent", AdmissionToken: adm.AdmissionToken})
				assert.True(t, res[0].Close, "re-check under a retired generation closes")
				assert.Empty(t, res[0].AdmissionToken)
				_, err = p.refresh(t, p.b, "s-agent", adm.AdmissionToken)
				assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "refresh under a retired generation")
			}
		})
	}

	t.Run("sign_until", func(t *testing.T) {
		// A replica whose active gen1 is bounded: it issues until
		// admSignUntil and nothing after.
		closing := admKeyring(t, "gen1", keyring.EmitVersioned, []string{"legacy", "gen1", "gen2"},
			admEntry{id: "legacy", secret: krLegacySecret}, admEntry{id: "gen1", secret: krGen1Secret, bounded: true}, admEntry{id: "gen2", secret: krGen2Secret})
		_, kb := admRollout(t, keyring.EmitVersioned)
		p := newAdmPair(t, closing, kb)
		logs := captureAdmissionLogs(t)

		healthy := p.a.mustAdmit(t, bearer(testDevToken), "s1")
		agentAdm := p.a.mustAdmit(t, p.agentHeader(t), "s-agent")
		// Minted now: A's agent JWTs come from the same closing generation,
		// so none can be minted at sign_until (the credential stays valid:
		// gen1 verifies until admVerifyUntil, the JWT lasts 10h).
		cred := p.agentToken(t)

		for _, at := range []time.Time{admSignUntil, admSignUntil.Add(time.Second)} {
			*p.clock = at
			_, err := p.a.admit(t, bearer(testDevToken), "s-new")
			require.Error(t, err, "no admission at sign_until%+v", at.Sub(admSignUntil))
			res := sessionRecheck(t, p.a.hub, relay.HubSessionRecheckItem{SessionID: "s1", AdmissionToken: healthy.AdmissionToken})
			assert.True(t, res[0].OK, "a healthy session stays open")
			assert.Empty(t, res[0].AdmissionToken, "but nothing is re-issued under a closed key")
			// The credential and the agent pass; only re-issuing the
			// admission token fails (hubRelayRefresh: 500 "refresh failed",
			// which the hub client reports as unavailable).
			_, err = p.refreshWith(t, p.a, "s-agent", agentAdm.AdmissionToken, cred)
			require.ErrorIs(t, err, relay.ErrHubUnavailable, "no refresh under a closed key")
			assert.ErrorContains(t, err, "HTTP 500", "the re-issue under the closed key fails")
		}
		assertNoAdmissionSecret(t, logs.String())
	})
}

// admLogBuffer is a log sink safe for concurrent handlers.
type admLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *admLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *admLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureAdmissionLogs routes the default logger to a buffer for the test.
func captureAdmissionLogs(t *testing.T) *admLogBuffer {
	t.Helper()
	buf := &admLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// preChangeAdmissionAEAD is the admission token cipher exactly as it was
// built before the signing ring: HKDF-SHA256 of the shared secret with the
// fixed label, AES-256-GCM. Recomputed here independently of
// conduitAdmissionPurpose.
func preChangeAdmissionAEAD(t *testing.T, secret string) cipher.AEAD {
	t.Helper()
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, "scion conduit relay admission token aead v2", 32)
	require.NoError(t, err)
	block, err := aes.NewCipher(key)
	require.NoError(t, err)
	aead, err := cipher.NewGCM(block)
	require.NoError(t, err)
	return aead
}

func sealWith(t *testing.T, aead cipher.AEAD, payload []byte, aad string) []byte {
	t.Helper()
	nonce := make([]byte, aead.NonceSize())
	_, err := rand.Read(nonce)
	require.NoError(t, err)
	return aead.Seal(nonce, nonce, payload, []byte(aad))
}

func openWith(aead cipher.AEAD, sealed []byte, aad string) ([]byte, error) {
	if len(sealed) < aead.NonceSize() {
		return nil, errAdmissionTokenInvalid
	}
	return aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte(aad))
}

// admUnitServer is a Server with only the configuration admission tokens
// read (unit level: no store).
func admUnitServer(k *keyring.Keyring, secret string, now time.Time) *Server {
	return &Server{config: ServerConfig{Keyring: k, SharedSigningSecret: secret, KeyringNow: func() time.Time { return now }}}
}

var admTestClaims = conduitAdmissionClaims{SessionID: "s1", Relay: "relay-0", RelayGen: 3, Kind: registry.PrincipalUser, ID: "u1", AdmittedAt: 1, IssuedAt: 1, Expiry: 1 << 40}

var admTestCheck = admissionCheck{sessionID: "s1", relay: "relay-0", relayGen: 3}

// TestAdmissionKeyring_WireFormats pins the legacy format byte-compatible
// with the pre-change implementation, the versioned format and its key ID
// binding, and the strict refusal of malformed or mismatched tokens
// without a fallback to another key or format.
func TestAdmissionKeyring_WireFormats(t *testing.T) {
	now := admT0
	payload, err := json.Marshal(conduitAdmissionClaims{V: 2, SessionID: "s1", Relay: "relay-0", RelayGen: 3, Kind: registry.PrincipalUser, ID: "u1", AdmittedAt: 1, IssuedAt: 1, Expiry: 1 << 40})
	require.NoError(t, err)

	t.Run("legacy mode is byte-compatible both ways", func(t *testing.T) {
		srv := admUnitServer(nil, testRelaySessionSecret, now)
		pre := preChangeAdmissionAEAD(t, testRelaySessionSecret)

		old := base64.RawURLEncoding.EncodeToString(sealWith(t, pre, payload, "scion conduit relay admission token"))
		c, err := srv.verifyAdmissionToken(old, admTestCheck, now)
		require.NoError(t, err, "a pre-change token opens")
		assert.Equal(t, "u1", c.ID)

		tok, err := srv.mintAdmissionToken(admTestClaims)
		require.NoError(t, err)
		sealed, err := base64.RawURLEncoding.DecodeString(tok)
		require.NoError(t, err, "a legacy token is bare base64url")
		_, err = openWith(pre, sealed, "scion conduit relay admission token")
		require.NoError(t, err, "the pre-change cipher opens a new legacy-mode token")
	})

	t.Run("ring legacy generation emits the pre-change format", func(t *testing.T) {
		k := admKeyring(t, "legacy", keyring.EmitLegacy, []string{"legacy"}, admEntry{id: "legacy", secret: krLegacySecret})
		srv := admUnitServer(k, krLegacySecret, now)
		tok, err := srv.mintAdmissionToken(admTestClaims)
		require.NoError(t, err)
		sealed, err := base64.RawURLEncoding.DecodeString(tok)
		require.NoError(t, err)
		_, err = openWith(preChangeAdmissionAEAD(t, krLegacySecret), sealed, "scion conduit relay admission token")
		require.NoError(t, err)
	})

	t.Run("versioned format binds the key ID", func(t *testing.T) {
		k := admKeyring(t, "gen1", keyring.EmitVersioned, []string{"legacy", "gen1", "gen2"},
			admEntry{id: "legacy", secret: krLegacySecret}, admEntry{id: "gen1", secret: krGen1Secret}, admEntry{id: "gen2", secret: krGen2Secret})
		srv := admUnitServer(k, krLegacySecret, now)
		tok, err := srv.mintAdmissionToken(admTestClaims)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(tok, "ca3.gen1."))
		body := strings.TrimPrefix(tok, "ca3.gen1.")
		sealed, err := base64.RawURLEncoding.DecodeString(body)
		require.NoError(t, err)
		_, err = openWith(preChangeAdmissionAEAD(t, krGen1Secret), sealed, "scion conduit relay admission token:ca3:gen1")
		require.NoError(t, err, "gen1's key (unchanged derivation) with the versioned AAD")
		_, err = srv.verifyAdmissionToken(tok, admTestCheck, now)
		require.NoError(t, err)

		for name, bad := range map[string]string{
			"key ID swapped to another generation": "ca3.gen2." + body,
			"key ID unknown":                       "ca3.gen9." + body,
			"version prefix stripped":              body,
			"key ID malformed":                     "ca3.gen!1." + body,
			"key ID overlong":                      "ca3." + strings.Repeat("a", 33) + "." + body,
			"key ID empty":                         "ca3.." + body,
			"no body":                              "ca3.gen1.",
			"no key ID separator":                  "ca3.gen1",
			"extra part":                           tok + ".x",
			"unknown version":                      "ca4.gen1." + body,
			"bad base64":                           "ca3.gen1." + body + "*",
			"empty":                                "",
		} {
			_, err := srv.verifyAdmissionToken(bad, admTestCheck, now)
			assert.ErrorIs(t, err, errAdmissionTokenInvalid, name)
		}
	})

	t.Run("an ID-less token opens only with the legacy readers", func(t *testing.T) {
		pre := preChangeAdmissionAEAD(t, krGen1Secret)
		old := base64.RawURLEncoding.EncodeToString(sealWith(t, pre, payload, "scion conduit relay admission token"))
		entries := []admEntry{{id: "legacy", secret: krLegacySecret}, {id: "gen1", secret: krGen1Secret}, {id: "gen2", secret: krGen2Secret}}
		with := admUnitServer(admKeyring(t, "gen2", keyring.EmitLegacy, []string{"gen2", "gen1"}, entries...), krLegacySecret, now)
		_, err := with.verifyAdmissionToken(old, admTestCheck, now)
		require.NoError(t, err, "gen1 is a legacy reader")
		without := admUnitServer(admKeyring(t, "gen2", keyring.EmitLegacy, []string{"gen2"}, entries...), krLegacySecret, now)
		_, err = without.verifyAdmissionToken(old, admTestCheck, now)
		assert.ErrorIs(t, err, errAdmissionTokenInvalid, "gen1 is not a legacy reader")
		// A versioned AAD sealed as a bare token never opens as legacy.
		v2AAD := base64.RawURLEncoding.EncodeToString(sealWith(t, pre, payload, "scion conduit relay admission token:ca3:gen1"))
		_, err = with.verifyAdmissionToken(v2AAD, admTestCheck, now)
		assert.ErrorIs(t, err, errAdmissionTokenInvalid)
	})
}

// TestAdmissionKeyring_ManagedNeverFallsBack: in managed ring mode an
// unusable ring refuses every seal and open and the startup check; the
// shared session secret is never used instead. Legacy mode without a
// secret keeps refusing.
func TestAdmissionKeyring_ManagedNeverFallsBack(t *testing.T) {
	pre := preChangeAdmissionAEAD(t, testRelaySessionSecret)
	payload, err := json.Marshal(conduitAdmissionClaims{V: 2, SessionID: "s1", Relay: "relay-0", RelayGen: 3, Kind: registry.PrincipalUser, ID: "u1", Expiry: 1 << 40})
	require.NoError(t, err)
	tokenUnderSecret := base64.RawURLEncoding.EncodeToString(sealWith(t, pre, payload, "scion conduit relay admission token"))

	srv := admUnitServer(&keyring.Keyring{}, testRelaySessionSecret, admT0)
	assert.ErrorIs(t, srv.checkConduitAdmissionKeys(), errConduitAdmissionNoActive)
	_, err = srv.mintAdmissionToken(admTestClaims)
	assert.ErrorIs(t, err, errConduitAdmissionIssuanceClosed)
	_, err = srv.verifyAdmissionToken(tokenUnderSecret, admTestCheck, admT0)
	assert.ErrorIs(t, err, errAdmissionTokenInvalid, "a token under the shared secret does not open in managed mode")

	assert.ErrorIs(t, admUnitServer(nil, "", admT0).checkConduitAdmissionKeys(), errConduitAdmissionNoKey)
	assert.NoError(t, admUnitServer(nil, testRelaySessionSecret, admT0).checkConduitAdmissionKeys())
}

// assertNoAdmissionSecret fails when s carries a fixture secret or a key
// derived from one.
func assertNoAdmissionSecret(t *testing.T, s string) {
	t.Helper()
	for _, secret := range []string{krLegacySecret, krGen1Secret, krGen2Secret} {
		require.NotContains(t, s, secret)
		key := conduitAdmissionPurposeKey(t, secret)
		require.NotContains(t, s, base64.RawURLEncoding.EncodeToString(key))
		require.NotContains(t, s, base64.StdEncoding.EncodeToString(key))
	}
}

func conduitAdmissionPurposeKey(t *testing.T, secret string) []byte {
	t.Helper()
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, "scion conduit relay admission token aead v2", 32)
	require.NoError(t, err)
	return key
}
