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

package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ProjectDomain describes the project (grove) entity for the CRUD-parity oracle.
func ProjectDomain() Domain[store.Project] {
	return Domain[store.Project]{
		Name: "project",
		Make: func(seq int) *store.Project {
			id := uuid.NewString()
			return &store.Project{
				ID:         id,
				Name:       fmt.Sprintf("Project %d", seq),
				Slug:       fmt.Sprintf("project-%d-%s", seq, id[:8]),
				Visibility: store.VisibilityPrivate,
				Labels:     map[string]string{"seq": fmt.Sprintf("%d", seq)},
			}
		},
		GetID: func(p *store.Project) string { return p.ID },
		Create: func(ctx context.Context, s store.Store, p *store.Project) error {
			return s.CreateProject(ctx, p)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.Project, error) {
			return s.GetProject(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.Project], error) {
			return s.ListProjects(ctx, store.ProjectFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.Project) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.Slug, got.Slug)
			assert.Equal(t, want.Visibility, got.Visibility)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(p *store.Project) {
			p.Name = "Renamed " + p.Name
			p.Visibility = "public"
		},
		Update: func(ctx context.Context, s store.Store, p *store.Project) error {
			return s.UpdateProject(ctx, p)
		},
		VerifyMutated: func(t *testing.T, got *store.Project) {
			assert.Contains(t, got.Name, "Renamed ")
			assert.Equal(t, "public", got.Visibility)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteProject(ctx, id)
		},
		Filters: []FilterCase[store.Project]{
			{
				Name: "ByVisibility",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateProject(ctx, &store.Project{
						ID: uuid.NewString(), Name: "Public", Slug: "public-" + uuid.NewString()[:8], Visibility: "public",
					}))
					require.NoError(t, s.CreateProject(ctx, &store.Project{
						ID: uuid.NewString(), Name: "Private", Slug: "private-" + uuid.NewString()[:8], Visibility: store.VisibilityPrivate,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.Project], error) {
					return s.ListProjects(ctx, store.ProjectFilter{Visibility: "public"}, store.ListOptions{})
				},
				WantCount: 1,
			},
		},
	}
}

// RuntimeBrokerDomain describes the runtime broker entity for the oracle.
func RuntimeBrokerDomain() Domain[store.RuntimeBroker] {
	return Domain[store.RuntimeBroker]{
		Name: "runtime_broker",
		Make: func(seq int) *store.RuntimeBroker {
			id := uuid.NewString()
			return &store.RuntimeBroker{
				ID:      id,
				Name:    fmt.Sprintf("Broker %d", seq),
				Slug:    fmt.Sprintf("broker-%d-%s", seq, id[:8]),
				Version: "1.0.0",
				Status:  store.BrokerStatusOffline,
			}
		},
		GetID: func(b *store.RuntimeBroker) string { return b.ID },
		Create: func(ctx context.Context, s store.Store, b *store.RuntimeBroker) error {
			return s.CreateRuntimeBroker(ctx, b)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.RuntimeBroker, error) {
			return s.GetRuntimeBroker(ctx, id)
		},
		List: func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
			return s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, opts)
		},
		VerifyEqual: func(t *testing.T, want, got *store.RuntimeBroker) {
			assert.Equal(t, want.ID, got.ID)
			assert.Equal(t, want.Name, got.Name)
			assert.Equal(t, want.Slug, got.Slug)
			assert.Equal(t, want.Version, got.Version)
			assert.False(t, got.Created.IsZero(), "Created timestamp should be set")
		},
		Mutate: func(b *store.RuntimeBroker) {
			b.Name = "Renamed " + b.Name
			b.Version = "2.0.0"
			b.Status = store.BrokerStatusOnline
		},
		Update: func(ctx context.Context, s store.Store, b *store.RuntimeBroker) error {
			return s.UpdateRuntimeBroker(ctx, b)
		},
		VerifyMutated: func(t *testing.T, got *store.RuntimeBroker) {
			assert.Contains(t, got.Name, "Renamed ")
			assert.Equal(t, "2.0.0", got.Version)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteRuntimeBroker(ctx, id)
		},
		Filters: []FilterCase[store.RuntimeBroker]{
			{
				Name: "ByStatus",
				Seed: func(t *testing.T, ctx context.Context, s store.Store) {
					require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
						ID: uuid.NewString(), Name: "Online", Slug: "online-" + uuid.NewString()[:8], Status: store.BrokerStatusOnline,
					}))
					require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
						ID: uuid.NewString(), Name: "Offline", Slug: "offline-" + uuid.NewString()[:8], Status: store.BrokerStatusOffline,
					}))
				},
				List: func(ctx context.Context, s store.Store) (*store.ListResult[store.RuntimeBroker], error) {
					return s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Status: store.BrokerStatusOnline}, store.ListOptions{})
				},
				WantCount: 1,
			},
		},
	}
}

// ensureBroker creates the runtime_brokers row a broker-scoped entity references
// via foreign key (broker_secrets / broker_join_tokens on the SQLite backend).
// It is idempotent: an already-existing broker is not an error. It keeps these
// domains self-contained without relying on a shared Prepare hook.
func ensureBroker(ctx context.Context, s store.Store, brokerID string) error {
	err := s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID:   brokerID,
		Name: "fk-broker-" + brokerID[:8],
		Slug: "fk-broker-" + brokerID[:8],
	})
	if err != nil && err != store.ErrAlreadyExists {
		return err
	}
	return nil
}

// BrokerSecretDomain describes the broker secret entity for the oracle. It has
// no List operation (one secret per broker, keyed on broker_id), so the
// pagination and filter categories are skipped.
func BrokerSecretDomain() Domain[store.BrokerSecret] {
	return Domain[store.BrokerSecret]{
		Name: "broker_secret",
		Make: func(seq int) *store.BrokerSecret {
			return &store.BrokerSecret{
				BrokerID:  uuid.NewString(),
				SecretKey: []byte(fmt.Sprintf("hmac-key-%d", seq)),
				Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
				Status:    store.BrokerSecretStatusActive,
			}
		},
		GetID: func(b *store.BrokerSecret) string { return b.BrokerID },
		Create: func(ctx context.Context, s store.Store, b *store.BrokerSecret) error {
			if err := ensureBroker(ctx, s, b.BrokerID); err != nil {
				return err
			}
			return s.CreateBrokerSecret(ctx, b)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.BrokerSecret, error) {
			return s.GetBrokerSecret(ctx, id)
		},
		VerifyEqual: func(t *testing.T, want, got *store.BrokerSecret) {
			assert.Equal(t, want.BrokerID, got.BrokerID)
			assert.Equal(t, want.SecretKey, got.SecretKey)
			assert.Equal(t, want.Algorithm, got.Algorithm)
			assert.Equal(t, want.Status, got.Status)
			assert.False(t, got.CreatedAt.IsZero(), "CreatedAt timestamp should be set")
		},
		Mutate: func(b *store.BrokerSecret) {
			b.SecretKey = []byte("rotated-key")
			b.Status = store.BrokerSecretStatusDeprecated
		},
		Update: func(ctx context.Context, s store.Store, b *store.BrokerSecret) error {
			return s.UpdateBrokerSecret(ctx, b)
		},
		VerifyMutated: func(t *testing.T, got *store.BrokerSecret) {
			assert.Equal(t, []byte("rotated-key"), got.SecretKey)
			assert.Equal(t, store.BrokerSecretStatusDeprecated, got.Status)
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteBrokerSecret(ctx, id)
		},
	}
}

// BrokerJoinTokenDomain describes the broker join token entity for the oracle.
// Join tokens are immutable (no Update) and have no List, so only the Create,
// Read and Delete categories apply.
func BrokerJoinTokenDomain() Domain[store.BrokerJoinToken] {
	return Domain[store.BrokerJoinToken]{
		Name: "broker_join_token",
		Make: func(seq int) *store.BrokerJoinToken {
			return &store.BrokerJoinToken{
				BrokerID:  uuid.NewString(),
				TokenHash: fmt.Sprintf("token-hash-%d-%s", seq, uuid.NewString()),
				ExpiresAt: time.Now().Add(time.Hour),
				CreatedBy: uuid.NewString(),
			}
		},
		GetID: func(tok *store.BrokerJoinToken) string { return tok.BrokerID },
		Create: func(ctx context.Context, s store.Store, tok *store.BrokerJoinToken) error {
			if err := ensureBroker(ctx, s, tok.BrokerID); err != nil {
				return err
			}
			return s.CreateJoinToken(ctx, tok)
		},
		Get: func(ctx context.Context, s store.Store, id string) (*store.BrokerJoinToken, error) {
			return s.GetJoinTokenByBrokerID(ctx, id)
		},
		VerifyEqual: func(t *testing.T, want, got *store.BrokerJoinToken) {
			assert.Equal(t, want.BrokerID, got.BrokerID)
			assert.Equal(t, want.TokenHash, got.TokenHash)
			assert.Equal(t, want.CreatedBy, got.CreatedBy)
			assert.False(t, got.CreatedAt.IsZero(), "CreatedAt timestamp should be set")
		},
		Delete: func(ctx context.Context, s store.Store, id string) error {
			return s.DeleteJoinToken(ctx, id)
		},
	}
}
