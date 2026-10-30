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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newRecordsTestBroker(t *testing.T, ctx context.Context, s *ProjectStore, id, name string) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:       id,
		Name:     name,
		Slug:     name,
		Status:   store.BrokerStatusOnline,
		Endpoint: "https://old.example.test",
		Profiles: []store.BrokerProfile{{Name: "old-profile", Type: "docker", Available: true}},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	return b
}

// A re-registration that commits between the heartbeat refresh's read and
// its write is kept: the refresh is re-applied to the re-registered row and
// writes only its own columns (ptone/scion#3169).
func TestRefreshRuntimeBrokerState_KeepsConcurrentRegistration(t *testing.T) {
	ctx := context.Background()
	s := NewProjectStore(enttest.NewClient(t))
	b := newRecordsTestBroker(t, ctx, s, "44444444-4444-4444-4444-444444444444", "refresh-race")

	calls := 0
	err := s.RefreshRuntimeBrokerState(ctx, b.ID, func(cur *store.RuntimeBroker) bool {
		calls++
		if calls == 1 {
			// The concurrent re-registration lands after this read.
			reg, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			reg.Profiles = []store.BrokerProfile{{Name: "new-a", Type: "docker", Available: true}, {Name: "new-b", Type: "kubernetes", Available: true}}
			reg.Endpoint = "https://new.example.test"
			require.NoError(t, s.UpdateRuntimeBroker(ctx, reg))
		}
		cur.Capabilities = &store.BrokerCapabilities{Attach: true}
		// Only the heartbeat columns are written; a change to any other
		// field is not stored.
		cur.Name = "renamed-by-refresh"
		return true
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls, "the write conflicts once and is retried on a fresh read")

	got, err := s.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.Len(t, got.Profiles, 2, "the re-registered profile set must survive the heartbeat write")
	assert.Equal(t, "new-a", got.Profiles[0].Name)
	assert.Equal(t, "new-b", got.Profiles[1].Name)
	assert.Equal(t, "https://new.example.test", got.Endpoint, "a column the heartbeat does not refresh keeps the concurrent write")
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.Attach, "the heartbeat's own change is stored")
	assert.Equal(t, "refresh-race", got.Name, "a field outside the heartbeat columns is never written")
}

// A refresh that reports no change writes nothing.
func TestRefreshRuntimeBrokerState_NoChangeNoWrite(t *testing.T) {
	ctx := context.Background()
	s := NewProjectStore(enttest.NewClient(t))
	b := newRecordsTestBroker(t, ctx, s, "55555555-5555-5555-5555-555555555555", "refresh-nochange")
	before, err := s.client.RuntimeBroker.Get(ctx, uuid.MustParse(b.ID))
	require.NoError(t, err)

	require.NoError(t, s.RefreshRuntimeBrokerState(ctx, b.ID, func(cur *store.RuntimeBroker) bool {
		cur.Capabilities = &store.BrokerCapabilities{Attach: true}
		return false
	}))

	after, err := s.client.RuntimeBroker.Get(ctx, before.ID)
	require.NoError(t, err)
	assert.Equal(t, before.LockVersion, after.LockVersion, "no write when refresh reports no change")
	got, err := s.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Capabilities)
}

// A missing broker is ErrNotFound and refresh is never called.
func TestRefreshRuntimeBrokerState_NotFound(t *testing.T) {
	ctx := context.Background()
	s := NewProjectStore(enttest.NewClient(t))
	err := s.RefreshRuntimeBrokerState(ctx, "66666666-6666-6666-6666-666666666666", func(*store.RuntimeBroker) bool {
		t.Fatal("refresh must not run for a missing broker")
		return false
	})
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// The heartbeat refresh stores the broker's build token (Version). A
// refresh that leaves Version alone keeps a version a concurrent
// re-registration stored, because it writes the value of its fresh read,
// and a version set on a stale read is not stored (ptone/scion#5972).
func TestRefreshRuntimeBrokerState_Version(t *testing.T) {
	ctx := context.Background()
	s := NewProjectStore(enttest.NewClient(t))
	b := newRecordsTestBroker(t, ctx, s, "77777777-7777-7777-7777-777777777777", "refresh-version")

	require.NoError(t, s.RefreshRuntimeBrokerState(ctx, b.ID, func(cur *store.RuntimeBroker) bool {
		cur.Version = "1.4.0"
		return true
	}))
	got, err := s.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "1.4.0", got.Version, "the refresh stores the version it sets")

	calls := 0
	require.NoError(t, s.RefreshRuntimeBrokerState(ctx, b.ID, func(cur *store.RuntimeBroker) bool {
		calls++
		if calls == 1 {
			// A re-registration with a newer build lands after this read.
			reg, err := s.GetRuntimeBroker(ctx, b.ID)
			require.NoError(t, err)
			reg.Version = "1.5.0"
			require.NoError(t, s.UpdateRuntimeBroker(ctx, reg))
			// A version set on the stale copy is never stored: the
			// write conflicts and the refresh runs again on the fresh row.
			cur.Version = "stale"
		}
		cur.Capabilities = &store.BrokerCapabilities{Attach: true}
		return true
	}))
	assert.Equal(t, 2, calls, "the write conflicts once and is retried on a fresh read")
	got, err = s.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "1.5.0", got.Version, "a refresh that does not set the version keeps the concurrent one")
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.Attach)
}
