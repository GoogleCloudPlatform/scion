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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The broker-scoped reservation views and the broker-quota reconcile read
// every broker's reservations with one store query (ptone/scion#2314). These
// tests pin the result set those views had with the former one-query-per-
// broker loop: only listed brokers contribute (a reservation whose scope ID is
// not a runtime broker, for example a deleted broker, is left out), and rows
// are in broker-list order (newest broker first), created_at ascending
// within a broker.

type brokerScopedFixture struct {
	def          *store.LimitDefinition
	olderBroker  *store.RuntimeBroker
	newerBroker  *store.RuntimeBroker
	unlistedID   string
	wantOrder    []string // resource IDs, in the expected response order
	unlistedRows []string // resource IDs scoped to unlistedID
}

func seedBrokerScopedFixture(t *testing.T, s store.Store) brokerScopedFixture {
	t.Helper()
	ctx := context.Background()

	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	mkBroker := func(name string, created time.Time) *store.RuntimeBroker {
		b := &store.RuntimeBroker{
			ID:      uuid.New().String(),
			Name:    name,
			Slug:    uuid.New().String(),
			Status:  store.BrokerStatusOnline,
			Created: created,
			Updated: created,
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		return b
	}
	older := mkBroker("scoped-older-broker", base)
	newer := mkBroker("scoped-newer-broker", base.Add(time.Minute))
	unlistedID := uuid.New().String()

	reserve := func(scopeID, resourceID string, offset time.Duration) string {
		_, err := s.CreateUsageReservation(ctx, &store.UsageReservation{
			LimitDefinitionID: def.ID,
			SubjectID:         scopeID,
			ScopeType:         store.QuotaScopeBroker,
			ScopeID:           scopeID,
			ResourceID:        resourceID,
			Reserved:          1,
			CreatedAt:         base.Add(offset),
		})
		require.NoError(t, err)
		return resourceID
	}

	// Inserted out of order on purpose.
	olderLate := reserve(older.ID, "scoped-older-late", 4*time.Minute)
	unlisted := reserve(unlistedID, "scoped-unlisted", 1*time.Minute)
	newerOnly := reserve(newer.ID, "scoped-newer", 3*time.Minute)
	olderEarly := reserve(older.ID, "scoped-older-early", 2*time.Minute)

	return brokerScopedFixture{
		def:          def,
		olderBroker:  older,
		newerBroker:  newer,
		unlistedID:   unlistedID,
		wantOrder:    []string{newerOnly, olderEarly, olderLate},
		unlistedRows: []string{unlisted},
	}
}

func TestGetUsageByLimit_MaxAgentsPerBroker_ListedBrokersOnlyInBrokerOrder(t *testing.T) {
	srv, s := testServer(t)
	f := seedBrokerScopedFixture(t, s)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage/"+f.def.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageByLimitResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, len(f.wantOrder), resp.TotalActive, "a reservation whose scope ID is not a listed broker is not counted")

	got := make([]string, len(resp.Reservations))
	for i, r := range resp.Reservations {
		got[i] = r.ResourceID
	}
	assert.Equal(t, f.wantOrder, got, "newest broker first, created_at ascending within a broker")
}

func TestGetUsageSummary_MaxAgentsPerBroker_ListedBrokersOnly(t *testing.T) {
	srv, s := testServer(t)
	f := seedBrokerScopedFixture(t, s)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/usage", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp usageSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var found *usageSummaryEntry
	for i := range resp.Items {
		if resp.Items[i].LimitDefinition.ID == f.def.ID {
			found = &resp.Items[i]
			break
		}
	}
	require.NotNil(t, found)
	assert.Equal(t, len(f.wantOrder), found.ActiveCount, "a reservation whose scope ID is not a listed broker is not counted")
}

func TestListAndCountBrokerScopedActiveReservations_Agree(t *testing.T) {
	srv, s := testServer(t)
	f := seedBrokerScopedFixture(t, s)
	ctx := context.Background()

	rows, err := srv.listBrokerScopedActiveReservations(ctx, f.def.ID)
	require.NoError(t, err)
	n, err := srv.countBrokerScopedActiveReservations(ctx, f.def.ID)
	require.NoError(t, err)
	assert.Len(t, rows, len(f.wantOrder))
	assert.Equal(t, len(rows), n)
}

// TestBrokerQuota_ReconcileLeavesUnlistedScopeReservation pins that the
// reconcile only visits listed brokers: a reservation scoped to an ID that is
// not a runtime broker stays active even though its agent does not exist,
// while a listed broker's reservation for a missing agent is released.
func TestBrokerQuota_ReconcileLeavesUnlistedScopeReservation(t *testing.T) {
	srv, s := testServer(t)
	f := seedBrokerScopedFixture(t, s)
	ctx := context.Background()

	require.EqualValues(t, 1, brokerReservationCount(t, s, f.unlistedID))
	require.EqualValues(t, 2, brokerReservationCount(t, s, f.olderBroker.ID))

	srv.ReconcileStaleBrokerQuotaReservations(ctx)

	assert.EqualValues(t, 1, brokerReservationCount(t, s, f.unlistedID),
		"reservation scoped to an unlisted ID must not be touched")
	for _, id := range f.unlistedRows {
		has, err := s.HasActiveReservation(ctx, f.def.ID, id)
		require.NoError(t, err)
		assert.True(t, has)
	}
	assert.EqualValues(t, 0, brokerReservationCount(t, s, f.olderBroker.ID),
		"listed broker's reservations for missing agents are released")
	assert.EqualValues(t, 0, brokerReservationCount(t, s, f.newerBroker.ID))
}
