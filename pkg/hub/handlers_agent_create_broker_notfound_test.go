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

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#2715: an explicitly named broker that does not exist at all is a
// 404 runtime_broker_not_found whose message names the broker and lists the
// brokers the caller can use — not a misleading 503.
func TestCreateAgent_UnknownBroker_Returns404(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:            "unknown-broker-agent",
		ProjectID:       project.ID,
		Task:            "do something",
		RuntimeBrokerID: "no-such-broker",
	})

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeRuntimeBrokerNotFound, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, `"no-such-broker"`)
	assert.Contains(t, resp.Error.Message, `"Create Test Broker"`,
		"message must list the brokers the caller can use (the CLI prints only the message)")
	assert.Equal(t, "no-such-broker", resp.Error.Details["requestedBrokerId"])
}

// Same as above, through the project-scoped create route the CLI uses.
func TestCreateProjectAgent_UnknownBroker_Returns404(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:            "unknown-broker-agent",
		Task:            "do something",
		RuntimeBrokerID: "no-such-broker",
	})

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeRuntimeBrokerNotFound, resp.Error.Code)
}

// With no usable brokers, the 404 message says so instead of listing nothing.
func TestResolveRuntimeBroker_UnknownBroker_NoUsableBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	project := &store.Project{ID: tid("nf-empty-project"), Slug: "nf-empty", Name: "NF Empty"}
	require.NoError(t, s.CreateProject(ctx, project))

	w := httptest.NewRecorder()
	brokerID, err := srv.resolveRuntimeBroker(devUserContext(ctx), w, "ghost", project)
	require.Error(t, err)
	assert.Empty(t, brokerID)
	require.Equal(t, http.StatusNotFound, w.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeRuntimeBrokerNotFound, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, `"ghost" not found`)
	assert.Contains(t, resp.Error.Message, "no runtime brokers are currently available")
}

// An explicitly named broker that exists but is offline gets 503
// runtime_broker_unavailable at resolution (not 404), whether it is named by
// ID, name or slug.
func TestResolveRuntimeBroker_ExistingOfflineProvider_Returns503(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("nf-off-project"), Slug: "nf-off", Name: "NF Offline"}
	require.NoError(t, s.CreateProject(ctx, project))
	offline := &store.RuntimeBroker{
		ID: tid("nf-off-broker"), Name: "Offline Broker", Slug: "offline-broker",
		Status: store.BrokerStatusOffline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, offline))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: offline.ID, BrokerName: offline.Name,
		Status: store.BrokerStatusOffline,
	}))

	for _, ref := range []string{offline.ID, offline.Name, offline.Slug} {
		w := httptest.NewRecorder()
		brokerID, err := srv.resolveRuntimeBroker(devUserContext(ctx), w, ref, project)
		require.Error(t, err, "ref=%q", ref)
		assert.Empty(t, brokerID, "ref=%q", ref)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, "ref=%q", ref)
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, ErrCodeRuntimeBrokerUnavail, resp.Error.Code, "ref=%q", ref)
	}
}

// End to end through the create route: an offline explicit broker is refused
// with 503 before any agent row is created, and nothing is dispatched.
func TestCreateAgent_ExplicitOfflineBroker_Returns503NoAgentRow(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	offline := &store.RuntimeBroker{
		ID: tid("create-off-broker"), Name: "Create Offline Broker", Slug: "create-offline-broker",
		Status: store.BrokerStatusOffline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, offline))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: offline.ID, BrokerName: offline.Name,
		Status: store.BrokerStatusOffline,
	}))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents", CreateAgentRequest{
		Name:            "offline-broker-agent",
		Task:            "do something",
		RuntimeBrokerID: offline.Slug,
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, ErrCodeRuntimeBrokerUnavail, resp.Error.Code)

	_, err := s.GetAgentBySlug(ctx, project.ID, "offline-broker-agent")
	assert.True(t, errors.Is(err, store.ErrNotFound), "no agent row may be created, got %v", err)
}

// An existing offline broker that is not yet a provider is neither linked
// nor reported as not found: 503, and no provider row is written.
func TestResolveRuntimeBroker_ExistingOfflineNonProvider_Returns503NoLink(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("nf-offlink-project"), Slug: "nf-offlink", Name: "NF Offline Link"}
	require.NoError(t, s.CreateProject(ctx, project))
	offline := &store.RuntimeBroker{
		ID: tid("nf-offlink-broker"), Name: "Offline Unlinked Broker", Slug: "offline-unlinked-broker",
		Status: store.BrokerStatusOffline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, offline))

	w := httptest.NewRecorder()
	brokerID, err := srv.resolveRuntimeBroker(devUserContext(ctx), w, offline.Slug, project)
	require.Error(t, err)
	assert.Empty(t, brokerID)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())

	_, err = s.GetProjectProvider(ctx, project.ID, offline.ID)
	assert.True(t, errors.Is(err, store.ErrNotFound), "offline broker must not be auto-linked, got %v", err)
	updated, err := s.GetProject(ctx, project.ID)
	require.NoError(t, err)
	assert.Empty(t, updated.DefaultRuntimeBrokerID, "offline broker must not become the project default")
}

// findBrokerByIDOrSlug really resolves by slug, so an explicit --broker <slug>
// for a broker that is not yet a provider is auto-linked rather than reported
// as not found.
func TestResolveRuntimeBroker_ExplicitSlugNotYetProvider_AutoLinks(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("nf-slug-project"), Slug: "nf-slug", Name: "NF Slug"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("nf-slug-broker"), Name: "Slug Only Broker", Slug: "slug-only-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	w := httptest.NewRecorder()
	brokerID, err := srv.resolveRuntimeBroker(devUserContext(ctx), w, "slug-only-broker", project)
	require.NoError(t, err, w.Body.String())
	assert.Equal(t, broker.ID, brokerID)

	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent-create", provider.LinkedBy)
}

func TestFindBrokerByIDOrSlug(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	broker := &store.RuntimeBroker{
		ID: tid("find-broker"), Name: "Find Me Broker", Slug: "find-me-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	for _, ref := range []string{broker.ID, "Find Me Broker", "find me broker", "find-me-broker", "FIND-ME-BROKER"} {
		got, err := srv.findBrokerByIDOrSlug(ctx, ref)
		require.NoError(t, err, "ref=%q", ref)
		assert.Equal(t, broker.ID, got.ID, "ref=%q", ref)
	}

	_, err := srv.findBrokerByIDOrSlug(ctx, "does-not-exist")
	assert.True(t, errors.Is(err, store.ErrNotFound), "unknown identifier must be ErrNotFound, got %v", err)
}

func devUserContext(ctx context.Context) context.Context {
	return contextWithIdentity(ctx, NewDevUser(DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}))
}
