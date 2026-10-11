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
	"github.com/stretchr/testify/require"
)

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
	// warnings, when set, are the hub-only env warnings the broker's
	// answer carries (relayed to the dispatch warnings collector).
	warnings []string
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
	// A deleting row whose lease expired: the engine died, the delete reads
	// as failed (abandoned), so the agent counts as live.
	{"delete-expired", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateDeleting, -time.Minute)
	}, false},
	// A finalizing row holds the agent even with its lease expired:
	// teardown already ran, only a retry or force lifts it.
	{"finalizing-expired", func(t *testing.T, s store.Store, id string) {
		claimForTest(t, s, id, store.DeletionStateFinalizing, -time.Minute)
	}, true},
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

// failingGetStore fails GetAgent, as a database outage would, once armed.
type failingGetStore struct {
	store.Store
	fail bool
}

func (c *landingClient) answer(slug, runID string) *RemoteAgentResponse {
	if c.onLand != nil {
		c.onLand()
	}
	info := &RemoteAgentInfo{ID: slug, Slug: slug, Name: slug, Phase: string(state.PhaseRunning), Warnings: c.warnings}
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

func (s *failingGetStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fail {
		return nil, errors.New("database is unavailable")
	}
	return s.Store.GetAgent(ctx, id)
}
