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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

func k8sEntry(ns string, sel map[string]string) V1RuntimeConfig {
	return V1RuntimeConfig{Type: "kubernetes", Context: "ctx", Namespace: ns, NodeSelector: sel}
}

func vePaths(vs []ValidationError) string {
	var p []string
	for _, v := range vs {
		p = append(p, v.Path)
	}
	return strings.Join(p, ",")
}

func TestRuntimeSaveChecks_FieldValues(t *testing.T) {
	good := k8sEntry("a", map[string]string{"scion.dev/pool": "agents"})
	good.Tolerations = []api.K8sToleration{{Key: "scion.dev/pool", Operator: "Equal", Value: "agents", Effect: "NoSchedule"}}
	good.RuntimeClassName = "gvisor"
	good.KubernetesServiceAccountMappings = map[string]string{"sa@p.iam.gserviceaccount.com": "agent-ksa"}
	good.KubernetesBlockServiceAccount = "block-ksa"
	bad := good
	bad.NodeSelector = map[string]string{"bad key!": "v"}
	bad.Tolerations = []api.K8sToleration{{Key: "k", Operator: "Exists", Value: "x"}}
	bad.RuntimeClassName = "Not_Valid"
	bad.KubernetesServiceAccountMappings = map[string]string{"NotAnEmail": "ksa"}
	bad.KubernetesBlockServiceAccount = "Bad_Name"
	all := "runtimes.gke.kubernetes_block_service_account,runtimes.gke.kubernetes_service_account_mappings,runtimes.gke.node_selector,runtimes.gke.runtime_class_name,runtimes.gke.tolerations"

	t.Run("valid entry passes", func(t *testing.T) {
		errs, warns := RuntimeSaveChecks(nil, map[string]V1RuntimeConfig{"gke": good}, nil, nil)
		if len(errs) != 0 || len(warns) != 0 {
			t.Fatalf("errs %v warns %v", errs, warns)
		}
	})
	t.Run("invalid values the save sets are refused", func(t *testing.T) {
		errs, warns := RuntimeSaveChecks(map[string]V1RuntimeConfig{"gke": good}, map[string]V1RuntimeConfig{"gke": bad}, nil, nil)
		if vePaths(errs) != all || len(warns) != 0 {
			t.Fatalf("errs %v warns %v", vePaths(errs), warns)
		}
	})
	t.Run("invalid values on a new entry are refused", func(t *testing.T) {
		errs, _ := RuntimeSaveChecks(nil, map[string]V1RuntimeConfig{"gke": bad}, nil, nil)
		if vePaths(errs) != all {
			t.Fatalf("errs %v", vePaths(errs))
		}
	})
	t.Run("stored invalid values the save does not touch are warnings", func(t *testing.T) {
		after := bad
		after.Host = "other" // an unrelated field changes
		errs, warns := RuntimeSaveChecks(map[string]V1RuntimeConfig{"gke": bad}, map[string]V1RuntimeConfig{"gke": after}, nil, nil)
		if len(errs) != 0 || vePaths(warns) != all {
			t.Fatalf("errs %v warns %v", vePaths(errs), vePaths(warns))
		}
	})
	t.Run("profiles follow the same rule", func(t *testing.T) {
		p := V1ProfileConfig{Runtime: "gke", RuntimeClassName: "Not_Valid"}
		errs, _ := RuntimeSaveChecks(nil, nil, nil, map[string]V1ProfileConfig{"p": p})
		if vePaths(errs) != "profiles.p.runtime_class_name" {
			t.Fatalf("errs %v", vePaths(errs))
		}
		errs, warns := RuntimeSaveChecks(nil, nil, map[string]V1ProfileConfig{"p": p}, map[string]V1ProfileConfig{"p": p})
		if len(errs) != 0 || vePaths(warns) != "profiles.p.runtime_class_name" {
			t.Fatalf("errs %v warns %v", vePaths(errs), vePaths(warns))
		}
	})
}

func TestRuntimeSaveChecks_Siblings(t *testing.T) {
	a := k8sEntry("agents", map[string]string{"pool": "a"})
	b := k8sEntry("agents", map[string]string{"pool": "a"})
	bDiff := k8sEntry("agents", map[string]string{"pool": "b"})
	other := k8sEntry("other-ns", map[string]string{"pool": "b"})

	t.Run("agreeing siblings pass", func(t *testing.T) {
		errs, warns := RuntimeSaveChecks(nil, map[string]V1RuntimeConfig{"a": a, "b": b}, nil, nil)
		if len(errs)+len(warns) != 0 {
			t.Fatalf("errs %v warns %v", errs, warns)
		}
	})
	t.Run("a change that creates a disagreement is refused", func(t *testing.T) {
		before := map[string]V1RuntimeConfig{"a": a, "b": b}
		errs, _ := RuntimeSaveChecks(before, map[string]V1RuntimeConfig{"a": a, "b": bDiff}, nil, nil)
		if vePaths(errs) != "runtimes.a" || !strings.Contains(errs[0].Message, "same cluster and namespace") {
			t.Fatalf("errs %v", errs)
		}
	})
	t.Run("adding an entry that disagrees is refused", func(t *testing.T) {
		errs, _ := RuntimeSaveChecks(map[string]V1RuntimeConfig{"a": a}, map[string]V1RuntimeConfig{"a": a, "b": bDiff}, nil, nil)
		if vePaths(errs) != "runtimes.a" {
			t.Fatalf("errs %v", errs)
		}
	})
	t.Run("moving an entry into a disagreeing group is refused", func(t *testing.T) {
		moved := other
		moved.Namespace = "agents"
		errs, _ := RuntimeSaveChecks(map[string]V1RuntimeConfig{"a": a, "o": other}, map[string]V1RuntimeConfig{"a": a, "o": moved}, nil, nil)
		if vePaths(errs) != "runtimes.a" {
			t.Fatalf("errs %v", errs)
		}
	})
	t.Run("an untouched stored disagreement is a warning", func(t *testing.T) {
		stored := map[string]V1RuntimeConfig{"a": a, "b": bDiff, "o": other}
		after := map[string]V1RuntimeConfig{"a": a, "b": bDiff, "o": other}
		o2 := other
		o2.GKE = true // an unrelated edit on another entry
		after["o"] = o2
		errs, warns := RuntimeSaveChecks(stored, after, nil, nil)
		if len(errs) != 0 || vePaths(warns) != "runtimes.a" {
			t.Fatalf("errs %v warns %v", errs, warns)
		}
	})
	t.Run("fixing a stored disagreement in the same save is accepted", func(t *testing.T) {
		errs, warns := RuntimeSaveChecks(map[string]V1RuntimeConfig{"a": a, "b": bDiff}, map[string]V1RuntimeConfig{"a": a, "b": b}, nil, nil)
		if len(errs)+len(warns) != 0 {
			t.Fatalf("errs %v warns %v", errs, warns)
		}
	})
}

// The three cases for each identity field: a new invalid value is refused,
// an untouched stored invalid value is a warning, and fixing it in the same
// save is accepted.
func TestRuntimeSaveChecks_IdentityFieldThreeCases(t *testing.T) {
	type tc struct {
		name, path  string
		bad, good   V1RuntimeConfig
		badP, goodP *V1ProfileConfig
	}
	cases := []tc{
		{name: "runtime priority class", path: "runtimes.k.priority_class_name",
			bad: V1RuntimeConfig{Type: "kubernetes", PriorityClassName: "Bad_Name"}, good: V1RuntimeConfig{Type: "kubernetes", PriorityClassName: "agents"}},
		{name: "runtime mappings", path: "runtimes.k.kubernetes_service_account_mappings",
			bad:  V1RuntimeConfig{Type: "kubernetes", KubernetesServiceAccountMappings: map[string]string{"NotAnEmail": "ksa"}},
			good: V1RuntimeConfig{Type: "kubernetes", KubernetesServiceAccountMappings: map[string]string{"sa@p.iam.gserviceaccount.com": "ksa"}}},
		{name: "runtime block SA", path: "runtimes.k.kubernetes_block_service_account",
			bad: V1RuntimeConfig{Type: "kubernetes", KubernetesBlockServiceAccount: "Bad_Name"}, good: V1RuntimeConfig{Type: "kubernetes", KubernetesBlockServiceAccount: "block-ksa"}},
		{name: "profile mappings", path: "profiles.k.kubernetes_service_account_mappings",
			badP:  &V1ProfileConfig{Runtime: "k", KubernetesServiceAccountMappings: map[string]string{"sa@p.iam.gserviceaccount.com": "Bad_Name"}},
			goodP: &V1ProfileConfig{Runtime: "k", KubernetesServiceAccountMappings: map[string]string{"sa@p.iam.gserviceaccount.com": "ksa"}}},
		{name: "profile block SA", path: "profiles.k.kubernetes_block_service_account",
			badP: &V1ProfileConfig{Runtime: "k", KubernetesBlockServiceAccount: "Bad_Name"}, goodP: &V1ProfileConfig{Runtime: "k", KubernetesBlockServiceAccount: "block-ksa"}},
	}
	for _, c := range cases {
		run := func(before, after any) ([]ValidationError, []ValidationError) {
			if c.badP != nil {
				var bp, ap map[string]V1ProfileConfig
				if before != nil {
					bp = map[string]V1ProfileConfig{"k": *before.(*V1ProfileConfig)}
				}
				ap = map[string]V1ProfileConfig{"k": *after.(*V1ProfileConfig)}
				return RuntimeSaveChecks(nil, nil, bp, ap)
			}
			var br map[string]V1RuntimeConfig
			if before != nil {
				br = map[string]V1RuntimeConfig{"k": before.(V1RuntimeConfig)}
			}
			return RuntimeSaveChecks(br, map[string]V1RuntimeConfig{"k": after.(V1RuntimeConfig)}, nil, nil)
		}
		var bad, good any = c.bad, c.good
		if c.badP != nil {
			bad, good = c.badP, c.goodP
		}
		t.Run(c.name+"/new invalid refused", func(t *testing.T) {
			errs, _ := run(nil, bad)
			if vePaths(errs) != c.path {
				t.Fatalf("errs %v", errs)
			}
		})
		t.Run(c.name+"/untouched stored invalid warned", func(t *testing.T) {
			errs, warns := run(bad, bad)
			if len(errs) != 0 || vePaths(warns) != c.path {
				t.Fatalf("errs %v warns %v", errs, warns)
			}
		})
		t.Run(c.name+"/fixed in the same save accepted", func(t *testing.T) {
			errs, warns := run(bad, good)
			if len(errs)+len(warns) != 0 {
				t.Fatalf("errs %v warns %v", errs, warns)
			}
		})
	}
}
