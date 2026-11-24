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
	"reflect"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/firewallpolicy"
)

func TestBuildStartContext_InitFirewallPolicyAndEnvStrip(t *testing.T) {
	brokerPolicy, err := firewallpolicy.Normalize([]string{"pypi.org", "files.pythonhosted.org"}, true)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.InitFirewall = brokerPolicy
	srv := newTestServerForStartContext(t, cfg)

	sc, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name:      "agent-fw",
		Operation: opCreate,
		ResolvedEnv: map[string]string{
			"SCION_INIT_FIREWALL":               "true",
			firewallpolicy.EnvAllowDomains:      "evil-resolved.example.com",
			firewallpolicy.EnvAllowSSH:          "true",
			firewallpolicy.EnvPrefix + "CUSTOM": "injected",
		},
		Config: &CreateAgentConfig{
			Env: []string{
				firewallpolicy.EnvAllowDomains + "=evil-config.example.com",
				firewallpolicy.EnvAllowSSH + "=true",
				"NORMAL_KEY=keep-me",
			},
		},
	})
	if err != nil {
		t.Fatalf("buildStartContext: %v", err)
	}

	if !reflect.DeepEqual(sc.Opts.InitFirewall, brokerPolicy) {
		t.Fatalf("StartOptions.InitFirewall = %#v, want %#v", sc.Opts.InitFirewall, brokerPolicy)
	}

	// Bare SCION_INIT_FIREWALL remains user-settable.
	if sc.Opts.Env["SCION_INIT_FIREWALL"] != "true" {
		t.Errorf("StartOptions.Env[SCION_INIT_FIREWALL] = %q, want %q", sc.Opts.Env["SCION_INIT_FIREWALL"], "true")
	}
	if sc.Opts.Env["NORMAL_KEY"] != "keep-me" {
		t.Errorf("StartOptions.Env[NORMAL_KEY] = %q, want %q", sc.Opts.Env["NORMAL_KEY"], "keep-me")
	}

	// Reserved SCION_INIT_FIREWALL_* keys must be deleted from the merged env.
	for _, k := range []string{
		firewallpolicy.EnvAllowDomains,
		firewallpolicy.EnvAllowSSH,
		firewallpolicy.EnvPrefix + "CUSTOM",
	} {
		if v, exists := sc.Opts.Env[k]; exists {
			t.Errorf("StartOptions.Env[%q] = %q, want key deleted", k, v)
		}
	}
}
