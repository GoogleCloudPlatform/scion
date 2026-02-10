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

package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ptone/scion-agent/pkg/store"
)

func TestBrokerSecretCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// First create a runtime broker to satisfy FK constraint
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host",
		Slug:    "test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Test CreateBrokerSecret
	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: []byte("test-secret-key-32-bytes-long!!"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("CreateBrokerSecret failed: %v", err)
	}

	// Verify timestamps were set
	if secret.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set automatically")
	}

	// Test GetBrokerSecret
	retrieved, err := s.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("GetBrokerSecret failed: %v", err)
	}
	if retrieved.BrokerID != brokerID {
		t.Errorf("BrokerID mismatch: got %s, want %s", retrieved.BrokerID, brokerID)
	}
	if string(retrieved.SecretKey) != string(secret.SecretKey) {
		t.Error("SecretKey mismatch")
	}
	if retrieved.Algorithm != store.BrokerSecretAlgorithmHMACSHA256 {
		t.Errorf("Algorithm mismatch: got %s, want %s", retrieved.Algorithm, store.BrokerSecretAlgorithmHMACSHA256)
	}
	if retrieved.Status != store.BrokerSecretStatusActive {
		t.Errorf("Status mismatch: got %s, want %s", retrieved.Status, store.BrokerSecretStatusActive)
	}

	// Test duplicate create returns error
	if err := s.CreateBrokerSecret(ctx, secret); err != store.ErrAlreadyExists {
		t.Errorf("Expected ErrAlreadyExists, got: %v", err)
	}

	// Test UpdateBrokerSecret
	newKey := []byte("new-secret-key-32-bytes-long!!!")
	retrieved.SecretKey = newKey
	retrieved.RotatedAt = time.Now()
	retrieved.Status = store.BrokerSecretStatusDeprecated

	if err := s.UpdateBrokerSecret(ctx, retrieved); err != nil {
		t.Fatalf("UpdateBrokerSecret failed: %v", err)
	}

	// Verify update
	updated, err := s.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("GetBrokerSecret after update failed: %v", err)
	}
	if string(updated.SecretKey) != string(newKey) {
		t.Error("SecretKey not updated")
	}
	if updated.Status != store.BrokerSecretStatusDeprecated {
		t.Errorf("Status not updated: got %s, want %s", updated.Status, store.BrokerSecretStatusDeprecated)
	}
	if updated.RotatedAt.IsZero() {
		t.Error("RotatedAt should be set")
	}

	// Test DeleteBrokerSecret
	if err := s.DeleteBrokerSecret(ctx, brokerID); err != nil {
		t.Fatalf("DeleteBrokerSecret failed: %v", err)
	}

	// Verify deletion
	_, err = s.GetBrokerSecret(ctx, brokerID)
	if err != store.ErrNotFound {
		t.Errorf("Expected ErrNotFound after delete, got: %v", err)
	}

	// Test delete non-existent returns error
	if err := s.DeleteBrokerSecret(ctx, "non-existent"); err != store.ErrNotFound {
		t.Errorf("Expected ErrNotFound for non-existent delete, got: %v", err)
	}
}

func TestBrokerSecretForeignKey(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Try to create secret for non-existent broker
	secret := &store.BrokerSecret{
		BrokerID:    "non-existent-host",
		SecretKey: []byte("test-secret"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}

	err := s.CreateBrokerSecret(ctx, secret)
	if err == nil {
		t.Error("Expected error when creating secret for non-existent broker")
	}
}

func TestBrokerJoinTokenCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// First create a runtime broker to satisfy FK constraint
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "test-host-for-token",
		Slug:    "test-host-for-token",
				Status:  store.BrokerStatusOffline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Test CreateJoinToken
	token := &store.BrokerJoinToken{
		BrokerID:    brokerID,
		TokenHash: "test-token-hash-abc123",
		ExpiresAt: time.Now().Add(1 * time.Hour),
		CreatedBy: "admin-user-id",
	}
	if err := s.CreateJoinToken(ctx, token); err != nil {
		t.Fatalf("CreateJoinToken failed: %v", err)
	}

	// Verify timestamps were set
	if token.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set automatically")
	}

	// Test GetJoinToken by hash
	retrieved, err := s.GetJoinToken(ctx, "test-token-hash-abc123")
	if err != nil {
		t.Fatalf("GetJoinToken failed: %v", err)
	}
	if retrieved.BrokerID != brokerID {
		t.Errorf("BrokerID mismatch: got %s, want %s", retrieved.BrokerID, brokerID)
	}
	if retrieved.TokenHash != "test-token-hash-abc123" {
		t.Errorf("TokenHash mismatch: got %s, want %s", retrieved.TokenHash, "test-token-hash-abc123")
	}
	if retrieved.CreatedBy != "admin-user-id" {
		t.Errorf("CreatedBy mismatch: got %s, want %s", retrieved.CreatedBy, "admin-user-id")
	}

	// Test GetJoinTokenByBrokerID
	byHost, err := s.GetJoinTokenByBrokerID(ctx, brokerID)
	if err != nil {
		t.Fatalf("GetJoinTokenByBrokerID failed: %v", err)
	}
	if byHost.TokenHash != "test-token-hash-abc123" {
		t.Errorf("TokenHash mismatch: got %s, want %s", byHost.TokenHash, "test-token-hash-abc123")
	}

	// Test duplicate create returns error
	if err := s.CreateJoinToken(ctx, token); err != store.ErrAlreadyExists {
		t.Errorf("Expected ErrAlreadyExists, got: %v", err)
	}

	// Test DeleteJoinToken
	if err := s.DeleteJoinToken(ctx, brokerID); err != nil {
		t.Fatalf("DeleteJoinToken failed: %v", err)
	}

	// Verify deletion
	_, err = s.GetJoinToken(ctx, "test-token-hash-abc123")
	if err != store.ErrNotFound {
		t.Errorf("Expected ErrNotFound after delete, got: %v", err)
	}

	// Test delete non-existent returns error
	if err := s.DeleteJoinToken(ctx, "non-existent"); err != store.ErrNotFound {
		t.Errorf("Expected ErrNotFound for non-existent delete, got: %v", err)
	}
}

func TestCleanExpiredJoinTokens(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create two brokers
	host1ID := uuid.New().String()
	host2ID := uuid.New().String()
	for i, id := range []string{host1ID, host2ID} {
		broker := &store.RuntimeBroker{
			ID:      id,
			Name:    "test-host-" + string(rune('a'+i)),
			Slug:    "test-host-" + string(rune('a'+i)),
						Status:  store.BrokerStatusOffline,
			Created: time.Now(),
			Updated: time.Now(),
		}
		if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
			t.Fatalf("failed to create runtime broker: %v", err)
		}
	}

	// Create an expired token and a valid token
	expiredToken := &store.BrokerJoinToken{
		BrokerID:    host1ID,
		TokenHash: "expired-token-hash",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // Already expired
		CreatedBy: "admin",
	}
	validToken := &store.BrokerJoinToken{
		BrokerID:    host2ID,
		TokenHash: "valid-token-hash",
		ExpiresAt: time.Now().Add(1 * time.Hour), // Still valid
		CreatedBy: "admin",
	}

	if err := s.CreateJoinToken(ctx, expiredToken); err != nil {
		t.Fatalf("CreateJoinToken (expired) failed: %v", err)
	}
	if err := s.CreateJoinToken(ctx, validToken); err != nil {
		t.Fatalf("CreateJoinToken (valid) failed: %v", err)
	}

	// Clean expired tokens
	if err := s.CleanExpiredJoinTokens(ctx); err != nil {
		t.Fatalf("CleanExpiredJoinTokens failed: %v", err)
	}

	// Verify expired token is gone
	_, err := s.GetJoinToken(ctx, "expired-token-hash")
	if err != store.ErrNotFound {
		t.Errorf("Expected expired token to be deleted, got: %v", err)
	}

	// Verify valid token still exists
	_, err = s.GetJoinToken(ctx, "valid-token-hash")
	if err != nil {
		t.Errorf("Expected valid token to still exist, got: %v", err)
	}
}

func TestBrokerSecretCascadeDelete(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create a runtime broker
	brokerID := uuid.New().String()
	broker := &store.RuntimeBroker{
		ID:      brokerID,
		Name:    "cascade-test-host",
		Slug:    "cascade-test-host",
				Status:  store.BrokerStatusOnline,
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create runtime broker: %v", err)
	}

	// Create a secret for the broker
	secret := &store.BrokerSecret{
		BrokerID:    brokerID,
		SecretKey: []byte("test-secret"),
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}
	if err := s.CreateBrokerSecret(ctx, secret); err != nil {
		t.Fatalf("CreateBrokerSecret failed: %v", err)
	}

	// Verify secret exists
	_, err := s.GetBrokerSecret(ctx, brokerID)
	if err != nil {
		t.Fatalf("GetBrokerSecret failed: %v", err)
	}

	// Delete the runtime broker
	if err := s.DeleteRuntimeBroker(ctx, brokerID); err != nil {
		t.Fatalf("DeleteRuntimeBroker failed: %v", err)
	}

	// Verify secret was cascade deleted
	_, err = s.GetBrokerSecret(ctx, brokerID)
	if err != store.ErrNotFound {
		t.Errorf("Expected secret to be cascade deleted, got: %v", err)
	}
}
