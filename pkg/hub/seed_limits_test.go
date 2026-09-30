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

// Tests for ptone/scion#2061 P1a: the max_agents_per_broker seed default
// (ptone/scion#2063 item "seed is insert-only").

import (
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestSeedLimitDefinitions_FreshDBSeedsMaxAgentsPerBrokerAt100 verifies that
// a fresh hub seeds max_agents_per_broker at 100, matching ptone's ruling
// (design.md §4.1, 2026-09-29) that the global default stays 100 for now.
func TestSeedLimitDefinitions_FreshDBSeedsMaxAgentsPerBrokerAt100(t *testing.T) {
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	seedLimitDefinitions(ctx, s)

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	require.Equal(t, int64(100), def.DefaultValue)
	require.True(t, def.System)
}

// TestSeedLimitDefinitions_ExistingRowUntouchedByReseed verifies the seed is
// insert-only: it never overwrites a hub's existing max_agents_per_broker
// value, whether that value is the old default (12), a hand-picked value
// (30), or anything else an operator has already set via the admin API.
func TestSeedLimitDefinitions_ExistingRowUntouchedByReseed(t *testing.T) {
	for _, existing := range []int64{12, 30} {
		t.Run(fmt.Sprintf("existing_%d", existing), func(t *testing.T) {
			s, err := newTestStore(":memory:")
			require.NoError(t, err)
			defer func() { _ = s.Close() }()

			ctx := context.Background()

			// Simulate a hub that already has a stored value (e.g. from a
			// build predating this change, or a deliberate admin edit)
			// by inserting the row directly before seeding runs.
			_, err = s.CreateLimitDefinition(ctx, &store.LimitDefinition{
				Name:         store.LimitMaxAgentsPerBroker,
				ResourceType: "agent",
				Unit:         "count",
				Description:  "Maximum concurrently live agents per runtime broker",
				DefaultValue: existing,
				System:       true,
			})
			require.NoError(t, err)

			// Re-seeding (as happens on every hub boot) must not touch it.
			seedLimitDefinitions(ctx, s)

			def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
			require.NoError(t, err)
			require.Equal(t, existing, def.DefaultValue)
		})
	}
}
