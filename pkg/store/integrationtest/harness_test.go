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

//go:build integration

package integrationtest

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// defaultConcurrency is the worker count used by the contention/pool tests when
// SCION_TEST_CONCURRENCY is unset. It is deliberately small so the suite stays
// well under the 5-minute local-Postgres target; bump the env var for stress.
const defaultConcurrency = 10

// requirePG skips a test unless a live Postgres backend was provisioned. Every
// test in this package is Postgres-only: the behaviors under test (real row
// locks, MVCC isolation, pool saturation, LISTEN/NOTIFY) do not exist on the
// single-writer in-memory SQLite fallback.
func requirePG(t *testing.T) {
	t.Helper()
	if !enttest.Active() {
		t.Skip("integration: set SCION_TEST_POSTGRES_URL to a live Postgres to run the stress suite")
	}
}

// concurrency returns the worker count for contention tests: the value of
// SCION_TEST_CONCURRENCY if set (>= 2), otherwise defaultConcurrency.
func concurrency(t *testing.T) int {
	t.Helper()
	v := os.Getenv("SCION_TEST_CONCURRENCY")
	if v == "" {
		return defaultConcurrency
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 2 {
		t.Fatalf("integration: invalid SCION_TEST_CONCURRENCY=%q (want integer >= 2): %v", v, err)
	}
	return n
}

// newStore returns a CompositeStore backed by a fresh, isolated Postgres schema
// with a pool large enough that the suite's concurrent writers genuinely overlap
// (rather than serializing behind a tiny pool). Each call gets its own schema, so
// tests never observe each other's rows; the schema and client are torn down on
// cleanup.
func newStore(t *testing.T) *entadapter.CompositeStore {
	return newStoreWithPool(t, 16)
}

// newStoreWithPool is newStore with an explicit MaxOpenConns, used by the
// connection-pool stress tests that need a known, small pool to saturate.
func newStoreWithPool(t *testing.T, maxOpen int) *entadapter.CompositeStore {
	t.Helper()
	dsn := enttest.NewSchemaURL(t) // skips when Postgres is inactive
	client, err := entc.OpenPostgres(dsn, entc.PoolConfig{MaxOpenConns: maxOpen, MaxIdleConns: maxOpen})
	require.NoError(t, err, "open postgres ent client")
	cs := entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// --- entity factories (minimal valid rows for the schema under test) ---

func makeProject(slug string) *store.Project {
	return &store.Project{
		ID:      uuid.NewString(),
		Name:    "project " + slug,
		Slug:    slug,
		Created: time.Now(),
		Updated: time.Now(),
	}
}

func makeAgent(projectID, slug string) *store.Agent {
	return &store.Agent{
		ID:        uuid.NewString(),
		Slug:      slug,
		Name:      "agent " + slug,
		Template:  "default",
		ProjectID: projectID,
		Phase:     "running",
	}
}

func makeUser(email string) *store.User {
	return &store.User{
		ID:          uuid.NewString(),
		Email:       email,
		DisplayName: "user " + email,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
}

func makeScheduledEvent(projectID string) *store.ScheduledEvent {
	return &store.ScheduledEvent{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		EventType: "message",
		FireAt:    time.Now().Add(time.Hour).UTC().Truncate(time.Second),
		Payload:   `{"text":"hi"}`,
		CreatedBy: "tester",
	}
}

// seedProject creates and returns a fresh project, satisfying the agent /
// scheduled-event foreign keys those entities require.
func seedProject(t *testing.T, cs *entadapter.CompositeStore) *store.Project {
	t.Helper()
	p := makeProject("seed-" + shortID())
	require.NoError(t, cs.CreateProject(context.Background(), p), "seed project")
	return p
}

// shortID returns a short, collision-resistant suffix for unique slugs/emails.
func shortID() string { return uuid.NewString()[:8] }

// runConcurrently starts n goroutines that all block on a shared barrier and are
// released simultaneously, maximizing real overlap (and thus real contention)
// rather than letting earlier goroutines finish before later ones start. fn
// receives the worker index 0..n-1. It returns once every worker has finished.
func runConcurrently(n int, fn func(i int)) {
	var release sync.WaitGroup
	var done sync.WaitGroup
	release.Add(1)
	done.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer done.Done()
			release.Wait()
			fn(i)
		}(i)
	}
	release.Done() // fire the starting gun
	done.Wait()
}
