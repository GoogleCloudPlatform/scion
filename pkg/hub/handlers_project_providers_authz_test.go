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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providersAuthzFixture extends the shared bypassAgents fixture with a project
// that has no default broker, a second broker that is not yet linked to it,
// and a hub member who holds no binding on the project.
type providersAuthzFixture struct {
	*bypassAgentsFixture
	// target is owned by owner, has one provider (linked) and no default broker.
	target *store.Project
	// linked is already a provider of target.
	linked *store.RuntimeBroker
	// unlinked exists but is not a provider of target.
	unlinked *store.RuntimeBroker
	// member is a hub member with no binding on target.
	member *store.User
}

func providersAuthzSetup(t *testing.T) *providersAuthzFixture {
	t.Helper()
	f := &providersAuthzFixture{bypassAgentsFixture: bypassAgentsSetup(t)}
	ctx := context.Background()

	f.target = &store.Project{
		ID:      tid("providers-target"),
		Name:    "Providers Target",
		Slug:    "providers-target",
		OwnerID: f.owner.ID,
	}
	require.NoError(t, f.store.CreateProject(ctx, f.target))

	mkBroker := func(name string) *store.RuntimeBroker {
		b := &store.RuntimeBroker{
			ID:      uuid.New().String(),
			Name:    name,
			Slug:    name,
			Status:  store.BrokerStatusOnline,
			Created: time.Now(),
			Updated: time.Now(),
		}
		require.NoError(t, f.store.CreateRuntimeBroker(ctx, b))
		return b
	}
	f.linked = mkBroker("providers-linked")
	f.unlinked = mkBroker("providers-unlinked")
	require.NoError(t, f.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  f.target.ID,
		BrokerID:   f.linked.ID,
		BrokerName: f.linked.Name,
		Status:     store.BrokerStatusOnline,
	}))

	f.member = &store.User{
		ID:          tid("member-without-binding"),
		Email:       "member-without-binding@example.com",
		DisplayName: "Member Without Binding",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, f.store.CreateUser(ctx, f.member))
	ensureHubMembership(ctx, f.store, f.member.ID)

	return f
}

func (f *providersAuthzFixture) path() string {
	return "/api/v1/projects/" + f.target.ID + "/providers"
}

// providerIDs returns the broker IDs currently linked to the project.
func (f *providersAuthzFixture) providerIDs(t *testing.T, projectID string) []string {
	t.Helper()
	providers, err := f.store.GetProjectProviders(context.Background(), projectID)
	require.NoError(t, err)
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.BrokerID)
	}
	return ids
}

func (f *providersAuthzFixture) defaultBroker(t *testing.T, projectID string) string {
	t.Helper()
	p, err := f.store.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	return p.DefaultRuntimeBrokerID
}

// TestProjectProviders_MemberWithoutBindingDenied: a hub member with no binding
// on the project receives 403 on every providers endpoint, and the store is
// left unchanged.
func TestProjectProviders_MemberWithoutBindingDenied(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		f := providersAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, f.path(), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		assert.NotContains(t, rec.Body.String(), f.linked.ID)
	})

	t.Run("add", func(t *testing.T) {
		f := providersAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, f.path(),
			AddProviderRequest{BrokerID: f.unlinked.ID})
		assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		assert.ElementsMatch(t, []string{f.linked.ID}, f.providerIDs(t, f.target.ID),
			"provider set must be unchanged")
		assert.Empty(t, f.defaultBroker(t, f.target.ID),
			"default runtime broker must be unchanged")
	})

	t.Run("remove", func(t *testing.T) {
		f := providersAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodDelete, f.path()+"/"+f.linked.ID, nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		assert.ElementsMatch(t, []string{f.linked.ID}, f.providerIDs(t, f.target.ID),
			"provider set must be unchanged")
	})
}

// TestProjectProviders_OwnerAllowed: the project owner can list, add and
// remove providers; adding the first provider still sets the default broker.
func TestProjectProviders_OwnerAllowed(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		f := providersAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodGet, f.path(), nil)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		assert.Contains(t, rec.Body.String(), f.linked.ID)
	})

	t.Run("add", func(t *testing.T) {
		f := providersAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.path(),
			AddProviderRequest{BrokerID: f.unlinked.ID})
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		assert.ElementsMatch(t, []string{f.linked.ID, f.unlinked.ID}, f.providerIDs(t, f.target.ID))
		assert.Equal(t, f.unlinked.ID, f.defaultBroker(t, f.target.ID),
			"linking to a project with no default broker still sets it")
	})

	t.Run("remove", func(t *testing.T) {
		f := providersAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodDelete, f.path()+"/"+f.linked.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
		assert.Empty(t, f.providerIDs(t, f.target.ID))
	})
}

// TestProjectProviders_CrossProjectAgentNotFound: an agent from a different
// project receives 404, not 403, on every providers endpoint, and the store is
// left unchanged.
func TestProjectProviders_CrossProjectAgentNotFound(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		request func(f *providersAuthzFixture) (string, interface{})
	}{
		{"list", http.MethodGet, func(f *providersAuthzFixture) (string, interface{}) {
			return f.path(), nil
		}},
		{"add", http.MethodPost, func(f *providersAuthzFixture) (string, interface{}) {
			return f.path(), AddProviderRequest{BrokerID: f.unlinked.ID}
		}},
		{"remove", http.MethodDelete, func(f *providersAuthzFixture) (string, interface{}) {
			return f.path() + "/" + f.linked.ID, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := providersAuthzSetup(t)
			path, body := tc.request(f)
			// The calling agent belongs to f.proj, not f.target.
			rec := f.asAgent(t, tc.method, path, body, ScopeAgentCreate)
			assert.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
			assert.ElementsMatch(t, []string{f.linked.ID}, f.providerIDs(t, f.target.ID))
			assert.Empty(t, f.defaultBroker(t, f.target.ID))
		})
	}
}

// TestProjectProviders_InProjectAgent: an agent may list its own project's
// providers but may not change them.
func TestProjectProviders_InProjectAgent(t *testing.T) {
	f := providersAuthzSetup(t)
	base := "/api/v1/projects/" + f.proj.ID + "/providers"

	rec := f.asAgent(t, http.MethodGet, base, nil)
	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	before := f.providerIDs(t, f.proj.ID)
	rec = f.asAgent(t, http.MethodPost, base, AddProviderRequest{BrokerID: f.unlinked.ID}, ScopeAgentCreate)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	rec = f.asAgent(t, http.MethodDelete, base+"/"+f.broker.ID, nil, ScopeAgentCreate)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.ElementsMatch(t, before, f.providerIDs(t, f.proj.ID))
}

// TestProjectProviders_BrokerCallerDenied: a broker-authenticated caller is
// not a principal the providers endpoints accept.
func TestProjectProviders_BrokerCallerDenied(t *testing.T) {
	f := providersAuthzSetup(t)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"list", http.MethodGet, f.path(), nil},
		{"add", http.MethodPost, f.path(), AddProviderRequest{BrokerID: f.broker.ID}},
		{"remove", http.MethodDelete, f.path() + "/" + f.linked.ID, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.asBroker(t, tc.method, tc.path, tc.body)
			assert.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
				"body: %s", rec.Body.String())
		})
	}
	assert.ElementsMatch(t, []string{f.linked.ID}, f.providerIDs(t, f.target.ID))
	assert.Empty(t, f.defaultBroker(t, f.target.ID))
}
