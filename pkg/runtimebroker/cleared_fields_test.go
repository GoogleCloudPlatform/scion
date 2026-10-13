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
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
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
	t.Setenv("HOME", t.TempDir())
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
	t.Setenv("HOME", t.TempDir())
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
		{Source: "ws", Target: "/workspace"},
		{Source: "ws2", Target: "/workspace/"},
	}
	want := []api.VolumeMount{
		{Source: "d", Target: "/x"},
		{Type: "gcs", Bucket: "b"},
		{Source: "c", Target: "/y"},
		{Type: "gcs", Bucket: "b"},
		{Source: "ws", Target: "/workspace"},
	}
	if got := dedupeVolumesByTarget(in); !reflect.DeepEqual(got, want) {
		t.Errorf("dedupeVolumesByTarget\n got: %+v\nwant: %+v", got, want)
	}
	if got := dedupeVolumesByTarget(nil); got != nil {
		t.Errorf("nil input: got %+v", got)
	}
}

// TestStartAgent_ClearedFieldsAppliedBeforeStart: the start request's
// inline cleared_fields are removed from scion-agent.json before the
// manager starts the agent (observed inside Start), and are not written to
// disk.
func TestStartAgent_ClearedFieldsAppliedBeforeStart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, mgr := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "cleared-start-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{
		Harness:       "gemini",
		MaxTurns:      3,
		ThinkingLevel: intPtr(2),
		Env:           map[string]string{"FOO": "old", "BAR": "kept"},
	})
	obs := observeStarts(mgr, projectDir, agentName, cfgPath)

	postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{
		"projectPath": %q,
		"inlineConfig": {
			"env": {"BAR": "kept"},
			"cleared_fields": ["max_turns", "thinking_level", "env.FOO"]
		}
	}`, projectDir))

	o := lastObservation(t, obs, 1)
	if o.file.MaxTurns != 0 || o.file.ThinkingLevel != nil {
		t.Errorf("cleared limits kept: max_turns=%d thinking_level=%v", o.file.MaxTurns, o.file.ThinkingLevel)
	}
	if _, ok := o.file.Env["FOO"]; ok || o.file.Env["BAR"] != "kept" {
		t.Errorf("env = %v, want only BAR", o.file.Env)
	}
	if strings.Contains(o.raw, "cleared_fields") {
		t.Errorf("cleared_fields written to scion-agent.json: %s", o.raw)
	}
}

// clearedStartObservation is what the agent's start read: scion-agent.json
// as the manager's Start found it, and the config GetAgent derives from it.
type clearedStartObservation struct {
	file      api.ScionConfig
	raw       string
	effective *api.ScionConfig
	err       error
}

// observeStarts makes mgr record, inside each Start, scion-agent.json and
// the config GetAgent derives for the agent.
func observeStarts(mgr *provisionCapturingManager, projectDir, agentName, cfgPath string) *[]clearedStartObservation {
	var obs []clearedStartObservation
	mgr.onStart = func(ctx context.Context, opts api.StartOptions) {
		var o clearedStartObservation
		data, err := os.ReadFile(cfgPath)
		if err == nil {
			o.raw = string(data)
			err = json.Unmarshal(data, &o.file)
		}
		if err == nil {
			_, _, _, o.effective, err = agent.GetAgent(ctx, agentName, "", "", "", projectDir, "", "", "", "")
		}
		o.err = err
		obs = append(obs, o)
	}
	return &obs
}

// writeAgentInfo writes the agent's agent-info.json, which names its
// template.
func writeAgentInfo(t *testing.T, projectDir, agentName string, info api.AgentInfo) {
	t.Helper()
	home := config.GetAgentHomePath(projectDir, agentName)
	if err := os.MkdirAll(home, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "agent-info.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func postClearedAgentAction(t *testing.T, srv *Server, agentName, action, body string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentName+"/"+action, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code >= 300 {
		t.Fatalf("%s: status %d: %s", action, w.Code, w.Body.String())
	}
}

// lastObservation returns the latest start's observation, failing on a
// read error.
func lastObservation(t *testing.T, obs *[]clearedStartObservation, wantStarts int) clearedStartObservation {
	t.Helper()
	if len(*obs) != wantStarts {
		t.Fatalf("starts observed = %d, want %d", len(*obs), wantStarts)
	}
	o := (*obs)[wantStarts-1]
	if o.err != nil {
		t.Fatalf("start %d: %v", wantStarts, o.err)
	}
	return o
}

// TestStartAgent_ClearedLimitInheritsTemplateByName: a template resolved by
// name sets max_turns 5; the owner sets 9, then clears it. The start after
// the clear reads a scion-agent.json without max_turns, GetAgent derives
// the template's 5, and a restart, which carries no inline config, still
// gives 5.
func TestStartAgent_ClearedLimitInheritsTemplateByName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, mgr := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "by-name-agent"
	tplDir := filepath.Join(projectDir, "templates", "limited")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness: claude\nmax_turns: 5\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// As provisioned: the template's values are written into the agent's
	// config.
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude", MaxTurns: 5})
	writeAgentInfo(t, projectDir, agentName, api.AgentInfo{Template: "limited"})
	obs := observeStarts(mgr, projectDir, agentName, cfgPath)

	postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"max_turns": 9}}`, projectDir))
	if o := lastObservation(t, obs, 1); o.file.MaxTurns != 9 || o.effective.MaxTurns != 9 {
		t.Fatalf("set: file max_turns=%d effective=%d, want 9", o.file.MaxTurns, o.effective.MaxTurns)
	}

	postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"cleared_fields": ["max_turns"]}}`, projectDir))
	o := lastObservation(t, obs, 2)
	if o.file.MaxTurns != 0 || strings.Contains(o.raw, "cleared_fields") {
		t.Fatalf("clear: the start read %s, want no max_turns and no cleared_fields", o.raw)
	}
	if o.effective.MaxTurns != 5 {
		t.Fatalf("clear: effective max_turns = %d, want the template's 5", o.effective.MaxTurns)
	}

	postClearedAgentAction(t, srv, agentName, "restart", fmt.Sprintf(`{"projectPath": %q}`, projectDir))
	if o := lastObservation(t, obs, 3); o.effective.MaxTurns != 5 {
		t.Fatalf("restart: effective max_turns = %d, want the template's 5", o.effective.MaxTurns)
	}
}

// TestStartAgent_ClearedLimitHydratedTemplate: for a template hydrated from
// the content-addressed cache, which GetAgent does not re-merge, a cleared
// limit is copied from the cached template; when the cached template
// cannot be loaded, the limit keeps its current value (a clear never
// removes a limit), while env keys are still removed.
func TestStartAgent_ClearedLimitHydratedTemplate(t *testing.T) {
	const hash = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name         string
		cached       bool
		wantMaxTurns int
	}{
		{name: "cached template", cached: true, wantMaxTurns: 5},
		{name: "cached template missing", cached: false, wantMaxTurns: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			srv, mgr := newTestServerWithProvisionCapture()
			cache, err := templatecache.New(t.TempDir(), 0)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cached {
				if _, err := cache.Put(hash, map[string][]byte{"scion-agent.yaml": []byte("harness: claude\nmax_turns: 5\n")}); err != nil {
					t.Fatal(err)
				}
			}
			srv.SetTemplateCache(cache)
			projectDir := filepath.Join(t.TempDir(), ".scion")
			agentName := "hydrated-agent"
			cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{
				Harness: "claude", MaxTurns: 5, Env: map[string]string{"FOO": "1"},
			})
			writeAgentInfo(t, projectDir, agentName, api.AgentInfo{Template: "limited", TemplateHash: hash})
			obs := observeStarts(mgr, projectDir, agentName, cfgPath)

			postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"max_turns": 9, "env": {"FOO": "1"}}}`, projectDir))
			lastObservation(t, obs, 1)
			postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"cleared_fields": ["max_turns", "env.FOO"]}}`, projectDir))
			o := lastObservation(t, obs, 2)
			if o.file.MaxTurns != tc.wantMaxTurns || o.effective.MaxTurns != tc.wantMaxTurns {
				t.Errorf("max_turns: file %d, effective %d; want %d", o.file.MaxTurns, o.effective.MaxTurns, tc.wantMaxTurns)
			}
			if _, ok := o.file.Env["FOO"]; ok {
				t.Errorf("env FOO kept: %v", o.file.Env)
			}
		})
	}
}

// TestApplyInlineConfigUpdate_WorkspaceVolumeKeepsFirst: an inline
// /workspace volume never replaces the provisioned one, which the start
// takes the agent's workspace from.
func TestApplyInlineConfigUpdate_WorkspaceVolumeKeepsFirst(t *testing.T) {
	srv, _ := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")
	agentName := "workspace-agent"
	provisioned := api.VolumeMount{Source: "/srv/checkout", Target: "/workspace"}
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude", Volumes: []api.VolumeMount{provisioned}})
	inline := &api.ScionConfig{Volumes: []api.VolumeMount{
		{Source: "/elsewhere", Target: "/workspace"},
		{Source: "/data", Target: "/data"},
	}}
	for i := 0; i < 3; i++ {
		srv.applyInlineConfigUpdate(agentName, projectDir, inline, false)
		got, _ := readAgentConfig(t, cfgPath)
		want := []api.VolumeMount{provisioned, {Source: "/data", Target: "/data"}}
		if !reflect.DeepEqual(got.Volumes, want) {
			t.Fatalf("after start #%d: volumes\n got: %+v\nwant: %+v", i+1, got.Volumes, want)
		}
	}
}

// clearedChainCase sets up an agent provisioned with max_turns 9 and
// resources set inline, starts it, then clears both, and returns what the
// start after the clear read.
func clearedChainCase(t *testing.T, srv *Server, mgr *provisionCapturingManager, projectDir string, info api.AgentInfo) clearedStartObservation {
	t.Helper()
	agentName := "chain-agent"
	cfgPath := writeAgentConfig(t, projectDir, agentName, api.ScionConfig{Harness: "claude"})
	writeAgentInfo(t, projectDir, agentName, info)
	obs := observeStarts(mgr, projectDir, agentName, cfgPath)
	postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"max_turns": 9, "resources": {"requests": {"cpu": "2"}}}}`, projectDir))
	lastObservation(t, obs, 1)
	postClearedAgentAction(t, srv, agentName, "start", fmt.Sprintf(`{"projectPath": %q, "inlineConfig": {"cleared_fields": ["max_turns", "resources"]}}`, projectDir))
	return lastObservation(t, obs, 2)
}

func writeTemplateFile(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.yaml"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestStartAgent_ClearedLimitHydratedTemplateDefaultLayer: provisioning
// layers the project's default template under a hydrated template, so a
// limit the default sets and the cached template does not is the inherited
// value; template resources are never introduced (templates may not set
// resources), so a cleared resources is removed.
func TestStartAgent_ClearedLimitHydratedTemplateDefaultLayer(t *testing.T) {
	const hash = "sha256:" + "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	t.Setenv("HOME", t.TempDir())
	srv, mgr := newTestServerWithProvisionCapture()
	cache, err := templatecache.New(t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(hash, map[string][]byte{"scion-agent.yaml": []byte("resources:\n  requests:\n    cpu: \"4\"\n")}); err != nil {
		t.Fatal(err)
	}
	srv.SetTemplateCache(cache)
	projectDir := filepath.Join(t.TempDir(), ".scion")
	writeTemplateFile(t, filepath.Join(projectDir, "templates", "default"), "max_turns: 7\n")

	o := clearedChainCase(t, srv, mgr, projectDir, api.AgentInfo{Template: "hub-tpl", TemplateHash: hash})
	if o.file.MaxTurns != 7 || o.effective.MaxTurns != 7 {
		t.Errorf("max_turns: file %d, effective %d; want the default layer's 7", o.file.MaxTurns, o.effective.MaxTurns)
	}
	if o.file.Resources != nil {
		t.Errorf("resources = %+v, want removed (a template's resources are never introduced)", o.file.Resources)
	}
}

// TestStartAgent_ClearedLimitUnresolvableTemplateKept: a by-name template
// that no longer resolves leaves the cleared keys at their current value
// (a clear never removes a limit), since GetAgent would then load the
// persisted config alone.
func TestStartAgent_ClearedLimitUnresolvableTemplateKept(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, mgr := newTestServerWithProvisionCapture()
	projectDir := filepath.Join(t.TempDir(), ".scion")

	o := clearedChainCase(t, srv, mgr, projectDir, api.AgentInfo{Template: "gone"})
	if o.file.MaxTurns != 9 || o.effective.MaxTurns != 9 {
		t.Errorf("max_turns: file %d, effective %d; want the current 9 kept", o.file.MaxTurns, o.effective.MaxTurns)
	}
	if o.file.Resources == nil || o.file.Resources.Requests.CPU != "2" {
		t.Errorf("resources = %+v, want the current value kept", o.file.Resources)
	}
}
