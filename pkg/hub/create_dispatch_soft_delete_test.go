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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A dispatch that completes after a soft delete must not clear deleted_at:
// preserveTerminalPhase used to adopt the soft-deleted row's stopped phase
// and StateVersion, so updateAgentAfterDispatch's write (from an in-memory
// agent with a zero DeletedAt) won the CAS and un-deleted the row.
func TestUpdateAgentAfterDispatch_KeepsSoftDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "keep-soft", state.PhaseProvisioning)
	inFlight := mustGetAgent(t, s, agent.ID) // the create's copy, before the delete

	row := mustGetAgent(t, s, agent.ID)
	row.Phase = string(state.PhaseStopped)
	row.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, row))

	inFlight.Phase = string(state.PhaseRunning)
	assert.True(t, srv.preserveTerminalPhase(ctx, inFlight), "a soft-deleted row reports softDeleted")
	_ = srv.updateAgentAfterDispatch(ctx, inFlight)

	got := mustGetAgent(t, s, agent.ID)
	assert.False(t, got.DeletedAt.IsZero(), "deleted_at survives the dispatch write")
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
}
