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
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Tests for ptone/scion#5676: the broker applies the resources sub-field
// entries of cleared_fields at start, emptying that sub-field alone.

// TestApplyInlineConfigUpdate_ResourceSubfieldClear: a cleared
// resources.limits.memory removes the persisted memory limit and keeps the
// cpu limit the same inline config sets and the requests it leaves alone;
// a second start with the same list gives the same file.
func TestApplyInlineConfigUpdate_ResourceSubfieldClear(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "subfield-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude", Resources: &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
		Limits:   api.ResourceList{CPU: "2", Memory: "4Gi"},
		Disk:     "10Gi",
	}})
	want := &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
		Limits:   api.ResourceList{CPU: "3"},
		Disk:     "10Gi",
	}
	for i := 0; i < 2; i++ {
		srv.applyInlineConfigUpdate(context.Background(), agentName, projectDir, &api.ScionConfig{
			Resources:     &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}},
			ClearedFields: []string{"resources.limits.memory"},
		}, false)
		got, raw := readAgentConfig(t, cfgPath)
		if !reflect.DeepEqual(got.Resources, want) {
			t.Errorf("start #%d: resources = %+v, want %+v (%s)", i+1, got.Resources, want, raw)
		}
	}
}

// TestApplyInlineConfigUpdate_NoResourceSubPathsKeepsSubfields is the
// consumer side of the boundary: an inline config that drops a sub-field
// but lists no sub-path (a hub that predates them) keeps the persisted
// sub-field, exactly as before.
func TestApplyInlineConfigUpdate_NoResourceSubPathsKeepsSubfields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "subfield-compat-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude", Resources: &api.ResourceSpec{
		Limits: api.ResourceList{CPU: "2", Memory: "4Gi"},
	}})
	srv.applyInlineConfigUpdate(context.Background(), agentName, projectDir, &api.ScionConfig{
		Resources: &api.ResourceSpec{Limits: api.ResourceList{CPU: "3"}},
	}, false)
	got, _ := readAgentConfig(t, cfgPath)
	want := &api.ResourceSpec{Limits: api.ResourceList{CPU: "3", Memory: "4Gi"}}
	if !reflect.DeepEqual(got.Resources, want) {
		t.Errorf("resources = %+v, want %+v", got.Resources, want)
	}
}

// TestStartAgent_ResourceSubfieldClearUnresolvableTemplate: when the
// agent's template chain does not resolve, a resources sub-field clear is
// still applied (templates never supply resources, so nothing is
// inherited from the chain), while a cleared limit keeps its current value
// as before.
func TestStartAgent_ResourceSubfieldClearUnresolvableTemplate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, mgr := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "subfield-chain-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude"})
	writeAgentInfo(t, projectDir, agentName, api.AgentInfo{Template: "gone"})
	obs := observeStarts(mgr, projectDir, agentName, cfgPath)
	postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"max_turns": 9, "resources": {"requests": {"cpu": "2"}, "limits": {"memory": "4Gi"}}}}`, projectDir))
	lastObservation(t, obs, 1)
	postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"resources": {"requests": {"cpu": "3"}}, "cleared_fields": ["max_turns", "resources.limits.memory"]}}`, projectDir))
	o := lastObservation(t, obs, 2)
	if !api.IntPtrEqual(o.file.MaxTurns, api.IntPtr(9)) {
		t.Errorf("max_turns = %s, want the current 9 kept on a chain miss", api.FormatCountLimit(o.file.MaxTurns))
	}
	want := &api.ResourceSpec{Requests: api.ResourceList{CPU: "3"}}
	if !reflect.DeepEqual(o.file.Resources, want) {
		t.Errorf("resources = %+v, want %+v (memory cleared, cpu from the inline config)", o.file.Resources, want)
	}
}

// TestStaticCapabilities_ClearedResourceSubfields: this broker advertises
// that it applies resources sub-field clears, so the hub reports them
// applied at the next start for its agents.
func TestStaticCapabilities_ClearedResourceSubfields(t *testing.T) {
	if !StaticCapabilities(nil, "").ClearedResourceSubfields {
		t.Fatal("StaticCapabilities: ClearedResourceSubfields = false")
	}
	found := false
	for _, n := range StaticCapabilityNames(nil) {
		if n == "clearedResourceSubfields" {
			found = true
		}
	}
	if !found {
		t.Fatal("StaticCapabilityNames lacks clearedResourceSubfields")
	}
}
