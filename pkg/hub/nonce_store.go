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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/noncecache"
)

// NonceCacheStore is a database-backed nonce cache for HMAC replay prevention.
// It stores nonces in a Postgres table so that replay protection works across
// multiple hub instances. This replaces the process-local in-memory NonceCache
// for multi-instance deployments.
//
// The implementation follows the GitHubResolutionStore pattern — a thin wrapper
// around the ent client with TTL-based expiration.
type NonceCacheStore struct {
	client *ent.Client
}

// NewNonceCacheStore creates a new database-backed nonce cache store.
func NewNonceCacheStore(client *ent.Client) *NonceCacheStore {
	return &NonceCacheStore{client: client}
}

// CheckAndStore atomically checks whether a nonce has been seen and, if not,
// stores it with the given TTL. Returns true if the nonce is new (request is
// valid); returns false if the nonce already exists (replay detected).
//
// The insert relies on the unique constraint on the nonce column: if the
// nonce already exists the database rejects the INSERT with a constraint
// error, which ent surfaces as ConstraintError. Any other error is treated
// as fail-closed (returns false) to avoid silently accepting replays.
func (s *NonceCacheStore) CheckAndStore(ctx context.Context, nonce string, ttl time.Duration) (bool, error) {
	expiresAt := time.Now().Add(ttl)

	// Plain insert — the unique constraint on nonce rejects duplicates.
	_, err := s.client.NonceCache.
		Create().
		SetNonce(nonce).
		SetExpiresAt(expiresAt).
		Save(ctx)
	if err != nil {
		// Unique constraint violation → nonce already exists → replay.
		if ent.IsConstraintError(err) {
			return false, nil
		}
		// Any other DB error → fail-closed (reject the request).
		return false, err
	}

	return true, nil // nonce is new
}

// PurgeExpired deletes all nonce entries where expires_at < now and returns
// the number of rows removed for observability.
func (s *NonceCacheStore) PurgeExpired(ctx context.Context) (int, error) {
	count, err := s.client.NonceCache.
		Delete().
		Where(noncecache.ExpiresAtLT(time.Now())).
		Exec(ctx)
	return count, err
}
