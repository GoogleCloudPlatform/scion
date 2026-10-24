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
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Deregistering a Runtime Broker releases its active broker-scoped
// reservations in the same request, each counted and audited as the sweep's
// deregistered-scope pass does, without waiting for the sweep; another
// broker's reservation stays active (ptone/scion#3994).
func TestDeleteRuntimeBroker_ReleasesItsReservations_Audited(t *testing.T) {
	srv, s := testServer(t)
	grantDevUserRuntimeBrokerAccess(t, s) // the DELETE endpoint checks runtime_broker permissions
	_, deregistered := capacityCounters(t, srv)
	gone, project := newQuotaTestBrokerAndProject(t, s, "delete-releases-gone")
	kept, keptProject := newQuotaTestBrokerAndProject(t, s, "delete-releases-kept")
	a1 := newQuotaTestAgent(t, s, gone, project, "delete-releases-a1", state.PhaseRunning)
	a2 := newQuotaTestAgent(t, s, gone, project, "delete-releases-a2", state.PhaseRunning)
	other := newQuotaTestAgent(t, s, kept, keptProject, "delete-releases-other", state.PhaseRunning)
	seedSlot(t, s, gone.ID, a1.ID)
	seedSlot(t, s, gone.ID, a2.ID)
	seedSlot(t, s, kept.ID, other.ID)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/runtime-brokers/"+gone.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	for _, a := range []*store.Agent{a1, a2} {
		assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID),
			"%s: a deleted broker's reservation is released by the delete", a.Slug)
		audits := capacityAudits(t, s, auditQuotaDeregisteredScopeRelease, a.ID)
		require.Len(t, audits, 1, a.Slug)
		assert.Contains(t, audits[0].AfterSummary, gone.ID)
	}
	assert.EqualValues(t, 2, deregistered(), "each release is counted")
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, other.ID), "another broker's reservation stays active")
	assert.Empty(t, brokerReservationIDs(t, s, gone.ID), "no active reservation points at the deleted broker")
}
