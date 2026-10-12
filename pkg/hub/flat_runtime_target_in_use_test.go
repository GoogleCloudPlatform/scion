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
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

	// A request that collides on both the name and the target gets the name
	// conflict: the name check runs first.
	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: newID, Name: "flat-in-use-holder", RuntimeTarget: f.target})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNameConflict)
	_, err = f.s.GetRuntimeBroker(ctx, newID)
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

// targetHolderHidingStore hides, while armed, the Runtime Broker rows that
// hold one runtime target ID from ListRuntimeBrokers until the first
// CreateRuntimeBroker call: the holder looks absent to the registration's
// checks and appears only once the create has hit the unique index, as when
// another registration takes the target between the check and the create.
type targetHolderHidingStore struct {
	store.Store
	fault    *storeFaultSwitch
	targetID string
	created  atomic.Bool
	creates  atomic.Int32
}

func (w *targetHolderHidingStore) ListRuntimeBrokers(ctx context.Context, filter store.RuntimeBrokerFilter, opts store.ListOptions) (*store.ListResult[store.RuntimeBroker], error) {
	res, err := w.Store.ListRuntimeBrokers(ctx, filter, opts)
	if err != nil || !w.fault.Active() || w.created.Load() {
		return res, err
	}
	kept := res.Items[:0:0]
	for _, b := range res.Items {
		if b.RuntimeTarget == nil || b.RuntimeTarget.ID != w.targetID {
			kept = append(kept, b)
		}
	}
	out := *res
	out.Items = kept
	return &out, nil
}

func (w *targetHolderHidingStore) CreateRuntimeBroker(ctx context.Context, b *store.RuntimeBroker) error {
	if w.fault.Active() {
		w.created.Store(true)
		w.creates.Add(1)
	}
	return w.Store.CreateRuntimeBroker(ctx, b)
}

// TestFlatRegistration_RuntimeTargetTakenAfterCheckRefused: when the target
// is free at the registration's check but taken by another row before the
// create (the create then fails on the store's unique index with
// ErrAlreadyExists), the registration re-checks and refuses with 409
// runtime_target_in_use carrying only the requested target ID, not a
// wrapped store error.
func TestFlatRegistration_RuntimeTargetTakenAfterCheckRefused(t *testing.T) {
	ctx := context.Background()
	target := api.RuntimeTargetDescriptor{ID: tid("flat-race-target"), Type: "docker", DisplayName: "Local Docker"}
	srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *targetHolderHidingStore {
		return &targetHolderHidingStore{Store: inner, fault: f, targetID: target.ID}
	})
	setFlatExperiment(t, srv, true)
	holder := &store.RuntimeBroker{ID: tid("flat-race-holder"), Name: "flat-race-holder", Slug: "flat-race-holder", Status: store.BrokerStatusOffline, RuntimeTarget: &target}
	require.NoError(t, s.CreateRuntimeBroker(ctx, holder))
	fault.Arm()

	newID := tid("flat-race-new")
	_, created, err := srv.registerFlatRuntimeBroker(ctx, flatRegistration{BrokerID: newID, Name: "flat-race-new", Target: target, CreatedBy: "test"})
	require.Error(t, err)
	assert.False(t, created)
	assert.EqualValues(t, 1, wrapped.creates.Load(), "the check saw no holder, so the create was attempted")
	var refusal *RuntimeTargetRefusal
	require.True(t, errors.As(err, &refusal), "got %v, want a runtime target refusal", err)
	assert.Equal(t, ErrCodeRuntimeTargetInUse, refusal.Code)
	assert.Equal(t, http.StatusConflict, refusal.Status)
	assert.Equal(t, map[string]interface{}{"runtimeTargetId": target.ID}, refusal.Details)
	assert.NotContains(t, err.Error(), holder.ID)
	assert.NotContains(t, err.Error(), "failed to create flat Runtime Broker")
	_, err = s.GetRuntimeBroker(ctx, newID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no row is created")
}
