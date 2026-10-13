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

package harness

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func TestGatherAuthWithEnv_ReservedHostEnvIsNotGathered(t *testing.T) {
	reserved := []string{"SCION_SERVER_SESSION_SECRET", "SESSION_SECRET", "SCION_SERVER_HUB_CONDUIT_PEERSECRET"}
	for _, name := range reserved {
		t.Setenv(name, "reserved-host-value")
	}
	t.Setenv("ORDINARY_API_KEY", "ordinary-value")

	groups := []config.HarnessAuthEnvRequirement{{AnyOf: []string{"ORDINARY_API_KEY"}}}
	for _, name := range reserved {
		groups = append(groups, config.HarnessAuthEnvRequirement{AnyOf: []string{name}})
	}
	meta := &config.HarnessAuthMetadata{
		DefaultType: "api-key",
		Types:       map[string]config.HarnessAuthTypeMetadata{"api-key": {RequiredEnv: groups}},
	}

	auth := GatherAuthWithEnv(nil, true, meta)
	for _, name := range reserved {
		if v, ok := auth.EnvVars[name]; ok {
			t.Errorf("%s gathered from the host env (len %d)", name, len(v))
		}
	}
	if auth.EnvVars["ORDINARY_API_KEY"] != "ordinary-value" {
		t.Errorf("ORDINARY_API_KEY = %q, want it gathered from the host env", auth.EnvVars["ORDINARY_API_KEY"])
	}

	// An explicit overlay value is not host env and is kept.
	auth = GatherAuthWithEnv(map[string]string{"SESSION_SECRET": "overlay"}, true, meta)
	if auth.EnvVars["SESSION_SECRET"] != "overlay" {
		t.Errorf("overlay SESSION_SECRET = %q, want %q", auth.EnvVars["SESSION_SECRET"], "overlay")
	}
}
