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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Flat instance placement: the node selector, tolerations and runtime
// class of a flat Kubernetes instance's runtime_target, resolved per start
// from the activated instance policy, checked against the agent's own
// kubernetes block before any side effect, and carried into the pod spec.

func flatPoolToleration() api.K8sToleration {
	return api.K8sToleration{Key: config.PoolNodeSelectorKey, Operator: "Equal", Value: "agents", Effect: "NoSchedule"}
}

// flatPlacementTarget is a flat Kubernetes runtime_target with the pool
// selector, its toleration and a runtime class.
func flatPlacementTarget() config.V1RuntimeTargetConfig {
	return config.V1RuntimeTargetConfig{
		Type: "kubernetes", Namespace: "agents",
		NodeSelector:     map[string]string{config.PoolNodeSelectorKey: "agents"},
		Tolerations:      []api.K8sToleration{flatPoolToleration()},
		RuntimeClassName: "gvisor",
	}
}

// legacyPlacementSettings gives the legacy runtime entry and profile
// placement that a flat instance must never read.
const legacyPlacementSettings = `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: kubernetes
        node_selector:
            tier: legacy-profile
runtimes:
    kubernetes:
        type: kubernetes
        node_selector:
            scion.dev/pool: legacy
        tolerations:
            - key: legacy
              operator: Exists
        runtime_class_name: legacy-class
`

// flatRes is a flat resolution on the fixture's real Kubernetes runtime.
func (f *flatPodFixture) flatRes(agentName string) runtimeResolution {
	return runtimeResolution{Manager: f.mgr, Runtime: f.mgr.Runtime, RuntimeType: "kubernetes", How: resolutionFlat, Agent: agentName}
}

// startFlatPlacedPod is startPlacedPod for a flat instance with the
// create's own kubernetes block k8sCfg: the create's placement check, its
// start context, its placement and the real agent manager's start.
func startFlatPlacedPod(t *testing.T, f *flatPodFixture, name string, gcp *GCPIdentityConfig, k8sCfg *api.KubernetesConfig) *corev1.Pod {
	t.Helper()
	var inline *api.ScionConfig
	if k8sCfg != nil {
		inline = &api.ScionConfig{Kubernetes: k8sCfg}
	}
	if err := f.srv.checkFlatCreatePlacement(CreateAgentRequest{Name: name, InlineConfig: inline}); err != nil {
		t.Fatalf("create placement check: %v", err)
	}
	sc, err := f.srv.buildStartContext(context.Background(), startContextInputs{
		Name: name, ProjectPath: f.projectDir, Operation: opCreate,
		Config:       &CreateAgentConfig{GCPIdentity: gcp},
		InlineConfig: inline,
		HTTPRequest:  httptest.NewRequest("POST", "/api/v1/agents", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := sc.Opts
	rp, err := f.srv.placementForStart(context.Background(), sc.Resolution, opts.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	if !rp.OperatorOwned {
		t.Fatalf("a flat start's placement must be operator-owned: %+v", rp)
	}
	opts.ResolvedPlacement, opts.PlacementResolved = rp, true
	opts.BrokerMode, opts.NoAuth = true, true
	if _, err := f.mgr.Start(context.Background(), opts); !errors.Is(err, errPodCaptured) {
		t.Fatalf("start: want the captured pod create, got %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pods[len(f.pods)-1]
}

func podTolerates(pod *corev1.Pod, key, value string, effect corev1.TaintEffect) bool {
	for _, tol := range pod.Spec.Tolerations {
		if tol.Key == key && tol.Value == value && tol.Effect == effect {
			return true
		}
	}
	return false
}

// The flat branch resolves the instance target's placement, operator-owned
// and sourced from the instance's runtime_target; a target without
// placement still resolves an (empty) operator-owned placement, so the
// agent's own fields are checked; an invalid target refuses the start with
// a fixed cause.
func TestResolvePlacementFlatTarget(t *testing.T) {
	f := newFlatPodFixture(t, "agents", flatPlacementTarget())
	rp, err := f.srv.resolvePlacement(context.Background(), f.flatRes("a1"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rp.OperatorOwned || !reflect.DeepEqual(rp.NodeSelector, map[string]string{config.PoolNodeSelectorKey: "agents"}) ||
		!reflect.DeepEqual(rp.Tolerations, []api.K8sToleration{flatPoolToleration()}) || rp.RuntimeClassName != "gvisor" {
		t.Fatalf("resolved %+v", rp)
	}
	if got := rp.Sources["node_selector"]; !reflect.DeepEqual(got, []string{"server.broker.instances[agents].runtime_target"}) {
		t.Fatalf("sources = %v", rp.Sources)
	}
	// The resolution is a copy: changing it does not change the policy.
	rp.NodeSelector["extra"] = "x"
	again, _ := f.srv.resolvePlacement(context.Background(), f.flatRes("a1"), nil)
	if _, leaked := again.NodeSelector["extra"]; leaked {
		t.Fatal("the resolution shares its selector with the instance policy")
	}

	empty := newFlatPodFixture(t, "bare", config.V1RuntimeTargetConfig{})
	rp, err = empty.srv.resolvePlacement(context.Background(), empty.flatRes("a2"), nil)
	if err != nil || rp == nil || !rp.OperatorOwned || rp.InEffect() {
		t.Fatalf("empty target: got %+v, %v", rp, err)
	}

	invalid := flatPlacementTarget()
	invalid.NodeSelector = map[string]string{"bad key!": "secret-value"}
	bad := newFlatPodFixture(t, "bad", invalid)
	rp, err = bad.srv.resolvePlacement(context.Background(), bad.flatRes("a3"), nil)
	var sce *startContextError
	if rp != nil || !errors.As(err, &sce) || sce.Status != http.StatusBadRequest || sce.Message != placementFlatInvalidCause {
		t.Fatalf("invalid target: got %+v, %v", rp, err)
	}
}

// The flat branch never borrows placement from the legacy runtime entries
// or profiles in the global settings, with or without target placement.
func TestFlatPlacementDoesNotUseLegacyMaps(t *testing.T) {
	for name, target := range map[string]config.V1RuntimeTargetConfig{
		"target placement":    flatPlacementTarget(),
		"no target placement": {},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFlatPodFixture(t, "agents", target)
			if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".scion", "settings.yaml"), []byte(legacyPlacementSettings), 0o644); err != nil {
				t.Fatal(err)
			}
			rp, err := f.srv.placementForStart(context.Background(), f.flatRes("a1"), f.projectDir)
			if err != nil {
				t.Fatal(err)
			}
			if rp.RuntimeClassName == "legacy-class" || rp.NodeSelector["tier"] != "" || rp.NodeSelector[config.PoolNodeSelectorKey] == "legacy" {
				t.Fatalf("legacy placement used: %+v", rp)
			}
			for _, tol := range rp.Tolerations {
				if tol.Key == "legacy" {
					t.Fatalf("legacy toleration used: %+v", rp)
				}
			}
			for field, sources := range rp.Sources {
				for _, s := range sources {
					if !strings.HasPrefix(s, "server.broker.instances[") {
						t.Errorf("%s sourced from %s", field, s)
					}
				}
			}
		})
	}
}

// The end-to-end chain: a host instance with a pool selector, its
// toleration and a runtime class starts an agent from the default template
// through the create's placement check, broker resolution and the real
// agent manager into the Kubernetes pod builder. The pod carries the
// target's placement plus an agent-added selector key, and the read-only
// report shows the same placement. Nothing resolved is written back to the
// agent's own config.
func TestDefaultTemplateReceivesFlatPlacement(t *testing.T) {
	f := newFlatPodFixture(t, "agents", flatPlacementTarget())
	pod := startFlatPlacedPod(t, f, "e2e-agent", nil, &api.KubernetesConfig{NodeSelector: map[string]string{"disk": "ssd"}})

	if want := map[string]string{config.PoolNodeSelectorKey: "agents", "disk": "ssd"}; !reflect.DeepEqual(pod.Spec.NodeSelector, want) {
		t.Errorf("node selector = %v, want %v", pod.Spec.NodeSelector, want)
	}
	if !podTolerates(pod, config.PoolNodeSelectorKey, "agents", corev1.TaintEffectNoSchedule) || len(pod.Spec.Tolerations) != 1 {
		t.Errorf("tolerations = %v", pod.Spec.Tolerations)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("runtimeClassName = %v, want gvisor", pod.Spec.RuntimeClassName)
	}
	if pod.Namespace != "agents" {
		t.Errorf("namespace = %q", pod.Namespace)
	}

	report := config.PresentInstance(f.srv.config.FlatInstance.Instance).API()
	if !report.Target.PlacementReported || report.Target.RuntimeClassName != "gvisor" ||
		!reflect.DeepEqual(report.Target.NodeSelector, map[string]string{config.PoolNodeSelectorKey: "agents"}) ||
		!reflect.DeepEqual(report.Target.Tolerations, []api.K8sToleration{flatPoolToleration()}) {
		t.Errorf("read-only report = %+v", report.Target)
	}
}

// Resolved placement is never persisted into the agent's own config: after
// a flat start, the agent's stored config carries only what the agent set.
func TestPlacementNotPersistedInAgentConfig(t *testing.T) {
	f := newFlatPodFixture(t, "agents", flatPlacementTarget())
	startFlatPlacedPod(t, f, "persist-agent", nil, nil)
	var found int
	err := filepath.Walk(filepath.Dir(f.projectDir), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "scion-agent.json" {
			return err
		}
		found++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, v := range []string{"gvisor", config.PoolNodeSelectorKey, "nodeSelector", "tolerations", "runtimeClassName"} {
			if strings.Contains(string(data), v) {
				t.Errorf("%s holds resolved placement (%s): %s", path, v, data)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("the start wrote no agent config to check")
	}
}

// The GCP identity "block" mode still adds the Workload Identity node label
// after the flat placement merge, next to the instance's pool selector.
func TestPodPlacementIdentityLabelRuleRetained(t *testing.T) {
	f := newFlatPodFixture(t, "agents", flatPlacementTarget())
	pod := startFlatPlacedPod(t, f, "block-agent", &GCPIdentityConfig{MetadataMode: "block"}, nil)
	assertBlockPod(t, pod, "")
	if pod.Spec.NodeSelector[config.PoolNodeSelectorKey] != "agents" {
		t.Errorf("node selector %v lost the pool key", pod.Spec.NodeSelector)
	}
	if pod.Spec.NodeSelector[scionrt.KubernetesWorkloadIdentityNodeLabel] != "true" {
		t.Errorf("node selector %v lacks the identity label", pod.Spec.NodeSelector)
	}
}

// newFlatPlacementInstanceServer is newFlatInstanceTestServer with the
// instance's runtime_target replaced by target (a Kubernetes target) and a
// recording runtime named kubernetes. The instance policy is activated from
// target when the server is built.
func newFlatPlacementInstanceServer(t *testing.T, target config.V1RuntimeTargetConfig) *flatInstanceFixture {
	t.Helper()
	f := newFlatInstanceTestServer(t, flatInstanceOpts{hubInProcess: true})
	cfg := f.srv.config
	inst := f.instances[0]
	inst.RuntimeTarget = &target
	cfg.FlatInstance = &FlatInstanceConfig{Identity: f.identity, Instance: inst, HubInProcess: true}
	rt := &scionrt.MockRuntime{NameFunc: func() string { return "kubernetes" }}
	f.srv = New(cfg, f.mgr, rt, config.RuntimeBuildRecord{})
	endAsyncLaunchesAtCleanup(t, f.srv)
	f.instances[0] = inst
	return f
}

func expectPlacementRefusal(t *testing.T, w *httptest.ResponseRecorder, wantText string, withheld ...string) {
	t.Helper()
	e := expectFlatRefusal(t, w, http.StatusBadRequest, ErrCodeValidationError)
	if !strings.Contains(e.Message, wantText) {
		t.Fatalf("message %q lacks %q", e.Message, wantText)
	}
	for _, v := range withheld {
		if strings.Contains(w.Body.String(), v) {
			t.Fatalf("the refusal echoes %q: %s", v, w.Body.String())
		}
	}
}

// The inverse chain: a create whose own kubernetes block sets a different
// pool is refused with a validation error before any start effect: no
// dispatch attempt, launch record, project directory, preflight or runtime
// start. A matching create on the same instance is accepted and its start
// carries the operator-owned placement.
func TestPlacementRejectedBeforeCreateSideEffects(t *testing.T) {
	f := newFlatPlacementInstanceServer(t, flatPlacementTarget())
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-pool-conflict", "pool-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
			"config": map[string]interface{}{"kubernetes": map[string]interface{}{
				"nodeSelector": map[string]string{config.PoolNodeSelectorKey: "other-pool"}}}}))
	expectPlacementRefusal(t, w, "pool selector conflicts with inherited placement", "other-pool")
	expectNoCreateSideEffects(t, f, "req-pool-conflict")

	w = serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-inline-toleration", "tol-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
			"inlineConfig": map[string]interface{}{"kubernetes": map[string]interface{}{
				"tolerations": []map[string]string{{"key": "dedicated", "operator": "Equal", "value": "batch-pool", "effect": "NoSchedule"}}}}}))
	expectPlacementRefusal(t, w, "kubernetes.tolerations[0] in the request", "batch-pool")
	expectNoCreateSideEffects(t, f, "req-inline-toleration")

	w = serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-runtime-class", "rc-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
			"config": map[string]interface{}{"kubernetes": map[string]interface{}{"runtimeClassName": "kata"}}}))
	expectPlacementRefusal(t, w, "runtimeClassName in the request", "kata")
	expectNoCreateSideEffects(t, f, "req-runtime-class")

	w = serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-ok", "ok-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
			"config": map[string]interface{}{"template": "claude", "kubernetes": map[string]interface{}{
				"nodeSelector": map[string]string{config.PoolNodeSelectorKey: "agents", "disk": "ssd"}}}}))
	if w.Code != http.StatusCreated {
		t.Fatalf("matching create: status %d: %s", w.Code, w.Body.String())
	}
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	rp := f.mgr.lastStartOpts.ResolvedPlacement
	if !f.mgr.lastStartOpts.PlacementResolved || rp == nil || !rp.OperatorOwned || rp.RuntimeClassName != "gvisor" {
		t.Fatalf("start placement = %+v (resolved %v)", rp, f.mgr.lastStartOpts.PlacementResolved)
	}
}

// A flat instance still refuses a Runtime Broker Profile with 422
// runtime_profile_unsupported when its target carries placement.
func TestFlatProfileStillUnsupported(t *testing.T) {
	f := newFlatPlacementInstanceServer(t, flatPlacementTarget())
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents", flatCreateBody("req-profile", "profile-agent",
		map[string]interface{}{"expectedRuntimeTargetId": f.identity.RuntimeTarget.ID,
			"config": map[string]interface{}{"profile": "batch"}}))
	expectFlatRefusal(t, w, http.StatusUnprocessableEntity, ErrCodeRuntimeProfileUnsupported)
	expectNoCreateSideEffects(t, f, "req-profile")
}

// writeStoredAgentConfig writes the stored config of test-agent-1 in the
// fixture's project, and records that project on the agent's container, as
// a running agent's labels do, so a start or restart that names no project
// recovers it from there.
func writeStoredAgentConfig(t *testing.T, f *flatInstanceFixture, content string) string {
	t.Helper()
	projectDir, err := config.GetResolvedProjectDir("")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(config.GetAgentDir(projectDir, "test-agent-1", false), "scion-agent.json")
	writeFlatFixtureFile(t, path, content)
	f.mgr.mu.Lock()
	for i := range f.mgr.agents {
		if f.mgr.agents[i].Name == "test-agent-1" {
			f.mgr.agents[i].ProjectPath = projectDir
		}
	}
	f.mgr.mu.Unlock()
	return path
}

func flatRestartBody(f *flatInstanceFixture) string {
	return `{"expectedRuntimeTargetId":"` + f.identity.RuntimeTarget.ID + `",` + flatAgentEnv + `}`
}

// A restart whose placement cannot be resolved (an instance target that is
// not valid, here installed without the load-time validation) is refused
// before the restart's stop: the agent is left running.
func TestRestartPlacementRejectedBeforeStop(t *testing.T) {
	invalid := flatPlacementTarget()
	invalid.RuntimeClassName = "Not_A_Class"
	f := newFlatPlacementInstanceServer(t, invalid)
	f.seedOwnedAgent(t)
	writeStoredAgentConfig(t, f, `{}`)
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/restart"+flatStartQuery, flatRestartBody(f))
	expectPlacementRefusal(t, w, placementFlatInvalidCause, "Not_A_Class")
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.stopCalls != 0 || f.mgr.startCalls != 0 {
		t.Errorf("stop=%d start=%d, want 0 and 0 (agent left running)", f.mgr.stopCalls, f.mgr.startCalls)
	}
}

// A restart of an agent whose stored kubernetes block conflicts with the
// instance's placement is refused before the stop, naming the stored
// config as the layer.
func TestRestartPersistedConflictRejectedBeforeStop(t *testing.T) {
	f := newFlatPlacementInstanceServer(t, flatPlacementTarget())
	f.seedOwnedAgent(t)
	writeStoredAgentConfig(t, f, `{"kubernetes": {"tolerations": [{"key": "dedicated", "operator": "Equal", "value": "batch", "effect": "NoSchedule"}]}}`)
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/restart"+flatStartQuery, flatRestartBody(f))
	expectPlacementRefusal(t, w, "kubernetes.tolerations[0] in the agent's stored config", "batch")
	f.mgr.mu.Lock()
	defer f.mgr.mu.Unlock()
	if f.mgr.stopCalls != 0 || f.mgr.startCalls != 0 {
		t.Errorf("stop=%d start=%d, want 0 and 0 (agent left running)", f.mgr.stopCalls, f.mgr.startCalls)
	}
}

// A start whose inline update would set a different pool is refused
// before that update writes the stored config, naming the request as the
// layer; a stored conflict is named as the stored config.
func TestFlatStartPlacementRejectedBeforeConfigWrite(t *testing.T) {
	f := newFlatPlacementInstanceServer(t, flatPlacementTarget())
	f.seedOwnedAgent(t)
	path := writeStoredAgentConfig(t, f, `{"max_turns": 3}`)
	before, _ := os.ReadFile(path)
	w := serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start"+flatStartQuery,
		`{"expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`",`+flatAgentEnv+
			`,"inlineConfig":{"max_turns":7,"kubernetes":{"nodeSelector":{"scion.dev/pool":"other-pool"}}}}`)
	expectPlacementRefusal(t, w, "in the request", "other-pool")
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Errorf("stored config changed:\n before %s\n after  %s", before, after)
	}
	if n := mgrStartCalls(f); n != 0 {
		t.Fatalf("a refused start started: %d", n)
	}

	writeStoredAgentConfig(t, f, `{"kubernetes": {"runtimeClassName": "kata"}}`)
	w = serveFlat(f.srv, http.MethodPost, "/api/v1/agents/test-agent-1/start"+flatStartQuery,
		`{"expectedRuntimeTargetId":"`+f.identity.RuntimeTarget.ID+`",`+flatAgentEnv+`}`)
	expectPlacementRefusal(t, w, "in the agent's stored config", "kata")
}

// The read-only report never drives dispatch: the start resolves the
// policy activated when the server was built, not a later change to the
// entry the report is built from, nor a change to a built report.
func TestPlacementPresentationDoesNotDriveDispatch(t *testing.T) {
	f := newFlatPlacementInstanceServer(t, flatPlacementTarget())
	report := config.PresentInstance(f.srv.config.FlatInstance.Instance).API()
	report.Target.NodeSelector[config.PoolNodeSelectorKey] = "from-report"
	report.Target.RuntimeClassName = "from-report"
	f.srv.config.FlatInstance.Instance.RuntimeTarget.NodeSelector[config.PoolNodeSelectorKey] = "edited-after-activation"

	rp, err := f.srv.resolveFlatPlacement("a1")
	if err != nil {
		t.Fatal(err)
	}
	if rp.NodeSelector[config.PoolNodeSelectorKey] != "agents" || rp.RuntimeClassName != "gvisor" {
		t.Fatalf("dispatch used the report or an unactivated edit: %+v", rp)
	}
}
