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

// Package storetest provides a backend-agnostic, table-driven CRUD-parity test
// oracle for implementations of store.Store.
//
// The harness is the cornerstone of the Postgres integration effort: every
// store backend (SQLite today, Postgres later) must produce identical
// observable results for the same operations. Rather than re-write the same
// CRUD assertions per backend, a test provides a Factory that returns a fresh,
// migrated store.Store, and the harness drives the standardized test categories
// against it:
//
//   - Create:        insert an entity, verify the returned/persisted fields.
//   - Read:          get by ID, verify all fields; missing ID -> ErrNotFound.
//   - Update:        modify fields, verify the change is persisted.
//   - Delete:        delete an entity, verify it is excluded from the default
//     list and Get returns ErrNotFound. For domains that support
//     soft-delete, additionally verify it is still returned when
//     deleted entities are explicitly included.
//   - List-paginate: insert N entities, verify limit/pagination behavior.
//   - List-filter:   verify filtering returns only matching entities.
//
// Each entity type is described by a generic Domain[T]. Because every domain
// has different method signatures on store.Store, the Domain captures each
// operation as a closure. This keeps the harness itself entity-agnostic while
// letting new domains be onboarded by adding a single Domain descriptor (see
// domains.go for the group and policy examples).
package storetest

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Factory builds a fresh, migrated store.Store for a single test. It receives
// the subtest's *testing.T so the implementation can register cleanup (closing
// connections, dropping temp databases) via t.Cleanup. Each test category gets
// its own store so cases are isolated from one another.
type Factory func(t *testing.T) store.Store

// Domain describes how to exercise one entity type (T) through the standardized
// CRUD test categories. Required fields must be set; optional fields enable
// additional categories when the entity supports them.
type Domain[T any] struct {
	// Name is the entity name, used as the subtest group name (e.g. "group").
	Name string

	// Make builds a fresh, valid entity. seq is a monotonically increasing
	// counter the implementation should weave into unique fields (slug, name)
	// so that many entities can be created in the same store without collisions.
	Make func(seq int) *T

	// GetID returns the primary identifier used for Get/Delete.
	GetID func(*T) string

	// Prepare, when non-nil, runs once against each fresh store before a test
	// category exercises it. It seeds prerequisite rows that entities of this
	// domain depend on (e.g. the project an agent references via a required
	// foreign key). It must be idempotent with respect to a fresh store.
	Prepare func(t *testing.T, ctx context.Context, s store.Store)

	// Create persists a new entity.
	Create func(ctx context.Context, s store.Store, e *T) error

	// Get retrieves an entity by ID. It must return store.ErrNotFound when the
	// entity does not exist.
	Get func(ctx context.Context, s store.Store, id string) (*T, error)

	// List returns entities honoring ListOptions (in particular Limit). For the
	// Delete and Paginate categories this is the default, non-filtered listing.
	List func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[T], error)

	// VerifyEqual asserts that a freshly-read entity (got) matches the one that
	// was created (want). Implementations compare the fields they care about.
	VerifyEqual func(t *testing.T, want, got *T)

	// --- Optional: Update category ---

	// Mutate applies an in-place modification used to verify Update persists.
	// When nil (together with Update), the Update category is skipped.
	Mutate func(e *T)

	// Update persists modifications to an existing entity.
	Update func(ctx context.Context, s store.Store, e *T) error

	// VerifyMutated asserts that the change applied by Mutate is present on a
	// freshly-read entity.
	VerifyMutated func(t *testing.T, got *T)

	// --- Optional: Delete category ---

	// Delete removes an entity by ID. When nil, the Delete category is skipped.
	Delete func(ctx context.Context, s store.Store, id string) error

	// SoftDelete, when non-nil, marks the domain as soft-deleting: after Delete
	// the entity is excluded from the default List but must still be returned
	// when deleted entities are explicitly included.
	SoftDelete *SoftDeleteSpec[T]

	// --- Optional: List-filter category ---

	// Filters enumerates filter scenarios to verify. When empty, the
	// List-filter category is skipped.
	Filters []FilterCase[T]
}

// SoftDeleteSpec captures the extra behavior of domains that soft-delete rather
// than hard-delete. ListIncludeDeleted must list entities including any that
// have been soft-deleted.
type SoftDeleteSpec[T any] struct {
	ListIncludeDeleted func(ctx context.Context, s store.Store, opts store.ListOptions) (*store.ListResult[T], error)
}

// FilterCase describes one List-filter scenario. Seed inserts a known mix of
// entities into a fresh store; List applies the filter under test; WantCount is
// the number of entities expected to match.
type FilterCase[T any] struct {
	Name      string
	Seed      func(t *testing.T, ctx context.Context, s store.Store)
	List      func(ctx context.Context, s store.Store) (*store.ListResult[T], error)
	WantCount int
}

// RunDomain runs every applicable test category for a single domain against
// stores produced by factory. Each category obtains its own fresh store.
func RunDomain[T any](t *testing.T, factory Factory, d Domain[T]) {
	t.Helper()
	t.Run(d.Name, func(t *testing.T) {
		t.Run("Create", func(t *testing.T) { testCreate(t, factory, d) })
		t.Run("Read", func(t *testing.T) { testRead(t, factory, d) })

		if d.Update != nil && d.Mutate != nil {
			t.Run("Update", func(t *testing.T) { testUpdate(t, factory, d) })
		}
		if d.Delete != nil {
			t.Run("Delete", func(t *testing.T) { testDelete(t, factory, d) })
		}
		if d.List != nil {
			t.Run("ListPaginate", func(t *testing.T) { testPaginate(t, factory, d) })
		}
		if len(d.Filters) > 0 {
			t.Run("ListFilter", func(t *testing.T) { testFilter(t, factory, d) })
		}
	})
}

// prepareStore runs the domain's Prepare hook (if any) against a fresh store.
func prepareStore[T any](t *testing.T, ctx context.Context, s store.Store, d Domain[T]) {
	if d.Prepare != nil {
		d.Prepare(t, ctx, s)
	}
}

func testCreate[T any](t *testing.T, factory Factory, d Domain[T]) {
	ctx := context.Background()
	s := factory(t)
	prepareStore(t, ctx, s, d)

	e := d.Make(1)
	require.NoError(t, d.Create(ctx, s, e), "Create should succeed")

	// The created entity must be retrievable and have the fields we set.
	got, err := d.Get(ctx, s, d.GetID(e))
	require.NoError(t, err, "Get after Create should succeed")
	d.VerifyEqual(t, e, got)
}

func testRead[T any](t *testing.T, factory Factory, d Domain[T]) {
	ctx := context.Background()
	s := factory(t)
	prepareStore(t, ctx, s, d)

	e := d.Make(1)
	require.NoError(t, d.Create(ctx, s, e))

	got, err := d.Get(ctx, s, d.GetID(e))
	require.NoError(t, err)
	d.VerifyEqual(t, e, got)

	// A non-existent ID must surface as ErrNotFound across all backends.
	_, err = d.Get(ctx, s, missingID())
	assert.ErrorIs(t, err, store.ErrNotFound, "Get of missing entity should return ErrNotFound")
}

func testUpdate[T any](t *testing.T, factory Factory, d Domain[T]) {
	ctx := context.Background()
	s := factory(t)
	prepareStore(t, ctx, s, d)

	e := d.Make(1)
	require.NoError(t, d.Create(ctx, s, e))

	d.Mutate(e)
	require.NoError(t, d.Update(ctx, s, e), "Update should succeed")

	got, err := d.Get(ctx, s, d.GetID(e))
	require.NoError(t, err)
	d.VerifyMutated(t, got)
}

func testDelete[T any](t *testing.T, factory Factory, d Domain[T]) {
	ctx := context.Background()
	s := factory(t)
	prepareStore(t, ctx, s, d)

	e := d.Make(1)
	require.NoError(t, d.Create(ctx, s, e))
	id := d.GetID(e)

	require.NoError(t, d.Delete(ctx, s, id), "Delete should succeed")

	// Excluded from the default listing.
	if d.List != nil {
		res, err := d.List(ctx, s, store.ListOptions{})
		require.NoError(t, err)
		assert.False(t, containsID(d, res.Items, id), "deleted entity must be excluded from default List")
	}

	if d.SoftDelete != nil {
		// Soft-deleted: Get still treats it as gone by default, but it must
		// remain visible when deleted entities are explicitly included.
		incl, err := d.SoftDelete.ListIncludeDeleted(ctx, s, store.ListOptions{})
		require.NoError(t, err)
		assert.True(t, containsID(d, incl.Items, id), "soft-deleted entity must be returned with IncludeDeleted")
	} else {
		// Hard-deleted: Get must report ErrNotFound.
		_, err := d.Get(ctx, s, id)
		assert.ErrorIs(t, err, store.ErrNotFound, "Get of hard-deleted entity should return ErrNotFound")
	}
}

func testPaginate[T any](t *testing.T, factory Factory, d Domain[T]) {
	ctx := context.Background()
	s := factory(t)
	prepareStore(t, ctx, s, d)

	const n = 5
	for i := 0; i < n; i++ {
		require.NoError(t, d.Create(ctx, s, d.Make(i+1)))
	}

	// No limit: all entities returned, TotalCount reflects the full set.
	all, err := d.List(ctx, s, store.ListOptions{})
	require.NoError(t, err)
	assert.Len(t, all.Items, n, "unbounded List should return every entity")
	assert.Equal(t, n, all.TotalCount, "TotalCount should reflect the full set")

	// Limited: at most `limit` items, but TotalCount still reflects the full set.
	const limit = 2
	page, err := d.List(ctx, s, store.ListOptions{Limit: limit})
	require.NoError(t, err)
	assert.Len(t, page.Items, limit, "limited List should return exactly `limit` items")
	assert.Equal(t, n, page.TotalCount, "TotalCount should be independent of Limit")
}

func testFilter[T any](t *testing.T, factory Factory, d Domain[T]) {
	ctx := context.Background()
	for _, fc := range d.Filters {
		fc := fc
		t.Run(fc.Name, func(t *testing.T) {
			s := factory(t)
			prepareStore(t, ctx, s, d)
			fc.Seed(t, ctx, s)
			res, err := fc.List(ctx, s)
			require.NoError(t, err)
			assert.Equal(t, fc.WantCount, len(res.Items), "filtered List item count")
			assert.Equal(t, fc.WantCount, res.TotalCount, "filtered List TotalCount")
		})
	}
}

// containsID reports whether any item in items has the given ID.
func containsID[T any](d Domain[T], items []T, id string) bool {
	for i := range items {
		if d.GetID(&items[i]) == id {
			return true
		}
	}
	return false
}
