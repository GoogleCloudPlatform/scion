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
	"runtime"
	"strings"
	"sync"
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
	default:
		return "in-handler-final-write"
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
	applied     bool
}

func (p *startWriteDeleteStore) applyOnce() {
	if !p.applied {
		p.applied = true
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
			p.mu.Lock()
			p.applyOnce()
			p.mu.Unlock()
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
		p.mu.Lock()
		p.applyOnce()
		p.mu.Unlock()
	}
	return p.Store.UpdateAgentStatus(ctx, id, u)
}

func (p *startWriteDeleteStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := p.Store.GetAgent(ctx, id)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.window == windowAfterReRead && p.wroteStart && calledFromDeleteWonAfterLanding() {
		// This read is the handler's deleteWonAfterLanding re-read (other
		// reads run between the started write and it, such as the
		// compensating-stop check); the delete claims the row right after
		// it.
		p.wroteStart = false
		p.applyOnce()
	}
	return a, err
}

// calledFromDeleteWonAfterLanding reports whether the store call in
// progress was made by Server.deleteWonAfterLanding.
func calledFromDeleteWonAfterLanding() bool {
	pcs := make([]uintptr, 16)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(3, pcs)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, ".(*Server).deleteWonAfterLanding") {
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
						require.True(t, p.applied, "the delete was applied in its window")
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
	require.True(t, p.applied, "the delete claimed the row after the stop's write")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// stopClaimStore claims the row for a delete right after the stop's
// stopped write.
type stopClaimStore struct {
	store.Store
	apply   func()
	applied bool
}

func (p *stopClaimStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	err := p.Store.UpdateAgentStatus(ctx, id, u)
	if err == nil && !p.applied && u.Phase == string(state.PhaseStopped) {
		p.applied = true
		p.apply()
	}
	return err
}
