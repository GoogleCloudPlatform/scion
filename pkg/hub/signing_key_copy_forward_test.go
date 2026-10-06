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
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sync"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// copyForwardMockSMClient is a minimal secret.SMClient fake for exercising
// the hub-startup signing-key copy-forward (ensureSigningKey,
// OIDCKeyManager.loadOrCreateKey) against a real *secret.GCPBackend, without
// any GCP network calls — per the ptone/scion#2152 constraint that tests run
// only against fakes/mocks.
type copyForwardMockSMClient struct {
	mu       sync.Mutex
	secrets  map[string]*smpb.Secret
	versions map[string][]byte
}

func newCopyForwardMockSMClient() *copyForwardMockSMClient {
	return &copyForwardMockSMClient{
		secrets:  make(map[string]*smpb.Secret),
		versions: make(map[string][]byte),
	}
}

func (m *copyForwardMockSMClient) CreateSecret(_ context.Context, req *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fullName := fmt.Sprintf("%s/secrets/%s", req.Parent, req.SecretId)
	if _, exists := m.secrets[fullName]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "secret %s already exists", fullName)
	}
	sec := &smpb.Secret{Name: fullName, Labels: req.Secret.GetLabels()}
	m.secrets[fullName] = sec
	return sec, nil
}

func (m *copyForwardMockSMClient) AddSecretVersion(_ context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.secrets[req.Parent]; !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Parent)
	}
	m.versions[req.Parent] = req.Payload.Data
	return &smpb.SecretVersion{Name: req.Parent + "/versions/1"}, nil
}

func (m *copyForwardMockSMClient) AccessSecretVersion(_ context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := req.Name
	for _, suffix := range []string{"/versions/latest", "/versions/1"} {
		if len(name) > len(suffix) && name[len(name)-len(suffix):] == suffix {
			name = name[:len(name)-len(suffix)]
			break
		}
	}
	data, exists := m.versions[name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "version not found for %s", req.Name)
	}
	return &smpb.AccessSecretVersionResponse{Name: req.Name, Payload: &smpb.SecretPayload{Data: data}}, nil
}

func (m *copyForwardMockSMClient) DeleteSecret(_ context.Context, req *smpb.DeleteSecretRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.secrets[req.Name]; !exists {
		return status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}
	delete(m.secrets, req.Name)
	delete(m.versions, req.Name)
	return nil
}

func (m *copyForwardMockSMClient) GetSecret(_ context.Context, req *smpb.GetSecretRequest) (*smpb.Secret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sec, exists := m.secrets[req.Name]
	if !exists {
		return nil, status.Errorf(codes.NotFound, "secret %s not found", req.Name)
	}
	return sec, nil
}

func (m *copyForwardMockSMClient) Close() error { return nil }

func (m *copyForwardMockSMClient) seed(t *testing.T, projectID, smName, value string) {
	t.Helper()
	ctx := context.Background()
	fullName := fmt.Sprintf("projects/%s/secrets/%s", projectID, smName)
	if _, err := m.CreateSecret(ctx, &smpb.CreateSecretRequest{
		Parent: fmt.Sprintf("projects/%s", projectID), SecretId: smName, Secret: &smpb.Secret{},
	}); err != nil {
		t.Fatalf("failed to seed mock secret %s: %v", smName, err)
	}
	if _, err := m.AddSecretVersion(ctx, &smpb.AddSecretVersionRequest{
		Parent: fullName, Payload: &smpb.SecretPayload{Data: []byte(value)},
	}); err != nil {
		t.Fatalf("failed to seed mock secret version %s: %v", smName, err)
	}
}

// legacyGCPSecretNameForTest recomputes GCPBackend's pre-ptone/scion#2152
// legacy naming formula for test setup. pkg/hub can't reach GCPBackend's
// unexported naming methods directly (different package); this mirrors the
// formula independently verified by pkg/secret's own golden-vector test.
func legacyGCPSecretNameForTest(hubID, scope, scopeID, name string) string {
	return fmt.Sprintf("scion-%s-%s-%s", scope, hash12ForHubTest(hubID+":"+scopeID), name)
}

func prefixedGCPSecretNameForTest(hubID, scope, scopeID, name string) string {
	return fmt.Sprintf("scion-%s-%s-%s-%s", hash12ForHubTest(hubID), scope, hash12ForHubTest(hubID+":"+scopeID), name)
}

// TestEnsureSigningKey_CopiesLegacyKeyForwardAndRepairsRef reproduces
// ptone/scion#2152 review findings 3 and 7: a signing key created before
// this feature only exists under the legacy (pre hub-prefix) GCP SM name,
// with a DB record whose SecretRef still points there. ensureSigningKey must
// return the same key material, and afterward the DB ref must have moved to
// the hub-prefixed name — not just have a prefixed copy sitting unused in
// GCP SM while every read keeps going through the stale ref.
func TestEnsureSigningKey_CopiesLegacyKeyForwardAndRepairsRef(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("failed to migrate test store: %v", err)
	}

	hubID := "test-hub-copy-forward"
	projectID := "test-project"
	keyName := SecretKeyUserSigningKey
	mock := newCopyForwardMockSMClient()
	backend := secret.NewGCPBackendWithClient(st, mock, projectID, hubID)

	keyMaterial := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	legacyName := legacyGCPSecretNameForTest(hubID, store.ScopeHub, hubID, keyName)
	mock.seed(t, projectID, legacyName, keyMaterial)
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", projectID, legacyName)
	if err := st.CreateSecret(ctx, &store.Secret{
		ID:         tid("legacy-user-signing-key"),
		Key:        keyName,
		Scope:      store.ScopeHub,
		ScopeID:    hubID,
		SecretRef:  legacyRef,
		SecretType: store.SecretTypeInternal,
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	s := &Server{
		hubID:         hubID,
		store:         st,
		secretBackend: backend,
		config:        ServerConfig{},
	}

	key, err := s.ensureSigningKey(ctx, keyName, nil)
	if err != nil {
		t.Fatalf("ensureSigningKey failed: %v", err)
	}
	wantKey, _ := base64.StdEncoding.DecodeString(keyMaterial)
	if string(key) != string(wantKey) {
		t.Errorf("expected the legacy key material to be returned, got different bytes")
	}

	rec, err := st.GetSecret(ctx, keyName, store.ScopeHub, hubID)
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	prefixedName := prefixedGCPSecretNameForTest(hubID, store.ScopeHub, hubID, keyName)
	prefixedRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", projectID, prefixedName)
	if rec.SecretRef != prefixedRef {
		t.Errorf("expected SecretRef repaired to %q, got %q", prefixedRef, rec.SecretRef)
	}

	// Legacy secret must remain — deletion is migrate-names --delete-legacy's job.
	mock.mu.Lock()
	_, legacyStillThere := mock.secrets[fmt.Sprintf("projects/%s/secrets/%s", projectID, legacyName)]
	mock.mu.Unlock()
	if !legacyStillThere {
		t.Error("expected legacy signing key secret to remain")
	}
}

// TestOIDCKeyManager_CopiesLegacyKeyForwardAndRepairsRef is the OIDC
// equivalent of TestEnsureSigningKey_CopiesLegacyKeyForwardAndRepairsRef
// (ptone/scion#2152 review finding 5: .design/secret-id-hub-refactor.md §7
// names oidc_signing_key explicitly, but it has its own load path — separate
// from ensureSigningKey — that originally had no copy-forward at all).
func TestOIDCKeyManager_CopiesLegacyKeyForwardAndRepairsRef(t *testing.T) {
	st := createOIDCTestStore(t)
	ctx := context.Background()

	hubID := "test-hub-oidc-copy-forward"
	projectID := "test-project"
	keyName := SecretKeyOIDCSigningKey
	mock := newCopyForwardMockSMClient()
	backend := secret.NewGCPBackendWithClient(st, mock, projectID, hubID)

	// Seed a real PEM-encoded RSA key via the legacy name, matching what an
	// existing pre-ptone/scion#2152 deployment would have.
	priv, err := generateRSAKeyPair()
	if err != nil {
		t.Fatalf("generateRSAKeyPair: %v", err)
	}
	pemBytes, err := encodePEMPrivateKey(priv)
	if err != nil {
		t.Fatalf("encodePEMPrivateKey: %v", err)
	}

	legacyName := legacyGCPSecretNameForTest(hubID, store.ScopeHub, hubID, keyName)
	mock.seed(t, projectID, legacyName, string(pemBytes))
	legacyRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", projectID, legacyName)
	if err := st.CreateSecret(ctx, &store.Secret{
		ID:         tid("legacy-oidc-signing-key"),
		Key:        keyName,
		Scope:      store.ScopeHub,
		ScopeID:    hubID,
		SecretRef:  legacyRef,
		SecretType: store.SecretTypeInternal,
	}); err != nil {
		t.Fatalf("failed to seed DB record: %v", err)
	}

	mgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
		Store:     st,
		Backend:   backend,
		HubID:     hubID,
		IssuerURL: "https://hub.example.com",
	})
	if err != nil {
		t.Fatalf("NewOIDCKeyManager failed: %v", err)
	}
	keys := mgr.JWKS().Keys
	if len(keys) == 0 {
		t.Fatal("expected at least one JWKS key")
	}

	// ptone/scion#2152 round-2 review finding 4: assert the loaded key is
	// byte-identical to the seeded legacy one, not just "a key exists". A
	// broken copy-forward that fell through to generating a brand-new key
	// would still satisfy every assertion below this point (Set() writes the
	// prefixed name and ref, and the legacy copy is left untouched either
	// way) — only a modulus/value comparison against the original key
	// catches that, which is the whole point of "sessions and agent tokens
	// survive" for the OIDC-signed tokens this key protects.
	pub, ok := keys[0].Key.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("expected an RSA public key in the JWKS, got %T", keys[0].Key)
	}
	if pub.N.Cmp(priv.N) != 0 || pub.E != priv.E {
		t.Error("BUG: loaded OIDC key's public modulus/exponent does not match the seeded legacy key — a new key may have been generated instead of loading the legacy one")
	}

	mock.mu.Lock()
	prefixedValueAtCopy := string(mock.versions[fmt.Sprintf("projects/%s/secrets/%s", projectID, prefixedGCPSecretNameForTest(hubID, store.ScopeHub, hubID, keyName))])
	mock.mu.Unlock()
	if prefixedValueAtCopy != string(pemBytes) {
		t.Errorf("BUG: prefixed copy's PEM value does not match the seeded legacy PEM byte-for-byte")
	}

	rec, err := st.GetSecret(ctx, keyName, store.ScopeHub, hubID)
	if err != nil {
		t.Fatalf("GetSecret failed: %v", err)
	}
	prefixedName := prefixedGCPSecretNameForTest(hubID, store.ScopeHub, hubID, keyName)
	prefixedRef := "gcpsm:" + fmt.Sprintf("projects/%s/secrets/%s", projectID, prefixedName)
	if rec.SecretRef != prefixedRef {
		t.Errorf("expected SecretRef repaired to %q, got %q", prefixedRef, rec.SecretRef)
	}

	mock.mu.Lock()
	_, legacyStillThere := mock.secrets[fmt.Sprintf("projects/%s/secrets/%s", projectID, legacyName)]
	mock.mu.Unlock()
	if !legacyStillThere {
		t.Error("expected legacy OIDC signing key secret to remain")
	}
}

// hash12ForHubTest mirrors GCPBackend's internal 12-hex-char naming hash
// (first 6 bytes of sha256(...), hex-encoded = 12 hex chars) for test setup.
func hash12ForHubTest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}
