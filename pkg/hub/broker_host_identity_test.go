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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	hostIdentityX = "host-x@proj.iam.gserviceaccount.com"
	hostIdentityY = "host-y@proj.iam.gserviceaccount.com"
)

// getBrokerHostIdentity reads the broker detail as user and returns its
// hostIdentity object, or nil.
func getBrokerHostIdentity(t *testing.T, srv *Server, user *store.User, id string) *store.HostIdentityStatus {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/runtime-brokers/"+id, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		HostIdentity *store.HostIdentityStatus `json:"hostIdentity"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.HostIdentity
}

func recordObservation(t *testing.T, srv *Server, id, outcome, email string) {
	t.Helper()
	srv.recordHostObservation(context.Background(), id, store.HostIdentityObservation{Outcome: outcome, Email: email, ObservedAt: time.Now()})
}

func TestHostIdentityState_DetectionRecorded(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, false, "host-detection")
	f.update(t, gcpHostBody(hostIdentityX, "proj"))
	stale := f.row(t)

	recordObservation(t, f.srv, f.id, store.HostObservationPresent, hostIdentityY)

	f.requireGCPHost(t, hostIdentityX, "proj")
	hi := getBrokerHostIdentity(t, f.srv, f.owner, f.id)
	require.NotNil(t, hi)
	assert.Equal(t, HostIdentityMismatch, hi.State)
	assert.Equal(t, hostIdentityY, hi.ObservedEmail)
	assert.NotNil(t, hi.ObservedAt)

	// A heartbeat-style write of a copy read earlier leaves the observation.
	stale.Status = store.BrokerStatusOnline
	stale.LastHeartbeat = time.Now()
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), stale))
	hi = getBrokerHostIdentity(t, f.srv, f.owner, f.id)
	require.NotNil(t, hi)
	assert.Equal(t, HostIdentityMismatch, hi.State)
}

func TestHostIdentityState_EmbeddedFlatRecordsObservation(t *testing.T) {
	t.Parallel()
	f := newFlatRegFixture(t, true)
	ctx := context.Background()
	row := registerColocatedFlatRow(t, f, EmbeddedFlatRegistrationOptions{
		HostIdentity: &store.HostIdentityObservation{Outcome: store.HostObservationPresent, Email: hostIdentityX, ObservedAt: time.Now()},
	})

	b, err := f.s.GetRuntimeBroker(ctx, row.ID)
	require.NoError(t, err)
	require.NotNil(t, b.HostObservation)
	assert.Equal(t, hostIdentityX, b.HostObservation.Email)
	assert.Empty(t, b.GCPHostServiceAccountEmail, "the observation is never written to the GCP host fields")
	assert.Equal(t, HostIdentityUnsetDetected, hostIdentityState(b))
}

func TestHostIdentityState_PinnedAbsent(t *testing.T) {
	prev := healthSummaryBrokerLimit
	healthSummaryBrokerLimit = 1
	t.Cleanup(func() { healthSummaryBrokerLimit = prev })

	srv, s := testServer(t)
	ctx := context.Background()
	// Store order is newest first: the pinned broker is created first and
	// would fall past the cap without problem-first ordering.
	createSummaryBroker(t, s, &store.RuntimeBroker{ID: tid("hs-pinned-absent"), Name: "hs-pinned-absent", LastHeartbeat: time.Now()})
	_, err := s.SetRuntimeBrokerGCPHost(ctx, tid("hs-pinned-absent"), hostIdentityX, "proj")
	require.NoError(t, err)
	recordObservation(t, srv, tid("hs-pinned-absent"), store.HostObservationAbsent, "")
	time.Sleep(2 * time.Millisecond)
	createSummaryBroker(t, s, &store.RuntimeBroker{ID: tid("hs-pinned-ok"), Name: "hs-pinned-ok", LastHeartbeat: time.Now()})

	list, raw := getHealthSummaryBrokers(t, srv)
	require.Len(t, list.Items, 1)
	assert.Equal(t, tid("hs-pinned-absent"), list.Items[0].ID)
	require.NotNil(t, list.Items[0].HostIdentity)
	assert.Equal(t, HostIdentityPinnedAbsent, list.Items[0].HostIdentity.State, "distinct from mismatch")
	assert.JSONEq(t, `{"state":"pinned_absent"}`, string(raw[tid("hs-pinned-absent")]["host_identity"]), "no account names in the summary")

	rr := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rr.Code)
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &resp))
	found := false
	for _, it := range resp.Attention {
		if it.Kind == HealthAttentionBrokerHostIdentity && it.Subject.ID == tid("hs-pinned-absent") {
			found = true
			assert.NotContains(t, it.Message, hostIdentityX)
		}
	}
	assert.True(t, found, "an attention item names the broker")
}

func TestHostIdentityState_UpdateResolves(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, false, "host-update-resolves")
	f.update(t, gcpHostBody(hostIdentityX, "proj"))
	recordObservation(t, f.srv, f.id, store.HostObservationPresent, hostIdentityY)
	require.Equal(t, HostIdentityMismatch, getBrokerHostIdentity(t, f.srv, f.owner, f.id).State)

	f.update(t, gcpHostBody(hostIdentityY, "proj"))

	assert.Equal(t, HostIdentityMatch, getBrokerHostIdentity(t, f.srv, f.owner, f.id).State, "no new observation needed")
}

func TestHostIdentityState_Visibility(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, false, "host-visibility")
	f.update(t, gcpHostBody(hostIdentityX, "proj"))
	recordObservation(t, f.srv, f.id, store.HostObservationPresent, hostIdentityY)

	owner := getBrokerHostIdentity(t, f.srv, f.owner, f.id)
	require.NotNil(t, owner)
	assert.Equal(t, hostIdentityY, owner.ObservedEmail, "a caller allowed to update the broker sees the observed account")

	reader := newHubMemberUser(t, f.s, "host-visibility-reader")
	other := getBrokerHostIdentity(t, f.srv, reader, f.id)
	require.NotNil(t, other)
	assert.Equal(t, HostIdentityMismatch, other.State)
	assert.Empty(t, other.ObservedEmail, "other readers see only the state")
	assert.Nil(t, other.ObservedAt)

	// A remote broker that was never observed has no hostIdentity.
	remote := newOwnedBrokerFixture(t, false, "host-visibility-remote")
	assert.Nil(t, getBrokerHostIdentity(t, remote.srv, remote.owner, remote.id))
}

func TestHostIdentityState_Unknown(t *testing.T) {
	t.Parallel()
	f := newOwnedBrokerFixture(t, false, "host-unknown")
	f.update(t, gcpHostBody(hostIdentityX, "proj"))
	recordObservation(t, f.srv, f.id, store.HostObservationUnavailable, "")

	hi := getBrokerHostIdentity(t, f.srv, f.owner, f.id)
	require.NotNil(t, hi)
	assert.Equal(t, HostIdentityUnknown, hi.State)
	assert.False(t, hostIdentityIsProblem(hi.State))
}
