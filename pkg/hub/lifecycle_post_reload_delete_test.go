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
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A synchronous start or restart whose broker start landed while the agent
// was live, but whose row a delete claims afterwards, before the handler
// answers, answers 409 delete_in_progress rather than 200 with the delete's
// phase (ptone/scion#3546). The handler checks the row settleLifecycleWrite
// reloaded with deleteWonAfterLanding's rule, so a failed delete, or a
// deleting row whose lease expired, still answers 200.

// startWriteWindow is where startWriteDeleteStore applies the delete,
// relative to the start's status writes (store.AgentStatusUpdate with
// StartWrite and ClearExit: startAgentCore's started write, and the
// handler's own final write when startAgentCore's failed).
type startWriteWindow int

const (
	// Inside startAgentCore's started write, before it reaches the store:
	// the deleteWonAfterLanding re-read after the dispatch sees the delete.
	windowBeforeStartedWrite startWriteWindow = iota
	// After startAgentCore's started write landed and after the handler's
	// deleteWonAfterLanding re-read (the first GetAgent after the write):
	// no further status write runs, only settleLifecycleWrite's reload.
	windowAfterReRead
	// startAgentCore's started write fails (nothing is written), so the
	// handler writes the status itself; the delete claims the row inside
	// that final write, after the deleteWonAfterLanding re-read, and the
	// store's delete guard neutralises it and returns nil.
	windowInHandlerFinalWrite
)

func (w startWriteWindow) String() string {
	switch w {
	case windowBeforeStartedWrite:
		return "before-started-write"
	case windowAfterReRead:
		return "after-re-read"
	case windowInHandlerFinalWrite:
		return "in-handler-final-write"
	default:
		return fmt.Sprintf("startWriteWindow(%d)", int(w))
	}
}

// startWriteDeleteStore is set as srv.store (the dispatcher keeps the raw
// store) and applies the delete once, at its window. The delete is applied
// on the raw store, so it does not re-enter the hooks.
type startWriteDeleteStore struct {
	store.Store
	window startWriteWindow
	apply  func()

	mu          sync.Mutex
	startWrites int
	wroteStart  bool
	applied     atomic.Bool
}

func (p *startWriteDeleteStore) applyOnce() {
	if p.applied.CompareAndSwap(false, true) {
		p.apply()
	}
}

func (p *startWriteDeleteStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if !u.StartWrite || !u.ClearExit {
		return p.Store.UpdateAgentStatus(ctx, id, u)
	}
	p.mu.Lock()
	p.startWrites++
	n := p.startWrites
	p.mu.Unlock()
	switch p.window {
	case windowBeforeStartedWrite:
		if n == 1 {
			p.applyOnce()
		}
	case windowAfterReRead:
		err := p.Store.UpdateAgentStatus(ctx, id, u)
		if n == 1 && err == nil {
			p.mu.Lock()
			p.wroteStart = true
			p.mu.Unlock()
		}
		return err
	case windowInHandlerFinalWrite:
		if n == 1 {
			return errors.New("db unavailable")
		}
		p.applyOnce()
	}
	return p.Store.UpdateAgentStatus(ctx, id, u)
}

func (p *startWriteDeleteStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := p.Store.GetAgent(ctx, id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window == windowAfterReRead && p.wroteStart && calledFrom(".(*Server).deleteWonAfterLanding") {
		// This read is the handler's deleteWonAfterLanding re-read (other
		// reads run between the started write and it, such as the
		// compensating-stop check); the delete claims the row right after
		// it.
		p.wroteStart = false
		p.applyOnce()
	}
	return a, err
}

// calledFrom reports whether the store call in progress was made, directly
// or not, by the function whose qualified name ends in suffix (for example
// ".(*Server).deleteWonAfterLanding"). The tests use it to place a fault at
// one specific read: several reads of the row run between the started write
// and the reload (the compensating-stop check among them), so counting calls
// would be fragile. A rename that stops it matching is not silent: the tests
// that inject this way require that the fault was applied (p.applied, or
// failedReload), and fail otherwise.
//
// It does not skip a fixed number of frames: which frames exist depends on
// what the compiler inlines, so a skip count could drop the frame being
// looked for. It skips only runtime.Callers itself, collects the whole
// stack (growing the buffer until it is not filled), and scans every frame.
// Inlining cannot hide the caller: runtime.CallersFrames expands inlined
// calls into their own frames, with their own function names.
func calledFrom(suffix string) bool {
	pcs := make([]uintptr, 64)
	for {
		n := runtime.Callers(1, pcs)
		if n < len(pcs) {
			pcs = pcs[:n]
			break
		}
		pcs = make([]uintptr, 2*len(pcs))
	}
	frames := runtime.CallersFrames(pcs)
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, suffix) {
			return true
		}
		if !more {
			return false
		}
	}
}

func TestLifecycle_DeleteClaimAroundStartedWrite(t *testing.T) {
	const brokerWarning = "hub-only env FOO was not forwarded"
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		for _, window := range []startWriteWindow{windowBeforeStartedWrite, windowAfterReRead, windowInHandlerFinalWrite} {
			for _, del := range landingDeletes {
				t.Run(action+"/"+window.String()+"/"+del.name, func(t *testing.T) {
					srv, s, agent, client := newLandedDeleteServer(t)
					client.warnings = []string{brokerWarning}
					p := &startWriteDeleteStore{Store: s, window: window, apply: func() { del.apply(t, s, agent.ID) }}
					srv.store = p

					rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
					require.NotEmpty(t, client.lastStartExtras.RunID, "the start leg reached the broker")
					assert.Empty(t, client.deleteRuns, "the start landed while the agent was live: no compensating delete")
					if del.name != "none" {
						require.True(t, p.applied.Load(), "the delete was applied in its window")
					}

					// del.compensate is the delete-won rule: hard- or
					// soft-deleted, a live deleting claim, or finalizing
					// even with an expired lease.
					if !del.compensate {
						require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
						var resp agentLifecycleResponse
						require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
						require.NotNil(t, resp.Agent)
						assert.Equal(t, agent.ID, resp.ID)
						assert.Equal(t, string(state.PhaseRunning), resp.Phase)
						assert.Contains(t, resp.Warnings, brokerWarning)
						return
					}

					requireIntentDeleteInProgress(t, rec, agent.ID)
					var body ErrorResponse
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
					assert.Equal(t, deletedWhileStartingMessage, body.Error.Message)
					assert.Contains(t, body.Error.Details["warnings"], brokerWarning,
						"the dispatch warnings are carried in the details")
					var raw map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
					assert.NotContains(t, raw, "id", "no agent body")
					assert.NotContains(t, raw, "agent", "no agent body")
				})
			}
		}
	}
}

// A start or restart of an agent with no runtime broker records its run
// intent and writes its status in the handler. A delete that claims, soft-
// or hard-deletes the row before that write answers 409 delete_in_progress
// with no agent body, as a brokered start does (ptone/scion#3546,
// ptone/scion#3697); a failed delete, or a deleting row whose lease
// expired, still answers 200.
func TestLifecycle_NoBrokerDeleteClaimBeforeFinalWrite(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		for _, del := range landingDeletes {
			t.Run(action+"/"+del.name, func(t *testing.T) {
				srv, s, agent, client := newLandedDeleteServer(t)
				ctx := context.Background()
				a, err := s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				a.RuntimeBrokerID = ""
				require.NoError(t, s.UpdateAgent(ctx, a))
				// The first start write is the handler's own final write.
				p := &startWriteDeleteStore{Store: s, window: windowBeforeStartedWrite, apply: func() { del.apply(t, s, agent.ID) }}
				srv.store = p

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				assert.Empty(t, client.lastStartExtras.RunID, "nothing reached a broker")
				if del.name != "none" {
					require.True(t, p.applied.Load(), "the delete was applied before the final write")
				}
				if !del.compensate {
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					var resp agentLifecycleResponse
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					require.NotNil(t, resp.Agent)
					assert.Equal(t, agent.ID, resp.ID)
					assert.Equal(t, string(state.PhaseRunning), resp.Phase)
					return
				}
				requireIntentDeleteInProgress(t, rec, agent.ID)
				var body ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Equal(t, deletedWhileStartingMessage, body.Error.Message)
				var raw map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
				assert.NotContains(t, raw, "id", "no agent body")
				assert.NotContains(t, raw, "agent", "no agent body")
			})
		}
	}
}

// failStartWriteStore fails every start status write (StartWrite and
// ClearExit) with a database error that is not a delete.
type failStartWriteStore struct {
	store.Store
	failed atomic.Bool
}

func (p *failStartWriteStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if u.StartWrite && u.ClearExit {
		p.failed.Store(true)
		return errors.New("db unavailable")
	}
	return p.Store.UpdateAgentStatus(ctx, id, u)
}

// A no-broker start or restart whose final status write fails with an error
// that is not a delete answers a server error, not delete_in_progress.
func TestLifecycle_NoBrokerFinalWriteError_NotDeleteWon(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s, agent, _ := newLandedDeleteServer(t)
			ctx := context.Background()
			a, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			a.RuntimeBrokerID = ""
			require.NoError(t, s.UpdateAgent(ctx, a))
			p := &failStartWriteStore{Store: s}
			srv.store = p

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.True(t, p.failed.Load(), "the final write ran and failed")
			require.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError, rec.Body.String())
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.NotEqual(t, ErrCodeDeleteInProgress, body.Error.Code)
		})
	}
}

// failReloadStore fails settleLifecycleWrite's reload of the row with a
// database error that is not store.ErrNotFound.
type failReloadStore struct {
	store.Store
	failedReload atomic.Bool
}

func (p *failReloadStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if calledFrom(".(*Server).settleLifecycleWrite") {
		p.failedReload.Store(true)
		return nil, errors.New("db unavailable")
	}
	return p.Store.GetAgent(ctx, id)
}

// A start or restart whose settle reload fails with an error that is not
// "row gone" answers 200 from the requested phase, as before: the check
// cannot tell, so it does not claim a delete won.
func TestLifecycle_SettleReloadError_Answers200(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, s, agent, client := newLandedDeleteServer(t)
			p := &failReloadStore{Store: s}
			srv.store = p

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.NotEmpty(t, client.lastStartExtras.RunID, "the start leg reached the broker")
			require.True(t, p.failedReload.Load(), "the settle reload ran and failed")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp agentLifecycleResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotNil(t, resp.Agent)
			assert.Equal(t, string(state.PhaseRunning), resp.Phase)
		})
	}
}

// A stop is unchanged: a delete that claims the row while the stop runs
// still answers 200 from the stored row.
func TestLifecycle_StopDeleteClaimAfterWrite_Answers200(t *testing.T) {
	srv, s, agent, _ := newLandedDeleteServer(t)
	ctx := context.Background()
	a, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	a.Phase = string(state.PhaseRunning)
	require.NoError(t, s.UpdateAgent(ctx, a))
	p := &stopClaimStore{Store: s, apply: func() {
		claimForTest(t, s, agent.ID, store.DeletionStateDeleting, time.Minute)
	}}
	srv.store = p

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+api.AgentActionStop, nil)
	require.True(t, p.applied.Load(), "the delete claimed the row after the stop's write")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// stopClaimStore claims the row for a delete right after the stop's
// stopped write.
type stopClaimStore struct {
	store.Store
	apply   func()
	applied atomic.Bool
}

func (p *stopClaimStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	err := p.Store.UpdateAgentStatus(ctx, id, u)
	if err == nil && u.Phase == string(state.PhaseStopped) && p.applied.CompareAndSwap(false, true) {
		p.apply()
	}
	return err
}
