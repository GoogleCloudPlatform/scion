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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Profiles on the deprecated embedded-broker register path
// (POST /api/v1/projects/register with a "broker" object): a re-register
// that sends no profiles keeps the broker's stored profiles, a non-empty
// list replaces them, and a first register stores what it sends.

// registerProfilesTestBroker creates an existing broker owned by ownerID
// with two stored profiles.
func registerProfilesTestBroker(t *testing.T, s store.Store, name, ownerID string) *store.RuntimeBroker {
	t.Helper()
	broker := registerBrokerTestExistingBroker(t, s, name, ownerID)
	broker.Profiles = []store.BrokerProfile{
		{Name: "docker-default", Type: "docker", Available: true},
		{Name: "k8s-prod", Type: "kubernetes", Available: true, Namespace: "agents"},
	}
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), broker))
	return broker
}

func profileNames(profiles []store.BrokerProfile) []string {
	names := make([]string, 0, len(profiles))
	for _, p := range profiles {
		names = append(names, p.Name)
	}
	return names
}

func TestProjectRegisterEmbeddedBroker_ReregisterWithoutProfilesKeepsStored(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-profiles-owner-a")
	broker := registerProfilesTestBroker(t, s, "register-profiles-name-a", owner.ID)

	rec := doRequestAsUserLoopback(t, srv, owner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-profiles-project-a",
		Broker: &RegisterProjectBrokerInfo{
			ID:      broker.ID,
			Name:    broker.Name,
			Version: "2.0.0-reregister",
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "re-register should succeed; got: %s", rec.Body.String())

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0-reregister", updated.Version, "the re-register is applied")
	assert.Equal(t, []string{"docker-default", "k8s-prod"}, profileNames(updated.Profiles),
		"a re-register without profiles keeps the stored profiles")
}

func TestProjectRegisterEmbeddedBroker_ReregisterEmptyProfilesKeepsStored(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-profiles-owner-b")
	broker := registerProfilesTestBroker(t, s, "register-profiles-name-b", owner.ID)

	// An explicit empty list: RegisterProjectBrokerInfo omits an empty
	// slice when marshalled, so the body is written out.
	body, err := json.Marshal(map[string]interface{}{
		"name": "register-profiles-project-b",
		"broker": map[string]interface{}{
			"id":       broker.ID,
			"name":     broker.Name,
			"version":  "2.0.0-empty",
			"profiles": []interface{}{},
		},
	})
	require.NoError(t, err)
	require.Contains(t, string(body), `"profiles":[]`)

	rec := doRequestAsUserLoopback(t, srv, owner, http.MethodPost, "/api/v1/projects/register", json.RawMessage(body))
	require.Equal(t, http.StatusOK, rec.Code, "re-register should succeed; got: %s", rec.Body.String())

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0-empty", updated.Version, "the re-register is applied")
	assert.Equal(t, []string{"docker-default", "k8s-prod"}, profileNames(updated.Profiles),
		"an empty profiles list is treated as absent and keeps the stored profiles")
}

func TestProjectRegisterEmbeddedBroker_ReregisterWithProfilesReplaces(t *testing.T) {
	srv, s := testServer(t)
	owner := newHubMemberUser(t, s, "register-profiles-owner-c")
	broker := registerProfilesTestBroker(t, s, "register-profiles-name-c", owner.ID)

	rec := doRequestAsUserLoopback(t, srv, owner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-profiles-project-c",
		Broker: &RegisterProjectBrokerInfo{
			ID:       broker.ID,
			Name:     broker.Name,
			Version:  "2.0.0-replace",
			Profiles: []store.BrokerProfile{{Name: "podman-only", Type: "podman", Available: true}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "re-register should succeed; got: %s", rec.Body.String())

	updated, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"podman-only"}, profileNames(updated.Profiles),
		"a non-empty profiles list replaces the stored profiles")
}

func TestProjectRegisterEmbeddedBroker_FirstRegisterStoresProfiles(t *testing.T) {
	srv, s := testServer(t)
	requester := newHubMemberUser(t, s, "register-profiles-newuser-d")

	rec := doRequestAsUserLoopback(t, srv, requester, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "register-profiles-project-d",
		Broker: &RegisterProjectBrokerInfo{
			Name:     "register profiles first-time broker name",
			Version:  "1.0.0",
			Profiles: []store.BrokerProfile{{Name: "docker-default", Type: "docker", Available: true}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, "first register should succeed; got: %s", rec.Body.String())

	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Broker)

	created, err := s.GetRuntimeBroker(context.Background(), resp.Broker.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"docker-default"}, profileNames(created.Profiles),
		"a first register stores the profiles it sends")
}
