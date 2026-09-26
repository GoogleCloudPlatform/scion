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

package storetest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compositeFactory returns a Factory that builds the production-shaped
// CompositeStore: a single Ent-managed database serving every domain. This is
// exactly the single-database layout used by the hub today (see
// cmd/server_foreground.go:initStore), so a green run proves the oracle works
// against the current backend.
//
// The backend (SQLite by default, Postgres under -tags integration with
// SCION_TEST_POSTGRES_URL set) is selected by enttest.NewClient, so the same
// oracle asserts identical observable behavior across both backends.
func compositeFactory(t *testing.T) store.Store {
	t.Helper()

	cs := entadapter.NewCompositeStore(enttest.NewClient(t))
	return cs
}

// TestCompositeStore_CRUDParity runs the full CRUD-parity oracle against the
// current CompositeStore across all ported domains.
func TestCompositeStore_CRUDParity(t *testing.T) {
	storetest.RunStoreSuite(t, compositeFactory)
}

// TestCompositeStore_RuntimeBrokerCursorPagination is a targeted conformance
// case, not part of the generic CRUD-parity oracle above: the oracle's
// pagination category (storetest's internal testPaginate) only checks a
// single bounded page, it does not thread ListOptions.Cursor across pages.
// This proves ListRuntimeBrokers' keyset pagination enumerates every broker
// across pages — including the oldest — on whichever backend compositeFactory
// selects (SQLite by default, Postgres under -tags integration).
//
// Before the underlying fix, ListRuntimeBrokers silently ignored
// ListOptions.Cursor and never set NextCursor, so a paginating caller only
// ever saw the first page of newest brokers. A caller that must reach the
// oldest rows — exactly what a legacy-data backfill does — never would.
func TestCompositeStore_RuntimeBrokerCursorPagination(t *testing.T) {
	s := compositeFactory(t)
	ctx := context.Background()

	const total = 12
	const pageSize = 5
	created := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		id := uuid.NewString()
		name := fmt.Sprintf("broker-%d-%s", i, id[:8])
		require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: id, Name: name, Slug: name}))
		created[id] = true
	}

	first, err := s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: pageSize})
	require.NoError(t, err)
	assert.LessOrEqual(t, len(first.Items), pageSize, "page must cap at requested limit")
	assert.NotEmpty(t, first.NextCursor, "more pages exist, so NextCursor must be set")

	seen := make(map[string]bool, total)
	cursor := ""
	for pages := 0; ; pages++ {
		require.LessOrEqual(t, pages, total, "pagination did not terminate")
		page, err := s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{Limit: pageSize, Cursor: cursor})
		require.NoError(t, err)
		for _, b := range page.Items {
			require.False(t, seen[b.ID], "duplicate broker across pages: %s", b.ID)
			seen[b.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	assert.Len(t, seen, total, "cursor pagination must enumerate every broker")
	for id := range created {
		assert.True(t, seen[id], "broker missing from pagination: %s", id)
	}
}
