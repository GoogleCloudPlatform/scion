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
	"strings"
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

// A case variant of an existing provider's slug or name resolves to that
// provider through the provider match, never through the auto-link path:
// the provider row (LocalPath, LinkedBy) is left untouched, and a non-admin
// caller (who may not update the project) is not refused with 403.
func TestResolveRuntimeBroker_ProviderCaseVariant_DoesNotRelink(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("nf-case-project"), Slug: "nf-case", Name: "NF Case"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("nf-case-broker"), Name: "Case Broker", Slug: "case-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		LocalPath: "/srv/linked/path", LinkedBy: "cli", Status: store.BrokerStatusOnline,
	}))

	callers := map[string]context.Context{
		"admin":     devUserContext(ctx),
		"non-admin": memberContext(ctx),
	}
	for caller, cctx := range callers {
		for _, ref := range []string{"CASE-BROKER", "CASE BROKER", "case broker"} {
			w := httptest.NewRecorder()
			brokerID, err := srv.resolveRuntimeBroker(cctx, w, ref, project)
			require.NoError(t, err, "caller=%s ref=%q body=%s", caller, ref, w.Body.String())
			assert.Equal(t, broker.ID, brokerID, "caller=%s ref=%q", caller, ref)

			provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
			require.NoError(t, err)
			assert.Equal(t, "/srv/linked/path", provider.LocalPath, "caller=%s ref=%q", caller, ref)
			assert.Equal(t, "cli", provider.LinkedBy, "caller=%s ref=%q", caller, ref)
		}
	}
}

// Error responses for an explicit broker list only the brokers the caller
// may use: an online provider the caller cannot dispatch to is left out of
// both the 503 (offline broker) and the 404 (unknown broker).
func TestResolveRuntimeBroker_ExplicitBrokerErrors_ListOnlyUsableBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("nf-usable-project"), Slug: "nf-usable", Name: "NF Usable"}
	require.NoError(t, s.CreateProject(ctx, project))
	// As project admin the member may update the project (and so auto-link
	// a broker) but still may not dispatch to a non-AutoProvide broker.
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: tid("nf-member"), Email: "member@example.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: "active",
	}))
	grantProjectRole(t, s, tid("nf-member"), project.ID, "project-admin")
	addProvider := func(b *store.RuntimeBroker) {
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID: project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status,
		}))
	}
	offline := &store.RuntimeBroker{ID: tid("nf-usable-off"), Name: "Usable Offline", Slug: "usable-offline", Status: store.BrokerStatusOffline}
	shared := &store.RuntimeBroker{ID: tid("nf-usable-shared"), Name: "Shared Broker", Slug: "shared-broker", Status: store.BrokerStatusOnline, AutoProvide: true}
	private := &store.RuntimeBroker{ID: tid("nf-usable-private"), Name: "Private Broker", Slug: "private-broker", Status: store.BrokerStatusOnline}
	addProvider(offline)
	addProvider(shared)
	addProvider(private)

	listed := func(t *testing.T, resp ErrorResponse) []string {
		t.Helper()
		raw, ok := resp.Error.Details["availableBrokers"].([]interface{})
		require.True(t, ok, "availableBrokers must be a list, got %T", resp.Error.Details["availableBrokers"])
		var ids []string
		for _, entry := range raw {
			m, ok := entry.(map[string]interface{})
			require.True(t, ok)
			ids = append(ids, m["id"].(string))
		}
		return ids
	}

	t.Run("offline provider 503", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, err := srv.resolveRuntimeBroker(memberContext(ctx), w, offline.Slug, project)
		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, []string{shared.ID}, listed(t, resp))
	})

	t.Run("auto-link offline broker 503", func(t *testing.T) {
		unlinked := &store.RuntimeBroker{ID: tid("nf-usable-unlinked"), Name: "Usable Unlinked", Slug: "usable-unlinked", Status: store.BrokerStatusOffline}
		require.NoError(t, s.CreateRuntimeBroker(ctx, unlinked))
		w := httptest.NewRecorder()
		_, err := srv.resolveRuntimeBroker(memberContext(ctx), w, unlinked.Slug, project)
		require.Error(t, err)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, []string{shared.ID}, listed(t, resp))
		_, err = s.GetProjectProvider(ctx, project.ID, unlinked.ID)
		assert.True(t, errors.Is(err, store.ErrNotFound), "offline broker must not be linked, got %v", err)
	})

	t.Run("unknown broker 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		_, err := srv.resolveRuntimeBroker(memberContext(ctx), w, "ghost", project)
		require.Error(t, err)
		require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, []string{shared.ID}, listed(t, resp))
		assert.Contains(t, resp.Error.Message, `"Shared Broker"`)
		assert.False(t, strings.Contains(resp.Error.Message, "Private Broker"), resp.Error.Message)
	})
}

// A provider requested by its broker's current name after a rename (provider
// rows keep the name from link time) resolves to the existing provider
// instead of re-linking it: the provider row is unchanged and a caller
// without project-update rights is not refused. Offline is still a 503.
func TestResolveRuntimeBroker_RenamedProvider_DoesNotRelink(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("nf-rename-project"), Slug: "nf-rename", Name: "NF Rename"}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("nf-rename-broker"), Name: "Old Name", Slug: "old-name",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		LocalPath: "/srv/linked", LinkedBy: "cli", Status: store.BrokerStatusOnline,
	}))
	broker.Name = "New Name"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	callers := map[string]context.Context{
		"admin":     devUserContext(ctx),
		"non-admin": memberContext(ctx),
	}
	for caller, cctx := range callers {
		for _, ref := range []string{"New Name", "new name"} {
			w := httptest.NewRecorder()
			brokerID, err := srv.resolveRuntimeBroker(cctx, w, ref, project)
			require.NoError(t, err, "caller=%s ref=%q body=%s", caller, ref, w.Body.String())
			assert.Equal(t, broker.ID, brokerID, "caller=%s ref=%q", caller, ref)

			provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
			require.NoError(t, err)
			assert.Equal(t, "/srv/linked", provider.LocalPath, "caller=%s ref=%q", caller, ref)
			assert.Equal(t, "cli", provider.LinkedBy, "caller=%s ref=%q", caller, ref)
		}
	}

	broker.Status = store.BrokerStatusOffline
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))
	w := httptest.NewRecorder()
	_, err := srv.resolveRuntimeBroker(memberContext(ctx), w, "New Name", project)
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	provider, err := s.GetProjectProvider(ctx, project.ID, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, "cli", provider.LinkedBy)
}

// Without an explicit broker, the 422 no_runtime_broker responses (default
// unusable, several to choose from, none usable) list only the brokers the
// caller may use, and degrade to an empty list with a permission message when
// every online provider is filtered out.
func TestResolveRuntimeBroker_NoRuntimeBroker_ListsOnlyUsableBrokers(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	newProject := func(name string, brokers ...*store.RuntimeBroker) *store.Project {
		t.Helper()
		project := &store.Project{ID: tid("nrb-" + name), Slug: "nrb-" + name, Name: "NRB " + name}
		require.NoError(t, s.CreateProject(ctx, project))
		for _, b := range brokers {
			require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
				ProjectID: project.ID, BrokerID: b.ID, BrokerName: b.Name, Status: b.Status,
			}))
		}
		return project
	}
	newBroker := func(name string, autoProvide bool) *store.RuntimeBroker {
		t.Helper()
		b := &store.RuntimeBroker{ID: tid("nrb-broker-" + name), Name: "NRB " + name, Slug: "nrb-" + name,
			Status: store.BrokerStatusOnline, AutoProvide: autoProvide}
		require.NoError(t, s.CreateRuntimeBroker(ctx, b))
		return b
	}
	sharedA, sharedB := newBroker("shared-a", true), newBroker("shared-b", true)
	privateA, privateB := newBroker("private-a", false), newBroker("private-b", false)

	resolve422 := func(t *testing.T, project *store.Project) ErrorResponse {
		t.Helper()
		w := httptest.NewRecorder()
		brokerID, err := srv.resolveRuntimeBroker(memberContext(ctx), w, "", project)
		require.Error(t, err)
		assert.Empty(t, brokerID)
		require.Equal(t, http.StatusUnprocessableEntity, w.Code, w.Body.String())
		var resp ErrorResponse
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Equal(t, ErrCodeNoRuntimeBroker, resp.Error.Code)
		return resp
	}
	listedIDs := func(t *testing.T, resp ErrorResponse) []string {
		t.Helper()
		raw, ok := resp.Error.Details["availableBrokers"].([]interface{})
		require.True(t, ok, "availableBrokers must be a JSON list (even when empty), got %T", resp.Error.Details["availableBrokers"])
		ids := []string{}
		for _, entry := range raw {
			ids = append(ids, entry.(map[string]interface{})["id"].(string))
		}
		return ids
	}

	t.Run("several to choose from", func(t *testing.T) {
		resp := resolve422(t, newProject("multi", sharedA, sharedB, privateA))
		assert.ElementsMatch(t, []string{sharedA.ID, sharedB.ID}, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "Multiple runtime brokers")
	})

	t.Run("default not usable", func(t *testing.T) {
		project := newProject("default", privateA, sharedA)
		project.DefaultRuntimeBrokerID = privateA.ID
		require.NoError(t, s.UpdateProject(ctx, project))
		resp := resolve422(t, project)
		assert.Equal(t, []string{sharedA.ID}, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "specify an alternative")
	})

	t.Run("all filtered out", func(t *testing.T) {
		resp := resolve422(t, newProject("none", privateA, privateB))
		assert.Empty(t, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "permission to use")
	})

	t.Run("single provider online, not dispatchable", func(t *testing.T) {
		resp := resolve422(t, newProject("single-private", privateA))
		assert.Empty(t, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "permission to use")
	})

	t.Run("single provider offline", func(t *testing.T) {
		offline := &store.RuntimeBroker{ID: tid("nrb-broker-offline-a"), Name: "NRB offline a", Slug: "nrb-offline-a",
			Status: store.BrokerStatusOffline, AutoProvide: true}
		require.NoError(t, s.CreateRuntimeBroker(ctx, offline))
		resp := resolve422(t, newProject("single-offline", offline))
		assert.Empty(t, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "only runtime broker is offline")
		assert.NotContains(t, resp.Error.Message, "permission")
	})

	t.Run("several providers all offline", func(t *testing.T) {
		offB := &store.RuntimeBroker{ID: tid("nrb-broker-offline-b"), Name: "NRB offline b", Slug: "nrb-offline-b", Status: store.BrokerStatusOffline}
		offC := &store.RuntimeBroker{ID: tid("nrb-broker-offline-c"), Name: "NRB offline c", Slug: "nrb-offline-c", Status: store.BrokerStatusOffline}
		require.NoError(t, s.CreateRuntimeBroker(ctx, offB))
		require.NoError(t, s.CreateRuntimeBroker(ctx, offC))
		resp := resolve422(t, newProject("all-offline", offB, offC))
		assert.Empty(t, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "None of this project's runtime brokers are online")
	})

	t.Run("no providers", func(t *testing.T) {
		resp := resolve422(t, newProject("empty"))
		assert.Empty(t, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "register a runtime broker first")
	})

	t.Run("default not usable and no alternatives", func(t *testing.T) {
		project := newProject("default-none", privateA, privateB)
		project.DefaultRuntimeBrokerID = privateA.ID
		require.NoError(t, s.UpdateProject(ctx, project))
		resp := resolve422(t, project)
		assert.Empty(t, listedIDs(t, resp))
		assert.Contains(t, resp.Error.Message, "no alternatives found")
	})
}

func memberContext(ctx context.Context) context.Context {
	return contextWithIdentity(ctx, NewAuthenticatedUser(tid("nf-member"), "member@example.com", "Member", "member", "cli"))
}

func devUserContext(ctx context.Context) context.Context {
	return contextWithIdentity(ctx, NewDevUser(DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}))
}
