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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A launch-ending store write releases the agent's max_agents_per_broker
// reservation only when it is scoped to the agent's own Runtime Broker
// (ptone/scion#5137): a reservation the agent holds on another broker
// survives the launch end, and one on its own broker is still released. One
// case per write that releases in the write: a failed report on an active
// launch whose phase another writer moved to error (L4a), a failed report on
// a pre-running launch (L6), a deadline reap (L7) and a staleness reap (L8).
func TestLaunchEnd_ReleaseScopedToAgentBroker(t *testing.T) {
	ctx := context.Background()
	ends := []struct {
		name string
		// prepare runs after the launch began and the reservation was
		// seeded, before the launch is ended with how.
		prepare func(t *testing.T, s *AgentStore, a *store.Agent)
		how     string
	}{
		{"L4a failed report on an error phase", func(t *testing.T, s *AgentStore, a *store.Agent) {
			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			got.Phase = "error"
			got.Message = "some other failure"
			require.NoError(t, s.UpdateAgent(ctx, got))
		}, "report_failed"},
		{"L6 failed report before running", nil, "report_failed"},
		{"L7 deadline reap", nil, "reaper_deadline"},
		{"L8 staleness reap", nil, "reaper_lost"},
	}
	scopes := []struct {
		name     string
		scope    string
		released bool
		why      string
	}{
		{"agent's broker", testQuotaBroker, true, "a launch end releases the reservation on the agent's own broker"},
		{"another broker", "broker-2", false, "a launch end keeps the reservation on another broker"},
	}
	for _, end := range ends {
		for _, sc := range scopes {
			t.Run(end.name+"/"+sc.name, func(t *testing.T) {
				s, projectID := newTestAgentStore(t)
				a, launchID := beginTestLaunch(t, ctx, s, projectID, "scope-end")
				setAgentRuntimeBroker(t, ctx, s, a, testQuotaBroker)
				quota := NewQuotaStore(s.client)
				ld, err := quota.CreateLimitDefinition(ctx, &store.LimitDefinition{
					Name: store.LimitMaxAgentsPerBroker, ResourceType: "agent", Unit: "count", DefaultValue: 1,
				})
				require.NoError(t, err)
				seeded, err := quota.CreateUsageReservation(ctx, &store.UsageReservation{
					LimitDefinitionID: ld.ID, SubjectID: sc.scope, ScopeType: store.QuotaScopeBroker, ScopeID: sc.scope,
					ResourceID: a.ID, Reserved: 1,
				})
				require.NoError(t, err)
				if end.prepare != nil {
					end.prepare(t, s, a)
				}

				endStartLaunchByFailure(t, ctx, s, a, launchID, end.how)

				got, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				require.Equal(t, store.LaunchStateEnded, got.LaunchState, "setup: the launch ended")
				has, err := quota.HasActiveReservation(ctx, ld.ID, a.ID)
				require.NoError(t, err)
				assert.Equal(t, !sc.released, has, sc.why)
				if !sc.released {
					kept, err := quota.GetActiveReservation(ctx, ld.ID, a.ID)
					require.NoError(t, err)
					assert.Equal(t, seeded.ID, kept.ID, sc.why)
					assert.Equal(t, sc.scope, kept.ScopeID, sc.why)
				}
			})
		}
	}
}

// An agent row with no Runtime Broker releases nothing at a launch end: the
// scope predicate names the row's broker, and no broker-scoped reservation
// has an empty broker ID, so a reservation the agent holds (here on
// testQuotaBroker) is kept for the sweep. Only the reaper's writes are
// covered: a launch report is accepted only from the row's own broker.
func TestLaunchEnd_AgentWithoutBrokerReleasesNothing(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"reaper_deadline", "reaper_lost"} {
		t.Run(how, func(t *testing.T) {
			s, projectID := newTestAgentStore(t)
			a, launchID := beginTestLaunch(t, ctx, s, projectID, "scope-none")
			require.Empty(t, a.RuntimeBrokerID, "setup: the agent has no Runtime Broker")
			quota := NewQuotaStore(s.client)
			ld, err := quota.CreateLimitDefinition(ctx, &store.LimitDefinition{
				Name: store.LimitMaxAgentsPerBroker, ResourceType: "agent", Unit: "count", DefaultValue: 1,
			})
			require.NoError(t, err)
			seeded, err := quota.CreateUsageReservation(ctx, &store.UsageReservation{
				LimitDefinitionID: ld.ID, SubjectID: testQuotaBroker, ScopeType: store.QuotaScopeBroker, ScopeID: testQuotaBroker,
				ResourceID: a.ID, Reserved: 1,
			})
			require.NoError(t, err)

			endStartLaunchByFailure(t, ctx, s, a, launchID, how)

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, store.LaunchStateEnded, got.LaunchState, "setup: the launch ended")
			kept, err := quota.GetActiveReservation(ctx, ld.ID, a.ID)
			require.NoError(t, err, "an agent without a broker releases nothing at a launch end")
			assert.Equal(t, seeded.ID, kept.ID, "an agent without a broker releases nothing at a launch end")
		})
	}
}

// The Postgres counterpart of the other-broker rows: on Postgres too, a
// failed report before running (L6) and a deadline reap (L7) keep the
// reservation the agent holds on another Runtime Broker. Skipped without a
// Postgres URL (fork CI runs it).
func TestLaunchEnd_OtherBrokerReservationKeptOnPG(t *testing.T) {
	ctx := context.Background()
	for _, how := range []string{"report_failed", "reaper_deadline"} {
		t.Run(how, func(t *testing.T) {
			s, projectID := newPGTestAgentStore(t, 2)
			a, launchID := beginTestLaunch(t, ctx, s, projectID, "scope-pg-"+how)
			setAgentRuntimeBroker(t, ctx, s, a, testQuotaBroker)
			quota := NewQuotaStore(s.client)
			ld, err := quota.CreateLimitDefinition(ctx, &store.LimitDefinition{
				Name: store.LimitMaxAgentsPerBroker, ResourceType: "agent", Unit: "count", DefaultValue: 1,
			})
			require.NoError(t, err)
			seeded, err := quota.CreateUsageReservation(ctx, &store.UsageReservation{
				LimitDefinitionID: ld.ID, SubjectID: "broker-2", ScopeType: store.QuotaScopeBroker, ScopeID: "broker-2",
				ResourceID: a.ID, Reserved: 1,
			})
			require.NoError(t, err)

			endStartLaunchByFailure(t, ctx, s, a, launchID, how)

			got, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			require.Equal(t, store.LaunchStateEnded, got.LaunchState, "setup: the launch ended")
			kept, err := quota.GetActiveReservation(ctx, ld.ID, a.ID)
			require.NoError(t, err, "a launch end keeps the reservation on another broker")
			assert.Equal(t, seeded.ID, kept.ID, "a launch end keeps the reservation on another broker")
		})
	}
}
