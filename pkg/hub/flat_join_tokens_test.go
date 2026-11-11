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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Flat Runtime Brokers and GoogleCloudPlatform/scion#2702 (headless join
// tokens, PreserveSettings re-issue, UpsertJoinToken): the contract's
// registration rules hold on the new paths (.design/flat-runtime-brokers-contract.md
// section 6).

// TestFlatRegistration_HeadlessLegacyReissueOfFlatRowRefused: a headless
// legacy token re-issue (no runtime target, PreserveSettings, a token TTL)
// for a flat row is refused with 409 runtime_target_changed before any
// write: the outstanding flat join token is not replaced (it still joins),
// the broker secret is unchanged and the row keeps its target and metadata.
func TestFlatRegistration_HeadlessLegacyReissueOfFlatRowRefused(t *testing.T) {
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-headless"), "flat-headless")
	secretBefore, err := f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
	rowBefore, err := f.s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)

	// An outstanding flat join token (a flat re-registration re-mints one).
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-headless", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var outstanding CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &outstanding))

	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{
		BrokerID: id, Name: "flat-headless", PreserveSettings: true, JoinTokenTTLSeconds: 600,
	})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	assert.Equal(t, id, d["runtimeBrokerId"])

	secretAfter, err := f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, secretBefore.SecretKey, secretAfter.SecretKey, "the refused re-issue leaves the broker secret unchanged")
	rowAfter, err := f.s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, rowAfter.RuntimeTarget)
	assert.Equal(t, f.target.ID, rowAfter.RuntimeTarget.ID)
	assert.Equal(t, rowBefore.AutoProvide, rowAfter.AutoProvide)

	// The outstanding flat token was not replaced or consumed: it still joins.
	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: outstanding.JoinToken, Hostname: "flat-headless", Version: "0.1.0", RuntimeTarget: f.target})
	require.Equal(t, http.StatusOK, rec.Code, "the outstanding flat token still joins: %s", rec.Body.String())
}

// TestFlatRegistration_PreserveSettingsReissueReplacesOutstandingToken: a
// flat re-registration with PreserveSettings leaves the row's metadata as it
// is and replaces the outstanding join token: the response reports
// Reissued, the earlier token no longer joins and the new one does.
func TestFlatRegistration_PreserveSettingsReissueReplacesOutstandingToken(t *testing.T) {
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	id := tid("flat-preserve")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-preserve", RuntimeTarget: f.target, AutoProvide: false,
		Labels: map[string]string{"team": "a"}})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var first CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &first))

	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-preserve", RuntimeTarget: f.target,
		PreserveSettings: true, AutoProvide: true, Labels: map[string]string{"team": "b"}})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var second CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &second))
	assert.True(t, second.Reregistered)
	assert.True(t, second.Reissued, "the outstanding join token was replaced")
	require.NotNil(t, second.RuntimeTarget)
	assert.Equal(t, f.target.ID, second.RuntimeTarget.ID)

	row, err := f.s.GetRuntimeBroker(ctx, id)
	require.NoError(t, err)
	assert.False(t, row.AutoProvide, "PreserveSettings leaves the row's metadata unchanged")
	assert.Equal(t, "a", row.Labels["team"])

	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: first.JoinToken, Hostname: "flat-preserve", Version: "0.1.0", RuntimeTarget: f.target})
	requireAPIError(t, rec, http.StatusUnauthorized, ErrCodeInvalidJoinToken)
	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: second.JoinToken, Hostname: "flat-preserve", Version: "0.1.0", RuntimeTarget: f.target})
	require.Equal(t, http.StatusOK, rec.Code, "the re-issued token joins: %s", rec.Body.String())
	_, err = f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
}

// TestFlatRegistration_RefusedJoinKeepsTokenUsable: a join refused by the
// descriptor check (contract section 6) is answered 409
// runtime_target_changed with the stored and reported targets, leaves the
// broker secret unchanged and does NOT consume the join token: the refusal
// rolls the join transaction back, so the same token then joins with the
// stored descriptor (a sibling of the frozen
// TestFlatRegistration_JoinDescriptorMismatchKeepsSecret).
func TestFlatRegistration_RefusedJoinKeepsTokenUsable(t *testing.T) {
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-refused-token"), "flat-refused-token")
	before, err := f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)

	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-refused-token", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	wrong := &api.RuntimeTargetDescriptor{ID: tid("refused-wrong-target"), Type: "docker"}
	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: resp.JoinToken, Hostname: "flat-refused-token", Version: "0.1.0", RuntimeTarget: wrong})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	assert.Equal(t, id, d["runtimeBrokerId"])
	assert.Equal(t, f.target.ID, d["storedRuntimeTargetId"])
	assert.Equal(t, wrong.ID, d["reportedRuntimeTargetId"])
	after, err := f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, before.SecretKey, after.SecretKey, "a refused join leaves the existing secret untouched")

	// The refused join did not consume the token: it now joins.
	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: resp.JoinToken, Hostname: "flat-refused-token", Version: "0.1.0", RuntimeTarget: f.target})
	require.Equal(t, http.StatusOK, rec.Code, "the refused token stays usable: %s", rec.Body.String())
	rotated, err := f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
	assert.NotEqual(t, before.SecretKey, rotated.SecretKey, "the successful join installs a new secret")
}
