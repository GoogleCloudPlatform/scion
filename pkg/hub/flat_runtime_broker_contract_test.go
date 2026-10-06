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
	"sync"
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
	return f.pinnedAgentWith(t, slug, phase, nil)
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

// countingTokenGen counts agent credential mints.
type countingTokenGen struct {
	inner AgentTokenGenerator
	mints int
}

func (c *countingTokenGen) GenerateAgentToken(agentID, projectID string, ancestry []string, role AgentRole, scopes []AgentTokenScope) (string, error) {
	c.mints++
	return c.inner.GenerateAgentToken(agentID, projectID, ancestry, role, scopes)
}

func (c *countingTokenGen) GenerateAgentTokenForAgent(ctx context.Context, agent *store.Agent) (string, error) {
	c.mints++
	return c.inner.GenerateAgentTokenForAgent(ctx, agent)
}

// racingPinStore lands a competing first placement just before the handler's
// own SetAgentPinnedRuntimeTarget, so the handler loses the compare-and-set.
type racingPinStore struct {
	store.Store
	competing store.PinnedPlacement
	raced     bool
}

func (r *racingPinStore) SetAgentPinnedRuntimeTarget(ctx context.Context, agentID string, expected, next store.PinnedPlacement) (*store.Agent, error) {
	if !r.raced {
		r.raced = true
		if _, err := r.Store.SetAgentPinnedRuntimeTarget(ctx, agentID, expected, r.competing); err != nil {
			return nil, err
		}
	}
	return r.Store.SetAgentPinnedRuntimeTarget(ctx, agentID, expected, next)
}

// slogRecords captures records written through slog's default logger for the
// rest of the test.
type slogRecords struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *slogRecords) Enabled(context.Context, slog.Level) bool { return true }
func (h *slogRecords) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, r.Clone())
	return nil
}
func (h *slogRecords) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *slogRecords) WithGroup(string) slog.Handler      { return h }

func (h *slogRecords) messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.recs))
	for _, r := range h.recs {
		out = append(out, r.Message)
	}
	return out
}

func captureFlatSlog(t *testing.T) *slogRecords {
	t.Helper()
	h := &slogRecords{}
	prev := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return h
}

// noAgentWritten asserts the create left no trace: no agent row, no run
// intent and no dispatch.
func (f *flatHubFixture) noAgentWritten(t *testing.T, slug string) {
	t.Helper()
	assert.Nil(t, f.agentBySlug(t, slug), "no agent row may be written")
	assert.False(t, f.client.createCalled, "nothing may be dispatched")
	assert.False(t, f.client.startCalled, "nothing may be dispatched")
}

// flatOwnerUser is a project owner, admitted to project update and agent
// create. Dispatch admission on the non-auto-provide flat row comes only from
// canDispatchToBroker, which admits that row's creator; this user is not its
// creator.
func flatOwnerUser(t *testing.T, s store.Store, projectID, name string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(name), Email: name + "@example.com", DisplayName: name, Role: store.UserRoleMember, Status: "active", Created: time.Now()}
	require.NoError(t, s.CreateUser(context.Background(), u))
	createTestUserWithProjectRole(t, s, u.ID, u.Email, projectID, store.ProjectRoleOwner)
	return u
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
	reservationsBefore := brokerReservationCount(t, f.s, f.flat.ID)
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
	assert.Equal(t, reservationsBefore, brokerReservationCount(t, f.s, f.flat.ID), "no quota reservation")
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
		// With the capability present, the flat refusal is experiment_disabled.
		f.flat.Capabilities.EmptyPerAgentWorkspace = true
		require.NoError(t, f.s.UpdateRuntimeBroker(ctx, f.flat))
		rec = f.create(t, map[string]interface{}{"name": "p1", "runtimeBrokerId": f.flat.ID, "task": "t",
			"profile": "local", "expectedRuntimeTargetId": "other"})
		requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
		f.noAgentWritten(t, "p1")
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
		// "resume": true resumes a stopped agent in place (the lifecycle
		// branch of section 9 step 4).
		f.stalePinnedAgent(t, "stale-p", string(state.PhaseStopped))
		body := map[string]interface{}{"name": "stale-p", "task": "t", "resume": true, "profile": "local", "expectedRuntimeTargetId": "other"}
		requireAPIError(t, f.create(t, body), http.StatusConflict, ErrCodeRuntimeTargetPinStale)

		f.pinnedAgent(t, "pinned-p", string(state.PhaseStopped))
		body = map[string]interface{}{"name": "pinned-p", "task": "t", "resume": true, "profile": "local", "expectedRuntimeTargetId": "other"}
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

	// A non-local target type is denied by the same gate.
	k8s := &store.RuntimeBroker{ID: tid("flat-k8s-" + t.Name()), Name: "flat-k8s", Slug: "flat-k8s", Status: store.BrokerStatusOnline,
		Endpoint: "http://k8s.invalid", RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("flat-k8s-target-" + t.Name()), Type: "kubernetes"}}
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), k8s))
	f.srv.SetEmbeddedBrokerID(k8s.ID)
	allowed, pinnedProfile = f.srv.hubDefaultPassthroughAllowed(context.Background(), k8s.ID, f.project.ID, "agent", "")
	assert.False(t, allowed, "a kubernetes target is not a local container runtime")
	assert.Empty(t, pinnedProfile)
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
	// agent's Runtime Broker; create-on-existing with resume resumes it in
	// place on its own Runtime Broker.
	rec := f.create(t, map[string]interface{}{"name": "resume-me", "task": "t", "resume": true})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "resume must work with the experiment off: %d %s", rec.Code, rec.Body.String())
	require.True(t, f.client.startCalled)
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID, "dispatched to the agent's own Runtime Broker")
	assert.Equal(t, f.flat.RuntimeTarget.ID, startExtrasWireKey(f.client.lastStartExtras, "expectedRuntimeTargetId"),
		"the expected target is sent whatever the experiment state")

	// Stop it again and resume with a matching client-supplied target.
	got, err := f.s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	got.Phase = string(state.PhaseStopped)
	require.NoError(t, f.s.UpdateAgent(context.Background(), got))
	f.client.startCalled = false
	rec = f.create(t, map[string]interface{}{"name": "resume-me", "task": "t", "resume": true, "expectedRuntimeTargetId": f.flat.RuntimeTarget.ID})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "matching target accepted with the experiment off: %d %s", rec.Code, rec.Body.String())
	assert.True(t, f.client.startCalled)
	assert.Equal(t, f.flat.RuntimeTarget.ID, startExtrasWireKey(f.client.lastStartExtras, "expectedRuntimeTargetId"))
}

func TestFlatCreate_ExistingAgentChecksUseAgentBrokerNotResolved(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	f.pinnedAgent(t, "existing", string(state.PhaseStopped))
	// The request names the legacy row explicitly, but a resumed existing
	// agent is checked against its own Runtime Broker and pin: a mismatching
	// expected target is refused with the pin as the actual target (not the
	// resolved legacy row's empty one), before anything is dispatched.
	rec := f.create(t, map[string]interface{}{"name": "existing", "runtimeBrokerId": f.legacy.ID, "task": "t", "resume": true,
		"expectedRuntimeTargetId": "other"})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
	assert.Equal(t, f.flat.RuntimeTarget.ID, d["actualRuntimeTargetId"])
	assert.False(t, f.client.startCalled)

	// A matching expected target passes; the start goes to the agent's own
	// Runtime Broker with the pinned target.
	rec = f.create(t, map[string]interface{}{"name": "existing", "runtimeBrokerId": f.legacy.ID, "task": "t", "resume": true,
		"expectedRuntimeTargetId": f.flat.RuntimeTarget.ID})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "%d %s", rec.Code, rec.Body.String())
	assert.Equal(t, f.flat.ID, f.client.lastBrokerID)
	assert.Equal(t, f.flat.RuntimeTarget.ID, startExtrasWireKey(f.client.lastStartExtras, "expectedRuntimeTargetId"))
}

func TestFlatCreate_DeleteAndRecreateChecksBeforeDelete(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
	// A legacy agent still provisioning, re-created with gatherEnv: the
	// env-gather re-provisioning branch deletes and re-creates it. Re-creating
	// it on the flat row is a new create, refused (experiment off) before the
	// delete.
	old := f.unpinnedAgentOn(t, "recreate-me", f.legacy.ID, string(state.PhaseProvisioning))
	rec := f.create(t, map[string]interface{}{"name": "recreate-me", "runtimeBrokerId": f.flat.ID, "task": "t", "gatherEnv": true})
	requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	still, err := f.s.GetAgent(context.Background(), old.ID)
	require.NoError(t, err, "the existing agent row must not be deleted")
	assert.Equal(t, f.legacy.ID, still.RuntimeBrokerID)
	assert.Equal(t, string(state.PhaseProvisioning), still.Phase)
	assert.False(t, f.client.deleteCalled, "no delete is dispatched")
	assert.False(t, f.client.createCalled)
}

func TestFlatCreate_ExistingAgentWithoutBrokerTreatedAsNewCreate(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})

	// The new-create checks run: with the experiment off the first placement
	// is refused and nothing is pinned or dispatched.
	off := f.unpinnedAgentOn(t, "no-broker-off", "", string(state.PhaseCreated))
	setFlatExperiment(t, f.srv, false)
	rec := f.create(t, map[string]interface{}{"name": "no-broker-off", "runtimeBrokerId": f.flat.ID, "task": "t"})
	requireAPIError(t, rec, http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	gotOff, err := f.s.GetAgent(ctx, off.ID)
	require.NoError(t, err)
	assert.False(t, gotOff.IsPinned())
	assert.Equal(t, "", gotOff.RuntimeBrokerID)
	assert.False(t, f.client.startCalled)
	setFlatExperiment(t, f.srv, true)

	// Success: pinned with runtime_broker_id in one state_version-bumping
	// write before the start dispatch, which carries the expected target.
	a := f.unpinnedAgentOn(t, "no-broker", "", string(state.PhaseCreated))
	before := a.StateVersion
	rec = f.create(t, map[string]interface{}{"name": "no-broker", "runtimeBrokerId": f.flat.ID, "task": "t"})
	require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusCreated, "%d %s", rec.Code, rec.Body.String())
	got, err := f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, f.flat.ID, got.RuntimeBrokerID)
	assert.True(t, got.PinValid(), "pinned together with runtime_broker_id")
	assert.Greater(t, got.StateVersion, before)
	assert.Equal(t, f.flat.RuntimeTarget.ID, startExtrasWireKey(f.client.lastStartExtras, "expectedRuntimeTargetId"))
	assert.NotEqual(t, string(state.PhaseCreated), got.Phase, "the post-dispatch update landed without a version conflict")

	// A failed start leaves the first placement persisted.
	b := f.unpinnedAgentOn(t, "no-broker-fail", "", string(state.PhaseCreated))
	f.client.returnErr = &brokerStatusError{StatusCode: http.StatusInternalServerError, Body: `{"error":{"code":"runtime_error","message":"boom"}}`}
	_ = f.create(t, map[string]interface{}{"name": "no-broker-fail", "runtimeBrokerId": f.flat.ID, "task": "t"})
	gotB, err := f.s.GetAgent(ctx, b.ID)
	require.NoError(t, err)
	assert.True(t, gotB.PinValid(), "the first placement stays persisted when the start fails")
	f.client.returnErr = nil

	// A concurrent placement lands between the handler's read and its
	// compare-and-set: the handler answers 409 conflict before any dispatch.
	other := &store.RuntimeBroker{ID: tid("racing-flat-" + t.Name()), Name: "racing-flat", Slug: "racing-flat", Status: store.BrokerStatusOnline,
		Endpoint: "http://racing.invalid", RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("racing-target-" + t.Name()), Type: "docker"}}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, other))
	c := f.unpinnedAgentOn(t, "no-broker-race", "", string(state.PhaseCreated))
	f.srv.store = &racingPinStore{Store: f.s, competing: store.PinnedPlacement{RuntimeBrokerID: other.ID, RuntimeTargetID: other.RuntimeTarget.ID, RuntimeTargetType: "docker"}}
	f.client.startCalled = false
	rec = f.create(t, map[string]interface{}{"name": "no-broker-race", "runtimeBrokerId": f.flat.ID, "task": "t"})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeConflict)
	assert.False(t, f.client.startCalled, "nothing is dispatched after a lost placement")
	gotC, err := f.s.GetAgent(ctx, c.ID)
	require.NoError(t, err)
	assert.Equal(t, other.ID, gotC.RuntimeBrokerID, "the concurrent placement stands")
}

func TestFlatCreate_LifecycleClientExpectedTargetComparedWithPin(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	f.pinnedAgent(t, "pinned-life", string(state.PhaseStopped))
	rec := f.create(t, map[string]interface{}{"name": "pinned-life", "task": "t", "resume": true, "expectedRuntimeTargetId": "other"})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.Equal(t, f.flat.RuntimeTarget.ID, d["actualRuntimeTargetId"])

	// An unpinned agent on a legacy row mismatches with an empty actual.
	f.unpinnedAgentOn(t, "legacy-life", f.legacy.ID, string(state.PhaseStopped))
	rec = f.create(t, map[string]interface{}{"name": "legacy-life", "task": "t", "resume": true, "expectedRuntimeTargetId": "other"})
	d = requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMismatch)
	assert.Equal(t, "", d["actualRuntimeTargetId"])
	assert.False(t, f.client.startCalled)
}

func TestFlatCreate_RuntimeBrokerRejectionRelayed(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	cases := []struct {
		status  int
		code    string
		details string
		keys    []string
	}{
		{http.StatusConflict, ErrCodeRuntimeTargetMismatch, `"expectedRuntimeTargetId":"x","actualRuntimeTargetId":"y"`,
			[]string{"runtimeBrokerId", "expectedRuntimeTargetId", "actualRuntimeTargetId"}},
		{http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported, `"profile":"local"`,
			[]string{"runtimeBrokerId", "profile"}},
		{http.StatusPreconditionFailed, ErrCodeRuntimeTargetRequired, ``,
			[]string{"runtimeBrokerId"}},
	}
	for _, c := range cases {
		t.Run(c.code, func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
			extra := ""
			if c.details != "" {
				extra = "," + c.details
			}
			// The Runtime Broker envelope carries start markers, which the
			// Hub public envelope strips.
			f.client.returnErr = &brokerStatusError{StatusCode: c.status, Body: fmt.Sprintf(
				`{"error":{"code":%q,"message":"refused","details":{"runtimeBrokerId":%q%s,"startAttempted":true,"runId":"run-1"}}}`, c.code, f.flat.ID, extra)}
			rec := f.create(t, map[string]interface{}{"name": "relayed", "runtimeBrokerId": f.flat.ID, "task": "t"})
			d := requireAPIError(t, rec, c.status, c.code)
			for _, k := range c.keys {
				assert.Contains(t, d, k, "frozen details key %q", k)
			}
			assert.Equal(t, f.flat.ID, d["runtimeBrokerId"])
			requireNoStartMarkers(t, d)
			assert.Nil(t, f.agentBySlug(t, "relayed"), "the create is rolled back")
		})
	}
}

func TestFlatCreate_AccessCheckBeforeLink(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
	// The caller is a project owner admitted to project update and agent
	// create; dispatch admission on the non-auto-provide flat row comes only
	// from canDispatchToBroker. Dispatch authorization runs before any link:
	// today's authorization response, no provider row and no project default
	// written. This deliberately overlaps
	// TestFlatCreate_UnlinkedFlatRowAuthorizationWins (both are section 15
	// names): this test pins the no-write side effects, that one the
	// precedence over runtime_broker_not_linked with an admitted control.
	owner := flatOwnerUser(t, f.s, f.project.ID, "flat-access-owner")
	before := f.providerIDs(t, f.project.ID)
	rec := doRequestAsUser(t, f.srv, owner, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents",
		map[string]interface{}{"name": "access", "runtimeBrokerId": f.flat.ID, "task": "t"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.ElementsMatch(t, before, f.providerIDs(t, f.project.ID), "no provider row is written before dispatch authorization")
	assert.Equal(t, f.legacy.ID, f.projectDefault(t, f.project.ID))
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
	owner := flatOwnerUser(t, f.s, f.project.ID, "flat-unlinked-owner")
	rec := doRequestAsUser(t, f.srv, owner, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents",
		map[string]interface{}{"name": "authz-wins", "runtimeBrokerId": f.flat.ID, "task": "t"})
	assert.Equal(t, http.StatusForbidden, rec.Code, "authorization wins over runtime_broker_not_linked: %s", rec.Body.String())
	assert.NotEqual(t, ErrCodeRuntimeBrokerNotLinked, decodeFlatAPIError(t, rec).Code)
	assert.NotContains(t, f.providerIDs(t, f.project.ID), f.flat.ID, "no provider row")
	f.noAgentWritten(t, "authz-wins")

	// Positive control: an admitted caller gets runtime_broker_not_linked.
	rec = f.create(t, map[string]interface{}{"name": "authz-wins", "runtimeBrokerId": f.flat.ID, "task": "t"})
	requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodeRuntimeBrokerNotLinked)
	assert.NotContains(t, f.providerIDs(t, f.project.ID), f.flat.ID)
}

func TestFlatCreate_UnlinkedFlatRowNotReachedThroughDefaults(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	t.Run("project default pointing at an unlinked flat row", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
		// Defaults select only among the project's linked providers, so an
		// unlinked flat project default answers today's 422
		// no_runtime_broker; it never becomes runtime_broker_not_linked.
		f.project.DefaultRuntimeBrokerID = f.flat.ID
		require.NoError(t, f.s.UpdateProject(ctx, f.project))
		rec := f.create(t, map[string]interface{}{"name": "defaults", "task": "t"})
		requireAPIError(t, rec, http.StatusUnprocessableEntity, ErrCodeNoRuntimeBroker)
		assert.NotContains(t, f.providerIDs(t, f.project.ID), f.flat.ID, "no link is written")
		f.noAgentWritten(t, "defaults")
	})
	t.Run("hub default pointing at an unlinked flat row falls through", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true})
		f.project.DefaultRuntimeBrokerID = ""
		require.NoError(t, f.s.UpdateProject(ctx, f.project))
		f.srv.mu.Lock()
		f.srv.config.AgentDefaults.DefaultRuntimeBroker = f.flat.ID
		f.srv.mu.Unlock()
		rec := f.create(t, map[string]interface{}{"name": "hub-default", "task": "t"})
		require.Equal(t, http.StatusCreated, rec.Code, "the hub default falls through to the single linked provider: %s", rec.Body.String())
		a := f.agentBySlug(t, "hub-default")
		require.NotNil(t, a)
		assert.Equal(t, f.legacy.ID, a.RuntimeBrokerID)
		assert.False(t, a.IsPinned())
		assert.NotContains(t, f.providerIDs(t, f.project.ID), f.flat.ID, "no link is written")
	})
}

func TestFlatCreate_AuthorizationBeforeFlatChecks(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	// Fixture: a linked flat row, experiment off, with a profile and a
	// mismatching expected target: every flat check would fail.
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: false, linkFlat: true})
	body := map[string]interface{}{"name": "authz-first", "runtimeBrokerId": f.flat.ID, "task": "t",
		"profile": "local", "expectedRuntimeTargetId": "other"}
	member := flatMemberUser(t, f.s, f.project.ID, "flat-authz-member")
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	code := decodeFlatAPIError(t, rec).Code
	for _, flatCode := range []string{ErrCodeExperimentDisabled, ErrCodeRuntimeTargetMismatch, ErrCodeRuntimeProfileUnsupported} {
		assert.NotEqual(t, flatCode, code, "no flat code is reported when authorization fails")
	}
	f.noAgentWritten(t, "authz-first")

	// Positive control: the same request from an admitted caller gets the
	// first flat code.
	requireAPIError(t, f.create(t, body), http.StatusPreconditionFailed, ErrCodeExperimentDisabled)
	f.noAgentWritten(t, "authz-first")
}

func TestFlatCreate_FlatChecksDoNotGrantDispatch(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	member := flatMemberUser(t, f.s, f.project.ID, "flat-nogrant-member")
	// Passes every flat check (experiment on, matching target, no profile).
	body := map[string]interface{}{"name": "no-grant", "runtimeBrokerId": f.flat.ID, "task": "t",
		"expectedRuntimeTargetId": f.flat.RuntimeTarget.ID}
	rec := doRequestAsUser(t, f.srv, member, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents", body)
	assert.Equal(t, http.StatusForbidden, rec.Code, "canDispatchToBroker still decides: %s", rec.Body.String())
	f.noAgentWritten(t, "no-grant")

	// Positive control: an admitted caller's identical request is created
	// and pinned.
	rec = f.create(t, body)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	a := f.agentBySlug(t, "no-grant")
	require.NotNil(t, a)
	assert.True(t, a.PinValid())
	assert.Equal(t, f.flat.RuntimeTarget.ID, a.PinnedRuntimeTargetID)
}

func TestLegacyCreate_AgentCallerCreateTimeLinkUnchanged(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := brokerLinkAuthzSetup(t)
	f.unlinked.AutoProvide = false
	require.NoError(t, f.store.UpdateRuntimeBroker(context.Background(), f.unlinked))
	setFlatExperiment(t, f.srv, true)

	// An agent caller with agent-create scope names an explicit, unlinked
	// legacy row: the resolver's project-update CheckAccess denies it, so it
	// gets today's 403 and no provider link is written (unchanged).
	rec := f.asAgent(t, http.MethodPost, "/api/v1/projects/"+f.proj.ID+"/agents",
		CreateAgentRequest{Name: "agent-linked", RuntimeBrokerID: f.unlinked.ID}, ScopeAgentCreate)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.NotEqual(t, ErrCodeRuntimeBrokerNotLinked, decodeAPIErrorIfAny(rec))
	providers, err := f.store.GetProjectProviders(context.Background(), f.proj.ID)
	require.NoError(t, err)
	for _, p := range providers {
		assert.NotEqual(t, f.unlinked.ID, p.BrokerID, "no provider link may be written for the agent caller")
	}

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
	d := store.BrokerDispatch{ID: tid("dispatch-" + t.Name()), BrokerID: a.RuntimeBrokerID, AgentID: a.ID, Op: "start", Args: args}
	_, execErr := f.srv.executeDispatch(context.Background(), d)
	require.Error(t, execErr, "the dispatcher backstop refuses")
	assert.False(t, f.client.startCalled)

	// The executing node carries the typed refusal in the dispatch failure
	// envelope (what reconcileBroker records with FailBrokerDispatch), and
	// the requesting node rebuilds it from that envelope.
	envelope := dispatchFailureResult(execErr)
	require.NotEmpty(t, envelope, "the refusal travels in the dispatch failure envelope")
	rebuilt := dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: envelope, Error: execErr.Error()})
	var refusal *RuntimeTargetRefusal
	require.True(t, errors.As(rebuilt, &refusal), "a typed refusal rebuilt from the envelope, got %v", rebuilt)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, refusal.Code)
	assert.Equal(t, http.StatusConflict, refusal.Status)
	assert.Equal(t, a.ID, refusal.Details["agentId"])
	assert.Equal(t, f.flat.ID, refusal.Details["pinnedRuntimeBrokerId"])
	assert.Equal(t, f.legacy.ID, refusal.Details["runtimeBrokerId"])
}

func TestFlatStart_StalePinRefused_WakeDM(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-wake", string(state.PhaseSuspended))
	_, dmErr := f.srv.wakeAgentForDM(context.Background(), a)
	require.NotNil(t, dmErr)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, dmErr.Code)
	requireStalePinDetails(t, dmErr.Details, a, f)
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
	// A dispatcher that mints agent credentials through the Hub, counted.
	minter := &countingTokenGen{inner: f.srv}
	disp := NewHTTPAgentDispatcherWithClient(f.s, f.client, false, slog.Default())
	disp.SetTokenGenerator(minter)
	err := disp.DispatchAgentStart(context.Background(), a, "t", false)
	require.Error(t, err)
	assert.True(t, isConfirmedStartNotActedOnError(err))
	var typed *RuntimeTargetRefusal
	require.True(t, errors.As(err, &typed))
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, typed.Code)
	assert.Equal(t, 0, minter.mints, "no agent credential is minted")
	assert.False(t, f.client.startCalled, "nothing is sent")
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

	// Executor leg: a queued start reaches the refusal only in the executing
	// node's dispatcher backstop. The row ends failed with the typed refusal
	// in its envelope, nothing is dispatched, and the row is never
	// re-executed. (Section 7: the executor is not a retry loop and does not
	// settle the intent itself.)
	t.Run("executor", func(t *testing.T) {
		ctx := context.Background()
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		a := f.stalePinnedAgent(t, "terminal", string(state.PhaseStopped))
		_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
		require.NoError(t, err)
		args, err := MarshalDispatchArgs(&StartDispatchArgs{Task: "t"})
		require.NoError(t, err)
		row := &store.BrokerDispatch{ID: tid("dispatch-terminal-" + t.Name()), BrokerID: a.RuntimeBrokerID, AgentID: a.ID, Op: "start", Args: args}
		require.NoError(t, f.s.InsertBrokerDispatch(ctx, row))

		f.srv.ReconcileBroker(ctx, a.RuntimeBrokerID)
		failed, err := f.s.GetBrokerDispatch(ctx, row.ID)
		require.NoError(t, err)
		require.Equal(t, store.DispatchStateFailed, failed.State, "the refused start is recorded as failed")
		var refusal *RuntimeTargetRefusal
		require.True(t, errors.As(dispatchFailureError(failed), &refusal), "the failure envelope carries the typed refusal")
		assert.Equal(t, ErrCodeRuntimeTargetPinStale, refusal.Code)
		require.NotEmpty(t, refusal.Message)
		assert.False(t, f.client.startCalled, "nothing is dispatched")

		attempts := failed.Attempts
		f.srv.ReconcileBroker(ctx, a.RuntimeBrokerID)
		again, err := f.s.GetBrokerDispatch(ctx, row.ID)
		require.NoError(t, err)
		assert.Equal(t, store.DispatchStateFailed, again.State)
		assert.Equal(t, attempts, again.Attempts, "the same intent is never re-dispatched")
		assert.False(t, f.client.startCalled)
	})

	// Requester leg: the requesting node's handler recorded the intent and
	// deferred the start to the node holding the broker's control channel.
	// The owner's backstop refuses (state changed after the requester's own
	// backstop passed); the requester rebuilds the typed refusal from the
	// row's envelope and settles the intent as a definite start failure,
	// setting the agent message to the refusal message (section 7).
	t.Run("requester", func(t *testing.T) {
		ctx := context.Background()
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
		a := f.pinnedAgent(t, "terminal-requester", string(state.PhaseStopped))
		refusal := &RuntimeTargetRefusal{
			Code:    ErrCodeRuntimeTargetPinStale,
			Status:  http.StatusConflict,
			Message: "agent placement is stale (owner-side refusal)",
			Details: map[string]interface{}{"agentId": a.ID, "pinnedRuntimeBrokerId": a.PinnedRuntimeBrokerID, "runtimeBrokerId": a.RuntimeBrokerID},
		}

		events := NewChannelEventPublisher()
		t.Cleanup(events.Close)
		ownerDisp := &ownerErrDispatcher{err: refusal}
		owner := &Server{store: f.s, instanceID: "hub-owner-" + t.Name(), agentLifecycleLog: slog.Default(), events: events}
		owner.SetDispatcher(ownerDisp)
		owner.execDispatch = owner.executeDispatch
		owner.deliverMsg = owner.deliverMessage

		requester := NewHTTPAgentDispatcherWithClient(f.s, &deferredTestClient{localBroker: "some-other-broker"}, false, slog.Default())
		requester.SetCrossNodeDeps(events, ownerSignalBus{owner: owner})
		f.srv.SetDispatcher(requester)

		reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil).WithContext(reqCtx)
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		rec := httptest.NewRecorder()
		f.srv.Handler().ServeHTTP(rec, req)

		d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
		assert.Equal(t, a.ID, d["agentId"])
		got, err := f.s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, refusal.Message, got.Message, "the requesting node settles the intent with the refusal message")
	})
}

func TestFlatStart_CrossNodeRefusalRebuiltFromEnvelope(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	refusal := &RuntimeTargetRefusal{
		Code: ErrCodeRuntimeTargetPinStale, Status: http.StatusConflict, Message: "stale",
		Details: map[string]interface{}{"agentId": "a", "pinnedRuntimeBrokerId": "p", "runtimeBrokerId": "r"},
	}
	// Encode on the executing node.
	result := dispatchFailureResult(fmt.Errorf("start: %w", refusal))
	require.NotEmpty(t, result, "the executing node carries the typed refusal in the envelope")
	// Decode on the requesting node.
	rebuilt := dispatchFailureError(&store.BrokerDispatch{Op: "start", Result: result, Error: "start: " + refusal.Error()})
	var got *RuntimeTargetRefusal
	require.True(t, errors.As(rebuilt, &got), "rebuilt as the typed refusal, got %v", rebuilt)
	assert.Equal(t, refusal.Code, got.Code)
	assert.Equal(t, refusal.Status, got.Status)
	assert.Equal(t, refusal.Message, got.Message)
	assert.Equal(t, refusal.Details, got.Details)
	assert.True(t, isConfirmedStartNotActedOnError(rebuilt), "the rebuilt refusal is confirmed not-acted-on")
}

// --- reincarnate / finalize-env ----------------------------------------------

func TestFlatReincarnate_StalePinRefusedBeforeStop(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgentWith(t, "stale-reinc", string(state.PhaseRunning), reincarnationEligible)
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
	a := f.pinnedAgentWith(t, "move-dry", string(state.PhaseRunning), reincarnationEligible)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, TargetBroker: f.legacy.ID})
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported)
	verdictJSON, err := json.Marshal(d["verdict"])
	require.NoError(t, err)
	var verdict MoveVerdict
	require.NoError(t, json.Unmarshal(verdictJSON, &verdict))
	require.NotEmpty(t, verdict.Checks)
	assert.Equal(t, "runtime_target", verdict.Checks[0].Name, "runtime_target is the first check")
	assert.Equal(t, MoveCheckFailed, verdict.Checks[0].Result)
	for _, c := range verdict.Checks[1:] {
		assert.Equal(t, MoveCheckNotEvaluated, c.Result, "check %s", c.Name)
	}

	// Moving a legacy agent onto a flat Runtime Broker is ineligible too.
	l := f.unpinnedAgentOnWith(t, "move-onto-flat", f.legacy.ID, string(state.PhaseRunning), reincarnationEligible)
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+l.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, TargetBroker: f.flat.ID})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported)
}

func TestFlatReincarnate_DryRunMoveOfStalePinGetsPlanAnswer(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgentWith(t, "stale-move-dry", string(state.PhaseRunning), reincarnationEligible)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{DryRun: true, TargetBroker: f.flat.ID})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetMoveUnsupported)
	assert.NotEqual(t, ErrCodeRuntimeTargetPinStale, decodeFlatAPIError(t, rec).Code)
}

func TestFlatReincarnate_ProfileNotRederived(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	ctx := context.Background()
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	setProjectAnnotations(t, f.s, f.project, map[string]string{projectSettingActiveProfile: "local"})

	// reincarnateInPlace runs a real in-place reincarnation and waits for
	// the worker to finish; it returns the stored agent and the reprovision
	// request.
	reincarnateInPlace := func(t *testing.T, a *store.Agent) (*store.Agent, *RemoteCreateAgentRequest) {
		t.Helper()
		f.client.createCalled = false
		f.client.lastCreateReq = nil
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/reincarnate", ReincarnateAgentRequest{})
		require.Truef(t, rec.Code == http.StatusOK || rec.Code == http.StatusAccepted, "reincarnate: %d %s", rec.Code, rec.Body.String())
		var got *store.Agent
		require.Eventually(t, func() bool {
			cur, err := f.s.GetAgent(ctx, a.ID)
			if err != nil {
				return false
			}
			got = cur
			return cur.Generation > a.Generation && cur.ReincarnationState == store.ReincarnationStateNone
		}, 10*time.Second, 20*time.Millisecond, "the reincarnation completes")
		require.True(t, f.client.createCalled, "the worker reprovisions")
		return got, f.client.lastCreateReq
	}

	// Positive control: a legacy agent re-derives the project active profile.
	legacy := f.unpinnedAgentOnWith(t, "reinc-legacy", f.legacy.ID, string(state.PhaseStopped), reincarnationEligible)
	gotLegacy, reqLegacy := reincarnateInPlace(t, legacy)
	require.NotNil(t, gotLegacy.AppliedConfig)
	assert.Equal(t, "local", gotLegacy.AppliedConfig.Profile, "control: a legacy agent re-derives the project active profile")
	require.NotNil(t, reqLegacy)
	require.NotNil(t, reqLegacy.Config)
	assert.Equal(t, "local", reqLegacy.Config.Profile)

	// A pinned agent does not.
	a := f.pinnedAgentWith(t, "reinc-profile", string(state.PhaseStopped), reincarnationEligible)
	got, req := reincarnateInPlace(t, a)
	require.NotNil(t, got.AppliedConfig)
	assert.Empty(t, got.AppliedConfig.Profile, "the project active profile is not re-derived for a pinned agent")
	require.NotNil(t, req)
	require.NotNil(t, req.Config)
	assert.Empty(t, req.Config.Profile, "the reprovision request carries no profile")
	assert.True(t, got.PinValid(), "the pin is kept")
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
	if a.AppliedConfig.CreateInputs != nil {
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile, "no profile recorded in the create inputs")
	}
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
	createProject := func(t *testing.T, f *flatHubFixture, name string) string {
		t.Helper()
		rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/projects", map[string]interface{}{"name": name})
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
		return pid
	}
	for _, on := range []bool{true, false} {
		t.Run(fmt.Sprintf("experiment=%v", on), func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: on})
			// Positive control: an auto-provide legacy row is linked to a new
			// project, as today.
			f.legacy.AutoProvide = true
			require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), f.legacy))
			f.flat.AutoProvide = true
			require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), f.flat))
			f.srv.SetEmbeddedBrokerID(f.flat.ID) // the embedded flat instance gets no link either
			pid := createProject(t, f, "auto-provide-target")
			assert.Contains(t, f.providerIDs(t, pid), f.legacy.ID, "control: auto-provide still links a legacy row")
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
	var resp RegisterProjectResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Project)
	assert.Contains(t, f.providerIDs(t, resp.Project.ID), f.legacy.ID, "the legacy link row exists")
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
	logs := captureFlatSlog(t)
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
	// The heartbeat handler drops DefaultProfile/ProfileAttach for a flat
	// row before writing (section 6), so the store's own last-resort strip
	// never has a profile to drop.
	assert.NotContains(t, logs.messages(), store.FlatRuntimeBrokerProfilesDroppedMessage,
		"the heartbeat handler drops the profile before the store write")
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
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, id, resp.BrokerID, "matched by ID, never by name")
	require.NotNil(t, resp.RuntimeTarget, "the stored binding is acknowledged")
	assert.Equal(t, f.target.ID, resp.RuntimeTarget.ID)
}

func TestFlatRegistration_NameChangeInConfigNotApplied(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := f.registerFlat(t, tid("flat-reg-rename"), "flat-original")
	rec := f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-renamed", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, id, resp.BrokerID)
	require.NotNil(t, resp.RuntimeTarget, "the stored binding is acknowledged")
	assert.Equal(t, f.target.ID, resp.RuntimeTarget.ID)
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

	// Positive control: the owner's identical flat re-registration passes
	// the same gate and is acknowledged with the stored binding.
	rec = f.register(t, f.operator, CreateBrokerRegistrationRequest{BrokerID: id, Name: "flat-owner", RuntimeTarget: f.target})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateBrokerRegistrationResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, id, resp.BrokerID)
	require.NotNil(t, resp.RuntimeTarget, "the stored binding is acknowledged")
	assert.Equal(t, f.target.ID, resp.RuntimeTarget.ID)
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

// embeddedRegCode extracts a Hub error code from an embedded registration
// error (a *RuntimeTargetRefusal, or an error whose message carries the code,
// such as a binding-conflict acknowledgement error).
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

// TestFlatRegistration_EmbeddedPathUsesSharedRules is F-arrange: its act
// step goes through registerEmbeddedFlatForTest, whose body P1.2 replaces.
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

// TestFlatRegistration_EmbeddedSideDuties is F-arrange: its act step goes
// through registerEmbeddedFlatForTest, whose body P1.2 replaces. R7 places
// reporting on the embedded flat path itself, so after a refusal the helper
// must leave the Hub exactly as the startup path leaves it: the refusal
// reported through EmbeddedBrokerRegistrationFailed (asserted here on the Hub
// side), not embedded, and no legacy fallback.
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

	// No legacy fallback: a fresh server that refuses on first boot
	// (experiment off) is not embedded and creates no legacy row.
	fresh := newFlatRegFixture(t, false)
	freshID := tid("embedded-refused-first-boot")
	_, err = registerEmbeddedFlatForTest(t, fresh.srv, freshID, "embedded-refused", fresh.target)
	require.Error(t, err)
	assert.Equal(t, ErrCodeExperimentDisabled, embeddedRegCode(err))
	assert.False(t, fresh.srv.isEmbeddedBroker(freshID), "not embedded after a refusal")
	requireEmbeddedRegistrationFailedReported(t, fresh.srv, ErrCodeExperimentDisabled)
	_, err = fresh.s.GetLegacyRuntimeBrokerByName(ctx, "embedded-refused")
	assert.ErrorIs(t, err, store.ErrNotFound, "no legacy row as a fallback")
	_, err = fresh.s.GetRuntimeBroker(ctx, freshID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no row for the refused identity")
}

// TestFlatRegistration_EmbeddedBoundResultRequired is F-arrange: its act
// step goes through registerEmbeddedFlatForTest, whose body P1.2 replaces.
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
	assert.True(t, f.srv.isEmbeddedBroker(id), "activated only on a bound result")
}

// TestFlatRegistration_EmbeddedConflictingRowNotActivated is F-arrange: its
// act step goes through registerEmbeddedFlatForTest, whose body P1.2
// replaces.
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
	requireEmbeddedRegistrationFailedReported(t, f.srv, code)
	got, getErr := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, getErr)
	assert.Equal(t, tid("stored-other"), got.RuntimeTarget.ID, "the stored target is unchanged")
}

// TestFlatRegistration_EmbeddedLegacyRowNotActivated is F-arrange: its act
// step goes through registerEmbeddedFlatForTest, whose body P1.2 replaces.
func TestFlatRegistration_EmbeddedLegacyRowNotActivated(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	id := tid("embedded-legacy")
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: id, Name: "embedded-legacy", Slug: "embedded-legacy", Status: store.BrokerStatusOffline}))
	_, err := registerEmbeddedFlatForTest(t, f.srv, id, "embedded-legacy", f.target)
	assert.Equal(t, ErrCodeRuntimeBrokerNotFlat, embeddedRegCode(err))
	assert.False(t, f.srv.isEmbeddedBroker(id))
	requireEmbeddedRegistrationFailedReported(t, f.srv, ErrCodeRuntimeBrokerNotFlat)
	got, getErr := f.s.GetRuntimeBroker(context.Background(), id)
	require.NoError(t, getErr, "no automatic cleanup")
	assert.Nil(t, got.RuntimeTarget)
}

// TestFlatRegistration_EmbeddedNameCollisionNotAdopted is F-arrange: its act
// step goes through registerEmbeddedFlatForTest, whose body P1.2 replaces.
func TestFlatRegistration_EmbeddedNameCollisionNotAdopted(t *testing.T) {
	t.Skip(pendingFlatDispatch)
	f := newFlatRegFixture(t, true)
	existing := &store.RuntimeBroker{ID: tid("embedded-taken"), Name: "embedded-taken", Slug: "embedded-taken", Status: store.BrokerStatusOffline}
	require.NoError(t, f.s.CreateRuntimeBroker(context.Background(), existing))
	newID := tid("embedded-new-id")
	_, err := registerEmbeddedFlatForTest(t, f.srv, newID, "embedded-taken", f.target)
	assert.Equal(t, ErrCodeRuntimeBrokerNameConflict, embeddedRegCode(err))
	got, getErr := f.s.GetRuntimeBroker(context.Background(), existing.ID)
	require.NoError(t, getErr)
	assert.Nil(t, got.RuntimeTarget, "the existing row is not adopted")
	_, getErr = f.s.GetRuntimeBroker(context.Background(), newID)
	assert.ErrorIs(t, getErr, store.ErrNotFound, "no row with the new ID")
	assert.False(t, f.srv.isEmbeddedBroker(newID), "not activated")
	assert.False(t, f.srv.isEmbeddedBroker(existing.ID))
	requireEmbeddedRegistrationFailedReported(t, f.srv, ErrCodeRuntimeBrokerNameConflict)
}

// requireEmbeddedRegistrationFailedReported asserts the refusal was reported
// through EmbeddedBrokerRegistrationFailed (appendix R7/R10: the embedded flat
// path reports every refusal there, never falling back to the legacy
// identity).
func requireEmbeddedRegistrationFailedReported(t *testing.T, srv *Server, code string) {
	t.Helper()
	srv.mu.Lock()
	regErr := srv.embeddedBrokerRegErr
	srv.mu.Unlock()
	require.NotEmpty(t, regErr, "the refusal must be reported through EmbeddedBrokerRegistrationFailed")
	assert.Contains(t, regErr, code)
}
