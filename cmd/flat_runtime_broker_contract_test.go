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

package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Frozen group F tests of the flat Runtime Broker contract for the embedded
// legacy registration (.design/flat-runtime-brokers-contract.md section 6,
// R4 and the writer table). Wired by P1.2 (ptone/scion#3268); the
// assertions are unchanged.

func createFlatRuntimeBrokerRow(t *testing.T, s store.Store, id, name, slug string) *store.RuntimeBroker {
	t.Helper()
	ctx := context.Background()
	b := &store.RuntimeBroker{
		ID:     id,
		Name:   name,
		Slug:   slug,
		Status: store.BrokerStatusOffline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{
			ID: tid("flat-target-" + name), Type: "docker", DisplayName: "Local Docker",
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	got, err := s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	require.True(t, got.IsFlat())
	return got
}

func assertFlatRowUntouched(t *testing.T, s store.Store, before *store.RuntimeBroker) {
	t.Helper()
	after, err := s.GetRuntimeBroker(context.Background(), before.ID)
	require.NoError(t, err)
	require.NotNil(t, after.RuntimeTarget, "the flat row must stay flat")
	assert.Equal(t, *before.RuntimeTarget, *after.RuntimeTarget)
	assert.Equal(t, before.Name, after.Name)
	assert.Equal(t, before.Slug, after.Slug)
	assert.Empty(t, after.Profiles, "no profiles may be written onto a flat row")
	assert.Empty(t, after.DefaultProfile)
	assert.NotEqual(t, "embedded", after.Labels["scion.io/broker-role"], "the legacy embedded registration must not adopt the flat row")
}

func TestLegacyRegistration_NameCollidingWithFlatRowRefused_Embedded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	flat := createFlatRuntimeBrokerRow(t, s, tid("flat-broker"), "test-broker", "test-broker")
	settings := &config.Settings{}

	// A legacy embedded registration with a new ID whose name collides with
	// the flat row (case-insensitively) is a startup error: no duplicate row
	// is created next to the flat row.
	for _, name := range []string{"test-broker", "Test-Broker"} {
		_, err := registerGlobalProjectAndBroker(ctx, s, tid("legacy-new-"+name), name, "http://localhost:9800", nil, true, settings, nil)
		require.Error(t, err, "name %q collides with a flat row", name)
		assert.Contains(t, err.Error(), "runtime_broker_name_conflict")
		_, getErr := s.GetRuntimeBroker(ctx, tid("legacy-new-"+name))
		assert.ErrorIs(t, getErr, store.ErrNotFound, "no legacy row may be created")
	}
	assertFlatRowUntouched(t, s, flat)

	// A slug-only collision is refused too (R4: name or slug): "test broker"
	// differs from the flat row's name but slugifies to its slug.
	require.Equal(t, flat.Slug, api.Slugify("test broker"))
	_, err := registerGlobalProjectAndBroker(ctx, s, tid("legacy-new-slug"), "test broker", "http://localhost:9800", nil, true, settings, nil)
	require.Error(t, err, "slug collides with a flat row")
	assert.Contains(t, err.Error(), "runtime_broker_name_conflict")
	_, getErr := s.GetRuntimeBroker(ctx, tid("legacy-new-slug"))
	assert.ErrorIs(t, getErr, store.ErrNotFound, "no legacy row may be created")
	assertFlatRowUntouched(t, s, flat)

	// An ID match on the flat row is also a startup error (R4).
	_, err = registerGlobalProjectAndBroker(ctx, s, flat.ID, "test-broker", "http://localhost:9800", nil, true, settings, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime_target_changed")
	assertFlatRowUntouched(t, s, flat)
}

func TestLegacyEmbeddedRegistration_SkipsFlatRowByName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	// The flat row is created first, so a plain first-match name lookup
	// would find it; the filtered lookup must return the legacy row.
	flat := createFlatRuntimeBrokerRow(t, s, tid("flat-broker"), "shared-name", "shared-name")
	legacy := &store.RuntimeBroker{ID: tid("legacy-broker"), Name: "Shared-Name", Slug: "shared-name-legacy", Status: store.BrokerStatusOffline}
	require.NoError(t, s.CreateRuntimeBroker(ctx, legacy))

	settings := &config.Settings{}
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, tid("legacy-restart"), "shared-name", "http://localhost:9800", nil, true, settings, nil)
	require.NoError(t, err)
	assert.Equal(t, legacy.ID, effectiveID, "the legacy row is adopted by name exactly as today")

	_, err = s.GetRuntimeBroker(ctx, tid("legacy-restart"))
	assert.ErrorIs(t, err, store.ErrNotFound, "no duplicate row is created")
	assertFlatRowUntouched(t, s, flat)

	adopted, err := s.GetRuntimeBroker(ctx, legacy.ID)
	require.NoError(t, err)
	assert.Nil(t, adopted.RuntimeTarget, "the adopted row stays legacy")
	assert.Equal(t, store.BrokerStatusOnline, adopted.Status)
}
