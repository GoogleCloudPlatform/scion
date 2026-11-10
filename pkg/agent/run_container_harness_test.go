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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// harnessEnvCasesPath is the case table shared with
// pkg/sciontool/telemetry (TestContainerHarnessCasesUsageOutcome), which
// reads this same file and checks the usage-parsing outcome of each
// container_harness value.
const harnessEnvCasesPath = "testdata/harness_env_cases.json"

type harnessEnvCase struct {
	Name          string `json:"name"`
	StoredHarness string `json:"stored_harness"`
	Override      string `json:"override"`
	// OverrideSource is empty for an override in the start request's env.
	OverrideSource   string `json:"override_source"`
	ContainerHarness string `json:"container_harness"`
}

func loadHarnessEnvCases(t *testing.T) map[string]harnessEnvCase {
	t.Helper()
	data, err := os.ReadFile(harnessEnvCasesPath)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []harnessEnvCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]harnessEnvCase, len(file.Cases))
	for _, c := range file.Cases {
		out[c.Name] = c
	}
	return out
}

const (
	// Declarative harness-configs (config_dir is declarative metadata).
	containerHarnessHCClaude = "harness: claude\nimage: scion-base:test\nuser: scion\nconfig_dir: .claude\n"
	containerHarnessHCGemini = "harness: gemini-cli\nimage: scion-base:test\nuser: scion\nconfig_dir: .gemini\n"
	// No declarative metadata: Resolve falls back to the plain Generic.
	containerHarnessHCBare = "harness: claude\nimage: scion-base:test\nuser: scion\n"
)

// containerEnvManager returns a manager whose runtime records the container
// env and argv of the last run.
func containerEnvManager(env *map[string]string, args *[]string) Manager {
	return NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			m := map[string]string{}
			for _, kv := range cfg.Env {
				k, v, _ := strings.Cut(kv, "=")
				m[k] = v
			}
			*env = m
			if args != nil {
				*args = append([]string(nil), cfg.CommandArgs...)
			}
			return "mock-id", nil
		},
	})
}

// rewriteStoredHarness sets scion-agent.json's stored harness (removed when
// empty) and, when dropHC, removes the stored harness-config, as for a
// legacy agent.
func (e *policyTestEnv) rewriteStoredHarness(t *testing.T, name, stored string, dropHC bool) {
	t.Helper()
	path := filepath.Join(e.scion, "agents", name, "scion-agent.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if stored == "" {
		delete(cfg, "harness")
	} else {
		cfg["harness"] = stored
	}
	if dropHC {
		delete(cfg, "harness_config")
	}
	out, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

// dropDefaultHarnessConfig removes default_harness_config from the
// machine and project settings, so no settings tier names a harness-config.
func (e *policyTestEnv) dropDefaultHarnessConfig(t *testing.T) {
	t.Helper()
	e.rewriteSettings(t, "", "")
}

// rewriteSettings sets default_harness_config to defaultHC (empty: none) in
// the machine and project settings, and appends projectExtra (top-level
// YAML) to the project settings.
func (e *policyTestEnv) rewriteSettings(t *testing.T, defaultHC, projectExtra string) {
	t.Helper()
	projectYAML := filepath.Join(e.scion, "settings.yaml")
	if _, err := os.Stat(projectYAML); os.IsNotExist(err) {
		if err := os.WriteFile(projectYAML, []byte("schema_version: \"1\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{filepath.Join(filepath.Dir(e.root), ".scion"), e.scion} {
		for _, base := range []string{"settings.yaml", "settings.json"} {
			path := filepath.Join(dir, base)
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var kept []string
			for _, line := range strings.Split(string(data), "\n") {
				if strings.Contains(line, "default_harness_config") {
					continue
				}
				kept = append(kept, line)
			}
			if base == "settings.yaml" {
				// The embedded defaults name one too; an explicit
				// top-level value (empty included) overrides it.
				kept = append(kept, "default_harness_config: \""+defaultHC+"\"")
				if path == projectYAML && projectExtra != "" {
					if strings.Contains(string(data), "harness_configs:") {
						t.Fatalf("fixture: %s already has harness_configs", path)
					}
					kept = append(kept, projectExtra)
				}
			}
			if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func (e *policyTestEnv) storedHarness(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.scion, "agents", name, "scion-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Harness string `json:"harness"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Harness
}

// captureHarnessWarnings routes slog's default logger to a buffer at warn level
// for the test.
func captureHarnessWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

const containerHarnessOverrideWarning = "Start: SCION_HARNESS override differs from the launched harness"

// startCase runs a real Start for one case of the shared table and returns
// the container env.
func startCase(t *testing.T, c harnessEnvCase) map[string]string {
	t.Helper()
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-claude", containerHarnessHCClaude)
	e.projectHC(t, "hc-gemini", containerHarnessHCGemini)
	e.projectHC(t, "hc-bare", containerHarnessHCBare)
	const name = "harness-env"
	opts := api.StartOptions{Name: name, ProjectPath: e.scion, NoAuth: true}
	switch c.Name {
	case "switch", "override":
		e.provisionExisting(t, name, "hc-claude")
		opts.HarnessConfig = "hc-gemini"
	case "override-settings-env":
		// The settings entry's env, merged into the start env before
		// SCION_HARNESS is set; not an env: in the config.yaml.
		e.provisionExisting(t, name, "hc-claude")
		e.rewriteSettings(t, "hc-gemini", "harness_configs:\n  hc-gemini:\n    env:\n      SCION_HARNESS: claude")
		opts.HarnessConfig = "hc-gemini"
	case "metadata-less":
		e.provisionExisting(t, name, "hc-bare")
	case "legacy-default":
		e.provisionExisting(t, name, "hc-claude")
		e.rewriteStoredHarness(t, name, "", true)
	case "legacy", "legacy-gemini":
		e.provisionExisting(t, name, "hc-claude")
		e.rewriteStoredHarness(t, name, c.StoredHarness, true)
		// A legacy agent: no harness-config from any tier, so the image
		// comes from the request.
		e.dropDefaultHarnessConfig(t)
		opts.Image = "scion-base:test"
	default:
		t.Fatalf("no Start setup for case %q", c.Name)
	}
	if got := e.storedHarness(t, name); got != c.StoredHarness {
		t.Fatalf("fixture: stored harness = %q, want %q", got, c.StoredHarness)
	}
	if c.Override != "" && c.OverrideSource == "" {
		opts.Env = map[string]string{"SCION_HARNESS": c.Override}
	}
	var env map[string]string
	if _, err := containerEnvManager(&env, nil).Start(context.Background(), opts); err != nil {
		t.Fatalf("Start: %v", err)
	}
	return env
}

// TestStart_ContainerHarnessCases pins the container SCION_HARNESS for each
// case of the shared table: the harness the launch resolved, not the stored
// name (ptone/scion#5946), unless opts.Env overrides it. The usage-parsing
// outcome of each value is checked in pkg/sciontool/telemetry from the same
// table.
func TestStart_ContainerHarnessCases(t *testing.T) {
	cases := loadHarnessEnvCases(t)
	for _, name := range []string{"switch", "metadata-less", "legacy", "legacy-default", "legacy-gemini", "override", "override-settings-env"} {
		c, ok := cases[name]
		if !ok {
			t.Fatalf("case %q missing from %s", name, harnessEnvCasesPath)
		}
		t.Run(name, func(t *testing.T) {
			warnings := captureHarnessWarnings(t)
			env := startCase(t, c)
			if got := env["SCION_HARNESS"]; got != c.ContainerHarness {
				t.Fatalf("container SCION_HARNESS = %q, want %q", got, c.ContainerHarness)
			}
			warned := strings.Contains(warnings.String(), containerHarnessOverrideWarning)
			if c.Override != "" {
				// The override (claude) differs from the launched gemini-cli.
				if !warned {
					t.Fatalf("override differing from the launched harness logged no warning; log:\n%s", warnings)
				}
				if !strings.Contains(warnings.String(), "override=claude") || !strings.Contains(warnings.String(), "launched=gemini-cli") {
					t.Fatalf("warning does not name both harnesses; log:\n%s", warnings)
				}
			} else if warned {
				t.Fatalf("override warning logged with no override; log:\n%s", warnings)
			}
		})
	}
	if len(cases) != 7 {
		t.Fatalf("%s has %d cases; add a Start setup for each new one", harnessEnvCasesPath, len(cases))
	}
}

// TestStart_ContainerHarnessOverrideEqualToLaunchedDoesNotWarn: an override
// naming the launched harness is kept and logs nothing.
func TestStart_ContainerHarnessOverrideEqualToLaunchedDoesNotWarn(t *testing.T) {
	warnings := captureHarnessWarnings(t)
	env := startCase(t, harnessEnvCase{Name: "switch", StoredHarness: "claude", Override: "gemini-cli"})
	if got := env["SCION_HARNESS"]; got != "gemini-cli" {
		t.Fatalf("container SCION_HARNESS = %q, want gemini-cli", got)
	}
	if strings.Contains(warnings.String(), containerHarnessOverrideWarning) {
		t.Fatalf("override equal to the launched harness must not warn; log:\n%s", warnings)
	}
}

// TestContainerHarnessEnv pins the precedence of the container's
// SCION_HARNESS: an SCION_HARNESS already in the start env (the caller's,
// or the settings harness_configs.<name>.env merged into it earlier) wins over
// the launched harness; the warning fires only when the two differ, at warn
// level, with fixed text and the two harness names only.
func TestContainerHarnessEnv(t *testing.T) {
	tests := []struct {
		name      string
		env       map[string]string
		launched  string
		wantValue string
		wantSet   bool
		wantWarn  bool
	}{
		{name: "unset, launched", env: map[string]string{}, launched: "gemini-cli", wantValue: "gemini-cli", wantSet: true},
		{name: "unset, generic", env: map[string]string{}, launched: "generic", wantValue: "generic", wantSet: true},
		{name: "unset, nothing launched", env: map[string]string{}, launched: ""},
		{name: "override equal", env: map[string]string{"SCION_HARNESS": "gemini-cli"}, launched: "gemini-cli"},
		{name: "override differs", env: map[string]string{"SCION_HARNESS": "claude"}, launched: "gemini-cli", wantWarn: true},
		{name: "empty override differs", env: map[string]string{"SCION_HARNESS": ""}, launched: "generic", wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			got, set := containerHarnessEnv(context.Background(), tt.env, tt.launched)
			if got != tt.wantValue || set != tt.wantSet {
				t.Fatalf("containerHarnessEnv = (%q, %v), want (%q, %v)", got, set, tt.wantValue, tt.wantSet)
			}
			log := buf.String()
			if !tt.wantWarn {
				if log != "" {
					t.Fatalf("unexpected log: %s", log)
				}
				return
			}
			lines := strings.Split(strings.TrimSpace(log), "\n")
			if len(lines) != 1 {
				t.Fatalf("want one log line, got %q", log)
			}
			want := "level=WARN msg=\"" + containerHarnessOverrideWarning + "\" override=" + fmtSlogValue(tt.env["SCION_HARNESS"]) + " launched=" + tt.launched
			if !strings.HasSuffix(lines[0], want) {
				t.Fatalf("log line = %q, want suffix %q", lines[0], want)
			}
		})
	}
}

// fmtSlogValue renders a string as slog's text handler does for these
// test values.
func fmtSlogValue(v string) string {
	if v == "" {
		return `""`
	}
	return v
}

// TestStart_ModelAliasAfterSwitchUsesResolvedHarness: after a switch from a
// claude harness-config to a gemini-cli one that defines no model_aliases,
// a size alias resolves with gemini-cli's built-in aliases, not the stored
// harness's (ptone/scion#5946), in both SCION_MODEL and argv --model.
func TestStart_ModelAliasAfterSwitchUsesResolvedHarness(t *testing.T) {
	want := harness.DefaultModelAliases("gemini-cli")["large"]
	stale := harness.DefaultModelAliases("claude")["large"]
	if want == "" || stale == "" || want == stale {
		t.Fatalf("fixture: built-in large aliases gemini-cli=%q claude=%q must both exist and differ", want, stale)
	}
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-claude", containerHarnessHCClaude)
	e.projectHC(t, "hc-gemini", containerHarnessHCGemini)
	e.provisionExisting(t, "alias", "hc-claude")
	var env map[string]string
	var args []string
	_, err := containerEnvManager(&env, &args).Start(context.Background(), api.StartOptions{
		Name: "alias", ProjectPath: e.scion, NoAuth: true, HarnessConfig: "hc-gemini",
		Env: map[string]string{"SCION_MODEL": "large"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := env["SCION_MODEL"]; got != want {
		t.Fatalf("SCION_MODEL = %q, want %q (gemini-cli built-in; claude's is %q)", got, want, stale)
	}
	if !containsModelArg(args, want) {
		t.Fatalf("argv %q lacks --model %s", args, want)
	}
}

func containsModelArg(args []string, model string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--model" && args[i+1] == model {
			return true
		}
	}
	return false
}

// TestResolveEffectiveModel_AliasHarnessKey: the built-in fallback is keyed
// on the harness name passed (the resolved harness-config's declared
// harness); an empty name has no built-in table.
func TestResolveEffectiveModel_AliasHarnessKey(t *testing.T) {
	gem := harness.DefaultModelAliases("gemini-cli")["large"]
	cla := harness.DefaultModelAliases("claude")["large"]
	tests := []struct {
		name         string
		aliasHarness string
		want         string
	}{
		// switch claude -> gemini-cli config without aliases
		{name: "switch", aliasHarness: "gemini-cli", want: gem},
		// metadata-less config declaring claude (Generic implementation)
		{name: "metadata-less declared claude", aliasHarness: "claude", want: cla},
		// resolved config declares no harness
		{name: "declared empty", aliasHarness: "", want: "large"},
		// legacy: no stored harness, no harness-config
		{name: "legacy", aliasHarness: "", want: "large"},
		// legacy stored name with no embedded bundle
		{name: "legacy gemini", aliasHarness: "gemini", want: "large"},
		{name: "generic implementation name", aliasHarness: "generic", want: "large"},
		// legacy-default: no stored harness, the settings default
		// harness-config (antigravity in the embedded defaults) resolves
		{name: "legacy default antigravity", aliasHarness: "antigravity", want: harness.DefaultModelAliases("antigravity")["large"]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveEffectiveModel("L", nil, nil, tt.aliasHarness); got != tt.want {
				t.Fatalf("resolveEffectiveModel(L, %q) = %q, want %q", tt.aliasHarness, got, tt.want)
			}
		})
	}
}

// TestStart_ModelAliasLegacyDefault: an agent with no stored harness, whose
// harness-config comes from the settings default. A size alias resolves
// with that config's own model_aliases when it has them; without them, with
// the built-in aliases of the harness the config declares (before
// ptone/scion#5946 the key was the empty stored name, so the alias reached
// the harness verbatim).
func TestStart_ModelAliasLegacyDefault(t *testing.T) {
	builtin := harness.DefaultModelAliases("gemini-cli")["large"]
	if builtin == "" {
		t.Fatal("fixture: gemini-cli has no built-in large alias")
	}
	tests := []struct {
		name, hc, want string
	}{
		{name: "config with model_aliases", hc: containerHarnessHCGemini + "model_aliases:\n  large: config-large\n", want: "config-large"},
		{name: "config without model_aliases", hc: containerHarnessHCGemini, want: builtin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newPolicyTestEnv(t)
			e.projectHC(t, "hc-claude", containerHarnessHCClaude)
			e.projectHC(t, "hc-default", tt.hc)
			e.provisionExisting(t, "legacy-alias", "hc-claude")
			e.rewriteStoredHarness(t, "legacy-alias", "", true)
			e.rewriteSettings(t, "hc-default", "")
			var env map[string]string
			var args []string
			_, err := containerEnvManager(&env, &args).Start(context.Background(), api.StartOptions{
				Name: "legacy-alias", ProjectPath: e.scion, NoAuth: true,
				Env: map[string]string{"SCION_MODEL": "large"},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := env["SCION_HARNESS"]; got != "gemini-cli" {
				t.Errorf("SCION_HARNESS = %q, want gemini-cli (settings default config)", got)
			}
			if got := env["SCION_MODEL"]; got != tt.want {
				t.Fatalf("SCION_MODEL = %q, want %q", got, tt.want)
			}
			if !containsModelArg(args, tt.want) {
				t.Fatalf("argv %q lacks --model %s", args, tt.want)
			}
		})
	}
}
