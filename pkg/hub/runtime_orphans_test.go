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
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Tests for the runtime orphan report (ptone/scion#7930).

const logRuntimeOrphan = "runtime orphan: runtime entry has no hub agent"

type orphanFixture struct {
	*reconcileFixture
	logs   *captureHandler
	reader *sdkmetric.ManualReader
}

func newOrphanFixture(t *testing.T, on bool) *orphanFixture {
	t.Helper()
	f := &orphanFixture{reconcileFixture: newReconcileFixture(t), logs: &captureHandler{}, reader: sdkmetric.NewManualReader()}
	f.srv.agentLifecycleLog = slog.New(f.logs)
	setExperimentOverrides(t, f.srv, map[string]bool{experiments.RuntimeOrphanReconcile: on})
	require.NoError(t, f.srv.RegisterRuntimeOrphanMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(f.reader))))
	return f
}

// entry is a running entry on target docker.
func entry(slug, agentID string) brokerAgentHeartbeat {
	return brokerAgentHeartbeat{Slug: slug, AgentID: agentID, Phase: "running", RuntimeTarget: "docker", RunID: "run-" + slug}
}

// beat sends an online heartbeat with inv, the fixture project's entries
// and any further projects.
func (f *orphanFixture) beat(inv *brokerInventory, entries []brokerAgentHeartbeat, more ...brokerProjectHeartbeat) {
	f.t.Helper()
	f.send(brokerHeartbeatRequest{Status: store.BrokerStatusOnline, Inventory: inv,
		Projects: append([]brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: entries}}, more...)})
}

// reasons returns the recorded orphans' reasons by slug.
func (f *orphanFixture) reasons() map[string]string {
	f.t.Helper()
	got, err := f.s.ListRuntimeOrphans(context.Background(), store.RuntimeOrphanFilter{})
	require.NoError(f.t, err)
	out := map[string]string{}
	for _, o := range got {
		out[o.Slug] = o.Reason
	}
	return out
}

// gauge returns scion.hub.runtime_orphans by reason.
func (f *orphanFixture) gauge() map[string]int64 {
	f.t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(f.t, f.reader.Collect(context.Background(), &rm))
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "scion.hub.runtime_orphans" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Gauge[int64]).DataPoints {
				r, _ := dp.Attributes.Value(attribute.Key("reason"))
				b, _ := dp.Attributes.Value(attribute.Key("broker"))
				assert.Equal(f.t, f.brokerID, b.AsString())
				out[r.AsString()] = dp.Value
			}
		}
	}
	return out
}

// TestRuntimeOrphans_Classification covers every reason, the matched and
// excluded entries, one log per new orphan, and the gauge.
func TestRuntimeOrphans_Classification(t *testing.T) {
	f := newOrphanFixture(t, true)
	ctx := context.Background()
	other := &store.RuntimeBroker{ID: tid("rc-other"), Name: "other", Slug: "other", Status: store.BrokerStatusOnline}
	require.NoError(t, f.s.CreateRuntimeBroker(ctx, other))

	live := f.addAgent("live", "running", "working")
	gone := f.addAgent("gone", "running", "working")
	require.NoError(t, f.s.DeleteAgent(ctx, gone.ID)) // a forced, hard delete
	reused := f.addAgent("reused", "running", "working")
	soft := f.addAgent("soft", "running", "working")
	soft.DeletedAt = time.Now()
	require.NoError(t, f.s.UpdateAgent(ctx, soft))
	elsewhere := f.addAgent("elsewhere", "running", "working", func(a *store.Agent) { a.RuntimeBrokerID = other.ID })
	lease := time.Now().Add(time.Hour)
	deleting := f.addAgent("deleting", "running", "working", func(a *store.Agent) { a.RuntimeBrokerID = other.ID })
	state := store.DeletionStateDeleting
	_, err := f.s.UpdateAgentDeletion(ctx, deleting.ID, store.DeletionPredicate{}, store.DeletionFields{State: &state, LeaseAt: &lease})
	require.NoError(t, err)
	starting := f.addAgent("starting", "provisioning", "")
	require.NoError(t, f.s.InsertBrokerDispatch(ctx, &store.BrokerDispatch{
		ID: uuid.NewString(), BrokerID: f.brokerID, AgentID: starting.ID, ProjectID: f.projectID, Op: "start",
	}))

	entries := []brokerAgentHeartbeat{
		entry("live", live.ID),
		entry("legacy", ""),        // older broker, no agent ID, no agent: no_agent
		entry("gone", gone.ID),     // no_agent
		entry("reused", reused.ID), // the current agent: matched
		entry("reused", uuid.NewString()),
		entry("soft", soft.ID),
		entry("elsewhere", elsewhere.ID),
		entry("deleting", deleting.ID),      // delete in flight: excluded
		entry("starting", uuid.NewString()), // queued start: excluded
		entry("live", ""),                   // older broker, slug only: matched
	}
	unknown := brokerProjectHeartbeat{ProjectID: uuid.NewString(), Agents: []brokerAgentHeartbeat{entry("stray", "")}}
	unlabeled := brokerProjectHeartbeat{ProjectID: "default", Agents: []brokerAgentHeartbeat{entry("bare", "")}}
	f.beat(completeInventory(), entries, unknown, unlabeled)

	want := map[string]string{
		"legacy": orphanNoAgent, "gone": orphanNoAgent, "reused": orphanSuperseded, "soft": orphanDeleted,
		"elsewhere": orphanOtherBroker, "stray": orphanUnknownProject, "bare": orphanUnlabeled,
	}
	assert.Equal(t, want, f.reasons())
	assert.Len(t, f.logs.records(logRuntimeOrphan), len(want))

	// The next heartbeat logs nothing new.
	f.beat(completeInventory(), entries, unknown, unlabeled)
	assert.Equal(t, want, f.reasons())
	assert.Len(t, f.logs.records(logRuntimeOrphan), len(want), "one log per new orphan")
	assert.Equal(t, map[string]int64{orphanNoAgent: 2, orphanSuperseded: 1, orphanDeleted: 1,
		orphanOtherBroker: 1, orphanUnknownProject: 1, orphanUnlabeled: 1}, f.gauge())
}

// TestRuntimeOrphans_IncompleteInventory: a project-filtered heartbeat (no
// targets), a failed target listing, or an entry on an unlisted target
// records nothing.
func TestRuntimeOrphans_IncompleteInventory(t *testing.T) {
	f := newOrphanFixture(t, true)
	stray := []brokerAgentHeartbeat{entry("stray", uuid.NewString())}
	f.beat(nil, stray)
	f.beat(&brokerInventory{}, stray)
	f.beat(&brokerInventory{Targets: []brokerInventoryTarget{{ID: "docker", Complete: false}}}, stray)
	f.beat(completeInventory("k8s"), stray)
	assert.Empty(t, f.reasons())
	assert.Empty(t, f.logs.records(logRuntimeOrphan))
}

// TestRuntimeOrphans_Resolved: an orphan missing from three complete
// inventories in a row is removed; incomplete ones do not count.
func TestRuntimeOrphans_Resolved(t *testing.T) {
	f := newOrphanFixture(t, true)
	f.beat(completeInventory(), []brokerAgentHeartbeat{entry("stray", "")})
	require.Equal(t, map[string]string{"stray": orphanNoAgent}, f.reasons())
	f.beat(completeInventory(), nil)
	f.beat(completeInventory(), nil)
	f.beat(nil, nil)
	f.beat(completeInventory("k8s"), nil)
	assert.Len(t, f.reasons(), 1, "two complete inventories without it")
	f.beat(completeInventory(), nil)
	assert.Empty(t, f.reasons())
	assert.Empty(t, f.gauge())
}

// TestRuntimeOrphans_ExperimentOff: nothing is classified or written, the
// gauge observes nothing and the admin route answers 404.
func TestRuntimeOrphans_ExperimentOff(t *testing.T) {
	f := newOrphanFixture(t, false)
	f.beat(completeInventory(), []brokerAgentHeartbeat{entry("stray", "")})
	assert.Empty(t, f.reasons())
	assert.Empty(t, f.logs.records(logRuntimeOrphan))
	assert.Empty(t, f.gauge())
	rec := doRequest(t, f.srv, http.MethodGet, "/api/v1/admin/runtime-orphans", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// TestAdminRuntimeOrphans: the listing needs hub.health.read, filters by
// broker, project and kind, and refuses another kind or method.
func TestAdminRuntimeOrphans(t *testing.T) {
	srv, s := testServerNoDevAuth(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.RuntimeOrphanReconcile: true})
	ctx := context.Background()
	o := func(target, project string) store.RuntimeOrphan {
		return store.RuntimeOrphan{Kind: store.RuntimeOrphanKindInstance, RuntimeTarget: target, ProjectID: project, Slug: "s", Reason: orphanNoAgent}
	}
	_, err := s.RecordRuntimeOrphans(ctx, "b1", []string{"docker"}, []store.RuntimeOrphan{o("docker", "p1"), o("docker", "p2")}, time.Now(), 3)
	require.NoError(t, err)
	_, err = s.RecordRuntimeOrphans(ctx, "b2", []string{"docker"}, []store.RuntimeOrphan{o("docker", "p1")}, time.Now(), 3)
	require.NoError(t, err)
	path := "/api/v1/admin/runtime-orphans"

	rec := doRequestAsUser(t, srv, newPlainUser(t, s, "orphans-plain"), http.MethodGet, path, nil)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	admin := newHubAdminRoleUser(t, s, "orphans-hub-admin")
	count := func(query string) int {
		t.Helper()
		rec := doRequestAsUser(t, srv, admin, http.MethodGet, path+query, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp RuntimeOrphansResponse
		decodeJSONBody(t, rec, &resp)
		return len(resp.Orphans)
	}
	assert.Equal(t, 3, count(""))
	assert.Equal(t, 2, count("?broker=b1"))
	assert.Equal(t, 1, count("?broker=b1&project=p2"))
	assert.Equal(t, 3, count("?kind=instance"))
	assert.Equal(t, 0, count("?kind=nfs_tree"))

	rec = doRequestAsUser(t, srv, admin, http.MethodGet, path+"?kind=volume", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = doRequestAsUser(t, srv, admin, http.MethodPost, path, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, rec.Body.String())
}
