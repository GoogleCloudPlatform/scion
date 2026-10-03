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
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTimedTestStore opens a fresh Ent-backed store like newTestStore, but
// also returns the raw *ent.Client so a test can pin exact Created
// timestamps on Project/RuntimeBroker rows (both fields are immutable and
// always default to time.Now() through the store.Store interface, which
// isn't precise enough for the delta-logging assertions below).
func newTimedTestStore(t *testing.T) (*ent.Client, store.Store) {
	t.Helper()
	dsn := fmt.Sprintf("file:brokerownershipdeltatest%d?mode=memory&cache=shared", time.Now().UnixNano())
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	s := entadapter.NewCompositeStore(client)
	require.NoError(t, s.Migrate(context.Background()))
	t.Cleanup(func() { _ = s.Close() })
	return client, s
}

// timedProject creates a project with the given owner and exact Created
// timestamp via the raw Ent client, returning its ID.
func timedProject(t *testing.T, ctx context.Context, client *ent.Client, createdBy string, created time.Time) string {
	t.Helper()
	id := uuid.New()
	_, err := client.Project.Create().
		SetID(id).
		SetName("project-" + id.String()).
		SetSlug("project-" + id.String()).
		SetCreatedBy(createdBy).
		SetCreated(created).
		SetUpdated(created).
		Save(ctx)
	require.NoError(t, err)
	return id.String()
}

// timedBroker creates an ownerless runtime broker with the given exact
// Created timestamp and labels via the raw Ent client, returning its ID and
// name.
func timedBroker(t *testing.T, ctx context.Context, client *ent.Client, created time.Time, labels map[string]string) (id, name string) {
	t.Helper()
	u := uuid.New()
	name = "broker-" + u.String()
	create := client.RuntimeBroker.Create().
		SetID(u).
		SetName(name).
		SetSlug(name).
		SetCreated(created).
		SetUpdated(created)
	if labels != nil {
		create.SetLabels(labels)
	}
	_, err := create.Save(ctx)
	require.NoError(t, err)
	return u.String(), name
}

// createTestOwnedProject creates a project with the given owner. CreatedBy
// can only be set at creation time — store.Store.UpdateProject, like
// UpdateRuntimeBroker, never touches it — so this cannot reuse
// createTestProject, which leaves CreatedBy empty.
func createTestOwnedProject(t *testing.T, ctx context.Context, s store.Store, name, createdBy string) store.Project {
	t.Helper()
	p := &store.Project{
		ID:        uuid.NewString(),
		Name:      name,
		Slug:      uuid.NewString(),
		CreatedBy: createdBy,
	}
	require.NoError(t, s.CreateProject(ctx, p))
	return *p
}

// createTestRuntimeBroker creates an ownerless runtime broker with the given
// name and labels. Precise Created timestamp control isn't needed at this
// wiring-level test: sequential store calls in a test land well within the
// default window, and boundary behavior is covered by
// pkg/brokerownership's own tests.
func createTestRuntimeBroker(t *testing.T, ctx context.Context, s store.Store, name string, labels map[string]string) store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:     uuid.NewString(),
		Name:   name,
		Slug:   name,
		Labels: labels,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, b))
	return *b
}

// TestBrokerOwnershipBackfill_AttributesAndMarksComplete exercises the boot
// wiring end to end: a broker created in the same request-shaped sequence
// as its owning project gets attributed, and the completion marker and log
// summary reflect the pass.
func TestBrokerOwnershipBackfill_AttributesAndMarksComplete(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	project := createTestOwnedProject(t, ctx, s, "owner-project", "user-a")

	broker := createTestRuntimeBroker(t, ctx, s, "test-broker", nil)
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerOwnershipBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "pass completed")
	assert.Contains(t, logOutput, "attributed=1")

	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "user-a", got.CreatedBy)

	done, err := IsMigrationComplete(ctx, s, MigrationBrokerOwnershipBackfill)
	require.NoError(t, err)
	assert.True(t, done, "migration marker should be written after a successful pass")
}

// TestBrokerOwnershipBackfill_EmbeddedBrokerUntouched confirms the boot
// wiring leaves an embedded-role broker alone even with a perfect
// timing/linkage match, and doesn't log a per-broker line for it (excluded
// brokers are an expected, benign class, not something worth a log line
// each).
func TestBrokerOwnershipBackfill_EmbeddedBrokerUntouched(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	project := createTestOwnedProject(t, ctx, s, "owner-project", "user-a")

	broker := createTestRuntimeBroker(t, ctx, s, "co-located-broker", map[string]string{
		"scion.io/broker-role": "embedded",
	})
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerOwnershipBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "excluded_embedded=1")
	assert.NotContains(t, logOutput, broker.ID, "excluded brokers are not logged individually")

	got, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Empty(t, got.CreatedBy)
}

// TestBrokerOwnershipBackfill_Idempotent confirms a second boot is a no-op:
// the migration marker short-circuits the pass entirely.
func TestBrokerOwnershipBackfill_Idempotent(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	project := createTestOwnedProject(t, ctx, s, "owner-project", "user-a")

	broker := createTestRuntimeBroker(t, ctx, s, "test-broker", nil)
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	runBrokerOwnershipBackfill(ctx, s)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerOwnershipBackfill(ctx, s)

	assert.Contains(t, buf.String(), "already complete, skipping")
}

// TestBrokerOwnershipBackfill_NoBrokers confirms an empty pass (no
// ownerless brokers at all) still completes and writes the marker.
func TestBrokerOwnershipBackfill_NoBrokers(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerOwnershipBackfill(ctx, s)

	assert.Contains(t, buf.String(), "scanned=0")

	done, err := IsMigrationComplete(ctx, s, MigrationBrokerOwnershipBackfill)
	require.NoError(t, err)
	assert.True(t, done, "marker should be written for an empty pass")
}

// TestBrokerOwnershipBackfill_LogsDeltas proves the boot wiring's delta
// logging (logDeltaSummary) runs and reports real values: each attributed
// broker's own signed delta on its "attributed" line, and the run's maximum
// in-window delta — the larger of the two, not the smaller — across more
// than one attributed broker in the summary line.
func TestBrokerOwnershipBackfill_LogsDeltas(t *testing.T) {
	client, s := newTimedTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)

	// Two independent pairs, spaced minutes apart so neither pair's project
	// is a timing candidate for the other broker.
	p1 := timedProject(t, ctx, client, "user-1", base)
	b1, name1 := timedBroker(t, ctx, client, base.Add(300*time.Millisecond), nil) // delta 0.3s
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: p1, BrokerID: b1, BrokerName: name1, Status: store.BrokerStatusOnline,
	}))

	t0 := base.Add(10 * time.Minute)
	p2 := timedProject(t, ctx, client, "user-2", t0)
	b2, name2 := timedBroker(t, ctx, client, t0.Add(800*time.Millisecond), nil) // delta 0.8s, the run's max
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: p2, BrokerID: b2, BrokerName: name2, Status: store.BrokerStatusOnline,
	}))

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerOwnershipBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "attributed=2")
	assert.Contains(t, logOutput, "delta_seconds=0.3", "broker1's attributed line must carry its own delta")
	assert.Contains(t, logOutput, "delta_seconds=0.8", "broker2's attributed line must carry its own delta")
	assert.Contains(t, logOutput, "max_in_window_delta_seconds=0.8", "the summary must report the larger of the two in-window deltas, not the smaller")
}

// TestBrokerOwnershipBackfill_LogsNegativeNearMiss proves a negative
// near-miss delta (a project created after the broker — never a genuine
// pair, regardless of window width) is counted in the summary line and
// logged individually.
func TestBrokerOwnershipBackfill_LogsNegativeNearMiss(t *testing.T) {
	client, s := newTimedTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)

	p := timedProject(t, ctx, client, "user-a", base)
	b, name := timedBroker(t, ctx, client, base.Add(300*time.Millisecond), nil) // delta 0.3s, attributed
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: p, BrokerID: b, BrokerName: name, Status: store.BrokerStatusOnline,
	}))

	// Created after the broker: the nearest neighbor just outside the
	// window on the upper side, delta = 0.3 - 0.5 = -0.2s.
	_ = timedProject(t, ctx, client, "user-later", base.Add(500*time.Millisecond))

	buf, restore := captureSlog(t)
	defer restore()

	runBrokerOwnershipBackfill(ctx, s)

	logOutput := buf.String()
	assert.Contains(t, logOutput, "negative_near_misses=1")
	assert.Contains(t, logOutput, "negative near-miss delta observed")
	assert.Contains(t, logOutput, "delta_seconds=-0.2")
}

// cmdRaceLoserStore wraps a store.Store and makes SetRuntimeBrokerCreatedByIfEmpty
// report applied=false for one specific broker without touching the row,
// deterministically simulating a concurrent writer that got there first
// (mirrors pkg/brokerownership's own raceLoserStore test double).
type cmdRaceLoserStore struct {
	store.Store
	brokerID string
}

func (s *cmdRaceLoserStore) SetRuntimeBrokerCreatedByIfEmpty(ctx context.Context, id, createdBy string) (bool, error) {
	if id == s.brokerID {
		return false, nil
	}
	return s.Store.SetRuntimeBrokerCreatedByIfEmpty(ctx, id, createdBy)
}

// brokerOwnershipMarkerResiduals reads back the Residuals field of the
// broker-ownership migration's completion marker.
func brokerOwnershipMarkerResiduals(t *testing.T, ctx context.Context, s store.Store) int {
	t.Helper()
	_, raw, err := loadMigrationsDoc(ctx, s)
	require.NoError(t, err)
	entry, ok := raw[string(MigrationBrokerOwnershipBackfill)]
	require.True(t, ok, "expected a marker entry for the broker ownership migration")
	var marker migrationMarker
	require.NoError(t, json.Unmarshal(entry, &marker))
	return marker.Residuals
}

// TestBrokerOwnershipBackfill_MarkerResidualExcludesAlreadySet proves the
// completion marker's residual count is exactly TotalScanned minus every
// category that is NOT "left ownerless for admin review": a real
// attribution, a lost write race (ReasonAlreadySet — that broker was in
// fact attributed, just not by this pass), and an excluded-embedded broker
// (an expected, benign class, not an admin-review item) must each be
// absent from the residual, leaving only the genuinely ownerless broker.
// All three non-residual categories are exercised with a nonzero count so
// dropping any one of them from the subtraction changes the expected
// number.
func TestBrokerOwnershipBackfill_MarkerResidualExcludesAlreadySet(t *testing.T) {
	client, s := newTimedTestStore(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)

	// Four independent groups, spaced minutes apart so no group's project
	// is a timing candidate for another group's broker.

	// Group 1: a real attribution.
	pReal := timedProject(t, ctx, client, "user-real", base)
	bReal, nameReal := timedBroker(t, ctx, client, base.Add(300*time.Millisecond), nil)
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: pReal, BrokerID: bReal, BrokerName: nameReal, Status: store.BrokerStatusOnline,
	}))

	// Group 2: would be attributed, but is wrapped to lose the write race.
	t1 := base.Add(10 * time.Minute)
	pLoser := timedProject(t, ctx, client, "user-loser", t1)
	bLoser, nameLoser := timedBroker(t, ctx, client, t1.Add(300*time.Millisecond), nil)
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: pLoser, BrokerID: bLoser, BrokerName: nameLoser, Status: store.BrokerStatusOnline,
	}))

	// Group 3: excluded outright by the embedded-role label, regardless of
	// any timing/linkage match.
	t2 := base.Add(20 * time.Minute)
	pEmbedded := timedProject(t, ctx, client, "user-embedded", t2)
	bEmbedded, nameEmbedded := timedBroker(t, ctx, client, t2.Add(300*time.Millisecond), map[string]string{
		"scion.io/broker-role": "embedded",
	})
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: pEmbedded, BrokerID: bEmbedded, BrokerName: nameEmbedded, Status: store.BrokerStatusOnline,
	}))

	// Group 4: genuinely left ownerless — no timing candidate at all.
	_, _ = timedBroker(t, ctx, client, base.Add(30*time.Minute), nil)

	wrapped := &cmdRaceLoserStore{Store: s, brokerID: bLoser}
	runBrokerOwnershipBackfill(ctx, wrapped)

	got, err := s.GetRuntimeBroker(ctx, bReal)
	require.NoError(t, err)
	require.Equal(t, "user-real", got.CreatedBy, "sanity: the real attribution must have gone through")

	residuals := brokerOwnershipMarkerResiduals(t, ctx, s)
	assert.Equal(t, 1, residuals,
		"the residual must count only the genuinely-ownerless broker, not the real attribution, the lost-race broker, or the excluded-embedded broker")
}
