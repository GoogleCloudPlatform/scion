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

// Tests of the saved-placement guard (requirePinnedPlacement) on every
// broker-reaching operation of an existing agent: the dispatcher backstops,
// the handler pre-checks, and the non-dispatcher routes (PTY, conduit
// grants, workspace tunnel, keys).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- helpers -----------------------------------------------------------------

// errCountingClient is what countingBrokerClient answers to every call.
var errCountingClient = errors.New("counting broker client: call recorded")

// countingBrokerClient records every Runtime Broker call, keys included,
// and answers each one with errCountingClient.
type countingBrokerClient struct {
	mu    sync.Mutex
	calls []string
}

func (c *countingBrokerClient) record(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, name)
}

func (c *countingBrokerClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *countingBrokerClient) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = nil
}

func (c *countingBrokerClient) CreateAgent(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	c.record("CreateAgent")
	return nil, errCountingClient
}

func (c *countingBrokerClient) StartAgent(context.Context, string, string, string, string, string, string, string, string, string, string, map[string]string, []ResolvedSecret, *api.ScionConfig, []api.SharedDir, bool, bool, StartExtras) (*RemoteAgentResponse, error) {
	c.record("StartAgent")
	return nil, errCountingClient
}

func (c *countingBrokerClient) StopAgent(context.Context, string, string, string, string, string) error {
	c.record("StopAgent")
	return errCountingClient
}

func (c *countingBrokerClient) RestartAgent(context.Context, string, string, string, string, map[string]string, StartExtras) (*RemoteAgentResponse, error) {
	c.record("RestartAgent")
	return nil, errCountingClient
}

func (c *countingBrokerClient) ResetAuthAgent(context.Context, string, string, string, string, string, string) error {
	c.record("ResetAuthAgent")
	return errCountingClient
}

func (c *countingBrokerClient) DeleteAgent(context.Context, string, string, string, string, DeleteAgentOptions) error {
	c.record("DeleteAgent")
	return errCountingClient
}

func (c *countingBrokerClient) MessageAgent(context.Context, string, string, string, string, string, bool, *messages.StructuredMessage) error {
	c.record("MessageAgent")
	return errCountingClient
}

func (c *countingBrokerClient) CheckAgentPrompt(context.Context, string, string, string, string) (bool, error) {
	c.record("CheckAgentPrompt")
	return false, errCountingClient
}

func (c *countingBrokerClient) CreateAgentWithGather(context.Context, string, string, *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	c.record("CreateAgentWithGather")
	return nil, nil, errCountingClient
}

func (c *countingBrokerClient) GetAgentLogs(context.Context, string, string, string, string, int) (string, error) {
	c.record("GetAgentLogs")
	return "", errCountingClient
}

func (c *countingBrokerClient) ExecAgent(context.Context, string, string, string, string, []string, int) (string, int, error) {
	c.record("ExecAgent")
	return "", 0, errCountingClient
}

func (c *countingBrokerClient) CleanupProject(context.Context, string, string, string, string) error {
	c.record("CleanupProject")
	return errCountingClient
}

func (c *countingBrokerClient) ExecuteKeys(context.Context, string, string, string, agentkeys.BrokerRequest) (agentkeys.BrokerResult, error) {
	c.record("ExecuteKeys")
	return agentkeys.BrokerResult{}, errCountingClient
}

var (
	_ RuntimeBrokerClient    = (*countingBrokerClient)(nil)
	_ agentkeys.BrokerClient = (*countingBrokerClient)(nil)
)

// agentOverrideStore answers GetAgent for one agent ID with a copy of
// agent, so the dispatcher (and keys' row load) sees a placement state that
// the store would not accept as a row.
type agentOverrideStore struct {
	store.Store
	mu    sync.Mutex
	agent *store.Agent
}

func (s *agentOverrideStore) set(a *store.Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agent = a
}

func (s *agentOverrideStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	s.mu.Lock()
	a := s.agent
	s.mu.Unlock()
	if a != nil && a.ID == id {
		c := *a
		return &c, nil
	}
	return s.Store.GetAgent(ctx, id)
}

// brokerReadErrStore fails GetRuntimeBroker with err: always when fault is
// nil, otherwise only while fault is armed (installed through the store
// fault switch, so the server's store is never reassigned).
type brokerReadErrStore struct {
	store.Store
	err   error
	fault *storeFaultSwitch
}

func (s *brokerReadErrStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if s.fault.Active() {
		return nil, s.err
	}
	return s.Store.GetRuntimeBroker(ctx, id)
}

// brokerReadFaultWrap is a flatHubOpts.storeWrap (and installStoreFault
// wrapper) that fails every Runtime Broker row read with err once armed.
func brokerReadFaultWrap(err error) func(inner store.Store, fault *storeFaultSwitch) store.Store {
	return func(inner store.Store, fault *storeFaultSwitch) store.Store {
		return &brokerReadErrStore{Store: inner, err: err, fault: fault}
	}
}

// requirePlacementRefusal asserts err is a placement refusal raised on this
// node with code.
func requirePlacementRefusal(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var pc *PlacementConflictError
	require.True(t, errors.As(err, &pc), "want a *PlacementConflictError, got %v", err)
	assert.Equal(t, code, pc.Refusal.Code, "err: %v", err)
	assert.Equal(t, http.StatusConflict, pc.Refusal.Status)
	r, ok := placementRefusalOf(err)
	require.True(t, ok)
	assert.Equal(t, code, r.Code)
}

// pendingDispatchCount is the number of pending broker_dispatch rows of
// the given Runtime Brokers.
func pendingDispatchCount(t *testing.T, s store.Store, brokerIDs ...string) int {
	t.Helper()
	n := 0
	for _, id := range brokerIDs {
		rows, err := s.ListPendingDispatch(context.Background(), id)
		require.NoError(t, err)
		n += len(rows)
	}
	return n
}

// --- requirePinnedPlacement --------------------------------------------------

// placementState is one of the agent states of design §3.1, built in
// memory over the guard fixture's Runtime Broker rows.
type placementState struct {
	name  string
	build func(base store.Agent) *store.Agent
	// want is the guard's answer: a placement code, errNoRuntimeBrokerAssigned
	// (noBroker), or a pass.
	code     string
	noBroker bool
}

// placementStates returns the agent states of design §3.1 over flat (a
// flat Runtime Broker row), legacy (a legacy row) and missing (an ID with
// no row).
func placementStates(flat, legacy *store.RuntimeBroker, missing string) []placementState {
	pin := func(a *store.Agent, brokerID, targetID, targetType string) {
		a.PinnedRuntimeBrokerID = brokerID
		a.PinnedRuntimeTargetID = targetID
		a.PinnedRuntimeTargetType = targetType
	}
	return []placementState{
		{name: "unpinned/no Runtime Broker", noBroker: true, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = ""
			return &a
		}},
		{name: "unpinned/Runtime Broker row missing", build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = missing
			return &a
		}},
		{name: "unpinned/legacy row", build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = legacy.ID
			return &a
		}},
		{name: "unpinned/flat row", code: ErrCodeRuntimeTargetPinStale, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = flat.ID
			return &a
		}},
		{name: "pinned/no pinned Runtime Broker", code: ErrCodeRuntimeTargetPinStale, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = flat.ID
			pin(&a, "", flat.RuntimeTarget.ID, flat.RuntimeTarget.Type)
			return &a
		}},
		{name: "pinned/no target type", code: ErrCodeRuntimeTargetPinStale, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = flat.ID
			pin(&a, flat.ID, flat.RuntimeTarget.ID, "")
			return &a
		}},
		{name: "pinned/no Runtime Broker", code: ErrCodeRuntimeTargetPinStale, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = ""
			pin(&a, flat.ID, flat.RuntimeTarget.ID, flat.RuntimeTarget.Type)
			return &a
		}},
		{name: "pinned/assigned elsewhere", code: ErrCodeRuntimeTargetPinStale, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = legacy.ID
			pin(&a, flat.ID, flat.RuntimeTarget.ID, flat.RuntimeTarget.Type)
			return &a
		}},
		{name: "pinned/Runtime Broker row missing", code: ErrCodeRuntimeBrokerMissing, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = missing
			pin(&a, missing, "target-of-a-gone-broker", "docker")
			return &a
		}},
		{name: "pinned/row not flat", code: ErrCodeRuntimeTargetPinStale, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = legacy.ID
			pin(&a, legacy.ID, "some-target", "docker")
			return &a
		}},
		{name: "pinned/row serves another target", code: ErrCodeRuntimeTargetMismatch, build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = flat.ID
			pin(&a, flat.ID, "another-target", flat.RuntimeTarget.Type)
			return &a
		}},
		{name: "pinned/consistent", build: func(a store.Agent) *store.Agent {
			a.RuntimeBrokerID = flat.ID
			pin(&a, flat.ID, flat.RuntimeTarget.ID, flat.RuntimeTarget.Type)
			return &a
		}},
	}
}

func TestRequirePinnedPlacement_States(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	ctx := context.Background()
	base := store.Agent{ID: tid("guard-unit"), Slug: "guard-unit", ProjectID: f.project.ID}
	for _, st := range placementStates(f.flat, f.legacy, tid("no-such-broker")) {
		t.Run(st.name, func(t *testing.T) {
			a := st.build(base)
			broker, err := requirePinnedPlacement(ctx, f.s, a)
			switch {
			case st.code != "":
				requirePlacementRefusal(t, err, st.code)
				if st.code != ErrCodeRuntimeTargetMismatch {
					d := err.(*PlacementConflictError).Refusal.Details
					assert.Equal(t, a.ID, d["agentId"])
					assert.Equal(t, a.PinnedRuntimeBrokerID, d["pinnedRuntimeBrokerId"])
					assert.Equal(t, a.RuntimeBrokerID, d["runtimeBrokerId"])
				}
			case st.noBroker:
				assert.ErrorIs(t, err, errNoRuntimeBrokerAssigned)
			default:
				require.NoError(t, err)
				if a.IsPinned() {
					require.NotNil(t, broker)
					assert.Equal(t, f.flat.ID, broker.ID)
				}
			}
		})
	}

	t.Run("store error is returned, not a refusal", func(t *testing.T) {
		boom := errors.New("injected broker read failure")
		st := &brokerReadErrStore{Store: f.s, err: boom}
		for _, a := range []*store.Agent{
			{ID: "a", RuntimeBrokerID: f.flat.ID},
			{ID: "b", RuntimeBrokerID: f.flat.ID, PinnedRuntimeBrokerID: f.flat.ID, PinnedRuntimeTargetID: f.flat.RuntimeTarget.ID, PinnedRuntimeTargetType: "docker"},
		} {
			_, err := requirePinnedPlacement(ctx, st, a)
			require.ErrorIs(t, err, boom)
			_, isRefusal := placementRefusalOf(err)
			assert.False(t, isRefusal)
		}
	})

	t.Run("checkAgentPinnedPlacement passes only an unpinned agent with no Runtime Broker", func(t *testing.T) {
		_, err := checkAgentPinnedPlacement(ctx, f.s, &store.Agent{ID: "x"})
		assert.NoError(t, err)
		_, err = checkAgentPinnedPlacement(ctx, f.s, &store.Agent{ID: "y", PinnedRuntimeBrokerID: f.flat.ID, PinnedRuntimeTargetID: f.flat.RuntimeTarget.ID, PinnedRuntimeTargetType: "docker"})
		requirePlacementRefusal(t, err, ErrCodeRuntimeTargetPinStale)
	})

	t.Run("the refusal answers HTTPStatus and unwraps to the refusal", func(t *testing.T) {
		_, err := requirePinnedPlacement(ctx, f.s, &store.Agent{ID: "z", RuntimeBrokerID: f.flat.ID})
		var pc *PlacementConflictError
		require.True(t, errors.As(err, &pc))
		status, code, msg := pc.HTTPStatus()
		assert.Equal(t, http.StatusConflict, status)
		assert.Equal(t, ErrCodeRuntimeTargetPinStale, code)
		assert.Equal(t, pc.Refusal.Message, msg)
		var r *RuntimeTargetRefusal
		require.True(t, errors.As(err, &r))
		rec := httptest.NewRecorder()
		writeErrorFromErr(rec, err, "")
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Equal(t, ErrCodeRuntimeTargetPinStale, decodeFlatAPIError(t, rec).Code)
		assert.True(t, isConfirmedStartNotActedOnError(err), "a refusal raised before anything is sent")
	})
}

// --- classification list -------------------------------------------------

// placementGuardKind is how one dispatcher method meets the placement rule.
type placementGuardKind int

const (
	// placementGuardBackstop: the method runs requirePinnedPlacement
	// itself before any store write or broker call.
	placementGuardBackstop placementGuardKind = iota
	// placementGuardCreateShaped: a create of a new agent. Its placement
	// is decided by flatCreatePlacement before dispatch, written in the
	// create transaction, and checked by the Runtime Broker
	// (expectedRuntimeTargetId); there is no saved placement to check yet.
	placementGuardCreateShaped
)

// placementGuardOp is one classified dispatcher method.
type placementGuardOp struct {
	kind placementGuardKind
	// guardFirst: the guard runs before requireRuntimeBrokerAssigned, so a
	// pinned agent with no Runtime Broker gets the placement 409 (start,
	// restart, finalize-env, keys). Otherwise that state is answered by
	// requireRuntimeBrokerAssigned first; both send nothing.
	guardFirst bool
	// missingRowRefusedEarly: the method refuses a missing Runtime Broker
	// row on its own before the guard (provision-for-move loads the row to
	// check AgentMove).
	missingRowRefusedEarly bool
	call                   func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error
}

// placementGuardClassification classifies every method of AgentDispatcher,
// agentkeys.Dispatcher and agentMoveDispatcher. The calls name each method,
// so a renamed method does not compile, and
// TestPlacementGuard_ClassificationCoversInterfaces fails on a method the
// list does not name.
var placementGuardClassification = map[string]placementGuardOp{
	"DispatchAgentCreate":           {kind: placementGuardCreateShaped},
	"DispatchAgentCreateWithGather": {kind: placementGuardCreateShaped},
	"DispatchAgentProvision": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentProvision(ctx, a)
	}},
	"DispatchAgentReprovision": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentReprovision(ctx, a)
	}},
	"DispatchAgentStart": {guardFirst: true, call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentStart(ctx, a, "", false)
	}},
	"DispatchAgentStartWithOptions": {guardFirst: true, call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchAgentStartWithOptions(ctx, a, "", false, StartDispatchOptions{})
		return err
	}},
	"DispatchAgentStop": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentStop(ctx, a)
	}},
	"DispatchAgentRestart": {guardFirst: true, call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentRestart(ctx, a)
	}},
	"DispatchAgentResetAuth": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentResetAuth(ctx, a)
	}},
	"DispatchAgentDelete": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentDelete(ctx, a, false, false, false, time.Time{})
	}},
	"DispatchAgentMessage": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentMessage(ctx, a, "hello", false, nil)
	}},
	"DispatchAgentLogs": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchAgentLogs(ctx, a, 10)
		return err
	}},
	"DispatchAgentExec": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, _, err := d.DispatchAgentExec(ctx, a, []string{"true"}, 5)
		return err
	}},
	"DispatchCheckAgentPrompt": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchCheckAgentPrompt(ctx, a)
		return err
	}},
	"DispatchFinalizeEnv": {guardFirst: true, call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchFinalizeEnv(ctx, a, nil)
		return err
	}},
	"DispatchAgentKeys": {guardFirst: true, call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchAgentKeys(ctx, agentkeys.Target{
			RuntimeBrokerID: a.RuntimeBrokerID, AgentID: a.ID, AgentSlug: a.Slug, ProjectID: a.ProjectID,
		}, "op-placement", time.Now().Add(30*time.Second), "C-c")
		return err
	}},
	"DispatchAgentProvisionForMove": {missingRowRefusedEarly: true, call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentProvisionForMove(ctx, a, "/nfs/workspace", "")
	}},
	"DispatchAgentDeleteLocalOnly": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentDeleteLocalOnly(ctx, a)
	}},
	"DispatchAgentDeleteRecreated": {call: func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentDeleteRecreated(ctx, a, "reincarnation-1")
	}},
}

// classifiedDispatcherInterfaces are the interfaces whose every method
// must be classified.
func classifiedDispatcherInterfaces() []reflect.Type {
	return []reflect.Type{
		reflect.TypeOf((*AgentDispatcher)(nil)).Elem(),
		reflect.TypeOf((*agentkeys.Dispatcher)(nil)).Elem(),
		reflect.TypeOf((*agentMoveDispatcher)(nil)).Elem(),
		reflect.TypeOf((*startOptionsDispatcher)(nil)).Elem(),
	}
}

func TestPlacementGuard_ClassificationCoversInterfaces(t *testing.T) {
	methods := map[string]bool{}
	for _, it := range classifiedDispatcherInterfaces() {
		for i := 0; i < it.NumMethod(); i++ {
			name := it.Method(i).Name
			methods[name] = true
			op, ok := placementGuardClassification[name]
			if !assert.True(t, ok, "%s.%s is not classified in placementGuardClassification: decide its placement guard", it.Name(), name) {
				continue
			}
			if op.kind == placementGuardBackstop {
				assert.NotNil(t, op.call, "%s is a backstopped method with no call", name)
			}
		}
	}
	for name := range placementGuardClassification {
		assert.True(t, methods[name], "placementGuardClassification names %s, which no classified interface has", name)
	}
}

// --- every broker-reaching dispatcher method × every state ---------------------

func TestPlacementGuard_EveryBrokerReachingOp(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	ctx := context.Background()

	// Runtime Broker rows of this test, both advertising AgentMove so the
	// move legs reach their guard.
	caps := &store.BrokerCapabilities{Reprovision: true, Sync: true, AgentMove: true}
	legacy := &store.RuntimeBroker{ID: tid("guard-legacy"), Name: "guard-legacy", Slug: "guard-legacy",
		Status: store.BrokerStatusOnline, Endpoint: "http://guard-legacy.invalid", Capabilities: caps}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, legacy))
	flat := &store.RuntimeBroker{ID: tid("guard-flat"), Name: "guard-flat", Slug: "guard-flat",
		Status: store.BrokerStatusOnline, Endpoint: "http://guard-flat.invalid", Capabilities: caps,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("guard-flat-target"), Type: "docker"}}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, flat))
	flat, err := f.s.GetRuntimeBroker(ctx, flat.ID)
	require.NoError(t, err)
	require.True(t, flat.IsFlat())

	// The row every in-memory state shares its ID with: a write by agent ID
	// lands on it and is caught by the before/after comparison.
	baseRow := &store.Agent{
		ID: tid("guard-matrix-agent"), Slug: "guard-matrix-agent", Name: "guard-matrix-agent",
		ProjectID: f.project.ID, RuntimeBrokerID: legacy.ID, Phase: string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{CreateInputs: &store.AgentCreateInputs{}},
	}
	require.NoError(t, f.s.CreateAgent(ctx, baseRow))
	base, err := f.s.GetAgent(ctx, baseRow.ID)
	require.NoError(t, err)

	over := &agentOverrideStore{Store: f.s}
	client := &countingBrokerClient{}
	minter := &countingTokenGen{inner: f.srv}
	d := NewHTTPAgentDispatcherWithClient(over, client, false, slog.Default())
	d.SetTokenGenerator(minter)

	for name, op := range placementGuardClassification {
		if op.kind != placementGuardBackstop {
			continue
		}
		for _, st := range placementStates(flat, legacy, tid("guard-no-such-broker")) {
			t.Run(name+"/"+st.name, func(t *testing.T) {
				a := st.build(*base)
				over.set(a)
				client.reset()
				minter.mints = 0
				before, err := f.s.GetAgent(ctx, base.ID)
				require.NoError(t, err)

				callErr := op.call(ctx, d, a)

				code, noBroker := st.code, st.noBroker
				if st.name == "pinned/no Runtime Broker" && !op.guardFirst {
					code, noBroker = "", true
				}
				refusedEarly := st.name == "pinned/Runtime Broker row missing" && op.missingRowRefusedEarly
				switch {
				case refusedEarly:
					require.Error(t, callErr)
				case code != "":
					requirePlacementRefusal(t, callErr, code)
				case noBroker:
					assert.ErrorIs(t, callErr, errNoRuntimeBrokerAssigned)
				default:
					_, refused := placementRefusalOf(callErr)
					assert.False(t, refused, "the guard must pass this state: %v", callErr)
					if st.name == "unpinned/Runtime Broker row missing" {
						require.Error(t, callErr, "no endpoint for a missing row")
						assert.Equal(t, 0, client.count(), "nothing is sent without a Runtime Broker row")
					}
					return
				}
				// A refusal sends nothing, mints nothing and writes nothing.
				assert.Equal(t, 0, client.count(), "broker calls: %v", client.calls)
				assert.Equal(t, 0, minter.mints, "no agent credential is minted")
				after, err := f.s.GetAgent(ctx, base.ID)
				require.NoError(t, err)
				assert.Equal(t, before, after, "the agent row is unchanged")
				assert.Equal(t, 0, pendingDispatchCount(t, f.s, flat.ID, legacy.ID, f.flat.ID, f.legacy.ID), "no dispatch row is queued")
			})
		}
	}
}

// --- handler pre-checks ----------------------------------------------------

func TestPlacementGuard_StopAndSuspendRefusedBeforeAnyWrite(t *testing.T) {
	for _, action := range []string{"stop", "suspend"} {
		t.Run(action, func(t *testing.T) {
			f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
			a := f.stalePinnedAgent(t, "stale-"+action, string(state.PhaseRunning))
			reserveBrokerSlot(t, f.s, f.legacy, a.ID)
			before := mustGetAgent(t, f.s, a.ID)

			rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/"+action, nil)
			d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
			requireStalePinDetails(t, d, a, f)

			assert.False(t, f.client.stopCalled, "no stop is dispatched")
			after := mustGetAgent(t, f.s, a.ID)
			assert.Equal(t, before.RunIntent, after.RunIntent, "no stop intent is recorded")
			assert.Equal(t, before.Phase, after.Phase)
			assert.Equal(t, before.ContainerStatus, after.ContainerStatus, "no stop_queued state")
			assert.Equal(t, before.StartClaimID, after.StartClaimID)
			assert.Equal(t, int64(1), brokerReservationCount(t, f.s, f.legacy.ID), "the reservation is kept")
			assert.Equal(t, 0, pendingDispatchCount(t, f.s, f.flat.ID, f.legacy.ID), "nothing is queued for an offline Runtime Broker")
		})
	}
}

func TestPlacementGuard_KeysRefusedBeforeDispatch(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-keys", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/keys", validKeysBody)
	d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
	assert.NotEmpty(t, d["operation_id"], "every keys outcome after validation carries the operation ID")
	assert.Equal(t, a.ID, d["agentId"])
}

// reset-auth, exec and logs write the dispatcher's placement refusal as its
// own 409 and code, not as a 5xx.
func TestPlacementGuard_ResetAuthExecLogsAnswer409(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	a := f.stalePinnedAgent(t, "stale-rel", string(state.PhaseRunning))
	client := &countingBrokerClient{}
	f.srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(f.s, client, false, slog.Default()))
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   interface{}
	}{
		{"reset-auth", http.MethodPost, "/api/v1/agents/" + a.ID + "/reset-auth", nil},
		{"exec", http.MethodPost, "/api/v1/agents/" + a.ID + "/exec", map[string]interface{}{"command": []string{"true"}}},
		{"logs", http.MethodGet, "/api/v1/agents/" + a.ID + "/logs", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doRequest(t, f.srv, tc.method, tc.path, tc.body)
			d := requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)
			requireStalePinDetails(t, d, a, f)
			assert.Equal(t, 0, client.count(), "nothing is sent: %v", client.calls)
		})
	}
}

// A scope re-issue of an agent whose saved placement is inconsistent is
// refused before the commit: 409 with the placement code, the delegation
// edge and the agent's credentials unchanged, nothing dispatched.
func TestPlacementGuard_ScopeReissueRefusedBeforeCommit(t *testing.T) {
	ctx := context.Background()
	f := newReissueFixture(t, "rs-placement", store.ProjectRoleOwner)
	f.run(t, f.parent, false) // the child now has scopes to gain
	child := f.reload(t, f.child)
	// Pin the child to its own, non-flat Runtime Broker: an inconsistent
	// placement (runtime_target_pin_stale).
	_, err := f.store.SetAgentPinnedRuntimeTarget(ctx, child.ID,
		store.PinnedPlacement{RuntimeBrokerID: child.RuntimeBrokerID},
		store.PinnedPlacement{RuntimeBrokerID: child.RuntimeBrokerID, RuntimeTargetID: "target-of-a-non-flat-row", RuntimeTargetType: "docker"})
	require.NoError(t, err)
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, "rs-placement-jti")
	edgesBefore := f.allEdges(t, f.child)
	f.client.resetAuthCalled = false

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.child.ID+"/reset-auth", ScopeReissueRequest{ReissueScopes: true})
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)

	assert.Equal(t, edgesBefore, f.allEdges(t, f.child), "the delegation edge is unchanged: nothing was committed")
	cred := getTestAgentCredential(t, f.store, "rs-placement-jti")
	assert.Nil(t, cred.RevokedAt, "the agent's credentials are not revoked")
	assert.False(t, f.client.resetAuthCalled, "nothing is dispatched")
	assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch), "no dispatch record")
}

// --- non-dispatcher routes -------------------------------------------------

func TestPlacementGuard_PTYRefusedBeforeUpgrade(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	stale := f.stalePinnedAgent(t, "stale-pty", string(state.PhaseRunning))
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/agents/"+stale.ID+"/pty", nil)
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)

	ctx := context.Background()
	d := f.srv.resolvePTYPath(ctx, nil, stale)
	assert.Equal(t, ptyPathNone, d.Path)
	assert.Equal(t, http.StatusConflict, d.Status)
	assert.Equal(t, ErrCodeRuntimeTargetPinStale, d.Code)

	missing := *stale
	missing.RuntimeBrokerID = tid("pty-gone")
	missing.PinnedRuntimeBrokerID = missing.RuntimeBrokerID
	d = f.srv.resolvePTYPath(ctx, nil, &missing)
	assert.Equal(t, http.StatusConflict, d.Status)
	assert.Equal(t, ErrCodeRuntimeBrokerMissing, d.Code)

	// An unpinned legacy agent keeps today's decision (here: broker not
	// connected), and an agent with no Runtime Broker keeps the 422.
	legacy := f.unpinnedAgentOn(t, "legacy-pty", f.legacy.ID, string(state.PhaseRunning))
	d = f.srv.resolvePTYPath(ctx, nil, legacy)
	assert.NotEqual(t, http.StatusConflict, d.Status)
	none := *legacy
	none.RuntimeBrokerID = ""
	d = f.srv.resolvePTYPath(ctx, nil, &none)
	assert.Equal(t, http.StatusUnprocessableEntity, d.Status)
	assert.Equal(t, ErrCodeNoRuntimeBroker, d.Code)
}

func TestPlacementGuard_ConduitGrantRefusedAfterAuthorization(t *testing.T) {
	cf := newConduitFixture(t)
	ctx := context.Background()
	flat := &store.RuntimeBroker{ID: tid("conduit-flat"), Name: "conduit-flat", Slug: "conduit-flat", Status: store.BrokerStatusOnline,
		RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("conduit-flat-target"), Type: "docker"}}
	require.NoError(t, cf.store.CreateRuntimeBroker(ctx, flat))

	stale := *cf.agent
	stale.PinnedRuntimeBrokerID = flat.ID
	stale.PinnedRuntimeTargetID = flat.RuntimeTarget.ID
	stale.PinnedRuntimeTargetType = "docker" // assigned to "broker-1": stale
	mismatch := *cf.agent
	mismatch.RuntimeBrokerID = flat.ID
	mismatch.PinnedRuntimeBrokerID = flat.ID
	mismatch.PinnedRuntimeTargetID = "another-target"
	mismatch.PinnedRuntimeTargetType = "docker"
	consistent := mismatch
	consistent.PinnedRuntimeTargetID = flat.RuntimeTarget.ID

	mint := func(ident Identity, a *store.Agent, h grant.StreamHeader) error {
		_, _, err := cf.srv.mintConduitGrant(ctx, conduitGrantRequest{Identity: ident, Agent: a, Stream: h,
			Target: grant.Target{Kind: grant.TargetKindAgent, ID: a.ID, EndpointIncarnation: "inc-1", SessionID: "sess-1", ConnectionEpoch: 3}})
		return err
	}
	for _, h := range []grant.StreamHeader{tcpHeader("3000"), ptyHeader("80", "24")} {
		requirePlacementRefusal(t, mint(cf.owner, &stale, h), ErrCodeRuntimeTargetPinStale)
		requirePlacementRefusal(t, mint(cf.owner, &mismatch, h), ErrCodeRuntimeTargetMismatch)
		assert.NoError(t, mint(cf.owner, &consistent, h), "a consistent pin is granted")
	}

	// Permission first: a caller without it is denied as before and learns
	// nothing about the placement.
	err := mint(cf.stranger, &stale, tcpHeader("3000"))
	require.ErrorIs(t, err, errConduitForbidden)
	_, refused := placementRefusalOf(err)
	assert.False(t, refused)

	// The proxy writes the refusal's own 409, never a 403.
	pc := placementConflict(stalePinRefusal(&stale, "test"))
	rec := httptest.NewRecorder()
	writeConduitProxyError(rec, httptest.NewRequest(http.MethodGet, "/", nil), stale.ID, fmt.Errorf("open: %w", pc))
	requireAPIError(t, rec, http.StatusConflict, ErrCodeRuntimeTargetPinStale)

	// The tunnel maps each placement code to its own 409 refusal.
	for code, want := range tunnelPlacementFailures {
		r := &RuntimeTargetRefusal{Code: code, Status: http.StatusConflict, Message: "m"}
		var te *conduitTunnelError
		require.True(t, errors.As(conduitTunnelOpenError(fmt.Errorf("x: %w", placementConflict(r))), &te), code)
		assert.Equal(t, want, te.failure)
		assert.Equal(t, http.StatusConflict, conduitTunnelFailures[te.failure].status)
		assert.Equal(t, code, conduitTunnelFailures[te.failure].code)
	}
}

// A store failure during the placement check is a temporary routing
// failure on the conduit paths (503 registry_unavailable), never a 500 or
// a grant-key fault.
func TestPlacementGuard_ConduitStoreFaultIsRegistryUnavailable(t *testing.T) {
	boom := errors.New("injected broker read failure")

	t.Run("PTY", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true, storeWrap: brokerReadFaultWrap(boom)})
		a := f.pinnedAgent(t, "pty-fault", string(state.PhaseRunning))
		f.storeFault.Arm()
		d := f.srv.resolvePTYPath(context.Background(), nil, a)
		assert.Equal(t, ptyPathNone, d.Path)
		assert.Equal(t, http.StatusServiceUnavailable, d.Status)
		assert.Equal(t, ErrCodeUnavailable, d.Code)
		assert.Equal(t, ptyReasonRegistryUnavailable, d.Reason)
	})

	t.Run("PTY, unpinned agent keeps today's decision", func(t *testing.T) {
		f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true, storeWrap: brokerReadFaultWrap(boom)})
		a := f.unpinnedAgentOn(t, "pty-fault-unpinned", f.legacy.ID, string(state.PhaseRunning))
		f.storeFault.Arm()
		d := f.srv.resolvePTYPath(context.Background(), nil, a)
		assert.NotEqual(t, ptyReasonRegistryUnavailable, d.Reason, "the placement check fails open for an unpinned agent")
		assert.NotEqual(t, http.StatusConflict, d.Status)
	})

	t.Run("conduit grant, unpinned and managed agents are not refused", func(t *testing.T) {
		cf := newConduitFixture(t)
		ctx := context.Background()
		_, fault := installStoreFault(t, cf.srv, brokerReadFaultWrap(boom))
		fault.Arm()
		mint := func(a *store.Agent) error {
			_, _, err := cf.srv.mintConduitGrant(ctx, conduitGrantRequest{Identity: cf.owner, Agent: a, Stream: tcpHeader("3000"),
				Target: grant.Target{Kind: grant.TargetKindAgent, ID: a.ID, EndpointIncarnation: "inc-1", SessionID: "sess-1", ConnectionEpoch: 3}})
			return err
		}
		unpinned := *cf.agent // on "broker-1", whose row read now fails
		assert.NoError(t, mint(&unpinned), "an unpinned agent keeps today's path")
		managed := *cf.agent
		managed.Runtime = ManagedRuntimePrefix + "test"
		assert.NoError(t, mint(&managed), "a managed runtime is not checked")
	})

	t.Run("conduit grant", func(t *testing.T) {
		cf := newConduitFixture(t)
		ctx := context.Background()
		flat := &store.RuntimeBroker{ID: tid("conduit-fault-flat"), Name: "conduit-fault-flat", Slug: "conduit-fault-flat", Status: store.BrokerStatusOnline,
			RuntimeTarget: &api.RuntimeTargetDescriptor{ID: tid("conduit-fault-target"), Type: "docker"}}
		require.NoError(t, cf.store.CreateRuntimeBroker(ctx, flat))
		a := *cf.agent
		a.RuntimeBrokerID = flat.ID
		a.PinnedRuntimeBrokerID = flat.ID
		a.PinnedRuntimeTargetID = flat.RuntimeTarget.ID
		a.PinnedRuntimeTargetType = "docker"
		_, fault := installStoreFault(t, cf.srv, brokerReadFaultWrap(boom))
		fault.Arm()

		_, _, err := cf.srv.mintConduitGrant(ctx, conduitGrantRequest{Identity: cf.owner, Agent: &a, Stream: tcpHeader("3000"),
			Target: grant.Target{Kind: grant.TargetKindAgent, ID: a.ID, EndpointIncarnation: "inc-1", SessionID: "sess-1", ConnectionEpoch: 3}})
		require.ErrorIs(t, err, router.ErrRegistryUnavailable)
		require.ErrorIs(t, err, boom)
		_, refused := placementRefusalOf(err)
		assert.False(t, refused)

		// The tunnel keeps it as registry unavailable, not grant keys.
		assert.True(t, conduitTunnelMintErrPassesThrough(err))
		var te *conduitTunnelError
		require.True(t, errors.As(conduitTunnelOpenError(err), &te))
		assert.Equal(t, tunnelRegistryUnavailable, te.failure)
		assert.Equal(t, http.StatusServiceUnavailable, conduitTunnelFailures[te.failure].status)
		assert.Equal(t, ptyReasonRegistryUnavailable, conduitTunnelFailures[te.failure].reason)

		// The port proxy answers its routing-unavailable 503.
		rec := httptest.NewRecorder()
		writeConduitProxyError(rec, httptest.NewRequest(http.MethodGet, "/", nil), a.ID, err)
		requireAPIError(t, rec, http.StatusServiceUnavailable, ErrCodeUnavailable)
	})
}

func TestPlacementGuard_WorkspaceTunnelRefusedBeforeSend(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	stale := f.stalePinnedAgent(t, "stale-ws", string(state.PhaseRunning))
	// A nil control channel: the refusal comes before it is used.
	err := tunnelAgentWorkspaceRequest(context.Background(), f.s, nil, stale, http.MethodPost, "/api/v1/workspace/upload", nil, nil)
	requirePlacementRefusal(t, err, ErrCodeRuntimeTargetPinStale)

	// Every agent workspace request goes through the wrapper: outside the
	// wrapper's own body (which delegates to tunnelWorkspaceRequest), no
	// caller tunnels to agent.RuntimeBrokerID directly, and the three callers
	// (sync-to-hub upload, apply, stop sync-back) call the wrapper.
	direct := regexp.MustCompile(`tunnelWorkspaceRequest\([^)]*agent\.RuntimeBrokerID`)
	wrapperFunc := regexp.MustCompile(`(?s)\nfunc tunnelAgentWorkspaceRequest\(.*?\n}\n`)
	wrapped := 0
	for _, file := range []string{"workspace_handlers.go", "handlers_projects_core.go"} {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		outside := wrapperFunc.ReplaceAllString(string(src), "\n")
		assert.Empty(t, direct.FindAllString(outside, -1), "%s tunnels an agent workspace request without the placement wrapper", file)
		wrapped += strings.Count(outside, "tunnelAgentWorkspaceRequest(")
	}
	assert.Equal(t, 3, wrapped, "the sync-to-hub upload, the apply and the stop sync-back call the wrapper")
}

// --- guidance ----------------------------------------------------------------

func TestPlacementGuidance_NoDeleteAndRecreateOrForce(t *testing.T) {
	a := &store.Agent{ID: "agent-1", RuntimeBrokerID: "rb-2", PinnedRuntimeBrokerID: "rb-1", PinnedRuntimeTargetID: "t-1", PinnedRuntimeTargetType: "docker"}
	for _, r := range []*RuntimeTargetRefusal{
		stalePinRefusal(a, "it is assigned to Runtime Broker rb-2"),
		runtimeBrokerMissingRefusal(a),
	} {
		msg := r.Message
		assert.Contains(t, msg, "agent agent-1 is pinned to Runtime Broker \"rb-1\" (target \"t-1\") but its saved placement is inconsistent (")
		assert.Contains(t, msg, "Scion will not operate on it through another Runtime Broker.")
		assert.Contains(t, msg, "No per-agent repair is available in this release (tracked in ptone/scion#4844); see "+inconsistentPlacementDocsURL+".")
		lower := strings.ToLower(msg)
		for _, banned := range []string{"recreate", "re-create", "force", "delete"} {
			assert.NotContains(t, lower, banned, "the guidance must not suggest %q", banned)
		}
	}

	doc, err := os.ReadFile("../../docs-site/src/content/docs/hosted/ha/multi-broker.md")
	require.NoError(t, err)
	page := string(doc)
	assert.True(t, strings.HasSuffix(inconsistentPlacementDocsURL, "/hosted/ha/multi-broker/#inconsistent-agent-placement"))
	assert.Contains(t, page, "#### Inconsistent agent placement")
	assert.Contains(t, page, "ptone/scion#4844")
	assert.Contains(t, page, "ptone/scion#4811")
	lowerPage := strings.ToLower(page)
	assert.NotContains(t, lowerPage, "delete and recreate")
	assert.NotContains(t, lowerPage, "delete it and recreate")
}

// --- H1: no profile written onto a pinned agent ----------------------------------

func TestH1_ProfileDefaultNotWrittenOntoPinnedAgent(t *testing.T) {
	f := newFlatHubFixture(t, flatHubOpts{experimentOn: true, linkFlat: true})
	ctx := context.Background()
	sa := newSettingsTestSA(t, f.s, f.project.ID, "h1-sa")
	enforceSAAssign(f.srv, store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email))
	setProjectAnnotations(t, f.s, f.project, map[string]string{
		projectSettingActiveProfile:                   "remote",
		projectSettingDefaultGCPIdentitySAIDByProfile: fmt.Sprintf(`{"remote":%q}`, sa.ID),
	})
	project, err := f.s.GetProject(ctx, f.project.ID)
	require.NoError(t, err)

	// The lookup itself would apply to an unpinned agent; it is skipped for
	// a pinned one.
	unpinned := &store.Agent{ID: "u", AppliedConfig: &store.AgentAppliedConfig{}}
	profile, saID := f.srv.profileDefaultSAUnlessPinned(ctx, unpinned, f.legacy.ID, project)
	assert.Equal(t, "remote", profile)
	assert.Equal(t, sa.ID, saID)
	pinned := &store.Agent{ID: "p", RuntimeBrokerID: f.flat.ID, PinnedRuntimeBrokerID: f.flat.ID,
		PinnedRuntimeTargetID: f.flat.RuntimeTarget.ID, PinnedRuntimeTargetType: "docker", AppliedConfig: &store.AgentAppliedConfig{}}
	profile, saID = f.srv.profileDefaultSAUnlessPinned(ctx, pinned, f.flat.ID, project)
	assert.Empty(t, profile)
	assert.Empty(t, saID)

	// The interactive create path.
	rec := f.create(t, map[string]interface{}{"name": "h1-pinned", "runtimeBrokerId": f.flat.ID})
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	a := f.agentBySlug(t, "h1-pinned")
	require.NotNil(t, a)
	require.True(t, a.IsPinned())
	require.NotNil(t, a.AppliedConfig)
	assert.Empty(t, a.AppliedConfig.Profile, "no profile is written onto a pinned agent")
	if a.AppliedConfig.CreateInputs != nil {
		assert.Empty(t, a.AppliedConfig.CreateInputs.Profile, "nor into what reincarnate replays")
	}
	if a.AppliedConfig.GCPIdentity != nil {
		assert.NotEqual(t, sa.ID, a.AppliedConfig.GCPIdentity.ServiceAccountID, "the per-profile default is not applied")
	}

	// The scheduled create path skips it for a pinned agent too.
	scheduled := &store.Agent{ID: "s", ProjectID: project.ID, RuntimeBrokerID: f.flat.ID, PinnedRuntimeBrokerID: f.flat.ID,
		PinnedRuntimeTargetID: f.flat.RuntimeTarget.ID, PinnedRuntimeTargetType: "docker", AppliedConfig: &store.AgentAppliedConfig{}}
	origin, err := f.srv.applyScheduledProjectDefaultGCPIdentity(ctx, scheduled, project)
	require.NoError(t, err)
	assert.NotEqual(t, store.SAAssignmentOriginScheduledProjectProfileDefault, origin)
	assert.Empty(t, scheduled.AppliedConfig.Profile, "no profile is written onto a pinned scheduled agent")
	if scheduled.AppliedConfig.GCPIdentity != nil {
		assert.NotEqual(t, sa.ID, scheduled.AppliedConfig.GCPIdentity.ServiceAccountID)
	}

	// Both create paths call the pinned-aware helper, never the lookup
	// directly.
	for _, file := range []string{"handlers_agents_core.go", "server.go"} {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		assert.NotContains(t, string(src), "s.projectProfileDefaultSA(", file)
		assert.Contains(t, string(src), "s.profileDefaultSAUnlessPinned(ctx, agent,", file)
	}
}
