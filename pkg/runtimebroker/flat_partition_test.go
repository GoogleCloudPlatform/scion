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

package runtimebroker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Partition validation (ptone/scion#3274): two flat Runtime Broker
// instances whose real, owner-filtered agent managers share one container
// daemon never report or act on each other's agents, nor on unlabeled
// historical objects.

// sharedDaemon is one container daemon's object set, shared by every
// instance's runtime, recording each runtime call that names an object.
type sharedDaemon struct {
	mu      sync.Mutex
	objects []api.AgentInfo
	calls   []string // "stop:<id>", "delete:<id>", "logs:<id>", "exec:<id>", "deleteResource:<uid>"
	// ownedCleanupErr, when set, fails every owner-scoped leftover cleanup.
	ownedCleanupErr error
	// absentErr, when set, fails every exact absence check.
	absentErr error
	// keepOnDelete leaves deleted objects in place (a deletion still
	// pending, e.g. a terminating pod).
	keepOnDelete bool
}

func (d *sharedDaemon) add(o api.AgentInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.objects = append(d.objects, o)
}

func (d *sharedDaemon) record(call string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, call)
}

func (d *sharedDaemon) recorded() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

func (d *sharedDaemon) reset() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = nil
}

func (d *sharedDaemon) list(filter map[string]string) []api.AgentInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []api.AgentInfo
	for _, o := range d.objects {
		match := true
		for k, v := range filter {
			if o.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			c := o
			c.Labels = map[string]string{}
			for k, v := range o.Labels {
				c.Labels[k] = v
			}
			out = append(out, c)
		}
	}
	return out
}

// daemonRuntime is one instance's runtime over the shared daemon; it keeps
// the UID-precondition delete capability.
type daemonRuntime struct {
	*runtime.MockRuntime
	d *sharedDaemon
}

func (r *daemonRuntime) DeleteResource(_ context.Context, h api.ResourceHandle) error {
	r.d.record("deleteResource:" + h.UID)
	r.d.remove(h.UID)
	return nil
}

// ResourceAbsent is the daemon's exact absence check: an object is gone
// once no object with that ID remains (absentErr, when set, fails it).
func (r *daemonRuntime) ResourceAbsent(_ context.Context, h api.ResourceHandle) (bool, error) {
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	if r.d.absentErr != nil {
		return false, r.d.absentErr
	}
	for _, o := range r.d.objects {
		if o.ContainerID == h.UID || o.ID == h.UID {
			return false, nil
		}
	}
	return true, nil
}

func (d *sharedDaemon) remove(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.keepOnDelete {
		return
	}
	kept := d.objects[:0]
	for _, o := range d.objects {
		if o.ContainerID != id && o.ID != id {
			kept = append(kept, o)
		}
	}
	d.objects = kept
}

// CleanupAgentResources is the runtime's name+project leftover cleanup
// (runtime.AgentResourceCleaner).
func (r *daemonRuntime) CleanupAgentResources(_ context.Context, agentName, projectID, _ string) error {
	r.d.record("cleanupAgentResources:" + projectID + "/" + agentName)
	return nil
}

// CleanupOwnedAgentResources is the owner-scoped leftover cleanup
// (runtime.OwnedAgentResourceCleaner); ownedCleanupErr makes it fail.
func (r *daemonRuntime) CleanupOwnedAgentResources(_ context.Context, agentName, projectID, runtimeBrokerID, runID string) error {
	r.d.record("cleanupOwned:" + projectID + "/" + agentName + "/" + runtimeBrokerID)
	if runID != "" {
		r.d.record("cleanupOwnedRun:" + projectID + "/" + agentName + "/" + runID)
	}
	r.d.mu.Lock()
	defer r.d.mu.Unlock()
	return r.d.ownedCleanupErr
}

func newDaemonRuntime(d *sharedDaemon) *daemonRuntime {
	return &daemonRuntime{d: d, MockRuntime: &runtime.MockRuntime{
		NameFunc: func() string { return "mock" },
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			return d.list(filter), nil
		},
		StopFunc: func(_ context.Context, ref runtime.RunRef) error {
			d.record("stop:" + ref.ID)
			return nil
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			d.record("delete:" + ref.ID)
			d.remove(ref.ID)
			return nil
		},
		GetLogsFunc: func(_ context.Context, id string) (string, error) {
			d.record("logs:" + id)
			return "log of " + id, nil
		},
		ExecFunc: func(_ context.Context, id string, _ []string) (string, error) {
			d.record("exec:" + id)
			return "", nil
		},
	}}
}

// partitionInstance is one flat instance on the shared daemon.
type partitionInstance struct {
	key      string
	identity *brokeridentity.Identity
	stateDir string
	mgr      *agent.AgentManager
	rt       *daemonRuntime
	srv      *Server
}

const partitionDaemonID = "shared-daemon"

// newPartitionInstance builds (or, with the same key and stateDir,
// rebuilds after a restart) a flat instance whose real agent manager runs
// on the shared daemon.
func newPartitionInstance(t *testing.T, d *sharedDaemon, key, stateDir string) *partitionInstance {
	t.Helper()
	return newPartitionInstanceWithLocks(t, d, key, stateDir, nil)
}

// newPartitionInstanceWithLocks is newPartitionInstance sharing a host's
// workspace lock service (nil: the server's own).
func newPartitionInstanceWithLocks(t *testing.T, d *sharedDaemon, key, stateDir string, locks *WorkspaceLocks) *partitionInstance {
	t.Helper()
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	id, err := brokeridentity.LoadOrCreate(brokeridentity.InstanceDir(globalDir, key), key, brokeridentity.TargetTypeDocker,
		brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeDocker, Docker: &brokeridentity.DockerScope{DaemonID: partitionDaemonID}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt := newDaemonRuntime(d)
	mgr := agent.NewManager(rt).(*agent.AgentManager)
	cfg := DefaultServerConfig()
	cfg.BrokerID = id.RuntimeBrokerID
	cfg.BrokerName = key
	cfg.StateDir = stateDir
	cfg.HubEnabled = true
	cfg.HubEndpoint = "http://127.0.0.1:1"
	cfg.BrokerAuthStrictMode = false
	cfg.FlatInstance = &FlatInstanceConfig{Identity: id, HubInProcess: true,
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key, RuntimeTarget: &config.V1RuntimeTargetConfig{Type: "docker"}}}
	cfg.InMemoryCredentials = &brokercredentials.BrokerCredentials{BrokerID: id.RuntimeBrokerID, SecretKey: "c2VjcmV0", HubEndpoint: cfg.HubEndpoint}
	cfg.WorkspaceLocks = locks
	srv := New(cfg, mgr, rt)
	return &partitionInstance{key: key, identity: id, stateDir: stateDir, mgr: mgr, rt: rt, srv: srv}
}

// own creates an agent object of this instance on the daemon, labelled as
// a start would label it, and records it as the instance's start would.
func (p *partitionInstance) own(t *testing.T, d *sharedDaemon, projectID, agentID, slug, containerID string) {
	t.Helper()
	d.add(api.AgentInfo{ID: containerID, ContainerID: containerID, Name: slug, ProjectID: projectID, Phase: "running", ContainerStatus: "Up 1 minute",
		Labels: map[string]string{"scion.agent": "true", "scion.name": slug, "scion.project_id": projectID, "agent_id": agentID,
			api.LabelRunID: "run-" + containerID, api.LabelRuntimeBrokerID: p.identity.RuntimeBrokerID}})
	st := p.srv.ownership
	if err := st.BeginRun(projectID, agentID, slug, "run-"+containerID); err != nil {
		t.Fatal(err)
	}
	if err := st.AddResource(projectID, agentID, "run-"+containerID, api.ResourceHandle{Kind: api.ResourceKindContainer, Name: slug, UID: containerID}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetRunState(projectID, agentID, "run-"+containerID, OwnershipStateCreated); err != nil {
		t.Fatal(err)
	}
}

// partitionFixture: instances A and B on one daemon.
//   - A: worker in proj-1 (cid-a1).
//   - B: worker in proj-2 (cid-b1, same slug, other project) and helper in
//     proj-1 (cid-b2).
//   - an unlabeled historical object "old" in proj-1 (cid-old).
type partitionFixture struct {
	d    *sharedDaemon
	a, b *partitionInstance
}

func newPartitionFixture(t *testing.T) *partitionFixture {
	t.Helper()
	setupTestScionEnv(t)
	d := &sharedDaemon{}
	f := &partitionFixture{d: d,
		a: newPartitionInstance(t, d, "docker-a", t.TempDir()),
		b: newPartitionInstance(t, d, "docker-b", t.TempDir())}
	f.a.own(t, d, "proj-1", "agent-a1", "worker", "cid-a1")
	f.b.own(t, d, "proj-2", "agent-b1", "worker", "cid-b1")
	f.b.own(t, d, "proj-1", "agent-b2", "helper", "cid-b2")
	d.add(api.AgentInfo{ID: "cid-old", ContainerID: "cid-old", Name: "old", Phase: "running",
		Labels: map[string]string{"scion.agent": "true", "scion.name": "old", "scion.project_id": "proj-1"}})
	return f
}

// listedContainers returns the container IDs an instance's agent list
// reports.
func listedContainers(t *testing.T, p *partitionInstance, query string) []string {
	t.Helper()
	w := serveFlat(p.srv, http.MethodGet, "/api/v1/agents"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("%s list: status %d: %s", p.key, w.Code, w.Body.String())
	}
	var resp struct {
		Agents []api.AgentInfo `json:"agents"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range resp.Agents {
		id := a.ContainerID
		if id == "" {
			id = a.ID
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func assertCalls(t *testing.T, d *sharedDaemon, want ...string) {
	t.Helper()
	got := d.recorded()
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("daemon calls = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("daemon calls = %v, want %v", got, want)
		}
	}
}

func TestFlatPartition_ListsOnlyOwnAgents(t *testing.T) {
	f := newPartitionFixture(t)
	if got := listedContainers(t, f.a, ""); len(got) != 1 || got[0] != "cid-a1" {
		t.Fatalf("A lists %v, want only its worker", got)
	}
	if got := listedContainers(t, f.b, ""); len(got) != 2 || got[0] != "cid-b1" || got[1] != "cid-b2" {
		t.Fatalf("B lists %v, want only its two agents", got)
	}
	if got := listedContainers(t, f.a, "?projectId=proj-1"); len(got) != 1 || got[0] != "cid-a1" {
		t.Fatalf("A lists %v in proj-1, want only its worker (not B's helper nor the unlabeled object)", got)
	}
	assertCalls(t, f.d)
}

func TestFlatPartition_LookupNeverResolvesAnotherInstancesAgent(t *testing.T) {
	f := newPartitionFixture(t)
	for _, path := range []string{
		"/api/v1/agents/helper?projectId=proj-1", // B's agent, by slug
		"/api/v1/agents/helper",                  // without project
		"/api/v1/agents/old?projectId=proj-1",    // unlabeled historical
		"/api/v1/agents/cid-b2?projectId=proj-1", // B's container ID as the agent ID
	} {
		if w := serveFlat(f.a.srv, http.MethodGet, path, ""); w.Code != http.StatusNotFound {
			t.Errorf("A GET %s: status %d, want 404: %s", path, w.Code, w.Body.String())
		}
	}
	// Same slug, other project: B resolves its own worker in proj-2 only.
	w := serveFlat(f.b.srv, http.MethodGet, "/api/v1/agents/worker?projectId=proj-1", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("B GET worker in proj-1 (A's): status %d, want 404: %s", w.Code, w.Body.String())
	}
	assertCalls(t, f.d)
}

// TestFlatPartition_OperationsNeverTouchAnotherInstancesAgent: stop,
// restart, delete, logs and exec on instance A for B's agents, A's
// same-slug agent in another project, the unlabeled object or B's
// container IDs never reach the daemon.
func TestFlatPartition_OperationsNeverTouchAnotherInstancesAgent(t *testing.T) {
	f := newPartitionFixture(t)
	type op struct{ method, path, body string }
	ops := []op{}
	for _, target := range []string{"helper?projectId=proj-1", "helper", "old?projectId=proj-1", "cid-b2?projectId=proj-1", "cid-b1"} {
		base := "/api/v1/agents/"
		name, query := splitQuery(target)
		ops = append(ops,
			op{http.MethodPost, base + name + "/stop" + query, ""},
			op{http.MethodDelete, base + name + query, ""},
			op{http.MethodGet, base + name + "/logs" + query, ""},
			op{http.MethodPost, base + name + "/exec" + query, `{"command":["true"]}`},
			op{http.MethodPost, base + name + "/restart" + query,
				`{"expectedRuntimeTargetId":"` + f.a.identity.RuntimeTarget.ID + `","resolvedEnv":{"SCION_AGENT_ID":"agent-b2"}}`},
		)
	}
	ops = append(ops, op{http.MethodPost, "/api/v1/agents/worker/stop?projectId=proj-2", ""},
		op{http.MethodDelete, "/api/v1/agents/worker?projectId=proj-2", ""})
	for _, o := range ops {
		w := serveFlat(f.a.srv, o.method, o.path, o.body)
		// A stop or delete of an agent this instance does not list is
		// idempotent (accepted / not found) and never reaches the daemon
		// (asserted below); a read or exec must not succeed.
		// A restart is refused (no ownership record of this instance).
		if w.Code >= 200 && w.Code < 300 && o.method != http.MethodDelete && !strings.HasSuffix(splitPath(o.path), "/stop") {
			t.Errorf("A %s %s succeeded (%d): %s", o.method, o.path, w.Code, w.Body.String())
		}
	}
	assertCalls(t, f.d)
	if got := listedContainers(t, f.b, ""); len(got) != 2 {
		t.Fatalf("B's agents changed: %v", got)
	}
	for _, k := range [][2]string{{"proj-2", "agent-b1"}, {"proj-1", "agent-b2"}} {
		rec, ok, err := f.b.srv.ownership.Get(k[0], k[1])
		if err != nil || !ok || rec.State != OwnershipStateActive {
			t.Fatalf("B's record %v changed: %+v %v", k, rec, err)
		}
	}
}

func splitPath(p string) string {
	name, _ := splitQuery(p)
	return name
}

func splitQuery(target string) (string, string) {
	for i := range target {
		if target[i] == '?' {
			return target[:i], target[i:]
		}
	}
	return target, ""
}

// TestFlatPartition_OwnOperationsTargetOnlyOwnObject: A's operations on its
// own worker touch exactly its container, although B has a same-slug agent
// in another project on the same daemon.
func TestFlatPartition_OwnOperationsTargetOnlyOwnObject(t *testing.T) {
	f := newPartitionFixture(t)
	if w := serveFlat(f.a.srv, http.MethodGet, "/api/v1/agents/worker/logs?projectId=proj-1", ""); w.Code != http.StatusOK {
		t.Fatalf("A logs of its worker: %d %s", w.Code, w.Body.String())
	}
	assertCalls(t, f.d, "logs:cid-a1")
	f.d.reset()
	if w := serveFlat(f.a.srv, http.MethodPost, "/api/v1/agents/worker/stop?projectId=proj-1", ""); w.Code >= 300 {
		t.Fatalf("A stop of its worker: %d %s", w.Code, w.Body.String())
	}
	assertCalls(t, f.d, "stop:cid-a1")
	f.d.reset()
	if w := serveFlat(f.a.srv, http.MethodDelete, "/api/v1/agents/worker?projectId=proj-1", ""); w.Code >= 300 {
		t.Fatalf("A delete of its worker: %d %s", w.Code, w.Body.String())
	}
	for _, c := range f.d.recorded() {
		if c != "stop:cid-a1" && c != "delete:cid-a1" && c != "deleteResource:cid-a1" {
			t.Fatalf("A's delete touched %s", c)
		}
	}
	if rec, _, _ := f.b.srv.ownership.Get("proj-2", "agent-b1"); rec == nil || rec.State != OwnershipStateActive {
		t.Fatal("B's same-slug agent's record changed")
	}
}

// TestFlatPartition_HeartbeatInventoryIsPerInstance: each instance's
// heartbeat reports only its own agents.
func TestFlatPartition_HeartbeatInventoryIsPerInstance(t *testing.T) {
	f := newPartitionFixture(t)
	for _, tc := range []struct {
		p    *partitionInstance
		want []string
	}{{f.a, []string{"proj-1/worker"}}, {f.b, []string{"proj-1/helper", "proj-2/worker"}}} {
		client := &mockRuntimeBrokerService{}
		svc := NewHeartbeatService(client, tc.p.identity.RuntimeBrokerID, time.Hour, tc.p.mgr, nil, slog.Default())
		svc.flat = true
		hb := svc.buildHeartbeat(context.Background())
		var got []string
		for _, proj := range hb.Projects {
			for _, a := range proj.Agents {
				got = append(got, proj.ProjectID+"/"+a.Slug)
			}
		}
		sort.Strings(got)
		if len(got) != len(tc.want) {
			t.Fatalf("%s heartbeat agents = %v, want %v", tc.p.key, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s heartbeat agents = %v, want %v", tc.p.key, got, tc.want)
			}
		}
	}
	assertCalls(t, f.d)
}

// TestFlatPartition_OldLaunchCleanupNeverDeletesAnotherInstancesObject: a
// launch cleanup on A naming B's (or an unlabeled) object deletes nothing;
// A's own recorded object is deleted with its UID precondition.
func TestFlatPartition_OldLaunchCleanupNeverDeletesAnotherInstancesObject(t *testing.T) {
	f := newPartitionFixture(t)
	err := f.a.mgr.CleanupLaunch(context.Background(), []api.ResourceHandle{
		{Kind: api.ResourceKindContainer, Name: "helper", UID: "cid-b2"},
		{Kind: api.ResourceKindContainer, Name: "old", UID: "cid-old"},
		{Kind: api.ResourceKindContainer, Name: "worker", UID: "cid-a1"},
	})
	if !errors.Is(err, agent.ErrNotOwned) {
		t.Fatalf("CleanupLaunch error = %v, want not owned for the foreign handles", err)
	}
	assertCalls(t, f.d, "deleteResource:cid-a1")
}

// TestFlatPartition_RestartKeepsPartition: rebuilding A (same key, same
// state root) after a restart keeps its identity, its records and its
// partition (the production state root is covered by
// TestFlatPartition_StateRootIsPerRuntimeBrokerID).
func TestFlatPartition_RestartKeepsPartition(t *testing.T) {
	f := newPartitionFixture(t)
	a2 := newPartitionInstance(t, f.d, "docker-a", f.a.stateDir)
	if a2.identity.RuntimeBrokerID != f.a.identity.RuntimeBrokerID {
		t.Fatalf("identity changed across restart: %s -> %s", f.a.identity.RuntimeBrokerID, a2.identity.RuntimeBrokerID)
	}
	if rec, ok, err := a2.srv.ownership.Get("proj-1", "agent-a1"); err != nil || !ok || !rec.OwnsUID("cid-a1") {
		t.Fatalf("A's record after restart: %+v %v %v", rec, ok, err)
	}
	if got := listedContainers(t, a2, ""); len(got) != 1 || got[0] != "cid-a1" {
		t.Fatalf("A lists %v after restart", got)
	}
	if a2.srv.stateDir != f.a.stateDir {
		t.Fatalf("restart state root %q, want %q", a2.srv.stateDir, f.a.stateDir)
	}
	assertCalls(t, f.d)
}

// TestFlatPartition_SingletonReplacementOnOneScope: instance A is replaced
// by a new instance C on the same daemon (A no longer configured). A's
// labelled objects stay A's: C neither lists nor acts on them, and creating
// C's records never adopts them.
func TestFlatPartition_SingletonReplacementOnOneScope(t *testing.T) {
	f := newPartitionFixture(t)
	c := newPartitionInstance(t, f.d, "docker-c", t.TempDir())
	if got := listedContainers(t, c, ""); len(got) != 0 {
		t.Fatalf("C lists %v, want nothing (A's and B's objects are theirs)", got)
	}
	for _, o := range []struct{ method, path string }{
		{http.MethodPost, "/api/v1/agents/worker/stop?projectId=proj-1"},
		{http.MethodDelete, "/api/v1/agents/worker?projectId=proj-1"},
		{http.MethodGet, "/api/v1/agents/worker/logs?projectId=proj-1"},
	} {
		serveFlat(c.srv, o.method, o.path, "")
	}
	assertCalls(t, f.d)
	if recs, err := c.srv.ownership.List(); err != nil || len(recs) != 0 {
		t.Fatalf("C recorded %d agents (%v); A's objects must not be adopted", len(recs), err)
	}
}

// TestFlatPartition_DispatchAttemptsArePerInstance: a request ID A has
// recorded is unknown to B (separate state roots), and A still knows it
// after a restart.
func TestFlatPartition_DispatchAttemptsArePerInstance(t *testing.T) {
	f := newPartitionFixture(t)
	if a, _ := f.a.srv.beginCreateAttempt("req-1", "agent-a9"); a == nil {
		t.Fatal("A did not record the attempt")
	}
	if _, prior := f.b.srv.beginCreateAttempt("req-1", "agent-b9"); prior != nil {
		t.Fatalf("B sees A's attempt: %+v", prior)
	}
	a2 := newPartitionInstance(t, f.d, "docker-a", f.a.stateDir)
	if _, prior := a2.srv.beginCreateAttempt("req-1", "agent-a9"); prior == nil {
		t.Fatal("A lost its attempt across a restart")
	}
}
