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

package testinfra

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/mutationaudit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DEC-8 (1): mark generates a 64-byte key from crypto/rand, unique per
// mark, and ReadWebSessionKey returns it for the marker's own hub id.
func TestMark_GeneratesWebSessionKey(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()
	_, err := Mark(ctx, client, MarkInput{HubID: "hub-1", Operator: "op", Host: "h"})
	require.NoError(t, err)

	key, err := ReadWebSessionKey(ctx, client, "hub-1")
	require.NoError(t, err)
	assert.Len(t, key, WebSessionKeyLen)
	assert.False(t, bytes.Equal(key, make([]byte, WebSessionKeyLen)))
	// Not derived from portable inputs: nothing of the hub id, host or
	// operator appears in it, and a second database marked with the same
	// inputs gets a different key.
	other := newClientNamed(t, "other")
	_, err = Mark(ctx, other, MarkInput{HubID: "hub-1", Operator: "op", Host: "h"})
	require.NoError(t, err)
	key2, err := ReadWebSessionKey(ctx, other, "hub-1")
	require.NoError(t, err)
	assert.False(t, bytes.Equal(key, key2), "the same inputs must not give the same key")
}

// DEC-8 (2): the key is never part of Marker, the audit row, or any
// printed form of the row.
func TestMark_WebSessionKeyNeverSurfaced(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()
	m, err := Mark(ctx, client, MarkInput{HubID: "hub-1", Operator: "op", Host: "h"})
	require.NoError(t, err)
	key, err := ReadWebSessionKey(ctx, client, "hub-1")
	require.NoError(t, err)

	read, err := Read(ctx, client)
	require.NoError(t, err)
	row, err := client.TestInfraMarker.Get(ctx, MarkerID)
	require.NoError(t, err)
	audits, err := client.MutationAudit.Query().All(ctx)
	require.NoError(t, err)

	for _, s := range []string{fmt.Sprintf("%+v", m), fmt.Sprintf("%+v", read), row.String(), fmt.Sprintf("%v", audits)} {
		assert.NotContains(t, s, string(key))
		assert.NotContains(t, s, fmt.Sprintf("%x", key))
		assert.NotContains(t, s, fmt.Sprintf("%v", key))
	}
}

// DEC-8 (4): unmark destroys the key with the row; marking again generates
// a different one.
func TestUnmark_DestroysAndRemarkRegeneratesKey(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()
	_, err := Mark(ctx, client, MarkInput{HubID: "hub-1", Operator: "op"})
	require.NoError(t, err)
	k1, err := ReadWebSessionKey(ctx, client, "hub-1")
	require.NoError(t, err)

	deleted, err := Unmark(ctx, client, UnmarkInput{Operator: "op", Host: "h", Now: time.Now()})
	require.NoError(t, err)
	assert.Equal(t, "hub-1", deleted.HubID)
	_, err = ReadWebSessionKey(ctx, client, "hub-1")
	assert.ErrorIs(t, err, ErrMarkerAbsent)
	n, err := client.MutationAudit.Query().Where(mutationaudit.MutationType(UnmarkMutationType)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = Unmark(ctx, client, UnmarkInput{Operator: "op"})
	assert.ErrorIs(t, err, ErrMarkerAbsent)

	_, err = Mark(ctx, client, MarkInput{HubID: "hub-1", Operator: "op"})
	require.NoError(t, err)
	k2, err := ReadWebSessionKey(ctx, client, "hub-1")
	require.NoError(t, err)
	assert.False(t, bytes.Equal(k1, k2), "re-mark must generate a new key")
}

// DEC-8 (5): every unusable state is a named error, never a key.
func TestReadWebSessionKey_FailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("absent", func(t *testing.T) {
		_, err := ReadWebSessionKey(ctx, newClient(t), "hub-1")
		assert.ErrorIs(t, err, ErrMarkerAbsent)
	})
	t.Run("other hub", func(t *testing.T) {
		client := newClient(t)
		_, err := Mark(ctx, client, MarkInput{HubID: "hub-1", Operator: "op"})
		require.NoError(t, err)
		_, err = ReadWebSessionKey(ctx, client, "hub-2")
		assert.ErrorIs(t, err, ErrWebSessionKeyHubMismatch)
		_, err = ReadWebSessionKey(ctx, client, "")
		assert.ErrorIs(t, err, ErrWebSessionKeyHubMismatch)
	})
	t.Run("marked before keys existed", func(t *testing.T) {
		client := newClient(t)
		_, err := client.TestInfraMarker.Create().SetID(MarkerID).SetHubID("hub-1").SetClass("test").SetCreatedAt(time.Now()).Save(ctx)
		require.NoError(t, err)
		_, err = ReadWebSessionKey(ctx, client, "hub-1")
		assert.ErrorIs(t, err, ErrWebSessionKeyMissing)
	})
	for name, key := range map[string][]byte{
		"short":    bytes.Repeat([]byte{1}, 32),
		"long":     bytes.Repeat([]byte{1}, 65),
		"all zero": make([]byte, WebSessionKeyLen),
	} {
		t.Run("corrupt "+name, func(t *testing.T) {
			client := newClient(t)
			_, err := client.TestInfraMarker.Create().SetID(MarkerID).SetHubID("hub-1").SetClass("test").SetCreatedAt(time.Now()).SetWebSessionKey(key).Save(ctx)
			require.NoError(t, err)
			_, err = ReadWebSessionKey(ctx, client, "hub-1")
			assert.ErrorIs(t, err, ErrWebSessionKeyInvalid)
		})
	}
	t.Run("class not test", func(t *testing.T) {
		client := newClient(t)
		_, err := client.TestInfraMarker.Create().SetID(MarkerID).SetHubID("hub-1").SetClass("prod").SetCreatedAt(time.Now()).SetWebSessionKey(bytes.Repeat([]byte{1}, WebSessionKeyLen)).Save(ctx)
		require.NoError(t, err)
		_, err = ReadWebSessionKey(ctx, client, "hub-1")
		assert.ErrorIs(t, err, ErrWebSessionKeyClass)
	})
	t.Run("unreadable", func(t *testing.T) {
		client := newClient(t)
		require.NoError(t, client.Close())
		_, err := ReadWebSessionKey(ctx, client, "hub-1")
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrMarkerAbsent))
	})
}

// A failing random source fails the mark and writes nothing.
func TestMark_RandomFailureWritesNothing(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()
	orig := randRead
	t.Cleanup(func() { randRead = orig })
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }

	_, err := Mark(ctx, client, MarkInput{HubID: "hub-1", Operator: "op"})
	require.Error(t, err)
	_, err = Read(ctx, client)
	assert.ErrorIs(t, err, ErrMarkerAbsent)

	randRead = func(b []byte) (int, error) { return len(b), nil } // all zeros
	_, err = Mark(ctx, client, MarkInput{HubID: "hub-1", Operator: "op"})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "zeros"))
}

// newClientNamed opens a second, separate in-memory database for the test.
func newClientNamed(t *testing.T, suffix string) *ent.Client {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_") + "_" + suffix
	client, err := entc.OpenSQLite("file:"+name+"?mode=memory&cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	t.Cleanup(func() { _ = client.Close() })
	return client
}
