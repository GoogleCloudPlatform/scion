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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBrokerQuota_MigratedSettingEnforced is the end-to-end proof for
// ptone/scion#2061 P2-D4 / design.md §5.7: once a broker's
// max_agents_per_broker entitlement binding has been migrated into a
// broker_settings row (cmd's runBrokerQuotaBindingsToSettingsMigration,
// exercised directly in cmd/boot_broker_quota_bindings_to_settings_test.go),
// Reserve enforces that setting through the real create-agent/start HTTP
// path — and the setting wins over a much larger hub-wide default, which is
// the whole point of a per-broker override.
//
// This test does not re-invoke the cmd-package migration function (hub
// cannot import cmd without a cycle); it writes the exact
// store.BrokerSettings document the migration would have produced for a
// single broker-scoped binding of value 1, which is the migration's entire
// contract with the rest of the system (design.md §5.2: the broker setting,
// once written, is indistinguishable from one an admin set by hand).
func TestBrokerQuota_MigratedSettingEnforced(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	disp := &quotaLifecycleDispatcher{}
	srv.SetDispatcher(disp)

	// Hub-wide default is generous — the migrated per-broker setting must
	// still win.
	setBrokerAgentCeiling(t, s, 100)

	broker, project := newQuotaTestBrokerAndProject(t, s, "migrated-setting")

	// Simulate the migration's write: a broker-scoped binding of value 1
	// becomes settings.maxAgents = 1, attributed to the migration.
	migratedValue := int64(1)
	_, err := s.PutBrokerSettings(ctx, broker.ID, store.BrokerSettings{MaxAgents: &migratedValue}, 0, "migration:ptone/scion#2061")
	require.NoError(t, err)

	// occupant holds the only slot the migrated setting allows.
	occupant := newQuotaTestAgent(t, s, broker, project, "migrated-occupant", state.PhaseRunning)
	reserveBrokerSlot(t, s, broker, occupant.ID)

	candidate := newQuotaTestAgent(t, s, broker, project, "migrated-candidate", state.PhaseStopped)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+candidate.ID+"/start", nil)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())

	got, err := s.GetAgent(ctx, candidate.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "rejected start must not change the agent's phase")
}
