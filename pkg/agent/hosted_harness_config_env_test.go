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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// hostedEnvHostValue is the broker host's value of hostedEnvHostVar; it
// must never reach a container from member config on a hosted broker.
const (
	hostedEnvHostVar   = "P3_BROKER_HOST_VAR"
	hostedEnvHostValue = "broker-host-only-value"
)

// hostedEnvManager records the RunConfig of every container run.
func hostedEnvManager(runs *[]runtime.RunConfig) Manager {
	return NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			*runs = append(*runs, cfg)
			return "mock-id", nil
		},
	})
}

func assertHostedEnvRefused(t *testing.T, err error, entry, hcName string) {
	t.Helper()
	var re *hostedguard.RefusalError
	if !errors.As(err, &re) {
		t.Fatalf("expected a *hostedguard.RefusalError, got %v", err)
	}
	if re.Field != "env" || re.Entry != entry || re.Origin != (hostedguard.Origin{Kind: hostedguard.OriginHarnessConfig, Name: hcName}) {
		t.Errorf("refusal = %+v, want env %q from harness config %q", re, entry, hcName)
	}
	if strings.Contains(err.Error(), hostedEnvHostValue) {
		t.Errorf("error %q echoes a host value", err.Error())
	}
}

var hostedEnvCases = []struct {
	name, key, value string
}{
	{"reference", "HC_REF", "${" + hostedEnvHostVar + "}"},
	{"bare reference", "HC_REF", "$" + hostedEnvHostVar},
	{"empty passthrough", hostedEnvHostVar, ""},
	{"reserved name", "SCION_HUB_ENDPOINT", "http://127.0.0.1:1"},
}

func hcEnvBody(key, value string) string {
	return policyTestDecl + "env:\n  " + key + ": \"" + value + "\"\n"
}

// A hosted broker refuses a project-tier harness config's env when it
// references the broker's environment, passes a host variable through, or
// sets a reserved name: at admission and at start, before any container
// runs. A workstation broker still expands it.
func TestHostedHarnessConfigEnv_ProjectTier(t *testing.T) {
	t.Setenv(hostedEnvHostVar, hostedEnvHostValue)
	for _, tc := range hostedEnvCases {
		for _, hosted := range []bool{true, false} {
			mode := map[bool]string{true: "hosted", false: "workstation"}[hosted]
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				e := newPolicyTestEnv(t)
				e.projectHC(t, "hc-env", hcEnvBody(tc.key, tc.value))
				opts := api.StartOptions{Name: "e-" + mode, ProjectPath: e.scion, HarnessConfig: "hc-env", NoAuth: true, HostedMode: hosted}

				preErr := PreflightResolve(context.Background(), opts)
				var runs []runtime.RunConfig
				_, startErr := hostedEnvManager(&runs).Start(context.Background(), opts)
				if hosted {
					assertHostedEnvRefused(t, preErr, tc.key, "hc-env")
					assertHostedEnvRefused(t, startErr, tc.key, "hc-env")
					if len(runs) != 0 {
						t.Error("the container must not run when the harness config is refused")
					}
					return
				}
				if preErr != nil || startErr != nil {
					t.Fatalf("workstation: preflight %v, start %v", preErr, startErr)
				}
				if len(runs) != 1 {
					t.Fatalf("expected one container run, got %d", len(runs))
				}
				if tc.key != "SCION_HUB_ENDPOINT" {
					if v, _ := runEnvValue(runs[0], tc.key); v != hostedEnvHostValue {
						t.Errorf("workstation %s = %q, want the host value expanded", tc.key, v)
					}
				}
			})
		}
	}
}

// A harness config bundled in a template is member content too, and so is
// its env. ProvisionAgent refuses it through the hosted context flag.
func TestHostedHarnessConfigEnv_TemplateBundledAndProvision(t *testing.T) {
	t.Setenv(hostedEnvHostVar, hostedEnvHostValue)
	t.Run("template bundled start", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		tpl := e.template(t, "tple", "harness_config: hc-tpl\n")
		writePolicyHC(t, filepath.Join(tpl, "harness-configs", "hc-tpl"), hcEnvBody("HC_REF", "${"+hostedEnvHostVar+"}"))
		var runs []runtime.RunConfig
		_, err := hostedEnvManager(&runs).Start(context.Background(), api.StartOptions{Name: "t", ProjectPath: e.scion, Template: "tple", NoAuth: true, HostedMode: true})
		assertHostedEnvRefused(t, err, "HC_REF", "hc-tpl")
		if len(runs) != 0 {
			t.Error("the container must not run when the harness config is refused")
		}
	})
	t.Run("provision", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-env", hcEnvBody(hostedEnvHostVar, ""))
		_, _, _, err := ProvisionAgent(api.ContextWithHostedMode(context.Background()), "prov", "", "", "hc-env", e.scion, "", "", "", "")
		assertHostedEnvRefused(t, err, hostedEnvHostVar, "hc-env")
	})
	t.Run("literal env allowed", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		e.projectHC(t, "hc-env", hcEnvBody("HC_LITERAL", "plain"))
		var runs []runtime.RunConfig
		if _, err := hostedEnvManager(&runs).Start(context.Background(), api.StartOptions{Name: "l", ProjectPath: e.scion, HarnessConfig: "hc-env", NoAuth: true, HostedMode: true}); err != nil {
			t.Fatalf("hosted start with literal harness-config env: %v", err)
		}
		if v, _ := runEnvValue(runs[0], "HC_LITERAL"); v != "plain" {
			t.Errorf("HC_LITERAL = %q, want plain", v)
		}
	})
}

// The global harness-configs directory is operator content: its env
// references still expand on a hosted broker.
func TestHostedHarnessConfigEnv_GlobalDirStillExpands(t *testing.T) {
	t.Setenv(hostedEnvHostVar, hostedEnvHostValue)
	e := newPolicyTestEnv(t)
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	writePolicyHC(t, filepath.Join(globalDir, "harness-configs", "hc-global"), hcEnvBody("HC_REF", "${"+hostedEnvHostVar+"}"))
	var runs []runtime.RunConfig
	if _, err := hostedEnvManager(&runs).Start(context.Background(), api.StartOptions{Name: "g", ProjectPath: e.scion, HarnessConfig: "hc-global", NoAuth: true, HostedMode: true}); err != nil {
		t.Fatalf("hosted start with a global harness config: %v", err)
	}
	if v, _ := runEnvValue(runs[0], "HC_REF"); v != hostedEnvHostValue {
		t.Errorf("HC_REF = %q, want the operator reference expanded", v)
	}
}

// Template env is not refused on a hosted broker: it is already inert
// toward the broker host (template_env_keys.go, config/template_key_class.go).
// Its values are literal (never expanded against the broker environment), an
// empty value is skipped rather than passed through, and SCION_* names are
// stripped. This pins that a hosted start with such a template runs and that
// no broker-host value reaches the container through it.
func TestHostedTemplateEnv_NeverReachesHost(t *testing.T) {
	t.Setenv(hostedEnvHostVar, hostedEnvHostValue)
	t.Setenv("TPL_EMPTY", hostedEnvHostValue)
	t.Setenv("SCION_P3_TEMPLATE_KEY", hostedEnvHostValue)
	e := newPolicyTestEnv(t)
	tpl := e.template(t, "tplenv", "harness_config: hc-plain\nenv:\n"+
		"  TPL_REF: \"${"+hostedEnvHostVar+"}\"\n"+
		"  TPL_BARE: \"$"+hostedEnvHostVar+"\"\n"+
		"  TPL_EMPTY: \"\"\n"+
		"  SCION_P3_TEMPLATE_KEY: \"set-by-template\"\n")
	writePolicyHC(t, filepath.Join(tpl, "harness-configs", "hc-plain"), policyTestDecl)

	var runs []runtime.RunConfig
	if _, err := hostedEnvManager(&runs).Start(context.Background(), api.StartOptions{Name: "tenv", ProjectPath: e.scion, Template: "tplenv", NoAuth: true, HostedMode: true}); err != nil {
		t.Fatalf("hosted start with template env: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected one container run, got %d", len(runs))
	}
	run := runs[0]
	for _, e := range run.Env {
		if strings.Contains(e, hostedEnvHostValue) {
			t.Errorf("container env %q carries a broker-host value", e)
		}
	}
	for k, v := range run.FallbackEnv {
		if strings.Contains(v, hostedEnvHostValue) {
			t.Errorf("template fallback env %s carries a broker-host value", k)
		}
	}
	if got := run.FallbackEnv["TPL_REF"]; got != "${"+hostedEnvHostVar+"}" {
		t.Errorf("TPL_REF = %q, want the literal reference", got)
	}
	if got := run.FallbackEnv["TPL_BARE"]; got != "$"+hostedEnvHostVar {
		t.Errorf("TPL_BARE = %q, want the literal reference", got)
	}
	if _, ok := run.FallbackEnv["TPL_EMPTY"]; ok {
		t.Error("an empty template value must be skipped, not passed through")
	}
	if _, ok := run.FallbackEnv["SCION_P3_TEMPLATE_KEY"]; ok {
		t.Error("a template SCION_* key must be stripped")
	}
	if _, ok := runEnvValue(run, "SCION_P3_TEMPLATE_KEY"); ok {
		t.Error("a template SCION_* key must not reach the container env")
	}
}

// A hosted broker refuses a reserved name in a project-tier harness config's
// env_template (at admission and at start, no run) unless the member-safe
// list allows it for harness configs (SCION_AGENT_NAME); a workstation broker
// runs it.
func TestHostedHarnessConfigEnvTemplate(t *testing.T) {
	for _, tc := range []struct {
		name, key string
		refused   bool
	}{
		{"reserved name", "SCION_HUB_ENDPOINT", true},
		{"agent name", "SCION_AGENT_NAME", false},
		{"non-reserved", "NO_COLOR", false},
	} {
		for _, hosted := range []bool{true, false} {
			mode := map[bool]string{true: "hosted", false: "workstation"}[hosted]
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				e := newPolicyTestEnv(t)
				e.projectHC(t, "hc-tpl-env", policyTestDecl+"env_template:\n  "+tc.key+": \"{{ .AgentName }}\"\n")
				opts := api.StartOptions{Name: "et-" + mode, ProjectPath: e.scion, HarnessConfig: "hc-tpl-env", NoAuth: true, HostedMode: hosted}
				preErr := PreflightResolve(context.Background(), opts)
				var runs []runtime.RunConfig
				_, startErr := hostedEnvManager(&runs).Start(context.Background(), opts)
				if hosted && tc.refused {
					for _, err := range []error{preErr, startErr} {
						var re *hostedguard.RefusalError
						if !errors.As(err, &re) || re.Field != "env_template" || re.Entry != tc.key {
							t.Fatalf("expected an env_template refusal of %s, got %v", tc.key, err)
						}
					}
					if len(runs) != 0 {
						t.Error("the container must not run when the harness config is refused")
					}
					return
				}
				if preErr != nil || startErr != nil {
					t.Fatalf("preflight %v, start %v", preErr, startErr)
				}
				if len(runs) != 1 {
					t.Fatalf("expected one container run, got %d", len(runs))
				}
			})
		}
	}
}
