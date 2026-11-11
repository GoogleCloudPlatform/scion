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
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildCreateRequest_DispatchesSlugNotName is the regression anchor for
// sending agent.Slug, not agent.Name, as the broker-side create/replay
// identifier (buildCreateRequest). Name is a PATCHable display field that
// can diverge from Slug. This test writes a row with Name != Slug directly
// to the store and confirms buildCreateRequest still dispatches Slug, so
// create/replay dispatch is safe regardless of what Name holds.
func TestBuildCreateRequest_DispatchesSlugNotName(t *testing.T) {
	f := projectAgentAuthzSetup(t)
	ctx := context.Background()

	agent, err := f.store.GetAgent(ctx, f.target.ID)
	require.NoError(t, err)

	// Set Name directly in the store, not through applyAgentUpdate, so the
	// row's Name and Slug diverge.
	const divergentName = "../../legacy-sibling"
	agent.Name = divergentName
	require.NoError(t, f.store.UpdateAgent(ctx, agent))

	dispatcher := NewHTTPAgentDispatcher(f.store, false, slog.Default())
	req, err := dispatcher.buildCreateRequest(ctx, agent, "TestBuildCreateRequest_DispatchesSlugNotName")
	require.NoError(t, err)

	assert.Equal(t, agent.Slug, req.Name,
		"buildCreateRequest must dispatch Slug as the broker-side identifier, not the possibly-divergent Name")
	assert.NotEqual(t, divergentName, req.Name,
		"the divergent Name value must never reach the dispatched request")
}
