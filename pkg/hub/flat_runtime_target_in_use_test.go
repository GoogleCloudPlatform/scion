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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestFlatRegistration_RuntimeTargetInUseRefused: a flat registration under
// a NEW Runtime Broker ID whose runtime target another live row still holds
// gets a clean 409 runtime_target_in_use (only the requested target ID in
// the details, never the other row's ID), and creates no row. Once the
// holder is deleted, the same registration succeeds.
func TestFlatRegistration_RuntimeTargetInUseRefused(t *testing.T) {
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	holderID := f.registerFlat(t, tid("flat-in-use-holder"), "flat-in-use-holder")
	newID := tid("flat-in-use-new")

	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: newID, Name: "flat-in-use-new", RuntimeTarget: f.target})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetInUse)
	assert.Equal(t, f.target.ID, d["runtimeTargetId"])
	assert.NotContains(t, rec.Body.String(), holderID, "the response must not include the other Runtime Broker's ID")
	assert.NotContains(t, rec.Body.String(), "failed to create flat Runtime Broker")
	_, err := f.s.GetRuntimeBroker(ctx, newID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no row is created")

	rec = doRequest(t, f.srv, http.MethodDelete, "/api/v1/runtime-brokers/"+holderID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: newID, Name: "flat-in-use-new", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	got, err := f.s.GetRuntimeBroker(ctx, newID)
	require.NoError(t, err)
	require.True(t, got.IsFlat())
	assert.Equal(t, f.target.ID, got.RuntimeTarget.ID)
}
