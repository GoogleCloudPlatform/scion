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
	"errors"
	"fmt"
	"net/http"
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

func TestBuildStartContext_InitFirewallUnionWithHub(t *testing.T) {
	brokerPolicy, err := firewallpolicy.Normalize([]string{"pypi.org", "shared.example.com"}, false)
	if err != nil {
		t.Fatalf("Normalize broker: %v", err)
	}
	hubPolicy, err := firewallpolicy.Normalize([]string{"artifactory.corp.example", "shared.example.com"}, true)
	if err != nil {
		t.Fatalf("Normalize hub: %v", err)
	}

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.InitFirewall = brokerPolicy
	srv := newTestServerForStartContext(t, cfg)

	sc, scErr := srv.buildStartContext(context.Background(), startContextInputs{
		Name:         "agent-fw-union",
		Operation:    opCreate,
		InitFirewall: hubPolicy,
	})
	if scErr != nil {
		t.Fatalf("buildStartContext: %v", scErr)
	}

	want := firewallpolicy.Policy{
		AllowDomains: []string{"artifactory.corp.example", "pypi.org", "shared.example.com"},
		AllowSSH:     true,
	}
	if !reflect.DeepEqual(sc.Opts.InitFirewall, want) {
		t.Fatalf("StartOptions.InitFirewall = %#v, want %#v", sc.Opts.InitFirewall, want)
	}
}

func TestBuildStartContext_InitFirewallUnionCapExceeded(t *testing.T) {
	// Directly construct policies whose combined unique domains exceed 128.
	brokerDomains := make([]string, 65)
	for i := range brokerDomains {
		brokerDomains[i] = fmt.Sprintf("b-%03d.example.com", i)
	}
	hubDomains := make([]string, 64)
	for i := range hubDomains {
		hubDomains[i] = fmt.Sprintf("h-%03d.example.com", i)
	}

	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	cfg.InitFirewall = firewallpolicy.Policy{AllowDomains: brokerDomains}
	srv := newTestServerForStartContext(t, cfg)

	_, scErr := srv.buildStartContext(context.Background(), startContextInputs{
		Name:         "agent-fw-cap",
		Operation:    opCreate,
		InitFirewall: firewallpolicy.Policy{AllowDomains: hubDomains},
	})
	if scErr == nil {
		t.Fatal("expected buildStartContext error when union exceeds 128 domains")
	}
	var sce *startContextError
	if !errors.As(scErr, &sce) || sce.Status != http.StatusBadRequest {
		t.Errorf("err = %v, want *startContextError with status 400", scErr)
	}
}
