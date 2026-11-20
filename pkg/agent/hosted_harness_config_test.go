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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hostedguard"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

const hostedHCTarget = "/hc-data"

// hostedHCBody is a declarative harness config with one host volume.
func hostedHCBody(t *testing.T) string {
	t.Helper()
	return policyTestDecl + "volumes:\n  - source: " + t.TempDir() + "\n    target: " + hostedHCTarget + "\n"
}

// hostedHCManager records the volumes of every container run.
func hostedHCManager(runs *[][]api.VolumeMount) Manager {
	return NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			*runs = append(*runs, cfg.Volumes)
			return "mock-id", nil
		},
	})
}

func assertHostedHCRefused(t *testing.T, err error, name string) {
	t.Helper()
	var re *hostedguard.RefusalError
	if !errors.As(err, &re) {
		t.Fatalf("expected a *hostedguard.RefusalError, got %v", err)
	}
	if re.Entry != hostedHCTarget || re.Origin != (hostedguard.Origin{Kind: hostedguard.OriginHarnessConfig, Name: name}) {
		t.Errorf("refusal = %+v, want volume %q from harness config %q", re, hostedHCTarget, name)
	}
	if !strings.Contains(re.Error(), `harness config "`+name+`"`) {
		t.Errorf("message %q does not name the harness config", re.Error())
	}
}

func hostedHCMounted(t *testing.T, runs [][]api.VolumeMount) {
	t.Helper()
	if len(runs) != 1 {
		t.Fatalf("expected one container run, got %d", len(runs))
	}
	for _, v := range runs[0] {
		if v.Target == hostedHCTarget {
			return
		}
	}
	t.Errorf("harness-config volume %q not in the run's volumes %+v", hostedHCTarget, runs[0])
}

// A hosted broker refuses a project-tier harness config's volumes at
// admission, at provision and at start, before any container runs. A
// workstation broker mounts them.
func TestHostedHarnessConfig_ProjectTier(t *testing.T) {
	for _, hosted := range []bool{true, false} {
		name := map[bool]string{true: "hosted", false: "workstation"}[hosted]
		t.Run(name, func(t *testing.T) {
			e := newPolicyTestEnv(t)
			e.projectHC(t, "hc-vol", hostedHCBody(t))
			opts := api.StartOptions{Name: "p-" + name, ProjectPath: e.scion, HarnessConfig: "hc-vol", NoAuth: true, HostedMode: hosted}

			preErr := PreflightResolve(context.Background(), opts)
			var runs [][]api.VolumeMount
			_, startErr := hostedHCManager(&runs).Start(context.Background(), opts)
			if hosted {
				assertHostedHCRefused(t, preErr, "hc-vol")
				assertHostedHCRefused(t, startErr, "hc-vol")
				if len(runs) != 0 {
					t.Error("the container must not run when the harness config is refused")
				}
				return
			}
			if preErr != nil || startErr != nil {
				t.Fatalf("workstation: preflight %v, start %v", preErr, startErr)
			}
			hostedHCMounted(t, runs)
		})
	}
}

// A harness config bundled in a template is member content too.
func TestHostedHarnessConfig_TemplateBundled(t *testing.T) {
	for _, hosted := range []bool{true, false} {
		name := map[bool]string{true: "hosted", false: "workstation"}[hosted]
		t.Run(name, func(t *testing.T) {
			e := newPolicyTestEnv(t)
			tpl := e.template(t, "tplx", "harness_config: hc-tpl\n")
			writePolicyHC(t, filepath.Join(tpl, "harness-configs", "hc-tpl"), hostedHCBody(t))
			opts := api.StartOptions{Name: "t-" + name, ProjectPath: e.scion, Template: "tplx", NoAuth: true, HostedMode: hosted}

			var runs [][]api.VolumeMount
			_, err := hostedHCManager(&runs).Start(context.Background(), opts)
			if hosted {
				assertHostedHCRefused(t, err, "hc-tpl")
				if len(runs) != 0 {
					t.Error("the container must not run when the harness config is refused")
				}
				return
			}
			if err != nil {
				t.Fatalf("workstation start: %v", err)
			}
			hostedHCMounted(t, runs)
		})
	}
}

// An agent provisioned before the check (or on a workstation) is refused at
// its next hosted start: Start checks the harness config it resolves.
func TestHostedHarnessConfig_ExistingAgentRefusedAtStart(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-vol", hostedHCBody(t))
	e.provisionExisting(t, "existing", "hc-vol")

	var runs [][]api.VolumeMount
	_, err := hostedHCManager(&runs).Start(context.Background(), api.StartOptions{Name: "existing", ProjectPath: e.scion, NoAuth: true, HostedMode: true})
	assertHostedHCRefused(t, err, "hc-vol")
	if len(runs) != 0 {
		t.Error("the container must not run when the harness config is refused")
	}
}

// ProvisionAgent (a create without a start) refuses through the hosted
// context flag buildProvisionContext sets.
func TestHostedHarnessConfig_ProvisionRefused(t *testing.T) {
	e := newPolicyTestEnv(t)
	e.projectHC(t, "hc-vol", hostedHCBody(t))
	ctx := api.ContextWithHostedMode(context.Background())
	_, _, _, err := ProvisionAgent(ctx, "prov", "", "", "hc-vol", e.scion, "", "", "", "")
	assertHostedHCRefused(t, err, "hc-vol")
	if _, statErr := os.Stat(filepath.Join(e.scion, "agents", "prov", "scion-agent.json")); statErr == nil {
		t.Error("a refused provision must not write the agent's config")
	}
}

// Operator sources still mount on a hosted broker: the global
// harness-configs directory, and a hub-hydrated copy (checked by the Hub
// when it was stored).
func TestHostedHarnessConfig_OperatorSourcesMount(t *testing.T) {
	t.Run("global dir", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		globalDir, err := config.GetGlobalDir()
		if err != nil {
			t.Fatal(err)
		}
		writePolicyHC(t, filepath.Join(globalDir, "harness-configs", "hc-global"), hostedHCBody(t))
		var runs [][]api.VolumeMount
		if _, err := hostedHCManager(&runs).Start(context.Background(), api.StartOptions{
			Name: "g", ProjectPath: e.scion, HarnessConfig: "hc-global", NoAuth: true, HostedMode: true,
		}); err != nil {
			t.Fatalf("hosted start with a global harness config: %v", err)
		}
		hostedHCMounted(t, runs)
	})
	t.Run("hub hydrated", func(t *testing.T) {
		e := newPolicyTestEnv(t)
		hydrated := filepath.Join(t.TempDir(), "hc-hub")
		writePolicyHC(t, hydrated, hostedHCBody(t))
		var runs [][]api.VolumeMount
		if _, err := hostedHCManager(&runs).Start(context.Background(), api.StartOptions{
			Name: "h", ProjectPath: e.scion, HarnessConfig: "hc-hub", HarnessConfigPath: hydrated, NoAuth: true, HostedMode: true,
		}); err != nil {
			t.Fatalf("hosted start with a hub-hydrated harness config: %v", err)
		}
		hostedHCMounted(t, runs)
	})
}

// TestCheckHostedHarnessConfig covers the source and runtime decisions on
// their own.
func TestCheckHostedHarnessConfig(t *testing.T) {
	local := []api.VolumeMount{{Source: "/srv/x", Target: hostedHCTarget}}
	gcs := []api.VolumeMount{{Type: "gcs", Bucket: "b", Target: hostedHCTarget}}
	dir := func(src config.HarnessConfigSource, vols []api.VolumeMount) *config.HarnessConfigDir {
		return &config.HarnessConfigDir{Name: "hc", Source: src, Config: config.HarnessConfigEntry{Volumes: vols}}
	}
	cases := []struct {
		name    string
		hosted  bool
		dir     *config.HarnessConfigDir
		runtime string
		refused bool
	}{
		{"template-bundled host volume", true, dir(config.HarnessConfigSourceTemplateBundled, local), "docker", true},
		{"template-bundled gcs on docker", true, dir(config.HarnessConfigSourceTemplateBundled, gcs), "docker", true},
		{"template-bundled gcs on kubernetes", true, dir(config.HarnessConfigSourceTemplateBundled, gcs), hostedguard.RuntimeKubernetes, false},
		{"template-bundled gcs, runtime unknown", true, dir(config.HarnessConfigSourceTemplateBundled, gcs), "", false},
		{"workstation", false, dir(config.HarnessConfigSourceTemplateBundled, local), "docker", false},
		{"hub hydrated", true, dir(config.HarnessConfigSourceHubHydrated, local), "docker", false},
		// A broker-local dir not marked project tier is the global tier.
		{"global broker-local", true, dir(config.HarnessConfigSourceBrokerLocal, local), "docker", false},
		{"builtin", true, dir(config.HarnessConfigSourceBuiltin, local), "docker", false},
		{"no volumes", true, dir(config.HarnessConfigSourceTemplateBundled, nil), "docker", false},
		{"no dir", true, nil, "docker", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkHostedHarnessConfig(tc.hosted, tc.dir, tc.runtime)
			if refused := err != nil; refused != tc.refused {
				t.Fatalf("refused = %v (%v), want %v", refused, err, tc.refused)
			}
		})
	}
}
