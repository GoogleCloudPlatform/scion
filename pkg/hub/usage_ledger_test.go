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

package hub

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func setUsageLedgerExperimentForTest(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.TokenomicsUsageLedger {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
}

func TestUsageLedger_ExperimentGatingAndBackfill(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-tokenomics-test")

	uls, ok := s.(usageLedgerHookStore)
	require.True(t, ok, "ent store must implement usageLedgerHookStore")

	// Create a project, user, and agent.
	user := &store.User{
		ID:          uuid.NewString(),
		Email:       "ledger-user@example.com",
		DisplayName: "Ledger User",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "ledger-proj",
		Slug: "ledger-proj-" + uuid.NewString()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "ledger-agent",
		Name:          "ledger-agent",
		ProjectID:     proj.ID,
		OwnerID:       user.ID,
		CreatedBy:     user.ID,
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "claude"},
		Template:      "default",
		Phase:         "created",
	}
	require.NoError(t, s.CreateAgent(ctx, ag))

	// 1. Experiment is OFF by default: reconcile leaves hook uninstalled and mint writes nothing.
	srv.reconcileUsageLedger(ctx)
	assert.False(t, uls.RunAttributionConfigInstalled())

	_, err := s.SetAgentRunID(ctx, ag.ID, "run-before-enable", nil)
	require.NoError(t, err)
	_, err = s.GetRunAttribution(ctx, "run-before-enable")
	assert.ErrorIs(t, err, store.ErrNotFound, "no run_attribution row while tokenomics.usage_ledger is off")

	// 2. Enable tokenomics.usage_ledger and reconcile: hook installs AND active run is backfilled.
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)
	assert.True(t, uls.RunAttributionConfigInstalled())

	backfilled, err := s.GetRunAttribution(ctx, "run-before-enable")
	require.NoError(t, err)
	assert.Equal(t, "hub-tokenomics-test", backfilled.HubID)
	require.NotNil(t, backfilled.BillingUserID)
	assert.Equal(t, user.ID, *backfilled.BillingUserID)

	// 3. New run minted while experiment is ON gets an atomic run_attribution row.
	_, err = s.SetAgentRunID(ctx, ag.ID, "run-while-enabled", nil)
	require.NoError(t, err)
	live, err := s.GetRunAttribution(ctx, "run-while-enabled")
	require.NoError(t, err)
	assert.Equal(t, "run-before-enable", live.PriorRunID)
	assert.Equal(t, "hub-tokenomics-test", live.HubID)
	require.NotNil(t, live.BillingUserID)
	assert.Equal(t, user.ID, *live.BillingUserID)

	// 4. Disable tokenomics.usage_ledger and reconcile: hook clears, new runs write nothing.
	setUsageLedgerExperimentForTest(t, srv, false)
	srv.reconcileUsageLedger(ctx)
	assert.False(t, uls.RunAttributionConfigInstalled())

	_, err = s.SetAgentRunID(ctx, ag.ID, "run-after-disable", nil)
	require.NoError(t, err)
	_, err = s.GetRunAttribution(ctx, "run-after-disable")
	assert.ErrorIs(t, err, store.ErrNotFound, "no run_attribution row after tokenomics.usage_ledger is disabled")

	// 5. Re-enable tokenomics.usage_ledger (OFF -> ON -> OFF -> ON): backfill attributes run-after-disable.
	setUsageLedgerExperimentForTest(t, srv, true)
	srv.reconcileUsageLedger(ctx)
	assert.True(t, uls.RunAttributionConfigInstalled())

	reEnabled, err := s.GetRunAttribution(ctx, "run-after-disable")
	require.NoError(t, err)
	assert.Equal(t, "hub-tokenomics-test", reEnabled.HubID)
	require.NotNil(t, reEnabled.BillingUserID)
	assert.Equal(t, user.ID, *reEnabled.BillingUserID)
}

func TestUsageLedger_BackfillRetryAfterErrorAndAsync(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-retry-test")

	uls, ok := s.(usageLedgerHookStore)
	require.True(t, ok)

	user := &store.User{
		ID:          uuid.NewString(),
		Email:       "retry-user@example.com",
		DisplayName: "Retry User",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))
	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "retry-proj",
		Slug: "retry-proj-" + uuid.NewString()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "retry-agent",
		Name:          "retry-agent",
		ProjectID:     proj.ID,
		OwnerID:       user.ID,
		CreatedBy:     user.ID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "my-claude-fast"},
		Template:      "default",
		Phase:         "created",
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-needs-retry", nil)
	require.NoError(t, err)

	// Inject a transient error on the first backfill call.
	var calls atomic.Int32
	srv.usageLedger.backfillFn = func(bctx context.Context, hubID string) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("transient db error")
		}
		return uls.BackfillActiveRunAttributions(bctx, hubID)
	}

	setUsageLedgerExperimentForTest(t, srv, true)

	// First reconcile: writer installs, backfill fails, backfillSucceeded remains false.
	srv.reconcileUsageLedger(ctx)
	assert.True(t, uls.RunAttributionConfigInstalled())
	assert.Equal(t, int32(1), calls.Load())
	_, err = s.GetRunAttribution(ctx, "run-needs-retry")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Second reconcile (no restart, no OFF->ON toggle): async retry succeeds off the caller path.
	srv.reconcileUsageLedgerAsync(ctx)
	require.Eventually(t, func() bool {
		srv.usageLedger.mu.Lock()
		succeeded := srv.usageLedger.backfillSucceeded && !srv.usageLedger.backfillRunning
		srv.usageLedger.mu.Unlock()
		if !succeeded {
			return false
		}
		attr, getErr := s.GetRunAttribution(ctx, "run-needs-retry")
		return getErr == nil && attr.Harness == "claude" && attr.HarnessConfig == "my-claude-fast"
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(2), calls.Load())

	// Third reconcile: backfillSucceeded is now true, so backfillFn is not called again.
	srv.reconcileUsageLedger(ctx)
	assert.Equal(t, int32(2), calls.Load())
	srv.stopUsageLedger()
}

func TestUsageLedger_OffOnDuringInFlightBackfill(t *testing.T) {
	for _, cancelledReturnsSuccess := range []bool{false, true} {
		name := "cancelled_returns_error"
		if cancelledReturnsSuccess {
			name = "cancelled_returns_success"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			srv, s := testServer(t)
			srv.SetHubID("hub-race-test")

			uls, ok := s.(usageLedgerHookStore)
			require.True(t, ok)

			proj := &store.Project{
				ID:   uuid.NewString(),
				Name: "race-proj-" + name,
				Slug: "race-proj-" + uuid.NewString()[:8],
			}
			require.NoError(t, s.CreateProject(ctx, proj))
			ag := &store.Agent{
				ID:            uuid.NewString(),
				Slug:          "race-agent",
				Name:          "race-agent",
				ProjectID:     proj.ID,
				AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "my-claude"},
				Phase:         "created",
			}
			require.NoError(t, s.CreateAgent(ctx, ag))

			started := make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			srv.usageLedger.backfillFn = func(bctx context.Context, hubID string) (int, error) {
				n := calls.Add(1)
				if n == 1 {
					close(started)
					<-release
					if cancelledReturnsSuccess {
						return 0, nil
					}
					return 0, bctx.Err()
				}
				return uls.BackfillActiveRunAttributions(bctx, hubID)
			}

			// 1. ON: async backfill starts and blocks on release.
			setUsageLedgerExperimentForTest(t, srv, true)
			srv.reconcileUsageLedgerAsync(ctx)
			<-started

			// 2. OFF while backfill #1 is still unwinding.
			setUsageLedgerExperimentForTest(t, srv, false)
			srv.reconcileUsageLedgerAsync(ctx)
			assert.False(t, uls.RunAttributionConfigInstalled())

			// 3. Mint a run while OFF (must not get a row yet).
			_, err := s.SetAgentRunID(ctx, ag.ID, "run-minted-while-off", nil)
			require.NoError(t, err)
			_, err = s.GetRunAttribution(ctx, "run-minted-while-off")
			assert.ErrorIs(t, err, store.ErrNotFound)

			// 4. Switch back ON while backfill #1 is still unwinding.
			setUsageLedgerExperimentForTest(t, srv, true)
			srv.reconcileUsageLedgerAsync(ctx)
			assert.True(t, uls.RunAttributionConfigInstalled())

			// 5. Release backfill #1: its exit must launch generation #3's backfill.
			close(release)

			require.Eventually(t, func() bool {
				attr, getErr := s.GetRunAttribution(ctx, "run-minted-while-off")
				return getErr == nil && attr.HubID == "hub-race-test" && attr.Harness == "claude"
			}, 5*time.Second, 10*time.Millisecond, "run minted during OFF window must be backfilled after unwinding backfill exits")
			assert.GreaterOrEqual(t, calls.Load(), int32(2))
			srv.stopUsageLedger()
		})
	}
}

func TestUsageLedger_PeriodicRetryAfterFailure(t *testing.T) {
	ctx := context.Background()
	srv, s := testServer(t)
	srv.SetHubID("hub-periodic-retry")
	srv.usageLedger.retryInterval = 20 * time.Millisecond

	uls, ok := s.(usageLedgerHookStore)
	require.True(t, ok)

	proj := &store.Project{
		ID:   uuid.NewString(),
		Name: "periodic-proj",
		Slug: "periodic-proj-" + uuid.NewString()[:8],
	}
	require.NoError(t, s.CreateProject(ctx, proj))
	ag := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "periodic-agent",
		Name:          "periodic-agent",
		ProjectID:     proj.ID,
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", HarnessConfig: "claude"},
		Phase:         "created",
	}
	require.NoError(t, s.CreateAgent(ctx, ag))
	_, err := s.SetAgentRunID(ctx, ag.ID, "run-periodic-retry", nil)
	require.NoError(t, err)

	var calls atomic.Int32
	srv.usageLedger.backfillFn = func(bctx context.Context, hubID string) (int, error) {
		if calls.Add(1) == 1 {
			return 0, errors.New("transient error")
		}
		return uls.BackfillActiveRunAttributions(bctx, hubID)
	}

	setUsageLedgerExperimentForTest(t, srv, true)
	// Trigger reconcile ONCE; periodic retry timer must recover without a second reconcile call.
	srv.reconcileUsageLedger(ctx)
	assert.Equal(t, int32(1), calls.Load())

	require.Eventually(t, func() bool {
		attr, getErr := s.GetRunAttribution(ctx, "run-periodic-retry")
		return getErr == nil && attr.HubID == "hub-periodic-retry"
	}, 5*time.Second, 10*time.Millisecond, "periodic retry timer must backfill after transient failure")
	srv.stopUsageLedger()
	assert.GreaterOrEqual(t, calls.Load(), int32(2))
}
