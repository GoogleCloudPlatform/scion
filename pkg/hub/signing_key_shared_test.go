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
	"strings"
	"testing"
)

// TestEnsureSigningKey_RequireStableRefusesGeneration verifies that, with
// RequireStableSigningKey set and no existing key resolvable, ensureSigningKey
// fails fast instead of silently minting a new key (which would invalidate every
// live token). This is the regression guard for the hub-restart auth deadlock.
func TestEnsureSigningKey_RequireStableRefusesGeneration(t *testing.T) {
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	defer st.Close()

	s := &Server{
		hubID:  "host-with-no-key",
		store:  st,
		config: ServerConfig{RequireStableSigningKey: true},
	}

	_, err = s.ensureSigningKey(context.Background(), SecretKeyAgentSigningKey, nil)
	if err == nil {
		t.Fatal("expected ensureSigningKey to refuse generating a new key when RequireStableSigningKey is set")
	}
	if !strings.Contains(err.Error(), "RequireStableSigningKey") {
		t.Fatalf("error should explain the refusal, got: %v", err)
	}
}

// TestEnsureSigningKey_RequireStableAllowsSharedSecret verifies that stable-key
// enforcement still works when the operator supplies a SharedSigningSecret: the
// key is derived deterministically and no generation (or store access) occurs.
func TestEnsureSigningKey_RequireStableAllowsSharedSecret(t *testing.T) {
	// Nil store is fine: shared-secret derivation returns before any store access.
	s := &Server{
		hubID:  "host1",
		config: ServerConfig{RequireStableSigningKey: true, SharedSigningSecret: "deployment-secret"},
	}

	key, err := s.ensureSigningKey(context.Background(), SecretKeyAgentSigningKey, nil)
	if err != nil {
		t.Fatalf("ensureSigningKey with shared secret should succeed under require-stable, got: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected a 32-byte derived key, got %d bytes", len(key))
	}
	if !bytes.Equal(key, deriveSharedSigningKey("deployment-secret", SecretKeyAgentSigningKey)) {
		t.Fatal("require-stable should derive the same key as deriveSharedSigningKey")
	}
}

// TestEnsureSigningKey_GeneratesWhenNotRequired verifies the default behavior is
// preserved: without RequireStableSigningKey, a missing key is generated and
// persisted rather than erroring.
func TestEnsureSigningKey_GeneratesWhenNotRequired(t *testing.T) {
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	defer st.Close()

	s := &Server{hubID: "host1", store: st, config: ServerConfig{}}

	key, err := s.ensureSigningKey(context.Background(), SecretKeyAgentSigningKey, nil)
	if err != nil {
		t.Fatalf("ensureSigningKey should generate a key by default, got: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("expected a 32-byte generated key, got %d bytes", len(key))
	}

	// The generated key is persisted, so a second resolve returns the same key.
	key2, err := s.ensureSigningKey(context.Background(), SecretKeyAgentSigningKey, nil)
	if err != nil {
		t.Fatalf("second ensureSigningKey: %v", err)
	}
	if !bytes.Equal(key, key2) {
		t.Fatal("a generated key must persist and be returned on subsequent resolves")
	}
}

// TestDeriveSharedSigningKey_Deterministic verifies that the derivation is
// stable for a given (secret, keyName) pair and domain-separated across key
// names and secrets.
func TestDeriveSharedSigningKey_Deterministic(t *testing.T) {
	const secret = "shared-deployment-secret"

	userA := deriveSharedSigningKey(secret, SecretKeyUserSigningKey)
	userB := deriveSharedSigningKey(secret, SecretKeyUserSigningKey)
	if !bytes.Equal(userA, userB) {
		t.Fatal("same secret + key name must derive identical keys")
	}
	if len(userA) != 32 {
		t.Fatalf("expected a 32-byte key, got %d bytes", len(userA))
	}

	// Domain separation: user vs agent key must differ.
	agent := deriveSharedSigningKey(secret, SecretKeyAgentSigningKey)
	if bytes.Equal(userA, agent) {
		t.Fatal("user and agent keys derived from the same secret must differ")
	}

	// A different secret must produce a different key.
	other := deriveSharedSigningKey("a-different-secret", SecretKeyUserSigningKey)
	if bytes.Equal(userA, other) {
		t.Fatal("different secrets must derive different keys")
	}
}

// TestEnsureSigningKey_SharedSecretReplicaPortable is the regression test for
// the cross-replica "session_expired" login loop: two replicas with DIFFERENT
// host-derived hub IDs but the SAME shared signing secret must resolve
// identical signing keys, so a user JWT minted by one replica validates on the
// other. A replica with a different shared secret must NOT be able to validate
// the token.
func TestEnsureSigningKey_SharedSecretReplicaPortable(t *testing.T) {
	const sharedSecret = "the-load-balancer-shared-secret"
	ctx := context.Background()

	// Two replicas of one logical hub, distinct hub IDs (sha256(hostname)).
	replicaA := &Server{hubID: "ca39430276ee", config: ServerConfig{SharedSigningSecret: sharedSecret}}
	replicaB := &Server{hubID: "9662ebe99da4", config: ServerConfig{SharedSigningSecret: sharedSecret}}

	// ensureSigningKey returns before touching the store/secret backend when a
	// shared secret is set, so a nil store is fine here.
	keyA, err := replicaA.ensureSigningKey(ctx, SecretKeyUserSigningKey, nil)
	if err != nil {
		t.Fatalf("replicaA ensureSigningKey: %v", err)
	}
	keyB, err := replicaB.ensureSigningKey(ctx, SecretKeyUserSigningKey, nil)
	if err != nil {
		t.Fatalf("replicaB ensureSigningKey: %v", err)
	}
	if !bytes.Equal(keyA, keyB) {
		t.Fatal("replicas sharing a signing secret must derive identical keys despite different hub IDs")
	}

	// Mint a user token on replica A; it must validate on replica B.
	svcA, err := NewUserTokenService(UserTokenConfig{SigningKey: keyA})
	if err != nil {
		t.Fatalf("NewUserTokenService A: %v", err)
	}
	svcB, err := NewUserTokenService(UserTokenConfig{SigningKey: keyB})
	if err != nil {
		t.Fatalf("NewUserTokenService B: %v", err)
	}

	accessToken, _, _, err := svcA.GenerateTokenPair("uid-1", "user@example.com", "User", "admin", ClientTypeWeb)
	if err != nil {
		t.Fatalf("GenerateTokenPair: %v", err)
	}
	if _, err := svcB.ValidateUserToken(accessToken); err != nil {
		t.Fatalf("token minted on replica A must validate on replica B, got: %v", err)
	}

	// Negative: a replica with a different shared secret cannot validate it.
	replicaC := &Server{hubID: "ca39430276ee", config: ServerConfig{SharedSigningSecret: "a-totally-different-secret"}}
	keyC, err := replicaC.ensureSigningKey(ctx, SecretKeyUserSigningKey, nil)
	if err != nil {
		t.Fatalf("replicaC ensureSigningKey: %v", err)
	}
	svcC, err := NewUserTokenService(UserTokenConfig{SigningKey: keyC})
	if err != nil {
		t.Fatalf("NewUserTokenService C: %v", err)
	}
	if _, err := svcC.ValidateUserToken(accessToken); err == nil {
		t.Fatal("token must NOT validate under a different shared secret")
	}
}

// TestEnsureSigningKey_PreConfiguredKeyTakesPrecedence verifies that an
// explicitly supplied key still wins over shared-secret derivation, preserving
// existing behavior for callers that pass a key directly.
func TestEnsureSigningKey_PreConfiguredKeyTakesPrecedence(t *testing.T) {
	explicit := bytes.Repeat([]byte{0xAB}, 32)
	s := &Server{hubID: "host1", config: ServerConfig{SharedSigningSecret: "ignored-because-explicit-key-given"}}

	got, err := s.ensureSigningKey(context.Background(), SecretKeyUserSigningKey, explicit)
	if err != nil {
		t.Fatalf("ensureSigningKey: %v", err)
	}
	if !bytes.Equal(got, explicit) {
		t.Fatal("a pre-configured key must take precedence over shared-secret derivation")
	}
}
