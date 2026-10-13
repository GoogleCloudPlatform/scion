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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func readAgentConfig(t *testing.T, cfgPath string) (api.ScionConfig, string) {
	t.Helper()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("failed to read scion-agent.json: %v", err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("failed to parse scion-agent.json: %v", err)
	}
	return cfg, string(data)
}

// setInline is the inline config of an agent whose owner set the fields the
// configure form edits.
func setInline() *api.ScionConfig {
	return &api.ScionConfig{
		MaxTurns:      9,
		MaxModelCalls: 40,
		ThinkingLevel: intPtr(3),
		Resources:     &api.ResourceSpec{Requests: api.ResourceList{CPU: "2"}},
		SystemPrompt:  "be terse",
		Env:           map[string]string{"FOO": "set", "BAR": "kept"},
	}
}

// clearedInline is setInline after the owner cleared max_turns,
// thinking_level, resources, system_prompt and the env key FOO: the hub's
// inline config no longer holds them, and lists them as cleared.
func clearedInline(cleared bool) *api.ScionConfig {
	c := &api.ScionConfig{
		MaxModelCalls: 40,
		Env:           map[string]string{"BAR": "kept"},
	}
	if cleared {
		c.ClearedFields = []string{"env.FOO", "max_turns", "resources", "system_prompt", "thinking_level"}
	}
	return c
}

// TestApplyInlineConfigUpdate_ClearedFieldsRevertToInherited: set, start,
// then clear, start; the cleared fields are removed from scion-agent.json,
// so the start re-derives them from the template, and stay removed on the
// starts and restarts that follow.
func TestApplyInlineConfigUpdate_ClearedFieldsRevertToInherited(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "cleared-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude"})

	srv.applyInlineConfigUpdate(agentName, projectDir, setInline(), false)
	got, _ := readAgentConfig(t, cfgPath)
	if got.MaxTurns != 9 || got.ThinkingLevel == nil || got.Resources == nil || got.SystemPrompt == "" || got.Env["FOO"] != "set" {
		t.Fatalf("fixture: set fields not applied: %+v", got)
	}

	for i := 0; i < 2; i++ { // the start after the clear, then a second start
		srv.applyInlineConfigUpdate(agentName, projectDir, clearedInline(true), false)
		got, raw := readAgentConfig(t, cfgPath)
		if got.MaxTurns != 0 {
			t.Errorf("start #%d: max_turns = %d, want cleared", i+1, got.MaxTurns)
		}
		if got.ThinkingLevel != nil {
			t.Errorf("start #%d: thinking_level = %d, want cleared", i+1, *got.ThinkingLevel)
		}
		if got.Resources != nil {
			t.Errorf("start #%d: resources = %+v, want cleared", i+1, got.Resources)
		}
		if got.SystemPrompt != "" {
			t.Errorf("start #%d: system_prompt = %q, want cleared", i+1, got.SystemPrompt)
		}
		if _, ok := got.Env["FOO"]; ok {
			t.Errorf("start #%d: env FOO still set: %v", i+1, got.Env)
		}
		if got.Env["BAR"] != "kept" || got.MaxModelCalls != 40 || got.Harness != "claude" {
			t.Errorf("start #%d: a field that was not cleared changed: %+v", i+1, got)
		}
		if strings.Contains(raw, "cleared_fields") {
			t.Errorf("start #%d: cleared_fields written to scion-agent.json: %s", i+1, raw)
		}
	}
}

// TestApplyInlineConfigUpdate_NoClearedFieldsKeepsPreviousValues: without
// ClearedFields (a hub that does not send it) the start keeps the previous
// values, exactly as before the field existed.
func TestApplyInlineConfigUpdate_NoClearedFieldsKeepsPreviousValues(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "compat-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude"})

	srv.applyInlineConfigUpdate(agentName, projectDir, setInline(), false)
	want, _ := readAgentConfig(t, cfgPath)

	srv.applyInlineConfigUpdate(agentName, projectDir, clearedInline(false), false)
	got, _ := readAgentConfig(t, cfgPath)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("without ClearedFields the start changed the config\n got: %+v\nwant: %+v", got, want)
	}
}

// TestApplyInlineConfigUpdate_IgnoresUnclearablePaths: a path the broker
// does not clear (fixed keys, whole collections, telemetry, unknown keys)
// leaves the config as it was.
func TestApplyInlineConfigUpdate_IgnoresUnclearablePaths(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "ignored-agent"
	persisted := api.ScionConfig{
		Harness:   "claude",
		Volumes:   []api.VolumeMount{{Source: "/src", Target: "/workspace"}},
		Env:       map[string]string{"FOO": "x"},
		Telemetry: &api.TelemetryConfig{},
		MaxTurns:  4,
	}
	cfgPath := writeAgentConfig(t, projectDir, agentName, persisted)
	want, _ := readAgentConfig(t, cfgPath)

	srv.applyInlineConfigUpdate(agentName, projectDir, &api.ScionConfig{
		ClearedFields: []string{"harness", "volumes", "env", "env.", "telemetry", "no_such_key", "cleared_fields"},
	}, false)
	got, _ := readAgentConfig(t, cfgPath)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unclearable paths changed the config\n got: %+v\nwant: %+v", got, want)
	}
}

// TestApplyInlineConfigUpdate_VolumesDoNotGrow: the hub sends the agent's
// inline volumes on every start; across three starts the persisted list
// holds each target once, and an inline volume replaces a persisted one
// at the same target instead of mounting twice.
func TestApplyInlineConfigUpdate_VolumesDoNotGrow(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "volumes-agent"
	inline := &api.ScionConfig{Volumes: []api.VolumeMount{
		{Source: "/data", Target: "/mnt/data"},
		{Source: "/override", Target: "/mnt/default", ReadOnly: true},
	}}
	// Provisioned: a default mount, then the create-time inline volumes.
	provisioned := config.MergeScionConfig(&api.ScionConfig{
		Harness: "claude",
		Volumes: []api.VolumeMount{
			{Source: "/default", Target: "/mnt/default"},
			{Source: "/cache", Target: "/mnt/cache"},
		},
	}, &api.ScionConfig{Volumes: inline.Volumes[:1]})
	cfgPath := writeAgentConfig(t, projectDir, agentName, *provisioned)

	want := []api.VolumeMount{
		{Source: "/override", Target: "/mnt/default", ReadOnly: true},
		{Source: "/cache", Target: "/mnt/cache"},
		{Source: "/data", Target: "/mnt/data"},
	}
	for i := 0; i < 3; i++ {
		srv.applyInlineConfigUpdate(agentName, projectDir, inline, false)
		got, _ := readAgentConfig(t, cfgPath)
		if !reflect.DeepEqual(got.Volumes, want) {
			t.Fatalf("after start #%d: volumes\n got: %+v\nwant: %+v", i+1, got.Volumes, want)
		}
	}
}

func TestDedupeVolumesByTarget(t *testing.T) {
	in := []api.VolumeMount{
		{Source: "a", Target: "/x"},
		{Type: "gcs", Bucket: "b"},
		{Source: "c", Target: "/y"},
		{Source: "d", Target: "/x"},
		{Type: "gcs", Bucket: "b"},
	}
	want := []api.VolumeMount{
		{Source: "d", Target: "/x"},
		{Type: "gcs", Bucket: "b"},
		{Source: "c", Target: "/y"},
		{Type: "gcs", Bucket: "b"},
	}
	if got := dedupeVolumesByTarget(in); !reflect.DeepEqual(got, want) {
		t.Errorf("dedupeVolumesByTarget\n got: %+v\nwant: %+v", got, want)
	}
	if got := dedupeVolumesByTarget(nil); got != nil {
		t.Errorf("nil input: got %+v", got)
	}
}

// TestStartAgent_ClearedFieldsAppliedBeforeStart: the start request's
// inline cleared_fields remove the fields from scion-agent.json before the
// manager starts the agent, and are not written to disk.
func TestStartAgent_ClearedFieldsAppliedBeforeStart(t *testing.T) {
	srv, mgr := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "cleared-start-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{
		Harness:       "gemini",
		MaxTurns:      3,
		ThinkingLevel: intPtr(2),
		Env:           map[string]string{"FOO": "old", "BAR": "kept"},
	})

	body := fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"env": {"BAR": "kept"},
			"cleared_fields": ["max_turns", "thinking_level", "env.FOO"]
		}
	}`, projectDir)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
	}
	if !mgr.startCalled {
		t.Fatal("expected Start to be called")
	}
	got, raw := readAgentConfig(t, cfgPath)
	if got.MaxTurns != 0 || got.ThinkingLevel != nil {
		t.Errorf("cleared limits kept: max_turns=%d thinking_level=%v", got.MaxTurns, got.ThinkingLevel)
	}
	if _, ok := got.Env["FOO"]; ok || got.Env["BAR"] != "kept" {
		t.Errorf("env = %v, want only BAR", got.Env)
	}
	if strings.Contains(raw, "cleared_fields") {
		t.Errorf("cleared_fields written to scion-agent.json: %s", raw)
	}
}
