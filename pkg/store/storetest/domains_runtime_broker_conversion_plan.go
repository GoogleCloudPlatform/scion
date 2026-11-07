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

package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func conversionPlanFor(key string, at time.Time) *api.ConversionPlanPresentation {
	return &api.ConversionPlanPresentation{
		SchemaVersion: api.ConversionPlanPresentationSchemaVersion,
		Instances:     []api.RuntimeTargetPresentation{{SchemaVersion: 1, Key: key, Name: key, Target: &api.PresentedTarget{Type: "docker"}}},
		ProfileMap:    map[string]string{"local": key},
		Findings:      []api.ConversionPlanFinding{{Code: "profile_conflict", Severity: "notice", Path: "profiles.*", Profiles: []string{"a", "b"}}},
		ComputedAt:    at,
	}
}

// RuntimeBrokerConversionPlanConformance checks SetRuntimeBrokerConversionPlan
// and that no full-row writer touches its columns, on whichever backend
// factory selects.
func RuntimeBrokerConversionPlanConformance(t *testing.T, factory Factory) {
	ctx := context.Background()

	t.Run("WriteConfirmAndObservedRefresh", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		p := conversionPlanFor("docker-1", f.t0)
		out, err := f.s.SetRuntimeBrokerConversionPlan(ctx, f.legacy.ID, p, "h1", f.at(0))
		require.NoError(t, err)
		require.Equal(t, store.PresentationWritten, out)
		got, err := f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ConversionPlanPresentation)
		assert.Equal(t, p.Instances, got.ConversionPlanPresentation.Instances)
		assert.Equal(t, p.ProfileMap, got.ConversionPlanPresentation.ProfileMap)
		assert.Equal(t, p.Findings, got.ConversionPlanPresentation.Findings)
		assert.True(t, p.ComputedAt.Equal(got.ConversionPlanPresentation.ComputedAt))
		assert.Equal(t, "h1", got.ConversionPlanHash)
		require.NotNil(t, got.ConversionPlanObservedAt)
		assert.True(t, f.at(0).Equal(*got.ConversionPlanObservedAt))

		out, err = f.s.SetRuntimeBrokerConversionPlan(ctx, f.legacy.ID, p, "h1", f.at(1))
		require.NoError(t, err)
		require.Equal(t, store.PresentationConfirmed, out)
		got, err = f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		assert.True(t, f.at(0).Equal(*got.ConversionPlanObservedAt), "observed-at not refreshed within the period")

		later := f.t0.Add(store.PresentationObservedRefresh + time.Second)
		out, err = f.s.SetRuntimeBrokerConversionPlan(ctx, f.legacy.ID, p, "h1", later)
		require.NoError(t, err)
		require.Equal(t, store.PresentationConfirmed, out)
		got, err = f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		assert.True(t, later.Equal(*got.ConversionPlanObservedAt), "observed-at refreshed after the period")

		p2 := conversionPlanFor("docker-2", later)
		out, err = f.s.SetRuntimeBrokerConversionPlan(ctx, f.legacy.ID, p2, "h2", later.Add(presentationInterval))
		require.NoError(t, err)
		require.Equal(t, store.PresentationWritten, out)
		got, err = f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		assert.Equal(t, "docker-2", got.ConversionPlanPresentation.Instances[0].Key)
		assert.Equal(t, "h2", got.ConversionPlanHash)
	})

	t.Run("FlatRowRefusedAndUnwritten", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		_, err := f.s.SetRuntimeBrokerConversionPlan(ctx, f.flat.ID, conversionPlanFor("x", f.t0), "h1", f.at(0))
		require.ErrorIs(t, err, store.ErrRuntimeBrokerNotLegacy)
		got := f.row(t)
		assert.Nil(t, got.ConversionPlanPresentation)
		assert.Empty(t, got.ConversionPlanHash)
		assert.Nil(t, got.ConversionPlanObservedAt)
	})

	t.Run("MissingRow", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		_, err := f.s.SetRuntimeBrokerConversionPlan(ctx, uuid.NewString(), conversionPlanFor("x", f.t0), "h1", f.at(0))
		require.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("FullRowWritersKeepIt", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		_, err := f.s.SetRuntimeBrokerConversionPlan(ctx, f.legacy.ID, conversionPlanFor("docker-1", f.t0), "h1", f.at(0))
		require.NoError(t, err)

		// A copy read before a newer write, written back by the
		// full-row writer, must not roll the plan back.
		stale, err := f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		_, err = f.s.SetRuntimeBrokerConversionPlan(ctx, f.legacy.ID, conversionPlanFor("docker-2", f.t0), "h2", f.at(1))
		require.NoError(t, err)
		stale.ConversionPlanPresentation, stale.ConversionPlanHash = nil, ""
		stale.Name = stale.Name + "-renamed"
		require.NoError(t, f.s.UpdateRuntimeBroker(ctx, stale))
		require.NoError(t, f.s.UpdateRuntimeBrokerHeartbeat(ctx, f.legacy.ID, store.BrokerStatusOnline))

		got, err := f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ConversionPlanPresentation)
		assert.Equal(t, "docker-2", got.ConversionPlanPresentation.Instances[0].Key)
		assert.Equal(t, "h2", got.ConversionPlanHash)
	})

	t.Run("NeverSerializedWithTheBroker", func(t *testing.T) {
		f := newPresentationFixture(t, factory)
		_, err := f.s.SetRuntimeBrokerConversionPlan(ctx, f.legacy.ID, conversionPlanFor("docker-1", f.t0), "h1", f.at(0))
		require.NoError(t, err)
		got, err := f.s.GetRuntimeBroker(ctx, f.legacy.ID)
		require.NoError(t, err)
		b, err := json.Marshal(got)
		require.NoError(t, err)
		assert.NotContains(t, string(b), "docker-1")
		assert.NotContains(t, string(b), "conversionPlan")
	})
}
