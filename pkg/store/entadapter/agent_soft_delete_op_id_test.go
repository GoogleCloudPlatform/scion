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

package entadapter

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SoftDeleteOpID round-trips through UpdateAgent and GetAgent, and an empty
// value clears the column.
func TestAgentSoftDeleteOpIDRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "soft-op-id")
	require.NoError(t, s.CreateAgent(ctx, a))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.SoftDeleteOpID, "unset on create")

	got.SoftDeleteOpID = "op-1"
	require.NoError(t, s.UpdateAgent(ctx, got))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "op-1", got.SoftDeleteOpID)

	got.SoftDeleteOpID = ""
	require.NoError(t, s.UpdateAgent(ctx, got))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.SoftDeleteOpID, "an empty value clears the column")
}
