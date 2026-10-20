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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// reservedHostEnvNames are host env names reserved for the server process or
// holding its own hub credentials; they must never reach an agent from the
// host env.
var reservedHostEnvNames = []string{
	"SCION_SERVER_SESSION_SECRET",
	"SESSION_SECRET",
	"SCION_SERVER_HUB_CONDUIT_PEERSECRET",
	"SCION_SERVER_HUB_CONDUIT_PEER_SECRET",
	"SCION_SERVER_DATABASE_URL",
	"SCION_DEV_TOKEN",
	"SCION_AUTH_TOKEN",
}

const reservedHostEnvSentinel = "reserved-host-value-0123456789"

func setReservedHostEnv(t *testing.T) {
	t.Helper()
	for _, name := range reservedHostEnvNames {
		t.Setenv(name, reservedHostEnvSentinel)
	}
}

func assertNoReservedValue(t *testing.T, env, warnings []string) {
	t.Helper()
	for _, e := range env {
		if strings.Contains(e, reservedHostEnvSentinel) {
			t.Errorf("agent env carries a reserved host value: %q", strings.SplitN(e, "=", 2)[0])
		}
	}
	for _, w := range warnings {
		if strings.Contains(w, reservedHostEnvSentinel) {
			t.Errorf("warning carries a reserved host value: %q", w)
		}
	}
}

func assertWarnsName(t *testing.T, warnings []string, name string) {
	t.Helper()
	for _, w := range warnings {
		if strings.Contains(w, "'"+name+"'") && strings.Contains(w, "reserved for the server") {
			return
		}
	}
	t.Errorf("no reserved-name warning for %s in %v", name, warnings)
}

func TestBuildAgentEnv_EmptyValueDoesNotPassReservedHostEnv(t *testing.T) {
	setReservedHostEnv(t)
	for _, name := range reservedHostEnvNames {
		t.Run(name, func(t *testing.T) {
			cfg := &api.ScionConfig{Env: map[string]string{name: ""}}
			for _, brokerMode := range []bool{false, true} {
				env, warnings, missing, _ := buildAgentEnv(cfg, nil, nil, brokerMode)
				assertNoReservedValue(t, env, warnings)
				if envToMap(env)[name] != "" {
					t.Errorf("brokerMode=%v: %s present in agent env", brokerMode, name)
				}
				for _, k := range missing {
					if k == name {
						t.Errorf("brokerMode=%v: %s reported as missing instead of omitted", brokerMode, name)
					}
				}
				assertWarnsName(t, warnings, name)
			}
		})
	}
}

func TestBuildAgentEnv_ExpansionOfReservedHostEnvIsEmpty(t *testing.T) {
	setReservedHostEnv(t)
	for _, name := range reservedHostEnvNames {
		t.Run(name, func(t *testing.T) {
			cfg := &api.ScionConfig{Env: map[string]string{
				"BRACED":          "${" + name + "}",
				"BARE":            "$" + name,
				"MIXED":           "prefix-${" + name + "}-suffix",
				"${" + name + "}": "key-expansion",
			}}
			env, warnings, missing, _ := buildAgentEnv(cfg, nil, nil, false)
			assertNoReservedValue(t, env, warnings)
			got := envToMap(env)
			if v, ok := got["BRACED"]; ok {
				t.Errorf("BRACED = %q, want omitted", v)
			}
			if v, ok := got["BARE"]; ok {
				t.Errorf("BARE = %q, want omitted", v)
			}
			if got["MIXED"] != "prefix--suffix" {
				t.Errorf("MIXED = %q, want %q", got["MIXED"], "prefix--suffix")
			}
			if len(missing) != 0 {
				t.Errorf("missing = %v, want none", missing)
			}
			assertWarnsName(t, warnings, name)
		})
	}
}

func TestBuildAgentEnv_NonReservedHostEnvStillPasses(t *testing.T) {
	setReservedHostEnv(t)
	t.Setenv("ORDINARY_HOST_VAR", "ordinary-value")
	cfg := &api.ScionConfig{Env: map[string]string{
		"ORDINARY_HOST_VAR": "",
		"EXPANDED":          "x-${ORDINARY_HOST_VAR}",
	}}
	env, warnings, missing, _ := buildAgentEnv(cfg, nil, nil, false)
	got := envToMap(env)
	if got["ORDINARY_HOST_VAR"] != "ordinary-value" {
		t.Errorf("ORDINARY_HOST_VAR = %q, want passthrough", got["ORDINARY_HOST_VAR"])
	}
	if got["EXPANDED"] != "x-ordinary-value" {
		t.Errorf("EXPANDED = %q, want %q", got["EXPANDED"], "x-ordinary-value")
	}
	if len(warnings) != 0 || len(missing) != 0 {
		t.Errorf("warnings = %v, missing = %v; want none", warnings, missing)
	}
}

func TestBuildAgentEnv_ExplicitValuesForReservedNamesAreKept(t *testing.T) {
	// Values set by the caller (opts.Env) or literally in config are not
	// host passthrough and are not refused.
	setReservedHostEnv(t)
	cfg := &api.ScionConfig{Env: map[string]string{"SESSION_SECRET": "literal"}}
	env, _, _, _ := buildAgentEnv(cfg, map[string]string{"SCION_SERVER_X": "caller"}, nil, false)
	got := envToMap(env)
	if got["SESSION_SECRET"] != "literal" || got["SCION_SERVER_X"] != "caller" {
		t.Errorf("explicit values not kept: %v", got)
	}
}

func TestExpandConfigHubEndpoint_RefusesReservedHostEnv(t *testing.T) {
	setReservedHostEnv(t)
	t.Setenv("ORDINARY_HUB_HOST", "hub.example.com")
	for _, name := range reservedHostEnvNames {
		if got := expandConfigHubEndpoint("a", map[string]string{"SCION_HUB_ENDPOINT": "${" + name + "}"}); got != "" {
			t.Errorf("%s: expandConfigHubEndpoint = %q, want empty", name, got)
		}
		if got := expandConfigHubEndpoint("a", map[string]string{"SCION_HUB_ENDPOINT": "https://$" + name}); strings.Contains(got, reservedHostEnvSentinel) {
			t.Errorf("%s: expandConfigHubEndpoint carries a reserved host value", name)
		}
	}
	if got := expandConfigHubEndpoint("a", map[string]string{"SCION_HUB_ENDPOINT": "https://${ORDINARY_HUB_HOST}"}); got != "https://hub.example.com" {
		t.Errorf("expandConfigHubEndpoint = %q, want ordinary expansion", got)
	}
	if got := expandConfigHubEndpoint("a", nil); got != "" {
		t.Errorf("expandConfigHubEndpoint(nil) = %q, want empty", got)
	}
}

func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}
