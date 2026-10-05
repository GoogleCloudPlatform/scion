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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Associating a broker with a project needs project.update on the project
// and broker.update on the broker (its owner or a super-admin). A default
// runtime broker must already be a provider of the project. Unlinking one
// broker accepts project.update or broker.update on that broker.

// brokerAssocFixture is a project owned by projectOwner with no providers
// and no default broker, a broker owned by brokerOwner (a hub member with
// no binding on the project), and a second broker owned by projectOwner.
type brokerAssocFixture struct {
	srv          *Server
	store        store.Store
	project      *store.Project
	projectOwner *store.User
	brokerOwner  *store.User
	// otherBroker is owned by brokerOwner.
	otherBroker *store.RuntimeBroker
	// ownBroker is owned by projectOwner.
	ownBroker *store.RuntimeBroker
}

func brokerAssocSetup(t *testing.T, name string) *brokerAssocFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &brokerAssocFixture{srv: srv, store: s}

	projectID := tid(name + "-project")
	ownerID := tid(name + "-project-owner")
	createRS1Project(t, s, projectID, ownerID)
	var err error
	f.project, err = s.GetProject(ctx, projectID)
	require.NoError(t, err)
	f.projectOwner, err = s.GetUser(ctx, ownerID)
	require.NoError(t, err)

	f.brokerOwner = newHubMemberUser(t, s, name+"-broker-owner")
	f.otherBroker = createReregistrationTestBroker(t, s, name+"-other-broker", f.brokerOwner.ID)
	f.ownBroker = createReregistrationTestBroker(t, s, name+"-own-broker", f.projectOwner.ID)
	return f
}

func (f *brokerAssocFixture) providersPath() string {
	return "/api/v1/projects/" + f.project.ID + "/providers"
}

func (f *brokerAssocFixture) link(t *testing.T, broker *store.RuntimeBroker) {
	t.Helper()
	require.NoError(t, f.store.AddProjectProvider(context.Background(), &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
}

func assertNoProvider(t *testing.T, s store.Store, projectID, brokerID string) {
	t.Helper()
	_, err := s.GetProjectProvider(context.Background(), projectID, brokerID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "no provider row expected, got %v", err)
}

func assertDefaultBroker(t *testing.T, s store.Store, projectID, want string) {
	t.Helper()
	p, err := s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	assert.Equal(t, want, p.DefaultRuntimeBrokerID)
}

func TestBrokerAssociation_ProjectOwnerCannotLinkOtherUsersBroker(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-other")

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.otherBroker.ID})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), brokerProvideDeniedMessage)
	assertNoProvider(t, f.store, f.project.ID, f.otherBroker.ID)
	assertDefaultBroker(t, f.store, f.project.ID, "")
}

func TestBrokerAssociation_OwnerOfBothLinks(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-both")

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	provider, err := f.store.GetProjectProvider(context.Background(), f.project.ID, f.ownBroker.ID)
	require.NoError(t, err)
	assert.Equal(t, f.projectOwner.ID, provider.LinkedBy)
	assertDefaultBroker(t, f.store, f.project.ID, f.ownBroker.ID)
}

func TestBrokerAssociation_SuperAdminLinksUsersBroker(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-admin")
	admin := newSuperAdminUser(t, f.store, "assoc-admin-super")

	rec := doRequestAsUser(t, f.srv, admin, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.otherBroker.ID})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	provider, err := f.store.GetProjectProvider(context.Background(), f.project.ID, f.otherBroker.ID)
	require.NoError(t, err)
	assert.Equal(t, admin.ID, provider.LinkedBy)
}

func TestBrokerAssociation_BrokerOwnerWithoutProjectUpdateDenied(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-noproj")

	rec := doRequestAsUser(t, f.srv, f.brokerOwner, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.otherBroker.ID})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.project.ID, f.otherBroker.ID)
	assertDefaultBroker(t, f.store, f.project.ID, "")
}

func TestBrokerAssociation_ProjectTokenCannotLink(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-ptoken")
	key, _, err := f.srv.uatService.CreateTokenWithParams(rs4MintContext(f.projectOwner.ID), CreateTokenParams{
		UserID: f.projectOwner.ID, Name: "assoc-ptoken", ProjectID: f.project.ID, Scopes: []string{"project:update"},
	})
	require.NoError(t, err)

	rec := doRequestWithToken(t, f.srv, key, http.MethodPost, f.providersPath(),
		AddProviderRequest{BrokerID: f.ownBroker.ID})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.project.ID, f.ownBroker.ID)
}

func TestBrokerAssociation_RegisterExistingProjectWithOtherUsersBrokerDenied(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-reg-existing")

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		ID:       f.project.ID,
		Name:     f.project.Name,
		BrokerID: f.otherBroker.ID,
	})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.project.ID, f.otherBroker.ID)
	assertDefaultBroker(t, f.store, f.project.ID, "")
}

func TestBrokerAssociation_RegisterNewProjectWithOtherUsersBrokerCreatesNothing(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-reg-new")
	ctx := context.Background()
	const name = "Assoc Register New Project"
	before, err := f.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:     name,
		BrokerID: f.otherBroker.ID,
	})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, err = f.store.GetProjectBySlugCaseInsensitive(ctx, api.Slugify(name))
	assert.True(t, errors.Is(err, store.ErrNotFound), "no project may be created, got %v", err)
	after, err := f.store.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	assert.Equal(t, before.TotalCount, after.TotalCount, "no project may be created")
	links, err := f.store.GetBrokerProjects(ctx, f.otherBroker.ID)
	require.NoError(t, err)
	assert.Empty(t, links)
}

func TestBrokerAssociation_RegisterNewProjectWithOwnBrokerLinks(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-reg-own")
	ctx := context.Background()

	rec := doRequestAsUser(t, f.srv, f.brokerOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:     "Assoc Register Own Broker",
		BrokerID: f.otherBroker.ID,
	})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	links, err := f.store.GetBrokerProjects(ctx, f.otherBroker.ID)
	require.NoError(t, err)
	require.Len(t, links, 1)
	assert.Equal(t, f.brokerOwner.ID, links[0].LinkedBy)
}

func TestBrokerAssociation_RegisterUnknownBrokerCreatesNoProject(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-reg-unknown")
	const name = "Assoc Register Unknown Broker"

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name:     name,
		BrokerID: tid("assoc-reg-unknown-missing"),
	})

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	_, err := f.store.GetProjectBySlugCaseInsensitive(context.Background(), api.Slugify(name))
	assert.True(t, errors.Is(err, store.ErrNotFound), "no project may be created, got %v", err)
}

func TestBrokerAssociation_AgentCreateCannotLinkOtherUsersBroker(t *testing.T) {
	f := brokerLinkAuthzSetup(t)
	ctx := context.Background()
	other := newHubMemberUser(t, f.store, "assoc-agent-broker-owner")
	otherBroker := createReregistrationTestBroker(t, f.store, "assoc-agent-other-broker", other.ID)
	otherBroker.Status = store.BrokerStatusOnline
	require.NoError(t, f.store.UpdateRuntimeBroker(ctx, otherBroker))

	rec := createAgentAsOwner(t, f.bypassAgentsFixture, CreateAgentRequest{
		Name:            "assoc-agent-other-broker",
		RuntimeBrokerID: otherBroker.ID,
	})

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.proj.ID, otherBroker.ID)
	assertDefaultBroker(t, f.store, f.proj.ID, "")
	_, err := f.store.GetAgentBySlug(ctx, f.proj.ID, "assoc-agent-other-broker")
	assert.True(t, errors.Is(err, store.ErrNotFound), "no agent may be created, got %v", err)
}

func TestBrokerAssociation_AgentCreateLinkRecordsLinkingUser(t *testing.T) {
	f := brokerLinkAuthzSetup(t)

	rec := createAgentAsOwner(t, f.bypassAgentsFixture, CreateAgentRequest{
		Name:            "assoc-agent-own-broker",
		RuntimeBrokerID: f.unlinked.ID,
	})

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	provider, err := f.store.GetProjectProvider(context.Background(), f.proj.ID, f.unlinked.ID)
	require.NoError(t, err)
	assert.Equal(t, f.owner.ID, provider.LinkedBy)
}

func TestBrokerAssociation_DefaultMustBeProvider(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-default-np")

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPatch, "/api/v1/projects/"+f.project.ID,
		map[string]string{"defaultRuntimeBrokerId": f.ownBroker.ID})

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assertDefaultBroker(t, f.store, f.project.ID, "")
}

func TestBrokerAssociation_DefaultProviderAccepted(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-default-p")
	f.link(t, f.otherBroker)

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodPatch, "/api/v1/projects/"+f.project.ID,
		map[string]string{"defaultRuntimeBrokerId": f.otherBroker.ID})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assertDefaultBroker(t, f.store, f.project.ID, f.otherBroker.ID)
}

func TestBrokerAssociation_RegistrationCreatesNoAssociation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	member := newHubMemberUser(t, s, "assoc-reg-none-member")

	rec := doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/brokers", CreateBrokerRegistrationRequest{
		Name: "assoc-reg-none-broker",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	brokerID := decodeBrokerRegistration(t, rec.Body).BrokerID

	// A project created afterwards by the same user gets no link and no
	// default from the registration.
	rec = doRequestAsUser(t, srv, member, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "Assoc Reg None Project",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp RegisterProjectResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	links, err := s.GetBrokerProjects(ctx, brokerID)
	require.NoError(t, err)
	assert.Empty(t, links)
	assertDefaultBroker(t, s, resp.Project.ID, "")
}

func TestBrokerAssociation_BrokerOwnerUnlinksOwnBroker(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-unlink-own")
	f.link(t, f.otherBroker)

	rec := doRequestAsUser(t, f.srv, f.brokerOwner, http.MethodDelete, f.providersPath()+"/"+f.otherBroker.ID, nil)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.project.ID, f.otherBroker.ID)
}

func TestBrokerAssociation_BrokerOwnerCannotUnlinkAnotherBroker(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-unlink-another")
	f.link(t, f.ownBroker)

	rec := doRequestAsUser(t, f.srv, f.brokerOwner, http.MethodDelete, f.providersPath()+"/"+f.ownBroker.ID, nil)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, err := f.store.GetProjectProvider(context.Background(), f.project.ID, f.ownBroker.ID)
	assert.NoError(t, err, "the provider row stays")
}

func TestBrokerAssociation_UnrelatedMemberCannotUnlink(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-unlink-member")
	f.link(t, f.otherBroker)
	member := newHubMemberUser(t, f.store, "assoc-unlink-member-user")

	rec := doRequestAsUser(t, f.srv, member, http.MethodDelete, f.providersPath()+"/"+f.otherBroker.ID, nil)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, err := f.store.GetProjectProvider(context.Background(), f.project.ID, f.otherBroker.ID)
	assert.NoError(t, err, "the provider row stays")
}

func TestBrokerAssociation_BrokerOwnerHubTokenCannotUnlink(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-unlink-token")
	f.link(t, f.otherBroker)
	key := mintHubBrokerUAT(t, f.srv, f.brokerOwner.ID, "broker:create")

	rec := doRequestWithToken(t, f.srv, key, http.MethodDelete, f.providersPath()+"/"+f.otherBroker.ID, nil)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, err := f.store.GetProjectProvider(context.Background(), f.project.ID, f.otherBroker.ID)
	assert.NoError(t, err, "the provider row stays")
}

func TestBrokerAssociation_ProjectOwnerUnlinksOtherUsersBroker(t *testing.T) {
	f := brokerAssocSetup(t, "assoc-unlink-proj")
	f.link(t, f.otherBroker)

	rec := doRequestAsUser(t, f.srv, f.projectOwner, http.MethodDelete, f.providersPath()+"/"+f.otherBroker.ID, nil)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assertNoProvider(t, f.store, f.project.ID, f.otherBroker.ID)
}
