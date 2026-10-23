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
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const initiatorFilterAgentID = "initiator-filter-agent"

// initiatorFilterAttributions are the attributions of the fixture rows by
// name. Only "held" is held by initiatorFilterAgentID with a recorded
// attribution.
func initiatorFilterAttributions() map[string]store.InitiatorAttribution {
	agent := func(id, cred string, version int) store.InitiatorAttribution {
		return store.InitiatorAttribution{
			InitiatorPrincipalKind:  store.DelegationPrincipalAgent,
			InitiatorPrincipalID:    id,
			InitiatorCredentialKind: cred,
			AttributionVersion:      version,
			AuthorizationRevision:   1,
		}
	}
	return map[string]store.InitiatorAttribution{
		"held":             agent(initiatorFilterAgentID, store.InitiatorCredentialKindAgent, 1),
		"other-agent":      agent("initiator-filter-other", store.InitiatorCredentialKindAgent, 1),
		"legacy-version":   agent(initiatorFilterAgentID, store.InitiatorCredentialKindAgent, 0),
		"legacy-cred":      agent(initiatorFilterAgentID, store.InitiatorCredentialKindLegacyUnknown, 1),
		"none":             {},
		"user-same-id":     {InitiatorPrincipalKind: store.DelegationPrincipalUser, InitiatorPrincipalID: initiatorFilterAgentID, InitiatorCredentialKind: store.InitiatorCredentialKindSession, AttributionVersion: 1},
		"agent-blank-cred": agent(initiatorFilterAgentID, "", 1),
	}
}

// InitiatorAgentID keeps only the schedules the agent holds with a
// recorded attribution, and the count covers only those rows.
func TestListSchedulesFiltersByInitiatorAgent(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	ids := map[string]string{}
	for name, attr := range initiatorFilterAttributions() {
		sc := newTestSchedule(projectID, name)
		sc.InitiatorAttribution = attr
		require.NoError(t, s.CreateSchedule(ctx, sc))
		ids[name] = sc.ID
	}

	res, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID, InitiatorAgentID: initiatorFilterAgentID}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, ids["held"], res.Items[0].ID)
	assert.Equal(t, 1, res.TotalCount)
	assert.Empty(t, res.NextCursor)

	all, err := s.ListSchedules(ctx, store.ScheduleFilter{ProjectID: projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, all.Items, len(ids), "without the filter every row is listed")
}

// InitiatorAgentID keeps only the scheduled events the agent holds with a
// recorded attribution.
func TestListScheduledEventsFiltersByInitiatorAgent(t *testing.T) {
	s := newTestScheduleStore(t)
	ctx := context.Background()
	projectID := uuid.NewString()
	ids := map[string]string{}
	for name, attr := range initiatorFilterAttributions() {
		evt := newTestScheduledEvent(projectID)
		evt.InitiatorAttribution = attr
		require.NoError(t, s.CreateScheduledEvent(ctx, evt))
		ids[name] = evt.ID
	}

	res, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID, InitiatorAgentID: initiatorFilterAgentID}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	require.Len(t, res.Items, 1)
	assert.Equal(t, ids["held"], res.Items[0].ID)
	assert.Equal(t, 1, res.TotalCount)
	assert.Empty(t, res.NextCursor)

	all, err := s.ListScheduledEvents(ctx, store.ScheduledEventFilter{ProjectID: projectID}, store.ListOptions{Limit: 100})
	require.NoError(t, err)
	assert.Len(t, all.Items, len(ids), "without the filter every row is listed")
}
