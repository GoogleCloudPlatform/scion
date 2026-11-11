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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// intersectEffectCeilings keeps what both ceilings allow, treats principal
// as the identity, and refuses any ceiling it cannot read rather than
// returning the other side.
func TestIntersectEffectCeilings(t *testing.T) {
	principal := store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	ab := store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.create", "agent.read"}}
	bc := store.EffectCeiling{
		Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.read", "agent.list"},
		BoundaryKind: string(permissions.BoundaryKindProject), BoundaryProjectID: "p1",
	}

	t.Run("bounded with bounded", func(t *testing.T) {
		got, err := intersectEffectCeilings(ab, bc)
		require.NoError(t, err)
		assert.Equal(t, store.EffectCeilingBounded, got.Kind)
		assert.Equal(t, []string{"agent.read"}, got.PermissionIDs)
		assert.Equal(t, "p1", got.BoundaryProjectID)
	})
	t.Run("principal is the identity", func(t *testing.T) {
		got, err := intersectEffectCeilings(principal, ab)
		require.NoError(t, err)
		assert.Equal(t, ab, got)
		got, err = intersectEffectCeilings(ab, principal)
		require.NoError(t, err)
		assert.Equal(t, ab, got)
		got, err = intersectEffectCeilings(principal, principal)
		require.NoError(t, err)
		assert.Equal(t, principal, got)
	})
	for _, tc := range []struct {
		name string
		c    store.EffectCeiling
	}{
		{"unrecorded", store.EffectCeiling{}},
		{"unknown kind", store.EffectCeiling{Kind: "other"}},
	} {
		t.Run(tc.name+" refused", func(t *testing.T) {
			for _, pair := range [][2]store.EffectCeiling{{tc.c, ab}, {ab, tc.c}, {tc.c, principal}, {principal, tc.c}} {
				got, err := intersectEffectCeilings(pair[0], pair[1])
				assert.Error(t, err)
				assert.Equal(t, store.EffectCeiling{}, got, "a refused intersection returns no ceiling")
			}
		})
	}
}
