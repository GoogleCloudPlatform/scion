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

package entadapter

import (
	"context"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentcredential"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentCredentialStore implements store.AgentCredentialStore using Ent ORM.
type AgentCredentialStore struct {
	client *ent.Client
}

// NewAgentCredentialStore creates a new Ent-backed AgentCredentialStore.
func NewAgentCredentialStore(client *ent.Client) *AgentCredentialStore {
	return &AgentCredentialStore{client: client}
}

// entAgentCredentialToStore converts an Ent AgentCredential entity to a store model.
func entAgentCredentialToStore(ac *ent.AgentCredential) *store.AgentCredential {
	return &store.AgentCredential{
		ID:           ac.ID.String(),
		AgentID:      ac.AgentID,
		ProjectID:    ac.ProjectID,
		TokenJTIHash: ac.TokenJtiHash,
		IssuedAt:     ac.IssuedAt,
		ExpiresAt:    ac.ExpiresAt,
		RevokedAt:    ac.RevokedAt,
		RevokedBy:    ac.RevokedBy,
		RevokeReason: ac.RevokeReason,
		LastSeenAt:   ac.LastSeenAt,
	}
}

// CreateAgentCredential records a newly issued agent token.
func (s *AgentCredentialStore) CreateAgentCredential(ctx context.Context, cred *store.AgentCredential) error {
	builder := s.client.AgentCredential.Create().
		SetAgentID(cred.AgentID).
		SetProjectID(cred.ProjectID).
		SetTokenJtiHash(cred.TokenJTIHash).
		SetIssuedAt(cred.IssuedAt).
		SetExpiresAt(cred.ExpiresAt)

	if cred.ID != "" {
		uid, err := parseUUID(cred.ID)
		if err != nil {
			return err
		}
		builder.SetID(uid)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return mapError(err)
	}
	cred.ID = created.ID.String()
	return nil
}

// GetAgentCredentialByJTIHash looks up a credential by its JTI hash.
func (s *AgentCredentialStore) GetAgentCredentialByJTIHash(ctx context.Context, jtiHash string) (*store.AgentCredential, error) {
	ac, err := s.client.AgentCredential.Query().
		Where(agentcredential.TokenJtiHashEQ(jtiHash)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return entAgentCredentialToStore(ac), nil
}

// RevokeAgentCredential marks a credential as revoked.
func (s *AgentCredentialStore) RevokeAgentCredential(ctx context.Context, id string, revokedBy string, reason string) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = s.client.AgentCredential.UpdateOneID(uid).
		SetRevokedAt(now).
		SetRevokedBy(revokedBy).
		SetRevokeReason(reason).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// RevokeAgentCredentialsByAgent revokes all active credentials for an agent.
func (s *AgentCredentialStore) RevokeAgentCredentialsByAgent(ctx context.Context, agentID string, revokedBy string, reason string) (int, error) {
	now := time.Now()
	n, err := s.client.AgentCredential.Update().
		Where(
			agentcredential.AgentIDEQ(agentID),
			agentcredential.RevokedAtIsNil(),
		).
		SetRevokedAt(now).
		SetRevokedBy(revokedBy).
		SetRevokeReason(reason).
		Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// UpdateAgentCredentialLastSeen updates the last_seen_at timestamp.
func (s *AgentCredentialStore) UpdateAgentCredentialLastSeen(ctx context.Context, id string, lastSeen time.Time) error {
	uid, err := parseGetID(id)
	if err != nil {
		return err
	}
	_, err = s.client.AgentCredential.UpdateOneID(uid).
		SetLastSeenAt(lastSeen).
		Save(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}

// PurgeExpiredAgentCredentials removes expired credentials older than cutoff.
func (s *AgentCredentialStore) PurgeExpiredAgentCredentials(ctx context.Context, cutoff time.Time) (int, error) {
	n, err := s.client.AgentCredential.Delete().
		Where(agentcredential.ExpiresAtLT(cutoff)).
		Exec(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}
