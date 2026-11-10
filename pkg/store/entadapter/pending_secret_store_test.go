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
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func newPendingSecretInput(key, ownerID, agentID, ref string) *store.PendingSecret {
	return &store.PendingSecret{
		ID:          api.NewUUID(),
		Key:         key,
		SecretType:  store.SecretTypeFile,
		Target:      "~/.config/tool/creds.json",
		Scope:       store.ScopeUser,
		ScopeID:     ownerID,
		ValueRef:    ref,
		ValueLen:    12,
		AgentID:     agentID,
		AgentSlug:   "slug-" + agentID,
		ProjectID:   "project-of-" + agentID,
		ProjectName: "Project of " + agentID,
	}
}

func TestPendingSecretStore_CreateGetListDelete(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	in := newPendingSecretInput("TOOL_AUTH", "owner-1", "agent-a", "ref-1")
	in.AllowProgeny = true
	in.ReplacesSecretID = "secret-id-1"
	in.ReplacesVersion = 3
	res, err := cs.CreateOrReplacePendingSecret(ctx, in, now)
	require.NoError(t, err)
	assert.False(t, res.Replaced)
	assert.Empty(t, res.DroppedValueRefs)
	assert.Equal(t, in.ID, res.Secret.ID)
	assert.True(t, res.Secret.Created.Equal(now))
	assert.True(t, res.Secret.Updated.Equal(now))
	assert.True(t, res.Secret.ExpiresAt.Equal(now.Add(store.PendingSecretTTL)), "expiry is created + 7 days")

	got, err := cs.GetPendingSecret(ctx, in.ID)
	require.NoError(t, err)
	assert.Equal(t, "ref-1", got.ValueRef)
	assert.Equal(t, "TOOL_AUTH", got.Key)
	assert.Equal(t, store.SecretTypeFile, got.SecretType)
	assert.Equal(t, "~/.config/tool/creds.json", got.Target)
	assert.Equal(t, "owner-1", got.ScopeID)
	assert.True(t, got.AllowProgeny)
	assert.Equal(t, 12, got.ValueLen)
	assert.Equal(t, "agent-a", got.AgentID)
	assert.Equal(t, "slug-agent-a", got.AgentSlug)
	assert.Equal(t, "project-of-agent-a", got.ProjectID)
	assert.Equal(t, "Project of agent-a", got.ProjectName)
	assert.Equal(t, "secret-id-1", got.ReplacesSecretID)
	assert.Equal(t, 3, got.ReplacesVersion)
	assert.True(t, got.ExpiresAt.Equal(now.Add(store.PendingSecretTTL)))

	other := newPendingSecretInput("OTHER_KEY", "owner-2", "agent-b", "ref-2")
	_, err = cs.CreateOrReplacePendingSecret(ctx, other, now)
	require.NoError(t, err)

	list, err := cs.ListPendingSecretsByOwner(ctx, store.ScopeUser, "owner-1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, in.ID, list[0].ID)
	assert.Empty(t, list[0].ValueRef, "the list never carries the value ref")

	require.NoError(t, cs.DeletePendingSecret(ctx, in.ID))
	_, err = cs.GetPendingSecret(ctx, in.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, cs.DeletePendingSecret(ctx, in.ID), store.ErrNotFound)
	_, err = cs.GetPendingSecret(ctx, "not-a-uuid")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestPendingSecretStore_OtherAgentRefused(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	now := time.Now()

	first := newPendingSecretInput("TOOL_AUTH", "owner-1", "agent-a", "ref-a")
	_, err := cs.CreateOrReplacePendingSecret(ctx, first, now)
	require.NoError(t, err)

	second := newPendingSecretInput("TOOL_AUTH", "owner-1", "agent-b", "ref-b")
	_, err = cs.CreateOrReplacePendingSecret(ctx, second, now.Add(time.Minute))
	require.ErrorIs(t, err, store.ErrPendingSecretHeldByOtherAgent)

	got, err := cs.GetPendingSecret(ctx, first.ID)
	require.NoError(t, err, "the first agent's row keeps its ID")
	assert.Equal(t, "ref-a", got.ValueRef)
	assert.Equal(t, "agent-a", got.AgentID)
	_, err = cs.GetPendingSecret(ctx, second.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// The same name for another owner is independent.
	third := newPendingSecretInput("TOOL_AUTH", "owner-2", "agent-b", "ref-c")
	_, err = cs.CreateOrReplacePendingSecret(ctx, third, now)
	require.NoError(t, err)
}

func TestPendingSecretStore_SameAgentReplaces(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Second)

	first := newPendingSecretInput("TOOL_AUTH", "owner-1", "agent-a", "ref-old")
	_, err := cs.CreateOrReplacePendingSecret(ctx, first, created)
	require.NoError(t, err)

	later := created.Add(2 * time.Hour)
	second := newPendingSecretInput("TOOL_AUTH", "owner-1", "agent-a", "ref-new")
	second.ValueLen = 40
	res, err := cs.CreateOrReplacePendingSecret(ctx, second, later)
	require.NoError(t, err)
	assert.True(t, res.Replaced)
	assert.Equal(t, []string{"ref-old"}, res.DroppedValueRefs)
	assert.NotEqual(t, first.ID, res.Secret.ID, "a replacement gets a new ID")
	assert.True(t, res.Secret.Created.Equal(created), "created is kept")
	assert.True(t, res.Secret.Updated.Equal(later), "updated is the replacement time")
	assert.True(t, res.Secret.ExpiresAt.Equal(created.Add(store.PendingSecretTTL)), "a replacement does not extend the expiry")

	_, err = cs.GetPendingSecret(ctx, first.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	got, err := cs.GetPendingSecret(ctx, second.ID)
	require.NoError(t, err)
	assert.Equal(t, "ref-new", got.ValueRef)
	assert.Equal(t, 40, got.ValueLen)

	list, err := cs.ListPendingSecretsByOwner(ctx, store.ScopeUser, "owner-1")
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

func TestPendingSecretStore_ExpiredRowIsGone(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Second).Add(-8 * 24 * time.Hour)

	stale := newPendingSecretInput("TOOL_AUTH", "owner-1", "agent-a", "ref-stale")
	_, err := cs.CreateOrReplacePendingSecret(ctx, stale, created)
	require.NoError(t, err)

	// Another agent's write is not refused by an expired row: the row is
	// deleted and the write is a fresh one (new created and expiry).
	now := time.Now().UTC().Truncate(time.Second)
	fresh := newPendingSecretInput("TOOL_AUTH", "owner-1", "agent-b", "ref-fresh")
	res, err := cs.CreateOrReplacePendingSecret(ctx, fresh, now)
	require.NoError(t, err)
	assert.False(t, res.Replaced)
	assert.Equal(t, []string{"ref-stale"}, res.DroppedValueRefs)
	assert.True(t, res.Secret.Created.Equal(now))
	assert.True(t, res.Secret.ExpiresAt.Equal(now.Add(store.PendingSecretTTL)))
	_, err = cs.GetPendingSecret(ctx, stale.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// The same agent's expired row is not a replacement either: the new
	// write starts a new expiry.
	again := newPendingSecretInput("OTHER", "owner-1", "agent-a", "ref-x")
	_, err = cs.CreateOrReplacePendingSecret(ctx, again, created)
	require.NoError(t, err)
	again2 := newPendingSecretInput("OTHER", "owner-1", "agent-a", "ref-y")
	res, err = cs.CreateOrReplacePendingSecret(ctx, again2, now)
	require.NoError(t, err)
	assert.False(t, res.Replaced)
	assert.True(t, res.Secret.Created.Equal(now))
	assert.Equal(t, []string{"ref-x"}, res.DroppedValueRefs)
}

// TestPendingSecretStore_ConcurrentFirstWrites: concurrent first writes by
// different agents for one name resolve to one winner; every loser gets
// ErrPendingSecretHeldByOtherAgent.
func TestPendingSecretStore_ConcurrentFirstWrites(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	now := time.Now()

	const n = 6
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := newPendingSecretInput("RACE_KEY", "owner-1", "agent-"+string(rune('a'+i)), "ref")
			_, errs[i] = cs.CreateOrReplacePendingSecret(ctx, in, now)
		}(i)
	}
	wg.Wait()

	winners := 0
	for _, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrPendingSecretHeldByOtherAgent):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	assert.Equal(t, 1, winners)
	list, err := cs.ListPendingSecretsByOwner(ctx, store.ScopeUser, "owner-1")
	require.NoError(t, err)
	assert.Len(t, list, 1)
}

// TestPendingSecretStore_ConcurrentSameAgentReplaces: concurrent
// replacements by the writing agent each succeed against the row the
// previous one left. One row remains, it keeps the first write's created
// time and expiry, and every replaced value ref is returned exactly once.
func TestPendingSecretStore_ConcurrentSameAgentReplaces(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	created := time.Now().UTC().Truncate(time.Second)

	first := newPendingSecretInput("RACE_KEY", "owner-1", "agent-a", "ref-0")
	_, err := cs.CreateOrReplacePendingSecret(ctx, first, created)
	require.NoError(t, err)

	const n = 6
	results := make([]*store.PendingSecretWriteResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := newPendingSecretInput("RACE_KEY", "owner-1", "agent-a", "ref-"+string(rune('1'+i)))
			results[i], errs[i] = cs.CreateOrReplacePendingSecret(ctx, in, created.Add(time.Minute))
		}(i)
	}
	wg.Wait()

	dropped := map[string]int{}
	written := map[string]bool{}
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.True(t, results[i].Replaced)
		require.Len(t, results[i].DroppedValueRefs, 1)
		dropped[results[i].DroppedValueRefs[0]]++
		written[results[i].Secret.ValueRef] = true
	}

	list, err := cs.ListPendingSecretsByOwner(ctx, store.ScopeUser, "owner-1")
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.True(t, list[0].Created.Equal(created), "created is kept")
	assert.True(t, list[0].ExpiresAt.Equal(created.Add(store.PendingSecretTTL)), "the expiry is kept")
	final, err := cs.GetPendingSecret(ctx, list[0].ID)
	require.NoError(t, err)

	// Every ref but the surviving one was dropped exactly once.
	assert.Equal(t, 1, dropped["ref-0"])
	for ref := range written {
		if ref == final.ValueRef {
			assert.Zero(t, dropped[ref], "the surviving value is not dropped")
		} else {
			assert.Equal(t, 1, dropped[ref], "replaced value %s is dropped once", ref)
		}
	}
	assert.Len(t, dropped, n)
}
