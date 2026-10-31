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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
)

// Ordering tests for the Kubernetes placement defaults (ptone/scion#5085):
// only legs that start a pod resolve placement, and a refusal comes before
// the leg's side effects (the 201, the restart endpoint's stop, the config
// write). Test IDs are from the placement design's catalogue.

const plOrderAgent = "agent-1"

// newOrderFixture is newPlacementFixture with the forced Kubernetes runtime
// (the entry rung only) and one existing, running agent whose
// scion-agent.json is scionAgent.
func newOrderFixture(t *testing.T, global string, rec config.RuntimeBuildRecord, scionAgent string) (*plFixture, string) {
	t.Helper()
	f := newPlacementFixture(t, global, k8s.ConnKubeconfig, rec)
	f.srv.config.ForceRuntime = "kubernetes"
	// Provisioned with no profile (an empty provisioned profile).
	agentDir := writePlacementAgent(t, f.dotScion, plOrderAgent, `{"version": 1, "profile": ""}`, "", scionAgent)
	f.mgr.agents = []api.AgentInfo{{
		ID: plOrderAgent, Name: plOrderAgent, ContainerID: plOrderAgent, ProjectPath: f.dotScion, Phase: "running",
		Labels: map[string]string{"scion.name": plOrderAgent},
	}}
	return f, agentDir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func projectBody(dotScion, extra string) string {
	return `{"projectPath": ` + strconvQuote(dotScion) + extra + `}`
}

// A global file whose placement cannot be determined at step 2: legacy
// format, mentioning a placement key.
const plLegacyGlobal = "harnesses:\n  claude:\n    image: x\n# nodeSelector: set here\n"

// O1, O2: provision-only creates and reprovisions never resolve placement,
// so a global file that fails placement does not affect them; the same
// file refuses a create that starts a pod (control).
func TestPlacementOrder_ProvisionOnlyAndReprovision_O1_O2(t *testing.T) {
	for _, tc := range []struct {
		id, extra string
	}{
		{"O1", ""},
		{"O2", `, "reprovision": true`},
	} {
		t.Run(tc.id, func(t *testing.T) {
			mgr := &provisionCapturingManager{}
			f := newPlacementServer(t, plLegacyGlobal, k8s.ConnKubeconfig, plGoodRecord(), mgr)
			f.srv.config.ForceRuntime = "kubernetes"
			w := postCreate(t, f.srv, map[string]any{"name": "prov-" + tc.id, "id": "uuid-" + tc.id, "slug": "prov-" + tc.id,
				"provisionOnly": true, "reprovision": tc.extra != "", "config": map[string]any{"template": "claude"}})
			if w.Code != http.StatusCreated {
				t.Fatalf("provision-only create: got %d %s", w.Code, w.Body.String())
			}
			if mgr.startCalled {
				t.Error("provision-only must not start")
			}
			if tc.extra != "" && !mgr.reprovisionCalled {
				t.Error("expected Reprovision to be called")
			}
			if tc.extra == "" && !mgr.provisionCalled {
				t.Error("expected Provision to be called")
			}
		})
	}

	// Control: the same settings refuse a create that starts a pod.
	f := newPlacementFixture(t, plLegacyGlobal, k8s.ConnKubeconfig, plGoodRecord())
	f.srv.config.ForceRuntime = "kubernetes"
	w := postCreate(t, f.srv, map[string]any{"name": "started", "config": map[string]any{"template": "claude"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "legacy format") {
		t.Fatalf("control create: got %d %s", w.Code, w.Body.String())
	}
	if f.mgr.startCalls != 0 {
		t.Error("a refused create must not start")
	}
}

// O3: reincarnation's start leg (a start after a reprovision recorded a
// fresh provenance profile) applies that profile's placement.
func TestPlacementOrder_ReincarnationStartUsesFreshProvenance_O3(t *testing.T) {
	f := newPlacementFixture(t, plGlobal(`profiles:
  old:
    runtime: gke
    node_selector:
      accel: t4
  fresh:
    runtime: gke
    node_selector:
      accel: l4
`), k8s.ConnKubeconfig, plGoodRecord())
	f.projectSettings(plProjectVS("", map[string]config.V1ProfileConfig{"old": {Runtime: "gke"}, "fresh": {Runtime: "gke"}}), nil)
	writePlacementAgent(t, f.dotScion, plOrderAgent, `{"version": 1, "profile": "fresh"}`, "old", `{}`)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "start", projectBody(f.dotScion, ""))
	if w.Code != http.StatusAccepted {
		t.Fatalf("start: got %d %s", w.Code, w.Body.String())
	}
	opts := f.mgr.lastStartOpts
	if !opts.PlacementResolved || opts.ResolvedPlacement == nil || opts.ResolvedPlacement.NodeSelector["accel"] != "l4" {
		t.Fatalf("want the fresh provenance profile's placement, got resolved=%v %+v", opts.PlacementResolved, opts.ResolvedPlacement)
	}
}

// optsRecordingAsyncManager is an asyncManager that records the options
// each Start receives, after waiting for release (when set).
type optsRecordingAsyncManager struct {
	*asyncManager
	release chan struct{}
	mu      sync.Mutex
	opts    []api.StartOptions
}

func (m *optsRecordingAsyncManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	if m.release != nil {
		<-m.release
	}
	m.mu.Lock()
	m.opts = append(m.opts, opts)
	m.mu.Unlock()
	return m.asyncManager.Start(ctx, opts)
}

func (m *optsRecordingAsyncManager) recorded() []api.StartOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.StartOptions(nil), m.opts...)
}

func newAsyncPlacementFixture(t *testing.T, global string, rec config.RuntimeBuildRecord) (*plFixture, *optsRecordingAsyncManager, *mockRuntimeBrokerService) {
	t.Helper()
	mgr := &optsRecordingAsyncManager{asyncManager: newAsyncManager()}
	f := newPlacementServer(t, global, k8s.ConnKubeconfig, rec, mgr)
	f.srv.config.ForceRuntime = "kubernetes"
	rtb := &mockRuntimeBrokerService{}
	f.srv.hubMu.Lock()
	f.srv.hubConnections["hub-a"] = &HubConnection{Name: "hub-a", BrokerID: "broker-on-a", HubClient: &stubBrokerHubClient{brokers: rtb}}
	f.srv.hubMu.Unlock()
	return f, mgr, rtb
}

// O4: an async create whose placement cannot be determined is refused with
// the fixed cause: no 201, no launch, and the dispatch attempt is marked
// failed with the cause.
func TestPlacementOrder_AsyncCreateRefused_O4(t *testing.T) {
	f, mgr, rtb := newAsyncPlacementFixture(t, plGlobal(""), config.RuntimeBuildRecord{})
	body := asyncCreateBody("async-refused", "L-O4")
	body["requestId"] = "req-O4"
	w := postCreate(t, f.srv, body)
	const cause = "cannot determine whether runtime entry gke targets this agent's runtime (runtime has no build record or client)"
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), cause) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	// The refusal is written before beginAsyncLaunch, which is the only place
	// a launch starts, so nothing can start after this response.
	if n := len(mgr.recorded()); n != 0 {
		t.Errorf("Start ran %d times for a refused create", n)
	}
	if n := len(rtb.getLaunchReports()); n != 0 {
		t.Errorf("a refused create sent %d launch reports", n)
	}
	f.srv.dispatchAttemptsMu.Lock()
	defer f.srv.dispatchAttemptsMu.Unlock()
	var failed bool
	for _, a := range f.srv.dispatchAttempts {
		if a.Status == dispatchAttemptFailed && a.HTTPStatus == http.StatusBadRequest && strings.Contains(a.Error, cause) {
			failed = true
		}
	}
	if !failed {
		t.Error("the dispatch attempt was not marked failed with the fixed cause")
	}
}

// O5: a restart through the agent runtime's restart endpoint whose placement
// cannot be determined is refused before that endpoint's stop: the agent is
// left running. (A restart the hub performs as its own stop followed by a
// start is refused at the start, after the hub's stop.)
func TestPlacementOrder_RestartRefusedBeforeStop_O5(t *testing.T) {
	f, _ := newOrderFixture(t, plGlobal(""), config.RuntimeBuildRecord{}, `{}`)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "restart", projectBody(f.dotScion, ""))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot determine whether runtime entry gke") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if f.mgr.stopCalls != 0 || f.mgr.startCalls != 0 {
		t.Errorf("stop=%d start=%d, want 0 and 0 (agent left running)", f.mgr.stopCalls, f.mgr.startCalls)
	}
}

// O6: a restart through the agent runtime's restart endpoint whose persisted
// kubernetes block conflicts with the placement in effect is refused before
// that endpoint's stop.
func TestPlacementOrder_RestartLegacyBlockRefusedBeforeStop_O6(t *testing.T) {
	f, _ := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{"kubernetes": {"tolerations": [{"operator": "Exists"}]}}`)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "restart", projectBody(f.dotScion, ""))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kubernetes.tolerations[0] in the agent config matches all taints") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if f.mgr.stopCalls != 0 || f.mgr.startCalls != 0 {
		t.Errorf("stop=%d start=%d, want 0 and 0 (agent left running)", f.mgr.stopCalls, f.mgr.startCalls)
	}
}

// O7: a start whose placement cannot be determined is refused before the
// inline config update: scion-agent.json is unchanged.
func TestPlacementOrder_StartRefusedBeforeConfigWrite_O7(t *testing.T) {
	f, agentDir := newOrderFixture(t, plGlobal(""), config.RuntimeBuildRecord{}, `{"max_turns": 3}`)
	before := readFile(t, filepath.Join(agentDir, "scion-agent.json"))
	w := postAgentOpFor(t, f.srv, plOrderAgent, "start", projectBody(f.dotScion, `, "inlineConfig": {"max_turns": 7}`))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "cannot determine whether runtime entry gke") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if after := readFile(t, filepath.Join(agentDir, "scion-agent.json")); after != before {
		t.Errorf("scion-agent.json changed:\n before %s\n after  %s", before, after)
	}
	if f.mgr.startCalls != 0 {
		t.Error("a refused start must not start")
	}
}

// O8: a start whose inline update would make the merged kubernetes block
// conflict with the placement in effect is refused, writing nothing. With
// an unreadable scion-agent.json (the update would write nothing), the
// inline block alone is checked.
func TestPlacementOrder_StartInlineConflictWritesNothing_O8(t *testing.T) {
	f, agentDir := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{"max_turns": 3, "kubernetes": {"namespace": "agents"}}`)
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	before := readFile(t, cfgPath)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "start", projectBody(f.dotScion,
		`, "inlineConfig": {"max_turns": 7, "kubernetes": {"runtimeClassName": "kata"}}`))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "kubernetes.runtimeClassName in the agent config differs from the agent runtime's runtime_class_name") {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if after := readFile(t, cfgPath); after != before {
		t.Errorf("scion-agent.json changed:\n before %s\n after  %s", before, after)
	}

	// Unreadable scion-agent.json: the inline block alone decides.
	if err := os.WriteFile(cfgPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	w = postAgentOpFor(t, f.srv, plOrderAgent, "start", projectBody(f.dotScion,
		`, "inlineConfig": {"kubernetes": {"tolerations": [{"operator": "Exists"}]}}`))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "matches all taints") {
		t.Fatalf("unreadable file, conflicting inline block: got %d %s", w.Code, w.Body.String())
	}
	if got := readFile(t, cfgPath); got != "{not json" {
		t.Errorf("scion-agent.json changed: %s", got)
	}
}

// The start pre-check's inputs, directly: the persisted block merged with
// the inline one; the inline block alone when there is no project path or
// the file cannot be read; nothing to check while placement is not in
// effect.
func TestCheckStartLegacyPlacement(t *testing.T) {
	f, agentDir := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{"kubernetes": {"tolerations": [{"operator": "Exists"}]}}`)
	rp := &api.ResolvedPlacement{NodeSelector: map[string]string{"pool": "agents"}}
	matchAll := &api.ScionConfig{Kubernetes: &api.KubernetesConfig{Tolerations: []api.K8sToleration{{Operator: "Exists"}}}}
	benign := &api.ScionConfig{MaxTurns: api.IntPtr(4)}

	if err := f.srv.checkStartLegacyPlacement(plOrderAgent, f.dotScion, benign, false, rp); err == nil {
		t.Error("persisted match-all toleration with a benign inline update: want a refusal")
	}
	if err := f.srv.checkStartLegacyPlacement(plOrderAgent, f.dotScion, nil, false, rp); err == nil {
		t.Error("persisted match-all toleration and no inline update: want a refusal")
	}
	if err := f.srv.checkStartLegacyPlacement(plOrderAgent, f.dotScion, matchAll, false, nil); err != nil {
		t.Errorf("not in effect: %v", err)
	}
	if err := f.srv.checkStartLegacyPlacement(plOrderAgent, "", benign, false, rp); err != nil {
		t.Errorf("no project path, benign inline block: %v", err)
	}
	if err := f.srv.checkStartLegacyPlacement(plOrderAgent, "", matchAll, false, rp); err == nil {
		t.Error("no project path, conflicting inline block: want a refusal")
	}
	if err := os.Remove(filepath.Join(agentDir, "scion-agent.json")); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.checkStartLegacyPlacement(plOrderAgent, f.dotScion, benign, false, rp); err != nil {
		t.Errorf("missing file, benign inline block: %v", err)
	}
}

// The restart pre-check fails closed when the persisted config cannot be
// read while placement is in effect.
func TestCheckRestartLegacyPlacement_UnreadableFailsClosed(t *testing.T) {
	f, agentDir := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{}`)
	rp := &api.ResolvedPlacement{NodeSelector: map[string]string{"pool": "agents"}}
	if err := f.srv.checkRestartLegacyPlacement(plOrderAgent, f.dotScion, false, rp); err != nil {
		t.Fatalf("readable, no conflict: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := f.srv.checkRestartLegacyPlacement(plOrderAgent, f.dotScion, false, rp)
	wantRefusal(t, err, placementAgentConfigCause)
	if err := f.srv.checkRestartLegacyPlacement(plOrderAgent, f.dotScion, false, nil); err != nil {
		t.Errorf("not in effect: %v", err)
	}
}

// O9: a sync create and an async create each hand the resolved placement
// to Manager.Start. The async worker uses the admission result: a settings
// change after the 201 does not affect that launch, and placement is
// resolved once.
func TestPlacementOrder_CreatesCarryPlacement_O9(t *testing.T) {
	t.Run("sync", func(t *testing.T) {
		f := newPlacementFixture(t, plGlobal(""), k8s.ConnKubeconfig, plGoodRecord())
		f.srv.config.ForceRuntime = "kubernetes"
		w := postCreate(t, f.srv, map[string]any{"name": "sync-agent", "config": map[string]any{"template": "claude"}})
		if w.Code != http.StatusCreated {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
		opts := f.mgr.lastStartOpts
		if !opts.PlacementResolved || opts.ResolvedPlacement == nil || opts.ResolvedPlacement.NodeSelector["pool"] != "agents" {
			t.Fatalf("Start got resolved=%v %+v", opts.PlacementResolved, opts.ResolvedPlacement)
		}
	})
	t.Run("async", func(t *testing.T) {
		f, mgr, _ := newAsyncPlacementFixture(t, plGlobal(""), plGoodRecord())
		mgr.release = make(chan struct{})
		w := postCreate(t, f.srv, asyncCreateBody("async-agent", "L-O9"))
		if w.Code != http.StatusCreated || !decodeCreateResponse(t, w).LaunchPending {
			t.Fatalf("got %d %s", w.Code, w.Body.String())
		}
		// The settings change after the 201; the launch already has its
		// placement.
		f.writeGlobal(t, strings.Replace(plGlobal(""), "pool: agents", "pool: changed", 1))
		close(mgr.release)
		if !waitUntil(t, 5*time.Second, func() bool { return len(mgr.recorded()) == 1 }) {
			t.Fatal("the launch did not start")
		}
		opts := mgr.recorded()[0]
		if !opts.PlacementResolved || opts.ResolvedPlacement == nil || opts.ResolvedPlacement.NodeSelector["pool"] != "agents" {
			t.Fatalf("Start got resolved=%v %+v; want the admission result", opts.PlacementResolved, opts.ResolvedPlacement)
		}
		if n := strings.Count(f.logs.String(), "Kubernetes placement resolved"); n != 1 {
			t.Errorf("placement resolved %d times, want once (at admission)", n)
		}
	})
}

// O10: after a successful start, scion-agent.json holds no resolved
// placement: only the agent's own block, unchanged.
func TestPlacementOrder_NotPersisted_O10(t *testing.T) {
	f, agentDir := newOrderFixture(t, plGlobal(""), plGoodRecord(), `{"kubernetes": {"namespace": "agents"}}`)
	w := postAgentOpFor(t, f.srv, plOrderAgent, "start", projectBody(f.dotScion, `, "inlineConfig": {"max_turns": 9}`))
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	if !f.mgr.lastStartOpts.PlacementResolved || f.mgr.lastStartOpts.ResolvedPlacement == nil {
		t.Fatal("the start did not resolve placement")
	}
	after := readFile(t, filepath.Join(agentDir, "scion-agent.json"))
	for _, key := range []string{"pool", "dedicated", "nodeSelector", "tolerations", "resolvedPlacement"} {
		if strings.Contains(after, key) {
			t.Errorf("scion-agent.json carries %q after the start:\n%s", key, after)
		}
	}
	if !strings.Contains(after, `"namespace": "agents"`) || !strings.Contains(after, `"max_turns": 9`) {
		t.Errorf("scion-agent.json lost the agent's own values:\n%s", after)
	}
}

// OP1 (carry a, handler level): the start and restart handlers settle How for
// an existing agent with an empty selection profile, so the refusal while a
// placement profile exists for its runtime names the agent and its missing
// recorded profile (not the "not classified" fallback cause).
func TestPlacementOrder_HandlersSettleUnrecorded(t *testing.T) {
	for _, op := range []string{"start", "restart"} {
		t.Run(op, func(t *testing.T) {
			f, _ := newOrderFixture(t, plGlobal(plGPUProfile), plGoodRecord(), `{}`)
			f.srv.config.ForceRuntime = ""
			f.projectSettings(plProjectVS("gpu", map[string]config.V1ProfileConfig{"gpu": {Runtime: "gke"}}), nil)
			w := postAgentOpFor(t, f.srv, plOrderAgent, op, projectBody(f.dotScion, ""))
			const want = "agent agent-1 has no recorded profile; profile-level placement is configured for its runtime (profiles gpu)"
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), want) {
				t.Fatalf("%s: got %d %s; want 400 with %q", op, w.Code, w.Body.String(), want)
			}
			if f.mgr.stopCalls != 0 || f.mgr.startCalls != 0 {
				t.Errorf("%s: stop=%d start=%d, want 0 and 0", op, f.mgr.stopCalls, f.mgr.startCalls)
			}
		})
	}
}
