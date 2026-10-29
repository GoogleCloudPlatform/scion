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
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 3 of ptone/scion#1530 through real hub servers over one real
// SQLite store and a real local secret backend: managed ring mode signs and
// verifies agent and user JWTs from the ring, never reads, publishes or
// rewrites the retained single-key aliases (agent_signing_key,
// user_signing_key), and refuses missing or incomplete ring material
// without reading them. The store and backend are wrapped only to count
// the alias accesses.

const (
	p3HubID = "test-hub-id-p3"
	// p3StaleAliasSecret derives the retained aliases' key bytes: valid
	// keys that differ from every ring generation.
	p3StaleAliasSecret = "test-only-p3-stale-alias-secret"
)

var p3AliasNames = []string{SecretKeyAgentSigningKey, SecretKeyUserSigningKey}

func p3StaleKey(name string) []byte { return deriveSharedSigningKey(p3StaleAliasSecret, name) }

// aliasCounter counts every key-targeted access to the two JWT signing-key
// aliases, by key name.
type aliasCounter struct {
	mu     sync.Mutex
	reads  map[string]int
	writes map[string]int
	ops    []string
}

func newAliasCounter() *aliasCounter {
	return &aliasCounter{reads: map[string]int{}, writes: map[string]int{}}
}

func isAliasName(name string) bool {
	return name == SecretKeyAgentSigningKey || name == SecretKeyUserSigningKey
}

func (c *aliasCounter) read(op, name string) {
	if !isAliasName(name) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads[name]++
	c.ops = append(c.ops, "read:"+op+":"+name)
}

func (c *aliasCounter) write(op, name string) {
	if !isAliasName(name) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes[name]++
	c.ops = append(c.ops, "write:"+op+":"+name)
}

func (c *aliasCounter) totals() (reads, writes int, ops []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.reads {
		reads += n
	}
	for _, n := range c.writes {
		writes += n
	}
	return reads, writes, append([]string(nil), c.ops...)
}

func (c *aliasCounter) readsOf(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[name]
}

func (c *aliasCounter) writesOf(name string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes[name]
}

// jwtAliasCountingStore is the real store with alias accesses counted.
type jwtAliasCountingStore struct {
	store.Store
	c *aliasCounter
}

// DB and Dialect forward to the wrapped store, so New runs its
// migrations and startup checks against the real store.
func (s *jwtAliasCountingStore) DB() *sql.DB {
	if p, ok := s.Store.(interface{ DB() *sql.DB }); ok {
		return p.DB()
	}
	return nil
}

func (s *jwtAliasCountingStore) Dialect() string {
	if p, ok := s.Store.(interface{ Dialect() string }); ok {
		return p.Dialect()
	}
	return ""
}

func (s *jwtAliasCountingStore) GetSecret(ctx context.Context, key, scope, scopeID string) (*store.Secret, error) {
	s.c.read("store.GetSecret", key)
	return s.Store.GetSecret(ctx, key, scope, scopeID)
}

func (s *jwtAliasCountingStore) GetSecretValue(ctx context.Context, key, scope, scopeID string) (string, error) {
	s.c.read("store.GetSecretValue", key)
	return s.Store.GetSecretValue(ctx, key, scope, scopeID)
}

func (s *jwtAliasCountingStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	rows, err := s.Store.ListSecrets(ctx, filter)
	for _, row := range rows {
		s.c.read("store.ListSecrets", row.Key)
	}
	return rows, err
}

func (s *jwtAliasCountingStore) ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]store.Secret, error) {
	rows, err := s.Store.ListProgenySecrets(ctx, ancestorIDs)
	for _, row := range rows {
		s.c.read("store.ListProgenySecrets", row.Key)
	}
	return rows, err
}

func (s *jwtAliasCountingStore) CreateSecret(ctx context.Context, sec *store.Secret) error {
	s.c.write("store.CreateSecret", sec.Key)
	return s.Store.CreateSecret(ctx, sec)
}

func (s *jwtAliasCountingStore) UpdateSecret(ctx context.Context, sec *store.Secret) error {
	s.c.write("store.UpdateSecret", sec.Key)
	return s.Store.UpdateSecret(ctx, sec)
}

func (s *jwtAliasCountingStore) UpsertSecret(ctx context.Context, sec *store.Secret) (bool, error) {
	s.c.write("store.UpsertSecret", sec.Key)
	return s.Store.UpsertSecret(ctx, sec)
}

func (s *jwtAliasCountingStore) DeleteSecret(ctx context.Context, key, scope, scopeID string) error {
	s.c.write("store.DeleteSecret", key)
	return s.Store.DeleteSecret(ctx, key, scope, scopeID)
}

func (s *jwtAliasCountingStore) UpdateSecretValueIfVersion(ctx context.Context, key, scope, scopeID string, v int, val string) (bool, error) {
	s.c.write("store.UpdateSecretValueIfVersion", key)
	return s.Store.UpdateSecretValueIfVersion(ctx, key, scope, scopeID, v, val)
}

func (s *jwtAliasCountingStore) UpdateSecretRefIfMatches(ctx context.Context, key, scope, scopeID, expectedRef string, v int, newRef string) (bool, error) {
	s.c.write("store.UpdateSecretRefIfMatches", key)
	return s.Store.UpdateSecretRefIfMatches(ctx, key, scope, scopeID, expectedRef, v, newRef)
}

func (s *jwtAliasCountingStore) UpdateSecretMeta(ctx context.Context, key, scope, scopeID string, meta *store.SecretMetaUpdate) (*store.Secret, error) {
	s.c.write("store.UpdateSecretMeta", key)
	return s.Store.UpdateSecretMeta(ctx, key, scope, scopeID, meta)
}

// aliasCountingBackend is the real local secret backend with alias
// accesses counted.
type aliasCountingBackend struct {
	secret.SecretBackend
	c *aliasCounter
}

func (b *aliasCountingBackend) Get(ctx context.Context, name, scope, scopeID string) (*secret.SecretWithValue, error) {
	b.c.read("backend.Get", name)
	return b.SecretBackend.Get(ctx, name, scope, scopeID)
}

func (b *aliasCountingBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	b.c.read("backend.GetMeta", name)
	return b.SecretBackend.GetMeta(ctx, name, scope, scopeID)
}

func (b *aliasCountingBackend) FetchValues(ctx context.Context, metas []secret.SecretMeta) (map[string]secret.FetchResult, error) {
	for _, m := range metas {
		b.c.read("backend.FetchValues", m.Name)
	}
	return b.SecretBackend.FetchValues(ctx, metas)
}

func (b *aliasCountingBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	metas, err := b.SecretBackend.List(ctx, filter)
	for _, m := range metas {
		b.c.read("backend.List", m.Name)
	}
	return metas, err
}

func (b *aliasCountingBackend) Set(ctx context.Context, in *secret.SetSecretInput) (bool, *secret.SecretMeta, error) {
	b.c.write("backend.Set", in.Name)
	return b.SecretBackend.Set(ctx, in)
}

func (b *aliasCountingBackend) Delete(ctx context.Context, name, scope, scopeID string) error {
	b.c.write("backend.Delete", name)
	return b.SecretBackend.Delete(ctx, name, scope, scopeID)
}

func (b *aliasCountingBackend) UpdateMeta(ctx context.Context, in *secret.UpdateMetaInput) (*secret.SecretMeta, error) {
	b.c.write("backend.UpdateMeta", in.Name)
	return b.SecretBackend.UpdateMeta(ctx, in)
}

// aliasSnapshot is a retained alias row's identity and payload.
type aliasSnapshot struct {
	encryptedValue string
	version        int
	updated        time.Time
	secretRef      string
}

// p3Fixture is one real store and local backend shared by every replica,
// holding valid stale aliases for both JWT key names.
type p3Fixture struct {
	raw     store.Store
	st      *jwtAliasCountingStore
	backend *aliasCountingBackend
	c       *aliasCounter
	base    time.Time
	clk     *jkClock
}

func newP3Fixture(t *testing.T) *p3Fixture {
	t.Helper()
	raw, err := newTestStore(t, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	_ = raw.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")
	c := newAliasCounter()
	st := &jwtAliasCountingStore{Store: raw, c: c}
	base := time.Now().UTC().Truncate(time.Second)
	f := &p3Fixture{
		raw: raw, st: st, c: c, base: base, clk: &jkClock{t: base},
		backend: &aliasCountingBackend{SecretBackend: secret.NewLocalBackend(st, p3HubID, jkLegacySecret), c: c},
	}
	// Seed the retained aliases through an uncounted backend over the raw
	// store, encrypted exactly as a legacy-mode hub writes them.
	seed := secret.NewLocalBackend(raw, p3HubID, jkLegacySecret)
	for _, name := range p3AliasNames {
		_, _, err := seed.Set(context.Background(), &secret.SetSecretInput{
			Name: name, Value: base64.StdEncoding.EncodeToString(p3StaleKey(name)),
			SecretType: store.SecretTypeInternal, Scope: store.ScopeHub, ScopeID: p3HubID,
			Description: "Hub signing key for " + name,
		})
		require.NoError(t, err)
	}
	return f
}

func (f *p3Fixture) at(d time.Duration) *time.Time {
	t := f.base.Add(d)
	return &t
}

func (f *p3Fixture) snapshot(t *testing.T) map[string]aliasSnapshot {
	t.Helper()
	out := map[string]aliasSnapshot{}
	for _, name := range p3AliasNames {
		sec, err := f.raw.GetSecret(context.Background(), name, store.ScopeHub, p3HubID)
		require.NoError(t, err)
		out[name] = aliasSnapshot{encryptedValue: sec.EncryptedValue, version: sec.Version, updated: sec.Updated, secretRef: sec.SecretRef}
	}
	return out
}

// aliasValue decrypts a retained alias through an uncounted backend.
func (f *p3Fixture) aliasValue(t *testing.T, name string) []byte {
	t.Helper()
	sv, err := secret.NewLocalBackend(f.raw, p3HubID, jkLegacySecret).Get(context.Background(), name, store.ScopeHub, p3HubID)
	require.NoError(t, err)
	b, err := base64.StdEncoding.DecodeString(sv.Value)
	require.NoError(t, err)
	return b
}

// config is a replica's ServerConfig: the shared counting store and
// backend, the keyring, and the legacy entry as SharedSigningSecret (the
// other purposes still use it).
func (f *p3Fixture) config(k *keyring.Keyring, configured bool) ServerConfig {
	cfg := testServerConfig()
	cfg.HubID = p3HubID
	cfg.SecretBackend = f.backend
	cfg.Keyring = k
	cfg.KeyringConfigured = configured
	cfg.KeyringNow = f.clk.now
	if legacy, ok := k.Signing().LegacySecret(); ok {
		cfg.SharedSigningSecret = legacy
	}
	return cfg
}

func (f *p3Fixture) start(t *testing.T, cfg ServerConfig) (*Server, error) {
	t.Helper()
	srv, err := newTestHubServer(t, cfg, f.st)
	if err != nil {
		return nil, err
	}
	srv.SetHubID(p3HubID)
	waitUserScopedDataSweep(t, srv)
	return srv, nil
}

// allowVersionedJWTRings lets New accept emit versioned rings for the test
// (the config refuses them until phase 6) through the shared seam.
func allowVersionedJWTRings(t *testing.T) {
	t.Helper()
	prev := keyringReleaseCheck
	keyringReleaseCheck = func(k *keyring.Keyring) error {
		if k.Signing().Emit() == keyring.EmitVersioned {
			return nil
		}
		return prev(k)
	}
	t.Cleanup(func() { keyringReleaseCheck = prev })
}

// p3Rings is the rollout over the fixture's clock: A issues with legacy;
// B has activated gen2 and keeps legacy as the outgoing generation
// (issuance closed at base+1h, verification until base+3h). versioned
// selects B's emit; with emit legacy both replicas list gen2 as a legacy
// reader, since emit legacy requires the active ID among the legacy
// readers (rule owned by phase 4).
func (f *p3Fixture) p3Rings(t *testing.T, versioned bool) (a, b *keyring.Keyring) {
	t.Helper()
	legacyB := jkEntry{id: "legacy", secret: jkLegacySecret, signUntil: f.at(time.Hour), verifyUntl: f.at(3 * time.Hour)}
	if versioned {
		a = jkRing(t, "legacy", "legacy", []string{"legacy"},
			jkEntry{id: "legacy", secret: jkLegacySecret}, jkEntry{id: "gen2", secret: jkGen2Secret})
		b = jkRing(t, "versioned", "gen2", []string{"legacy"}, legacyB, jkEntry{id: "gen2", secret: jkGen2Secret})
		return a, b
	}
	a = jkRing(t, "legacy", "legacy", []string{"legacy", "gen2"},
		jkEntry{id: "legacy", secret: jkLegacySecret}, jkEntry{id: "gen2", secret: jkGen2Secret})
	b = jkRing(t, "legacy", "gen2", []string{"legacy", "gen2"}, legacyB, jkEntry{id: "gen2", secret: jkGen2Secret})
	return a, b
}

// requireNoAliasAccess fails when any alias read or write was counted.
func requireNoAliasAccess(t *testing.T, f *p3Fixture) {
	t.Helper()
	reads, writes, ops := f.c.totals()
	require.Zero(t, reads, "retained-alias reads: %v", ops)
	require.Zero(t, writes, "retained-alias writes: %v", ops)
}

// TestP3_AliasHarnessSeesLegacyFallback is the harness self-test: a
// legacy-mode hub over the same fixture (no shared secret) resolves its
// JWT keys from the retained aliases, and the counters see those reads.
// Zero counts in the managed-mode tests therefore mean no fallback, not a
// blind harness.
func TestP3_AliasHarnessSeesLegacyFallback(t *testing.T) {
	f := newP3Fixture(t)
	cfg := testServerConfig()
	cfg.HubID = p3HubID
	cfg.SecretBackend = f.backend
	srv, err := f.start(t, cfg)
	require.NoError(t, err)
	for _, name := range p3AliasNames {
		assert.Positive(t, f.c.readsOf(name), name)
	}
	assert.Equal(t, p3StaleKey(SecretKeyAgentSigningKey), srv.agentTokenService.config.SigningKey)
	assert.Equal(t, p3StaleKey(SecretKeyUserSigningKey), srv.userTokenService.config.SigningKey)
}

// TestP3_ManagedStartupRefusesWithoutAliasReads: with a valid stale alias
// present, missing or incomplete managed ring material refuses New (so
// nothing serves: no Server, no handler, no readiness) with zero alias
// reads or writes, never silently selecting legacy mode; errors and logs
// name no key material.
func TestP3_ManagedStartupRefusesWithoutAliasReads(t *testing.T) {
	noLegacy := jkRing(t, "legacy", "gen2", []string{"gen2"}, jkEntry{id: "gen2", secret: jkGen2Secret})
	good := jkRing(t, "legacy", "legacy", []string{"legacy"}, jkEntry{id: "legacy", secret: jkLegacySecret})
	cases := []struct {
		name string
		cfg  func(f *p3Fixture) ServerConfig
		// noopSeam swaps keyringReleaseCheck for a no-op: the refusal
		// must not depend on the test seam.
		noopSeam bool
	}{
		// (a) ring file missing or unreadable: cmd built no keyring.
		{"file missing or unreadable", func(f *p3Fixture) ServerConfig { return f.config(nil, true) }, false},
		// (a') a legacy adapter in place of the configured file.
		{"legacy adapter instead of the file", func(f *p3Fixture) ServerConfig {
			cfg := f.config(keyring.Legacy(jkLegacySecret), true)
			cfg.SharedSigningSecret = jkLegacySecret
			return cfg
		}, false},
		// (b) active generation absent.
		{"active generation absent (configured)", func(f *p3Fixture) ServerConfig { return f.config(&keyring.Keyring{}, true) }, false},
		{"active generation absent (keyring only)", func(f *p3Fixture) ServerConfig { return f.config(&keyring.Keyring{}, false) }, false},
		// (c) incomplete required signing material.
		{"no legacy entry", func(f *p3Fixture) ServerConfig { return f.config(noLegacy, true) }, false},
		{"no legacy entry, release check swapped out", func(f *p3Fixture) ServerConfig { return f.config(noLegacy, true) }, true},
		{"shared secret not the legacy entry", func(f *p3Fixture) ServerConfig {
			cfg := f.config(good, true)
			cfg.SharedSigningSecret = p3StaleAliasSecret
			return cfg
		}, false},
		// Explicit keys never combine with managed mode.
		{"explicit agent key", func(f *p3Fixture) ServerConfig {
			cfg := f.config(good, true)
			cfg.AgentTokenConfig.SigningKey = p3StaleKey(SecretKeyAgentSigningKey)
			return cfg
		}, false},
		{"explicit user key", func(f *p3Fixture) ServerConfig {
			cfg := f.config(good, true)
			cfg.UserTokenConfig.SigningKey = p3StaleKey(SecretKeyUserSigningKey)
			return cfg
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.noopSeam {
				prev := keyringReleaseCheck
				keyringReleaseCheck = func(*keyring.Keyring) error { return nil }
				t.Cleanup(func() { keyringReleaseCheck = prev })
			}
			f := newP3Fixture(t)
			before := f.snapshot(t)
			logs := captureLogs(t)
			srv, err := f.start(t, tc.cfg(f))
			require.Error(t, err)
			require.Nil(t, srv)
			if tc.noopSeam {
				assert.Contains(t, err.Error(), `no "legacy" entry`)
			}
			requireNoAliasAccess(t, f)
			assert.Equal(t, before, f.snapshot(t))
			assertNoJWTKeyMaterial(t, err.Error())
			assertNoJWTKeyMaterial(t, logs.String())
			for _, name := range p3AliasNames {
				assert.NotContains(t, err.Error()+logs.String(), base64.StdEncoding.EncodeToString(p3StaleKey(name)))
			}
		})
	}
}

// TestP3_ManagedStartupDoesNotTouchAliases: two managed startup
// generations (A: legacy active; B: gen2 active, versioned) alternating
// over one store and backend never read, publish, rewrite or delete the
// retained aliases, so they cannot oscillate; both sign and verify from
// the ring.
func TestP3_ManagedStartupDoesNotTouchAliases(t *testing.T) {
	allowVersionedJWTRings(t)
	f := newP3Fixture(t)
	a, b := f.p3Rings(t, true)
	before := f.snapshot(t)
	for i, k := range []*keyring.Keyring{a, b, a, b} {
		srv, err := f.start(t, f.config(k, true))
		require.NoError(t, err, "start %d", i)
		assert.Equal(t, SigningKeySourceKeyring, srv.signingKeySource(SecretKeyAgentSigningKey))
		assert.Equal(t, SigningKeySourceKeyring, srv.signingKeySource(SecretKeyUserSigningKey))
		assert.Empty(t, srv.agentTokenService.config.SigningKey)
		assert.Empty(t, srv.userTokenService.config.SigningKey)
		tok, _, _, err := srv.userTokenService.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeWeb)
		require.NoError(t, err)
		_, err = srv.userTokenService.ValidateUserToken(tok)
		require.NoError(t, err)
		require.NoError(t, srv.Shutdown(context.Background()))
		requireNoAliasAccess(t, f)
		assert.Equal(t, before, f.snapshot(t), "start %d", i)
	}
	for _, name := range p3AliasNames {
		assert.Equal(t, p3StaleKey(name), f.aliasValue(t, name), "alias bytes kept")
	}
}

// TestP3_AliasBytesNeverBecomeSigningMaterial: a retained alias holding
// different valid key bytes never verifies a token minted in managed mode,
// and a token signed with the alias bytes never verifies there.
func TestP3_AliasBytesNeverBecomeSigningMaterial(t *testing.T) {
	f := newP3Fixture(t)
	a, _ := f.p3Rings(t, false)
	srv, err := f.start(t, f.config(a, true))
	require.NoError(t, err)

	agentTok, _, err := srv.agentTokenService.SignAgentToken(AgentTokenGrant{AgentID: "agent-1"}, "")
	require.NoError(t, err)
	userTok, _, _, err := srv.userTokenService.GenerateTokenPair("u1", "u1@example.com", "U", "member", ClientTypeWeb)
	require.NoError(t, err)
	for tok, name := range map[string]string{agentTok: SecretKeyAgentSigningKey, userTok: SecretKeyUserSigningKey} {
		parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.HS256})
		require.NoError(t, err)
		var c jwt.Claims
		assert.Error(t, parsed.Claims(p3StaleKey(name), &c), "alias bytes verify a managed token")
		assert.NoError(t, parsed.Claims(deriveSharedSigningKey(jkLegacySecret, name), &c))
	}
	now := f.clk.now()
	for _, kid := range []string{"", "legacy", "gen2"} {
		forged := signRaw(t, jose.HS256, p3StaleKey(SecretKeyAgentSigningKey), kid, jkAgentClaims(now, time.Hour))
		_, err := srv.agentTokenService.ValidateAgentToken(forged)
		assert.ErrorIs(t, err, errJWTNoEligibleKey, "kid %q", kid)
		forgedUser := signRaw(t, jose.HS256, p3StaleKey(SecretKeyUserSigningKey), kid, UserTokenClaims{Claims: jwt.Claims{
			Issuer: UserTokenIssuer, Subject: "u1", Audience: jwt.Audience{UserTokenAudience},
			IssuedAt: jwt.NewNumericDate(now), Expiry: jwt.NewNumericDate(now.Add(time.Hour)),
		}, UserID: "u1", TokenType: TokenTypeAccess})
		_, err = srv.userTokenService.ValidateUserToken(forgedUser)
		assert.ErrorIs(t, err, errJWTNoEligibleKey, "kid %q", kid)
	}
	requireNoAliasAccess(t, f)
}

// TestP3_LegacySyncUnchanged: legacy mode keeps its shared-key
// synchronization: the derived keys are published through the backend
// (overwriting the retained alias, exactly as before the keyring).
func TestP3_LegacySyncUnchanged(t *testing.T) {
	f := newP3Fixture(t)
	cfg := testServerConfig()
	cfg.HubID = p3HubID
	cfg.SecretBackend = f.backend
	cfg.SharedSigningSecret = jkLegacySecret
	srv, err := f.start(t, cfg)
	require.NoError(t, err)
	for _, name := range p3AliasNames {
		assert.Positive(t, f.c.writesOf(name), name)
		assert.Equal(t, deriveSharedSigningKey(jkLegacySecret, name), f.aliasValue(t, name), name)
	}
	assert.Equal(t, deriveSharedSigningKey(jkLegacySecret, SecretKeyAgentSigningKey), srv.agentTokenService.config.SigningKey)
	assert.Equal(t, signingKeySourceSharedSecret, srv.signingKeySource(SecretKeyAgentSigningKey))
	assert.Nil(t, srv.agentTokenService.ring)
}

// TestP3_ExplicitKeyPrecedenceInLegacyMode: an explicit agent key still
// wins over SharedSigningSecret and the aliases in legacy mode, without
// reading or writing the agent alias.
func TestP3_ExplicitKeyPrecedenceInLegacyMode(t *testing.T) {
	f := newP3Fixture(t)
	explicit := []byte("test-only-p3-explicit-agent-key-0123456789")
	cfg := testServerConfig()
	cfg.HubID = p3HubID
	cfg.SecretBackend = f.backend
	cfg.SharedSigningSecret = jkLegacySecret
	cfg.AgentTokenConfig.SigningKey = explicit
	srv, err := f.start(t, cfg)
	require.NoError(t, err)
	assert.Equal(t, explicit, srv.agentTokenService.config.SigningKey)
	assert.Equal(t, signingKeySourceConfig, srv.signingKeySource(SecretKeyAgentSigningKey))
	assert.Zero(t, f.c.readsOf(SecretKeyAgentSigningKey))
	assert.Zero(t, f.c.writesOf(SecretKeyAgentSigningKey))
}

// p3Do serves one request on srv's full handler chain.
func p3Do(t *testing.T, srv *Server, method, path string, body any, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// p3AgentAuth authenticates one request through the hub's own
// UnifiedAuthMiddleware and auth configuration (as Server.Handler chains
// it), in front of a probe that answers 200 with the authenticated agent
// ID, so acceptance is asserted exactly (status and identity), not as
// "not 401".
func p3AgentAuth(t *testing.T, srv *Server, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	h := UnifiedAuthMiddleware(srv.authConfig)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := "no-agent-identity"
		if a := GetAgentIdentityFromContext(r.Context()); a != nil {
			id = a.ID()
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(id))
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/p3-probe", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// p3ConduitRefresh runs the conduit relay's in-band agent credential
// refresh check on srv for the agent's session.
func p3ConduitRefresh(srv *Server, agentID, projectID, tok string) string {
	outcome, _ := srv.conduitAgentRefreshCheck(context.Background(),
		relay.Principal{Kind: registry.PrincipalAgent, ID: agentID, ProjectID: projectID},
		relay.RefreshSession{SessionID: "p3-session"}, &conduitv1.AuthRefresh{Credential: []byte(tok)})
	return outcome
}

// verifiesOnlyUnder reports which of the fixture generations verifies tok
// for the named purpose.
func verifiesOnlyUnder(t *testing.T, tok, name string) []string {
	t.Helper()
	parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.HS256})
	require.NoError(t, err)
	var out []string
	for id, s := range map[string]string{"legacy": jkLegacySecret, "gen2": jkGen2Secret} {
		var c jwt.Claims
		if parsed.Claims(deriveSharedSigningKey(s, name), &c) == nil {
			out = append(out, id)
		}
	}
	return out
}

// TestP3_CrossReplicaTokensThroughHandlers runs the rollout through two
// real replicas, for emit legacy and emit versioned on the new-active
// replica: old and new agent, user access, refresh, CLI and test-login
// JWTs validate on both replicas during grace; refresh with an old token
// returns a new-generation token; a revoked credential holding an
// otherwise valid previous-generation JWT is refused; after the outgoing
// generation's verify_until its tokens are refused.
func TestP3_CrossReplicaTokensThroughHandlers(t *testing.T) {
	for _, versioned := range []bool{false, true} {
		name := "emit legacy"
		if versioned {
			name = "emit versioned"
		}
		t.Run(name, func(t *testing.T) {
			if versioned {
				allowVersionedJWTRings(t)
			}
			f := newP3Fixture(t)
			ka, kb := f.p3Rings(t, versioned)
			srvA, err := f.start(t, f.config(ka, true))
			require.NoError(t, err)
			srvB, err := f.start(t, f.config(kb, true))
			require.NoError(t, err)
			_, _, user, project := setupCredentialTestServerOn(t, srvA, f.raw)
			agentID := tid("agent-p3-rollout")
			createCredTestAgent(t, f.raw, agentID, project.ID, user.ID)

			// Agent tokens from each replica authenticate on the other.
			oldAgent, err := srvA.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			newAgent, err := srvB.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			assert.Equal(t, []string{"legacy"}, verifiesOnlyUnder(t, oldAgent, SecretKeyAgentSigningKey))
			assert.Equal(t, []string{"gen2"}, verifiesOnlyUnder(t, newAgent, SecretKeyAgentSigningKey))
			if versioned {
				assert.Equal(t, "gen2", jwtHeader(t, newAgent)["kid"])
			} else {
				assert.NotContains(t, jwtHeader(t, newAgent), "kid")
			}
			for _, tc := range []struct {
				srv *Server
				tok string
			}{{srvB, oldAgent}, {srvA, newAgent}, {srvA, oldAgent}, {srvB, newAgent}} {
				rec := p3AgentAuth(t, tc.srv, map[string]string{"X-Scion-Agent-Token": tc.tok})
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, agentID, rec.Body.String())
				// The agent token wins over a federation header (auth.go
				// step 1 before 1.5), for either generation.
				rec = p3AgentAuth(t, tc.srv, map[string]string{"X-Scion-Agent-Token": tc.tok, FederationTokenHeader: "not-a-federation-token"})
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				assert.Equal(t, agentID, rec.Body.String())
				// Conduit in-band agent refresh accepts it on either replica.
				assert.Equal(t, conduitRefreshOK, p3ConduitRefresh(tc.srv, agentID, project.ID, tc.tok))
			}
			// An unknown generation is refused on every agent path, and the
			// federation header is never tried instead.
			c := jkRing(t, "versioned", "gen3", []string{"legacy"},
				jkEntry{id: "legacy", secret: jkLegacySecret}, jkEntry{id: "gen3", secret: jkGen3Secret})
			unknownAgent, _, err := jkAgentSvc(t, c, f.clk).SignAgentToken(AgentTokenGrant{AgentID: agentID, ProjectID: project.ID, Scopes: ScopesForRole(AgentRoleFull)}, "")
			require.NoError(t, err)
			for _, srv := range []*Server{srvA, srvB} {
				rec := p3AgentAuth(t, srv, map[string]string{"X-Scion-Agent-Token": unknownAgent, FederationTokenHeader: "not-a-federation-token"})
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
				assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
				assert.Equal(t, conduitRefreshRefused, p3ConduitRefresh(srv, agentID, project.ID, unknownAgent))
			}
			// A token for the conduit retirement check below (its own credential).
			conduitOld, err := srvA.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
			require.NoError(t, err)

			// User access, CLI, refresh and test-login tokens.
			oldAccess, oldRefresh, _, err := srvA.userTokenService.GenerateTokenPair(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb)
			require.NoError(t, err)
			oldCLI, _, err := srvA.userTokenService.GenerateAccessToken(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeCLI)
			require.NoError(t, err)
			newCLI, _, err := srvB.userTokenService.GenerateAccessToken(user.ID, user.Email, user.DisplayName, user.Role, ClientTypeCLI)
			require.NoError(t, err)
			oldTL, err := srvA.userTokenService.GenerateTestLoginToken(user.ID)
			require.NoError(t, err)
			newTL, err := srvB.userTokenService.GenerateTestLoginToken(user.ID)
			require.NoError(t, err)
			for _, tc := range []struct {
				srv *Server
				tok string
			}{{srvB, oldAccess}, {srvB, oldCLI}, {srvA, newCLI}, {srvA, oldCLI}} {
				rec := p3Do(t, tc.srv, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: tc.tok}, nil)
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				var resp AuthValidateResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				assert.True(t, resp.Valid)
				_, err := tc.srv.userTokenService.GetTokenExpiry(tc.tok)
				assert.NoError(t, err)
			}
			assert.NoError(t, srvB.userTokenService.ValidateTestLoginToken(oldTL))
			assert.NoError(t, srvA.userTokenService.ValidateTestLoginToken(newTL))

			// User refresh with the old refresh token on B: a new-generation pair.
			rec := p3Do(t, srvB, http.MethodPost, "/api/v1/auth/refresh", AuthRefreshRequest{RefreshToken: oldRefresh}, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var rr AuthRefreshResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rr))
			assert.Equal(t, []string{"gen2"}, verifiesOnlyUnder(t, rr.AccessToken, SecretKeyUserSigningKey))
			assert.Equal(t, []string{"gen2"}, verifiesOnlyUnder(t, rr.RefreshToken, SecretKeyUserSigningKey))

			// Agent refresh with the old token on B: a new-generation token,
			// and the old credential is revoked as before.
			oldClaims, err := srvB.agentTokenService.ValidateAgentToken(oldAgent)
			require.NoError(t, err)
			rec = p3Do(t, srvB, http.MethodPost, "/api/v1/agents/"+agentID+"/token/refresh", nil, map[string]string{"X-Scion-Agent-Token": oldAgent})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var ar struct {
				Token string `json:"token"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ar))
			assert.Equal(t, []string{"gen2"}, verifiesOnlyUnder(t, ar.Token, SecretKeyAgentSigningKey))
			oldCred, err := f.raw.GetAgentCredentialByJTIHash(context.Background(), hashJTI(oldClaims.ID))
			require.NoError(t, err)
			assert.NotNil(t, oldCred.RevokedAt)
			// The refreshed (revoked) old token is now refused on both replicas.
			for _, srv := range []*Server{srvA, srvB} {
				rec := p3AgentAuth(t, srv, map[string]string{"X-Scion-Agent-Token": oldAgent})
				assert.Equal(t, http.StatusUnauthorized, rec.Code)
				assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
			}

			// A revoked credential with an otherwise valid previous-generation JWT.
			revoked, err := srvA.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			rc, err := srvB.agentTokenService.ValidateAgentToken(revoked)
			require.NoError(t, err, "cryptographically valid on B")
			cred, err := f.raw.GetAgentCredentialByJTIHash(context.Background(), hashJTI(rc.ID))
			require.NoError(t, err)
			require.NoError(t, f.raw.RevokeAgentCredential(context.Background(), cred.ID, "test", "explicit"))
			rec = p3AgentAuth(t, srvB, map[string]string{"X-Scion-Agent-Token": revoked})
			assert.Equal(t, http.StatusUnauthorized, rec.Code)
			assert.Equal(t, agentTokenRefusedBody, rec.Body.String())
			rec = p3Do(t, srvB, http.MethodPost, "/api/v1/agents/"+agentID+"/token/refresh", nil, map[string]string{"X-Scion-Agent-Token": revoked})
			assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())

			// After the outgoing generation's verify_until (base+3h), B
			// refuses old-generation tokens still within their exp; A (no
			// deadline on legacy) still accepts them.
			lateOld, err := srvA.GenerateAgentToken(agentID, project.ID, nil, AgentRoleFull, nil)
			require.NoError(t, err)
			f.clk.t = f.base.Add(3 * time.Hour)
			recB := p3AgentAuth(t, srvB, map[string]string{"X-Scion-Agent-Token": lateOld})
			assert.Equal(t, http.StatusUnauthorized, recB.Code)
			assert.Equal(t, agentTokenRefusedBody, recB.Body.String())
			assert.Equal(t, conduitRefreshRefused, p3ConduitRefresh(srvB, agentID, project.ID, conduitOld))
			rec = p3AgentAuth(t, srvA, map[string]string{"X-Scion-Agent-Token": lateOld})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Equal(t, agentID, rec.Body.String())
			assert.Equal(t, conduitRefreshOK, p3ConduitRefresh(srvA, agentID, project.ID, conduitOld))
			recV := p3Do(t, srvB, http.MethodPost, "/api/v1/auth/validate", AuthValidateRequest{Token: oldCLI}, nil)
			var resp AuthValidateResponse
			require.NoError(t, json.Unmarshal(recV.Body.Bytes(), &resp))
			assert.False(t, resp.Valid)
			_, err = srvB.userTokenService.GetTokenExpiry(oldCLI)
			assert.ErrorIs(t, err, errJWTNoEligibleKey)

			requireNoAliasAccess(t, f)
			assertNoJWTKeyMaterial(t, recB.Body.String()+recV.Body.String())
		})
	}
}
