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

package runtime

import (
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/firewallpolicy"
)

func TestBuildNetnsSetupArgs_InitFirewallPolicy(t *testing.T) {
	policy, err := firewallpolicy.Normalize([]string{"pypi.org", "files.pythonhosted.org"}, true)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	t.Run("emits policy env only from typed field when firewall is effective", func(t *testing.T) {
		cfg := RunConfig{
			Image:        "scion-claude:latest",
			InitFirewall: policy,
			Env: []string{
				"SCION_METADATA_MODE=block",
				"SCION_METADATA_PORT=18380",
				"SCION_INIT_FIREWALL=true",
				firewallpolicy.EnvAllowDomains + "=evil.example.com",
				firewallpolicy.EnvAllowSSH + "=false",
			},
			ResolvedSecrets: []api.ResolvedSecret{
				{Type: "environment", Target: firewallpolicy.EnvAllowDomains, Value: "evil-secret.example.com"},
			},
			FallbackEnv: map[string]string{
				firewallpolicy.EnvAllowDomains: "evil-fallback.example.com",
			},
		}

		args := buildNetnsSetupArgs("abcdef0123456789", cfg)
		wantDomains := firewallpolicy.EnvAllowDomains + "=files.pythonhosted.org,pypi.org"
		wantSSH := firewallpolicy.EnvAllowSSH + "=true"

		if !slices.Contains(args, wantDomains) {
			t.Errorf("setup args missing %q: %v", wantDomains, args)
		}
		if !slices.Contains(args, wantSSH) {
			t.Errorf("setup args missing %q: %v", wantSSH, args)
		}
		for _, a := range args {
			if strings.Contains(a, "evil") {
				t.Errorf("untrusted value leaked into setup args: %q", a)
			}
		}
	})

	t.Run("omits policy env when firewall is off, false, or invalid", func(t *testing.T) {
		for _, fwVal := range []string{"", "false", "0", "not-a-bool"} {
			cfg := RunConfig{
				Image:        "scion-claude:latest",
				InitFirewall: policy,
				Env: []string{
					"SCION_METADATA_MODE=block",
					"SCION_INIT_FIREWALL=" + fwVal,
				},
			}
			args := buildNetnsSetupArgs("abcdef0123456789", cfg)
			for _, a := range args {
				if strings.HasPrefix(a, firewallpolicy.EnvPrefix) {
					t.Errorf("SCION_INIT_FIREWALL=%q: unexpected policy arg %q in %v", fwVal, a, args)
				}
			}
		}
	})
}

func TestBuildCommonRunArgs_StripsReservedInitFirewallPrefix(t *testing.T) {
	cfg := RunConfig{
		Name:         "test-agent",
		Image:        "scion-claude:latest",
		UnixUsername: "scion",
		RuntimeName:  "docker",
		Harness:      &harness.Generic{},
		Env: []string{
			"SCION_INIT_FIREWALL=true",
			firewallpolicy.EnvAllowDomains + "=evil-env.example.com",
			firewallpolicy.EnvAllowSSH + "=true",
			firewallpolicy.EnvPrefix + "EXTRA=1",
		},
		ResolvedSecrets: []api.ResolvedSecret{
			{Type: "environment", Target: firewallpolicy.EnvAllowDomains, Value: "evil-secret.example.com"},
			{Type: "environment", Target: firewallpolicy.EnvAllowSSH, Value: "true"},
		},
		FallbackEnv: map[string]string{
			firewallpolicy.EnvAllowDomains: "evil-fallback.example.com",
			firewallpolicy.EnvAllowSSH:     "true",
		},
	}

	args, err := buildCommonRunArgs(cfg)
	if err != nil {
		t.Fatalf("buildCommonRunArgs: %v", err)
	}

	if !slices.Contains(args, "SCION_INIT_FIREWALL=true") {
		t.Errorf("bare SCION_INIT_FIREWALL should remain in agent args: %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, firewallpolicy.EnvPrefix) {
			t.Errorf("reserved init-firewall policy env %q leaked into agent container args", a)
		}
	}
}

func TestReservedAgentEnvName_InitFirewallPrefix(t *testing.T) {
	if reservedAgentEnvName("SCION_INIT_FIREWALL") {
		t.Error("bare SCION_INIT_FIREWALL must NOT be reserved")
	}
	for _, name := range []string{
		firewallpolicy.EnvAllowDomains,
		firewallpolicy.EnvAllowSSH,
		firewallpolicy.EnvPrefix + "FUTURE_KEY",
	} {
		if !reservedAgentEnvName(name) {
			t.Errorf("reservedAgentEnvName(%q) = false, want true", name)
		}
	}

	env := map[string]string{
		"SCION_INIT_FIREWALL":          "true",
		firewallpolicy.EnvAllowDomains: "evil.example.com",
		firewallpolicy.EnvAllowSSH:     "true",
		"KEEP_ME":                      "1",
	}
	dropReservedAgentEnv(env)
	if env["SCION_INIT_FIREWALL"] != "true" || env["KEEP_ME"] != "1" {
		t.Errorf("dropReservedAgentEnv removed non-reserved keys: %v", env)
	}
	if _, ok := env[firewallpolicy.EnvAllowDomains]; ok {
		t.Error("dropReservedAgentEnv did not remove SCION_INIT_FIREWALL_ALLOW_DOMAINS")
	}
	if _, ok := env[firewallpolicy.EnvAllowSSH]; ok {
		t.Error("dropReservedAgentEnv did not remove SCION_INIT_FIREWALL_ALLOW_SSH")
	}
}
