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
	"testing"

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
