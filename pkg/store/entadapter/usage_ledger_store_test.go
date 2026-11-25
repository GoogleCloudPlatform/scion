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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/usageevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func createTestUser(t *testing.T, cs *CompositeStore, email string) string {
	t.Helper()
	u := &store.User{
		ID:          uuid.NewString(),
		Email:       email,
		DisplayName: email,
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, cs.CreateUser(context.Background(), u))
	return u.ID
}

func createTestProject(t *testing.T, cs *CompositeStore, name string) string {
	t.Helper()
	pid := uuid.NewString()
	require.NoError(t, cs.CreateProject(context.Background(), &store.Project{
		ID:   pid,
		Name: name,
		Slug: name + "-" + pid[:8],
	}))
	return pid
}

func TestUsageLedgerSchemas_MigrationAndNullability(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "usage-schema")
	userID := createTestUser(t, cs, "schema-user@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	dayUTC := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// 1. run_attribution
	created, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		Generation:    1,
		HubID:         "hub-test",
		ProjectID:     projectID,
		AgentID:       agentID,
		AgentSlug:     "worker-1",
		BrokerID:      "broker-1",
		BillingUserID: &userID,
		Harness:       "claude",
		HarnessConfig: "claude",
		Template:      "default",
		CreatedAt:     now,
	})
	require.NoError(t, err)
	require.True(t, created)

	// 2. usage_events: verify three-state nullability (unknown=NULL vs 0 vs positive).
	zero := int64(0)
	pos := int64(42)
	_, err = client.UsageEvent.Create().
		SetHubID("hub-test").
		SetEventID("evt-null-vs-zero").
		SetRunID(runID).
		SetProjectID(projectID).
		SetAgentID(agentID).
		SetBillingUserID(userID).
		SetHarness("claude").
		SetModel("claude-sonnet-4-6").
		SetModelSource("event").
		SetUsageSource(store.UsageSourceNative).
		SetStatus("success").
		SetCalls(1).
		SetInput(pos).
		SetOutput(zero).
		// cache_read, cache_write, cache_write_1h, reasoning left nil (SQL NULL)
		SetTokenStatus("partial").
		SetEventTs(now).
		SetReceivedAt(now).
		Save(ctx)
	require.NoError(t, err)

	evRow, err := client.UsageEvent.Query().
		Where(usageevent.HubIDEQ("hub-test"), usageevent.EventIDEQ("evt-null-vs-zero")).
		Only(ctx)
	require.NoError(t, err)
	require.NotNil(t, evRow.Input)
	assert.Equal(t, int64(42), *evRow.Input)
	require.NotNil(t, evRow.Output, "measured 0 must remain non-nil 0")
	assert.Equal(t, int64(0), *evRow.Output)
	assert.Nil(t, evRow.CacheRead, "unknown cache_read must remain SQL NULL")
	assert.Nil(t, evRow.CacheWrite, "unknown cache_write must remain SQL NULL")
	assert.Nil(t, evRow.CacheWrite1h, "unknown cache_write_1h must remain SQL NULL")
	assert.Nil(t, evRow.Reasoning, "unknown reasoning must remain SQL NULL")

	// Duplicate (hub_id, event_id) must fail unique constraint.
	_, err = client.UsageEvent.Create().
		SetHubID("hub-test").
		SetEventID("evt-null-vs-zero").
		SetRunID(runID).
		SetProjectID(projectID).
		SetAgentID(agentID).
		SetHarness("claude").
		SetModel("claude-sonnet-4-6").
		SetModelSource("event").
		SetUsageSource(store.UsageSourceNative).
		SetStatus("success").
		SetCalls(1).
		SetTokenStatus("not_reported").
		SetEventTs(now).
		SetReceivedAt(now).
		Save(ctx)
	require.Error(t, err)

	// 3. usage_rollups_daily: verify empty string for NULL billing_user_id unique key.
	_, err = client.UsageRollupDaily.Create().
		SetBucketStart(dayUTC).
		SetProjectID(projectID).
		SetBillingUserID("").
		SetAgentID(agentID).
		SetHarness("claude").
		SetModel("claude-sonnet-4-6").
		SetUsageSource(store.UsageSourceNative).
		SetCalls(1).
		SetInput(42).
		SetEvents(1).
		SetFirstEventTs(now).
		SetLastEventTs(now).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.UsageRollupDaily.Create().
		SetBucketStart(dayUTC).
		SetProjectID(projectID).
		SetBillingUserID("").
		SetAgentID(agentID).
		SetHarness("claude").
		SetModel("claude-sonnet-4-6").
		SetUsageSource(store.UsageSourceNative).
		SetFirstEventTs(now).
		SetLastEventTs(now).
		Save(ctx)
	require.Error(t, err, "duplicate usage_rollups_daily key must be rejected")

	// 4. usage_scope_counters_daily
	_, err = client.UsageScopeCounterDaily.Create().
		SetScopeKind(store.UsageScopeKindProject).
		SetScopeID(projectID).
		SetDayUtc(dayUTC).
		SetTokensSummable(42).
		SetCalls(1).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.UsageScopeCounterDaily.Create().
		SetScopeKind(store.UsageScopeKindProject).
		SetScopeID(projectID).
		SetDayUtc(dayUTC).
		Save(ctx)
	require.Error(t, err, "duplicate usage_scope_counters_daily key must be rejected")

	// 5. meter_health
	require.NoError(t, cs.UpsertMeterHealth(ctx, &store.MeterHealth{
		AgentID:           agentID,
		RunID:             runID,
		ProducerEpoch:     "epoch-1",
		LastReceiptAt:     now,
		LastContiguousSeq: 5,
		MaxSeqSeen:        5,
		SupportedSignals:  []string{"agent.usage", "telemetry.health"},
		FinalState:        store.MeterHealthFinalStateOpen,
	}))
	rows, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(5), rows[0].LastContiguousSeq)
}

func TestResolveBillingUserID_Table(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	userA := createTestUser(t, cs, "alice@example.com")
	userB := createTestUser(t, cs, "bob@example.com")
	userC := createTestUser(t, cs, "carol@example.com")
	agentPrincipal1 := uuid.NewString() // not a User row
	agentPrincipal2 := uuid.NewString() // not a User row

	tests := []struct {
		name      string
		ownerID   string
		createdBy string
		ancestry  []string
		want      *string
	}{
		{
			name:      "owner user wins over ancestry and created_by",
			ownerID:   userA,
			ancestry:  []string{userB, userC},
			createdBy: userC,
			want:      &userA,
		},
		{
			name:      "owner is non-user agent falls back to ancestry[0] user",
			ownerID:   agentPrincipal1,
			ancestry:  []string{userA, agentPrincipal2, userB},
			createdBy: userC,
			want:      &userA,
		},
		{
			name:      "ancestry skips leading non-user agents to pick first user",
			ownerID:   "",
			ancestry:  []string{agentPrincipal1, userB, userC},
			createdBy: userC,
			want:      &userB,
		},
		{
			name:      "no user in owner or ancestry falls back to created_by user",
			ownerID:   agentPrincipal1,
			ancestry:  []string{agentPrincipal1, agentPrincipal2},
			createdBy: userC,
			want:      &userC,
		},
		{
			name:      "principal-less agent resolves to NULL",
			ownerID:   "",
			ancestry:  nil,
			createdBy: "",
			want:      nil,
		},
		{
			name:      "all non-user principals resolve to NULL",
			ownerID:   agentPrincipal1,
			ancestry:  []string{agentPrincipal1, agentPrincipal2},
			createdBy: agentPrincipal2,
			want:      nil,
		},
		{
			name:      "malformed non-UUID strings safely skipped",
			ownerID:   "not-a-uuid",
			ancestry:  []string{"bad-id", userB},
			createdBy: "also-bad",
			want:      &userB,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := cs.ResolveBillingUserID(ctx, tc.ownerID, tc.createdBy, tc.ancestry)
			require.NoError(t, err)
			if tc.want == nil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, *tc.want, *got)
			}
		})
	}
}

func TestRunAttribution_EnsureImmutableAndFillOnceDigests(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "attr-immutable")
	userA := createTestUser(t, cs, "a@example.com")
	userB := createTestUser(t, cs, "b@example.com")
	agentID := uuid.NewString()
	runID := "run-" + uuid.NewString()

	created, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:          runID,
		Generation:     1,
		HubID:          "hub-1",
		ProjectID:      projectID,
		AgentID:        agentID,
		AgentSlug:      "a1",
		BillingUserID:  &userA,
		Harness:        "claude",
		HarnessConfig:  "claude",
		Template:       "tmpl-1",
		LabelsAtLaunch: map[string]string{"env": "prod"},
	})
	require.NoError(t, err)
	require.True(t, created)

	// Second call with different fields must NOT overwrite.
	created, err = cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:          runID,
		Generation:     2,
		HubID:          "hub-2",
		ProjectID:      projectID,
		AgentID:        agentID,
		AgentSlug:      "a2",
		BillingUserID:  &userB,
		Harness:        "codex",
		HarnessConfig:  "codex",
		Template:       "tmpl-2",
		LabelsAtLaunch: map[string]string{"env": "mutated"},
	})
	require.NoError(t, err)
	require.False(t, created)

	got, err := cs.GetRunAttribution(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, got.BillingUserID)
	assert.Equal(t, userA, *got.BillingUserID)
	assert.Equal(t, "claude", got.Harness)
	assert.Equal(t, "tmpl-1", got.Template)
	assert.Equal(t, map[string]string{"env": "prod"}, got.LabelsAtLaunch)

	// Fill-once digests: first write succeeds.
	hcd := "sha256:cfg1"
	td := "sha256:tmpl1"
	hb := "2.1.0"
	require.NoError(t, cs.FillRunAttributionDigests(ctx, runID, store.RunAttributionDigestUpdate{
		HarnessConfigDigest: &hcd,
		TemplateDigest:      &td,
		HarnessBuild:        &hb,
	}))

	// Identical retry is idempotent.
	require.NoError(t, cs.FillRunAttributionDigests(ctx, runID, store.RunAttributionDigestUpdate{
		HarnessConfigDigest: &hcd,
		TemplateDigest:      &td,
		HarnessBuild:        &hb,
	}))

	// Conflicting digest is quarantined (returns ErrRunAttributionDigestConflict) and does not overwrite.
	otherHCD := "sha256:cfg-conflict"
	err = cs.FillRunAttributionDigests(ctx, runID, store.RunAttributionDigestUpdate{
		HarnessConfigDigest: &otherHCD,
	})
	assert.ErrorIs(t, err, store.ErrRunAttributionDigestConflict)

	got, err = cs.GetRunAttribution(ctx, runID)
	require.NoError(t, err)
	require.NotNil(t, got.HarnessConfigDigest)
	assert.Equal(t, hcd, *got.HarnessConfigDigest)

	// Unknown runID returns ErrNotFound.
	assert.ErrorIs(t, cs.FillRunAttributionDigests(ctx, "nonexistent", store.RunAttributionDigestUpdate{
		HarnessConfigDigest: &hcd,
	}), store.ErrNotFound)
}

func TestRunAttribution_AtomicOnMintSwapAdoptRevertAndLaunch(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "attr-lifecycle")
	userA := createTestUser(t, cs, "owner-a@example.com")
	userB := createTestUser(t, cs, "owner-b@example.com")

	ag := makeAgent(projectID, "worker-attr")
	ag.OwnerID = userA
	ag.CreatedBy = userA
	ag.Template = "dev-template"
	ag.AppliedConfig = &store.AgentAppliedConfig{
		Harness:       "claude",
		HarnessConfig: "my-claude-fast",
	}
	ag.RuntimeBrokerID = "broker-1"
	ag.Labels = map[string]string{"issue": "ptone/scion#9233"}
	ag.Phase = string(state.PhaseCreated)
	require.NoError(t, cs.CreateAgent(ctx, ag))

	// 1. With hook OFF (experiment off), SetAgentRunID writes NO run_attribution row.
	_, err := cs.SetAgentRunID(ctx, ag.ID, "run-off-1", nil)
	require.NoError(t, err)
	_, err = cs.GetRunAttribution(ctx, "run-off-1")
	assert.ErrorIs(t, err, store.ErrNotFound, "experiment off must write no run_attribution rows")

	// 2. Turn hook ON.
	cs.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: "hub-alpha"})

	// Fresh mint (start): creates run_attribution atomically with credential.
	cred := &store.AgentCredential{
		AgentID:      ag.ID,
		ProjectID:    projectID,
		TokenJTIHash: "hash-run-1",
		IssuedAt:     time.Now().UTC(),
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	}
	prev, err := cs.SetAgentRunID(ctx, ag.ID, "run-1", cred)
	require.NoError(t, err)
	assert.Equal(t, "run-off-1", prev)

	attr1, err := cs.GetRunAttribution(ctx, "run-1")
	require.NoError(t, err)
	assert.Equal(t, "hub-alpha", attr1.HubID)
	assert.Equal(t, projectID, attr1.ProjectID)
	assert.Equal(t, ag.ID, attr1.AgentID)
	assert.Equal(t, "worker-attr", attr1.AgentSlug)
	assert.Equal(t, "run-off-1", attr1.PriorRunID)
	require.NotNil(t, attr1.BillingUserID)
	assert.Equal(t, userA, *attr1.BillingUserID)
	assert.Equal(t, "claude", attr1.Harness)
	assert.Equal(t, "my-claude-fast", attr1.HarnessConfig)
	assert.Equal(t, "dev-template", attr1.Template)
	assert.Equal(t, map[string]string{"issue": "ptone/scion#9233"}, attr1.LabelsAtLaunch)
	assert.Empty(t, attr1.LaunchID)

	// BeginLaunch binds launch_id fill-once onto run_attribution(run-1).
	launchID, err := cs.BeginLaunchWithOptions(ctx, ag.ID, store.LaunchKindCreate, time.Minute, store.BeginLaunchOptions{
		RunID: "run-1",
	})
	require.NoError(t, err)
	attr1AfterLaunch, err := cs.GetRunAttribution(ctx, "run-1")
	require.NoError(t, err)
	assert.Equal(t, launchID, attr1AfterLaunch.LaunchID)

	// 3. Mutate agent owner & labels, then mint run-2 (resume/restart).
	loaded, err := cs.GetAgent(ctx, ag.ID)
	require.NoError(t, err)
	loaded.OwnerID = userB
	loaded.Labels = map[string]string{"issue": "ptone/scion#9233", "pr": "123"}
	require.NoError(t, cs.UpdateAgent(ctx, loaded))

	prev, err = cs.SetAgentRunID(ctx, ag.ID, "run-2", nil)
	require.NoError(t, err)
	assert.Equal(t, "run-1", prev)

	// run-2 captures the new owner and labels; run-1 stays frozen on userA.
	attr2, err := cs.GetRunAttribution(ctx, "run-2")
	require.NoError(t, err)
	require.NotNil(t, attr2.BillingUserID)
	assert.Equal(t, userB, *attr2.BillingUserID)
	assert.Equal(t, "run-1", attr2.PriorRunID)
	assert.Equal(t, "123", attr2.LabelsAtLaunch["pr"])

	attr1Again, err := cs.GetRunAttribution(ctx, "run-1")
	require.NoError(t, err)
	require.NotNil(t, attr1Again.BillingUserID)
	assert.Equal(t, userA, *attr1Again.BillingUserID, "historical run_attribution must remain immutable")
	assert.NotContains(t, attr1Again.LabelsAtLaunch, "pr")

	// 4. Swap / adopt back to run-1: run-1 keeps its original immutable row (ON CONFLICT DO NOTHING).
	swapped, err := cs.CompareAndSwapAgentRunID(ctx, ag.ID, "run-2", "run-1")
	require.NoError(t, err)
	require.True(t, swapped)
	attr1PostAdopt, err := cs.GetRunAttribution(ctx, "run-1")
	require.NoError(t, err)
	assert.Equal(t, userA, *attr1PostAdopt.BillingUserID)

	// 5. Swap / adopt to a pre-experiment run (run-off-1): creates its missing row atomically.
	swapped, err = cs.CompareAndSwapAgentRunID(ctx, ag.ID, "run-1", "run-off-1")
	require.NoError(t, err)
	require.True(t, swapped)
	attrAdopted, err := cs.GetRunAttribution(ctx, "run-off-1")
	require.NoError(t, err)
	assert.Equal(t, "run-off-1", attrAdopted.RunID)
	require.NotNil(t, attrAdopted.BillingUserID)
	assert.Equal(t, userB, *attrAdopted.BillingUserID)

	// 6. Revert (run-3 minted, then reverted to a pre-experiment run without a row):
	//    the newly inserted row for the restored run must have prior_run_id == "" (O5).
	_, err = cs.SetAgentRunID(ctx, ag.ID, "run-3", nil)
	require.NoError(t, err)
	reverted, err := cs.RevertAgentRunID(ctx, ag.ID, "run-3", "run-reverted-target")
	require.NoError(t, err)
	require.True(t, reverted)
	revertedAttr, err := cs.GetRunAttribution(ctx, "run-reverted-target")
	require.NoError(t, err)
	assert.Empty(t, revertedAttr.PriorRunID, "reverted run must not record the reverted run as its prior_run_id")

	// 7. Same-value settle skips attribution work cleanly.
	settled, err := cs.CompareAndSwapAgentRunID(ctx, ag.ID, "run-reverted-target", "run-reverted-target")
	require.NoError(t, err)
	require.True(t, settled)

	allAttrs, err := cs.ListRunAttributionsByAgent(ctx, ag.ID, 10)
	require.NoError(t, err)
	assert.Len(t, allAttrs, 5, "expected exactly 1 row each for run-1, run-2, run-off-1, run-3, run-reverted-target")
}

func TestRunAttribution_HarnessResolution(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })
	cs.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: "hub-harness"})

	projectID := createTestProject(t, cs, "attr-harness")

	cases := []struct {
		name              string
		appliedConfig     *store.AgentAppliedConfig
		effectiveHarness  string
		wantHarness       string
		wantHarnessConfig string
	}{
		{
			name: "custom config name with stamped AppliedConfig.Harness",
			appliedConfig: &store.AgentAppliedConfig{
				Harness:       "claude",
				HarnessConfig: "my-claude-fast",
			},
			wantHarness:       "claude",
			wantHarnessConfig: "my-claude-fast",
		},
		{
			name: "inline config harness canonicalized",
			appliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: "custom-gemini-cfg",
				InlineConfig:  &api.ScionConfig{Harness: "gemini"},
			},
			wantHarness:       "gemini-cli",
			wantHarnessConfig: "custom-gemini-cfg",
		},
		{
			name: "effective config harness fallback",
			appliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: "custom-codex-cfg",
			},
			effectiveHarness:  "codex",
			wantHarness:       "codex",
			wantHarnessConfig: "custom-codex-cfg",
		},
		{
			name: "pre-stamp agent with no harness stores empty string (never config name or env)",
			appliedConfig: &store.AgentAppliedConfig{
				HarnessConfig: "my-claude-fast",
				Env:           map[string]string{"SCION_HARNESS": "claude"},
			},
			wantHarness:       "",
			wantHarnessConfig: "my-claude-fast",
		},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ag := makeAgent(projectID, "harness-worker-"+uuid.NewString()[:8])
			ag.AppliedConfig = tc.appliedConfig
			require.NoError(t, cs.CreateAgent(ctx, ag))
			if tc.effectiveHarness != "" {
				// Populate effective_config via direct Ent update for this test case.
				agUID, err := parseUUID(ag.ID)
				require.NoError(t, err)
				_, err = cs.client.Agent.UpdateOneID(agUID).
					SetEffectiveConfig(`{"runId":"prev","harness":"` + tc.effectiveHarness + `"}`).
					Save(ctx)
				require.NoError(t, err)
			}
			runID := "run-harness-" + uuid.NewString()
			_, err := cs.SetAgentRunID(ctx, ag.ID, runID, nil)
			require.NoError(t, err)

			attr, err := cs.GetRunAttribution(ctx, runID)
			require.NoError(t, err)
			assert.Equal(t, tc.wantHarness, attr.Harness, "case %d", i)
			assert.Equal(t, tc.wantHarnessConfig, attr.HarnessConfig, "case %d", i)
		})
	}
}

type stubAgentWriteObserver struct {
	calls int
}

func (s *stubAgentWriteObserver) ObserveAgentWrite(_ context.Context, _ store.LifecycleTx, _ store.AgentWrite) error {
	s.calls++
	return nil
}

func TestRunAttribution_AtomicWithLifecycleObserver(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	obs := &stubAgentWriteObserver{}
	cs.SetAgentWriteObserver(obs)
	cs.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: "hub-obs"})

	projectID := createTestProject(t, cs, "attr-observer")
	userA := createTestUser(t, cs, "obs-owner@example.com")

	ag := makeAgent(projectID, "worker-obs")
	ag.OwnerID = userA
	ag.AppliedConfig = &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "my-claude"}
	ag.Phase = string(state.PhaseCreated)
	require.NoError(t, cs.CreateAgent(ctx, ag))

	// Mint with credential under observer.
	cred := &store.AgentCredential{
		AgentID:      ag.ID,
		ProjectID:    projectID,
		TokenJTIHash: "hash-obs-1",
		IssuedAt:     time.Now().UTC(),
		ExpiresAt:    time.Now().UTC().Add(time.Hour),
	}
	_, err := cs.SetAgentRunID(ctx, ag.ID, "obs-run-1", cred)
	require.NoError(t, err)

	attr1, err := cs.GetRunAttribution(ctx, "obs-run-1")
	require.NoError(t, err)
	assert.Equal(t, "claude", attr1.Harness)
	assert.Equal(t, "my-claude", attr1.HarnessConfig)
	assert.GreaterOrEqual(t, obs.calls, 1)

	// Swap/adopt to a different run under observer.
	swapped, err := cs.CompareAndSwapAgentRunID(ctx, ag.ID, "obs-run-1", "obs-run-2")
	require.NoError(t, err)
	require.True(t, swapped)
	attr2, err := cs.GetRunAttribution(ctx, "obs-run-2")
	require.NoError(t, err)
	assert.Equal(t, "obs-run-1", attr2.PriorRunID)

	// Revert under observer.
	reverted, err := cs.RevertAgentRunID(ctx, ag.ID, "obs-run-2", "obs-run-reverted")
	require.NoError(t, err)
	require.True(t, reverted)
	attrRev, err := cs.GetRunAttribution(ctx, "obs-run-reverted")
	require.NoError(t, err)
	assert.Empty(t, attrRev.PriorRunID)
}

func TestRunAttribution_RollbackOnAttributionFailure(t *testing.T) {
	injectedErr := errors.New("injected attribution insert failure")

	for _, withObserver := range []bool{false, true} {
		name := "without_observer"
		if withObserver {
			name = "with_observer"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cs := NewCompositeStore(enttest.NewClient(t))
			t.Cleanup(func() { _ = cs.Close() })

			if withObserver {
				cs.SetAgentWriteObserver(&stubAgentWriteObserver{})
			}
			cs.SetRunAttributionConfig(&store.RunAttributionConfig{HubID: "hub-rb"})

			projectID := createTestProject(t, cs, "attr-rollback")
			ag := makeAgent(projectID, "worker-rb")
			ag.AppliedConfig = &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"}
			require.NoError(t, cs.CreateAgent(ctx, ag))

			// Seed initial run-0 successfully.
			_, err := cs.SetAgentRunID(ctx, ag.ID, "run-0", nil)
			require.NoError(t, err)

			// Inject failure during run_attribution insert.
			cs.beforeRunAttributionInsert = func(_ context.Context, _ string) error {
				return injectedErr
			}

			// 1. SetAgentRunID with credential must roll back both run_id and credential.
			cred := &store.AgentCredential{
				AgentID:      ag.ID,
				ProjectID:    projectID,
				TokenJTIHash: "hash-rb-fail",
				IssuedAt:     time.Now().UTC(),
				ExpiresAt:    time.Now().UTC().Add(time.Hour),
			}
			_, err = cs.SetAgentRunID(ctx, ag.ID, "run-fail-mint", cred)
			require.ErrorIs(t, err, injectedErr)

			loaded, err := cs.GetAgent(ctx, ag.ID)
			require.NoError(t, err)
			assert.Equal(t, "run-0", loaded.RunID, "agent run_id must roll back on attribution failure")

			hasCred, err := cs.HasAgentCredentialForRun(ctx, ag.ID, "run-fail-mint")
			require.NoError(t, err)
			assert.False(t, hasCred, "agent credential must roll back on attribution failure")

			_, err = cs.GetRunAttribution(ctx, "run-fail-mint")
			assert.ErrorIs(t, err, store.ErrNotFound)

			// 2. CompareAndSwapAgentRunID must also roll back run_id on attribution failure.
			swapped, err := cs.CompareAndSwapAgentRunID(ctx, ag.ID, "run-0", "run-fail-swap")
			require.ErrorIs(t, err, injectedErr)
			assert.False(t, swapped)

			loaded, err = cs.GetAgent(ctx, ag.ID)
			require.NoError(t, err)
			assert.Equal(t, "run-0", loaded.RunID, "swap must roll back run_id on attribution failure")

			_, err = cs.GetRunAttribution(ctx, "run-fail-swap")
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}

func TestBackfillActiveRunAttributions_Paging(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "attr-paging")
	runIDs := make([]string, 5)
	for i := range runIDs {
		ag := makeAgent(projectID, "paged-"+uuid.NewString()[:8])
		ag.AppliedConfig = &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"}
		require.NoError(t, cs.CreateAgent(ctx, ag))
		runIDs[i] = "paged-run-" + uuid.NewString()
		_, err := cs.SetAgentRunID(ctx, ag.ID, runIDs[i], nil)
		require.NoError(t, err)
	}

	// Page size 2 forces 3 pages (2 + 2 + 1).
	n, err := cs.backfillActiveRunAttributionsPaged(ctx, "hub-paged", 2)
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	for _, rid := range runIDs {
		attr, err := cs.GetRunAttribution(ctx, rid)
		require.NoError(t, err)
		assert.Equal(t, "claude", attr.Harness)
	}

	// Second pass finds 0 via NOT EXISTS filter.
	n, err = cs.backfillActiveRunAttributionsPaged(ctx, "hub-paged", 2)
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestBackfillActiveRunAttributions_Idempotent(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	projectID := createTestProject(t, cs, "attr-backfill")
	userA := createTestUser(t, cs, "backfill-a@example.com")

	// Agent 1: active with owner user, no attribution row yet.
	ag1 := makeAgent(projectID, "bf-1")
	ag1.OwnerID = userA
	ag1.HarnessConfig = "claude"
	require.NoError(t, cs.CreateAgent(ctx, ag1))
	_, err := cs.SetAgentRunID(ctx, ag1.ID, "bf-run-1", nil)
	require.NoError(t, err)

	// Agent 2: active principal-less agent, no attribution row yet.
	ag2 := makeAgent(projectID, "bf-2")
	ag2.HarnessConfig = "gemini-cli"
	require.NoError(t, cs.CreateAgent(ctx, ag2))
	_, err = cs.SetAgentRunID(ctx, ag2.ID, "bf-run-2", nil)
	require.NoError(t, err)

	// Agent 3: active, empty run_id (must be skipped).
	ag3 := makeAgent(projectID, "bf-3")
	require.NoError(t, cs.CreateAgent(ctx, ag3))

	// Agent 4: soft-deleted with run_id (must be skipped).
	ag4 := makeAgent(projectID, "bf-4")
	require.NoError(t, cs.CreateAgent(ctx, ag4))
	_, err = cs.SetAgentRunID(ctx, ag4.ID, "bf-run-4", nil)
	require.NoError(t, err)
	now := time.Now().UTC()
	loaded4, err := cs.GetAgent(ctx, ag4.ID)
	require.NoError(t, err)
	loaded4.DeletedAt = now
	require.NoError(t, cs.UpdateAgent(ctx, loaded4))

	// Agent 5: active, already has an attribution row (must remain untouched).
	ag5 := makeAgent(projectID, "bf-5")
	ag5.OwnerID = userA
	require.NoError(t, cs.CreateAgent(ctx, ag5))
	_, err = cs.SetAgentRunID(ctx, ag5.ID, "bf-run-5", nil)
	require.NoError(t, err)
	created, err := cs.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:     "bf-run-5",
		HubID:     "pre-existing-hub",
		ProjectID: projectID,
		AgentID:   ag5.ID,
		Harness:   "preserved-harness",
	})
	require.NoError(t, err)
	require.True(t, created)

	// First backfill: inserts exactly bf-run-1 and bf-run-2.
	n, err := cs.BackfillActiveRunAttributions(ctx, "hub-bf")
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	a1, err := cs.GetRunAttribution(ctx, "bf-run-1")
	require.NoError(t, err)
	require.NotNil(t, a1.BillingUserID)
	assert.Equal(t, userA, *a1.BillingUserID)
	assert.Equal(t, "hub-bf", a1.HubID)

	a2, err := cs.GetRunAttribution(ctx, "bf-run-2")
	require.NoError(t, err)
	assert.Nil(t, a2.BillingUserID, "principal-less agent must backfill with NULL billing_user_id")

	_, err = cs.GetRunAttribution(ctx, "bf-run-4")
	assert.ErrorIs(t, err, store.ErrNotFound, "soft-deleted agent must not be backfilled")

	a5, err := cs.GetRunAttribution(ctx, "bf-run-5")
	require.NoError(t, err)
	assert.Equal(t, "preserved-harness", a5.Harness, "pre-existing row must not be overwritten")

	// Second backfill: idempotent, returns 0.
	n, err = cs.BackfillActiveRunAttributions(ctx, "hub-bf")
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// A new run minted while the hook is off gets picked up on a subsequent backfill
	// (e.g. OFF -> ON -> OFF -> ON transition), without touching earlier rows.
	_, err = cs.SetAgentRunID(ctx, ag1.ID, "bf-run-1-later", nil)
	require.NoError(t, err)
	n, err = cs.BackfillActiveRunAttributions(ctx, "hub-bf-2")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	laterAttr, err := cs.GetRunAttribution(ctx, "bf-run-1-later")
	require.NoError(t, err)
	assert.Equal(t, "hub-bf-2", laterAttr.HubID)
}

func TestMeterHealth_UpsertMonotonic(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	agentID := uuid.NewString()
	runID := "run-mh-1"
	t1 := time.Now().UTC().Truncate(time.Microsecond)
	t2 := t1.Add(10 * time.Second)

	require.NoError(t, cs.UpsertMeterHealth(ctx, &store.MeterHealth{
		AgentID:           agentID,
		RunID:             runID,
		ProducerEpoch:     "ep-1",
		LastReceiptAt:     t1,
		LastContiguousSeq: 10,
		MaxSeqSeen:        15,
		MissingRanges:     []store.SeqRange{{Start: 11, End: 14}},
		AnomalousCount:    1,
		FinalState:        store.MeterHealthFinalStateOpen,
	}))

	// Out-of-order / regressive update must not move seqs or timestamps backward.
	require.NoError(t, cs.UpsertMeterHealth(ctx, &store.MeterHealth{
		AgentID:             agentID,
		RunID:               runID,
		ProducerEpoch:       "ep-1",
		LastReceiptAt:       t1.Add(-time.Minute),
		LastContiguousSeq:   5,
		MaxSeqSeen:          12,
		MissingRanges:       []store.SeqRange{{Start: 6, End: 14}},
		AnomalousCount:      0,
		SchemaDriftDetected: true,
		FinalState:          store.MeterHealthFinalStateFinal,
	}))

	rows, err := cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, int64(10), rows[0].LastContiguousSeq)
	assert.Equal(t, int64(15), rows[0].MaxSeqSeen)
	assert.Equal(t, []store.SeqRange{{Start: 11, End: 14}}, rows[0].MissingRanges)
	assert.Equal(t, int64(1), rows[0].AnomalousCount)
	assert.True(t, rows[0].SchemaDriftDetected)
	assert.Equal(t, store.MeterHealthFinalStateFinal, rows[0].FinalState)

	// Once final, final_state must not transition to incomplete or open.
	require.NoError(t, cs.UpsertMeterHealth(ctx, &store.MeterHealth{
		AgentID:           agentID,
		RunID:             runID,
		ProducerEpoch:     "ep-1",
		LastReceiptAt:     t2,
		LastContiguousSeq: 15,
		MaxSeqSeen:        15,
		MissingRanges:     []store.SeqRange{},
		FinalState:        store.MeterHealthFinalStateIncomplete,
	}))

	rows, err = cs.GetMeterHealth(ctx, agentID, runID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, store.MeterHealthFinalStateFinal, rows[0].FinalState)
	assert.Equal(t, int64(15), rows[0].LastContiguousSeq)
	assert.Empty(t, rows[0].MissingRanges)
}
