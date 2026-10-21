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
	"strings"
	"testing"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// builtinAlias returns the embedded harness's concrete model for alias,
// failing the test when the table does not resolve it, so the assertions
// below cannot pass vacuously and survive routine model bumps.
func builtinAlias(t *testing.T, harnessName, alias string) string {
	t.Helper()
	v := harness.DefaultModelAliases(harnessName)[alias]
	if v == "" || v == alias {
		t.Fatalf("built-in %s alias %q = %q, want a concrete model", harnessName, alias, v)
	}
	return v
}

// TestResolveEffectiveModel: the one effective model (ptone/scion#3914) is
// SCION_MODEL from the start env when set, else the broker's config model,
// with any size alias resolved. The broker's config model never replaces a
// SCION_MODEL value.
func TestResolveEffectiveModel(t *testing.T) {
	claudeLarge := builtinAlias(t, "claude", "large")
	claudeMedium := builtinAlias(t, "claude", "medium")
	hcAliases := map[string]string{"large": "hc-large-model", "medium": "hc-medium-model"}

	tests := []struct {
		name        string
		envModel    string
		cfg         *api.ScionConfig
		hcAliases   map[string]string
		harnessName string
		want        string
	}{
		{
			name:     "hub-resolved SCION_MODEL beats a broker harness-config default",
			envModel: "project-model",
			cfg:      &api.ScionConfig{Model: "broker-default-model"},
			want:     "project-model",
		},
		{
			name:     "explicit --env SCION_MODEL beats the config model",
			envModel: "env-model",
			cfg:      &api.ScionConfig{Model: "template-model"},
			want:     "env-model",
		},
		{
			name: "no SCION_MODEL: the broker config model (no hub-resolved model)",
			cfg:  &api.ScionConfig{Model: "template-model"},
			want: "template-model",
		},
		{
			name:        "no SCION_MODEL: a config alias is resolved",
			cfg:         &api.ScionConfig{Model: "large"},
			harnessName: "claude",
			want:        claudeLarge,
		},
		{
			name: "nothing set: no model",
			cfg:  &api.ScionConfig{},
			want: "",
		},
		{
			name: "nil config, no env: no model",
			want: "",
		},
		{
			name:      "leaked alias resolves through the on-disk harness-config aliases",
			envModel:  "large",
			cfg:       &api.ScionConfig{Model: "large"},
			hcAliases: hcAliases,
			want:      "hc-large-model",
		},
		{
			// The pre-#3914 safety net swapped in cfg.Model here, which
			// was the broker's medium default: the wrong tier.
			name:        "leaked alias never takes a differing broker config model",
			envModel:    "large",
			cfg:         &api.ScionConfig{Model: claudeMedium},
			harnessName: "claude",
			want:        claudeLarge,
		},
		{
			name:        "uppercase shorthand alias resolves through the built-in table",
			envModel:    "L",
			harnessName: "claude",
			want:        claudeLarge,
		},
		{
			name:        "on-disk aliases take precedence over the built-in table",
			envModel:    "medium",
			hcAliases:   hcAliases,
			harnessName: "claude",
			want:        "hc-medium-model",
		},
		{
			name:        "alias missing from on-disk aliases falls back to the built-in table",
			envModel:    "large",
			hcAliases:   map[string]string{"small": "hc-small"},
			harnessName: "claude",
			want:        claudeLarge,
		},
		{
			// A non-canonical spelling must not stop at the partial
			// on-disk table's normalized miss.
			name:        "shorthand alias missing from on-disk aliases falls back to the built-in table",
			envModel:    "L",
			hcAliases:   map[string]string{"small": "hc-small"},
			harnessName: "claude",
			want:        claudeLarge,
		},
		{
			name:        "XL shorthand resolves through the built-in table",
			envModel:    "XL",
			hcAliases:   map[string]string{"small": "hc-small"},
			harnessName: "claude",
			want:        builtinAlias(t, "claude", "extra-large"),
		},
		{
			name:        "unresolvable alias is returned normalized",
			envModel:    "L",
			harnessName: "not-a-real-harness",
			want:        "large",
		},
		{
			name:        "unresolvable alias is returned unchanged",
			envModel:    "large",
			harnessName: "not-a-real-harness",
			want:        "large",
		},
		{
			name:     "concrete model is never altered",
			envModel: "gemini-3.6-flash",
			cfg:      &api.ScionConfig{Model: "gemini-3.1-pro-preview"},
			want:     "gemini-3.6-flash",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveEffectiveModel(tt.envModel, tt.cfg, tt.hcAliases, tt.harnessName); got != tt.want {
				t.Errorf("resolveEffectiveModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestModelCommandArgs(t *testing.T) {
	if got := modelCommandArgs("", []string{"--x"}); len(got) != 1 || got[0] != "--x" {
		t.Errorf("empty model: got %v, want [--x]", got)
	}
	got := modelCommandArgs("m1", []string{"--x"})
	want := []string{"--model", "m1", "--x"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestHarnessArgvModelMatchesSCIONModel: for claude, codex and gemini, the
// --model value in the harness command line equals SCION_MODEL, including
// when the broker's own config carries a different (default) model.
func TestHarnessArgvModelMatchesSCIONModel(t *testing.T) {
	for _, name := range []string{"claude", "codex", "gemini-cli"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := config.SeedHarnessConfigFromDir(dir, harnessesEmbed.FS, name, false); err != nil {
				t.Fatalf("SeedHarnessConfigFromDir: %v", err)
			}
			resolved, err := harness.Resolve(context.Background(), harness.ResolveOptions{Name: name, ConfigDirPath: dir})
			if err != nil {
				t.Fatalf("harness.Resolve: %v", err)
			}
			brokerCfg := &api.ScionConfig{Model: "broker-default-model", CommandArgs: []string{"--user-arg"}}
			cases := map[string]string{
				"hub-resolved model":  "hub-resolved-model",
				"leaked alias":        "large",
				"broker config model": "",
			}
			for label, hubModel := range cases {
				env := map[string]string{}
				if hubModel != "" {
					env["SCION_MODEL"] = hubModel
				}
				model := resolveEffectiveModel(env["SCION_MODEL"], brokerCfg, resolved.Config.ModelAliases, name)
				if model != "" {
					env["SCION_MODEL"] = model
				}
				argv := resolved.Harness.GetCommand("do the task", false, modelCommandArgs(model, brokerCfg.CommandArgs))
				got, ok := argvModel(argv)
				if !ok {
					t.Fatalf("%s: no --model in argv %v", label, argv)
				}
				if got != env["SCION_MODEL"] {
					t.Errorf("%s: argv --model %q != SCION_MODEL %q (argv %v)", label, got, env["SCION_MODEL"], argv)
				}
				if got == "large" {
					t.Errorf("%s: alias reached the harness verbatim", label)
				}
				if hubModel != "" && got == "broker-default-model" {
					t.Errorf("%s: broker default overrode the hub-resolved model", label)
				}
			}
		})
	}
}

// argvModel returns the value following the single --model flag in argv.
func argvModel(argv []string) (string, bool) {
	val, found := "", false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--model" {
			if found {
				return "", false
			}
			val, found = argv[i+1], true
		}
	}
	return val, found
}

// modelTestHC is a project harness-config whose own model (the broker's
// harness-config default) differs from every hub-resolved model below.
const modelTestHC = policyTestDecl + "model: broker-default-model\nmodel_aliases:\n  large: hc-large-model\n"

// TestStart_ArgvModelAndSCIONModelAgree runs Start and captures the
// runtime.RunConfig it launches: the harness argv --model (CommandArgs) and
// the container env SCION_MODEL carry the same model, the hub-resolved one
// when the start env has it (ptone/scion#3914). Before the fix, argv was
// built from the broker's own config (here the harness-config default).
func TestStart_ArgvModelAndSCIONModelAgree(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "hub-resolved SCION_MODEL", env: map[string]string{"SCION_MODEL": "hub-model"}, want: "hub-model"},
		{name: "explicit --env SCION_MODEL", env: map[string]string{"SCION_MODEL": "env-model"}, want: "env-model"},
		{name: "broker config only (no SCION_MODEL)", want: "broker-default-model"},
		{name: "leaked alias resolved through the harness-config aliases", env: map[string]string{"SCION_MODEL": "large"}, want: "hc-large-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newPolicyTestEnv(t)
			e.projectHC(t, "hc-model", modelTestHC)
			var runCfg runtime.RunConfig
			runs := 0
			mgr := NewManager(&runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
					runs++
					runCfg = cfg
					return "mock-id", nil
				},
			})
			env := map[string]string{}
			for k, v := range tc.env {
				env[k] = v
			}
			const name = "model-agent"
			if _, err := mgr.Start(context.Background(), api.StartOptions{
				Name: name, ProjectPath: e.scion, HarnessConfig: "hc-model", NoAuth: true, Env: env,
			}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if runs != 1 {
				t.Fatalf("runtime Run called %d times, want 1", runs)
			}
			argv, ok := argvModel(runCfg.CommandArgs)
			if !ok {
				t.Fatalf("no single --model in CommandArgs %v", runCfg.CommandArgs)
			}
			envModel, found := "", 0
			for _, kv := range runCfg.Env {
				if v, ok := strings.CutPrefix(kv, "SCION_MODEL="); ok {
					envModel = v
					found++
				}
			}
			if found != 1 {
				t.Fatalf("SCION_MODEL appears %d times in the run env, want 1", found)
			}
			if argv != tc.want || envModel != tc.want {
				t.Errorf("argv --model = %q, SCION_MODEL = %q; want both %q", argv, envModel, tc.want)
			}

			// The effective model is not written into the agent's stored
			// config: a hub-resolved value never becomes agent config.
			if tc.env["SCION_MODEL"] != "" {
				data, err := os.ReadFile(filepath.Join(e.scion, "agents", name, "scion-agent.json"))
				if err != nil {
					t.Fatalf("read scion-agent.json: %v", err)
				}
				var stored struct {
					Model string `json:"model"`
				}
				if err := json.Unmarshal(data, &stored); err != nil {
					t.Fatalf("parse scion-agent.json: %v", err)
				}
				if stored.Model == tc.want {
					t.Errorf("scion-agent.json model = %q: the effective model was persisted", stored.Model)
				}
			}
		})
	}
}
