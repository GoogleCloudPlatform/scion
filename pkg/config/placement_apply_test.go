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
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

var placementTolerationEffects = []string{"", "NoSchedule", "PreferNoSchedule", "NoExecute"}

func requireRefusal(t *testing.T, base *api.KubernetesConfig, d *api.ResolvedPlacement, want string) {
	t.Helper()
	checkErr := CheckLegacyPlacement(base, d)
	if checkErr == nil || checkErr.Error() != want {
		t.Fatalf("CheckLegacyPlacement = %v, want %q", checkErr, want)
	}
	out, warnings, err := ApplyPlacement(base, d)
	if err == nil || err.Error() != want {
		t.Fatalf("ApplyPlacement error = %v, want %q", err, want)
	}
	if out != nil || warnings != nil {
		t.Fatalf("ApplyPlacement on refusal returned out=%+v warnings=%v, want nil", out, warnings)
	}
}

func requireApply(t *testing.T, base *api.KubernetesConfig, d *api.ResolvedPlacement) (*api.KubernetesConfig, []string) {
	t.Helper()
	if err := CheckLegacyPlacement(base, d); err != nil {
		t.Fatalf("CheckLegacyPlacement = %v, want nil", err)
	}
	out, warnings, err := ApplyPlacement(base, d)
	if err != nil {
		t.Fatalf("ApplyPlacement = %v, want nil", err)
	}
	return out, warnings
}

// E1: runtime-class-only placement and a differing agent runtimeClassName
// are refused.
func TestApplyPlacement_E1_RuntimeClassOnlyDifferingAgentClass(t *testing.T) {
	requireRefusal(t,
		&api.KubernetesConfig{RuntimeClassName: "kata"},
		&api.ResolvedPlacement{RuntimeClassName: "gvisor"},
		legacyRuntimeClassCause)
}

// E2: runtime-class-only placement and an agent empty-key Exists toleration
// are refused.
func TestApplyPlacement_E2_RuntimeClassOnlyMatchAllToleration(t *testing.T) {
	requireRefusal(t,
		&api.KubernetesConfig{Tolerations: []api.K8sToleration{{Operator: "Exists"}}},
		&api.ResolvedPlacement{RuntimeClassName: "gvisor"},
		strings.Replace(legacyMatchAllTolerationCause, "%d", "0", 1))
}

// E4: nothing set (not in effect): the agent runtimeClassName applies as
// today.
func TestApplyPlacement_E4_NotInEffectAgentRuntimeClass(t *testing.T) {
	for _, d := range []*api.ResolvedPlacement{nil, {}, {Sources: map[string][]string{"x": {"y"}}}} {
		out, warnings := requireApply(t, &api.KubernetesConfig{RuntimeClassName: "kata"}, d)
		if out == nil || out.RuntimeClassName != "kata" {
			t.Fatalf("ApplyPlacement(%+v) runtime class = %+v, want kata", d, out)
		}
		if warnings != nil {
			t.Fatalf("ApplyPlacement(%+v) warnings = %v, want none", d, warnings)
		}
	}
	out, warnings, err := ApplyPlacement(nil, nil)
	if out != nil || warnings != nil || err != nil {
		t.Fatalf("ApplyPlacement(nil, nil) = %+v, %v, %v; want all nil", out, warnings, err)
	}
}

// (18): an empty-key Exists toleration with any effect is refused while
// placement is in effect; it is kept, with no warnings from ApplyPlacement,
// when not in effect.
func TestApplyPlacement_18_MatchAllTolerationEveryEffect(t *testing.T) {
	inEffect := &api.ResolvedPlacement{NodeSelector: map[string]string{"pool": "agents"}}
	for _, effect := range placementTolerationEffects {
		t.Run("effect="+effect, func(t *testing.T) {
			base := &api.KubernetesConfig{Tolerations: []api.K8sToleration{
				{Key: "dedicated", Operator: "Exists"},
				{Operator: "Exists", Effect: effect},
			}}
			requireRefusal(t, base, inEffect, strings.Replace(legacyMatchAllTolerationCause, "%d", "1", 1))

			out, warnings := requireApply(t, base, nil)
			if !reflect.DeepEqual(out.Tolerations, base.Tolerations) {
				t.Fatalf("not in effect: tolerations = %v, want %v", out.Tolerations, base.Tolerations)
			}
			if warnings != nil {
				t.Fatalf("not in effect: warnings = %v, want none", warnings)
			}
		})
	}
}

// (19): key-specific Exists tolerations, including one on the default's own
// taint key, are kept.
func TestApplyPlacement_19_KeySpecificExistsKept(t *testing.T) {
	d := &api.ResolvedPlacement{
		NodeSelector: map[string]string{"pool": "agents"},
		Tolerations:  []api.K8sToleration{{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"}},
	}
	agent := []api.K8sToleration{
		{Key: "dedicated", Operator: "Exists"},
		{Key: "nvidia.com/gpu", Operator: "Exists", Effect: "NoSchedule"},
	}
	out, _ := requireApply(t, &api.KubernetesConfig{Tolerations: agent}, d)
	want := []api.K8sToleration{d.Tolerations[0], agent[0], agent[1]}
	if !reflect.DeepEqual(out.Tolerations, want) {
		t.Fatalf("tolerations = %v, want %v", out.Tolerations, want)
	}
}

// (20): an agent-only selector key is added; a key the default sets keeps
// the default's value, with a warning when the agent value differs.
func TestApplyPlacement_20_DefaultSelectorKeysWin(t *testing.T) {
	d := &api.ResolvedPlacement{NodeSelector: map[string]string{
		"cloud.google.com/gke-nodepool": "scion-agents",
		"tier":                          "standard",
	}}
	base := &api.KubernetesConfig{NodeSelector: map[string]string{
		"cloud.google.com/gke-nodepool": "other-pool", // differs: ignored with a warning
		"tier":                          "standard",   // equal: no warning
		"disk":                          "ssd",        // agent-only: added
	}}
	out, warnings := requireApply(t, base, d)
	want := map[string]string{
		"cloud.google.com/gke-nodepool": "scion-agents",
		"tier":                          "standard",
		"disk":                          "ssd",
	}
	if !reflect.DeepEqual(out.NodeSelector, want) {
		t.Fatalf("node selector = %v, want %v", out.NodeSelector, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `"cloud.google.com/gke-nodepool"`) {
		t.Fatalf("warnings = %v, want one naming cloud.google.com/gke-nodepool", warnings)
	}
}

// (21): a tolerations-only default still refuses a match-all agent
// toleration.
func TestApplyPlacement_21_TolerationsOnlyDefaultRefusesMatchAll(t *testing.T) {
	requireRefusal(t,
		&api.KubernetesConfig{Tolerations: []api.K8sToleration{{Operator: "Exists", Effect: "NoSchedule"}}},
		&api.ResolvedPlacement{Tolerations: []api.K8sToleration{{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"}}},
		strings.Replace(legacyMatchAllTolerationCause, "%d", "0", 1))
}

// (22): runtimeClassName: differing is an error; set while the runtime-level
// value is unset (placement in effect) is an error; equal is accepted; agent
// unset uses the runtime-level value.
func TestApplyPlacement_22_RuntimeClassRules(t *testing.T) {
	t.Run("differing", func(t *testing.T) {
		requireRefusal(t,
			&api.KubernetesConfig{RuntimeClassName: "kata"},
			&api.ResolvedPlacement{RuntimeClassName: "gvisor", NodeSelector: map[string]string{"pool": "a"}},
			legacyRuntimeClassCause)
	})
	t.Run("set while runtime-level unset", func(t *testing.T) {
		requireRefusal(t,
			&api.KubernetesConfig{RuntimeClassName: "kata"},
			&api.ResolvedPlacement{NodeSelector: map[string]string{"pool": "a"}},
			legacyRuntimeClassCause)
	})
	t.Run("equal", func(t *testing.T) {
		out, _ := requireApply(t,
			&api.KubernetesConfig{RuntimeClassName: "gvisor"},
			&api.ResolvedPlacement{RuntimeClassName: "gvisor"})
		if out.RuntimeClassName != "gvisor" {
			t.Fatalf("runtime class = %q, want gvisor", out.RuntimeClassName)
		}
	})
	t.Run("agent unset", func(t *testing.T) {
		for _, base := range []*api.KubernetesConfig{nil, {}} {
			out, _ := requireApply(t, base, &api.ResolvedPlacement{RuntimeClassName: "gvisor"})
			if out == nil || out.RuntimeClassName != "gvisor" {
				t.Fatalf("base %+v: runtime class = %+v, want gvisor", base, out)
			}
		}
	})
	t.Run("errors are fixed causes", func(t *testing.T) {
		err := CheckLegacyPlacement(&api.KubernetesConfig{RuntimeClassName: "kata"}, &api.ResolvedPlacement{RuntimeClassName: "gvisor"})
		if errors.Unwrap(err) != nil || strings.Contains(err.Error(), "kata") || strings.Contains(err.Error(), "gvisor") {
			t.Fatalf("error %q should be the fixed cause without values", err)
		}
	})
}

// (23): when placement is not in effect, legacy values pass through
// unchanged, duplicates included.
func TestApplyPlacement_23_NotInEffectLegacyUnchanged(t *testing.T) {
	base := &api.KubernetesConfig{
		RuntimeClassName: "kata",
		NodeSelector:     map[string]string{"pool": "agents"},
		Tolerations: []api.K8sToleration{
			{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
			{Key: "dedicated", Operator: "", Value: "agents", Effect: "NoSchedule"},
			{Key: "dedicated", Operator: "Equal", Value: "agents", Effect: "NoSchedule"},
			{Operator: "Exists"},
		},
	}
	for _, d := range []*api.ResolvedPlacement{nil, {}} {
		out, warnings := requireApply(t, base, d)
		if !reflect.DeepEqual(out, base) {
			t.Fatalf("ApplyPlacement(%+v) = %+v, want %+v", d, out, base)
		}
		if warnings != nil {
			t.Fatalf("warnings = %v, want none", warnings)
		}
	}
}
