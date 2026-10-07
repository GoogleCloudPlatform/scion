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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
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

// compensationMetrics wires a manual-reader dispatch metrics recorder into
// d and returns a reader of the compensating_delete counters.
func compensationMetrics(t *testing.T, d *HTTPAgentDispatcher) func() (done, failed int64) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	rec, err := dispatchmetrics.New(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	require.NoError(t, err)
	d.SetDispatchMetrics(rec)
	return func() (done, failed int64) {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				sum, ok := m.Data.(metricdata.Sum[int64])
				if !ok {
					continue
				}
				for _, dp := range sum.DataPoints {
					if op, _ := dp.Attributes.Value(attribute.Key("op")); op.AsString() != compensatingDeleteOp {
						continue
					}
					switch m.Name {
					case dispatchmetrics.MetricDispatchDone:
						done += dp.Value
					case dispatchmetrics.MetricDispatchFailed:
						failed += dp.Value
					}
				}
			}
		}
		return done, failed
	}
}

// A successful compensating delete is counted as done{op=compensating_delete}.
func TestLandedRunCompensation_Success_CountsDone(t *testing.T) {
	f, c := newLandingFixture(t, "land-metric-ok")
	counters := compensationMetrics(t, f.dispatcher)
	c.onLand = func() { require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID)) }
	_, err := f.dispatcher.DispatchAgentCreate(context.Background(), f.agent)
	require.NoError(t, err)
	require.Len(t, c.deleteRuns, 1)
	done, failed := counters()
	assert.Equal(t, int64(1), done)
	assert.Zero(t, failed)
}

// The request is cancelled right after the broker answered (the caller
// went away): the re-read and the delete are detached from it, so the run
// is still deleted.
func TestLandedRunCompensation_RequestCancelled_StillCompensates(t *testing.T) {
	f, c := newLandingFixture(t, "land-cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.onLand = func() {
		require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID))
		cancel()
	}
	_, err := f.dispatcher.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	assert.Equal(t, []string{c.lastCreateReq.RunID}, c.deleteRuns,
		"a cancelled request still deletes the run that landed")
}

// failingGetStore fails GetAgent, as a database outage would, once armed.
type failingGetStore struct {
	store.Store
	fail bool
}

func (s *failingGetStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fail {
		return nil, errors.New("database is unavailable")
	}
	return s.Store.GetAgent(ctx, id)
}

// A failed re-read cannot tell whether the agent was deleted: it is
// counted as a failure and the caller gets a warning.
func TestLandedRunCompensation_ReadFails_WarnsAndCounts(t *testing.T) {
	f, c := newLandingFixture(t, "land-readfail")
	fs := &failingGetStore{Store: f.store}
	f.dispatcher = NewHTTPAgentDispatcherWithClient(fs, c, false, f.dispatcher.log)
	counters := compensationMetrics(t, f.dispatcher)
	c.onLand = func() { fs.fail = true }
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.dispatcher.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	assert.Empty(t, c.deleteRuns)
	assert.Contains(t, warns.Warnings(), "could not check whether the agent was deleted while it was starting: database is unavailable")
	_, failed := counters()
	assert.Equal(t, int64(1), failed)
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
	counters := compensationMetrics(t, f.dispatcher)
	c.onLand = func() { require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID)) }
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.dispatcher.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	assert.Len(t, c.deleteRuns, 1)
	assert.Contains(t, warns.Warnings(), "agent was deleted while it was starting; removing its container failed: broker unreachable")
	done, failed := counters()
	assert.Zero(t, done)
	assert.Equal(t, int64(1), failed, "the failure is counted as scion.dispatch.failed{op=compensating_delete}")
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

// The broker refuses the compensating delete because a same-name successor
// holds the name (ptone/scion#3080): the landed run is already gone, the
// successor is left alone, and it counts as done, not as a failure.
func TestLandedRunCompensation_SuccessorHoldsName_CountsDone(t *testing.T) {
	f, c := newLandingFixture(t, "land-successor")
	counters := compensationMetrics(t, f.dispatcher)
	c.onLand = func() { require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID)) }
	c.deleteErr = &DeleteRunMismatchError{RequestedRunID: "run-landed", CurrentRunID: "run-successor",
		Err: &brokerStatusError{StatusCode: http.StatusNotFound}}
	ctx, warns := withDispatchWarnings(context.Background())
	_, err := f.dispatcher.DispatchAgentCreate(ctx, f.agent)
	require.NoError(t, err)
	assert.Len(t, c.deleteRuns, 1)
	assert.Contains(t, warns.Warnings(), "agent was deleted while it was starting; its container was already replaced by another run")
	done, failed := counters()
	assert.Equal(t, int64(1), done)
	assert.Zero(t, failed)
}
