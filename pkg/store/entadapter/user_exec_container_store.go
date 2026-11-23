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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/userexeccontainer"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// UserExecContainerStore implements store.UserExecContainerStore using Ent ORM.
// There is at most one row per user, keyed on the unique user_id column;
// writes are unconditional atomic upserts (last-writer-wins).
type UserExecContainerStore struct {
	client *ent.Client
}

// NewUserExecContainerStore creates a new Ent-backed UserExecContainerStore.
func NewUserExecContainerStore(client *ent.Client) *UserExecContainerStore {
	return &UserExecContainerStore{client: client}
}

func entUserExecContainerToStore(r *ent.UserExecContainer) *store.UserExecContainer {
	return &store.UserExecContainer{
		UserID:    r.UserID.String(),
		AgentID:   r.AgentID.String(),
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

// GetUserExecContainer returns the registered execution container for userID,
// or store.ErrNotFound when none is registered.
func (s *UserExecContainerStore) GetUserExecContainer(ctx context.Context, userID string) (*store.UserExecContainer, error) {
	uid, err := parseGetID(userID)
	if err != nil {
		return nil, err
	}

	row, err := s.client.UserExecContainer.Query().
		Where(userexeccontainer.UserIDEQ(uid)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("query user exec container: %w", err)
	}
	return entUserExecContainerToStore(row), nil
}

// PutUserExecContainer atomically upserts the registered execution container
// for userID (last-writer-wins) and returns the stored row.
func (s *UserExecContainerStore) PutUserExecContainer(ctx context.Context, userID, agentID string) (*store.UserExecContainer, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}
	aid, err := parseUUID(agentID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC().Truncate(time.Microsecond)

	err = s.client.UserExecContainer.Create().
		SetUserID(uid).
		SetAgentID(aid).
		SetCreatedAt(now).
		SetUpdatedAt(now).
		OnConflictColumns(userexeccontainer.FieldUserID).
		Update(func(u *ent.UserExecContainerUpsert) {
			u.UpdateAgentID()
			u.SetUpdatedAt(now)
		}).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert user exec container: %w", err)
	}

	return s.GetUserExecContainer(ctx, userID)
}

// DeleteUserExecContainer removes any registered execution container for
// userID. It is idempotent and returns nil when no row exists.
func (s *UserExecContainerStore) DeleteUserExecContainer(ctx context.Context, userID string) error {
	uid, err := parseGetID(userID)
	if err != nil {
		if err == store.ErrNotFound {
			return nil
		}
		return err
	}

	_, err = s.client.UserExecContainer.Delete().
		Where(userexeccontainer.UserIDEQ(uid)).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("delete user exec container: %w", err)
	}
	return nil
}
