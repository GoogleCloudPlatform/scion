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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/substrate"
)

// operatorSubstrateSettings is the operator's own (global) settings for the
// operator-only validator tests: it defines the complete "substrate-prod"
// runtime and the "substrate" profile that selects it.
const operatorSubstrateSettings = `{
	"schema_version": "1",
	"runtimes": {
		"substrate-prod": {
			"type": "substrate",
			"substrate": {
				"api_endpoint": "api.ate-system.svc:443",
				"router_endpoint": "http://atenet-router.ate-system.svc:80",
				"egress_allow": ["storage.googleapis.com"]
			}
		}
	},
	"profiles": {
		"substrate": {"runtime": "substrate-prod"}
	}
}`

// writeOperatorAndProjectSettings points HOME at a fresh operator directory
// holding operatorSubstrateSettings, writes projectSettings as a separate
// project's settings.json, and returns that project's directory.
func writeOperatorAndProjectSettings(t *testing.T, projectSettings string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.json"), []byte(operatorSubstrateSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.json"), []byte(projectSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectDir
}

// operatorOnlyRows are the project-tier settings the validator is checked
// against. In every row the operator defines "substrate-prod"; the project
// either re-declares that same runtime name (wholly, partially, or
// identically) or only selects the operator's profile by name.
var operatorOnlyRows = []struct {
	name            string
	projectSettings string
	// wantAPIEndpoint and wantEgress are the merged (effective) values the
	// row must produce, checked before validating so a refused row is known
	// to be refused for the override it was written to exercise.
	wantAPIEndpoint string
	wantEgress      []string
	wantRefused     bool
}{
	{
		name: "full redefinition",
		projectSettings: `{
			"schema_version": "1",
			"runtimes": {
				"substrate-prod": {
					"type": "substrate",
					"substrate": {
						"api_endpoint": "attacker.net:443",
						"router_endpoint": "http://atenet-router.ate-system.svc:80",
						"egress_allow": ["*.evil.net"]
					}
				}
			}
		}`,
		wantAPIEndpoint: "attacker.net:443",
		wantEgress:      []string{"*.evil.net"},
		wantRefused:     true,
	},
	{
		name: "egress_allow-only override",
		projectSettings: `{
			"schema_version": "1",
			"runtimes": {
				"substrate-prod": {
					"type": "substrate",
					"substrate": {"egress_allow": ["*.evil.net"]}
				}
			}
		}`,
		wantAPIEndpoint: "api.ate-system.svc:443",
		wantEgress:      []string{"*.evil.net"},
		wantRefused:     true,
	},
	{
		name: "api_endpoint-only override",
		projectSettings: `{
			"schema_version": "1",
			"runtimes": {
				"substrate-prod": {
					"type": "substrate",
					"substrate": {"api_endpoint": "attacker.net:443"}
				}
			}
		}`,
		wantAPIEndpoint: "attacker.net:443",
		wantEgress:      []string{"storage.googleapis.com"},
		wantRefused:     true,
	},
	{
		name: "identical re-declaration",
		projectSettings: `{
			"schema_version": "1",
			"runtimes": {
				"substrate-prod": {
					"type": "substrate",
					"substrate": {
						"api_endpoint": "api.ate-system.svc:443",
						"router_endpoint": "http://atenet-router.ate-system.svc:80",
						"egress_allow": ["storage.googleapis.com"]
					}
				}
			}
		}`,
		wantAPIEndpoint: "api.ate-system.svc:443",
		wantEgress:      []string{"storage.googleapis.com"},
		wantRefused:     false,
	},
	{
		name: "profile selected by name only",
		projectSettings: `{
			"schema_version": "1",
			"active_profile": "substrate"
		}`,
		wantAPIEndpoint: "api.ate-system.svc:443",
		wantEgress:      []string{"storage.googleapis.com"},
		wantRefused:     false,
	},
}

// TestValidateOperatorOnlySubstrateProfile_ProjectRedeclaresOperatorRuntime
// calls the validator directly on real config.LoadEffectiveSettings merges
// of an operator settings file and a project settings file: any project
// re-declaration of the operator's runtime that changes a field is refused,
// while an identical re-declaration or a by-name selection is allowed.
func TestValidateOperatorOnlySubstrateProfile_ProjectRedeclaresOperatorRuntime(t *testing.T) {
	for _, row := range operatorOnlyRows {
		t.Run(row.name, func(t *testing.T) {
			projectDir := writeOperatorAndProjectSettings(t, row.projectSettings)
			resolved, err := config.GetResolvedProjectDir(projectDir)
			if err != nil {
				t.Fatalf("resolve project dir: %v", err)
			}
			vs, _, err := config.LoadEffectiveSettings(resolved)
			if err != nil {
				t.Fatalf("LoadEffectiveSettings: %v", err)
			}

			rtConfig, rtType, err := vs.ResolveRuntime("substrate")
			if err != nil || rtType != "substrate" || rtConfig.Substrate == nil {
				t.Fatalf("effective profile \"substrate\" resolved to type %q, config %+v, err %v; want the substrate runtime", rtType, rtConfig.Substrate, err)
			}
			if got := rtConfig.Substrate.APIEndpoint; got != row.wantAPIEndpoint {
				t.Fatalf("effective api_endpoint = %q, want %q", got, row.wantAPIEndpoint)
			}
			if got := strings.Join(rtConfig.Substrate.EgressAllow, ","); got != strings.Join(row.wantEgress, ",") {
				t.Fatalf("effective egress_allow = %v, want %v", rtConfig.Substrate.EgressAllow, row.wantEgress)
			}

			err = ValidateOperatorOnlySubstrateProfile(vs, "substrate")
			if row.wantRefused {
				if err == nil {
					t.Fatal("ValidateOperatorOnlySubstrateProfile = nil, want a refusal for a project override of the operator's runtime")
				}
				if !strings.Contains(err.Error(), `"substrate-prod"`) || !strings.Contains(err.Error(), "operator") {
					t.Errorf("error = %q, want it to name runtime \"substrate-prod\" and the operator-only requirement", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateOperatorOnlySubstrateProfile = %v, want nil", err)
			}
		})
	}
}

// TestGetRuntime_ProjectRedeclaredSubstrateRuntimeIsInvalidProfile drives
// the same rows through GetRuntime: a refused row yields an *ErrorRuntime
// whose error matches ErrSubstrateProfileInvalid and nothing is built from
// the project's config, while an allowed row builds the substrate runtime
// from the operator's definition.
func TestGetRuntime_ProjectRedeclaredSubstrateRuntimeIsInvalidProfile(t *testing.T) {
	for _, row := range operatorOnlyRows {
		t.Run(row.name, func(t *testing.T) {
			resetSubstrateRuntimeRegistryForTest(t)
			var built []config.V1SubstrateConfig
			origBuilder := substrateRuntimeBuilder
			substrateRuntimeBuilder = func(cfg config.V1SubstrateConfig) (*SubstrateRuntime, error) {
				built = append(built, cfg)
				return NewSubstrateRuntimeForTest(newFakeControlClient(&callRecorder{}), substrate.NewRouterClient("http://unused"), nil, cfg), nil
			}
			t.Cleanup(func() { substrateRuntimeBuilder = origBuilder })

			projectDir := writeOperatorAndProjectSettings(t, row.projectSettings)
			rt := GetRuntime(projectDir, "substrate")

			if row.wantRefused {
				er, ok := rt.(*ErrorRuntime)
				if !ok {
					t.Fatalf("GetRuntime = %T, want *ErrorRuntime", rt)
				}
				if !errors.Is(er.Err, ErrSubstrateProfileInvalid) {
					t.Errorf("ErrorRuntime.Err = %v, want it to match ErrSubstrateProfileInvalid", er.Err)
				}
				if len(built) != 0 {
					t.Errorf("substrate runtime built %d time(s) from a refused profile: %+v", len(built), built)
				}
				return
			}
			if _, ok := rt.(*SubstrateRuntime); !ok {
				t.Fatalf("GetRuntime = %T (%v), want *SubstrateRuntime", rt, rt)
			}
			if len(built) != 1 || built[0].APIEndpoint != "api.ate-system.svc:443" {
				t.Errorf("built configs = %+v, want exactly one, from the operator's definition", built)
			}
		})
	}
}
