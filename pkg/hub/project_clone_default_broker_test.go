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
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A project's default runtime broker must be one of its providers. A clone
// keeps the source's default only when the clone has that provider row
// after its auto-provide brokers are linked.

func newCloneDefaultBroker(t *testing.T, s store.Store, name string, autoProvide bool) *store.RuntimeBroker {
	t.Helper()
	b := &store.RuntimeBroker{
		ID:          tid(name),
		Name:        name,
		Slug:        name,
		Status:      store.BrokerStatusOnline,
		AutoProvide: autoProvide,
		CreatedBy:   DevUserID,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), b))
	return b
}

func cloneWithDefault(t *testing.T, srv *Server, s store.Store, defaultBrokerID, name string) *store.Project {
	t.Helper()
	ctx := context.Background()
	src := &store.Project{
		ID:                     api.NewUUID(),
		Name:                   name + " Source",
		Slug:                   name + "-source",
		GitRemote:              "https://github.com/test/" + name + ".git",
		DefaultRuntimeBrokerID: defaultBrokerID,
		OwnerID:                DevUserID,
		CreatedBy:              DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, src))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+src.ID+"/clone",
		map[string]string{"name": name + " Clone"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	stored, err := s.GetProject(ctx, resp.ID)
	require.NoError(t, err)
	assert.Equal(t, stored.DefaultRuntimeBrokerID, resp.DefaultRuntimeBrokerID,
		"the response and the stored clone agree on the default")
	return stored
}

func TestProjectClone_DefaultKeptWhenCloneHasProvider(t *testing.T) {
	srv, s := testServer(t)
	newCloneDefaultBroker(t, s, "clone-default-auto-a", true)
	auto := newCloneDefaultBroker(t, s, "clone-default-auto-b", true)

	clone := cloneWithDefault(t, srv, s, auto.ID, "clone-default-kept")

	assert.Equal(t, auto.ID, clone.DefaultRuntimeBrokerID)
	_, err := s.GetProjectProvider(context.Background(), clone.ID, auto.ID)
	require.NoError(t, err)
}

func TestProjectClone_DefaultReplacedWhenNotAProvider(t *testing.T) {
	srv, s := testServer(t)
	private := newCloneDefaultBroker(t, s, "clone-default-private", false)
	auto := newCloneDefaultBroker(t, s, "clone-default-auto", true)

	clone := cloneWithDefault(t, srv, s, private.ID, "clone-default-replaced")

	assert.Equal(t, auto.ID, clone.DefaultRuntimeBrokerID, "the auto-linked broker becomes the default")
	assertNoProvider(t, s, clone.ID, private.ID)
}

func TestProjectClone_DefaultClearedWhenNoProvider(t *testing.T) {
	srv, s := testServer(t)
	private := newCloneDefaultBroker(t, s, "clone-default-none", false)

	clone := cloneWithDefault(t, srv, s, private.ID, "clone-default-cleared")

	assert.Empty(t, clone.DefaultRuntimeBrokerID)
	assertNoProvider(t, s, clone.ID, private.ID)
}

func TestFindConnectedProvider_DefaultMustBeProvider(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	provider := newCloneDefaultBroker(t, s, "find-connected-provider", false)
	outsider := newCloneDefaultBroker(t, s, "find-connected-outsider", false)
	project := &store.Project{
		ID: api.NewUUID(), Name: "Find Connected", Slug: "find-connected",
		DefaultRuntimeBrokerID: outsider.ID, OwnerID: DevUserID, CreatedBy: DevUserID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: provider.ID, BrokerName: provider.Name, Status: store.BrokerStatusOnline,
	}))

	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	mgr.mu.Lock()
	mgr.connections[provider.ID] = &BrokerConnection{brokerID: provider.ID, sessionID: "s1"}
	mgr.connections[outsider.ID] = &BrokerConnection{brokerID: outsider.ID, sessionID: "s2"}
	mgr.mu.Unlock()
	srv.mu.Lock()
	srv.controlChannel = mgr
	srv.mu.Unlock()

	got, err := srv.findConnectedProvider(ctx, project)
	require.NoError(t, err)
	assert.Equal(t, provider.ID, got, "a connected default that is not a provider is not chosen")

	project.DefaultRuntimeBrokerID = provider.ID
	got, err = srv.findConnectedProvider(ctx, project)
	require.NoError(t, err)
	assert.Equal(t, provider.ID, got, "a connected default that is a provider is chosen")
}
