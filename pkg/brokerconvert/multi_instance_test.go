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

//go:build !windows

package brokerconvert

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/capacityplan"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

const (
	multiPlantedSecret     = "PLANTED-MULTI-SECRET-99"
	multiPlantedKubeconfig = "/home/operator/PLANTED-KUBECONFIG-99"
	multiPlantedContext    = "PLANTED-CONTEXT-PROD-99"
)

const multiDockerAndK8sSettings = `schema_version: "1"
active_profile: build
server:
  broker:
    broker_id: ` + applyLegacyID + `
runtimes:
  docker:
    type: docker
  cluster:
    type: kubernetes
    context: ` + multiPlantedContext + `
    namespace: scion-agents
    env:
      KUBECONFIG: ` + multiPlantedKubeconfig + `
profiles:
  build:
    runtime: docker
    image_registry: reg.example/build
    harness_overrides:
      claude:
        env:
          API_TOKEN: ` + multiPlantedSecret + `
  k8s:
    runtime: cluster
    image_registry: reg.example/k8s
    harness_overrides:
      claude:
        env:
          API_TOKEN: ` + multiPlantedSecret + `
`

// multiApplyHub is a stateful multi-instance Hub supporting N dynamically
// registered flat Runtime Broker instances alongside the legacy Runtime Broker.
type multiApplyHub struct {
	*httptest.Server
	t *testing.T

	mu          sync.Mutex
	writes      []string
	order       []string
	legacy      map[string]any
	maxAgents   any
	canUpdate   bool
	settingsErr int
	instances   map[string]map[string]any // brokerID -> broker row
	instMax     map[string]*int64         // brokerID -> explicit maxAgents
	instRev     map[string]int64          // brokerID -> settings revision
	instPuts    map[string]int            // brokerID -> PutMaxAgents calls
	agents      map[string][]map[string]any
	projects    map[string]*applyProject
	brokers     []map[string]any // pre-existing unrelated brokers
	failNext    map[string]int   // "METHOD path" -> HTTP status (once)
	onRequest   func(method, path string) int
}

func newMultiApplyHub(t *testing.T) *multiApplyHub {
	t.Helper()
	h := &multiApplyHub{
		t: t,
		legacy: map[string]any{
			"id":     applyLegacyID,
			"name":   "legacy-host",
			"status": "offline",
			"profiles": []map[string]any{
				{"name": "build", "type": "docker"},
				{"name": "k8s", "type": "kubernetes"},
			},
		},
		maxAgents: 11,
		canUpdate: true,
		instances: map[string]map[string]any{},
		instMax:   map[string]*int64{},
		instRev:   map[string]int64{},
		instPuts:  map[string]int{},
		agents: map[string][]map[string]any{
			// Historical terminal agent on legacy broker: does not block
			// drain checks and remains readable throughout conversion/rollback.
			applyLegacyID: {
				{"id": "a-hist-1", "name": "past-job", "projectId": "p-1", "project": "web", "phase": "stopped", "appliedConfig": map[string]any{"profile": "build"}},
			},
		},
		projects: map[string]*applyProject{
			"p-1": {name: "web", defaultID: applyLegacyID, activeProfile: "build", providers: map[string]string{applyLegacyID: ""}},
			"p-2": {name: "worker", defaultID: applyLegacyID, activeProfile: "k8s", providers: map[string]string{applyLegacyID: ""}},
			"p-3": {name: "fallback", defaultID: applyLegacyID, activeProfile: "", providers: map[string]string{applyLegacyID: ""}},
			"p-4": {name: "external", defaultID: "b-other", activeProfile: "k8s", providers: map[string]string{applyLegacyID: "", "b-other": ""}},
		},
		failNext: map[string]int{},
	}
	h.Server = httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(h.Close)
	return h
}

func (h *multiApplyHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()

	key := r.Method + " " + r.URL.Path
	if r.Method != http.MethodGet {
		h.writes = append(h.writes, key)
	}
	if h.onRequest != nil {
		if st := h.onRequest(r.Method, r.URL.Path); st != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"error":{"code":"injected","message":"injected PLANTED-HUB-MSG"}}`))
			return
		}
	}
	if st, ok := h.failNext[key]; ok {
		delete(h.failNext, key)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(`{"error":{"code":"injected","message":"injected PLANTED-HUB-MSG"}}`))
		return
	}

	reply := func(body any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	notFound := func() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
	}

	p := r.URL.Path
	const rb = "/api/v1/runtime-brokers"
	switch {
	case p == rb && r.Method == http.MethodGet:
		name := r.URL.Query().Get("name")
		var out []map[string]any
		for _, b := range h.brokers {
			if name == "" || strings.EqualFold(b["name"].(string), name) {
				out = append(out, b)
			}
		}
		ids := make([]string, 0, len(h.instances))
		for id := range h.instances {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			b := h.instances[id]
			if name == "" || strings.EqualFold(b["name"].(string), name) {
				out = append(out, b)
			}
		}
		reply(map[string]any{"brokers": out})

	case p == rb+"/"+applyLegacyID && r.Method == http.MethodGet:
		reply(h.legacy)

	case p == rb+"/"+applyLegacyID+"/settings" && r.Method == http.MethodGet:
		if h.settingsErr != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(h.settingsErr)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"no"}}`))
			return
		}
		reply(map[string]any{
			"brokerId":      applyLegacyID,
			"settings":      map[string]any{"maxAgents": h.maxAgents},
			"effective":     map[string]any{"maxAgents": map[string]any{"value": h.maxAgents, "source": "broker", "count": 0, "inherited": map[string]any{"value": 25, "source": "default"}}},
			"_capabilities": map[string]any{"update": h.canUpdate},
		})

	case p == rb+"/"+applyLegacyID+"/projects" && r.Method == http.MethodGet:
		var ps []map[string]any
		ids := make([]string, 0, len(h.projects))
		for id := range h.projects {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if _, ok := h.projects[id].providers[applyLegacyID]; ok {
				ps = append(ps, map[string]any{"projectId": id, "projectName": h.projects[id].name})
			}
		}
		reply(map[string]any{"projects": ps})

	case strings.HasPrefix(p, rb+"/"):
		rest := strings.Split(strings.TrimPrefix(p, rb+"/"), "/")
		brokerID := rest[0]
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			row := h.instances[brokerID]
			if row == nil {
				notFound()
				return
			}
			reply(row)
		case len(rest) == 1 && r.Method == http.MethodDelete:
			if h.instances[brokerID] == nil {
				notFound()
				return
			}
			delete(h.instances, brokerID)
			delete(h.instMax, brokerID)
			delete(h.instRev, brokerID)
			for _, pr := range h.projects {
				delete(pr.providers, brokerID)
			}
			h.order = append(h.order, "deregister:"+brokerID)
			w.WriteHeader(http.StatusNoContent)
		case len(rest) == 2 && rest[1] == "settings" && r.Method == http.MethodGet:
			if h.instances[brokerID] == nil {
				notFound()
				return
			}
			settings := map[string]any{}
			if v := h.instMax[brokerID]; v != nil {
				settings["maxAgents"] = *v
			}
			reply(map[string]any{
				"brokerId":      brokerID,
				"settings":      settings,
				"revision":      h.instRev[brokerID],
				"_capabilities": map[string]any{"update": h.canUpdate},
			})
		case len(rest) == 2 && rest[1] == "settings" && r.Method == http.MethodPut:
			if h.instances[brokerID] == nil {
				notFound()
				return
			}
			var body struct {
				Settings struct {
					MaxAgents *int64 `json:"maxAgents"`
				} `json:"settings"`
				ExpectedRevision int64 `json:"expectedRevision"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ExpectedRevision != h.instRev[brokerID] {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"revision_conflict","message":"stale"}`))
				return
			}
			h.instPuts[brokerID]++
			h.instMax[brokerID] = body.Settings.MaxAgents
			h.instRev[brokerID]++
			h.order = append(h.order, "capacity:"+brokerID)
			reply(map[string]any{
				"brokerId": brokerID,
				"settings": map[string]any{"maxAgents": h.instMax[brokerID]},
				"revision": h.instRev[brokerID],
			})
		default:
			h.t.Errorf("unexpected broker request %s", key)
			notFound()
		}

	case p == "/api/v1/agents" && r.Method == http.MethodGet:
		reply(map[string]any{"agents": h.agents[r.URL.Query().Get("runtimeBrokerId")]})

	case strings.HasPrefix(p, "/api/v1/projects/"):
		h.serveProject(w, r, strings.Split(strings.TrimPrefix(p, "/api/v1/projects/"), "/"), reply, notFound)

	default:
		h.t.Errorf("unexpected request %s", key)
		notFound()
	}
}

func (h *multiApplyHub) serveProject(w http.ResponseWriter, r *http.Request, parts []string, reply func(any), notFound func()) {
	pr := h.projects[parts[0]]
	if pr == nil {
		notFound()
		return
	}
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		reply(map[string]any{"id": parts[0], "name": pr.name, "defaultRuntimeBrokerId": pr.defaultID})
	case len(parts) == 1:
		var body struct {
			DefaultRuntimeBrokerID string `json:"defaultRuntimeBrokerId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.DefaultRuntimeBrokerID != "" {
			pr.defaultID = body.DefaultRuntimeBrokerID
		}
		h.order = append(h.order, "default:"+parts[0]+":"+pr.defaultID)
		reply(map[string]any{"id": parts[0], "defaultRuntimeBrokerId": pr.defaultID})
	case len(parts) == 2 && parts[1] == "settings":
		body := map[string]any{}
		if pr.activeProfile != "" {
			body["activeProfile"] = pr.activeProfile
		}
		reply(body)
	case len(parts) == 2 && parts[1] == "providers" && r.Method == http.MethodGet:
		var out []map[string]any
		ids := make([]string, 0, len(pr.providers))
		for id := range pr.providers {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, map[string]any{"brokerId": id, "localPath": pr.providers[id]})
		}
		reply(map[string]any{"providers": out})
	case len(parts) == 2 && parts[1] == "providers" && r.Method == http.MethodPost:
		var body struct {
			BrokerID string `json:"brokerId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pr.providers[body.BrokerID] = ""
		h.order = append(h.order, "link:"+parts[0]+":"+body.BrokerID)
		reply(map[string]any{"provider": map[string]any{"brokerId": body.BrokerID}})
	case len(parts) == 3 && parts[1] == "providers" && r.Method == http.MethodDelete:
		if _, ok := pr.providers[parts[2]]; !ok {
			notFound()
			return
		}
		delete(pr.providers, parts[2])
		h.order = append(h.order, "unlink:"+parts[0]+":"+parts[2])
		w.WriteHeader(http.StatusNoContent)
	default:
		h.t.Errorf("unexpected project request %s %v", r.Method, parts)
		notFound()
	}
}

func (h *multiApplyHub) client(t *testing.T) hubclient.Client {
	t.Helper()
	c, err := hubclient.New(h.URL)
	require.NoError(t, err)
	return c
}

func (h *multiApplyHub) writeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.writes)
}

type multiApplyFixture struct {
	t          *testing.T
	g          string
	hub        *multiApplyHub
	env        *Env
	choices    []config.ConversionChoice
	running    bool
	regCalls   []string
	regHook    func(inst config.V1RuntimeBrokerInstanceConfig) error
	afterRowFn func(inst config.V1RuntimeBrokerInstanceConfig, id *brokeridentity.Identity) error
}

func newMultiApplyFixture(t *testing.T, rawSettings string) *multiApplyFixture {
	t.Helper()
	g := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(g, "settings.yaml"), []byte(rawSettings), 0o640))
	require.NoError(t, os.Chmod(filepath.Join(g, "settings.yaml"), 0o640))

	f := &multiApplyFixture{
		t:   t,
		g:   g,
		hub: newMultiApplyHub(t),
	}
	f.env = &Env{
		GlobalDir: g,
		Client:    f.hub.client(t),
		Register:  f.registerInstance,
		ServerRunning: func(context.Context) bool {
			return f.running
		},
		LoadInstances: func() ([]config.V1RuntimeBrokerInstanceConfig, error) {
			data, err := os.ReadFile(filepath.Join(g, "settings.yaml"))
			if err != nil {
				return nil, err
			}
			var vs config.VersionedSettings
			if err := yamlv3.Unmarshal(data, &vs); err != nil {
				return nil, err
			}
			if vs.Server == nil || vs.Server.Broker == nil {
				return nil, nil
			}
			return vs.Server.Broker.Instances, nil
		},
		ReadSettings:    func() ([]byte, error) { return os.ReadFile(filepath.Join(g, "settings.yaml")) },
		WriteInstances:  func(i []config.V1RuntimeBrokerInstanceConfig) error { return config.WriteRuntimeBrokerInstances(g, i) },
		RemoveInstances: func(e []config.V1RuntimeBrokerInstanceConfig) error { return config.RemoveRuntimeBrokerInstances(g, e) },
		Now:             func() time.Time { return testNow },
	}
	return f
}

func observedScopeFor(inst config.V1RuntimeBrokerInstanceConfig) (string, brokeridentity.ExecutionScope) {
	tType := inst.RuntimeTarget.Type
	switch tType {
	case brokeridentity.TargetTypeKubernetes:
		ns := "default"
		if inst.RuntimeTarget != nil && inst.RuntimeTarget.Namespace != "" {
			ns = inst.RuntimeTarget.Namespace
		}
		return tType, brokeridentity.ExecutionScope{
			Type: brokeridentity.TargetTypeKubernetes,
			Kubernetes: &brokeridentity.KubernetesScope{
				ClusterUID: "uid-cluster-1",
				Namespace:  ns,
			},
		}
	default:
		return brokeridentity.TargetTypeDocker, brokeridentity.ExecutionScope{
			Type: brokeridentity.TargetTypeDocker,
			Docker: &brokeridentity.DockerScope{
				DaemonID: "daemon-1",
			},
		}
	}
}

func (f *multiApplyFixture) registerInstance(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
	f.regCalls = append(f.regCalls, inst.Key)
	if f.regHook != nil {
		if err := f.regHook(inst); err != nil {
			return "", "", err
		}
	}
	tType, scope := observedScopeFor(inst)
	dir := brokeridentity.InstanceDir(f.g, inst.Key)
	id, err := brokeridentity.LoadOrCreate(dir, inst.Key, tType, scope, []string{applyLegacyID})
	if err != nil {
		return "", "", err
	}
	f.hub.mu.Lock()
	f.hub.instances[id.RuntimeBrokerID] = map[string]any{
		"id":              id.RuntimeBrokerID,
		"name":            inst.Name,
		"status":          "offline",
		"runtimeTargetId": id.RuntimeTarget.ID,
	}
	f.hub.order = append(f.hub.order, "register:"+inst.Key+":"+id.RuntimeBrokerID)
	f.hub.mu.Unlock()
	if f.afterRowFn != nil {
		if err := f.afterRowFn(inst, id); err != nil {
			return "", "", err
		}
	}
	return id.RuntimeBrokerID, id.RuntimeTarget.ID, nil
}

func (f *multiApplyFixture) buildInput() (*Plan, ApplyInput) {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.g, "settings.yaml"))
	require.NoError(f.t, err)
	vs := &config.VersionedSettings{}
	require.NoError(f.t, yamlv3.Unmarshal(raw, vs))
	in := Input{Settings: vs, RawSettings: raw, Choices: f.choices}
	if vs.Server != nil && vs.Server.Broker != nil && len(vs.Server.Broker.Instances) > 0 {
		j, jerr := LoadJournal(f.g)
		require.NoError(f.t, jerr)
		require.NotNil(f.t, j)
		ev, rerr := ReadRunCopy(f.g, j.EvidenceFile)
		require.NoError(f.t, rerr)
		vsCopy := *vs
		srv := *vsCopy.Server
		brk := *srv.Broker
		brk.Instances = nil
		srv.Broker = &brk
		vsCopy.Server = &srv
		in.Settings = &vsCopy
		in.RawSettings = ev
	}
	p, err := BuildLocal(in)
	require.NoError(f.t, err)
	require.NoError(f.t, CollectHub(context.Background(), f.env.Client, applyLegacyID, p))
	return p, ApplyInput{
		Plan:                 p,
		Choices:              f.choices,
		LegacyDefaultProfile: vs.ActiveProfile,
	}
}

func (f *multiApplyFixture) apply() (*Report, error) {
	f.t.Helper()
	_, in := f.buildInput()
	return Apply(context.Background(), f.env, in)
}

// Rehearsal 1: Multi-instance remote --apply on one host with both Docker and
// Kubernetes profiles, uneven capacity split (11 -> 6 + 5), distinct project
// default routing by active profile + legacy default fallback, Status Switched
// requiring ALL instances online, idempotent repeat --apply, and redaction.
func TestMultiInstanceApply_DockerAndKubernetes_ConvertsIdempotently(t *testing.T) {
	f := newMultiApplyFixture(t, multiDockerAndK8sSettings)

	planBefore, _ := f.buildInput()
	plannedInsts := planBefore.SettingsInstances()
	require.Len(t, plannedInsts, 2)

	var dockerKey, k8sKey string
	for _, inst := range plannedInsts {
		switch inst.RuntimeTarget.Type {
		case brokeridentity.TargetTypeDocker:
			dockerKey = inst.Key
		case brokeridentity.TargetTypeKubernetes:
			k8sKey = inst.Key
		}
	}
	require.NotEmpty(t, dockerKey)
	require.NotEmpty(t, k8sKey)
	require.NotEqual(t, dockerKey, k8sKey)

	rep, err := f.apply()
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, rep.State)
	assert.Contains(t, rep.Notes, NoteEvidenceKept)
	assert.Contains(t, rep.Notes, NoteLegacyLinks)
	assert.Contains(t, rep.Notes, NoteSwitch)
	require.Len(t, rep.Instances, 2)

	brokerByKey := map[string]string{}
	targetByKey := map[string]string{}
	for _, line := range rep.Instances {
		assert.Equal(t, StepDone, line.State)
		assert.NotEmpty(t, line.BrokerID)
		assert.NotEmpty(t, line.RuntimeTargetID)
		brokerByKey[line.Key] = line.BrokerID
		targetByKey[line.Key] = line.RuntimeTargetID
	}
	dockerID := brokerByKey[dockerKey]
	k8sID := brokerByKey[k8sKey]
	require.NotEmpty(t, dockerID)
	require.NotEmpty(t, k8sID)
	assert.NotEqual(t, dockerID, k8sID, "each flat instance mints a distinct Runtime Broker ID")
	assert.NotEqual(t, targetByKey[dockerKey], targetByKey[k8sKey], "each flat instance mints a distinct runtime target ID")

	// Per-instance capacity split: 11 across 2 sorted keys -> 6 and 5, sum == 11.
	require.NotNil(t, f.hub.instMax[dockerID])
	require.NotNil(t, f.hub.instMax[k8sID])
	assert.Positive(t, *f.hub.instMax[dockerID])
	assert.Positive(t, *f.hub.instMax[k8sID])
	assert.Equal(t, int64(11), *f.hub.instMax[dockerID]+*f.hub.instMax[k8sID])
	assert.Equal(t, 1, f.hub.instPuts[dockerID])
	assert.Equal(t, 1, f.hub.instPuts[k8sID])

	// Ordering invariant: both registrations and both capacity writes precede any project link/default write.
	firstProjIdx := -1
	lastCapIdx := -1
	for i, op := range f.hub.order {
		if strings.HasPrefix(op, "capacity:") {
			lastCapIdx = i
		}
		if (strings.HasPrefix(op, "link:") || strings.HasPrefix(op, "default:")) && firstProjIdx == -1 {
			firstProjIdx = i
		}
	}
	require.GreaterOrEqual(t, lastCapIdx, 0)
	require.Greater(t, firstProjIdx, lastCapIdx, "all per-instance capacities are written before step 3 repoints projects")

	// Settings written with both instances; legacy runtimes/profiles preserved.
	written, err := f.env.LoadInstances()
	require.NoError(t, err)
	require.Len(t, written, 2)
	assert.True(t, config.InstancesEqual(plannedInsts, written))

	// Every project links BOTH flat instances and retains the legacy link;
	// defaults route to the active profile's instance (or legacyDefaultProfile fallback).
	for _, pid := range []string{"p-1", "p-2", "p-3", "p-4"} {
		pr := f.hub.projects[pid]
		assert.Contains(t, pr.providers, dockerID, "project %s links docker instance", pid)
		assert.Contains(t, pr.providers, k8sID, "project %s links kubernetes instance", pid)
		assert.Contains(t, pr.providers, applyLegacyID, "project %s keeps legacy link", pid)
	}
	assert.Equal(t, dockerID, f.hub.projects["p-1"].defaultID, "activeProfile=build -> docker instance")
	assert.Equal(t, k8sID, f.hub.projects["p-2"].defaultID, "activeProfile=k8s -> kubernetes instance")
	assert.Equal(t, dockerID, f.hub.projects["p-3"].defaultID, "unset activeProfile -> legacyDefaultProfile (build) -> docker instance")
	assert.Equal(t, "b-other", f.hub.projects["p-4"].defaultID, "default on another broker is untouched")

	// Historical terminal agent on legacy broker remains intact and readable.
	resp, err := f.env.Client.Agents().List(context.Background(), &hubclient.ListAgentsOptions{RuntimeBrokerID: applyLegacyID})
	require.NoError(t, err)
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "a-hist-1", resp.Agents[0].ID)

	// Status reports Switched == true ONLY when EVERY flat instance is online.
	stRep, err := Status(context.Background(), f.env)
	require.NoError(t, err)
	assert.False(t, stRep.Switched, "both offline -> not switched")

	f.hub.mu.Lock()
	f.hub.instances[dockerID]["status"] = "online"
	f.hub.mu.Unlock()
	stRep, err = Status(context.Background(), f.env)
	require.NoError(t, err)
	assert.False(t, stRep.Switched, "only 1 of 2 online -> not switched")

	f.hub.mu.Lock()
	f.hub.instances[k8sID]["status"] = "online"
	f.hub.mu.Unlock()
	stRep, err = Status(context.Background(), f.env)
	require.NoError(t, err)
	assert.True(t, stRep.Switched, "all flat instances online -> switched")

	// Repeat --apply is a strict no-op: zero Hub writes, zero registrations.
	writesBefore := f.hub.writeCount()
	regBefore := len(f.regCalls)
	rep2, err := f.apply()
	require.NoError(t, err)
	assert.Contains(t, rep2.Notes, NoteNoop)
	assert.Equal(t, writesBefore, f.hub.writeCount(), "repeat --apply sends zero Hub writes")
	assert.Equal(t, regBefore, len(f.regCalls), "repeat --apply does not re-register")

	// Security / redaction invariant (sec R2): no planted secret, kubeconfig path
	// or context alias appears in any report format or in the journal.
	for _, r := range []*Report{rep, stRep, rep2} {
		var hBuf, jBuf bytes.Buffer
		require.NoError(t, WriteReportHuman(&hBuf, r))
		require.NoError(t, WriteReportJSON(&jBuf, r))
		for _, needle := range []string{multiPlantedSecret, multiPlantedKubeconfig, multiPlantedContext, "API_TOKEN"} {
			assert.NotContains(t, hBuf.String(), needle)
			assert.NotContains(t, jBuf.String(), needle)
		}
	}
	journalRaw, err := os.ReadFile(filepath.Join(ConvertDir(f.g), JournalFileName))
	require.NoError(t, err)
	for _, needle := range []string{multiPlantedSecret, multiPlantedKubeconfig, multiPlantedContext, "API_TOKEN"} {
		assert.NotContains(t, string(journalRaw), needle)
	}
}

// Rehearsal 2: Split-profile Kubernetes instances via --choices, OutcomeNoLimit
// writing explicit 0 to every split instance, repeat --apply idempotence,
// changed-choices refusal, and full multi-instance rollback.
func TestMultiInstanceApply_SplitProfileKubernetesWithChoices(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "split_choice.yaml"))
	require.NoError(t, err)
	settings := string(raw) + "server:\n  broker:\n    broker_id: " + applyLegacyID + "\n"

	f := newMultiApplyFixture(t, settings)
	choices, err := ParseChoices([]byte("- target_group: k8s-agents\n  split: [fast]\n"))
	require.NoError(t, err)
	f.choices = choices
	f.hub.maxAgents = 0 // OutcomeNoLimit -> explicit 0 on every split instance
	f.hub.legacy["profiles"] = []map[string]any{
		{"name": "fast", "type": "kubernetes"},
		{"name": "slow", "type": "kubernetes"},
		{"name": "other", "type": "kubernetes"},
	}
	f.hub.projects = map[string]*applyProject{
		"p-fast": {name: "fast-svc", defaultID: applyLegacyID, activeProfile: "fast", providers: map[string]string{applyLegacyID: ""}},
		"p-slow": {name: "slow-svc", defaultID: applyLegacyID, activeProfile: "slow", providers: map[string]string{applyLegacyID: ""}},
	}

	beforeSettings := settingsBytes(t, f.g)
	rep, err := f.apply()
	require.NoError(t, err)
	assert.Equal(t, JournalApplied, rep.State)
	require.Len(t, rep.Instances, 2)

	byKey := map[string]string{}
	for _, in := range rep.Instances {
		byKey[in.Key] = in.BrokerID
		require.NotNil(t, f.hub.instMax[in.BrokerID], "explicit 0 written for %s", in.Key)
		assert.Equal(t, int64(0), *f.hub.instMax[in.BrokerID])
	}
	require.Contains(t, byKey, "k8s-agents")
	require.Contains(t, byKey, "k8s-agents-fast")
	assert.NotEqual(t, byKey["k8s-agents"], byKey["k8s-agents-fast"])

	assert.Equal(t, byKey["k8s-agents-fast"], f.hub.projects["p-fast"].defaultID)
	assert.Equal(t, byKey["k8s-agents"], f.hub.projects["p-slow"].defaultID)
	for _, pid := range []string{"p-fast", "p-slow"} {
		assert.Contains(t, f.hub.projects[pid].providers, byKey["k8s-agents"])
		assert.Contains(t, f.hub.projects[pid].providers, byKey["k8s-agents-fast"])
	}

	// Repeat --apply with identical choices is a no-op.
	writes := f.hub.writeCount()
	rep2, err := f.apply()
	require.NoError(t, err)
	assert.Contains(t, rep2.Notes, NoteNoop)
	assert.Equal(t, writes, f.hub.writeCount())

	// Changing --choices after apply refuses with RefusePlanChanged.
	otherChoices, err := ParseChoices([]byte("- target_group: k8s-agents\n  split: [other]\n"))
	require.NoError(t, err)
	f.choices = otherChoices
	_, err = f.apply()
	require.Error(t, err)
	assert.Equal(t, RefusePlanChanged, refusalCode(t, err))
	f.choices = choices

	// Full rollback restores settings, project defaults/links, Hub rows, and moves both identities aside.
	rbRep, err := Rollback(context.Background(), f.env)
	require.NoError(t, err)
	assert.Equal(t, JournalRolledBack, rbRep.State)
	assert.Equal(t, string(beforeSettings), string(settingsBytes(t, f.g)))
	assert.Empty(t, f.hub.instances)
	for _, pid := range []string{"p-fast", "p-slow"} {
		assert.Equal(t, applyLegacyID, f.hub.projects[pid].defaultID)
		assert.Equal(t, map[string]string{applyLegacyID: ""}, f.hub.projects[pid].providers)
	}
	for _, key := range []string{"k8s-agents", "k8s-agents-fast"} {
		_, err := os.Lstat(filepath.Join(f.g, "runtime-brokers", key))
		assert.True(t, errors.Is(err, fs.ErrNotExist), "%s moved out of active runtime-brokers dir", key)
		_, err = os.Lstat(filepath.Join(ConvertDir(f.g), RolledBackDirName, rbRep.RunID, key, "identity.json"))
		require.NoError(t, err, "%s identity preserved in rolled-back tree", key)
	}
}

// Rehearsal 3: Multi-instance default-broker ambiguity refuses in prechecks
// (RefuseDefaultUnknown) naming ALL ambiguous projects before any Hub or local write.
func TestMultiInstanceApply_AmbiguousProjectDefaultRefusesUpFront(t *testing.T) {
	// Remove top-level active_profile so there is no legacyDefaultProfile fallback.
	noDefaultProfile := strings.Replace(multiDockerAndK8sSettings, "active_profile: build\n", "", 1)
	f := newMultiApplyFixture(t, noDefaultProfile)
	f.hub.projects["p-3"].activeProfile = ""               // no active profile
	f.hub.projects["p-2"].activeProfile = "unknown-legacy" // unmapped profile

	before := settingsBytes(t, f.g)
	_, err := f.apply()
	require.Error(t, err)
	assert.Equal(t, RefuseDefaultUnknown, refusalCode(t, err))
	var ref *Refusal
	require.ErrorAs(t, err, &ref)
	assert.Equal(t, []string{"p-2", "p-3"}, ref.Projects, "names all ambiguous projects in sorted order")
	assert.Zero(t, f.hub.writeCount(), "zero Hub writes on ambiguous default")
	assert.Empty(t, f.regCalls, "zero registrations on ambiguous default")
	assert.Equal(t, before, settingsBytes(t, f.g), "settings untouched")
	_, err = os.Lstat(filepath.Join(ConvertDir(f.g), JournalFileName))
	assert.True(t, errors.Is(err, fs.ErrNotExist), "journal not created when precheck refuses")
}

// Rehearsal 4: Multi-instance capacity validation refusals and conservative split support.
func TestMultiInstanceApply_CapacityValidation(t *testing.T) {
	t.Run("empty plan refuses with plan_not_clean", func(t *testing.T) {
		f := newMultiApplyFixture(t, "schema_version: \"1\"\n")
		p, err := BuildLocal(Input{Settings: &config.VersionedSettings{SchemaVersion: "1"}, RawSettings: []byte("schema_version: \"1\"\n")})
		require.NoError(t, err)
		p.LegacyBrokerID = applyLegacyID
		p.Agents = &AgentsSection{Items: []AgentEntry{}}
		p.Projects = &ProjectsSection{Items: []ProjectEntry{}}
		p.Capacity = Capacity{Status: CapacityPlanned, Outcome: capacityplan.OutcomeNoLimit}

		_, err = Apply(context.Background(), f.env, ApplyInput{Plan: p})
		require.Error(t, err)
		assert.Equal(t, RefusePlanNotClean, refusalCode(t, err))
	})

	t.Run("limit below instance count refuses as blocked plan", func(t *testing.T) {
		f := newMultiApplyFixture(t, multiDockerAndK8sSettings)
		f.hub.maxAgents = 1 // 1 cannot split into 2 positive caps -> operator_decision_required
		_, err := f.apply()
		require.Error(t, err)
		assert.Equal(t, RefusePlanNotClean, refusalCode(t, err))
		assert.Zero(t, f.hub.writeCount())
	})

	t.Run("malformed capacity allocations refuse with capacity_split", func(t *testing.T) {
		f := newMultiApplyFixture(t, multiDockerAndK8sSettings)
		p, in := f.buildInput()
		insts := p.SettingsInstances()
		require.Len(t, insts, 2)
		ten := int64(10)

		badCases := []struct {
			name  string
			alloc []Allocation
		}{
			{"missing second instance", []Allocation{{Instance: insts[0].Key, MaxAgents: 10}}},
			{"unknown instance key", []Allocation{{Instance: insts[0].Key, MaxAgents: 5}, {Instance: "stranger", MaxAgents: 5}}},
			{"zero allocation", []Allocation{{Instance: insts[0].Key, MaxAgents: 0}, {Instance: insts[1].Key, MaxAgents: 10}}},
			{"sum exceeds legacy limit", []Allocation{{Instance: insts[0].Key, MaxAgents: 6}, {Instance: insts[1].Key, MaxAgents: 6}}},
		}
		for _, tc := range badCases {
			t.Run(tc.name, func(t *testing.T) {
				cp := *p
				cp.Capacity = Capacity{
					Status:      CapacityPlanned,
					Outcome:     capacityplan.OutcomePreserveBySplit,
					MaxAgents:   &ten,
					Allocations: tc.alloc,
				}
				inCopy := in
				inCopy.Plan = &cp
				_, err := Apply(context.Background(), f.env, inCopy)
				require.Error(t, err)
				assert.Equal(t, RefuseCapacitySplit, refusalCode(t, err))
				assert.Zero(t, f.hub.writeCount())
			})
		}
	})

	t.Run("conservative split below legacy cap applies exact per-instance allocations", func(t *testing.T) {
		f := newMultiApplyFixture(t, multiDockerAndK8sSettings)
		p, in := f.buildInput()
		insts := p.SettingsInstances()
		require.Len(t, insts, 2)
		ten := int64(10)
		p.Capacity = Capacity{
			Status:    CapacityPlanned,
			Outcome:   capacityplan.OutcomePreserveBySplit,
			MaxAgents: &ten,
			Allocations: []Allocation{
				{Instance: insts[0].Key, MaxAgents: 5},
				{Instance: insts[1].Key, MaxAgents: 3},
			},
		}
		in.Plan = p
		rep, err := Apply(context.Background(), f.env, in)
		require.NoError(t, err)
		assert.Equal(t, JournalApplied, rep.State)
		byKey := map[string]string{}
		for _, line := range rep.Instances {
			byKey[line.Key] = line.BrokerID
		}
		assert.Equal(t, int64(5), *f.hub.instMax[byKey[insts[0].Key]])
		assert.Equal(t, int64(3), *f.hub.instMax[byKey[insts[1].Key]])
	})
}

// Rehearsal 5: Mid-conversion interruptions across multiple instances.
// Every failure point is rehearsed along BOTH recovery paths:
//
//	(a) Rerun --apply converges idempotently without duplicate rows, puts or links.
//	(b) --rollback reverses every partial mutation cleanly (zero orphan rows,
//	    zero stranded provider links, legacy defaults and settings restored,
//	    created identity dirs moved aside) AND a subsequent fresh --apply succeeds.
func TestMultiInstanceRehearsal_InterruptionsResumeAndRollback(t *testing.T) {
	type faultPoint struct {
		name   string
		inject func(t *testing.T, f *multiApplyFixture, keys []string) (clearFault func())
		verify func(t *testing.T, f *multiApplyFixture)
	}

	faults := []faultPoint{
		{
			name: "instance_2_fails_before_row_creation",
			inject: func(t *testing.T, f *multiApplyFixture, keys []string) func() {
				fail := true
				f.regHook = func(inst config.V1RuntimeBrokerInstanceConfig) error {
					if fail && inst.Key == keys[1] {
						return errors.New("hub unavailable PLANTED-HUB-MSG")
					}
					return nil
				}
				return func() { fail = false }
			},
			verify: func(t *testing.T, f *multiApplyFixture) {
				j, err := LoadJournal(f.g)
				require.NoError(t, err)
				assert.Equal(t, StepDone, j.Register[0].State)
				assert.Equal(t, StepStarted, j.Register[1].State)
				assert.Empty(t, j.Register[1].BrokerID)
				assert.Len(t, f.hub.instances, 1, "only instance 1 row exists")
			},
		},
		{
			name: "instance_2_crashes_after_row_and_identity_before_journal_id",
			inject: func(t *testing.T, f *multiApplyFixture, keys []string) func() {
				fail := true
				f.afterRowFn = func(inst config.V1RuntimeBrokerInstanceConfig, _ *brokeridentity.Identity) error {
					if fail && inst.Key == keys[1] {
						return errors.New("join ack lost PLANTED-HUB-MSG")
					}
					return nil
				}
				return func() {
					// Simulate hard crash before register() saved BrokerID to journal.
					j, err := LoadJournal(f.g)
					require.NoError(t, err)
					j.Register[1].BrokerID = ""
					require.NoError(t, SaveJournal(f.g, j, testNow))
					fail = false
				}
			},
			verify: func(t *testing.T, f *multiApplyFixture) {
				assert.Len(t, f.hub.instances, 2, "both Hub rows exist despite crash")
			},
		},
		{
			name: "instance_2_capacity_put_fails_after_instance_1_capacity_done",
			inject: func(t *testing.T, f *multiApplyFixture, _ []string) func() {
				putCalls := 0
				fail := true
				f.hub.onRequest = func(method, path string) int {
					if fail && method == http.MethodPut && strings.HasSuffix(path, "/settings") {
						putCalls++
						if putCalls == 2 {
							return http.StatusInternalServerError
						}
					}
					return 0
				}
				return func() { fail = false }
			},
			verify: func(t *testing.T, f *multiApplyFixture) {
				j, err := LoadJournal(f.g)
				require.NoError(t, err)
				assert.Equal(t, StepDone, j.Register[0].Capacity.State)
				assert.Equal(t, StepStarted, j.Register[1].Capacity.State)
				assert.Equal(t, StepPending, j.Settings.State)
			},
		},
		{
			name: "interrupted_after_settings_written_before_projects",
			inject: func(t *testing.T, f *multiApplyFixture, _ []string) func() {
				origWrite := f.env.WriteInstances
				fail := true
				f.env.WriteInstances = func(insts []config.V1RuntimeBrokerInstanceConfig) error {
					if err := origWrite(insts); err != nil {
						return err
					}
					if fail {
						f.hub.failNext["GET /api/v1/agents"] = http.StatusServiceUnavailable
					}
					return nil
				}
				return func() { fail = false }
			},
			verify: func(t *testing.T, f *multiApplyFixture) {
				j, err := LoadJournal(f.g)
				require.NoError(t, err)
				assert.Equal(t, StepDone, j.Settings.State)
				assert.Empty(t, j.Projects)
			},
		},
		{
			name: "crash_mid_project_2_after_partial_provider_link_before_journal_save",
			inject: func(t *testing.T, f *multiApplyFixture, _ []string) func() {
				p2Links := 0
				fail := true
				f.hub.onRequest = func(method, path string) int {
					if fail && method == http.MethodPost && path == "/api/v1/projects/p-2/providers" {
						p2Links++
						if p2Links == 2 {
							return http.StatusInternalServerError
						}
					}
					return 0
				}
				return func() {
					// Also simulate hard crash where p-2's first Linked entry was not flushed to disk!
					j, err := LoadJournal(f.g)
					require.NoError(t, err)
					for i := range j.Projects {
						if j.Projects[i].ProjectID == "p-2" {
							j.Projects[i].Linked = []string{}
						}
					}
					require.NoError(t, SaveJournal(f.g, j, testNow))
					fail = false
				}
			},
			verify: func(t *testing.T, f *multiApplyFixture) {
				j, err := LoadJournal(f.g)
				require.NoError(t, err)
				require.GreaterOrEqual(t, len(j.Projects), 2)
				assert.Equal(t, StepDone, j.Projects[0].State)
				assert.Equal(t, StepStarted, j.Projects[1].State)
				assert.Greater(t, len(f.hub.projects["p-2"].providers), 1, "first flat link was added on Hub before crash")
			},
		},
	}

	for _, fp := range faults {
		t.Run(fp.name+"/resume_apply_converges", func(t *testing.T) {
			f := newMultiApplyFixture(t, multiDockerAndK8sSettings)
			p0, _ := f.buildInput()
			var keys []string
			for _, inst := range p0.SettingsInstances() {
				keys = append(keys, inst.Key)
			}
			require.Len(t, keys, 2)

			clearFault := fp.inject(t, f, keys)
			_, err := f.apply()
			require.Error(t, err)
			fp.verify(t, f)
			clearFault()

			rep, err := f.apply()
			require.NoError(t, err)
			assert.Equal(t, JournalApplied, rep.State)
			assert.Len(t, f.hub.instances, 2, "converges to exactly 2 Hub broker rows")

			for id := range f.hub.instances {
				assert.Equal(t, 1, f.hub.instPuts[id], "capacity written once for %s", id)
				for _, pid := range []string{"p-1", "p-2", "p-3", "p-4"} {
					assert.Contains(t, f.hub.projects[pid].providers, id)
				}
			}
		})

		t.Run(fp.name+"/rollback_reverses_cleanly_and_fresh_apply_succeeds", func(t *testing.T) {
			f := newMultiApplyFixture(t, multiDockerAndK8sSettings)
			before := settingsBytes(t, f.g)
			p0, _ := f.buildInput()
			var keys []string
			for _, inst := range p0.SettingsInstances() {
				keys = append(keys, inst.Key)
			}
			require.Len(t, keys, 2)

			clearFault := fp.inject(t, f, keys)
			_, err := f.apply()
			require.Error(t, err)
			fp.verify(t, f)
			clearFault()

			rbRep, err := Rollback(context.Background(), f.env)
			require.NoError(t, err)
			assert.Equal(t, JournalRolledBack, rbRep.State)
			assert.Empty(t, f.hub.instances, "zero orphan flat broker rows left on Hub")
			assert.Equal(t, string(before), string(settingsBytes(t, f.g)), "settings restored byte-for-byte")

			for _, pid := range []string{"p-1", "p-2", "p-3"} {
				assert.Equal(t, applyLegacyID, f.hub.projects[pid].defaultID)
				assert.Equal(t, map[string]string{applyLegacyID: ""}, f.hub.projects[pid].providers, "project %s has zero stranded flat links", pid)
			}
			assert.Equal(t, "b-other", f.hub.projects["p-4"].defaultID)
			assert.Equal(t, map[string]string{applyLegacyID: "", "b-other": ""}, f.hub.projects["p-4"].providers)

			for _, key := range keys {
				_, err := os.Lstat(filepath.Join(f.g, "runtime-brokers", key))
				assert.True(t, errors.Is(err, fs.ErrNotExist), "active identity dir removed for %s", key)
			}

			// A fresh --apply after rollback mints clean identities and succeeds!
			freshRep, err := f.apply()
			require.NoError(t, err)
			assert.Equal(t, JournalApplied, freshRep.State)
			assert.NotEqual(t, rbRep.RunID, freshRep.RunID)
			assert.Len(t, f.hub.instances, 2)
		})
	}
}

// Rehearsal 6: Rollback refuses cleanly when ANY of N flat instances has a
// non-terminal agent, before deregistering sibling instances or modifying projects.
func TestMultiInstanceRollback_RefusesWhenAnyInstanceHasActiveAgent(t *testing.T) {
	f := newMultiApplyFixture(t, multiDockerAndK8sSettings)
	rep, err := f.apply()
	require.NoError(t, err)
	require.Len(t, rep.Instances, 2)

	firstID := rep.Instances[0].BrokerID
	secondID := rep.Instances[1].BrokerID

	// Put a running agent ONLY on the second flat instance; first instance is drained.
	f.hub.agents[secondID] = []map[string]any{
		{"id": "a-k8s-live", "name": "k8s-worker", "projectId": "p-2", "project": "worker", "phase": "running"},
	}
	writesBefore := f.hub.writeCount()
	settingsBefore := settingsBytes(t, f.g)

	_, err = Rollback(context.Background(), f.env)
	require.Error(t, err)
	assert.Equal(t, RefuseInstanceDrained, refusalCode(t, err))
	var ref *Refusal
	require.ErrorAs(t, err, &ref)
	require.Len(t, ref.Agents, 1)
	assert.Equal(t, "a-k8s-live", ref.Agents[0].ID)

	assert.Equal(t, writesBefore, f.hub.writeCount(), "zero Hub writes when sibling instance is not drained")
	assert.Contains(t, f.hub.instances, firstID, "first instance was NOT deregistered")
	assert.Contains(t, f.hub.instances, secondID, "second instance was NOT deregistered")
	assert.Equal(t, settingsBefore, settingsBytes(t, f.g))

	// When BOTH instances have running agents, all blocking agents are reported together.
	f.hub.agents[firstID] = []map[string]any{
		{"id": "a-docker-live", "name": "docker-worker", "projectId": "p-1", "project": "web", "phase": "provisioning"},
	}
	_, err = Rollback(context.Background(), f.env)
	require.Error(t, err)
	require.ErrorAs(t, err, &ref)
	assert.Len(t, ref.Agents, 2)
}

// Rehearsal 7: Multi-instance co-located two-phase --apply (including mid-running
// interruption, stopped-phase local plan, and staggered per-instance credentials
// cleanup) and two-phase co-located --rollback across multiple instances.
func TestMultiInstanceColocated_TwoPhaseApplyAndRollback(t *testing.T) {
	newColo := func(t *testing.T) (*multiApplyFixture, *bool, *time.Time, map[string]bool, *[]string, func() *ColocatedEnv) {
		t.Helper()
		f := newMultiApplyFixture(t, multiDockerAndK8sSettings)
		serverUp := true
		now := testNow
		onlineByID := map[string]bool{}
		var removed []string

		f.env.Now = func() time.Time { return now }
		baseReg := f.env.Register
		f.env.Register = func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (string, string, error) {
			bid, rtid, err := baseReg(ctx, inst)
			if err != nil {
				return bid, rtid, err
			}
			cdir := filepath.Join(f.g, "runtime-brokers", inst.Key, "hub-credentials")
			require.NoError(t, os.MkdirAll(cdir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(cdir, "hub.json"), []byte(`{"secretKey":"`+plantedJoinSecret+`"}`), 0o600))
			return bid, rtid, nil
		}
		ce := func() *ColocatedEnv {
			return &ColocatedEnv{
				ServerUp: func(context.Context) bool { return serverUp },
				Q7: func(context.Context, *Journal) Q7Input {
					return Q7Input{Topology: TopologySingleSQLite, LegacyStore: drainedStore}
				},
				InstanceOnline: func(_ context.Context, brokerID string) (bool, error) {
					return onlineByID[brokerID], nil
				},
				RemoveCredentials: func(key string) error {
					removed = append(removed, key)
					return os.RemoveAll(filepath.Join(f.g, "runtime-brokers", key, "hub-credentials"))
				},
			}
		}
		return f, &serverUp, &now, onlineByID, &removed, ce
	}

	t.Run("running_resume_stopped_and_staggered_cleanup", func(t *testing.T) {
		f, serverUp, now, onlineByID, removed, ce := newColo(t)
		p0, _ := f.buildInput()
		var keys []string
		for _, inst := range p0.SettingsInstances() {
			keys = append(keys, inst.Key)
		}
		require.Len(t, keys, 2)

		// Interrupt running phase on second instance, then resume to awaiting-stop.
		failSecond := true
		f.regHook = func(inst config.V1RuntimeBrokerInstanceConfig) error {
			if failSecond && inst.Key == keys[1] {
				return errors.New("transient")
			}
			return nil
		}
		_, in := f.buildInput()
		_, err := ApplyColocated(context.Background(), f.env, ce(), in)
		require.Error(t, err)
		failSecond = false

		_, in = f.buildInput()
		rep, err := ApplyColocated(context.Background(), f.env, ce(), in)
		require.NoError(t, err)
		assert.Equal(t, JournalAwaitingStop, rep.State)
		assert.Contains(t, rep.Notes, NoteColocatedAwaitingStop)
		require.Len(t, rep.Instances, 2)

		// Stop server and run stopped phase with LocalPlan=true and nil Hub client.
		*serverUp = false
		*now = now.Add(5 * time.Minute)
		raw, err := os.ReadFile(filepath.Join(f.g, "settings.yaml"))
		require.NoError(t, err)
		vs := &config.VersionedSettings{}
		require.NoError(t, yamlv3.Unmarshal(raw, vs))
		localPlan, err := BuildLocal(Input{Settings: vs, RawSettings: raw})
		require.NoError(t, err)

		savedClient := f.env.Client
		f.env.Client = nil
		rep, err = ApplyColocated(context.Background(), f.env, ce(), ApplyInput{Plan: localPlan, LocalPlan: true})
		f.env.Client = savedClient
		require.NoError(t, err)
		assert.Equal(t, JournalApplied, rep.State)
		assert.Contains(t, rep.Notes, NoteColocatedStart)

		written, err := f.env.LoadInstances()
		require.NoError(t, err)
		assert.Len(t, written, 2)

		// Server starts; first instance comes online while second is still starting.
		*serverUp = true
		id0 := rep.Instances[0].BrokerID
		id1 := rep.Instances[1].BrokerID
		onlineByID[id0] = true
		onlineByID[id1] = false

		_, in = f.buildInput()
		rep, err = ApplyColocated(context.Background(), f.env, ce(), in)
		require.NoError(t, err)
		assert.Contains(t, rep.Notes, NoteColocatedCleanupWaiting)
		assert.Equal(t, []string{keys[0]}, *removed, "only the online instance's credentials are removed")
		_, err = os.Lstat(filepath.Join(f.g, "runtime-brokers", keys[1], "hub-credentials", "hub.json"))
		require.NoError(t, err, "offline instance's credentials kept until online")

		// Second instance comes online -> cleanup completes.
		onlineByID[id1] = true
		_, in = f.buildInput()
		rep, err = ApplyColocated(context.Background(), f.env, ce(), in)
		require.NoError(t, err)
		assert.Contains(t, rep.Notes, NoteColocatedDone)
		assert.Equal(t, []string{keys[0], keys[1]}, *removed)
	})

	t.Run("two_phase_rollback_removes_all_credentials_before_moving_identities", func(t *testing.T) {
		f, serverUp, now, _, removed, ce := newColo(t)
		before := settingsBytes(t, f.g)

		_, in := f.buildInput()
		_, err := ApplyColocated(context.Background(), f.env, ce(), in)
		require.NoError(t, err)

		*serverUp = false
		*now = now.Add(2 * time.Minute)
		raw, err := os.ReadFile(filepath.Join(f.g, "settings.yaml"))
		require.NoError(t, err)
		vs := &config.VersionedSettings{}
		require.NoError(t, yamlv3.Unmarshal(raw, vs))
		localPlan, err := BuildLocal(Input{Settings: vs, RawSettings: raw})
		require.NoError(t, err)
		_, err = ApplyColocated(context.Background(), f.env, ce(), ApplyInput{Plan: localPlan, LocalPlan: true})
		require.NoError(t, err)

		// Operator decides to roll back: step 1 with server running.
		*serverUp = true
		rbRun, err := RollbackColocated(context.Background(), f.env, ce())
		require.NoError(t, err)
		assert.Equal(t, JournalAwaitingStop, rbRun.State)
		assert.Contains(t, rbRun.Notes, NoteColocatedRollbackAwaitingStop)
		assert.Empty(t, f.hub.instances, "both flat rows deregistered in running rollback phase")

		// Step 2 with server stopped: removes unused credentials for BOTH instances and moves dirs aside.
		*serverUp = false
		savedClient := f.env.Client
		f.env.Client = nil
		rbStop, err := RollbackColocated(context.Background(), f.env, ce())
		f.env.Client = savedClient
		require.NoError(t, err)
		assert.Equal(t, JournalRolledBack, rbStop.State)
		assert.Contains(t, rbStop.Notes, NoteColocatedRollbackStart)
		assert.Equal(t, string(before), string(settingsBytes(t, f.g)))
		assert.Len(t, *removed, 2, "unused credentials removed for both instances before move")

		for path, val := range listTree(t, ConvertDir(f.g)) {
			assert.NotContains(t, val, plantedJoinSecret, "moved tree %s holds no join secret", path)
			assert.NotContains(t, path, "hub-credentials", "moved tree %s holds no credentials dir", path)
		}
	})
}
