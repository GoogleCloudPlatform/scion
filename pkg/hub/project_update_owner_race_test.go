// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// beforeProjectUpdateStore runs hook once, immediately before the first
// UpdateProject call for projectID is forwarded to the wrapped store. That is
// after the handler has read the row and applied its changes, so the hook
// lands exactly in the read-then-write window of a full-row update.
type beforeProjectUpdateStore struct {
	store.Store
	projectID string
	hook      func()
	fired     bool
}

func (b *beforeProjectUpdateStore) UpdateProject(ctx context.Context, p *store.Project) error {
	if !b.fired && p.ID == b.projectID {
		b.fired = true
		b.hook()
	}
	return b.Store.UpdateProject(ctx, p)
}

// A PATCH /projects/{id} that read the row before an ownership transfer and
// writes it back after the transfer commits must not undo the transfer
// (ptone/scion#2597). Before the fix the full-row UpdateProject wrote the
// stale OwnerID (alice) back over the transferred one (bob).
func TestUpdateProject_ConcurrentTransferOwnershipSurvives(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	wrapped := &beforeProjectUpdateStore{Store: s, projectID: project.ID}
	wrapped.hook = func() {
		rec := doRequestAsUser(t, srv, alice, http.MethodPost,
			"/api/v1/projects/"+project.ID+"/transfer-ownership",
			map[string]string{"newOwnerId": bob.ID})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		got, err := s.GetProject(ctx, project.ID)
		require.NoError(t, err)
		require.Equal(t, bob.ID, got.OwnerID, "precondition: transfer committed inside the PATCH window")
	}
	srv.store = wrapped

	rec := doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/projects/"+project.ID,
		map[string]string{"name": "Renamed During Transfer"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.True(t, wrapped.fired, "the transfer hook must have run between the PATCH read and write")

	got, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed During Transfer", got.Name, "the PATCH itself must still apply")
	assert.Equal(t, bob.ID, got.OwnerID, "a racing PATCH must not write the stale OwnerID back")

	var resp store.Project
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, bob.ID, resp.OwnerID, "the PATCH response must report the stored owner, not the stale read")
}

// A PATCH body that names an owner field is not an ownership API: the field
// is ignored, as it was before ptone/scion#2597, and OwnerID is unchanged.
func TestUpdateProject_OwnerFieldInBodyIsIgnored(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)

	rec := doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/projects/"+project.ID,
		map[string]string{"name": "Renamed", "ownerId": bob.ID, "owner_id": bob.ID})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	got, err := s.GetProject(context.Background(), project.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.Name)
	assert.Equal(t, alice.ID, got.OwnerID, "PATCH must not change OwnerID")
}
