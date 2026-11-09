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
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// conversionPlanRace is a legacy Runtime Broker row whose conditional plan
// writes are raced: a hook on the RuntimeBroker bulk UPDATE (the form
// SetRuntimeBrokerConversionPlan uses) writes a different stored hash through
// a single-row update just before the store's update runs, as a concurrent
// writer would between the store's read and its update.
type conversionPlanRace struct {
	cs    *CompositeStore
	id    string
	races int // concurrent writes injected so far
}

// newConversionPlanRace makes the row and installs the hook. The hook races
// the first maxRaces store updates and lets later ones through.
func newConversionPlanRace(t *testing.T, maxRaces int) *conversionPlanRace {
	t.Helper()
	ctx := context.Background()
	client := enttest.NewClient(t)
	r := &conversionPlanRace{cs: NewCompositeStore(client), id: uuid.NewString()}
	t.Cleanup(func() { _ = r.cs.Close() })
	require.NoError(t, r.cs.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: r.id, Name: "race", Slug: "race-" + r.id[:8], Status: store.BrokerStatusOnline,
	}))
	uid := uuid.MustParse(r.id)
	client.RuntimeBroker.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if rm, ok := m.(*ent.RuntimeBrokerMutation); ok && rm.Op().Is(ent.OpUpdate) && r.races < maxRaces {
				r.races++
				// A single-row update (OpUpdateOne) passes this hook unchanged.
				if err := client.RuntimeBroker.UpdateOneID(uid).
					SetConversionPlanPresentationHash(fmt.Sprintf("concurrent-%d", r.races)).
					Exec(ctx); err != nil {
					return nil, err
				}
			}
			return next.Mutate(ctx, m)
		})
	})
	return r
}

func racePlan() *api.ConversionPlanPresentation {
	return &api.ConversionPlanPresentation{
		SchemaVersion: api.ConversionPlanPresentationSchemaVersion,
		Instances:     []api.RuntimeTargetPresentation{{SchemaVersion: 1, Key: "docker-1", Name: "docker-1", Target: &api.PresentedTarget{Type: "docker"}}},
		ProfileMap:    map[string]string{"local": "docker-1"},
		Findings:      []api.ConversionPlanFinding{},
		ComputedAt:    time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
	}
}

// TestSetRuntimeBrokerConversionPlan_RetriesAfterConcurrentWrite: when the
// stored hash changes between the store's read and its update, the update
// matches no row; the store reads again and its second update writes.
func TestSetRuntimeBrokerConversionPlan_RetriesAfterConcurrentWrite(t *testing.T) {
	r := newConversionPlanRace(t, 1)
	ctx := context.Background()
	out, err := r.cs.SetRuntimeBrokerConversionPlan(ctx, r.id, racePlan(), "h1", time.Now().UTC())
	require.NoError(t, err)
	assert.Equal(t, store.PresentationWritten, out)
	assert.Equal(t, 1, r.races, "the first update was raced once")
	got, err := r.cs.GetRuntimeBroker(ctx, r.id)
	require.NoError(t, err)
	assert.Equal(t, "h1", got.ConversionPlanHash, "the retry wrote over the concurrent hash")
	require.NotNil(t, got.ConversionPlanPresentation)
	assert.Equal(t, "docker-1", got.ConversionPlanPresentation.Instances[0].Key)
}

// TestSetRuntimeBrokerConversionPlan_VersionConflictAfterRetries: when every
// attempt is raced, the store gives up after maxCASRetries attempts with
// ErrVersionConflict, and its own plan is not written.
func TestSetRuntimeBrokerConversionPlan_VersionConflictAfterRetries(t *testing.T) {
	r := newConversionPlanRace(t, maxCASRetries+10)
	ctx := context.Background()
	_, err := r.cs.SetRuntimeBrokerConversionPlan(ctx, r.id, racePlan(), "h1", time.Now().UTC())
	require.ErrorIs(t, err, store.ErrVersionConflict)
	assert.Equal(t, maxCASRetries, r.races, "one raced update per attempt")
	got, err := r.cs.GetRuntimeBroker(ctx, r.id)
	require.NoError(t, err)
	assert.Nil(t, got.ConversionPlanPresentation, "the store's plan was never written")
	assert.Equal(t, fmt.Sprintf("concurrent-%d", maxCASRetries), got.ConversionPlanHash)
}
