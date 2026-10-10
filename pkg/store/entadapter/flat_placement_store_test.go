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
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/project"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Group C of the flat Runtime Broker contract
// (.design/flat-runtime-brokers-contract.md section 15). Every test uses the
// dual-dialect enttest client, so `make test-launch-store-postgres` runs the
// same bodies against Postgres.

type flatStores struct {
	agents    *AgentStore
	brokers   *ProjectStore
	client    *ent.Client
	projectID string
}

func newFlatStores(t *testing.T) flatStores {
	t.Helper()
	client := enttest.NewClient(t)
	pid := uuid.New()
	_, err := client.Project.Create().SetID(pid).SetName("flat-project").SetSlug("flat-project").Save(context.Background())
	require.NoError(t, err)
	return flatStores{agents: NewAgentStore(client), brokers: NewProjectStore(client), client: client, projectID: pid.String()}
}

func flatBroker(name, status string, target *api.RuntimeTargetDescriptor) *store.RuntimeBroker {
	return &store.RuntimeBroker{
		ID:            uuid.NewString(),
		Name:          name,
		Slug:          name,
		Status:        status,
		RuntimeTarget: target,
	}
}

func dockerTarget() *api.RuntimeTargetDescriptor {
	return &api.RuntimeTargetDescriptor{ID: uuid.NewString(), Type: "docker", DisplayName: "Local Docker"}
}

func (fs flatStores) createBroker(t *testing.T, b *store.RuntimeBroker) *store.RuntimeBroker {
	t.Helper()
	require.NoError(t, fs.brokers.CreateRuntimeBroker(context.Background(), b))
	got, err := fs.brokers.GetRuntimeBroker(context.Background(), b.ID)
	require.NoError(t, err)
	return got
}

func (fs flatStores) createPinnedAgent(t *testing.T, slug string, b *store.RuntimeBroker) *store.Agent {
	t.Helper()
	a := makeAgent(fs.projectID, slug)
	a.RuntimeBrokerID = b.ID
	a.PinnedRuntimeBrokerID = b.ID
	a.PinnedRuntimeTargetID = b.RuntimeTarget.ID
	a.PinnedRuntimeTargetType = b.RuntimeTarget.Type
	require.NoError(t, fs.agents.CreateAgent(context.Background(), a))
	got, err := fs.agents.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

func legacyAppliedConfig() *store.AgentAppliedConfig {
	return &store.AgentAppliedConfig{
		Profile:                "batch",
		RuntimeTarget:          "kubernetes|context=c|namespace=n",
		RuntimeTargetCandidate: "docker",
		CreateInputs:           &store.AgentCreateInputs{Profile: "batch"},
	}
}

// runFlatPlacementAdditiveUpgrade builds legacy rows, removes the new columns
// and index to reproduce the pre-change shape, re-runs AutoMigrate and checks
// that legacy data round-trips and the new columns read NULL/empty.
func runFlatPlacementAdditiveUpgrade(t *testing.T, client *ent.Client, raw *sql.DB) {
	t.Helper()
	ctx := context.Background()
	pid := uuid.New()
	_, err := client.Project.Create().SetID(pid).SetName("upgrade").SetSlug("upgrade").Save(ctx)
	require.NoError(t, err)
	agents, brokers := NewAgentStore(client), NewProjectStore(client)

	legacyBroker := &store.RuntimeBroker{
		ID: uuid.NewString(), Name: "legacy", Slug: "legacy", Status: store.BrokerStatusOnline,
		Profiles:       []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}, {Name: "batch", Type: "kubernetes", Context: "c", Namespace: "n", Available: true}},
		DefaultProfile: "local",
	}
	require.NoError(t, brokers.CreateRuntimeBroker(ctx, legacyBroker))
	legacyAgent := makeAgent(pid.String(), "legacy-agent")
	legacyAgent.RuntimeBrokerID = legacyBroker.ID
	legacyAgent.AppliedConfig = legacyAppliedConfig()
	require.NoError(t, agents.CreateAgent(ctx, legacyAgent))

	// Pre-change shape: drop the named index first (SQLite cannot drop an
	// indexed column), then the six new columns.
	for _, stmt := range []string{
		"DROP INDEX runtimebroker_runtime_target_id",
		"ALTER TABLE runtime_brokers DROP COLUMN runtime_target_id",
		"ALTER TABLE runtime_brokers DROP COLUMN runtime_target_type",
		"ALTER TABLE runtime_brokers DROP COLUMN runtime_target_display_name",
		"ALTER TABLE agents DROP COLUMN pinned_runtime_broker_id",
		"ALTER TABLE agents DROP COLUMN pinned_runtime_target_id",
		"ALTER TABLE agents DROP COLUMN pinned_runtime_target_type",
	} {
		_, err := raw.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	require.NoError(t, entc.AutoMigrate(ctx, client))
	require.NoError(t, entc.AutoMigrate(ctx, client), "AutoMigrate must be idempotent")

	gotBroker, err := brokers.GetRuntimeBroker(ctx, legacyBroker.ID)
	require.NoError(t, err)
	assert.Nil(t, gotBroker.RuntimeTarget, "legacy broker must read as legacy")
	assert.Equal(t, legacyBroker.Profiles, gotBroker.Profiles)
	assert.Equal(t, "local", gotBroker.DefaultProfile)

	gotAgent, err := agents.GetAgent(ctx, legacyAgent.ID)
	require.NoError(t, err)
	assert.False(t, gotAgent.IsPinned())
	assert.Empty(t, gotAgent.PinnedRuntimeBrokerID)
	assert.Empty(t, gotAgent.PinnedRuntimeTargetType)
	require.NotNil(t, gotAgent.AppliedConfig)
	assert.Equal(t, "batch", gotAgent.AppliedConfig.Profile)
	assert.Equal(t, "kubernetes|context=c|namespace=n", gotAgent.AppliedConfig.RuntimeTarget)
	assert.Equal(t, "docker", gotAgent.AppliedConfig.RuntimeTargetCandidate)
	require.NotNil(t, gotAgent.AppliedConfig.CreateInputs)
	assert.Equal(t, "batch", gotAgent.AppliedConfig.CreateInputs.Profile)

	// Two legacy brokers (NULL runtime_target_id) coexist under the
	// re-created unique index, and new columns work after the upgrade.
	other := &store.RuntimeBroker{ID: uuid.NewString(), Name: "legacy-2", Slug: "legacy-2", Status: store.BrokerStatusOffline}
	require.NoError(t, brokers.CreateRuntimeBroker(ctx, other))
	flat := flatBroker("flat", store.BrokerStatusOnline, dockerTarget())
	require.NoError(t, brokers.CreateRuntimeBroker(ctx, flat))
	gotFlat, err := brokers.GetRuntimeBroker(ctx, flat.ID)
	require.NoError(t, err)
	assert.Equal(t, flat.RuntimeTarget, gotFlat.RuntimeTarget)
}

func TestFlatPlacementColumns_AdditiveUpgrade_SQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "flat.db")
	client, err := entc.OpenSQLite(dsn, entc.PoolConfig{MaxOpenConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("sqlite", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runFlatPlacementAdditiveUpgrade(t, client, raw)
}

// TestFlatPlacementColumns_AdditiveUpgrade_Postgres skips unless the enttest
// Postgres backend is built (-tags integration) and SCION_TEST_POSTGRES_URL is
// set; `make test-launch-store-postgres` fails on that skip.
func TestFlatPlacementColumns_AdditiveUpgrade_Postgres(t *testing.T) {
	url := enttest.NewSchemaURL(t)
	client, err := entc.OpenPostgres(url, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 1})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	require.NoError(t, entc.AutoMigrate(context.Background(), client))
	raw, err := sql.Open("pgx", url)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })
	runFlatPlacementAdditiveUpgrade(t, client, raw)
}

func TestCreateAgent_PinnedPlacementRoundTrip(t *testing.T) {
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	a := fs.createPinnedAgent(t, "pinned", b)
	assert.Equal(t, b.ID, a.PinnedRuntimeBrokerID)
	assert.Equal(t, b.RuntimeTarget.ID, a.PinnedRuntimeTargetID)
	assert.Equal(t, "docker", a.PinnedRuntimeTargetType)
	assert.True(t, a.PinValid())

	// A pin that does not name the agent's Runtime Broker is rejected.
	bad := makeAgent(fs.projectID, "bad-pin")
	bad.RuntimeBrokerID = b.ID
	bad.PinnedRuntimeBrokerID = uuid.NewString()
	bad.PinnedRuntimeTargetID = b.RuntimeTarget.ID
	bad.PinnedRuntimeTargetType = "docker"
	require.ErrorIs(t, fs.agents.CreateAgent(context.Background(), bad), store.ErrInvalidPinnedPlacement)
}

func TestUpdateAgent_PreservesPinnedPlacement(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	a := fs.createPinnedAgent(t, "pinned", b)

	// A full-row update from a model without pin values (as a stale or
	// older model would carry), rewriting applied_config.
	a.PinnedRuntimeBrokerID, a.PinnedRuntimeTargetID, a.PinnedRuntimeTargetType = "", "", ""
	a.AppliedConfig = &store.AgentAppliedConfig{Image: "rewritten"}
	a.Phase = "stopped"
	require.NoError(t, fs.agents.UpdateAgent(ctx, a))

	got, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", got.Phase)
	assert.Equal(t, "rewritten", got.AppliedConfig.Image)
	assert.Equal(t, b.ID, got.PinnedRuntimeBrokerID)
	assert.Equal(t, b.RuntimeTarget.ID, got.PinnedRuntimeTargetID)
	assert.Equal(t, "docker", got.PinnedRuntimeTargetType)
}

func TestUpdateAgent_LegacyColumnSetPreservesPin(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	a := fs.createPinnedAgent(t, "pinned", b)

	// An older binary updates only the columns it knows.
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = fs.client.Agent.UpdateOneID(uid).
		SetPhase("error").
		SetRuntimeBrokerID(a.RuntimeBrokerID).
		SetAppliedConfig(`{"profile":"local"}`).
		AddStateVersion(1).
		Save(ctx)
	require.NoError(t, err)

	got, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "error", got.Phase)
	assert.True(t, got.PinValid())
	assert.Equal(t, b.RuntimeTarget.ID, got.PinnedRuntimeTargetID)
}

func TestUpdateRuntimeBroker_PreservesRuntimeTarget(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	want := *b.RuntimeTarget

	// A write-back from a model without a target, with a profiles rewrite.
	b.RuntimeTarget = nil
	b.Status = store.BrokerStatusOffline
	b.Profiles = []store.BrokerProfile{{Name: "x", Type: "docker"}}
	require.NoError(t, fs.brokers.UpdateRuntimeBroker(ctx, b))

	got, err := fs.brokers.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOffline, got.Status)
	require.NotNil(t, got.RuntimeTarget)
	assert.Equal(t, want, *got.RuntimeTarget)
}

func TestUpdateRuntimeBroker_StripsProfilesOnFlatRow(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	b.Profiles = []store.BrokerProfile{{Name: "local", Type: "docker"}}
	b.DefaultProfile = "local"
	require.NoError(t, fs.brokers.UpdateRuntimeBroker(ctx, b))

	got, err := fs.brokers.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Profiles)
	assert.Empty(t, got.DefaultProfile)
}

func TestUpdateRuntimeBroker_FlatRowWithLeftoverProfiles(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	// An older binary left profiles on the flat row (written directly).
	uid, err := parseUUID(b.ID)
	require.NoError(t, err)
	_, err = fs.client.RuntimeBroker.UpdateOneID(uid).
		SetRuntimes(`[{"name":"local","type":"docker","available":true}]`).
		SetDefaultProfile("local").
		Save(ctx)
	require.NoError(t, err)

	// A capability-only read-modify-write succeeds and heals the row.
	cur, err := fs.brokers.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotEmpty(t, cur.Profiles)
	cur.Capabilities = &store.BrokerCapabilities{Sync: true}
	require.NoError(t, fs.brokers.UpdateRuntimeBroker(ctx, cur))

	got, err := fs.brokers.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.Sync)
	assert.Empty(t, got.Profiles)
	assert.Empty(t, got.DefaultProfile)
	assert.NotNil(t, got.RuntimeTarget)
}

// TestUpdateRuntimeBroker_NonFlatProfilesReplacedWithKubernetesProfile mirrors
// the GCP-identity dispatch fixture: a legacy row's profiles are replaced by a
// single kubernetes profile.
func TestUpdateRuntimeBroker_NonFlatProfilesReplacedWithKubernetesProfile(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, &store.RuntimeBroker{ID: uuid.NewString(), Name: "legacy", Slug: "legacy",
		Status: store.BrokerStatusOnline, Profiles: []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}}, DefaultProfile: "local"})
	b.Profiles = []store.BrokerProfile{{Name: "gke", Type: "kubernetes", Context: "ctx", Namespace: "ns", Available: true}}
	b.DefaultProfile = "gke"
	require.NoError(t, fs.brokers.UpdateRuntimeBroker(ctx, b))

	got, err := fs.brokers.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, b.Profiles, got.Profiles)
	assert.Equal(t, "gke", got.DefaultProfile)
	assert.Nil(t, got.RuntimeTarget)
}

// TestUpdateRuntimeBroker_NonFlatEndpointOnlyUpdate mirrors the reconcile
// fixture: an Endpoint-only update of a legacy row.
func TestUpdateRuntimeBroker_NonFlatEndpointOnlyUpdate(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	profiles := []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}}
	b := fs.createBroker(t, &store.RuntimeBroker{ID: uuid.NewString(), Name: "legacy", Slug: "legacy",
		Status: store.BrokerStatusOnline, Profiles: profiles, DefaultProfile: "local"})
	b.Endpoint = "http://broker.example:9800"
	require.NoError(t, fs.brokers.UpdateRuntimeBroker(ctx, b))

	got, err := fs.brokers.GetRuntimeBroker(ctx, b.ID)
	require.NoError(t, err)
	assert.Equal(t, "http://broker.example:9800", got.Endpoint)
	assert.Equal(t, profiles, got.Profiles)
	assert.Equal(t, "local", got.DefaultProfile)
}

func TestCreateRuntimeBroker_RuntimeTargetRoundTrip(t *testing.T) {
	fs := newFlatStores(t)
	b := flatBroker("flat", store.BrokerStatusOnline, dockerTarget())
	b.Profiles = []store.BrokerProfile{{Name: "local", Type: "docker"}}
	b.DefaultProfile = "local"
	got := fs.createBroker(t, b)
	require.NotNil(t, got.RuntimeTarget)
	assert.Equal(t, *b.RuntimeTarget, *got.RuntimeTarget)
	assert.True(t, got.IsFlat())
	assert.Empty(t, got.Profiles, "profiles never persist on a flat row")
	assert.Empty(t, got.DefaultProfile)
}

func TestRuntimeTargetID_UniqueIndex(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	target := dockerTarget()
	fs.createBroker(t, flatBroker("flat-1", store.BrokerStatusOnline, target))
	dup := flatBroker("flat-2", store.BrokerStatusOnline, &api.RuntimeTargetDescriptor{ID: target.ID, Type: "docker"})
	require.Error(t, fs.brokers.CreateRuntimeBroker(ctx, dup), "a target ID cannot belong to two Runtime Brokers")
	// Multiple NULLs coexist.
	fs.createBroker(t, flatBroker("legacy-1", store.BrokerStatusOnline, nil))
	fs.createBroker(t, flatBroker("legacy-2", store.BrokerStatusOnline, nil))
}

func TestSetRuntimeBrokerTarget_LegacyRowNotFlat(t *testing.T) {
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("legacy", store.BrokerStatusOnline, nil))
	_, err := fs.brokers.SetRuntimeBrokerTarget(context.Background(), b.ID, *dockerTarget())
	require.ErrorIs(t, err, store.ErrRuntimeBrokerNotFlat)
	got, err := fs.brokers.GetRuntimeBroker(context.Background(), b.ID)
	require.NoError(t, err)
	assert.Nil(t, got.RuntimeTarget, "a legacy row is never converted")
}

func TestSetRuntimeBrokerTarget_SameTargetUpdatesDisplayName(t *testing.T) {
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	desc := *b.RuntimeTarget
	desc.DisplayName = "Renamed"
	got, err := fs.brokers.SetRuntimeBrokerTarget(context.Background(), b.ID, desc)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", got.RuntimeTarget.DisplayName)
	assert.Equal(t, b.RuntimeTarget.ID, got.RuntimeTarget.ID)
}

func TestSetRuntimeBrokerTarget_DifferentTargetRejected(t *testing.T) {
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	for _, desc := range []api.RuntimeTargetDescriptor{
		{ID: uuid.NewString(), Type: "docker"},
		{ID: b.RuntimeTarget.ID, Type: "kubernetes"},
	} {
		_, err := fs.brokers.SetRuntimeBrokerTarget(context.Background(), b.ID, desc)
		require.ErrorIs(t, err, store.ErrRuntimeTargetChanged)
	}
	got, err := fs.brokers.GetRuntimeBroker(context.Background(), b.ID)
	require.NoError(t, err)
	assert.Equal(t, *b.RuntimeTarget, *got.RuntimeTarget, "stored target unchanged")
}

func TestSetAgentPinnedRuntimeTarget_CompareAndSet(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	next := store.PinnedPlacement{RuntimeBrokerID: b.ID, RuntimeTargetID: b.RuntimeTarget.ID, RuntimeTargetType: "docker"}

	// Empty runtime_broker_id ('') matches an empty expectation.
	a := makeAgent(fs.projectID, "empty-broker")
	require.NoError(t, fs.agents.CreateAgent(ctx, a))
	before, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	got, err := fs.agents.SetAgentPinnedRuntimeTarget(ctx, a.ID, store.PinnedPlacement{}, next)
	require.NoError(t, err)
	assert.Equal(t, b.ID, got.RuntimeBrokerID)
	assert.True(t, got.PinValid())
	assert.Equal(t, before.StateVersion+1, got.StateVersion, "the setter bumps state_version")

	// NULL runtime_broker_id also matches an empty expectation.
	n := makeAgent(fs.projectID, "null-broker")
	require.NoError(t, fs.agents.CreateAgent(ctx, n))
	uid, err := parseUUID(n.ID)
	require.NoError(t, err)
	_, err = fs.client.Agent.UpdateOneID(uid).ClearRuntimeBrokerID().Save(ctx)
	require.NoError(t, err)
	got, err = fs.agents.SetAgentPinnedRuntimeTarget(ctx, n.ID, store.PinnedPlacement{}, next)
	require.NoError(t, err)
	assert.True(t, got.PinValid())

	// It never unpins.
	_, err = fs.agents.SetAgentPinnedRuntimeTarget(ctx, n.ID, got.Placement(), store.PinnedPlacement{RuntimeBrokerID: b.ID})
	require.ErrorIs(t, err, store.ErrInvalidPinnedPlacement)

	// Missing agent.
	_, err = fs.agents.SetAgentPinnedRuntimeTarget(ctx, uuid.NewString(), store.PinnedPlacement{}, next)
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestSetAgentPinnedRuntimeTarget_MissReturnsPlacementChanged(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	a := fs.createPinnedAgent(t, "pinned", b)
	other := fs.createBroker(t, flatBroker("flat-2", store.BrokerStatusOnline, dockerTarget()))
	next := store.PinnedPlacement{RuntimeBrokerID: other.ID, RuntimeTargetID: other.RuntimeTarget.ID, RuntimeTargetType: "docker"}

	_, err := fs.agents.SetAgentPinnedRuntimeTarget(ctx, a.ID, store.PinnedPlacement{}, next)
	require.ErrorIs(t, err, store.ErrPinnedPlacementChanged, "expected unpinned, row is pinned")
	stale := a.Placement()
	stale.RuntimeTargetID = uuid.NewString()
	_, err = fs.agents.SetAgentPinnedRuntimeTarget(ctx, a.ID, stale, next)
	require.ErrorIs(t, err, store.ErrPinnedPlacementChanged)

	got, err := fs.agents.SetAgentPinnedRuntimeTarget(ctx, a.ID, a.Placement(), next)
	require.NoError(t, err)
	assert.Equal(t, other.ID, got.RuntimeBrokerID)
	assert.Equal(t, other.RuntimeTarget.ID, got.PinnedRuntimeTargetID)
}

func TestSetAgentPinnedRuntimeTarget_StaleUpdateAgentConflicts(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	a := fs.createPinnedAgent(t, "pinned", b)
	staleModel, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	other := fs.createBroker(t, flatBroker("flat-2", store.BrokerStatusOnline, dockerTarget()))
	_, err = fs.agents.SetAgentPinnedRuntimeTarget(ctx, a.ID, a.Placement(),
		store.PinnedPlacement{RuntimeBrokerID: other.ID, RuntimeTargetID: other.RuntimeTarget.ID, RuntimeTargetType: "docker"})
	require.NoError(t, err)

	staleModel.Phase = "stopped"
	require.ErrorIs(t, fs.agents.UpdateAgent(ctx, staleModel), store.ErrVersionConflict)

	got, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, other.ID, got.RuntimeBrokerID)
	assert.True(t, got.PinValid(), "pin and runtime_broker_id stay equal")
}

func TestRuntimeTargetCandidate_DoesNotTouchPin(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	a := fs.createPinnedAgent(t, "pinned", b)

	written, err := fs.agents.SetAgentRuntimeTarget(ctx, a.ID, a.StateVersion, "docker", "")
	require.NoError(t, err)
	require.True(t, written)
	_, _, err = fs.agents.ClearAgentRuntimeTarget(ctx, a.ID)
	require.NoError(t, err)

	got, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.True(t, got.PinValid())
	assert.Equal(t, b.RuntimeTarget.ID, got.PinnedRuntimeTargetID)
}

func TestPinnedPlacement_StaleWhenBrokerMoved(t *testing.T) {
	fs := newFlatStores(t)
	b := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	a := fs.createPinnedAgent(t, "pinned", b)
	require.True(t, a.PinValid())
	// An older binary moves the agent without touching the pin.
	uid, err := parseUUID(a.ID)
	require.NoError(t, err)
	_, err = fs.client.Agent.UpdateOneID(uid).SetRuntimeBrokerID(uuid.NewString()).Save(context.Background())
	require.NoError(t, err)
	got, err := fs.agents.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.True(t, got.IsPinned())
	assert.False(t, got.PinValid(), "a pin whose Runtime Broker differs is stale")
	legacy := makeAgent(fs.projectID, "legacy")
	assert.False(t, legacy.PinValid())
}

func (fs flatStores) createAgentOn(t *testing.T, slug, brokerID, phase string) *store.Agent {
	t.Helper()
	a := makeAgent(fs.projectID, slug)
	a.RuntimeBrokerID = brokerID
	a.Phase = phase
	require.NoError(t, fs.agents.CreateAgent(context.Background(), a))
	return a
}

func agentIDs(agents []*store.Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.ID)
	}
	return out
}

func TestFindOrphanedAgents_LegacyOfflineAndMissingBrokersUnchanged(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	current := fs.createBroker(t, flatBroker("current", store.BrokerStatusOffline, nil))
	offline := fs.createBroker(t, flatBroker("offline", store.BrokerStatusOffline, nil))
	online := fs.createBroker(t, flatBroker("online", store.BrokerStatusOnline, nil))

	onOffline := fs.createAgentOn(t, "on-offline", offline.ID, "running")
	onMissing := fs.createAgentOn(t, "on-missing", uuid.NewString(), "running")
	fs.createAgentOn(t, "on-online", online.ID, "running")
	fs.createAgentOn(t, "terminal", offline.ID, "stopped")
	fs.createAgentOn(t, "errored", offline.ID, "error")
	fs.createAgentOn(t, "on-current", current.ID, "running")
	deleted := fs.createAgentOn(t, "deleted", offline.ID, "running")
	duid, err := parseUUID(deleted.ID)
	require.NoError(t, err)
	_, err = fs.client.Agent.UpdateOneID(duid).SetDeletedAt(time.Now()).Save(ctx)
	require.NoError(t, err)

	got, err := fs.agents.FindOrphanedAgents(ctx, current.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{onOffline.ID, onMissing.ID}, agentIDs(got))
}

func TestFindOrphanedAgents_ExcludesPinnedAndFlat(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	current := fs.createBroker(t, flatBroker("current", store.BrokerStatusOffline, nil))
	offlineFlat := fs.createBroker(t, flatBroker("offline-flat", store.BrokerStatusOffline, dockerTarget()))
	offlineLegacy := fs.createBroker(t, flatBroker("offline-legacy", store.BrokerStatusOffline, nil))

	fs.createPinnedAgent(t, "pinned", offlineFlat)
	fs.createAgentOn(t, "unpinned-on-flat", offlineFlat.ID, "running")
	legacy := fs.createAgentOn(t, "legacy", offlineLegacy.ID, "running")

	got, err := fs.agents.FindOrphanedAgents(ctx, current.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{legacy.ID}, agentIDs(got))
}

func TestFindOrphanedAgents_FlatCurrentBrokerAdoptsNothing(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	flat := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	offline := fs.createBroker(t, flatBroker("offline", store.BrokerStatusOffline, nil))
	fs.createAgentOn(t, "legacy", offline.ID, "running")

	got, err := fs.agents.FindOrphanedAgents(ctx, flat.ID)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestReassignAgentsToBroker_LegacyUnchanged(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	dest := fs.createBroker(t, flatBroker("dest", store.BrokerStatusOnline, nil))
	a := fs.createAgentOn(t, "a", uuid.NewString(), "running")
	n, err := fs.agents.ReassignAgentsToBroker(ctx, []*store.Agent{a}, dest.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, dest.ID, got.RuntimeBrokerID)
}

func TestReassignAgentsToBroker_NeverTargetsFlatBroker(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	flat := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	old := uuid.NewString()
	a := fs.createAgentOn(t, "a", old, "running")
	n, err := fs.agents.ReassignAgentsToBroker(ctx, []*store.Agent{a}, flat.ID)
	require.ErrorIs(t, err, store.ErrFlatRuntimeBrokerReassign)
	assert.Equal(t, 0, n)
	got, err := fs.agents.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, old, got.RuntimeBrokerID)
}

func TestReassignAgentsToBroker_SkipsPinnedAgents(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	flat := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOffline, dockerTarget()))
	dest := fs.createBroker(t, flatBroker("dest", store.BrokerStatusOnline, nil))
	pinned := fs.createPinnedAgent(t, "pinned", flat)
	legacy := fs.createAgentOn(t, "legacy", uuid.NewString(), "running")

	n, err := fs.agents.ReassignAgentsToBroker(ctx, []*store.Agent{pinned, legacy}, dest.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	got, err := fs.agents.GetAgent(ctx, pinned.ID)
	require.NoError(t, err)
	assert.Equal(t, flat.ID, got.RuntimeBrokerID)
	assert.True(t, got.PinValid())
}

func setProjectDefault(t *testing.T, fs flatStores, brokerID string) uuid.UUID {
	t.Helper()
	pid := uuid.New()
	_, err := fs.client.Project.Create().SetID(pid).SetName("p-" + pid.String()[:8]).SetSlug("p-" + pid.String()[:8]).
		SetDefaultRuntimeBrokerID(brokerID).Save(context.Background())
	require.NoError(t, err)
	return pid
}

func projectDefault(t *testing.T, fs flatStores, pid uuid.UUID) string {
	t.Helper()
	p, err := fs.client.Project.Query().Where(project.IDEQ(pid)).Only(context.Background())
	require.NoError(t, err)
	return derefString(p.DefaultRuntimeBrokerID)
}

func TestReassignProjectBroker_LegacyUnchanged(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	offline := fs.createBroker(t, flatBroker("offline", store.BrokerStatusOffline, nil))
	dest := fs.createBroker(t, flatBroker("dest", store.BrokerStatusOnline, nil))
	p1 := setProjectDefault(t, fs, offline.ID)
	missing := uuid.NewString()
	p2 := setProjectDefault(t, fs, missing)

	n, err := fs.agents.ReassignProjectBroker(ctx, offline.ID, dest.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, dest.ID, projectDefault(t, fs, p1))
	n, err = fs.agents.ReassignProjectBroker(ctx, missing, dest.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "a missing old row counts as legacy")
	assert.Equal(t, dest.ID, projectDefault(t, fs, p2))
}

func TestReassignProjectBroker_NeverRepointsToOrFromFlat(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	flat := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOffline, dockerTarget()))
	legacyOff := fs.createBroker(t, flatBroker("legacy-off", store.BrokerStatusOffline, nil))
	dest := fs.createBroker(t, flatBroker("dest", store.BrokerStatusOnline, nil))
	flatDest := fs.createBroker(t, flatBroker("flat-dest", store.BrokerStatusOnline, dockerTarget()))
	pFromFlat := setProjectDefault(t, fs, flat.ID)
	pToFlat := setProjectDefault(t, fs, legacyOff.ID)

	n, err := fs.agents.ReassignProjectBroker(ctx, flat.ID, dest.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, flat.ID, projectDefault(t, fs, pFromFlat))
	n, err = fs.agents.ReassignProjectBroker(ctx, legacyOff.ID, flatDest.ID)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Equal(t, legacyOff.ID, projectDefault(t, fs, pToFlat))
}

func TestGetLegacyRuntimeBrokerByName_IgnoresFlatRowWithSameName(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	// The flat row is created first, so a plain First() could return it.
	fs.createBroker(t, flatBroker("Shared-Name", store.BrokerStatusOnline, dockerTarget()))
	legacy := fs.createBroker(t, &store.RuntimeBroker{ID: uuid.NewString(), Name: "shared-name", Slug: "shared-name-2", Status: store.BrokerStatusOnline})

	got, err := fs.brokers.GetLegacyRuntimeBrokerByName(ctx, "SHARED-NAME")
	require.NoError(t, err)
	assert.Equal(t, legacy.ID, got.ID)
	assert.Nil(t, got.RuntimeTarget)

	_, err = fs.brokers.GetLegacyRuntimeBrokerByName(ctx, "nobody")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestFindOrphanedAgents_ExcludesStalePinnedAgent: an agent an older binary
// moved off its flat Runtime Broker (pinned, but runtime_broker_id now names
// an offline legacy or a missing row) is still never an orphan candidate.
func TestFindOrphanedAgents_ExcludesStalePinnedAgent(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	current := fs.createBroker(t, flatBroker("current", store.BrokerStatusOffline, nil))
	flat := fs.createBroker(t, flatBroker("flat", store.BrokerStatusOnline, dockerTarget()))
	offlineLegacy := fs.createBroker(t, flatBroker("offline-legacy", store.BrokerStatusOffline, nil))

	staleOnLegacy := fs.createPinnedAgent(t, "stale-on-legacy", flat)
	staleOnMissing := fs.createPinnedAgent(t, "stale-on-missing", flat)
	for id, dest := range map[string]string{staleOnLegacy.ID: offlineLegacy.ID, staleOnMissing.ID: uuid.NewString()} {
		uid, err := parseUUID(id)
		require.NoError(t, err)
		_, err = fs.client.Agent.UpdateOneID(uid).SetRuntimeBrokerID(dest).Save(ctx)
		require.NoError(t, err)
	}
	legacy := fs.createAgentOn(t, "legacy", offlineLegacy.ID, "running")

	got, err := fs.agents.FindOrphanedAgents(ctx, current.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{legacy.ID}, agentIDs(got), "stale-pinned agents must be excluded by the pin predicate")
}

func TestGetLegacyRuntimeBrokerByName_OldestByCreatedWins(t *testing.T) {
	ctx := context.Background()
	fs := newFlatStores(t)
	mk := func(name, slug string, at time.Time) uuid.UUID {
		id := uuid.New()
		_, err := fs.client.RuntimeBroker.Create().SetID(id).SetName(name).SetSlug(slug).SetCreated(at).Save(ctx)
		require.NoError(t, err)
		return id
	}
	// Insert the newer row first so insertion order cannot explain the result.
	mk("DUP", "dup-2", time.Now())
	older := mk("dup", "dup-1", time.Now().Add(-time.Hour))
	got, err := fs.brokers.GetLegacyRuntimeBrokerByName(ctx, "dup")
	require.NoError(t, err)
	assert.Equal(t, older.String(), got.ID)
}
