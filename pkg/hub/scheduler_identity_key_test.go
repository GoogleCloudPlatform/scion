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
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestScheduledDispatch_WritesSlugIdentityKey mirrors
// TestCreateAgentInProject_WritesSlugIdentityKey for the scheduler's
// dispatch_agent create path: it must go through the same
// commitAgentCreate transaction as the HTTP create path, so a
// successful dispatch also writes an identity-key row for the new agent's
// Slug.
func TestScheduledDispatch_WritesSlugIdentityKey(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()

	require.NoError(t, fireScheduledDispatchAsOwner(t, f, "sched-fresh-recruit"))

	created, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "sched-fresh-recruit")
	require.NoError(t, err)

	keys, err := f.store.ListAgentIdentityKeys(ctx, f.proj.ID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, created.ID, "sched-fresh-recruit"),
		"expected an identity-key row for the scheduled agent's slug")
}

// TestScheduledDispatch_RejectsReservedSlug mirrors
// TestCreateAgentInProject_RejectsReservedSlug: a dispatch_agent event whose
// agentName is a reserved word must fail, and no agent may be created. The
// failure must specifically be the display-name validation, not some other
// error the dispatch path happens to also produce.
func TestScheduledDispatch_RejectsReservedSlug(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()

	err := fireScheduledDispatchAsOwner(t, f, "admin")
	require.Error(t, err)
	require.True(t, errors.Is(err, errInvalidDisplayName),
		"expected the reserved-word validation error, got: %v", err)

	_, getErr := f.store.GetAgentBySlug(ctx, f.proj.ID, "admin")
	require.ErrorIs(t, getErr, store.ErrNotFound, "no agent may have been created")
}

// TestScheduledDispatch_OrderingCollisionIsRejected mirrors
// TestCreateAgentInProject_OrderingCollisionIsRejected for the scheduler
// path: renaming an existing agent to a display name reserves that name's
// key, so a dispatch_agent event whose agentName resolves to a Slug equal to
// that same key must also be rejected, and no second agent created. The
// failure must specifically be the identity-key conflict, not some other
// error the dispatch path happens to also produce.
func TestScheduledDispatch_OrderingCollisionIsRejected(t *testing.T) {
	f := bypassAgentsSetup(t)
	ctx := context.Background()

	renameRec := doRequestAsUser(t, f.srv, f.owner, http.MethodPatch,
		"/api/v1/projects/"+f.proj.ID+"/agents/"+f.caller.ID,
		map[string]interface{}{"name": "Widget Bot"})
	require.Equal(t, http.StatusOK, renameRec.Code, "PATCH body: %s", renameRec.Body.String())

	err := fireScheduledDispatchAsOwner(t, f, "widget-bot")
	require.Error(t, err)
	require.True(t, errors.Is(err, store.ErrIdentityKeyConflict),
		"expected the identity-key conflict error, got: %v", err)

	_, getErr := f.store.GetAgentBySlug(ctx, f.proj.ID, "widget-bot")
	require.ErrorIs(t, getErr, store.ErrNotFound, "no second agent may have been created")
}
