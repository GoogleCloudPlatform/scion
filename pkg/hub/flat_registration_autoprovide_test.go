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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Flat registration applies the Hub's auto-provide authorization exactly as
// the legacy registration does (ptone/scion#3274 amendment): turning
// auto-provide on for a new row, or for a row that has it off, needs
// broker.auto_provide; keeping it on or turning it off needs nothing extra;
// PreserveSettings leaves the row's settings (a new row off); and the
// decision is pinned at the write.

// targetFor is a runtime target of its own for the flat row id (target IDs
// are unique per row).
func (f *flatRegFixture) targetFor(id string) *api.RuntimeTargetDescriptor {
	return &api.RuntimeTargetDescriptor{ID: tid("target-" + id), Type: "docker", DisplayName: "Local Docker"}
}

func (f *flatRegFixture) flatRow(t *testing.T, id string) *store.RuntimeBroker {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err)
	return b
}

// TestFlatRegistration_AutoProvideNeedsTheHubPermission: a member without
// broker.auto_provide cannot create a flat row with auto-provide on, nor
// turn it on for a row that has it off; nothing is created or changed. A
// super-admin can.
func TestFlatRegistration_AutoProvideNeedsTheHubPermission(t *testing.T) {
	f := newFlatRegFixture(t, true)
	newID := tid("flat-ap-new")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: newID, Name: "flat-ap-new", RuntimeTarget: f.targetFor(newID), AutoProvide: true})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	_, err := f.s.GetRuntimeBroker(context.Background(), newID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no row is created for a refused auto-provide registration")

	id := tid("flat-ap-off")
	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-ap-off", RuntimeTarget: f.targetFor(id)})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-ap-off", RuntimeTarget: f.targetFor(id), AutoProvide: true})
	assert.Equal(t, http.StatusForbidden, rec.Code, "turning auto-provide on needs broker.auto_provide: %s", rec.Body.String())
	assert.False(t, f.flatRow(t, id).AutoProvide, "the refused request changes nothing")

	admin := newSuperAdminUser(t, f.s, "flat-ap-admin")
	adminID := tid("flat-ap-admin-row")
	rec = f.register(t, admin, CreateBrokerRegistrationRequest{BrokerID: adminID, Name: "flat-ap-admin", RuntimeTarget: f.targetFor(adminID), AutoProvide: true})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.True(t, f.flatRow(t, adminID).AutoProvide)
}

// TestFlatRegistration_KeepOrTurnOffAutoProvideNeedsNothingExtra: the row's
// creator keeps auto-provide on, or turns it off, with its normal
// re-registration authorization; PreserveSettings creates a new row with
// auto-provide off.
func TestFlatRegistration_KeepOrTurnOffAutoProvideNeedsNothingExtra(t *testing.T) {
	f := newFlatRegFixture(t, true)
	id := tid("flat-ap-keep")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-ap-keep", RuntimeTarget: f.targetFor(id)})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	row := f.flatRow(t, id)
	row.AutoProvide = true // enabled earlier by someone holding broker.auto_provide
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), row))

	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-ap-keep", RuntimeTarget: f.targetFor(id), AutoProvide: true})
	require.Equal(t, http.StatusCreated, rec.Code, "keeping auto-provide on: %s", rec.Body.String())
	assert.True(t, f.flatRow(t, id).AutoProvide)

	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-ap-keep", RuntimeTarget: f.targetFor(id)})
	require.Equal(t, http.StatusCreated, rec.Code, "turning auto-provide off: %s", rec.Body.String())
	assert.False(t, f.flatRow(t, id).AutoProvide)

	preserveID := tid("flat-ap-preserve-new")
	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: preserveID, Name: "flat-ap-preserve-new", RuntimeTarget: f.targetFor(preserveID),
		PreserveSettings: true, AutoProvide: true})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	assert.False(t, f.flatRow(t, preserveID).AutoProvide, "PreserveSettings creates a new row with auto-provide off")
}

// TestFlatRegistration_StaleAutoProvideDecisionRefused: a write whose
// decision kept auto-provide on without broker.auto_provide is refused when
// the row turned auto-provide off meanwhile, before anything is written.
func TestFlatRegistration_StaleAutoProvideDecisionRefused(t *testing.T) {
	f := newFlatRegFixture(t, true)
	id := tid("flat-ap-stale")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-ap-stale", RuntimeTarget: f.targetFor(id),
		Labels: map[string]string{"team": "a"}})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	authorizedAgainst := f.flatRow(t, id)
	authorizedAgainst.AutoProvide = true // what the authorization saw

	applied := false
	_, _, err := f.srv.registerFlatRuntimeBroker(context.Background(), flatRegistration{
		BrokerID: id, Name: "flat-ap-stale", Target: *f.targetFor(id), CreatedBy: f.operator.ID, Existing: authorizedAgainst,
		GateAutoProvide: true, RequestedAutoProvide: true, AutoProvideAuthorized: false,
		Apply: func(b *store.RuntimeBroker, _ bool) { applied = true; b.AutoProvide = true; b.Labels["team"] = "b" },
	})
	require.True(t, errors.Is(err, ErrBrokerRegistrationAuthorizationStale), "got %v", err)
	assert.False(t, applied, "nothing is applied")
	row := f.flatRow(t, id)
	assert.False(t, row.AutoProvide)
	assert.Equal(t, "a", row.Labels["team"])
}

// TestFlatRegistration_EmbeddedAutoProvideComesFromOperatorConfig: the
// co-located registration stores the in-process operator configuration's
// auto-provide without a caller permission check, and links nothing (no
// project gets the flat row as a provider).
func TestFlatRegistration_EmbeddedAutoProvideComesFromOperatorConfig(t *testing.T) {
	f := newFlatRegFixture(t, true)
	id := &brokeridentity.Identity{
		SchemaVersion:   brokeridentity.SchemaVersion,
		InstanceKey:     "local-docker",
		RuntimeBrokerID: tid("flat-ap-embedded"),
		RuntimeTarget:   api.RuntimeTargetDescriptor{ID: f.target.ID, Type: f.target.Type},
		ExecutionScope:  brokeridentity.ExecutionScope{Type: f.target.Type, Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"}},
		CreatedAt:       time.Now(),
	}
	inst := config.V1RuntimeBrokerInstanceConfig{Key: "local-docker", Name: "flat-ap-embedded",
		RuntimeTarget: &config.V1RuntimeTargetConfig{Type: f.target.Type, DisplayName: f.target.DisplayName}}
	row, err := f.srv.RegisterEmbeddedFlatRuntimeBroker(context.Background(), id, inst,
		EmbeddedFlatRegistrationOptions{Endpoint: "http://localhost:9800", AutoProvide: true})
	require.NoError(t, err)
	assert.True(t, row.AutoProvide)
	// It links nothing: no project has the flat row as a provider.
	projects, err := f.s.ListProjects(context.Background(), store.ProjectFilter{}, store.ListOptions{})
	require.NoError(t, err)
	for _, p := range projects.Items {
		providers, err := f.s.GetProjectProviders(context.Background(), p.ID)
		require.NoError(t, err)
		for _, pr := range providers {
			assert.NotEqual(t, row.ID, pr.BrokerID, "project %s got a provider link to the flat row", p.Slug)
		}
	}
}
