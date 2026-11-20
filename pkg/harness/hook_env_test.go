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
	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
)

func TestHarnessGetEnv_EnforcesScionRefusalAndLegacyHintRewrite(t *testing.T) {
	entry := config.HarnessConfigEntry{
		Harness: "custom",
		EnvTemplate: map[string]string{
			"ORDINARY":                          "home={{ .AgentHome }}",
			"OPENSSL_CONF":                      "/workspace/o.cnf",
			"PATH=/opt/custom":                  ":/usr/bin:/bin",
			"BAD,KEY":                           "comma",
			" SPACED ":                          "spaces",
			"NUL\x00KEY":                        "nul",
			"SCION_HOOK_WITHHELD_ENV":           "FORGED",
			"SCION_EXTRA_PATH":                  "/evil",
			"SCION_AGENT_NAME":                  "spoofed-name",
			"SCION_LIVENESS_HARNESS_ROOT_DEPTH": "2",
			"SCION_AUTO_EXPOSE_MODE":            "denylist",
			envpolicy.HintAutoExposeMode:        "allowlist",
		},
	}

	cases := []struct {
		name string
		env  map[string]string
	}{
		{
			name: "DeclarativeGenericHarness",
			env:  (&DeclarativeGenericHarness{entry: entry}).GetEnv("real-agent", "/home/scion", "scion"),
		},
		{
			name: "ContainerScriptHarness",
			env:  (&ContainerScriptHarness{entry: entry}).GetEnv("real-agent", "/home/scion", "scion"),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env["SCION_AGENT_NAME"] != "real-agent" {
				t.Errorf("SCION_AGENT_NAME = %q, want authoritative %q", tc.env["SCION_AGENT_NAME"], "real-agent")
			}
			if tc.env["ORDINARY"] != "home=/home/scion" {
				t.Errorf("ORDINARY = %q, want %q", tc.env["ORDINARY"], "home=/home/scion")
			}
			if tc.env["OPENSSL_CONF"] != "/workspace/o.cnf" {
				t.Errorf("OPENSSL_CONF = %q, want %q", tc.env["OPENSSL_CONF"], "/workspace/o.cnf")
			}
			if tc.env[envpolicy.HintLivenessRootDepth] != "2" {
				t.Errorf("%s = %q, want %q", envpolicy.HintLivenessRootDepth, tc.env[envpolicy.HintLivenessRootDepth], "2")
			}
			if tc.env[envpolicy.HintAutoExposeMode] != "allowlist" {
				t.Errorf("%s = %q, want explicit %q preserved", envpolicy.HintAutoExposeMode, tc.env[envpolicy.HintAutoExposeMode], "allowlist")
			}
			for _, refused := range []string{
				"PATH=/opt/custom",
				"PATH",
				"BAD,KEY",
				" SPACED ",
				"NUL\x00KEY",
				"SCION_HOOK_WITHHELD_ENV",
				"SCION_EXTRA_PATH",
				"SCION_LIVENESS_HARNESS_ROOT_DEPTH",
				"SCION_AUTO_EXPOSE_MODE",
			} {
				if v, ok := tc.env[refused]; ok {
					t.Errorf("GetEnv() unexpectedly contains %s=%q", refused, v)
				}
			}
		})
	}
}
