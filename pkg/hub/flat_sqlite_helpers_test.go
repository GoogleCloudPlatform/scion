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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restartFlatHub simulates a Hub service restart: the running server is shut
// down and a new one is built over the same store, with a fresh dispatcher
// client. The experiment is set as given.
func restartFlatHub(t *testing.T, f *flatHubFixture, experimentOn bool) {
	t.Helper()
	require.NoError(t, f.srv.Shutdown(context.Background()))
	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.DevUserConfig = DevUserConfig{Username: "dev", DisplayName: "Development User", Email: "dev@localhost"}
	srv, err := New(cfg, f.s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	f.srv = srv
	f.client = &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.s, f.client, false, slog.Default()))
	setFlatExperiment(t, srv, experimentOn)
}

type flatHubOpts struct {
	experimentOn bool
	linkFlat     bool // flat row is a provider of the project
	// storeWrap, when set, is installed as srv.store right after the server
	// is built (installStoreFault), before any setup; the wrapper must
	// delegate until the test arms the fixture's storeFault switch.
	storeWrap func(inner store.Store, fault *storeFaultSwitch) store.Store
}

type flatHubFixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	flat    *store.RuntimeBroker // flat Runtime Broker (docker target)
	legacy  *store.RuntimeBroker // legacy, profile-based Runtime Broker
	client  *mockRuntimeBrokerClient
	// storeWrapper and storeFault are set when flatHubOpts.storeWrap is.
	storeWrapper store.Store
	storeFault   *storeFaultSwitch
}

func setFlatExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, experiments.FlatRuntimeBrokers, enabled)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

func newFlatHubFixture(t *testing.T, opts flatHubOpts) *flatHubFixture {
	t.Helper()
	srv, s := testServer(t)
	var storeWrapper store.Store
	var storeFault *storeFaultSwitch
	if opts.storeWrap != nil {
		storeWrapper, storeFault = installStoreFault(t, srv, opts.storeWrap)
	}
	ctx := context.Background()

	project := &store.Project{ID: tid("flat-project-" + t.Name()), Name: "Flat Project", Slug: "flat-project-" + tidSlugSafe(t.Name())}
	require.NoError(t, s.CreateProject(ctx, project))

	legacy := &store.RuntimeBroker{
		ID:             tid("flat-legacy-broker-" + t.Name()),
		Name:           "legacy-broker",
		Slug:           "legacy-broker",
		Status:         store.BrokerStatusOnline,
		Endpoint:       "http://legacy.invalid",
		Profiles:       []store.BrokerProfile{{Name: "local", Type: "docker", Available: true}},
		DefaultProfile: "local",
		Capabilities:   &store.BrokerCapabilities{Reprovision: true, Sync: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, legacy))

	flat := &store.RuntimeBroker{
		ID:            tid("flat-broker-" + t.Name()),
		Name:          "flat-docker",
		Slug:          "flat-docker",
		Status:        store.BrokerStatusOnline,
		Endpoint:      "http://flat.invalid",
		Capabilities:  &store.BrokerCapabilities{Reprovision: true, Sync: true},
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("flat-target-" + t.Name()), Type: "docker", DisplayName: "Local Docker"},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, flat))

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project.ID, BrokerID: legacy.ID, BrokerName: legacy.Name, Status: store.BrokerStatusOnline}))
	project.DefaultRuntimeBrokerID = legacy.ID
	if opts.linkFlat {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project.ID, BrokerID: flat.ID, BrokerName: flat.Name, Status: store.BrokerStatusOnline}))
	}
	require.NoError(t, s.UpdateProject(ctx, project))

	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	setFlatExperiment(t, srv, opts.experimentOn)

	flat, err := s.GetRuntimeBroker(ctx, flat.ID)
	require.NoError(t, err)
	legacy, err = s.GetRuntimeBroker(ctx, legacy.ID)
	require.NoError(t, err)
	return &flatHubFixture{srv: srv, s: s, project: project, flat: flat, legacy: legacy, client: client,
		storeWrapper: storeWrapper, storeFault: storeFault}
}

// reincarnationEligible gives an agent a clone-per-agent workspace (a git
// clone), which handleReincarnateAgent requires before its in-place
// placement check.
func reincarnationEligible(a *store.Agent) {
	if a.AppliedConfig == nil {
		a.AppliedConfig = &store.AgentAppliedConfig{}
	}
	a.AppliedConfig.Workspace = "/tmp/flat-reincarnate-workspace"
	a.AppliedConfig.GitClone = &api.GitCloneConfig{URL: "https://example.com/flat-reincarnate.git"}
	if a.AppliedConfig.CreateInputs == nil {
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	}
	a.AppliedConfig.CreateInputs.Workspace = "/tmp/flat-reincarnate-workspace"
}

// requireAPIError asserts status and code and returns the details.
func requireAPIError(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) map[string]interface{} {
	t.Helper()
	require.Equal(t, status, rec.Code, "body: %s", rec.Body.String())
	e := decodeFlatAPIError(t, rec)
	require.Equal(t, code, e.Code, "body: %s", rec.Body.String())
	return e.Details
}

// requireNoStartMarkers asserts that a Hub public envelope carries no start
// markers (section 9).
func requireNoStartMarkers(t *testing.T, details map[string]interface{}) {
	t.Helper()
	for _, k := range []string{"startAttempted", "runId", "currentRunId"} {
		_, ok := details[k]
		assert.False(t, ok, "Hub public envelope must not carry start marker %q", k)
	}
}

// startExtrasWireKey returns the value applyStartExtras writes under the
// frozen wire key for extras ("" if absent). Both transports build start and
// restart payloads with applyStartExtras, so this is the key the Runtime
// Broker receives.
func startExtrasWireKey(extras StartExtras, key string) string {
	payload := map[string]interface{}{}
	applyStartExtras(payload, extras)
	v, _ := payload[key].(string)
	return v
}

func flatMemberUser(t *testing.T, s store.Store, projectID, name string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(name), Email: name + "@example.com", DisplayName: name, Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	createTestUserWithProjectRole(t, s, u.ID, u.Email, projectID, store.ProjectRoleMember)
	return u
}

func newFlatRegFixture(t *testing.T, experimentOn bool) *flatRegFixture {
	t.Helper()
	srv, s := testServer(t)
	setFlatExperiment(t, srv, experimentOn)
	return &flatRegFixture{
		srv: srv, s: s,
		operator: newHubMemberUser(t, s, "flat-reg-operator"),
		other:    newHubMemberUser(t, s, "flat-reg-other"),
		target:   &api.RuntimeTargetDescriptor{ID: tid("flat-reg-target-" + t.Name()), Type: "docker", DisplayName: "Local Docker"},
	}
}

// create POSTs a raw-JSON create body to the project's agents endpoint as
// the dev super-admin.
func (f *flatHubFixture) create(t *testing.T, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents", body)
}

func (f *flatHubFixture) agentBySlug(t *testing.T, slug string) *store.Agent {
	t.Helper()
	a, err := f.s.GetAgentBySlug(context.Background(), f.project.ID, slug)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	require.NoError(t, err)
	return a
}

func (f *flatHubFixture) providerIDs(t *testing.T, projectID string) []string {
	t.Helper()
	providers, err := f.s.GetProjectProviders(context.Background(), projectID)
	require.NoError(t, err)
	ids := make([]string, 0, len(providers))
	for _, p := range providers {
		ids = append(ids, p.BrokerID)
	}
	return ids
}

func (f *flatHubFixture) projectDefault(t *testing.T, projectID string) string {
	t.Helper()
	p, err := f.s.GetProject(context.Background(), projectID)
	require.NoError(t, err)
	return p.DefaultRuntimeBrokerID
}

// pinnedAgent stores an agent pinned to the flat Runtime Broker.
func (f *flatHubFixture) pinnedAgent(t *testing.T, slug, phase string) *store.Agent {
	t.Helper()
	return f.pinnedAgentWith(t, slug, phase, nil)
}

// pinnedAgentWith is pinnedAgent with a mutation applied before the store
// write.
func (f *flatHubFixture) pinnedAgentWith(t *testing.T, slug, phase string, mutate func(*store.Agent)) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:                      tid("flat-agent-" + slug + "-" + t.Name()),
		Slug:                    slug,
		Name:                    slug,
		ProjectID:               f.project.ID,
		RuntimeBrokerID:         f.flat.ID,
		Phase:                   phase,
		PinnedRuntimeBrokerID:   f.flat.ID,
		PinnedRuntimeTargetID:   f.flat.RuntimeTarget.ID,
		PinnedRuntimeTargetType: f.flat.RuntimeTarget.Type,
		AppliedConfig:           &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

// stalePinnedAgent stores an agent whose pin names the flat Runtime Broker
// while runtime_broker_id was moved to the legacy one (as an older binary
// would do).
func (f *flatHubFixture) stalePinnedAgent(t *testing.T, slug, phase string) *store.Agent {
	t.Helper()
	return f.stalePinnedAgentWith(t, slug, phase, nil)
}

// stalePinnedAgentWith is stalePinnedAgent with a mutation applied before
// the first store write.
func (f *flatHubFixture) stalePinnedAgentWith(t *testing.T, slug, phase string, mutate func(*store.Agent)) *store.Agent {
	t.Helper()
	a := f.pinnedAgentWith(t, slug, phase, mutate)
	a.RuntimeBrokerID = f.legacy.ID
	require.NoError(t, f.s.UpdateAgent(context.Background(), a))
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	require.True(t, got.IsPinned())
	require.False(t, got.PinValid())
	return got
}

// unpinnedAgentOn stores an agent on brokerID with no pin.
func (f *flatHubFixture) unpinnedAgentOn(t *testing.T, slug, brokerID, phase string) *store.Agent {
	t.Helper()
	return f.unpinnedAgentOnWith(t, slug, brokerID, phase, nil)
}

func (f *flatHubFixture) unpinnedAgentOnWith(t *testing.T, slug, brokerID, phase string, mutate func(*store.Agent)) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:              tid("flat-agent-" + slug + "-" + t.Name()),
		Slug:            slug,
		Name:            slug,
		ProjectID:       f.project.ID,
		RuntimeBrokerID: brokerID,
		Phase:           phase,
		AppliedConfig:   &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
	}
	if mutate != nil {
		mutate(a)
	}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

// noAgentWritten asserts the create left no trace: no agent row, no run
// intent and no dispatch.
func (f *flatHubFixture) noAgentWritten(t *testing.T, slug string) {
	t.Helper()
	assert.Nil(t, f.agentBySlug(t, slug), "no agent row may be written")
	assert.False(t, f.client.createCalled, "nothing may be dispatched")
	assert.False(t, f.client.startCalled, "nothing may be dispatched")
}

// flatOnlyProject creates a project whose only provider is the flat row, so
// the scheduler's providers[0] is flat.
func (f *flatHubFixture) flatOnlyProject(t *testing.T) *store.Project {
	t.Helper()
	ctx := context.Background()
	p := &store.Project{ID: tid("flat-only-" + t.Name()), Name: "Flat Only", Slug: "flat-only-" + tidSlugSafe(t.Name())}
	require.NoError(t, f.s.CreateProject(ctx, p))
	require.NoError(t, f.s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: p.ID, BrokerID: f.flat.ID, BrokerName: f.flat.Name, Status: store.BrokerStatusOnline}))
	setProjectAnnotations(t, f.s, p, map[string]string{projectSettingActiveProfile: "local"})
	return p
}

func decodeFlatAPIError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp.Error
}

type flatRegFixture struct {
	srv      *Server
	s        store.Store
	operator *store.User
	other    *store.User
	target   *api.RuntimeTargetDescriptor
}

func (f *flatRegFixture) register(t *testing.T, as *store.User, req CreateBrokerRegistrationRequest) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, f.srv, as, http.MethodPost, "/api/v1/brokers", req)
}

func (f *flatRegFixture) join(t *testing.T, req BrokerJoinRequest) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestNoAuth(t, f.srv, http.MethodPost, "/api/v1/brokers/join", req)
}

// registerFlat performs a full flat registration and join, returning the
// broker ID.
func (f *flatRegFixture) registerFlat(t *testing.T, id, name string) string {
	t.Helper()
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: name, RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	rec = f.join(t, BrokerJoinRequest{BrokerID: resp.BrokerID, JoinToken: resp.JoinToken, Hostname: name, Version: "0.1.0", RuntimeTarget: f.target})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	return resp.BrokerID
}
