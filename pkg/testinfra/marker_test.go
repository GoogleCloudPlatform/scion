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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/mutationaudit"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newClient(t *testing.T) *ent.Client {
	t.Helper()
	name := strings.ReplaceAll(t.Name(), "/", "_")
	client, err := entc.OpenSQLite("file:"+name+"?mode=memory&cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestMarkAndRead(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()

	_, err := Read(ctx, client)
	assert.ErrorIs(t, err, ErrMarkerAbsent)

	at := time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
	m, err := Mark(ctx, client, MarkInput{
		HubID: "hub-1", Operator: "op", Host: "host-1",
		Attested: json.RawMessage(`{"bucket":"b"}`), Now: at,
	})
	require.NoError(t, err)
	assert.Equal(t, "hub-1", m.HubID)
	assert.Equal(t, "test", m.Class)

	got, err := Read(ctx, client)
	require.NoError(t, err)
	assert.Equal(t, "hub-1", got.HubID)
	assert.Equal(t, "test", got.Class)
	assert.Equal(t, "host-1", got.CreatedByHost)
	assert.True(t, got.CreatedAt.Equal(at))
	assert.JSONEq(t, `{"bucket":"b"}`, string(got.Attested))

	n, err := client.MutationAudit.Query().Where(mutationaudit.MutationType(MarkMutationType)).Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = Mark(ctx, client, MarkInput{HubID: "hub-2", Operator: "op"})
	assert.ErrorIs(t, err, ErrMarkerExists)
	got, err = Read(ctx, client)
	require.NoError(t, err)
	assert.Equal(t, "hub-1", got.HubID, "a refused mark changes nothing")
}

func TestMarkValidatesInput(t *testing.T) {
	client := newClient(t)
	ctx := context.Background()
	for _, id := range []string{"", " hub", "hub "} {
		_, err := Mark(ctx, client, MarkInput{HubID: id})
		assert.Error(t, err, "hub id %q", id)
	}
	_, err := Mark(ctx, client, MarkInput{HubID: "hub-1", Attested: json.RawMessage(`[1]`)})
	assert.ErrorContains(t, err, "JSON object")
	_, err = Read(ctx, client)
	assert.ErrorIs(t, err, ErrMarkerAbsent, "nothing was written")
	n, err := client.MutationAudit.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}
