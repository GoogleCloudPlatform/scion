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

package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Flat instance placement (node_selector, tolerations, runtime_class_name
// on server.broker.instances[].runtime_target) and the operator-owned
// placement rules every flat start applies.

const flatPlacementInstanceYAML = `schema_version: "1"
server:
  broker:
    enabled: true
    instances:
      - key: agents
        name: agents
        runtime_target:
          type: kubernetes
          namespace: agents
          node_selector:
            scion.dev/pool: agents
          tolerations:
            - key: scion.dev/pool
              operator: Equal
              value: agents
              effect: NoSchedule
          runtime_class_name: gvisor
`

func poolToleration() api.K8sToleration {
	return api.K8sToleration{Key: PoolNodeSelectorKey, Operator: "Equal", Value: "agents", Effect: "NoSchedule"}
}

// operatorPlacement is the operator-owned placement of a flat instance
// with the pool selector, its toleration and a runtime class.
func operatorPlacement() *api.ResolvedPlacement {
	return &api.ResolvedPlacement{
		NodeSelector:     map[string]string{PoolNodeSelectorKey: "agents"},
		Tolerations:      []api.K8sToleration{poolToleration()},
		RuntimeClassName: "gvisor",
		OperatorOwned:    true,
	}
}

// The three fields load from the host file through the strict loader, map
// to the server-config twin and back unchanged, and the two forms carry
// the same placement fields.
func TestFlatRuntimeTargetPlacementRoundTrip(t *testing.T) {
	global := flatTestHome(t)
	writeFlatTestFile(t, filepath.Join(global, "settings.yaml"), flatPlacementInstanceYAML)
	strict, err := LoadRuntimeBrokerInstances("")
	if err != nil {
		t.Fatal(err)
	}
	want := V1RuntimeTargetConfig{Type: "kubernetes", Namespace: "agents",
		NodeSelector:     map[string]string{PoolNodeSelectorKey: "agents"},
		Tolerations:      []api.K8sToleration{poolToleration()},
		RuntimeClassName: "gvisor"}
	if len(strict) != 1 || !reflect.DeepEqual(*strict[0].RuntimeTarget, want) {
		t.Fatalf("strict load: got %+v", strict)
	}
	gc, err := LoadGlobalConfig("")
	if err != nil {
		t.Fatal(err)
	}
	twin := gc.RuntimeBroker.Instances[0].RuntimeTarget
	if !reflect.DeepEqual(twin.NodeSelector, want.NodeSelector) || !reflect.DeepEqual(twin.Tolerations, want.Tolerations) ||
		twin.RuntimeClassName != want.RuntimeClassName {
		t.Fatalf("server-config form: %+v", twin)
	}
	if back := globalInstancesToV1(v1InstancesToGlobal(strict)); !reflect.DeepEqual(back, strict) {
		t.Fatalf("round trip = %+v, want %+v", back[0].RuntimeTarget, strict[0].RuntimeTarget)
	}
	// The twin keeps its camelCase wire names for the same three fields.
	raw, err := json.Marshal(twin)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"nodeSelector"`, `"tolerations"`, `"runtimeClassName"`} {
		if !strings.Contains(string(raw), k) {
			t.Errorf("server-config JSON lacks %s: %s", k, raw)
		}
	}
	for _, f := range []string{"NodeSelector", "Tolerations", "RuntimeClassName"} {
		a, okA := reflect.TypeOf(V1RuntimeTargetConfig{}).FieldByName(f)
		b, okB := reflect.TypeOf(RuntimeTargetConfig{}).FieldByName(f)
		if !okA || !okB || a.Type != b.Type {
			t.Errorf("field %s differs between the settings and server-config forms", f)
		}
	}
	// The JSON schema (scion config validate) accepts the same file.
	if errs := validateRawYAMLAgainstSchema(t, flatPlacementInstanceYAML); len(errs) > 0 {
		t.Fatalf("schema rejected the placement instance: %v", errs)
	}
}

// validateRawYAMLAgainstSchema validates a settings YAML document with the
// settings JSON schema.
func validateRawYAMLAgainstSchema(t *testing.T, doc string) []ValidationError {
	t.Helper()
	errs, err := ValidateSettings([]byte(doc), "1")
	if err != nil {
		t.Fatal(err)
	}
	return errs
}

// The instance policy, the conversions and the presentation each copy the
// selector map and the toleration slice: changing one never changes
// another.
func TestFlatInstancePlacementDeepCopy(t *testing.T) {
	inst := V1RuntimeBrokerInstanceConfig{Key: "agents", Name: "agents", RuntimeTarget: &V1RuntimeTargetConfig{
		Type: "kubernetes", NodeSelector: map[string]string{PoolNodeSelectorKey: "agents"},
		Tolerations: []api.K8sToleration{poolToleration()}, RuntimeClassName: "gvisor"}}
	policy := NewFlatInstancePolicy(inst)
	global := v1InstancesToGlobal([]V1RuntimeBrokerInstanceConfig{inst})
	back := globalInstancesToV1(global)
	pres := PresentInstance(inst)
	wire := pres.API()

	inst.RuntimeTarget.NodeSelector[PoolNodeSelectorKey] = "changed"
	inst.RuntimeTarget.Tolerations[0].Value = "changed"
	global[0].RuntimeTarget.NodeSelector["extra"] = "x"
	global[0].RuntimeTarget.Tolerations[0].Effect = "NoExecute"
	pres.RuntimeTarget.NodeSelector["extra"] = "x"
	pres.RuntimeTarget.Tolerations[0].Key = "changed"

	for name, got := range map[string]struct {
		sel map[string]string
		tol []api.K8sToleration
	}{
		"policy":       {policy.Target.NodeSelector, policy.Target.Tolerations},
		"converted":    {back[0].RuntimeTarget.NodeSelector, back[0].RuntimeTarget.Tolerations},
		"presentation": {wire.Target.NodeSelector, wire.Target.Tolerations},
	} {
		if !reflect.DeepEqual(got.sel, map[string]string{PoolNodeSelectorKey: "agents"}) ||
			!reflect.DeepEqual(got.tol, []api.K8sToleration{poolToleration()}) {
			t.Errorf("%s shares state with another copy: %v %v", name, got.sel, got.tol)
		}
	}
}

// Placement on a docker target is refused by name at load, by the
// validator and the JSON schema alike.
func TestFlatPlacementRejectsNonKubernetes(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "docker", NodeSelector: map[string]string{"k": "v"},
			Tolerations: []api.K8sToleration{poolToleration()}, RuntimeClassName: "gvisor"}}})
	expectValidationPaths(t, errs, "runtime_target.node_selector", "runtime_target.tolerations", "runtime_target.runtime_class_name")
	for _, e := range errs {
		if e.Message != "field not valid for runtime target type docker" {
			t.Errorf("unexpected message %q at %s", e.Message, e.Path)
		}
	}
	for _, field := range []string{"node_selector: {k: v}", "tolerations: [{key: k, operator: Exists}]", "runtime_class_name: gvisor"} {
		doc := instancesYAML("- {key: a, name: x, runtime_target: {type: docker, " + field + "}}")
		if errs := validateRawYAMLAgainstSchema(t, doc); len(errs) == 0 {
			t.Errorf("schema accepted %s on a docker target", field)
		}
	}
}

// An invalid lower layer is never hidden: an invalid target field is
// refused at load (validator, schema and the strict loader's raw checks),
// and an invalid agent field is refused even when the operator placement
// is empty, or valid and in effect.
func TestPlacementInvalidLowerLayerNotHidden(t *testing.T) {
	errs := ValidateRuntimeBrokerInstances([]V1RuntimeBrokerInstanceConfig{{Key: "a", Name: "n",
		RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes", NodeSelector: map[string]string{"bad key!": "v"},
			Tolerations: []api.K8sToleration{{Operator: "Equal", Value: "x"}}, RuntimeClassName: "Not_A_Name"}}})
	expectValidationPaths(t, errs, "instances[0].runtime_target.node_selector", "instances[0].runtime_target.tolerations",
		"instances[0].runtime_target.runtime_class_name")

	global := flatTestHome(t)
	for name, target := range map[string]string{
		"selector value not a string": "node_selector: {scion.dev/pool: 5}",
		"selector not a mapping":      "node_selector: [a]",
		"tolerations not a list":      "tolerations: {key: a}",
		"toleration seconds":          "tolerations: [{key: a, operator: Exists, tolerationSeconds: 30}]",
		"toleration seconds snake":    "tolerations: [{key: a, operator: Exists, toleration_seconds: 30}]",
		"toleration field not string": "tolerations: [{key: a, value: 1}]",
		"runtime class not a string":  "runtime_class_name: 5",
	} {
		writeFlatTestFile(t, filepath.Join(global, "settings.yaml"),
			instancesYAML("- {key: a, name: x, runtime_target: {type: kubernetes, "+target+"}}"))
		if _, err := LoadRuntimeBrokerInstances(""); err == nil {
			t.Errorf("%s: the strict loader accepted it", name)
		} else if strings.Contains(err.Error(), "30") {
			t.Errorf("%s: the error echoes a submitted value: %v", name, err)
		}
	}

	for name, d := range map[string]*api.ResolvedPlacement{
		"empty operator placement": {OperatorOwned: true},
		"operator placement":       operatorPlacement(),
	} {
		for field, block := range map[string]*api.KubernetesConfig{
			"selector":      {NodeSelector: map[string]string{"bad key!": "v"}},
			"toleration":    {Tolerations: []api.K8sToleration{{Operator: "Bogus"}}},
			"runtime class": {RuntimeClassName: "Not_A_Name"},
		} {
			err := CheckAgentPlacement(block, d, AgentPlacementLayerRequest)
			if err == nil {
				t.Errorf("%s / %s: invalid agent field accepted", name, field)
				continue
			}
			if strings.Contains(err.Error(), "bad key!") || strings.Contains(err.Error(), "Not_A_Name") || strings.Contains(err.Error(), "Bogus") {
				t.Errorf("%s / %s: the error echoes a submitted value: %v", name, field, err)
			}
			if !strings.Contains(err.Error(), "the request") {
				t.Errorf("%s / %s: the error does not name the layer: %v", name, field, err)
			}
		}
	}
}

// An agent may add node selector keys the operator does not set; they
// survive into the merged block next to the operator's keys.
func TestPlacementExtraSelectorSurvives(t *testing.T) {
	got, err := ApplyOperatorPlacement(&api.KubernetesConfig{NodeSelector: map[string]string{"disk": "ssd", PoolNodeSelectorKey: "agents"}},
		operatorPlacement(), AgentPlacementLayerRequest)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"disk": "ssd", PoolNodeSelectorKey: "agents"}; !reflect.DeepEqual(got.NodeSelector, want) {
		t.Fatalf("node selector = %v, want %v", got.NodeSelector, want)
	}
}

// The merged tolerations are the operator's, in order, followed by agent
// tolerations the operator lacks; an agent duplicate (also with an omitted
// operator, which counts as Equal) is not repeated.
func TestPlacementTolerationUnionCanonical(t *testing.T) {
	gpu := api.K8sToleration{Key: "gpu", Operator: "Exists", Effect: "NoSchedule"}
	d := operatorPlacement()
	d.Tolerations = append(d.Tolerations, gpu)
	agentDup := poolToleration()
	agentDup.Operator = ""
	got, err := ApplyOperatorPlacement(&api.KubernetesConfig{Tolerations: []api.K8sToleration{gpu, agentDup}}, d, AgentPlacementLayerRequest)
	if err != nil {
		t.Fatal(err)
	}
	if want := []api.K8sToleration{poolToleration(), gpu}; !reflect.DeepEqual(got.Tolerations, want) {
		t.Fatalf("tolerations = %v, want %v", got.Tolerations, want)
	}
	// Distinct effects stay distinct.
	noExec := poolToleration()
	noExec.Effect = "NoExecute"
	d.Tolerations = append(d.Tolerations, noExec)
	got, err = ApplyOperatorPlacement(nil, d, AgentPlacementLayerRequest)
	if err != nil || len(got.Tolerations) != 3 {
		t.Fatalf("got %v, %v", got, err)
	}
}

// No agent layer may change a node selector key the operator sets: the
// pool key is reported as a pool conflict and every other operator key as
// a selector conflict, each naming the layer and never the values.
func TestAgentCannotChangeOperatorSelectorKey(t *testing.T) {
	d := operatorPlacement()
	d.NodeSelector["tier"] = "gold"
	err := CheckAgentPlacement(&api.KubernetesConfig{NodeSelector: map[string]string{"tier": "silver"}}, d, AgentPlacementLayerStored)
	if err == nil || !strings.Contains(err.Error(), `key "tier"`) || !strings.Contains(err.Error(), "the agent's stored config") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "silver") || strings.Contains(err.Error(), "gold") {
		t.Fatalf("the error echoes values: %v", err)
	}
	// An equal duplicate is a redundant declaration and is accepted.
	if err := CheckAgentPlacement(&api.KubernetesConfig{NodeSelector: map[string]string{"tier": "gold"}}, d, AgentPlacementLayerStored); err != nil {
		t.Fatalf("equal duplicate refused: %v", err)
	}
}

// The pool key conflict is refused at every agent layer a flat start
// checks (the create or start request and the stored config), whatever
// else the block carries.
func TestPlacementPoolConflictAtEachLayer(t *testing.T) {
	for _, layer := range []string{AgentPlacementLayerRequest, AgentPlacementLayerStored, AgentPlacementLayerConfig} {
		err := CheckAgentPlacement(&api.KubernetesConfig{NodeSelector: map[string]string{PoolNodeSelectorKey: "other", "disk": "ssd"},
			Tolerations: []api.K8sToleration{poolToleration()}}, operatorPlacement(), layer)
		if err == nil || !strings.HasPrefix(err.Error(), "pool selector conflicts with inherited placement") || !strings.Contains(err.Error(), layer) {
			t.Errorf("%s: got %v", layer, err)
		}
		if _, err := ApplyOperatorPlacement(&api.KubernetesConfig{NodeSelector: map[string]string{PoolNodeSelectorKey: "other"}}, operatorPlacement(), layer); err == nil {
			t.Errorf("%s: ApplyOperatorPlacement merged a pool conflict", layer)
		}
	}
}

// The runtime class is the operator's: an agent cannot change it.
func TestAgentCannotChangeOperatorRuntimeClass(t *testing.T) {
	err := CheckAgentPlacement(&api.KubernetesConfig{RuntimeClassName: "kata"}, operatorPlacement(), AgentPlacementLayerRequest)
	if err == nil || !strings.Contains(err.Error(), "runtimeClassName") || strings.Contains(err.Error(), "kata") {
		t.Fatalf("got %v", err)
	}
}

// An agent runtime class equal to the operator's is a redundant
// declaration: accepted, and the result is the operator's class.
func TestAgentRuntimeClassEchoAccepted(t *testing.T) {
	got, err := ApplyOperatorPlacement(&api.KubernetesConfig{RuntimeClassName: "gvisor"}, operatorPlacement(), AgentPlacementLayerRequest)
	if err != nil || got.RuntimeClassName != "gvisor" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// An agent runtime class with no operator runtime class is refused, with
// and without other operator placement: the agent cannot choose a class.
func TestAgentRuntimeClassWithoutOperatorValueRejected(t *testing.T) {
	noClass := operatorPlacement()
	noClass.RuntimeClassName = ""
	for name, d := range map[string]*api.ResolvedPlacement{
		"no placement":       {OperatorOwned: true},
		"no runtime class":   noClass,
		"nil operator layer": nil,
	} {
		err := CheckAgentPlacement(&api.KubernetesConfig{RuntimeClassName: "gvisor"}, d, AgentPlacementLayerRequest)
		if err == nil || !strings.Contains(err.Error(), "configures no runtime class") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// Agent tolerations must each be one of the operator's: exact (key,
// operator, value, effect), so a broader operator Exists entry does not
// cover a different tuple. With no operator tolerations, any agent
// toleration is refused, including an empty-key Exists toleration when
// there is no operator placement at all.
func TestAgentTolerationNotInPolicyRejected(t *testing.T) {
	other := api.K8sToleration{Key: "dedicated", Operator: "Equal", Value: "batch", Effect: "NoSchedule"}
	err := CheckAgentPlacement(&api.KubernetesConfig{Tolerations: []api.K8sToleration{poolToleration(), other}}, operatorPlacement(), AgentPlacementLayerRequest)
	if err == nil || !strings.Contains(err.Error(), "kubernetes.tolerations[1]") || strings.Contains(err.Error(), "batch") {
		t.Fatalf("got %v", err)
	}
	broad := operatorPlacement()
	broad.Tolerations = []api.K8sToleration{{Key: "dedicated", Operator: "Exists"}}
	if err := CheckAgentPlacement(&api.KubernetesConfig{Tolerations: []api.K8sToleration{other}}, broad, AgentPlacementLayerRequest); err == nil {
		t.Fatal("an operator Exists entry authorized a different agent tuple")
	}

	// No operator tolerations: refused, with a cause naming the empty case.
	noTolerations := operatorPlacement()
	noTolerations.Tolerations = nil
	_, err = ApplyOperatorPlacement(&api.KubernetesConfig{Tolerations: []api.K8sToleration{other}}, noTolerations, AgentPlacementLayerStored)
	if err == nil || !strings.Contains(err.Error(), "configures no tolerations") || !strings.Contains(err.Error(), "the agent's stored config") ||
		strings.Contains(err.Error(), "batch") {
		t.Fatalf("no operator tolerations: got %v", err)
	}
	// No operator placement at all: an agent match-all toleration is
	// refused too (the match-all guard applies only while placement is in
	// effect; membership applies always).
	for name, d := range map[string]*api.ResolvedPlacement{"empty placement": {OperatorOwned: true}, "nil placement": nil} {
		if err := CheckAgentPlacement(&api.KubernetesConfig{Tolerations: []api.K8sToleration{{Operator: "Exists"}}}, d, AgentPlacementLayerRequest); err == nil ||
			!strings.Contains(err.Error(), "configures no tolerations") {
			t.Errorf("%s: match-all agent toleration got %v", name, err)
		}
	}
}

// The refusal of an agent empty-key Exists toleration stays while
// placement is in effect, even when the operator has the same entry.
func TestPlacementMatchAllGuardRetained(t *testing.T) {
	all := api.K8sToleration{Operator: "Exists"}
	d := operatorPlacement()
	d.Tolerations = append(d.Tolerations, all)
	err := CheckAgentPlacement(&api.KubernetesConfig{Tolerations: []api.K8sToleration{all}}, d, AgentPlacementLayerRequest)
	if err == nil || !strings.Contains(err.Error(), "matches all taints") {
		t.Fatalf("got %v", err)
	}
	// The legacy check keeps its own guard and message.
	if err := CheckLegacyPlacement(&api.KubernetesConfig{Tolerations: []api.K8sToleration{all}}, d); err == nil {
		t.Fatal("legacy guard dropped")
	}
}

// ApplyPlacement dispatches operator-owned placement to
// ApplyOperatorPlacement, even when none is in effect, so an agent runtime
// class with an empty operator policy is refused at Manager.Start too.
func TestApplyPlacementDispatchesOperatorOwned(t *testing.T) {
	if _, _, err := ApplyPlacement(&api.KubernetesConfig{RuntimeClassName: "gvisor"}, &api.ResolvedPlacement{OperatorOwned: true}); err == nil {
		t.Fatal("operator-owned empty placement did not check the agent block")
	}
	got, warnings, err := ApplyPlacement(&api.KubernetesConfig{NodeSelector: map[string]string{"disk": "ssd"}}, operatorPlacement())
	if err != nil || len(warnings) != 0 || got.NodeSelector["disk"] != "ssd" || got.RuntimeClassName != "gvisor" {
		t.Fatalf("got %+v, %v, %v", got, warnings, err)
	}
}

// The legacy path is unchanged: without OperatorOwned, a differing agent
// selector value is ignored with the same warning as before, agent keys are
// kept, tolerations merge placement-first, the agent's equal runtime class
// stays, and an empty placement returns a copy of the agent block. The
// legacy field validation keeps its paths and messages.
func TestApplyPlacementLegacyOutputUnchanged(t *testing.T) {
	d := &api.ResolvedPlacement{
		NodeSelector:     map[string]string{"pool": "a"},
		Tolerations:      []api.K8sToleration{{Key: "pool", Operator: "Equal", Value: "a", Effect: "NoSchedule"}},
		RuntimeClassName: "gvisor",
	}
	base := &api.KubernetesConfig{
		NodeSelector:     map[string]string{"pool": "b", "disk": "ssd"},
		Tolerations:      []api.K8sToleration{{Key: "extra", Operator: "Exists"}},
		RuntimeClassName: "gvisor",
	}
	got, warnings, err := ApplyPlacement(base, d)
	if err != nil {
		t.Fatal(err)
	}
	want := &api.KubernetesConfig{
		NodeSelector: map[string]string{"pool": "a", "disk": "ssd"},
		Tolerations: []api.K8sToleration{{Key: "pool", Operator: "Equal", Value: "a", Effect: "NoSchedule"},
			{Key: "extra", Operator: "Exists"}},
		RuntimeClassName: "gvisor",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy merge = %+v, want %+v", got, want)
	}
	if wantW := []string{`kubernetes.nodeSelector key "pool" in the agent config is ignored; the agent runtime's placement sets this key`}; !reflect.DeepEqual(warnings, wantW) {
		t.Fatalf("warnings = %q", warnings)
	}
	// Legacy still accepts an agent runtime class with no placement.
	if got, _, err := ApplyPlacement(&api.KubernetesConfig{RuntimeClassName: "kata"}, nil); err != nil || got.RuntimeClassName != "kata" {
		t.Fatalf("legacy without placement: %+v, %v", got, err)
	}
	// Legacy still accepts agent tolerations with no operator tolerations:
	// with placement in effect (a selector only) they merge after the
	// placement's; with no placement they are kept as they are. The flat
	// rule refusing them applies only to operator-owned placement.
	agentTols := []api.K8sToleration{{Key: "dedicated", Operator: "Equal", Value: "batch", Effect: "NoSchedule"}}
	selectorOnly := &api.ResolvedPlacement{NodeSelector: map[string]string{"pool": "a"}}
	got, _, err = ApplyPlacement(&api.KubernetesConfig{Tolerations: agentTols}, selectorOnly)
	if err != nil || !reflect.DeepEqual(got.Tolerations, agentTols) || got.NodeSelector["pool"] != "a" {
		t.Fatalf("legacy, selector only, agent tolerations: %+v, %v", got, err)
	}
	for name, d := range map[string]*api.ResolvedPlacement{"nil": nil, "empty": {}} {
		got, _, err := ApplyPlacement(&api.KubernetesConfig{Tolerations: append(agentTols, api.K8sToleration{Operator: "Exists"})}, d)
		if err != nil || len(got.Tolerations) != 2 {
			t.Fatalf("legacy, %s placement, agent tolerations: %+v, %v", name, got, err)
		}
	}
	errs := ValidatePlacement(map[string]V1RuntimeConfig{"k": {RuntimeClassName: "Bad_Name"}}, nil, nil)
	if len(errs) != 1 || errs[0].Path != "runtimes.k.runtime_class_name" ||
		!strings.HasPrefix(errs[0].Message, `runtime class name "Bad_Name": `) {
		t.Fatalf("legacy validation = %+v", errs)
	}
}

// The presented placement is a deep copy at both steps (PresentInstance
// and API), and the marker is set for a Kubernetes target only.
func TestPlacementPresentationDeepCopy(t *testing.T) {
	inst := V1RuntimeBrokerInstanceConfig{Key: "a", Name: "a", RuntimeTarget: &V1RuntimeTargetConfig{
		Type: "kubernetes", NodeSelector: map[string]string{PoolNodeSelectorKey: "agents"},
		Tolerations: []api.K8sToleration{poolToleration()}, RuntimeClassName: "gvisor"}}
	p := PresentInstance(inst)
	wire := p.API()
	if !p.RuntimeTarget.PlacementReported || !wire.Target.PlacementReported {
		t.Fatal("a Kubernetes target must report placement")
	}
	p.RuntimeTarget.NodeSelector[PoolNodeSelectorKey] = "changed"
	p.RuntimeTarget.Tolerations[0].Value = "changed"
	if wire.Target.NodeSelector[PoolNodeSelectorKey] != "agents" || wire.Target.Tolerations[0].Value != "agents" {
		t.Fatal("the wire copy shares state with the presentation")
	}
	if inst.RuntimeTarget.NodeSelector[PoolNodeSelectorKey] != "agents" || inst.RuntimeTarget.Tolerations[0].Value != "agents" {
		t.Fatal("the presentation shares state with the source")
	}
	// A Kubernetes target without placement still reports it (empty); a
	// docker target does not.
	empty := PresentInstance(V1RuntimeBrokerInstanceConfig{Key: "b", Name: "b", RuntimeTarget: &V1RuntimeTargetConfig{Type: "kubernetes"}}).API()
	if !empty.Target.PlacementReported || empty.Target.NodeSelector != nil || empty.Target.Tolerations != nil {
		t.Fatalf("reported empty placement: %+v", empty.Target)
	}
	if docker := PresentInstance(dockerInstance("c", "c")).API(); docker.Target.PlacementReported {
		t.Fatal("a docker target reports placement")
	}
}

// A presentation from an older instance has no placement fields and no
// marker: it decodes as "not reported", never as reported empty placement.
func TestOldPresentationPlacementNotReported(t *testing.T) {
	old := `{"schemaVersion":1,"key":"a","name":"a","target":{"type":"kubernetes","namespace":"agents"}}`
	var p api.RuntimeTargetPresentation
	if err := json.Unmarshal([]byte(old), &p); err != nil {
		t.Fatal(err)
	}
	if p.Target.PlacementReported || p.Target.NodeSelector != nil || p.Target.Tolerations != nil || p.Target.RuntimeClassName != "" {
		t.Fatalf("old presentation decoded as %+v", p.Target)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "placementReported") {
		t.Fatalf("re-encoding an old presentation added the marker: %s", raw)
	}
}
