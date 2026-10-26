package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func envListToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}

func TestBuildAgentEnv_BrokerModeSkipsConfigTZ(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	t.Setenv("TZ_KEY_NAME", "TZ")

	cases := []struct {
		name       string
		cfgEnv     map[string]string
		extraEnv   map[string]string
		wantTZ     string // "" means absent
		wantDrops  []droppedBrokerEnv
		wantNoMiss bool
	}{
		{
			// Rung test (i): an empty marker must not pull in the broker host's TZ.
			name:      "empty marker does not pass the host TZ through",
			cfgEnv:    map[string]string{"TZ": ""},
			wantDrops: []droppedBrokerEnv{{Key: "TZ", Value: "", Layer: envLayerConfig}},
		},
		{
			name:      "config value is dropped when the hub sends none",
			cfgEnv:    map[string]string{"TZ": "Europe/Paris"},
			wantDrops: []droppedBrokerEnv{{Key: "TZ", Value: "Europe/Paris", Layer: envLayerConfig}},
		},
		{
			name:      "key is matched after expansion",
			cfgEnv:    map[string]string{"${TZ_KEY_NAME}": "Europe/Paris"},
			wantDrops: []droppedBrokerEnv{{Key: "TZ", Value: "Europe/Paris", Layer: envLayerConfig}},
		},
		{
			name:      "the hub value is the only source",
			cfgEnv:    map[string]string{"TZ": "Europe/Paris"},
			extraEnv:  map[string]string{"TZ": "America/New_York"},
			wantTZ:    "America/New_York",
			wantDrops: []droppedBrokerEnv{{Key: "TZ", Value: "Europe/Paris", Layer: envLayerConfig}},
		},
		{
			name:       "an empty hub TZ is omitted, not reported missing",
			extraEnv:   map[string]string{"TZ": ""},
			wantNoMiss: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &api.ScionConfig{Env: tc.cfgEnv}
			cfgBefore := cloneEnv(tc.cfgEnv)
			env, warnings, missing, dropped := buildAgentEnv(cfg, tc.extraEnv, nil, true)

			got := envListToMap(env)
			if tc.wantTZ == "" {
				if v, ok := got["TZ"]; ok {
					t.Errorf("container env has TZ=%q, want no TZ", v)
				}
			} else if got["TZ"] != tc.wantTZ {
				t.Errorf("container TZ = %q, want %q", got["TZ"], tc.wantTZ)
			}
			if !slices.Equal(dropped, tc.wantDrops) {
				t.Errorf("dropped = %+v, want %+v", dropped, tc.wantDrops)
			}
			if slices.Contains(missing, "TZ") {
				t.Errorf("TZ must never be reported missing in broker mode; missing=%v warnings=%v", missing, warnings)
			}
			if !mapsEqual(cfg.Env, cfgBefore) {
				t.Errorf("scionCfg.Env was mutated: got %v, want %v", cfg.Env, cfgBefore)
			}
		})
	}
}

// TestBuildAgentEnv_SoloModeKeepsTZ pins that solo mode is unchanged: an
// empty TZ marker still passes the host value through, and a config TZ still
// reaches the container.
func TestBuildAgentEnv_SoloModeKeepsTZ(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")

	env, _, missing, dropped := buildAgentEnv(&api.ScionConfig{Env: map[string]string{"TZ": ""}}, nil, nil, false)
	if got := envListToMap(env)["TZ"]; got != "Asia/Tokyo" {
		t.Errorf("solo empty marker: TZ = %q, want the host value Asia/Tokyo", got)
	}
	if len(dropped) != 0 || len(missing) != 0 {
		t.Errorf("solo mode must drop nothing: dropped=%v missing=%v", dropped, missing)
	}

	env, _, _, dropped = buildAgentEnv(&api.ScionConfig{Env: map[string]string{"TZ": "Europe/Paris"}}, nil, nil, false)
	if got := envListToMap(env)["TZ"]; got != "Europe/Paris" {
		t.Errorf("solo config value: TZ = %q, want Europe/Paris", got)
	}
	if len(dropped) != 0 {
		t.Errorf("solo mode must drop nothing: dropped=%v", dropped)
	}
}

func TestResolveAuthEnvOverlay_BrokerModeSkipsHarnessConfigEntryTZ(t *testing.T) {
	settings := &config.VersionedSettings{
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"claude-cfg": {Harness: "claude", Env: map[string]string{"TZ": "Europe/Paris", "OTHER": "kept"}},
		},
	}

	opts := api.StartOptions{BrokerMode: true}
	overlay, dropped := resolveAuthEnvOverlay(&opts, settings, "", "claude-cfg")
	if _, ok := opts.Env["TZ"]; ok {
		t.Errorf("broker mode: harness-config entry TZ merged into opts.Env: %v", opts.Env)
	}
	if _, ok := overlay["TZ"]; ok {
		t.Errorf("broker mode: harness-config entry TZ in the auth overlay: %v", overlay)
	}
	if opts.Env["OTHER"] != "kept" {
		t.Errorf("broker mode: other entry keys must still merge; opts.Env=%v", opts.Env)
	}
	want := []droppedBrokerEnv{{Key: "TZ", Value: "Europe/Paris", Layer: envLayerHarnessConfigEntry}}
	if !slices.Equal(dropped, want) {
		t.Errorf("dropped = %+v, want %+v", dropped, want)
	}

	solo := api.StartOptions{}
	_, dropped = resolveAuthEnvOverlay(&solo, settings, "", "claude-cfg")
	if solo.Env["TZ"] != "Europe/Paris" {
		t.Errorf("solo mode: harness-config entry TZ = %q, want Europe/Paris", solo.Env["TZ"])
	}
	if len(dropped) != 0 {
		t.Errorf("solo mode must drop nothing: %v", dropped)
	}
}

func TestWarnDroppedBrokerEnv(t *testing.T) {
	dropped := []droppedBrokerEnv{
		{Key: "TZ", Value: "Europe/Paris", Layer: envLayerConfig},
		{Key: "TZ", Value: "", Layer: envLayerConfig},
		{Key: "TZ", Value: "Asia/Tokyo", Layer: envLayerHarnessConfigEntry},
	}

	warnings := warnDroppedBrokerEnv("agent-123", nil, dropped)
	if len(warnings) != 2 {
		t.Fatalf("want one warning per non-empty drop, got %d: %v", len(warnings), warnings)
	}
	for i, want := range []struct{ value, layer string }{{"Europe/Paris", "config"}, {"Asia/Tokyo", "harness-config entry"}} {
		w := warnings[i]
		for _, s := range []string{"agent-123", want.value, want.layer, "TZ"} {
			if !strings.Contains(w, s) {
				t.Errorf("warning %q does not name %q", w, s)
			}
		}
	}

	// A value equal to the hub's is what the container gets anyway.
	warnings = warnDroppedBrokerEnv("agent-123", map[string]string{"TZ": "Europe/Paris"}, dropped)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Asia/Tokyo") {
		t.Errorf("only the value that differs from the hub's should warn, got %v", warnings)
	}
}

// startTZFixture lays out a global .scion with a harness-config entry carrying
// entryTZ and a pre-provisioned agent whose scion-agent.json env carries
// cfgEnvTZJSON, and returns the project .scion dir and the agent dir.
func startTZFixture(t *testing.T, entryTZ, cfgEnvTZJSON string) (projectScionDir, agentDir string) {
	t.Helper()
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: test-image:latest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	settings := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"
	if entryTZ != "" {
		settings += "harness_configs:\n  test-harness:\n    harness: generic\n    env:\n      TZ: " + entryTZ + "\n"
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}

	projectScionDir = filepath.Join(tmpDir, "project", ".scion")
	agentDir = filepath.Join(projectScionDir, "agents", "tz-agent")
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{"harness": "generic", "harness_config": "test-harness", "env": {"TZ": ` + cfgEnvTZJSON + `}}`
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir, agentDir
}

func capturingRuntime(captured *[]string) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			*captured = cfg.Env
			return "mock-id", nil
		},
	}
}

func readPersistedConfig(t *testing.T, agentDir string) api.ScionConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// TestStart_BrokerMode_HubIsOnlyTZSource covers rung test (ii): a
// hub-dispatched agent whose scion-agent.json and harness-config entry both
// carry TZ=Europe/Paris, with no hub TZ, gets a container with no TZ and a
// warning naming both layers. HarnessAuth is set, so Start rewrites
// scion-agent.json before building the env; the on-disk TZ must survive.
func TestStart_BrokerMode_HubIsOnlyTZSource(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	projectScionDir, agentDir := startTZFixture(t, "Europe/Paris", `"Europe/Paris"`)

	var captured []string
	mgr := NewManager(capturingRuntime(&captured))
	info, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "tz-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		HarnessAuth: "api-key",
		Env:         map[string]string{"SCION_AGENT_ID": "agent-uuid-tz"},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if v, ok := envListToMap(captured)["TZ"]; ok {
		t.Errorf("container env has TZ=%q, want no TZ (the hub sent none)", v)
	}
	var tzWarnings []string
	for _, w := range info.Warnings {
		if strings.Contains(w, "TZ=") {
			tzWarnings = append(tzWarnings, w)
		}
	}
	for _, layer := range []string{"broker config layer", "broker harness-config entry layer"} {
		found := false
		for _, w := range tzWarnings {
			if strings.Contains(w, layer) && strings.Contains(w, "Europe/Paris") && strings.Contains(w, "agent-uuid-tz") {
				found = true
			}
		}
		if !found {
			t.Errorf("no start warning names the %q with the value and agent ID; TZ warnings: %v", layer, tzWarnings)
		}
	}
	// The same drop warnings, and only those, are carried for the hub.
	if len(info.HubOnlyEnvWarnings) != len(tzWarnings) {
		t.Errorf("HubOnlyEnvWarnings = %v, want the %d TZ drop warnings", info.HubOnlyEnvWarnings, len(tzWarnings))
	}
	for _, w := range info.HubOnlyEnvWarnings {
		if !strings.Contains(w, "TZ=") {
			t.Errorf("HubOnlyEnvWarnings carries a non-TZ warning %q", w)
		}
	}

	persisted := readPersistedConfig(t, agentDir)
	// AuthSelectedType proves Start rewrote scion-agent.json from
	// finalScionCfg; the TZ check then proves the rewrite kept it.
	if persisted.AuthSelectedType != "api-key" {
		t.Errorf("on-disk auth_selectedType = %q, want api-key (the HarnessAuth rewrite did not run)", persisted.AuthSelectedType)
	}
	if persisted.Env["TZ"] != "Europe/Paris" {
		t.Errorf("on-disk scion-agent.json TZ = %q, want Europe/Paris left intact", persisted.Env["TZ"])
	}
}

// TestStart_BrokerMode_HubTZWins pins that a hub-supplied TZ reaches the
// container over the on-disk and harness-config entry values.
func TestStart_BrokerMode_HubTZWins(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	projectScionDir, _ := startTZFixture(t, "Europe/Paris", `"Europe/Paris"`)

	var captured []string
	mgr := NewManager(capturingRuntime(&captured))
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "tz-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Env:         map[string]string{"TZ": "America/New_York"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if got := envListToMap(captured)["TZ"]; got != "America/New_York" {
		t.Errorf("container TZ = %q, want the hub value America/New_York", got)
	}
}

// TestStart_BrokerMode_EmptyTZMarkerDoesNotLeakHostTZ covers rung test (i):
// a TZ "" marker with no hub TZ and host TZ=Asia/Tokyo gives no TZ, and the
// start does not fail on it.
func TestStart_BrokerMode_EmptyTZMarkerDoesNotLeakHostTZ(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	projectScionDir, _ := startTZFixture(t, "", `""`)

	var captured []string
	mgr := NewManager(capturingRuntime(&captured))
	info, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "tz-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if v, ok := envListToMap(captured)["TZ"]; ok {
		t.Errorf("container env has TZ=%q, want no TZ (the broker host's TZ must not leak)", v)
	}
	for _, w := range info.Warnings {
		if strings.Contains(w, "TZ") {
			t.Errorf("an empty TZ marker should not produce a warning, got %q", w)
		}
	}
}

// TestStart_SoloMode_EmptyTZMarkerPassesHostTZ pins that solo mode is
// unchanged: a TZ "" marker still passes the host TZ through.
func TestStart_SoloMode_EmptyTZMarkerPassesHostTZ(t *testing.T) {
	t.Setenv("TZ", "Asia/Tokyo")
	projectScionDir, _ := startTZFixture(t, "", `""`)

	var captured []string
	mgr := NewManager(capturingRuntime(&captured))
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "tz-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if got := envListToMap(captured)["TZ"]; got != "Asia/Tokyo" {
		t.Errorf("solo container TZ = %q, want the host value Asia/Tokyo", got)
	}
}

func cloneEnv(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
