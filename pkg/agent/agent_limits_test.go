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

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#623: the hub agent_defaults limits (max_turns,
// max_model_calls, max_duration) and the broker's settings.yaml limit
// defaults are applied on every start and never persisted, so a changed
// default reaches the agent on its next start or restart, while a limit the
// agent sets itself (template or requester) still wins.

// hubLimitsCtx returns a context carrying hub agent_defaults limits, the shape
// the hub sends on create, start and restart.
func hubLimitsCtx(turns, calls int, duration string) context.Context {
	return api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
		MaxTurns: turns, MaxModelCalls: calls, MaxDuration: duration,
	})
}

// wantLimitEnv checks the SCION_MAX_* env Start handed to the runtime. An
// empty want means the key must be absent.
func wantLimitEnv(t *testing.T, label string, rc runtime.RunConfig, turns, calls, duration string) {
	t.Helper()
	for _, c := range []struct{ key, want string }{
		{"SCION_MAX_TURNS", turns},
		{"SCION_MAX_MODEL_CALLS", calls},
		{"SCION_MAX_DURATION", duration},
	} {
		got, set := envValue(rc.Env, c.key)
		switch {
		case c.want == "" && set:
			t.Errorf("%s: %s = %q, want unset", label, c.key, got)
		case c.want != "" && got != c.want:
			t.Errorf("%s: %s = %q (set=%v), want %q", label, c.key, got, set, c.want)
		}
	}
}

// wantPersistedLimits checks the limits in the agent's scion-agent.json.
func wantPersistedLimits(t *testing.T, label, project string, turns, calls int, duration string) {
	t.Helper()
	cfg, raw := persistedEnvAgentConfig(t, project)
	if !api.IntPtrEqual(cfg.MaxTurns, wantCount(turns)) || !api.IntPtrEqual(cfg.MaxModelCalls, wantCount(calls)) || cfg.MaxDuration != duration {
		t.Errorf("%s: persisted limits = (%s, %s, %q), want (%d, %d, %q): %s", label,
			api.FormatCountLimit(cfg.MaxTurns), api.FormatCountLimit(cfg.MaxModelCalls), cfg.MaxDuration, turns, calls, duration, raw)
	}
}

// wantCount is a test's expected count limit: 0 means unset (nil), as in
// the tests written before count limits could be an explicit 0; a test of
// an explicit "no limit" compares with api.IntPtr(0) directly.
func wantCount(n int) *int {
	if n == 0 {
		return nil
	}
	return api.IntPtr(n)
}

// globalTemplatePath is the default template envFixture writes.
func globalTemplatePath(project string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(project)), ".scion", "templates", "default", "scion-agent.json")
}

// A changed hub default reaches the next start and restart, and is never
// written into scion-agent.json.
func TestStart_HubDefaultLimitsAppliedPerStart(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)

	rc := startEnvCtx(t, hubLimitsCtx(10, 100, "1h"), project, nil)
	wantLimitEnv(t, "first start", rc, "10", "100", "1h")
	wantPersistedLimits(t, "after first start", project, 0, 0, "")

	rc = startEnvCtx(t, hubLimitsCtx(10, 100, "1h"), project, nil)
	wantLimitEnv(t, "restart", rc, "10", "100", "1h")

	rc = startEnvCtx(t, hubLimitsCtx(20, 200, "2h"), project, nil)
	wantLimitEnv(t, "restart after hub change", rc, "20", "200", "2h")
	wantPersistedLimits(t, "after hub change", project, 0, 0, "")
}

// The agent's own limits (template, then requester inline config) win over
// the hub default and the settings default, on the first start and on every
// later one, and are what scion-agent.json keeps. The hub default fills only
// the limit the agent leaves unset, and the requester's inline config is not
// written to.
func TestStart_OwnLimitsWinOverDefaults(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	mustWrite(t, globalTemplatePath(project), `{"default_harness_config": "test-harness", "max_turns": 5}`)
	mustWrite(t, settingsPath, envSettingsV1.yaml()+"default_max_turns: 9\ndefault_max_model_calls: 99\ndefault_max_duration: 9m\n")

	inline := &api.ScionConfig{MaxModelCalls: api.IntPtr(7)}
	rc := startEnvCtx(t, hubLimitsCtx(20, 200, "2h"), project, func(o *api.StartOptions) { o.InlineConfig = inline })
	wantLimitEnv(t, "first start", rc, "5", "7", "2h")
	wantPersistedLimits(t, "after first start", project, 5, 7, "")
	if inline.MaxTurns != nil || !api.IntPtrEqual(inline.MaxModelCalls, api.IntPtr(7)) || inline.MaxDuration != "" {
		t.Errorf("Start wrote defaults into the requester's inline config: %+v", inline)
	}

	rc = startEnvCtx(t, hubLimitsCtx(30, 300, "3h"), project, nil)
	wantLimitEnv(t, "restart after hub change", rc, "5", "7", "3h")
	wantPersistedLimits(t, "after restart", project, 5, 7, "")
}

// An explicit "no limit" (a count limit of 0) is the agent's own value
// (ptone/scion#5605): it beats the template's limit, the hub default and the
// settings default, sets no SCION_MAX_* env var on any start, and is
// persisted as 0, so a restart without the inline config keeps it.
func TestStart_UnlimitedCountBeatsTemplateAndDefaults(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	mustWrite(t, globalTemplatePath(project), `{"default_harness_config": "test-harness", "max_turns": 5, "max_model_calls": 6}`)
	mustWrite(t, settingsPath, envSettingsV1.yaml()+"default_max_turns: 9\ndefault_max_model_calls: 99\n")

	inline := &api.ScionConfig{MaxTurns: api.IntPtr(0), MaxModelCalls: api.IntPtr(0)}
	rc := startEnvCtx(t, hubLimitsCtx(20, 200, ""), project, func(o *api.StartOptions) { o.InlineConfig = inline })
	wantLimitEnv(t, "first start", rc, "", "", "")
	cfg, raw := persistedEnvAgentConfig(t, project)
	if !api.IsUnlimitedCount(cfg.MaxTurns) || !api.IsUnlimitedCount(cfg.MaxModelCalls) {
		t.Fatalf("persisted limits = %s/%s, want 0/0: %s", api.FormatCountLimit(cfg.MaxTurns), api.FormatCountLimit(cfg.MaxModelCalls), raw)
	}

	rc = startEnvCtx(t, hubLimitsCtx(30, 300, ""), project, nil)
	wantLimitEnv(t, "restart", rc, "", "", "")
}

// A persisted explicit "no limit" from an earlier start is kept when a later
// start's inline config leaves the limit unset, and replaced when it sets a
// value.
func TestStart_PersistedUnlimitedOverlay(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	rc := startEnvCtx(t, hubLimitsCtx(20, 0, ""), project, func(o *api.StartOptions) {
		o.InlineConfig = &api.ScionConfig{MaxTurns: api.IntPtr(0)}
	})
	wantLimitEnv(t, "unlimited", rc, "", "", "")
	rc = startEnvCtx(t, hubLimitsCtx(20, 0, ""), project, func(o *api.StartOptions) { o.InlineConfig = &api.ScionConfig{} })
	wantLimitEnv(t, "inline unset keeps unlimited", rc, "", "", "")
	rc = startEnvCtx(t, hubLimitsCtx(20, 0, ""), project, func(o *api.StartOptions) {
		o.InlineConfig = &api.ScionConfig{MaxTurns: api.IntPtr(4)}
	})
	wantLimitEnv(t, "inline value replaces unlimited", rc, "4", "", "")
}

// With no hub default and no settings default, no limit is set and nothing
// is persisted.
func TestStart_NoDefaultLimitsNoChange(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	rc := startEnv(t, project, nil)
	wantLimitEnv(t, "first start", rc, "", "", "")
	rc = startEnv(t, project, nil)
	wantLimitEnv(t, "restart", rc, "", "", "")
	wantPersistedLimits(t, "after restart", project, 0, 0, "")
}

// The broker's settings.yaml defaults sit below the hub default, and a
// change to them also reaches the next start.
func TestStart_SettingsDefaultLimitsPerStartBelowHub(t *testing.T) {
	project, settingsPath := envFixture(t, envSettingsV1)
	mustWrite(t, settingsPath, envSettingsV1.yaml()+"default_max_turns: 9\n")

	wantLimitEnv(t, "no hub default", startEnv(t, project, nil), "9", "", "")
	wantLimitEnv(t, "hub default", startEnvCtx(t, hubLimitsCtx(20, 0, ""), project, nil), "20", "", "")

	mustWrite(t, settingsPath, envSettingsV1.yaml()+"default_max_turns: 8\n")
	wantLimitEnv(t, "settings change", startEnv(t, project, nil), "8", "", "")
	wantPersistedLimits(t, "after settings change", project, 0, 0, "")
}

// A limit the user cleared on the hub is not re-pinned to the hub default as
// if it were the agent's own. The broker applies a clear by writing the
// template's value (here none) into scion-agent.json before Start
// (applyClearedFields); the hub default then fills the limit on each start
// without being persisted, so a later hub change still reaches the agent.
func TestStart_ClearedLimitNotRepinnedToHubDefault(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	inline := &api.ScionConfig{MaxTurns: api.IntPtr(5)}
	rc := startEnvCtx(t, hubLimitsCtx(20, 0, ""), project, func(o *api.StartOptions) { o.InlineConfig = inline })
	wantLimitEnv(t, "explicit", rc, "5", "", "")
	wantPersistedLimits(t, "explicit", project, 5, 0, "")

	// The clear, as the broker applies it: the template sets no max_turns.
	cfg, _ := persistedEnvAgentConfig(t, project)
	if ignored := api.ApplyClearedFields(&cfg, []string{"max_turns"}, &api.ScionConfig{}); len(ignored) != 0 {
		t.Fatalf("ApplyClearedFields ignored %v", ignored)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(project, "agents", "env-agent", "scion-agent.json"), string(data))

	rc = startEnvCtx(t, hubLimitsCtx(20, 0, ""), project, nil)
	wantLimitEnv(t, "after clear", rc, "20", "", "")
	wantPersistedLimits(t, "after clear", project, 0, 0, "")

	rc = startEnvCtx(t, hubLimitsCtx(30, 0, ""), project, nil)
	wantLimitEnv(t, "after clear and hub change", rc, "30", "", "")
}

// A Start that rewrites scion-agent.json (here the corrupted
// auth_selectedType repair) persists the agent's own limits only.
func TestStart_RewriteDoesNotPersistDefaultLimits(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	startEnvCtx(t, hubLimitsCtx(10, 100, "1h"), project, nil)

	cfgPath := filepath.Join(project, "agents", "env-agent", "scion-agent.json")
	cfg, _ := persistedEnvAgentConfig(t, project)
	cfg.AuthSelectedType = "generic"
	data, _ := json.Marshal(cfg)
	mustWrite(t, cfgPath, string(data))

	rc := startEnvCtx(t, hubLimitsCtx(10, 100, "1h"), project, nil)
	wantLimitEnv(t, "rewrite start", rc, "10", "100", "1h")
	cfg, raw := persistedEnvAgentConfig(t, project)
	if cfg.AuthSelectedType != "" {
		t.Fatalf("test precondition: Start did not rewrite scion-agent.json: %s", raw)
	}
	wantPersistedLimits(t, "after rewrite", project, 0, 0, "")
}

// The HarnessAuth rewrite of scion-agent.json in Start persists the agent's
// own limits only. The harness auth check below is only a precondition: the
// provision write already stores it, so it does not prove this rewrite ran;
// the limits checks hold whichever write was last.
func TestStart_HarnessAuthRewriteDoesNotPersistDefaultLimits(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	withAuth := func(o *api.StartOptions) { o.HarnessAuth = "api-key" }

	rc := startEnvCtx(t, hubLimitsCtx(10, 100, "1h"), project, withAuth)
	wantLimitEnv(t, "first start", rc, "10", "100", "1h")
	cfg, raw := persistedEnvAgentConfig(t, project)
	if cfg.AuthSelectedType != "api-key" {
		t.Fatalf("test precondition: harness auth not persisted in scion-agent.json: %s", raw)
	}
	wantPersistedLimits(t, "after HarnessAuth rewrite", project, 0, 0, "")

	rc = startEnvCtx(t, hubLimitsCtx(20, 200, "2h"), project, withAuth)
	wantLimitEnv(t, "restart after hub change", rc, "20", "200", "2h")
	wantPersistedLimits(t, "after second HarnessAuth rewrite", project, 0, 0, "")
}

// finishProvision's HarnessAuth rewrite (a provision-only create) persists
// the agent's own limits only, while the returned config carries the
// effective ones. As above, the harness auth check is only a precondition.
func TestProvision_HarnessAuthRewriteDoesNotPersistDefaultLimits(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
	}
	cfg, err := NewManager(mockRT).Provision(hubLimitsCtx(10, 100, "1h"), api.StartOptions{
		Name:        "env-agent",
		ProjectPath: project,
		BrokerMode:  true,
		NoAuth:      true,
		HarnessAuth: "api-key",
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if cfg == nil || !api.IntPtrEqual(cfg.MaxTurns, api.IntPtr(10)) || !api.IntPtrEqual(cfg.MaxModelCalls, api.IntPtr(100)) || cfg.MaxDuration != "1h" {
		t.Errorf("returned config = %+v, want the effective limits 10/100/1h", cfg)
	}
	persisted, raw := persistedEnvAgentConfig(t, project)
	if persisted.AuthSelectedType != "api-key" {
		t.Fatalf("test precondition: harness auth not persisted in scion-agent.json: %s", raw)
	}
	wantPersistedLimits(t, "after finishProvision rewrite", project, 0, 0, "")
}

// An agent provisioned before ptone/scion#623 has the defaults of that time
// in scion-agent.json. They read back as the agent's own limits and keep
// applying over a changed hub default (a documented limitation).
func TestStart_LegacyPersistedLimitsKeepApplying(t *testing.T) {
	project, _ := envFixture(t, envSettingsV1)
	startEnv(t, project, nil)

	cfgPath := filepath.Join(project, "agents", "env-agent", "scion-agent.json")
	cfg, _ := persistedEnvAgentConfig(t, project)
	cfg.MaxTurns = api.IntPtr(50)
	data, _ := json.Marshal(cfg)
	mustWrite(t, cfgPath, string(data))

	wantLimitEnv(t, "legacy", startEnvCtx(t, hubLimitsCtx(20, 0, ""), project, nil), "50", "", "")
}

func TestApplyDefaultLimits(t *testing.T) {
	cfg := &api.ScionConfig{MaxTurns: api.IntPtr(3)}
	applyDefaultLimits(hubLimitsCtx(20, 200, ""), cfg, nil)
	if !api.IntPtrEqual(cfg.MaxTurns, api.IntPtr(3)) || !api.IntPtrEqual(cfg.MaxModelCalls, api.IntPtr(200)) || cfg.MaxDuration != "" {
		t.Errorf("applyDefaultLimits = %+v, want own max_turns 3 kept and hub max_model_calls 200 filled", cfg)
	}
	applyDefaultLimits(context.Background(), nil, nil) // nil config is a no-op
}

func TestReadPersistedAgentLimits(t *testing.T) {
	dir := t.TempDir()
	if _, ok := readPersistedAgentLimits(dir); ok {
		t.Error("missing file: ok = true")
	}
	if _, ok := readPersistedAgentLimits(""); ok {
		t.Error("empty dir: ok = true")
	}
	if err := os.WriteFile(filepath.Join(dir, "scion-agent.json"), []byte(`{"max_turns": 4, "max_duration": "4m"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	l, ok := readPersistedAgentLimits(dir)
	if !ok || !reflect.DeepEqual(l, agentLimits{MaxTurns: api.IntPtr(4), MaxDuration: "4m"}) {
		t.Errorf("readPersistedAgentLimits = %+v, %v", l, ok)
	}
}
