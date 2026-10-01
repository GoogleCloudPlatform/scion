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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/userterminalworkspace"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// UserTerminalWorkspaceStore implements store.UserTerminalWorkspaceStore using
// Ent ORM. There is exactly one row per user, keyed on the unique user_id
// column; writes are unconditional upserts (last-writer-wins).
type UserTerminalWorkspaceStore struct {
	client *ent.Client
}

// NewUserTerminalWorkspaceStore creates a new Ent-backed
// UserTerminalWorkspaceStore.
func NewUserTerminalWorkspaceStore(client *ent.Client) *UserTerminalWorkspaceStore {
	return &UserTerminalWorkspaceStore{client: client}
}

// entUserTerminalWorkspaceToStore converts an Ent UserTerminalWorkspace to a
// store model.
func entUserTerminalWorkspaceToStore(w *ent.UserTerminalWorkspace) *store.UserTerminalWorkspace {
	sw := &store.UserTerminalWorkspace{
		UserID:   w.UserID.String(),
		AgentIDs: w.AgentIds,
		Revision: w.Revision,
		Updated:  w.UpdateTime,
	}
	if sw.AgentIDs == nil {
		sw.AgentIDs = []string{}
	}
	if w.FrontmostAgentID != nil {
		sw.FrontmostAgentID = *w.FrontmostAgentID
	}
	return sw
}

// GetUserTerminalWorkspace returns the saved workspace for userID. Returns
// store.ErrNotFound when the user has never saved one.
func (s *UserTerminalWorkspaceStore) GetUserTerminalWorkspace(ctx context.Context, userID string) (*store.UserTerminalWorkspace, error) {
	uid, err := parseGetID(userID)
	if err != nil {
		return nil, err
	}

	w, err := s.client.UserTerminalWorkspace.Query().
		Where(userterminalworkspace.UserIDEQ(uid)).
		Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("query user terminal workspace: %w", err)
	}
	return entUserTerminalWorkspaceToStore(w), nil
}

// PutUserTerminalWorkspace unconditionally upserts the workspace for userID,
// keyed on the unique user_id column, incrementing revision atomically:
// INSERT starts revision at 1, and a conflict (an existing row) does
// revision = revision + 1. The caller has already validated agentIDs and
// frontmostAgentID (pkg/hub/handlers_user_terminal_workspace.go); this store
// does not re-validate them.
//
// Returns the row as read immediately after the write, via a separate
// GetUserTerminalWorkspace call (the upsert and the read are not in a single
// transaction). Under concurrent writers this can echo a revision or list
// from a write that raced with this one; that is harmless under
// last-writer-wins, since the response is informational and the caller does
// not compare against an expected revision.
func (s *UserTerminalWorkspaceStore) PutUserTerminalWorkspace(ctx context.Context, userID string, agentIDs []string, frontmostAgentID string) (*store.UserTerminalWorkspace, error) {
	uid, err := parseUUID(userID)
	if err != nil {
		return nil, err
	}
	if agentIDs == nil {
		agentIDs = []string{}
	}
	now := time.Now()

	create := s.client.UserTerminalWorkspace.Create().
		SetUserID(uid).
		SetAgentIds(agentIDs).
		SetRevision(1).
		SetUpdateTime(now)
	if frontmostAgentID != "" {
		create.SetFrontmostAgentID(frontmostAgentID)
	}

	// The conflict-side Update must reference the values already prepared for
	// the INSERT (via UpdateXXX / SetExcluded) rather than set them again
	// directly: the upsert setter's Set() passes the value straight to the
	// SQL driver with no field-type encoding, so a second, direct
	// u.SetAgentIds(agentIDs) here would hand the driver a raw []string
	// instead of the JSON-encoded value Create() already produced, and the
	// driver has no encoding for a bare slice ("unsupported type []string").
	// UpdateAgentIds()/UpdateFrontmostAgentID() instead emit
	// "col = excluded.col", reusing the properly encoded INSERT value for
	// both branches of the upsert — which is exactly what we want, since the
	// intended value is the same either way.
	err = create.
		OnConflictColumns(userterminalworkspace.FieldUserID).
		Update(func(u *ent.UserTerminalWorkspaceUpsert) {
			u.UpdateAgentIds()
			u.UpdateFrontmostAgentID()
			u.AddRevision(1)
			u.SetUpdateTime(now)
		}).
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("upsert user terminal workspace: %w", err)
	}

	return s.GetUserTerminalWorkspace(ctx, userID)
}
