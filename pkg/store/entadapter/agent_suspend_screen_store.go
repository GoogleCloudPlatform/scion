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
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agentsuspendscreen"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// AgentSuspendScreenStore implements store.AgentSuspendScreenStore using Ent ORM.
type AgentSuspendScreenStore struct {
	client *ent.Client
}

// NewAgentSuspendScreenStore creates a new Ent-backed AgentSuspendScreenStore.
func NewAgentSuspendScreenStore(client *ent.Client) *AgentSuspendScreenStore {
	return &AgentSuspendScreenStore{client: client}
}

var _ store.AgentSuspendScreenStore = (*AgentSuspendScreenStore)(nil)
var _ store.AgentSuspendScreenStore = (*CompositeStore)(nil)

// PutAgentSuspendScreen upserts the agent's saved terminal screen
// (ON CONFLICT(agent_id) DO UPDATE). Returns store.ErrNotFound if the agent
// does not exist.
func (s *AgentSuspendScreenStore) PutAgentSuspendScreen(ctx context.Context, screen *store.AgentSuspendScreen) error {
	if screen == nil {
		return fmt.Errorf("%w: nil agent suspend screen", store.ErrInvalidInput)
	}
	agentUID, err := parseUUID(screen.AgentID)
	if err != nil {
		return fmt.Errorf("%w: invalid agent ID %q", store.ErrInvalidInput, screen.AgentID)
	}
	capturedAt := screen.CapturedAt.UTC().Truncate(time.Microsecond)

	// CreateBulk avoids single-row Create+OnConflict RETURNING quirks across dialects.
	err = s.client.AgentSuspendScreen.CreateBulk(
		s.client.AgentSuspendScreen.Create().
			SetID(uuid.New()).
			SetAgentID(agentUID).
			SetContent(screen.Content).
			SetTruncated(screen.Truncated).
			SetCapturedAt(capturedAt),
	).
		OnConflictColumns(agentsuspendscreen.FieldAgentID).
		Update(func(u *ent.AgentSuspendScreenUpsert) {
			u.UpdateContent()
			u.UpdateTruncated()
			u.UpdateCapturedAt()
		}).
		Exec(ctx)
	if err != nil {
		if ent.IsConstraintError(err) {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "foreign key") || strings.Contains(msg, "sqlstate 23503") {
				return store.ErrNotFound
			}
		}
		return mapError(err)
	}
	return nil
}

// GetAgentSuspendScreen returns the agent's saved terminal screen, or
// store.ErrNotFound when no row exists.
func (s *AgentSuspendScreenStore) GetAgentSuspendScreen(ctx context.Context, agentID string) (*store.AgentSuspendScreen, error) {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	row, err := s.client.AgentSuspendScreen.Query().
		Where(agentsuspendscreen.AgentIDEQ(agentUID)).
		Only(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	return &store.AgentSuspendScreen{
		AgentID:    row.AgentID.String(),
		Content:    row.Content,
		Truncated:  row.Truncated,
		CapturedAt: row.CapturedAt.UTC(),
	}, nil
}

// DeleteAgentSuspendScreen deletes the agent's saved terminal screen. It is a
// no-op when no row exists.
func (s *AgentSuspendScreenStore) DeleteAgentSuspendScreen(ctx context.Context, agentID string) error {
	agentUID, err := parseUUID(agentID)
	if err != nil {
		return nil
	}
	_, err = s.client.AgentSuspendScreen.Delete().
		Where(agentsuspendscreen.AgentIDEQ(agentUID)).
		Exec(ctx)
	if err != nil {
		return mapError(err)
	}
	return nil
}
