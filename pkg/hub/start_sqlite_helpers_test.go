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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// claimTestDispatcher is an AgentDispatcher whose start and stop are
// supplied by the test; every other method is unused.
type claimTestDispatcher struct {
	AgentDispatcher
	start func(ctx context.Context, a *store.Agent) error
	stops atomic.Int32
}

// fastClaims shortens a claim run's lease timing so lease behaviour is
// observable in milliseconds.
func fastClaims(srv *Server, ttl time.Duration) {
	srv.startClaimTestHook = func(r *startClaimRun) {
		r.cfg.LeaseTTL = ttl
		r.fenceAt = time.Now().Add(ttl - ttl/3)
		r.renewEvery = ttl / 3
		r.retryEvery = ttl / 10
	}
}

func newClaimFixture(t *testing.T) (*reconcileFixture, *claimTestDispatcher, *store.Agent) {
	t.Helper()
	f := newReconcileFixture(t)
	d := &claimTestDispatcher{}
	f.srv.SetDispatcher(d)
	f.srv.startClaimsOn = true
	a := f.addAgent("claimed", "stopped", "")
	return f, d, a
}

func getAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

// stopHookDispatcher is a claimTestDispatcher whose stop also runs a hook.
type stopHookDispatcher struct {
	*claimTestDispatcher
	onStop func(ctx context.Context, a *store.Agent)
}

func lifecycle(t *testing.T, f *reconcileFixture, id, action string) (int, map[string]interface{}) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+id+"/"+action, nil)
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func errorDetails(body map[string]interface{}) (string, map[string]interface{}) {
	e, _ := body["error"].(map[string]interface{})
	code, _ := e["code"].(string)
	details, _ := e["details"].(map[string]interface{})
	return code, details
}

// failRunningStatusStore fails the started-status write.
type failRunningStatusStore struct {
	store.Store
	agentID string
}

// claimRefusingStore refuses every start claim with err.
type claimRefusingStore struct {
	store.Store
	err error
}

func (d *claimTestDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	if d.start == nil {
		return nil
	}
	return d.start(ctx, a)
}

func (d *claimTestDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	d.stops.Add(1)
	return nil
}

func (d *stopHookDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	if d.onStop != nil {
		d.onStop(ctx, a)
	}
	return d.claimTestDispatcher.DispatchAgentStop(ctx, a)
}

func (s failRunningStatusStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if id == s.agentID && u.ClearExit {
		return errors.New("database is locked")
	}
	return s.Store.UpdateAgentStatus(ctx, id, u)
}

func (s claimRefusingStore) ClaimAgentStart(context.Context, string, string, store.StartClaimKind, string, time.Duration) (store.StartClaim, error) {
	return store.StartClaim{}, s.err
}
