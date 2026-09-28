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

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentidentitykey"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentIdentityKeyStore implements store.AgentIdentityKeyStore using Ent ORM.
type AgentIdentityKeyStore struct {
	client *ent.Client
}

// NewAgentIdentityKeyStore creates a new Ent-backed AgentIdentityKeyStore.
func NewAgentIdentityKeyStore(client *ent.Client) *AgentIdentityKeyStore {
	return &AgentIdentityKeyStore{client: client}
}

func entAgentIdentityKeyToStore(k *ent.AgentIdentityKey) *store.AgentIdentityKey {
	return &store.AgentIdentityKey{
		ID:        k.ID.String(),
		ProjectID: k.ProjectID.String(),
		Key:       k.Key,
		AgentID:   k.AgentID.String(),
	}
}

// ReplaceAgentIdentityKeys atomically replaces agentID's identity-key rows in
// projectID with keys: rows for keys no longer present are deleted, rows for
// new keys are inserted, and rows for keys already present are left alone
// (so their row IDs are stable across calls). A unique-constraint violation
// on insert -- another agent in the project already holds that key --
// surfaces as store.ErrIdentityKeyConflict.
func (s *AgentIdentityKeyStore) ReplaceAgentIdentityKeys(ctx context.Context, agentID, projectID string, keys []string) error {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return err
	}
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return err
	}

	existing, err := s.client.AgentIdentityKey.Query().
		Where(agentidentitykey.AgentIDEQ(agentUID)).
		All(ctx)
	if err != nil {
		return mapError(err)
	}

	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	have := make(map[string]uuid.UUID, len(existing))
	for _, row := range existing {
		have[row.Key] = row.ID
	}

	var staleIDs []uuid.UUID
	for k, id := range have {
		if !want[k] {
			staleIDs = append(staleIDs, id)
		}
	}
	if len(staleIDs) > 0 {
		if _, err := s.client.AgentIdentityKey.Delete().
			Where(agentidentitykey.IDIn(staleIDs...)).
			Exec(ctx); err != nil {
			return mapError(err)
		}
	}

	for k := range want {
		if _, alreadyHeld := have[k]; alreadyHeld {
			continue
		}
		_, err := s.client.AgentIdentityKey.Create().
			SetProjectID(projectUID).
			SetKey(k).
			SetAgentID(agentUID).
			Save(ctx)
		if err != nil {
			if ent.IsConstraintError(err) {
				return store.ErrIdentityKeyConflict
			}
			return mapError(err)
		}
	}
	return nil
}

// DeleteAgentIdentityKeys removes all of agentID's identity-key rows,
// freeing its keys for reuse. Used on hard delete/purge.
func (s *AgentIdentityKeyStore) DeleteAgentIdentityKeys(ctx context.Context, agentID string) error {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return err
	}
	if _, err := s.client.AgentIdentityKey.Delete().
		Where(agentidentitykey.AgentIDEQ(agentUID)).
		Exec(ctx); err != nil {
		return mapError(err)
	}
	return nil
}

// ListAgentIdentityKeys returns every identity-key row in projectID, across
// all agents.
func (s *AgentIdentityKeyStore) ListAgentIdentityKeys(ctx context.Context, projectID string) ([]*store.AgentIdentityKey, error) {
	projectUID, err := parseUUID(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.AgentIdentityKey.Query().
		Where(agentidentitykey.ProjectIDEQ(projectUID)).
		All(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	result := make([]*store.AgentIdentityKey, len(rows))
	for i, row := range rows {
		result[i] = entAgentIdentityKeyToStore(row)
	}
	return result, nil
}
