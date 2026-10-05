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

// Frozen group F (dispatch half) Hub tests of the flat Runtime Broker
// contract (.design/flat-runtime-brokers-contract.md section 15). They are
// written in full in P1.1 and skipped until P1.2 (ptone/scion#3268) wires
// flat dispatch: P1.2 removes pendingFlatDispatch and must make them pass
// unchanged, changing only the bodies of the named F-arrange helpers
// (registerEmbeddedFlatForTest). Fields that P1.1 does not add to request
// structs (expectedRuntimeTargetId on the Hub create request and on the
// Runtime Broker request) are sent and asserted as raw JSON.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const pendingFlatDispatch = "pending ptone/scion#3268: flat dispatch not wired yet"

// --- fixture -------------------------------------------------------------

type flatHubOpts struct {
	experimentOn bool
	linkFlat     bool // flat row is a provider of the project
	flatDefault  bool // flat row is the project default (implies linkFlat)
}

type flatHubFixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	flat    *store.RuntimeBroker // flat Runtime Broker (docker target)
	legacy  *store.RuntimeBroker // legacy, profile-based Runtime Broker
	client  *mockRuntimeBrokerClient
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
	if opts.linkFlat || opts.flatDefault {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: project.ID, BrokerID: flat.ID, BrokerName: flat.Name, Status: store.BrokerStatusOnline}))
	}
	if opts.flatDefault {
		project.DefaultRuntimeBrokerID = flat.ID
	}
	require.NoError(t, s.UpdateProject(ctx, project))

	client := &mockRuntimeBrokerClient{}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	setFlatExperiment(t, srv, opts.experimentOn)

	flat, err := s.GetRuntimeBroker(ctx, flat.ID)
	require.NoError(t, err)
	legacy, err = s.GetRuntimeBroker(ctx, legacy.ID)
	require.NoError(t, err)
	return &flatHubFixture{srv: srv, s: s, project: project, flat: flat, legacy: legacy, client: client}
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
	a := f.pinnedAgent(t, slug, phase)
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
	a := &store.Agent{
		ID:              tid("flat-agent-" + slug + "-" + t.Name()),
		Slug:            slug,
		Name:            slug,
		ProjectID:       f.project.ID,
		RuntimeBrokerID: brokerID,
		Phase:           phase,
		AppliedConfig:   &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
	}
	require.NoError(t, f.s.CreateAgent(context.Background(), a))
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

func decodeFlatAPIError(t *testing.T, rec *httptest.ResponseRecorder) APIError {
	t.Helper()
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "body: %s", rec.Body.String())
	return resp.Error
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

// jsonField marshals v and returns the top-level field key ("" if absent).
func jsonField(t *testing.T, v interface{}, key string) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var m map[string]interface{}
	require.NoError(t, json.Unmarshal(b, &m))
	s, _ := m[key].(string)
	return s
}

// noAgentWritten asserts the create left no trace: no agent row, no run
// intent and no dispatch.
func (f *flatHubFixture) noAgentWritten(t *testing.T, slug string) {
	t.Helper()
	assert.Nil(t, f.agentBySlug(t, slug), "no agent row may be written")
	assert.False(t, f.client.createCalled, "nothing may be dispatched")
	assert.False(t, f.client.startCalled, "nothing may be dispatched")
}

func flatMemberUser(t *testing.T, s store.Store, projectID, name string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(name), Email: name + "@example.com", DisplayName: name, Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	createTestUserWithProjectRole(t, s, u.ID, u.Email, projectID, store.ProjectRoleMember)
	return u
}

// --- create ----------------------------------------------------------------

func TestFlatCreate_ExpectedTargetMismatchRejectedBeforeSideEffects(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	// The flat row is not linked, so a successful create would also have to
	// link it: the refusal must come before any link.
	rec := f.create(t, map[string]interface{}{
		"name": "mismatch", "runtimeBrokerId": f.flat.ID, "task": "t",
		"expectedRuntimeTargetId": "some-other-target",
	})
	// Unlinked flat rows are answered by the resolver first (422, nothing
	// written); then link it so the mismatch itself is exercised.
	requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodeRuntimeBrokerNotLinked)
	f.noAgentWritten(t, "mismatch")
	require.NoError(t, f.s.AddProjectProvider(context.Background(), &store.ProjectProvider{ProjectID: f.project.ID, BrokerID: f.flat.ID, BrokerName: f.flat.Name, Status: store.BrokerStatusOnline}))
	before := f.providerIDs(t, f.project.ID)
	rec = f.create(t, map[string]interface{}{
		"name": "mismatch", "runtimeBrokerId": f.flat.ID, "task": "t",
		"expectedRuntimeTargetId": "some-other-target",
	})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
	assert.Equal(t, "some-other-target", d["expectedRuntimeTargetId"])
	assert.Equal(t, f.flat.RuntimeTarget.ID, d["actualRuntimeTargetId"])
	requireNoStartMarkers(t, d)
	f.noAgentWritten(t, "mismatch")
	assert.ElementsMatch(t, before, f.providerIDs(t, f.project.ID), "no provider link may be written")
	assert.Equal(t, f.legacy.ID, f.projectDefault(t, f.project.ID), "the project default must not change")
}

func TestFlatCreate_ExpectedTargetTowardLegacyBrokerRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	rec := f.create(t, map[string]interface{}{
		"name": "toward-legacy", "runtimeBrokerId": f.legacy.ID, "task": "t",
		"expectedRuntimeTargetId": f.flat.RuntimeTarget.ID,
	})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.Equal(t, "", d["actualRuntimeTargetId"], "a legacy row has no runtime target")
	f.noAgentWritten(t, "toward-legacy")

	// Experiment off: the same request gets 412 experiment_disabled.
	setFlatExperiment(t, f.srv, false)
	rec = f.create(t, map[string]interface{}{
		"name": "toward-legacy-off", "runtimeBrokerId": f.legacy.ID, "task": "t",
		"expectedRuntimeTargetId": f.flat.RuntimeTarget.ID,
	})
	requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	f.noAgentWritten(t, "toward-legacy-off")
}

func TestFlatCreate_CheckPrecedence(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	t.Run("new create: empty-per-agent capability 412 before experiment_disabled", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
		f.project.Labels = map[string]string{store.LabelWorkspaceMode: string(store.SharingModeEmptyPerAgent)}
		require.NoError(t, f.s.UpdateProject(ctx, f.project))
		rec := f.create(t, map[string]interface{}{"name": "p1", "runtimeBrokerId": f.flat.ID, "task": "t",
			"profile": "local", "expectedRuntimeTargetId": "other"})
		requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeUnsupportedCapability)
	})
	t.Run("new create: experiment, then target, then profile", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
		body := map[string]interface{}{"name": "p2", "runtimeBrokerId": f.flat.ID, "task": "t",
			"profile": "local", "expectedRuntimeTargetId": "other"}
		requireAPIError(t, f.create(t, body), http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		setFlatExperiment(t, f.srv, true)
		requireAPIError(t, f.create(t, body), http.StatusConflict, ErrCodeRuntimeTargetMismatch)
		delete(body, "expectedRuntimeTargetId")
		requireAPIError(t, f.create(t, body), http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported)
		f.noAgentWritten(t, "p2")
	})
	t.Run("lifecycle branch: stale pin, then client target, then profile", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		f.stalePinnedAgent(t, "stale-p", string(state.PhaseStopped))
		body := map[string]interface{}{"name": "stale-p", "task": "t", "profile": "local", "expectedRuntimeTargetId": "other"}
		requireAPIError(t, f.create(t, body), http.StatusConflict, ErrCodeRuntimeTargetPinStale)

		f.pinnedAgent(t, "pinned-p", string(state.PhaseStopped))
		body = map[string]interface{}{"name": "pinned-p", "task": "t", "profile": "local", "expectedRuntimeTargetId": "other"}
		requireAPIError(t, f.create(t, body), http.StatusConflict, ErrCodeRuntimeTargetMismatch)
		delete(body, "expectedRuntimeTargetId")
		requireAPIError(t, f.create(t, body), http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported)
		assert.False(t, f.client.startCalled)
	})
}

func TestFlatCreate_ExplicitProfileRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	rec := f.create(t, map[string]interface{}{"name": "with-profile", "runtimeBrokerId": f.flat.ID, "task": "t", "profile": "local"})
	d := requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported)
	assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
	assert.Equal(t, "local", d["profile"])
	f.noAgentWritten(t, "with-profile")
}

func TestFlatCreate_DefaultProfileNotAppliedWithWarning(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	setProjectAnnotations(t, f.s, f.project, map[string]string{projectSettingActiveProfile: "local"})
	rec := f.create(t, map[string]interface{}{"name": "default-profile", "runtimeBrokerId": f.flat.ID, "task": "t"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	a := f.agentBySlug(t, "default-profile")
	require.NotNil(t, a)
	require.NotNil(t, a.AppliedConfig)
	assert.Empty(t, a.AppliedConfig.Profile, "a project default profile is not applied to a flat target")
	if a.AppliedConfig.CreateInputs != nil {
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile)
	}
	require.True(t, f.client.createCalled)
	require.NotNil(t, f.client.lastCreateReq.Config)
	assert.Empty(t, f.client.lastCreateReq.Config.Profile, "no profile is sent to a flat Runtime Broker")
	found := false
	for _, w := range resp.Warnings {
		if strings.Contains(w, `default Runtime Broker Profile "local" was not applied`) {
			found = true
		}
	}
	assert.True(t, found, "a dispatch warning must report the dropped default profile, got %v", resp.Warnings)
}

func TestFlatCreate_PassthroughGateUsesTargetType(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	f.srv.SetEmbeddedBrokerID(f.flat.ID)
	allowed, pinnedProfile := f.srv.hubDefaultPassthroughAllowed(context.Background(), f.flat.ID, f.project.ID, "agent", "")
	assert.True(t, allowed, "a docker flat target is a local container runtime: the gate evaluates RuntimeTarget.Type")
	assert.Empty(t, pinnedProfile, "no profile is pinned for a flat target")
}

func TestFlatCreate_PinsPlacementAndSendsExpectedTarget(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	rec := f.create(t, map[string]interface{}{"name": "pinned", "runtimeBrokerId": f.flat.ID, "task": "t"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	a := f.agentBySlug(t, "pinned")
	require.NotNil(t, a)
	assert.Equal(t, f.flat.ID, a.RuntimeBrokerID)
	assert.Equal(t, f.flat.ID, a.PinnedRuntimeBrokerID)
	assert.Equal(t, f.flat.RuntimeTarget.ID, a.PinnedRuntimeTargetID)
	assert.Equal(t, "docker", a.PinnedRuntimeTargetType)
	require.True(t, f.client.createCalled)
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID)
	assert.Equal(t, f.flat.RuntimeTarget.ID, jsonField(t, f.client.lastCreateReq, "expectedRuntimeTargetId"))
}

func TestFlatCreate_ExperimentOffRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
	rec := f.create(t, map[string]interface{}{"name": "off", "runtimeBrokerId": f.flat.ID, "task": "t"})
	d := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	assert.Equal(t, experiments.FlatRuntimeBrokers, d["experiment"])
	f.noAgentWritten(t, "off")
}

func TestFlatCreate_ExperimentOffExistingPinnedAgentLifecycleWorks(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
	a := f.pinnedAgent(t, "resume-me", string(state.PhaseStopped))
	// No runtimeBrokerId: the resolved default (legacy) differs from the
	// agent's Runtime Broker; create-on-existing resumes it in place.
	rec := f.create(t, map[string]interface{}{"name": "resume-me", "task": "t"})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "resume must work with the experiment off: %d %s", rec.Code, rec.Body.String())
	require.True(t, f.client.startCalled)
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID, "dispatched to the agent's own Runtime Broker")
	assert.Equal(t, f.flat.RuntimeTarget.ID, jsonField(t, f.client.lastStartExtras, "ExpectedRuntimeTargetID"),
		"the expected target is sent whatever the experiment state")

	// Stop it again and resume with a matching client-supplied target.
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	got.Phase = string(state.PhaseStopped)
	require.NoError(t, f.s.UpdateAgent(context.Background(), got))
	f.client.startCalled = false
	rec = f.create(t, map[string]interface{}{"name": "resume-me", "task": "t", "expectedRuntimeTargetId": f.flat.RuntimeTarget.ID})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "matching target accepted with the experiment off: %d %s", rec.Code, rec.Body.String())
	assert.True(t, f.client.startCalled)
}

func TestFlatCreate_ExistingAgentChecksUseAgentBrokerNotResolved(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	f.pinnedAgent(t, "existing", string(state.PhaseStopped))
	// The resolved Runtime Broker (explicit legacy) is not the one checked:
	// the agent's pin is. A matching expected target passes, a mismatching
	// one is compared with the pin and refused.
	rec := f.create(t, map[string]interface{}{"name": "existing", "runtimeBrokerId": f.legacy.ID, "task": "t",
		"expectedRuntimeTargetId": f.flat.RuntimeTarget.ID})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "%d %s", rec.Code, rec.Body.String())
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID)
}

func TestFlatCreate_DeleteAndRecreateChecksBeforeDelete(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
	// A legacy agent in a terminal phase; recreating it on the flat row is a
	// new create, refused (experiment off) before the delete.
	old := f.unpinnedAgentOn(t, "recreate-me", f.legacy.ID, string(state.PhaseError))
	rec := f.create(t, map[string]interface{}{"name": "recreate-me", "runtimeBrokerId": f.flat.ID, "task": "t"})
	requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	still, err := f.s.GetAgent(context.Background(), old.ID)
	require.NoError(t, err, "the existing agent must not be deleted")
	assert.Equal(t, f.legacy.ID, still.RuntimeBrokerID)
	assert.False(t, f.client.deleteCalled)
}

func TestFlatCreate_ExistingAgentWithoutBrokerTreatedAsNewCreate(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.unpinnedAgentOn(t, "no-broker", "", string(state.PhaseCreated))
	before := a.StateVersion

	rec := f.create(t, map[string]interface{}{"name": "no-broker", "runtimeBrokerId": f.flat.ID, "task": "t"})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "%d %s", rec.Code, rec.Body.String())
	got, err := f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, f.flat.ID, got.RuntimeBrokerID)
	assert.True(t, got.PinValid(), "pinned together with runtime_broker_id")
	assert.Greater(t, got.StateVersion, before)
	assert.Equal(t, f.flat.RuntimeTarget.ID, jsonField(t, f.client.lastStartExtras, "ExpectedRuntimeTargetID"))
	assert.NotEqual(t, string(state.PhaseCreated), got.Phase, "the post-dispatch update landed without a version conflict")

	// A failed start leaves the first placement persisted.
	b := f.unpinnedAgentOn(t, "no-broker-fail", "", string(state.PhaseCreated))
	f.client.returnErr = &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: `{"error":{"code":"runtime_error","message":"boom"}}`}
	_ = f.create(t, map[string]interface{}{"name": "no-broker-fail", "runtimeBrokerId": f.flat.ID, "task": "t"})
	gotB, err := f.s.GetAgent(ctx, b.ID)
	require.NoError(t, err)
	assert.True(t, gotB.PinValid(), "the first placement stays persisted when the start fails")

	// A concurrent placement gets 409 conflict.
	c := f.unpinnedAgentOn(t, "no-broker-race", "", string(state.PhaseCreated))
	_, err = f.s.SetAgentPinnedRuntimeTarget(ctx, c.ID, store.PinnedPlacement{},
		store.PinnedPlacement{RuntimeBrokerID: f.flat.ID, RuntimeTargetID: f.flat.RuntimeTarget.ID, RuntimeTargetType: "docker"})
	require.NoError(t, err)
	f.client.returnErr = nil
	cur, err := f.s.GetAgent(ctx, c.ID)
	require.NoError(t, err)
	_ = cur
	// (the concurrent writer already placed it: a second first-placement
	// attempt from a stale read must answer 409 conflict)
	_, err = f.s.SetAgentPinnedRuntimeTarget(ctx, c.ID, store.PinnedPlacement{},
		store.PinnedPlacement{RuntimeBrokerID: f.flat.ID, RuntimeTargetID: f.flat.RuntimeTarget.ID, RuntimeTargetType: "docker"})
	require.ErrorIs(t, err, store.ErrPinnedPlacementChanged)
}

func TestFlatCreate_LifecycleClientExpectedTargetComparedWithPin(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	f.pinnedAgent(t, "pinned-life", string(state.PhaseStopped))
	rec := f.create(t, map[string]interface{}{"name": "pinned-life", "task": "t", "expectedRuntimeTargetId": "other"})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.Equal(t, f.flat.RuntimeTarget.ID, d["actualRuntimeTargetId"])

	// An unpinned agent on a legacy row mismatches with an empty actual.
	f.unpinnedAgentOn(t, "legacy-life", f.legacy.ID, string(state.PhaseStopped))
	rec = f.create(t, map[string]interface{}{"name": "legacy-life", "task": "t", "expectedRuntimeTargetId": "other"})
	d = requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.Equal(t, "", d["actualRuntimeTargetId"])
	assert.False(t, f.client.startCalled)
}

func TestFlatCreate_RuntimeBrokerRejectionRelayed(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	cases := []struct {
		status int
		code   string
	}{
		{http.StatusConflict, ErrCodeRuntimeTargetMismatch},
		{http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported},
		{http.StatusPreconditionFailed, ErrCodeRuntimeTargetRequired},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
			f.client.returnErr = &brokerStatusError{StatusCode: c.status, Body: fmt.Sprintf(
				`{"error":{"code":%q,"message":"refused","details":{"runtimeBrokerId":%q,"startAttempted":false}}}`, c.code, f.flat.ID)}
			rec := f.create(t, map[string]interface{}{"name": "relayed", "runtimeBrokerId": f.flat.ID, "task": "t"})
			d := requireAPIError(t, rec, c.status, c.code)
			assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
			requireNoStartMarkers(t, d)
			assert.Nil(t, f.agentBySlug(t, "relayed"), "the create is rolled back")
		})
	}
}

func TestFlatCreate_AccessCheckBeforeLink(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	// A legacy, unlinked, non-auto-provide row: a project member without
	// broker dispatch permission is denied before any link is written.
	unlinked := &store.RuntimeBroker{ID: tid("unlinked-legacy-" + t.Name()), Name: "unlinked-legacy", Slug: "unlinked-legacy",
		Status: store.BrokerStatusOnline, Endpoint: "http://unlinked.invalid"}
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), unlinked))
	member := flatMemberUser(t, f.s, f.project.ID, "flat-access-member")
	before := f.providerIDs(t, f.project.ID)
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents",
		map[string]interface{}{"name": "access", "runtimeBrokerId": unlinked.ID, "task": "t"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.ElementsMatch(t, before, f.providerIDs(t, f.project.ID))
	f.noAgentWritten(t, "access")
}

func TestFlatCreate_UnlinkedFlatBrokerNotAutoLinked(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	rec := f.create(t, map[string]interface{}{"name": "unlinked", "runtimeBrokerId": f.flat.ID, "task": "t"})
	d := requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodeRuntimeBrokerNotLinked)
	assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
	assert.Equal(t, f.project.ID, d["projectId"])
	assert.NotContains(t, f.providerIDs(t, f.project.ID), f.flat.ID, "no provider row")
	assert.Equal(t, f.legacy.ID, f.projectDefault(t, f.project.ID), "no project default")
	f.noAgentWritten(t, "unlinked")
}

func TestFlatCreate_ExplicitLinkThenCreateWithBrokerIDOnly(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/providers", AddProviderRequest{BrokerID: f.flat.ID})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "explicit link: %d %s", rec.Code, rec.Body.String())
	assert.Contains(t, f.providerIDs(t, f.project.ID), f.flat.ID)

	rec = f.create(t, map[string]interface{}{"name": "slice", "runtimeBrokerId": f.flat.ID, "task": "t"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	a := f.agentBySlug(t, "slice")
	require.NotNil(t, a)
	assert.True(t, a.PinValid())
	assert.Equal(t, f.flat.RuntimeTarget.ID, a.PinnedRuntimeTargetID)
}

func TestFlatCreate_UnlinkedFlatRowAuthorizationWins(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	member := flatMemberUser(t, f.s, f.project.ID, "flat-unlinked-member")
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents",
		map[string]interface{}{"name": "authz-wins", "runtimeBrokerId": f.flat.ID, "task": "t"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "authorization wins over runtime_broker_not_linked: %s", rec.Body.String())
	assert.NotEqual(t, ErrCodeRuntimeBrokerNotLinked, decodeFlatAPIError(t, rec).Code)
	f.noAgentWritten(t, "authz-wins")
}

func TestFlatCreate_UnlinkedFlatRowNotReachedThroughDefaults(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	// Point the project default at the unlinked flat row (as a stale default
	// could): selection only considers linked providers, so the create falls
	// through to the linked legacy row exactly as today.
	f.project.DefaultRuntimeBrokerID = f.flat.ID
	require.NoError(t, f.s.UpdateProject(ctx, f.project))
	rec := f.create(t, map[string]interface{}{"name": "defaults", "task": "t"})
	require.NotEqual(t, http.StatusUnprocessableEntity, rec.Code, "the default fall-through never yields runtime_broker_not_linked: %s", rec.Body.String())
	if rec.Code == http.StatusCreated {
		a := f.agentBySlug(t, "defaults")
		require.NotNil(t, a)
		assert.Equal(t, f.legacy.ID, a.RuntimeBrokerID)
		assert.False(t, a.IsPinned())
	}
	assert.NotContains(t, f.providerIDs(t, f.project.ID), f.flat.ID)
}

func TestFlatCreate_AuthorizationBeforeFlatChecks(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	// Fixture: a linked flat row, experiment off, with a profile and a
	// mismatching expected target: every flat check would fail.
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
	member := flatMemberUser(t, f.s, f.project.ID, "flat-authz-member")
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents",
		map[string]interface{}{"name": "authz-first", "runtimeBrokerId": f.flat.ID, "task": "t",
			"profile": "local", "expectedRuntimeTargetId": "other"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	code := decodeFlatAPIError(t, rec).Code
	for _, flatCode := range []string{ErrCodeExperimentDisabled, ErrCodeRuntimeTargetMismatch, ErrCodeRuntimeProfileUnsupported} {
		assert.NotEqual(t, flatCode, code, "no flat code is reported when authorization fails")
	}
	f.noAgentWritten(t, "authz-first")
}

func TestFlatCreate_FlatChecksDoNotGrantDispatch(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	member := flatMemberUser(t, f.s, f.project.ID, "flat-nogrant-member")
	// Passes every flat check (experiment on, matching target, no profile).
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents",
		map[string]interface{}{"name": "no-grant", "runtimeBrokerId": f.flat.ID, "task": "t",
			"expectedRuntimeTargetId": f.flat.RuntimeTarget.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, "canDispatchToBroker still decides: %s", rec.Body.String())
	f.noAgentWritten(t, "no-grant")
}

func TestLegacyCreate_AgentCallerCreateTimeLinkUnchanged(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := brokerLinkAuthzSetup(t)
	f.unlinked.AutoProvide = false
	require.NoError(t, f.store.UpdateRuntimeBroker(context.Background(), f.unlinked))
	setFlatExperiment(t, f.srv, true)

	// An agent caller with agent-create scope names an explicit, unlinked
	// legacy row: the in-resolver link still happens, then
	// checkBrokerDispatchAccess passes through brokerServesProject, as today.
	rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "agent-linked", RuntimeBrokerID: f.unlinked.ID}, ScopeAgentCreate)
	assert.NotEqual(t, http.StatusUnprocessableEntity, rec.Code, "legacy rows are linked as today: %s", rec.Body.String())
	assert.NotEqual(t, ErrCodeRuntimeBrokerNotLinked, decodeAPIErrorIfAny(rec))

	// A caller denied by canDispatchToBroker who sends an
	// expectedRuntimeTargetId toward an unlinked legacy row gets today's 403.
	rec = doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		map[string]interface{}{"name": "denied-legacy", "runtimeBrokerId": f.unlinked.ID, "expectedRuntimeTargetId": "x"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func decodeAPIErrorIfAny(rec *httptest.ResponseRecorder) string {
	var resp ErrorResponse
	if json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		return ""
	}
	return resp.Error.Code
}

func TestLegacyCreate_ExperimentOffUnchanged(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false})
	rec := f.create(t, map[string]interface{}{"name": "legacy", "runtimeBrokerId": f.legacy.ID, "task": "t", "profile": "local"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	a := f.agentBySlug(t, "legacy")
	require.NotNil(t, a)
	assert.False(t, a.IsPinned())
	assert.Equal(t, "local", a.AppliedConfig.Profile)
	require.True(t, f.client.createCalled)
	assert.Equal(t, "", jsonField(t, f.client.lastCreateReq, "expectedRuntimeTargetId"), "never sent toward a legacy row")
	require.NotNil(t, f.client.lastCreateReq.Config)
	assert.Equal(t, "local", f.client.lastCreateReq.Config.Profile)
}

// --- start / restart / wake / reconcile -------------------------------------

func requireStalePinDetails(t *testing.T, d map[string]interface{}, a *store.Agent, f *flatHubFixture) {
	t.Helper()
	assert.Equal(t, a.ID, d["agentId"])
	assert.Equal(t, f.flat.ID, d["pinnedRuntimeBrokerId"])
	assert.Equal(t, f.legacy.ID, d["runtimeBrokerId"])
}

func TestFlatStart_StalePinRefused_Handler(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-start", string(state.PhaseStopped))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	requireStalePinDetails(t, d, a, f)
	assert.False(t, f.client.startCalled, "refused before dispatch, never 502")
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.RunIntent, got.RunIntent, "refused before the run intent write")
}

func TestFlatStart_StalePinRefused_Restart(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-restart", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	requireStalePinDetails(t, d, a, f)
}

func TestFlatStart_StalePinRefused_Reconcile(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-reconcile", string(state.PhaseStopped))
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Task: "t"})
	require.NoError(t, err)
	result, execErr := f.srv.executeDispatch(context.Background(), store.BrokerDispatch{
		ID: tid("dispatch-" + t.Name()), BrokerID: a.RuntimeBrokerID, AgentID: a.ID, Op: "start", Args: args,
	})
	require.Error(t, execErr, "the dispatcher backstop refuses")
	var refusal *RuntimeTargetRefusal
	require.True(t, errors.As(execErr, &refusal), "a typed refusal, got %v", execErr)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, refusal.Code)
	assert.Equal(t, http.StatusConflict, refusal.Status)
	assert.Contains(t, result, ErrCodeRuntimeTargetPinStale, "the refusal travels in the dispatch result envelope")
	assert.False(t, f.client.startCalled)
}

func TestFlatStart_StalePinRefused_WakeDM(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-wake", string(state.PhaseSuspended))
	_, dmErr := f.srv.wakeAgentForDM(context.Background(), a)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, dmErr.Code)
	assert.False(t, f.client.startCalled)
}

func TestFlatRestart_StalePinRefusedBeforeStopLeg(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-restart-stop", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/restart", nil)
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	requireStalePinDetails(t, d, a, f)
	assert.False(t, f.client.stopCalled, "no stop leg is dispatched")
	assert.False(t, f.client.startCalled)
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.RunIntent, got.RunIntent, "run intent unchanged")
	assert.Equal(t, string(state.PhaseRunning), got.Phase, "no reservation or phase change")
}

func TestFlatStart_StalePinRefusalIsConfirmedNotActedOn(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	refusal := &RuntimeTargetRefusal{Code: ErrCodeRuntimeTargetPinStale, Status: http.StatusConflict, Message: "stale"}
	assert.True(t, isConfirmedStartNotActedOnError(refusal))
	assert.True(t, isConfirmedStartNotActedOnError(fmt.Errorf("wrapped: %w", refusal)))

	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-classify", string(state.PhaseStopped))
	err := f.srv.GetDispatcher().DispatchAgentStart(context.Background(), a, "t", false)
	require.Error(t, err)
	assert.True(t, isConfirmedStartNotActedOnError(err))
	assert.False(t, f.client.startCalled, "no credential is minted and nothing is sent")
	assert.False(t, f.client.stopCalled, "no compensating stop")
}

func TestFlatStart_UnpinnedAgentOnFlatRowIsStale(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.unpinnedAgentOn(t, "unpinned-on-flat", f.flat.ID, string(state.PhaseStopped))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	assert.Equal(t, "", d["pinnedRuntimeBrokerId"])
	assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
}

func TestFlatStart_RuntimeBrokerMismatchOnStartRelayed(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "mismatch-start", string(state.PhaseStopped))
	f.client.returnErr = &brokerStatusError{StatusCode: http.StatusConflict, Body: fmt.Sprintf(
		`{"error":{"code":%q,"message":"refused","details":{"runtimeBrokerId":%q,"expectedRuntimeTargetId":"x","actualRuntimeTargetId":%q}}}`,
		ErrCodeRuntimeTargetMismatch, f.flat.ID, f.flat.RuntimeTarget.ID)}
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.NotEqual(t, http.StatusBadGateway, rec.Code)
	requireNoStartMarkers(t, d)
}

func TestFlatStart_RefusalIsTerminalForIntent(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "terminal", string(state.PhaseStopped))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Contains(t, got.Message, ErrCodeRuntimeTargetPinStale, "the agent message carries the refusal")

	// Executing the same start again (as a cross-node executor would) does
	// not dispatch: no hot retry loop.
	args, err := MarshalDispatchArgs(&StartDispatchArgs{Task: "t"})
	require.NoError(t, err)
	_, _ = f.srv.executeDispatch(context.Background(), store.BrokerDispatch{
		ID: tid("dispatch-terminal-" + t.Name()), BrokerID: got.RuntimeBrokerID, AgentID: got.ID, Op: "start", Args: args,
	})
	assert.False(t, f.client.startCalled, "the same intent is never re-dispatched")
}

func TestFlatStart_CrossNodeRefusalRebuiltFromEnvelope(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	refusal := &RuntimeTargetRefusal{
		Code: ErrCodeRuntimeTargetPinStale, Status: http.StatusConflict, Message: "stale",
		Details: map[string]interface{}{"agentId": "a", "pinnedRuntimeBrokerId": "p", "runtimeBrokerId": "r"},
	}
	result := dispatchFailureResult(refusal)
	require.NotEmpty(t, result)
	assert.Contains(t, result, ErrCodeRuntimeTargetPinStale, "the executing node carries the typed refusal in the envelope")
	assert.Contains(t, result, `"pinnedRuntimeBrokerId"`)
	assert.Contains(t, result, "409")
}

// --- reincarnate / finalize-env ----------------------------------------------

func TestFlatReincarnate_StalePinRefusedBeforeStop(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-reinc", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	requireStalePinDetails(t, d, a, f)
	assert.False(t, f.client.stopCalled, "no stop")
	assert.False(t, f.client.createCalled, "no reprovision, no credential mint")
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.Generation, got.Generation, "no reincarnation record")
	assert.Equal(t, a.ReincarnationState, got.ReincarnationState)
}

func TestFlatReincarnate_ReprovisionSendsExpectedTarget(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "reprovision", string(state.PhaseStopped))
	require.NoError(t, f.srv.GetDispatcher().DispatchAgentReprovision(context.Background(), a))
	require.True(t, f.client.createCalled)
	assert.Equal(t, f.flat.RuntimeTarget.ID, jsonField(t, f.client.lastCreateReq, "expectedRuntimeTargetId"))
}

func TestFlatFinalizeEnv_SendsExpectedTarget(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "finalize", string(state.PhaseProvisioning))
	_, err := f.srv.GetDispatcher().DispatchFinalizeEnv(context.Background(), a, map[string]string{"K": "v"})
	require.NoError(t, err)
	require.True(t, f.client.createCalled)
	assert.Equal(t, f.flat.RuntimeTarget.ID, jsonField(t, f.client.lastCreateReq, "expectedRuntimeTargetId"))
}

func TestFlatReincarnate_MoveStillNotImplemented(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "move", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{TargetBroker: f.legacy.ID})
	requireAPIError(t, rec, http.StatusNotImplemented, ErrCodeNotImplemented)
}

func TestFlatReincarnate_MoveDryRunReportsPinnedIneligible(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.pinnedAgent(t, "move-dry", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, TargetBroker: f.legacy.ID})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported)
	verdictJSON, err := json.Marshal(d["verdict"])
	require.NoError(t, err)
	var verdict MoveVerdict
	require.NoError(t, json.Unmarshal(verdictJSON, &verdict))
	require.NotEmpty(t, verdict.Checks)
	assert.Equal(t, "runtime_target", verdict.Checks[0].Name, "runtime_target is the first check")
	assert.NotEqual(t, MoveCheckNotEvaluated, verdict.Checks[0].Result)
	for _, c := range verdict.Checks[1:] {
		assert.Equal(t, MoveCheckNotEvaluated, c.Result, "check %s", c.Name)
	}

	// Moving a legacy agent onto a flat Runtime Broker is ineligible too.
	l := f.unpinnedAgentOn(t, "move-onto-flat", f.legacy.ID, string(state.PhaseRunning))
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+l.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, TargetBroker: f.flat.ID})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported)
}

func TestFlatReincarnate_DryRunMoveOfStalePinGetsPlanAnswer(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-move-dry", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, TargetBroker: f.flat.ID})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported)
	assert.NotEqual(t, ErrCodeRuntimeTargetPinStale, decodeFlatAPIError(t, rec).Code)
}

func TestFlatReincarnate_ProfileNotRederived(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	setProjectAnnotations(t, f.s, f.project, map[string]string{projectSettingActiveProfile: "local"})
	a := f.pinnedAgent(t, "reinc-profile", string(state.PhaseStopped))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"profile":"local"`, "the project active profile is not re-derived for a pinned agent")
}

// --- scheduler -------------------------------------------------------------

func fireScheduledCreate(t *testing.T, srv *Server, s store.Store, projectID, agentName string) error {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetUser(ctx, DevUserID); errors.Is(err, store.ErrNotFound) {
		require.NoError(t, s.CreateUser(ctx, &store.User{ID: DevUserID, Email: "dev@localhost", DisplayName: "Dev User", Role: store.UserRoleAdmin}))
	}
	payload, err := json.Marshal(DispatchAgentEventPayload{AgentName: agentName, Task: "scheduled"})
	require.NoError(t, err)
	return srv.dispatchAgentEventHandler()(ctx, store.ScheduledEvent{
		ID: tid("sched-" + agentName + "-" + t.Name()), ProjectID: projectID, EventType: "dispatch_agent",
		Payload: string(payload), CreatedBy: DevUserID,
	})
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

func TestFlatScheduledCreate_PinsPlacement(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	p := f.flatOnlyProject(t)
	f.srv.SetEmbeddedBrokerID(f.flat.ID) // the passthrough default could otherwise pin a profile
	require.NoError(t, fireScheduledCreate(t, f.srv, f.s, p.ID, "scheduled"))
	a, err := f.s.GetAgentBySlug(context.Background(), p.ID, "scheduled")
	require.NoError(t, err)
	assert.True(t, a.PinValid())
	assert.Equal(t, f.flat.RuntimeTarget.ID, a.PinnedRuntimeTargetID)
	require.NotNil(t, a.AppliedConfig)
	assert.Empty(t, a.AppliedConfig.Profile, "neither the passthrough nor the project active profile is applied")
	require.True(t, f.client.createCalled)
	assert.Equal(t, f.flat.RuntimeTarget.ID, jsonField(t, f.client.lastCreateReq, "expectedRuntimeTargetId"))
	require.NotNil(t, f.client.lastCreateReq.Config)
	assert.Empty(t, f.client.lastCreateReq.Config.Profile,
		"flatCreatePlacement ran before applyScheduledProjectDefaultGCPIdentity and deriveAgentConfig")
}

func TestFlatScheduledCreate_ExperimentOffRefused(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false})
	p := f.flatOnlyProject(t)
	err := fireScheduledCreate(t, f.srv, f.s, p.ID, "scheduled-off")
	require.Error(t, err, "the scheduler records a failed run")
	var refusal *RuntimeTargetRefusal
	require.True(t, errors.As(err, &refusal), "a typed refusal, got %v", err)
	assert.Equal(t, ErrCodeExperimentDisabled, refusal.Code)
	_, getErr := f.s.GetAgentBySlug(context.Background(), p.ID, "scheduled-off")
	assert.ErrorIs(t, getErr, store.ErrNotFound, "refused before any write")
	assert.False(t, f.client.createCalled)
}

// --- links and auto-provide --------------------------------------------------

func TestFlatAutoProvide_NoAutomaticProjectLink(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("experiment=%v", on), func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: on})
			f.flat.AutoProvide = true
			require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), f.flat))
			f.srv.SetEmbeddedBrokerID(f.flat.ID) // the embedded flat instance gets no link either
			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", map[string]interface{}{"name": "auto-provide-target"})
			require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "%d %s", rec.Code, rec.Body.String())
			var created struct {
				ID      string         `json:"id"`
				Project *store.Project `json:"project"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
			pid := created.ID
			if created.Project != nil {
				pid = created.Project.ID
			}
			require.NotEmpty(t, pid)
			assert.NotContains(t, f.providerIDs(t, pid), f.flat.ID, "no automatic link to a flat row")
			assert.NotEqual(t, f.flat.ID, f.projectDefault(t, pid), "no default set to a flat row")
		})
	}
}

func TestRegisterProjectBrokerID_FlatRowRefused(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "linked-by-id", BrokerID: f.flat.ID})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerLinkPathUnsupported)
	assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
	projects, err := f.s.ListProjects(context.Background(), store.ProjectFilter{Name: "linked-by-id"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, projects.Items, "refused before any project mutation")

	// A legacy row is linked as today.
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{Name: "linked-legacy", BrokerID: f.legacy.ID})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

func TestDeprecatedRegisterProject_DoesNotAdoptFlatRow(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	// A flat row found only by name: no adoption, no duplicate.
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "deprecated-by-name", Broker: &RegisterProjectBrokerInfo{Name: strings.ToUpper(f.flat.Name)},
	})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNameConflict)
	// A flat row found by ID.
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/register", RegisterProjectRequest{
		Name: "deprecated-by-id", Broker: &RegisterProjectBrokerInfo{ID: f.flat.ID, Name: "whatever",
			Profiles: []store.BrokerProfile{{Name: "local", Type: "docker"}}},
	})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	for _, name := range []string{"deprecated-by-name", "deprecated-by-id"} {
		projects, err := f.s.ListProjects(context.Background(), store.ProjectFilter{Name: name}, store.ListOptions{})
		require.NoError(t, err)
		assert.Empty(t, projects.Items, "decided before any project mutation")
	}
	got, err := f.s.GetRuntimeBroker(context.Background(), f.flat.ID)
	require.NoError(t, err)
	assert.Equal(t, f.flat.Name, got.Name)
	assert.Empty(t, got.Profiles, "never writes profiles to a flat row")
}

func TestAdminPatch_RenameCollidingWithFlatRowRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	rec := doRequest(t, f.srv, http.MethodPatch, "/api/v1/runtime-brokers/"+f.legacy.ID, map[string]interface{}{"name": f.flat.Name})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNameConflict)
	rec = doRequest(t, f.srv, http.MethodPatch, "/api/v1/runtime-brokers/"+f.flat.ID, map[string]interface{}{"name": f.legacy.Name})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNameConflict)
	got, err := f.s.GetRuntimeBroker(context.Background(), f.legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, "legacy-broker", got.Name)
}

func TestHeartbeat_DropsProfilesForFlatRow(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	grantDevUserRuntimeBrokerAccess(t, f.s)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/runtime-brokers/"+f.flat.ID+"/heartbeat", map[string]interface{}{
		"status":         "online",
		"defaultProfile": "local",
		"profileAttach":  []map[string]interface{}{{"name": "local", "attach": true}},
		"capabilities":   map[string]interface{}{"sync": true, "reprovision": true, "attach": true},
	})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusNoContent, "%d %s", rec.Code, rec.Body.String())
	got, err := f.s.GetRuntimeBroker(context.Background(), f.flat.ID)
	require.NoError(t, err)
	assert.Empty(t, got.DefaultProfile)
	assert.Empty(t, got.Profiles)
	require.NotNil(t, got.Capabilities)
	assert.True(t, got.Capabilities.Attach, "capabilities refresh as today")
	assert.NotNil(t, got.RuntimeTarget)
}

// --- registration ------------------------------------------------------------

type flatRegFixture struct {
	srv      *Server
	s        store.Store
	operator *store.User
	other    *store.User
	target   *api.RuntimeTargetDescriptor
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

func TestFlatRegistration_TargetChangeRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-change"), "flat-change")
	other := &api.RuntimeTargetDescriptor{ID: tid("other-target"), Type: "docker"}
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-change", RuntimeTarget: other})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	assert.Equal(t, id, d["runtimeBrokerId"])
	assert.Equal(t, f.target.ID, d["storedRuntimeTargetId"])
	assert.Equal(t, other.ID, d["reportedRuntimeTargetId"])
	got, err := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, f.target.ID, got.RuntimeTarget.ID, "stored target unchanged")
}

func TestFlatRegistration_JoinDescriptorMismatchKeepsSecret(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-secret"), "flat-secret")
	before, err := f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)

	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-secret", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: resp.JoinToken, Hostname: "flat-secret", Version: "0.1.0",
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("wrong-target"), Type: "docker"}})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	after, err := f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, before.SecretKey, after.SecretKey, "a refused join leaves the existing secret untouched")

	// A flat row joined without a descriptor is refused the same way.
	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-secret", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	rec = f.join(t, BrokerJoinRequest{BrokerID: id, JoinToken: resp.JoinToken, Hostname: "flat-secret", Version: "0.1.0"})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	after, err = f.s.GetBrokerSecret(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, before.SecretKey, after.SecretKey)
}

func TestFlatRegistration_ResponseEchoesRuntimeTarget(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: tid("flat-reg-echo"), Name: "flat-echo", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.RuntimeTarget, "the registration response acknowledges the stored binding")
	assert.Equal(t, f.target.ID, resp.RuntimeTarget.ID)
	assert.Equal(t, f.target.Type, resp.RuntimeTarget.Type)

	rec = f.join(t, BrokerJoinRequest{BrokerID: resp.BrokerID, JoinToken: resp.JoinToken, Hostname: "flat-echo", Version: "0.1.0", RuntimeTarget: f.target})
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
	var join BrokerJoinResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &join))
	require.NotNil(t, join.RuntimeTarget, "the join response acknowledges the stored binding")
	assert.Equal(t, f.target.ID, join.RuntimeTarget.ID)

	// A legacy row gets no acknowledgement.
	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{Name: "legacy-echo"})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var legacy CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &legacy))
	assert.Nil(t, legacy.RuntimeTarget)
}

func TestFlatRegistration_LegacyRowNotConverted(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	legacyID := tid("flat-reg-legacy-row")
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: legacyID, Name: "legacy-row", Slug: "legacy-row", Status: store.BrokerStatusOffline, CreatedBy: f.operator.ID,
		Profiles: []store.BrokerProfile{{Name: "local", Type: "docker"}},
	}))
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: legacyID, Name: "legacy-row", RuntimeTarget: f.target})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNotFlat)
	assert.Equal(t, legacyID, d["runtimeBrokerId"])
	got, err := f.s.GetRuntimeBroker(context.Background(), legacyID)
	require.NoError(t, err)
	assert.Nil(t, got.RuntimeTarget, "a legacy row is never converted")
	assert.NotEmpty(t, got.Profiles)
}

func TestFlatRegistration_LegacyReRegistrationOfFlatIDRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-legacy-rereg"), "flat-legacy-rereg")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-legacy-rereg"})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetChanged)
	got, err := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, got.RuntimeTarget)
}

func TestFlatRegistration_NameOrSlugCollisionOnCreateRejected(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	existing := &store.RuntimeBroker{ID: tid("taken-row"), Name: "Taken-Name", Slug: "taken-slug", Status: store.BrokerStatusOffline}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, existing))
	for _, name := range []string{"taken-name", "taken-slug"} {
		rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: tid("new-flat-" + name), Name: name, RuntimeTarget: f.target})
		d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNameConflict)
		assert.Equal(t, existing.ID, d["existingRuntimeBrokerId"])
		_, err := f.s.GetRuntimeBroker(ctx, tid("new-flat-"+name))
		assert.ErrorIs(t, err, store.ErrNotFound, "no row is created")
	}
}

func TestFlatRegistration_ReRegistrationNotBlockedByLaterNameCollision(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-later"), "flat-later")
	// An older binary later created a legacy row with the same name.
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: tid("later-legacy"), Name: "flat-later", Slug: "flat-later-2", Status: store.BrokerStatusOffline}))
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-later", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "re-registration by ID is never blocked by a later collision: %s", rec.Body.String())
}

func TestFlatRegistration_NameChangeInConfigNotApplied(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-rename"), "flat-original")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-renamed", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	got, err := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, "flat-original", got.Name, "name is set only at creation")
}

func TestFlatRegistration_ReRegistrationRequiresOwner(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-owner"), "flat-owner")
	rec := f.register(t, f.other, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-owner", RuntimeTarget: f.target})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
}

func TestFlatRegistration_ExperimentOffRejectsNewAllowsExisting(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-existing"), "flat-existing")
	setFlatExperiment(t, f.srv, false)

	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: tid("flat-reg-new"), Name: "flat-new", RuntimeTarget: f.target})
	d := requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	assert.Equal(t, experiments.FlatRuntimeBrokers, d["experiment"])

	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-existing", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "an existing flat row re-registers with the experiment off: %s", rec.Body.String())
}

func TestLegacyRegistration_NameCollidingWithFlatRowRefused_Brokerauth(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	flatID := f.registerFlat(t, tid("flat-reg-collide"), "flat-collide")
	rec := f.register(t, f.other, CreateBrokerRegistrationRequest{Name: "FLAT-COLLIDE"})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeBrokerNameConflict)
	assert.Equal(t, flatID, d["existingRuntimeBrokerId"])
	_, err := f.s.GetLegacyRuntimeBrokerByName(context.Background(), "flat-collide")
	assert.ErrorIs(t, err, store.ErrNotFound, "no duplicate legacy row next to a flat row")
}

// --- embedded flat registration (F-arrange) ---------------------------------

// registerEmbeddedFlatForTest is the F-arrange helper for the embedded flat
// path. P1.2 replaces its body with a call to
// (*Server).RegisterEmbeddedFlatRuntimeBroker and returns its stored row and
// the activation result (the CheckActivationAck outcome for phase embedded).
func registerEmbeddedFlatForTest(t *testing.T, srv *Server, brokerID, name string, target *api.RuntimeTargetDescriptor) (*store.RuntimeBroker, error) {
	t.Helper()
	_ = srv
	_, _, _ = brokerID, name, target
	t.Fatalf("P1.2 implements RegisterEmbeddedFlatRuntimeBroker")
	return nil, nil
}

// embeddedRegError extracts a Hub error code from an embedded registration
// error (an APIError-style refusal or a binding-conflict ack error).
func embeddedRegCode(err error) string {
	var refusal *RuntimeTargetRefusal
	if errors.As(err, &refusal) {
		return refusal.Code
	}
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, c := range []string{ErrCodeRuntimeTargetChanged, ErrCodeRuntimeBrokerNotFlat, ErrCodeRuntimeBrokerNameConflict,
		ErrCodeExperimentDisabled, api.ErrCodeRuntimeTargetBindingConflict, api.ErrCodeRuntimeTargetAckMissing} {
		if strings.Contains(msg, c) {
			return c
		}
	}
	return msg
}

func TestFlatRegistration_EmbeddedPathUsesSharedRules(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, false)
	// Experiment off: a new embedded flat registration is refused like an
	// HTTP one.
	_, err := registerEmbeddedFlatForTest(t, f.srv, tid("embedded-new"), "embedded-new", f.target)
	assert.Equal(t, ErrCodeExperimentDisabled, embeddedRegCode(err))
	setFlatExperiment(t, f.srv, true)
	row, err := registerEmbeddedFlatForTest(t, f.srv, tid("embedded-new"), "embedded-new", f.target)
	require.NoError(t, err)
	require.NotNil(t, row.RuntimeTarget)
	assert.Equal(t, f.target.ID, row.RuntimeTarget.ID)
	assert.Empty(t, row.Profiles)
}

func TestFlatRegistration_EmbeddedSideDuties(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	f := newFlatRegFixture(t, true)
	row, err := registerEmbeddedFlatForTest(t, f.srv, tid("embedded-duties"), "embedded-duties", f.target)
	require.NoError(t, err)
	assert.True(t, f.srv.isEmbeddedBroker(row.ID), "SetEmbeddedBrokerID(flatID) recorded")
	global, err := f.s.GetProjectBySlug(ctx, "global")
	require.NoError(t, err, "the global project exists")
	providers, err := f.s.GetProjectProviders(ctx, global.ID)
	require.NoError(t, err)
	for _, p := range providers {
		assert.NotEqual(t, row.ID, p.BrokerID, "no provider link for the flat row")
	}
	assert.NotEqual(t, row.ID, global.DefaultRuntimeBrokerID, "no default for the flat row")

	// A refusal is reported without a legacy fallback.
	_, err = registerEmbeddedFlatForTest(t, f.srv, row.ID, "embedded-duties", &api.RuntimeTargetDescriptor{ID: tid("other"), Type: "docker"})
	require.Error(t, err)
	assert.True(t, f.srv.isEmbeddedBroker(row.ID), "no fallback to a legacy identity")
}

func TestFlatRegistration_EmbeddedBoundResultRequired(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := tid("embedded-bound")
	row, err := registerEmbeddedFlatForTest(t, f.srv, id, "embedded-bound", f.target)
	require.NoError(t, err)
	assert.Equal(t, id, row.ID)
	require.NotNil(t, row.RuntimeTarget)
	assert.Equal(t, f.target.ID, row.RuntimeTarget.ID)
	assert.Equal(t, f.target.Type, row.RuntimeTarget.Type)
}

func TestFlatRegistration_EmbeddedConflictingRowNotActivated(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := tid("embedded-conflict")
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: id, Name: "embedded-conflict", Slug: "embedded-conflict", Status: store.BrokerStatusOffline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("stored-other"), Type: "docker"},
	}))
	_, err := registerEmbeddedFlatForTest(t, f.srv, id, "embedded-conflict", f.target)
	require.Error(t, err)
	code := embeddedRegCode(err)
	assert.Contains(t, []string{ErrCodeRuntimeTargetChanged, api.ErrCodeRuntimeTargetBindingConflict}, code)
	assert.False(t, f.srv.isEmbeddedBroker(id), "not activated")
}

func TestFlatRegistration_EmbeddedLegacyRowNotActivated(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := tid("embedded-legacy")
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: id, Name: "embedded-legacy", Slug: "embedded-legacy", Status: store.BrokerStatusOffline}))
	_, err := registerEmbeddedFlatForTest(t, f.srv, id, "embedded-legacy", f.target)
	assert.Equal(t, ErrCodeRuntimeBrokerNotFlat, embeddedRegCode(err))
	assert.False(t, f.srv.isEmbeddedBroker(id))
	got, getErr := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, getErr, "no automatic cleanup")
	assert.Nil(t, got.RuntimeTarget)
}

func TestFlatRegistration_EmbeddedNameCollisionNotAdopted(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	existing := &store.RuntimeBroker{ID: tid("embedded-taken"), Name: "embedded-taken", Slug: "embedded-taken", Status: store.BrokerStatusOffline}
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), existing))
	_, err := registerEmbeddedFlatForTest(t, f.srv, tid("embedded-new-id"), "embedded-taken", f.target)
	assert.Equal(t, ErrCodeRuntimeBrokerNameConflict, embeddedRegCode(err))
	got, getErr := f.s.GetRuntimeBroker(context.Background(), existing.ID)
	require.NoError(t, getErr)
	assert.Nil(t, got.RuntimeTarget, "the existing row is not adopted")
}
