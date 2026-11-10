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

package hub

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 3 of ptone/scion#1530: agent and user HS256 JWTs signed and
// verified from the keyring signing ring. Synthetic fixtures only.
const (
	jkLegacySecret = "test-only-p3-legacy-server-secret"
	jkGen2Secret   = "test-only-p3-generation-2-secret-0123456789abcdef"
	jkGen3Secret   = "test-only-p3-generation-3-secret-0123456789abcdef"
	jkGen1Secret   = "test-only-p3-generation-1-secret-0123456789abcdef"
)

var jkT0 = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

type jkClock struct{ t time.Time }

func (c *jkClock) now() time.Time { return c.t }

type jkEntry struct {
	id, secret            string
	signUntil, verifyUntl *time.Time
}

func jkAt(d time.Duration) *time.Time {
	t := jkT0.Add(d)
	return &t
}

// jkRing builds and parses a version-1 keyring file. It does not apply the
// release check (emit versioned parses; the config refuses it until phase
// 6), so tests can build rings directly.
func jkRing(t testing.TB, emit, active string, legacyReads []string, entries ...jkEntry) *keyring.Keyring {
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
		"revision": "test-p3-rev",
		"signing":  map[string]any{"active": active, "emit": emit, "keys": keys, "legacy_read_ids": reads},
		"encryption": map[string]any{
			"active": "legacy", "write_format": "v1",
			"keys":               []any{map[string]any{"id": "legacy", "key_b64": base64.StdEncoding.EncodeToString(keyring.DeriveLegacyEncryptionKey(jkLegacySecret))}},
			"legacy_v1_read_ids": []any{"legacy"},
		},
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	k, err := keyring.Parse(data)
	require.NoError(t, err)
	return k
}

// jkRollout is the design's signing activation (Migration step 4) for two
// replicas. A still issues with the imported legacy generation (emit
// legacy) and knows gen2 as staged; B has activated gen2 with emit
// versioned and keeps legacy as the outgoing generation, issuance closed at
// T0+1h and verification until verifyUntil. Both read ID-less tokens with
// the legacy generation only.
func jkRollout(t testing.TB, verifyUntil time.Duration) (a, b *keyring.Keyring) {
	t.Helper()
	a = jkRing(t, "legacy", "legacy", []string{"legacy"},
		jkEntry{id: "legacy", secret: jkLegacySecret},
		jkEntry{id: "gen2", secret: jkGen2Secret})
	b = jkRing(t, "versioned", "gen2", []string{"legacy"},
		jkEntry{id: "legacy", secret: jkLegacySecret, signUntil: jkAt(time.Hour), verifyUntl: jkAt(verifyUntil)},
		jkEntry{id: "gen2", secret: jkGen2Secret})
	return a, b
}

func jkAgentSvc(t testing.TB, k *keyring.Keyring, clk *jkClock) *AgentTokenService {
	t.Helper()
	svc, err := newRingAgentTokenService(k.Signing(), AgentTokenConfig{TokenDuration: 10 * time.Hour}, clk.now)
	require.NoError(t, err)
	return svc
}

func jkUserSvc(t testing.TB, k *keyring.Keyring, clk *jkClock, cfg UserTokenConfig) *UserTokenService {
	t.Helper()
	svc, err := newRingUserTokenService(k.Signing(), cfg, clk.now)
	require.NoError(t, err)
	return svc
}

// jkWebRefresh refreshes a user token pair the way the web server does
// (refreshHubTokens), with svc as its token service and no store: the new
// pair carries the presented refresh token's session generation.
func jkWebRefresh(t *testing.T, svc *UserTokenService, refreshToken string) (string, string, int64, error) {
	t.Helper()
	ws := newTestWebServer(t, WebServerConfig{})
	ws.SetUserTokenService(svc)
	return ws.refreshHubTokens(context.Background(), refreshToken)
}

// jwtHeader returns the decoded protected header of a compact JWS.
func jwtHeader(t testing.TB, token string) map[string]any {
	t.Helper()
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	require.NoError(t, err)
	var h map[string]any
	require.NoError(t, json.Unmarshal(raw, &h))
	return h
}

// withHeader replaces the protected header of a compact JWS, keeping its
// payload and signature.
func withHeader(t testing.TB, token string, h map[string]any) string {
	t.Helper()
	parts := strings.Split(token, ".")
	raw, err := json.Marshal(h)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(raw) + "." + parts[1] + "." + parts[2]
}

// signRaw signs claims with key under alg, with kid in the protected header
// when non-empty.
func signRaw(t testing.TB, alg jose.SignatureAlgorithm, key []byte, kid string, claims any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts = opts.WithHeader(jose.HeaderKey("kid"), kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	require.NoError(t, err)
	tok, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	if kid != "" {
		require.Equal(t, kid, jwtHeader(t, tok)["kid"], "signRaw must write the kid header")
	}
	return tok
}

func jkAgentClaims(now time.Time, ttl time.Duration) AgentTokenClaims {
	return AgentTokenClaims{
		Claims: jwt.Claims{
			Issuer: AgentTokenIssuer, Subject: "agent-1", Audience: jwt.Audience{AgentTokenAudience},
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now),
			Expiry: jwt.NewNumericDate(now.Add(ttl)), ID: "jti-1",
		},
		ScopeSchema: CurrentAgentScopeSchema,
	}
}

// assertNoJWTKeyMaterial checks that s names no fixture secret and no derived
// JWT key (raw, hex or a fingerprint prefix).
func assertNoJWTKeyMaterial(t testing.TB, s string) {
	t.Helper()
	for _, secret := range []string{jkLegacySecret, jkGen1Secret, jkGen2Secret, jkGen3Secret} {
		assert.NotContains(t, s, secret)
		for _, name := range []string{SecretKeyAgentSigningKey, SecretKeyUserSigningKey} {
			k := deriveSharedSigningKey(secret, name)
			assert.NotContains(t, s, string(k))
			assert.NotContains(t, s, hex.EncodeToString(k)[:16])
			assert.NotContains(t, s, base64.StdEncoding.EncodeToString(k))
		}
	}
}

// TestJWTPurpose_LegacyGenerationMatchesSharedSecretDerivation: the ring's
// "legacy" generation derives exactly today's shared-secret JWT keys
// (golden, compared by bytes), so existing tokens keep validating.
func TestJWTPurpose_LegacyGenerationMatchesSharedSecretDerivation(t *testing.T) {
	k := jkRing(t, "legacy", "legacy", []string{"legacy"}, jkEntry{id: "legacy", secret: jkLegacySecret})
	for _, tc := range []struct {
		p    keyring.Purpose
		name string
	}{{agentJWTPurpose, SecretKeyAgentSigningKey}, {userJWTPurpose, SecretKeyUserSigningKey}} {
		got, err := k.Signing().Active(tc.p, jkT0)
		require.NoError(t, err)
		assert.Equal(t, "legacy", got.ID)
		assert.Equal(t, deriveSharedSigningKey(jkLegacySecret, tc.name), got.Bytes(), tc.name)
		assert.Equal(t, tc.name, tc.p.Name())
	}
	// Domain separation is kept: the agent and user keys differ.
	a, _ := k.Signing().Active(agentJWTPurpose, jkT0)
	u, _ := k.Signing().Active(userJWTPurpose, jkT0)
	assert.NotEqual(t, a.Bytes(), u.Bytes())
	// Pinned golden: sha256("scion-hub-signing-key:agent_signing_key:" + secret).
	assert.Equal(t, "3c2970fc", hex.EncodeToString(deriveSharedSigningKey("golden", SecretKeyAgentSigningKey))[:8])
}

// TestJWTRing_LegacyEmitInteropWithLegacyMode: under emit legacy the ring
// writes exactly the pre-keyring JOSE header and its tokens and a
// legacy-mode (SharedSigningSecret) hub's tokens validate on each other.
func TestJWTRing_LegacyEmitInteropWithLegacyMode(t *testing.T) {
	clk := &jkClock{t: jkT0}
	k := jkRing(t, "legacy", "legacy", []string{"legacy"}, jkEntry{id: "legacy", secret: jkLegacySecret})
	ringAgent := jkAgentSvc(t, k, clk)
	ringUser := jkUserSvc(t, k, clk, UserTokenConfig{})
	legacyAgent, err := NewAgentTokenService(AgentTokenConfig{SigningKey: deriveSharedSigningKey(jkLegacySecret, SecretKeyAgentSigningKey)})
	require.NoError(t, err)
	legacyUser, err := NewUserTokenService(UserTokenConfig{SigningKey: deriveSharedSigningKey(jkLegacySecret, SecretKeyUserSigningKey)})
	require.NoError(t, err)
	clk.t = time.Now() // the legacy services use the wall clock

	fromRing, _, err := ringAgent.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "run-1")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"alg": "HS256", "typ": "JWT"}, jwtHeader(t, fromRing))
	claims, err := legacyAgent.ValidateAgentToken(fromRing)
	require.NoError(t, err)
	assert.Equal(t, "run-1", claims.RunID)
	fromLegacy, _, err := legacyAgent.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "")
	require.NoError(t, err)
	_, err = ringAgent.ValidateAgentToken(fromLegacy)
	require.NoError(t, err)

	access, refresh, _, err := ringUser.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeWeb)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"alg": "HS256", "typ": "JWT"}, jwtHeader(t, access))
	_, err = legacyUser.ValidateUserToken(access)
	require.NoError(t, err)
	_, err = legacyUser.ValidateRefreshToken(refresh)
	require.NoError(t, err)
	la, _, _, err := legacyUser.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeCLI)
	require.NoError(t, err)
	_, err = ringUser.ValidateUserToken(la)
	require.NoError(t, err)
}

// TestJWTRing_VersionedHeaderCarriesProtectedKid: emit versioned writes a
// protected kid equal to the active generation ID; the kid is covered by
// the MAC, so pointing it at another generation breaks verification.
func TestJWTRing_VersionedHeaderCarriesProtectedKid(t *testing.T) {
	clk := &jkClock{t: jkT0}
	_, b := jkRollout(t, 30*24*time.Hour)
	svc := jkAgentSvc(t, b, clk)
	tok, _, err := svc.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "")
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"alg": "HS256", "kid": "gen2", "typ": "JWT"}, jwtHeader(t, tok))
	_, err = svc.ValidateAgentToken(tok)
	require.NoError(t, err)

	for _, h := range []map[string]any{
		{"alg": "HS256", "kid": "legacy", "typ": "JWT"}, // known generation, wrong key
		{"alg": "HS256", "typ": "JWT"},                  // kid stripped: legacy readers only
		{"alg": "HS256", "kid": "nope", "typ": "JWT"},   // unknown
	} {
		_, err := svc.ValidateAgentToken(withHeader(t, tok, h))
		require.Error(t, err, "%v", h)
		assertNoJWTKeyMaterial(t, err.Error())
	}
}

// TestJWTRing_UnknownKidNeverFallsBack: a token whose signature is valid
// under the active (or a legacy-reader) generation but whose kid names no
// generation refuses: an unknown ID never falls back to other keys.
func TestJWTRing_UnknownKidNeverFallsBack(t *testing.T) {
	clk := &jkClock{t: jkT0}
	a, b := jkRollout(t, 30*24*time.Hour)
	for _, k := range []*keyring.Keyring{a, b} {
		svc := jkAgentSvc(t, k, clk)
		// The legacy-secret token is the discriminating case: without its
		// kid it would verify through the legacy reader, so only the
		// unknown kid refuses it. The gen2 token refuses either way on A
		// (gen2 is no legacy reader there) and is a second unknown-kid check.
		for _, secret := range []string{jkLegacySecret, jkGen2Secret} {
			tok := signRaw(t, jose.HS256, deriveSharedSigningKey(secret, SecretKeyAgentSigningKey), "nope", jkAgentClaims(jkT0, time.Hour))
			_, err := svc.ValidateAgentToken(tok)
			require.Error(t, err)
			assert.ErrorIs(t, err, errJWTNoEligibleKey)
			assert.NotContains(t, err.Error(), "nope", "the attacker-supplied kid is not echoed")
			assertNoJWTKeyMaterial(t, err.Error())
		}
	}
}

// TestJWTRing_IDlessUsesOnlyLegacyReaders: an ID-less token is verified
// only against legacy_read_ids, not against every generation in the ring.
func TestJWTRing_IDlessUsesOnlyLegacyReaders(t *testing.T) {
	clk := &jkClock{t: jkT0}
	a, _ := jkRollout(t, 30*24*time.Hour)
	svc := jkAgentSvc(t, a, clk)
	gen2NoKid := signRaw(t, jose.HS256, deriveSharedSigningKey(jkGen2Secret, SecretKeyAgentSigningKey), "", jkAgentClaims(jkT0, time.Hour))
	_, err := svc.ValidateAgentToken(gen2NoKid)
	assert.ErrorIs(t, err, errJWTNoEligibleKey)
	gen2Kid := signRaw(t, jose.HS256, deriveSharedSigningKey(jkGen2Secret, SecretKeyAgentSigningKey), "gen2", jkAgentClaims(jkT0, time.Hour))
	_, err = svc.ValidateAgentToken(gen2Kid)
	assert.NoError(t, err, "the staged generation verifies by its key ID")
}

// TestJWTRing_AlgorithmPinned: only HS256 is accepted, under any key.
func TestJWTRing_AlgorithmPinned(t *testing.T) {
	clk := &jkClock{t: jkT0}
	a, _ := jkRollout(t, 30*24*time.Hour)
	svc := jkAgentSvc(t, a, clk)
	key := deriveSharedSigningKey(jkLegacySecret, SecretKeyAgentSigningKey)
	for _, alg := range []jose.SignatureAlgorithm{jose.HS384, jose.HS512} {
		long := append(append([]byte{}, key...), key...) // HS384/512 need longer keys
		_, err := svc.ValidateAgentToken(signRaw(t, alg, long, "", jkAgentClaims(jkT0, time.Hour)))
		require.Error(t, err, alg)
	}
	ok := signRaw(t, jose.HS256, key, "", jkAgentClaims(jkT0, time.Hour))
	none := withHeader(t, ok, map[string]any{"alg": "none", "typ": "JWT"})
	_, err := svc.ValidateAgentToken(none)
	require.Error(t, err)
}

// TestJWTRing_RetiredGenerationRefuses: at or after a generation's
// verify_until its tokens refuse, in kid and ID-less form, even while
// their exp is far in the future.
func TestJWTRing_RetiredGenerationRefuses(t *testing.T) {
	verifyUntil := 48 * time.Hour
	_, b := jkRollout(t, verifyUntil)
	clk := &jkClock{t: jkT0}
	svc := jkAgentSvc(t, b, clk)
	key := deriveSharedSigningKey(jkLegacySecret, SecretKeyAgentSigningKey)
	idless := signRaw(t, jose.HS256, key, "", jkAgentClaims(jkT0, 365*24*time.Hour))
	withKid := signRaw(t, jose.HS256, key, "legacy", jkAgentClaims(jkT0, 365*24*time.Hour))
	for _, tok := range []string{idless, withKid} {
		clk.t = jkT0.Add(verifyUntil - time.Second)
		_, err := svc.ValidateAgentToken(tok)
		require.NoError(t, err)
		clk.t = jkT0.Add(verifyUntil)
		_, err = svc.ValidateAgentToken(tok)
		assert.ErrorIs(t, err, errJWTNoEligibleKey)
	}

	// A retired generation that is not a legacy reader (gen1), selectable
	// only by its kid: verify_until is enforced on the kid-selected
	// generation too, and its ID-less form never verifies at all.
	k := jkRing(t, "versioned", "gen2", []string{"legacy"},
		jkEntry{id: "legacy", secret: jkLegacySecret},
		jkEntry{id: "gen1", secret: jkGen1Secret, signUntil: jkAt(time.Hour), verifyUntl: jkAt(verifyUntil)},
		jkEntry{id: "gen2", secret: jkGen2Secret})
	clk.t = jkT0
	svc = jkAgentSvc(t, k, clk)
	gen1Key := deriveSharedSigningKey(jkGen1Secret, SecretKeyAgentSigningKey)
	gen1Kid := signRaw(t, jose.HS256, gen1Key, "gen1", jkAgentClaims(jkT0, 365*24*time.Hour))
	gen1Idless := signRaw(t, jose.HS256, gen1Key, "", jkAgentClaims(jkT0, 365*24*time.Hour))
	clk.t = jkT0.Add(verifyUntil - time.Second)
	_, err := svc.ValidateAgentToken(gen1Kid)
	require.NoError(t, err, "gen1 by kid before its verify_until")
	_, err = svc.ValidateAgentToken(gen1Idless)
	assert.ErrorIs(t, err, errJWTNoEligibleKey, "gen1 is not a legacy reader")
	for _, at := range []time.Duration{verifyUntil, verifyUntil + time.Hour} {
		clk.t = jkT0.Add(at)
		_, err = svc.ValidateAgentToken(gen1Kid)
		assert.ErrorIs(t, err, errJWTNoEligibleKey, "gen1 by kid at T0+%s", at)
	}
}

// TestJWTRing_NoIssuanceAfterSignUntil: every agent and user signing path
// refuses at or after the active generation's sign_until, while existing
// tokens keep verifying until verify_until.
func TestJWTRing_NoIssuanceAfterSignUntil(t *testing.T) {
	k := jkRing(t, "legacy", "legacy", []string{"legacy"},
		jkEntry{id: "legacy", secret: jkLegacySecret, signUntil: jkAt(time.Hour), verifyUntl: jkAt(48 * time.Hour)})
	clk := &jkClock{t: jkT0}
	agent := jkAgentSvc(t, k, clk)
	user := jkUserSvc(t, k, clk, UserTokenConfig{})
	before, _, err := agent.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "")
	require.NoError(t, err)
	_, beforeRefresh, _, err := user.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeWeb)
	require.NoError(t, err)

	clk.t = jkT0.Add(time.Hour)
	_, _, err = agent.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "")
	assert.ErrorIs(t, err, keyring.ErrIssuanceClosed)
	_, _, _, err = user.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeWeb)
	assert.ErrorIs(t, err, keyring.ErrIssuanceClosed)
	_, _, err = user.GenerateAccessToken("u1", "u1@example.com", "U", "member", ClientTypeCLI)
	assert.ErrorIs(t, err, keyring.ErrIssuanceClosed)
	_, _, err = user.GenerateAccessTokenWithTTL("u1", "u1@example.com", "U", "member", ClientTypeAPI, time.Minute)
	assert.ErrorIs(t, err, keyring.ErrIssuanceClosed)
	_, err = user.GenerateTestLoginToken("u1")
	assert.ErrorIs(t, err, keyring.ErrIssuanceClosed)
	_, _, _, err = jkWebRefresh(t, user, beforeRefresh)
	assert.ErrorIs(t, err, keyring.ErrIssuanceClosed)

	_, err = agent.ValidateAgentToken(before)
	assert.NoError(t, err, "verification continues until verify_until")
	_, err = user.ValidateRefreshToken(beforeRefresh)
	assert.NoError(t, err)
}

// TestJWTRing_EveryValidatorAcrossReplicas: each user and agent validation
// entry point (the paths every call site uses: auth middleware, refresh,
// validate, GetTokenExpiry for the conduit re-check, test-login) accepts
// old- and new-generation tokens on both replicas during grace, and
// refuses unknown and retired generations.
func TestJWTRing_EveryValidatorAcrossReplicas(t *testing.T) {
	a, b := jkRollout(t, 48*time.Hour)
	clk := &jkClock{t: jkT0}
	userA, userB := jkUserSvc(t, a, clk, UserTokenConfig{}), jkUserSvc(t, b, clk, UserTokenConfig{})
	agentA, agentB := jkAgentSvc(t, a, clk), jkAgentSvc(t, b, clk)

	type minted struct{ agent, access, refresh, cli, testLogin string }
	mint := func(u *UserTokenService, ag *AgentTokenService) minted {
		var m minted
		var err error
		m.agent, _, err = ag.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "")
		require.NoError(t, err)
		m.access, m.refresh, _, err = u.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeWeb)
		require.NoError(t, err)
		m.cli, _, err = u.GenerateAccessToken("u1", "u1@example.com", "U", "member", ClientTypeCLI)
		require.NoError(t, err)
		m.testLogin, err = u.GenerateTestLoginToken("u1")
		require.NoError(t, err)
		return m
	}
	old, fresh := mint(userA, agentA), mint(userB, agentB)
	assert.NotContains(t, jwtHeader(t, old.access), "kid")
	assert.Equal(t, "gen2", jwtHeader(t, fresh.access)["kid"])

	accepts := func(u *UserTokenService, ag *AgentTokenService, m minted) []error {
		_, e1 := ag.ValidateAgentToken(m.agent)
		_, e2 := u.ValidateUserToken(m.access)
		_, e3 := u.ValidateUserToken(m.cli)
		_, e4 := u.ValidateRefreshToken(m.refresh)
		_, e5 := u.GetTokenExpiry(m.access)
		e6 := u.ValidateTestLoginToken(m.testLogin)
		return []error{e1, e2, e3, e4, e5, e6}
	}
	clk.t = jkT0.Add(4 * time.Minute) // within every default lifetime, test-login's 5m included
	for name, errs := range map[string][]error{
		"old on A": accepts(userA, agentA, old), "old on B": accepts(userB, agentB, old),
		"new on A": accepts(userA, agentA, fresh), "new on B": accepts(userB, agentB, fresh),
	} {
		for i, err := range errs {
			assert.NoError(t, err, "%s #%d", name, i)
		}
	}
	// Audiences stay separate: a test-login token is no access token.
	_, err := userB.ValidateUserToken(fresh.testLogin)
	assert.Error(t, err)
	assert.Error(t, userB.ValidateTestLoginToken(fresh.access))

	// Unknown generation: a replica without gen3 refuses gen3 tokens on every path.
	c := jkRing(t, "versioned", "gen3", []string{"legacy"},
		jkEntry{id: "legacy", secret: jkLegacySecret, signUntil: jkAt(time.Hour), verifyUntl: jkAt(48 * time.Hour)},
		jkEntry{id: "gen3", secret: jkGen3Secret})
	unknown := mint(jkUserSvc(t, c, clk, UserTokenConfig{}), jkAgentSvc(t, c, clk))
	for i, err := range accepts(userB, agentB, unknown) {
		assert.ErrorIs(t, err, errJWTNoEligibleKey, "unknown #%d", i)
	}
	// Retired generation: old refresh token (7d) after B's verify_until.
	clk.t = jkT0.Add(48 * time.Hour)
	_, err = userB.ValidateRefreshToken(old.refresh)
	assert.ErrorIs(t, err, errJWTNoEligibleKey)
	_, _, _, err = jkWebRefresh(t, userB, old.refresh)
	assert.ErrorIs(t, err, errJWTNoEligibleKey)
	_, err = userB.GetTokenExpiry(old.cli)
	assert.ErrorIs(t, err, errJWTNoEligibleKey)
}

// TestJWTRing_RefreshWithOldTokenReturnsNewGeneration: user token refresh
// with a valid previous-generation refresh token returns a pair from the
// active generation.
func TestJWTRing_RefreshWithOldTokenReturnsNewGeneration(t *testing.T) {
	a, b := jkRollout(t, 30*24*time.Hour)
	clk := &jkClock{t: jkT0}
	_, oldRefresh, _, err := jkUserSvc(t, a, clk, UserTokenConfig{}).GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeCLI)
	require.NoError(t, err)
	clk.t = jkT0.Add(2 * time.Hour) // after legacy's sign_until on B
	access, refresh, _, err := jkWebRefresh(t, jkUserSvc(t, b, clk, UserTokenConfig{}), oldRefresh)
	require.NoError(t, err)
	for _, tok := range []string{access, refresh} {
		assert.Equal(t, "gen2", jwtHeader(t, tok)["kid"])
		parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.HS256})
		require.NoError(t, err)
		var c UserTokenClaims
		require.NoError(t, parsed.Claims(deriveSharedSigningKey(jkGen2Secret, SecretKeyUserSigningKey), &c))
		assert.Equal(t, "u1", c.UserID)
		assert.Equal(t, map[string]UserTokenType{access: TokenTypeCLI, refresh: TokenTypeRefresh}[tok], c.TokenType)
	}
}

// TestJWTRing_CLIOverlapThirtyDays simulates the design's 30-day CLI
// overlap with a controlled clock: a CLI token minted by the outgoing
// generation just before its sign_until validates on the new-active replica
// until its expiry; with a longer configured CLI lifetime, the
// generation's verify_until (not exp) closes the window.
func TestJWTRing_CLIOverlapThirtyDays(t *testing.T) {
	const skew = 60 * time.Second
	cliTTL := DefaultCLIAccessTokenDuration
	require.Equal(t, 30*24*time.Hour, cliTTL)
	verifyUntil := time.Hour + cliTTL + skew // sign_until + max lifetime + skew
	a, b := jkRollout(t, verifyUntil)
	const lastOld = 50 * time.Minute // the last old-generation issuance, before sign_until (1h)
	clk := &jkClock{t: jkT0.Add(lastOld)}
	oldCLI, _, err := jkUserSvc(t, a, clk, UserTokenConfig{}).GenerateAccessToken("u1", "u1@example.com", "U", "member", ClientTypeCLI)
	require.NoError(t, err)
	userA, userB := jkUserSvc(t, a, clk, UserTokenConfig{}), jkUserSvc(t, b, clk, UserTokenConfig{})
	newCLI, _, err := userB.GenerateAccessToken("u1", "u1@example.com", "U", "member", ClientTypeCLI)
	require.NoError(t, err)

	for _, at := range []time.Duration{time.Hour, 24 * time.Hour, 15 * 24 * time.Hour, lastOld + cliTTL - time.Second} {
		clk.t = jkT0.Add(at)
		_, err := userB.ValidateUserToken(oldCLI)
		assert.NoError(t, err, "old CLI token on B at T0+%s", at)
		_, err = userA.ValidateUserToken(newCLI)
		assert.NoError(t, err, "new CLI token on A at T0+%s", at)
	}
	// Expired by its own exp (30d, plus go-jose's one-minute leeway) before
	// the generation deadline.
	clk.t = jkT0.Add(lastOld + cliTTL + 2*time.Minute)
	require.True(t, clk.t.Before(jkT0.Add(verifyUntil)))
	_, err = userB.ValidateUserToken(oldCLI)
	assert.Error(t, err)

	// A 60-day CLI lifetime: the outgoing generation's verify_until refuses
	// it while exp is still in the future.
	clk.t = jkT0.Add(lastOld)
	longCLI, _, err := jkUserSvc(t, a, clk, UserTokenConfig{CLIAccessTokenDuration: 60 * 24 * time.Hour}).GenerateAccessToken("u1", "u1@example.com", "U", "member", ClientTypeCLI)
	require.NoError(t, err)
	clk.t = jkT0.Add(verifyUntil - time.Second)
	_, err = userB.ValidateUserToken(longCLI)
	assert.NoError(t, err)
	clk.t = jkT0.Add(verifyUntil)
	_, err = userB.ValidateUserToken(longCLI)
	assert.ErrorIs(t, err, errJWTNoEligibleKey)
	exp, err := userA.GetTokenExpiry(longCLI)
	require.NoError(t, err, "A has no deadline on legacy yet")
	assert.True(t, exp.After(clk.t))
}

// TestJWTRing_ServicesRefuseExplicitKeyAndNoActive: a ring service refuses
// an explicit signing key, and a ring without an active generation signs
// nothing and verifies nothing.
func TestJWTRing_ServicesRefuseExplicitKeyAndNoActive(t *testing.T) {
	_, err := newRingAgentTokenService(nil, AgentTokenConfig{SigningKey: []byte("explicit")}, nil)
	require.Error(t, err)
	_, err = newRingUserTokenService(nil, UserTokenConfig{SigningKey: []byte("explicit")}, nil)
	require.Error(t, err)

	empty := &keyring.Keyring{}
	agent, err := newRingAgentTokenService(empty.Signing(), AgentTokenConfig{}, nil)
	require.NoError(t, err)
	_, _, err = agent.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "")
	assert.True(t, errors.Is(err, keyring.ErrNoActiveGeneration))
	tok := signRaw(t, jose.HS256, deriveSharedSigningKey(jkLegacySecret, SecretKeyAgentSigningKey), "", jkAgentClaims(time.Now(), time.Hour))
	_, err = agent.ValidateAgentToken(tok)
	assert.ErrorIs(t, err, errJWTNoEligibleKey)
}

// TestKeyringReleaseCheck_ProductionDefault pins the shared P3/P4 test
// seam to the release check New uses in production.
func TestKeyringReleaseCheck_ProductionDefault(t *testing.T) {
	versioned := jkRing(t, "versioned", "legacy", []string{"legacy"}, jkEntry{id: "legacy", secret: jkLegacySecret})
	assert.Error(t, keyringReleaseCheck(versioned), "the production default refuses emit versioned")
	assert.Equal(t, versioned.CheckSessionPilot() == nil, keyringReleaseCheck(versioned) == nil)
	ok := jkRing(t, "legacy", "legacy", []string{"legacy"}, jkEntry{id: "legacy", secret: jkLegacySecret})
	assert.NoError(t, keyringReleaseCheck(ok))
}
