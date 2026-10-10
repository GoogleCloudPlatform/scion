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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Flat Runtime Brokers with the experiment OFF (ptone/scion#3268, contract
// rule R1), including the GoogleCloudPlatform/scion#2702 registration shapes
// (PreserveSettings, a join token TTL, a headless legacy re-issue): OFF gates
// only a NEW flat registration; an existing flat row keeps its identity,
// target and status and still joins.

// requireFlatReregisterOffKeepsIdentity re-registers an existing flat row
// with the experiment OFF (optionally with PreserveSettings and a token TTL)
// and asserts the same id and target, an unchanged status, and a successful
// join afterwards.
func requireFlatReregisterOffKeepsIdentity(t *testing.T, preserve bool) {
	t.Helper()
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("off-existing"), "off-existing")
	before, err := f.s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, before.RuntimeTarget)
	setFlatExperiment(t, f.srv, false)

	req := CreateBrokerRegistrationRequest{BrokerID: id, Name: "off-existing", RuntimeTarget: f.target}
	if preserve {
		req.PreserveSettings, req.JoinTokenTTLSeconds = true, 600
	}
	rec := f.register(t, f.operator, req)
	require.Equal(t, http.StatusCreated, rec.Code, "an existing flat row re-registers with the experiment off: %s", rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, id, resp.BrokerID, "same id")
	require.NotNil(t, resp.RuntimeTarget)
	assert.Equal(t, f.target.ID, resp.RuntimeTarget.ID, "same target in the response")

	mid, err := f.s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, mid.RuntimeTarget)
	assert.Equal(t, before.RuntimeTarget.ID, mid.RuntimeTarget.ID, "same stored target")
	assert.Equal(t, before.Status, mid.Status, "the re-registration leaves the status unchanged (never deactivated)")

	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: resp.JoinToken, Hostname: "off-existing", Version: "0.1.0", RuntimeTarget: f.target})
	require.Equal(t, http.StatusOK, rec.Code, "the join succeeds with the experiment off: %s", rec.Body.String())
	after, err := f.s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, after.RuntimeTarget)
	assert.Equal(t, before.RuntimeTarget.ID, after.RuntimeTarget.ID)
	assert.Equal(t, "online", after.Status, "online after the join")
	assert.Empty(t, after.Profiles)
}

func TestFlatRegistration_ExperimentOffExistingRowKeepsIdentity(t *testing.T) {
	requireFlatReregisterOffKeepsIdentity(t, false)
}

func TestFlatRegistration_ExperimentOffPreserveSettingsReissueKeepsIdentity(t *testing.T) {
	requireFlatReregisterOffKeepsIdentity(t, true)
}

// TestFlatRegistration_ExperimentOffNewRowWithPreserveSettingsRefused: a NEW
// flat registration in the #2702 shape (PreserveSettings and a token TTL) is
// refused with 412 experiment_disabled and creates no row.
func TestFlatRegistration_ExperimentOffNewRowWithPreserveSettingsRefused(t *testing.T) {
	f := newFlatRegFixture(t, false)
	id := tid("off-new-preserve")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "off-new-preserve", RuntimeTarget: f.target,
		PreserveSettings: true, JoinTokenTTLSeconds: 600})
	requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	_, err := f.s.GetRuntimeBroker(context.Background(), id)
	assert.ErrorIs(t, err, store.ErrNotFound, "no row created")
}

// TestFlatRegistration_ExperimentOffHeadlessLegacyReissueRefused: with the
// experiment off, a headless legacy token re-issue (no runtime target,
// PreserveSettings, a token TTL) for a flat row is still refused with 409
// runtime_target_changed, and the row keeps its target.
func TestFlatRegistration_ExperimentOffHeadlessLegacyReissueRefused(t *testing.T) {
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("off-headless"), "off-headless")
	setFlatExperiment(t, f.srv, false)
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "off-headless", PreserveSettings: true, JoinTokenTTLSeconds: 600})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	row, err := f.s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, row.RuntimeTarget)
	assert.Equal(t, f.target.ID, row.RuntimeTarget.ID, "the flat row keeps its target")
}
