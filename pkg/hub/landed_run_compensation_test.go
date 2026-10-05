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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every synchronous dispatch that starts a runtime entry (create, create
// with gather, finalize env, start, restart) deletes the run it started
// again when the agent was deleted, or a delete holds it, while the broker
// call was in flight (ptone/scion#3055).

// landingClient is mockRuntimeBrokerClient whose create, start and restart
// run onLand (the racing delete) and then answer with a running entry
// labelled with the run the request named. reportRunID=false answers as an
// older broker, with no run ID.
type landingClient struct {
	*mockRuntimeBrokerClient
	onLand      func()
	reportRunID bool
	deleteRuns  []string
	deleteErr   error
}

func (c *landingClient) answer(slug, runID string) *RemoteAgentResponse {
	if c.onLand != nil {
		c.onLand()
	}
	info := &RemoteAgentInfo{ID: slug, Slug: slug, Name: slug, Phase: string(state.PhaseRunning)}
	if c.reportRunID {
		info.RunID = runID
	}
	return &RemoteAgentResponse{Agent: info, Created: true}
}

func (c *landingClient) CreateAgent(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error) {
	c.lastCreateReq = req
	return c.answer(req.Slug, req.RunID), nil
}

func (c *landingClient) CreateAgentWithGather(_ context.Context, _, _ string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error) {
	c.lastCreateReq = req
	return c.answer(req.Slug, req.RunID), nil, nil
}

func (c *landingClient) StartAgent(_ context.Context, _, _, agentID, _, _, _, _, _, _, _ string, _ map[string]string, _ []ResolvedSecret, _ *api.ScionConfig, _ []api.SharedDir, _, _ bool, extras StartExtras) (*RemoteAgentResponse, error) {
	c.lastStartExtras = extras
	return c.answer(agentID, extras.RunID), nil
}

func (c *landingClient) RestartAgent(_ context.Context, _, _, agentID, _ string, _ map[string]string, extras StartExtras) (*RemoteAgentResponse, error) {
	c.lastRestartExtras = extras
	return c.answer(agentID, extras.RunID), nil
}

func (c *landingClient) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	c.deleteRuns = append(c.deleteRuns, opts.RunID)
	return c.deleteErr
}

var landingOps = []struct {
	name string
	run  func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error
	sent func(c *landingClient) string
}{
	{"create", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchAgentCreate(ctx, a)
		return err
	}, func(c *landingClient) string { return c.lastCreateReq.RunID }},
	{"create-with-gather", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchAgentCreateWithGather(ctx, a)
		return err
	}, func(c *landingClient) string { return c.lastCreateReq.RunID }},
	{"finalize-env", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		_, err := d.DispatchFinalizeEnv(ctx, a, map[string]string{"K": "v"})
		return err
	}, func(c *landingClient) string { return c.lastCreateReq.RunID }},
	{"start", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentStart(ctx, a, "", false)
	}, func(c *landingClient) string { return c.lastStartExtras.RunID }},
	{"restart", func(ctx context.Context, d *HTTPAgentDispatcher, a *store.Agent) error {
		return d.DispatchAgentRestart(ctx, a)
	}, func(c *landingClient) string { return c.lastRestartExtras.RunID }},
}

// landingDeletes are the ways a delete can hold the row when the broker
// answers.
var landingDeletes = []struct {
	name       string
	apply      func(t *testing.T, s store.Store, id string)
	compensate bool
}{
	{"none", func(*testing.T, store.Store, string) {}, false},
	{"hard-deleted", func(t *testing.T, s store.Store, id string) {
		require.NoError(t, s.DeleteAgent(context.Background(), id))
	}, true},
	{"soft-deleted", func(t *testing.T, s store.Store, id string) {
		a, err := s.GetAgent(context.Background(), id)
		require.NoError(t, err)
		a.DeletedAt = time.Now()
		require.NoError(t, s.UpdateAgent(context.Background(), a))
	}, true},
	{"delete-claimed", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateDeleting, time.Minute)
	}, true},
	{"delete-failed", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateFailed, time.Minute)
	}, false},
}

func claimForTest(t *testing.T, s store.Store, id, st string, lease time.Duration) {
	t.Helper()
	at := time.Now().Add(lease)
	n, err := s.UpdateAgentDeletion(context.Background(), id,
		store.DeletionPredicate{States: []string{""}, DeletedAtNull: true},
		store.DeletionFields{State: &st, LeaseAt: &at})
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func newLandingFixture(t *testing.T, name string) (*runIDFixture, *landingClient) {
	t.Helper()
	f := newRunIDFixture(t, name)
	c := &landingClient{mockRuntimeBrokerClient: f.client, reportRunID: true}
	f.dispatcher = NewHTTPAgentDispatcherWithClient(f.store, c, false, f.dispatcher.log)
	return f, c
}

func TestLandedRunCompensation_EverySyncDispatch(t *testing.T) {
	for _, op := range landingOps {
		for _, del := range landingDeletes {
			t.Run(op.name+"/"+del.name, func(t *testing.T) {
				f, c := newLandingFixture(t, "land-"+op.name+"-"+del.name)
				c.onLand = func() { del.apply(t, f.store, f.agent.ID) }
				ctx, warns := withDispatchWarnings(context.Background())

				require.NoError(t, op.run(ctx, f.dispatcher, f.agent))
				sent := op.sent(c)
				require.NotEmpty(t, sent)
				if !del.compensate {
					assert.Empty(t, c.deleteRuns, "no compensating delete")
					assert.Empty(t, warns.Warnings())
					return
				}
				assert.Equal(t, []string{sent}, c.deleteRuns, "exactly one delete, scoped to the run that landed")
				assert.Contains(t, warns.Warnings(), "agent was deleted while it was starting; its container was removed")
			})
		}
	}
}

// An older broker reports no run ID: a delete by name could hit a same-name
// successor, so none is sent; the caller gets a warning.
func TestLandedRunCompensation_NoRunIDReported_NoDelete(t *testing.T) {
	f, c := newLandingFixture(t, "land-norunid")
	c.reportRunID = false
	c.onLand = func() { require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID)) }
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.dispatcher.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	assert.Empty(t, c.deleteRuns)
	assert.Contains(t, warns.Warnings(), "agent was deleted while it was starting; its container could not be removed safely (the broker reported no run ID)")
}

// A failed compensating delete does not fail the dispatch; it is reported
// as a warning (and logged and counted).
func TestLandedRunCompensation_DeleteFails_Warns(t *testing.T) {
	f, c := newLandingFixture(t, "land-delfail")
	c.deleteErr = errors.New("broker unreachable")
	c.onLand = func() { require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID)) }
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.dispatcher.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	assert.Len(t, c.deleteRuns, 1)
	assert.Contains(t, warns.Warnings(), "agent was deleted while it was starting; removing its container failed: broker unreachable")
}

// An agent with no row never recorded its run, so no delete can have
// removed it: no compensation.
func TestLandedRunCompensation_NoRow_NoDelete(t *testing.T) {
	f, c := newLandingFixture(t, "land-norow")
	rowless := *f.agent
	rowless.ID = tid("agent-never-stored")
	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), &rowless)
	require.NoError(t, err)
	assert.Empty(t, c.deleteRuns)
}
