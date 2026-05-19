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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.New(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRegisterGlobalGroveAndBroker_DedupByName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration: creates broker with ID "broker-1" and name "test-broker"
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, "broker-1", "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, "broker-1", effectiveID)

	// Verify broker was created
	broker, err := s.GetRuntimeBroker(ctx, "broker-1")
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, store.BrokerStatusOnline, broker.Status)

	// Second registration with a DIFFERENT ID but SAME name.
	// This simulates a restart where the broker ID was lost/regenerated.
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, "broker-2", "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)

	// Should return the original broker-1 ID (dedup by name)
	assert.Equal(t, "broker-1", effectiveID, "should reuse existing broker ID found by name")

	// Verify no duplicate was created
	_, err = s.GetRuntimeBroker(ctx, "broker-2")
	assert.ErrorIs(t, err, store.ErrNotFound, "broker-2 should NOT exist in the database")

	// Verify original broker was updated
	broker, err = s.GetRuntimeBroker(ctx, "broker-1")
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, store.BrokerStatusOnline, broker.Status)
}

func TestRegisterGlobalGroveAndBroker_SameIDNoDedup(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// First registration
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, "broker-1", "test-broker", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, "broker-1", effectiveID)

	// Second registration with the same ID (normal restart case)
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, "broker-1", "test-broker", "http://localhost:9800", nil, false, settings)
	require.NoError(t, err)
	assert.Equal(t, "broker-1", effectiveID)

	// Verify broker was updated (not duplicated)
	broker, err := s.GetRuntimeBroker(ctx, "broker-1")
	require.NoError(t, err)
	assert.Equal(t, "test-broker", broker.Name)
	assert.Equal(t, false, broker.AutoProvide, "auto-provide should be updated to false")
}

func TestRegisterGlobalGroveAndBroker_NewBrokerNewName(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register first broker
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, "broker-1", "broker-alpha", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, "broker-1", effectiveID)

	// Register a genuinely different broker (different ID AND different name)
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, "broker-2", "broker-beta", "http://localhost:9801", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, "broker-2", effectiveID)

	// Both brokers should exist
	_, err = s.GetRuntimeBroker(ctx, "broker-1")
	assert.NoError(t, err)
	_, err = s.GetRuntimeBroker(ctx, "broker-2")
	assert.NoError(t, err)
}

func TestRegisterGlobalGroveAndBroker_DedupCaseInsensitive(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	settings := &config.Settings{}

	// Register broker with lowercase name
	effectiveID, err := registerGlobalProjectAndBroker(ctx, s, "broker-1", "scion-demo", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, "broker-1", effectiveID)

	// Register with different ID and mixed-case name
	// GetRuntimeBrokerByName uses LOWER() for case-insensitive match
	effectiveID, err = registerGlobalProjectAndBroker(ctx, s, "broker-2", "Scion-Demo", "http://localhost:9800", nil, true, settings)
	require.NoError(t, err)
	assert.Equal(t, "broker-1", effectiveID, "should match case-insensitively")
}
