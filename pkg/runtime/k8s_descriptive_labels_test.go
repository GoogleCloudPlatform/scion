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

package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testHashLabelValue = "sha256:" + strings.Repeat("ab", 32)

func TestFilterDescriptiveLabels(t *testing.T) {
	invalidIdentity := "not:valid/" + strings.Repeat("x", 70)
	in := map[string]string{
		// Descriptive labels: invalid values are dropped, valid ones kept.
		"scion.template":       testHashLabelValue,
		"scion.harness_config": "claude",
		"scion.harness_auth":   "",
		// Identity labels pass through untouched, even when invalid.
		"scion.name":               invalidIdentity,
		"scion.agent":              "true",
		"agent_id":                 "agent:1",
		api.LabelRunID:             "run/1",
		labelStartID:               "start:1",
		projectkeys.LabelProjectID: "proj:1",
		projectkeys.LabelProject:   "my project",
		"some.other.label":         testHashLabelValue,
		"scion.namespace":          "team-a",
	}
	out := filterDescriptiveLabels("a1", in)

	if _, ok := out["scion.template"]; ok {
		t.Errorf("scion.template with value %q should be dropped", testHashLabelValue)
	}
	if out["scion.harness_config"] != "claude" {
		t.Errorf("valid scion.harness_config dropped or changed: %q", out["scion.harness_config"])
	}
	if v, ok := out["scion.harness_auth"]; !ok || v != "" {
		t.Errorf("empty scion.harness_auth should be kept, got %q (present=%v)", v, ok)
	}
	for k, v := range in {
		if descriptiveLabels[k] {
			continue
		}
		if out[k] != v {
			t.Errorf("label %s = %q, want %q unchanged", k, out[k], v)
		}
	}
	if len(out) != len(in)-1 {
		t.Errorf("len(out) = %d, want %d", len(out), len(in)-1)
	}
	if in["scion.template"] != testHashLabelValue {
		t.Error("input map was modified")
	}

	// Each descriptive label is filtered on its own value.
	for k := range descriptiveLabels {
		got := filterDescriptiveLabels("a1", map[string]string{k: "has a space"})
		if _, ok := got[k]; ok {
			t.Errorf("%s with an invalid value was kept", k)
		}
		got = filterDescriptiveLabels("a1", map[string]string{k: "web-dev"})
		if got[k] != "web-dev" {
			t.Errorf("%s with a valid value was dropped", k)
		}
	}
}

// TestRun_DropsInvalidTemplateLabelFromAllObjects runs a start whose
// scion.template label is a content hash and checks that the pod, both
// Secrets and the SecretProviderClass are created without it, with their
// identity labels intact.
func TestRun_DropsInvalidTemplateLabelFromAllObjects(t *testing.T) {
	rt, clientset, _ := newStartCleanupRuntime(t)
	created := notifyOnPodCreate(clientset)

	config := startCleanupConfig()
	config.Labels["scion.template"] = testHashLabelValue
	config.Labels["scion.harness_config"] = "claude"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, rt, config)
	pod := waitPodCreate(t, created)

	check := func(kind string, labels map[string]string) {
		t.Helper()
		if v, ok := labels["scion.template"]; ok {
			t.Errorf("%s has scion.template=%q, want it dropped", kind, v)
		}
		if labels["scion.name"] != "agent" {
			t.Errorf("%s scion.name = %q, want %q", kind, labels["scion.name"], "agent")
		}
		if labels[projectkeys.LabelProjectID] != "proj1" {
			t.Errorf("%s project ID label = %q, want proj1", kind, labels[projectkeys.LabelProjectID])
		}
		if labels[labelStartID] == "" {
			t.Errorf("%s has no start ID label", kind)
		}
		if labels["scion.harness_config"] != "claude" {
			t.Errorf("%s scion.harness_config = %q, want claude", kind, labels["scion.harness_config"])
		}
	}
	check("Pod", pod.Labels)
	bg := context.Background()
	for _, name := range []string{"scion-agent-" + startCleanupAgent, "scion-auth-" + startCleanupAgent} {
		s, err := clientset.CoreV1().Secrets("default").Get(bg, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get Secret %s: %v", name, err)
		}
		check("Secret "+name, s.Labels)
	}
	spc, err := rt.Client.Dynamic().Resource(k8s.SecretProviderClassGVR).Namespace("default").Get(bg, "scion-agent-"+startCleanupAgent, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get SecretProviderClass: %v", err)
	}
	check("SecretProviderClass", spc.GetLabels())
	if config.Labels["scion.template"] != testHashLabelValue {
		t.Error("caller's label map was modified")
	}

	cancel()
	_ = waitRun(t, done)
}
