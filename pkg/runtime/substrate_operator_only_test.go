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

// operatorSubstrateSettingsYAML is the operator's own (global) settings for
// the empty-profile-name tests, in YAML: a "substrate-prod" runtime and the
// "substrate" profile that selects it. active_profile is left to the
// embedded default.
const operatorSubstrateSettingsYAML = `schema_version: "1"
runtimes:
  substrate-prod:
    type: substrate
    substrate:
      api_endpoint: api.operator.example.com:443
      router_endpoint: http://router.operator.example.com:80
profiles:
  substrate:
    runtime: substrate-prod
`

// operatorEmptyKeySubstrateSettingsYAML is operator (global) settings that
// themselves store the substrate profile under the empty profile name and
// make it the default.
const operatorEmptyKeySubstrateSettingsYAML = `schema_version: "1"
active_profile: ""
runtimes:
  substrate-prod:
    type: substrate
    substrate:
      api_endpoint: api.operator.example.com:443
      router_endpoint: http://router.operator.example.com:80
profiles:
  "":
    runtime: substrate-prod
`

// projectEmptyKeySubstrateSettingsYAML is project settings that store a
// profile under the empty profile name, pointing at a substrate runtime the
// project defines itself, and make it the default.
const projectEmptyKeySubstrateSettingsYAML = `schema_version: "1"
active_profile: ""
runtimes:
  project-substrate:
    type: substrate
    substrate:
      api_endpoint: api.project.example.com:443
      router_endpoint: http://router.project.example.com:80
profiles:
  "":
    runtime: project-substrate
`

// projectEmptyKeyDockerSettingsYAML is project settings that store a
// non-substrate profile under the empty profile name and make it the
// default.
const projectEmptyKeyDockerSettingsYAML = `schema_version: "1"
active_profile: ""
profiles:
  "":
    runtime: docker
`

// writeYAMLSettings isolates the settings environment of a test, writes
// operatorSettings as the operator's global settings.yaml (under a HOME in
// a t.TempDir) and projectSettings as a project's .scion/settings.yaml, and
// returns the project's directory. The co-located settings overlay is
// removed and SCION_ACTIVE_PROFILE is blanked (an empty variable is treated
// as unset), so only the two files and the embedded defaults are merged.
func writeYAMLSettings(t *testing.T, operatorSettings, projectSettings string) string {
	t.Helper()
	prevOverlay := config.GetGlobalSettingsOverlay()
	config.SetGlobalSettingsOverlay(nil)
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(prevOverlay) })
	t.Setenv("SCION_ACTIVE_PROFILE", "")

	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(operatorSettings), 0o644); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(projectSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectDir
}

// recordSubstrateBuilds replaces substrateRuntimeBuilder with a fake for the
// test and returns the configs it was asked to build.
func recordSubstrateBuilds(t *testing.T) *[]config.V1SubstrateConfig {
	t.Helper()
	resetSubstrateRuntimeRegistryForTest(t)
	built := &[]config.V1SubstrateConfig{}
	origBuilder := substrateRuntimeBuilder
	substrateRuntimeBuilder = func(cfg config.V1SubstrateConfig) (*SubstrateRuntime, error) {
		*built = append(*built, cfg)
		return NewSubstrateRuntimeForTest(newFakeControlClient(&callRecorder{}), substrate.NewRouterClient("http://unused"), nil, cfg), nil
	}
	t.Cleanup(func() { substrateRuntimeBuilder = origBuilder })
	return built
}

// TestGetRuntime_EmptyProfileNameProjectSubstrateIsInvalidProfile loads
// project settings that store a project-defined substrate profile under the
// empty profile name through the real settings loader, checks that the
// loaded settings select it (active_profile "" overrides the embedded
// default, and the empty profile name survives the load), and then checks
// that GetRuntime with no profile name refuses it without building it.
func TestGetRuntime_EmptyProfileNameProjectSubstrateIsInvalidProfile(t *testing.T) {
	built := recordSubstrateBuilds(t)
	projectDir := writeYAMLSettings(t, operatorSubstrateSettingsYAML, projectEmptyKeySubstrateSettingsYAML)

	globalVS, _, err := config.LoadGlobalSettings()
	if err != nil {
		t.Fatalf("LoadGlobalSettings: %v", err)
	}
	if globalVS.ActiveProfile != "local" {
		t.Fatalf("operator active_profile = %q, want the embedded default %q", globalVS.ActiveProfile, "local")
	}

	resolved, err := config.GetResolvedProjectDir(projectDir)
	if err != nil {
		t.Fatalf("resolve project dir: %v", err)
	}
	vs, _, err := config.LoadEffectiveSettings(resolved)
	if err != nil {
		t.Fatalf("LoadEffectiveSettings: %v", err)
	}
	// The project's active_profile "" replaces the embedded default.
	if vs.ActiveProfile != "" {
		t.Fatalf("effective active_profile = %q, want the project's \"\"", vs.ActiveProfile)
	}
	// The empty profile name is kept by the loader.
	profile, ok := vs.Profiles[""]
	if !ok {
		t.Fatalf("effective profiles have no \"\" entry; profiles = %v", vs.Profiles)
	}
	if profile.Runtime != "project-substrate" {
		t.Fatalf("effective profiles[\"\"].runtime = %q, want %q", profile.Runtime, "project-substrate")
	}
	rtConfig, rtType, err := vs.ResolveRuntime("")
	if err != nil || rtType != "substrate" || rtConfig.Substrate == nil || rtConfig.Substrate.APIEndpoint != "api.project.example.com:443" {
		t.Fatalf("ResolveRuntime(\"\") = type %q, config %+v, err %v; want the project's substrate runtime", rtType, rtConfig.Substrate, err)
	}

	rt := GetRuntime(projectDir, "")
	er, ok := rt.(*ErrorRuntime)
	if !ok {
		t.Fatalf("GetRuntime = %T, want *ErrorRuntime", rt)
	}
	if !errors.Is(er.Err, ErrSubstrateProfileInvalid) {
		t.Errorf("ErrorRuntime.Err = %v, want it to match ErrSubstrateProfileInvalid", er.Err)
	}
	if len(*built) != 0 {
		t.Errorf("substrate runtime built %d time(s) from a refused profile: %+v", len(*built), *built)
	}
}

// TestGetRuntime_DefaultProfileStartupRefusal resolves the default runtime
// the way the runtime broker does at startup (GetRuntime with no profile
// name) for empty-profile-name settings, and checks the condition the
// broker refuses to start on (an *ErrorRuntime matching
// ErrSubstrateProfileInvalid; see refuseErrorRuntimeAtStartup in cmd).
func TestGetRuntime_DefaultProfileStartupRefusal(t *testing.T) {
	rows := []struct {
		name             string
		operatorSettings string
		projectSettings  string
		wantRefused      bool
		// wantBuiltAPIEndpoint is the api_endpoint of the one substrate
		// runtime the row must build, or "" when none may be built.
		wantBuiltAPIEndpoint string
	}{
		{
			name:             "project substrate under empty profile name",
			operatorSettings: operatorSubstrateSettingsYAML,
			projectSettings:  projectEmptyKeySubstrateSettingsYAML,
			wantRefused:      true,
		},
		{
			name:                 "operator substrate under empty profile name",
			operatorSettings:     operatorEmptyKeySubstrateSettingsYAML,
			projectSettings:      "schema_version: \"1\"\n",
			wantBuiltAPIEndpoint: "api.operator.example.com:443",
		},
		{
			name:             "project non-substrate under empty profile name",
			operatorSettings: operatorSubstrateSettingsYAML,
			projectSettings:  projectEmptyKeyDockerSettingsYAML,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			built := recordSubstrateBuilds(t)
			projectDir := writeYAMLSettings(t, row.operatorSettings, row.projectSettings)

			rt := GetRuntime(projectDir, "")
			er, isErr := rt.(*ErrorRuntime)
			refused := isErr && errors.Is(er.Err, ErrSubstrateProfileInvalid)
			if refused != row.wantRefused {
				t.Fatalf("GetRuntime = %T (%v); refused at startup = %v, want %v", rt, rt, refused, row.wantRefused)
			}
			if row.wantBuiltAPIEndpoint == "" {
				if len(*built) != 0 {
					t.Errorf("substrate runtime built %d time(s): %+v, want none", len(*built), *built)
				}
				return
			}
			if _, ok := rt.(*SubstrateRuntime); !ok {
				t.Fatalf("GetRuntime = %T (%v), want *SubstrateRuntime", rt, rt)
			}
			if len(*built) != 1 || (*built)[0].APIEndpoint != row.wantBuiltAPIEndpoint {
				t.Errorf("built configs = %+v, want exactly one with api_endpoint %q", *built, row.wantBuiltAPIEndpoint)
			}
		})
	}
}

// TestValidateOperatorOnlySubstrateProfile_EmptyProfileName calls the
// validator directly with an empty profile name on settings loaded by the
// real loader: a project-defined substrate profile under the empty name is
// refused, while the operator's own one and a non-substrate one are
// allowed.
func TestValidateOperatorOnlySubstrateProfile_EmptyProfileName(t *testing.T) {
	rows := []struct {
		name             string
		operatorSettings string
		projectSettings  string
		wantRefused      bool
	}{
		{"project substrate", operatorSubstrateSettingsYAML, projectEmptyKeySubstrateSettingsYAML, true},
		{"operator substrate", operatorEmptyKeySubstrateSettingsYAML, "schema_version: \"1\"\n", false},
		{"project non-substrate", operatorSubstrateSettingsYAML, projectEmptyKeyDockerSettingsYAML, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			projectDir := writeYAMLSettings(t, row.operatorSettings, row.projectSettings)
			resolved, err := config.GetResolvedProjectDir(projectDir)
			if err != nil {
				t.Fatalf("resolve project dir: %v", err)
			}
			vs, _, err := config.LoadEffectiveSettings(resolved)
			if err != nil {
				t.Fatalf("LoadEffectiveSettings: %v", err)
			}
			err = ValidateOperatorOnlySubstrateProfile(vs, "")
			if row.wantRefused {
				if err == nil {
					t.Fatal("ValidateOperatorOnlySubstrateProfile = nil, want a refusal")
				}
				if !strings.Contains(err.Error(), `"project-substrate"`) || !strings.Contains(err.Error(), "operator") {
					t.Errorf("error = %q, want it to name runtime \"project-substrate\" and the operator-only requirement", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateOperatorOnlySubstrateProfile = %v, want nil", err)
			}
		})
	}
}

// TestGetRuntime_UnresolvedSubstrateProfileNameIsInvalidProfile pins the
// legacy fallback that treats an unresolvable profile name "substrate" as
// the runtime type: the validator has nothing to check (no such profile),
// and NewSubstrateRuntime then refuses the empty runtime config, so
// GetRuntime still ends in ErrSubstrateProfileInvalid and builds nothing.
func TestGetRuntime_UnresolvedSubstrateProfileNameIsInvalidProfile(t *testing.T) {
	built := recordSubstrateBuilds(t)
	projectDir := writeYAMLSettings(t, "schema_version: \"1\"\n", "schema_version: \"1\"\n")

	resolved, err := config.GetResolvedProjectDir(projectDir)
	if err != nil {
		t.Fatalf("resolve project dir: %v", err)
	}
	vs, _, err := config.LoadEffectiveSettings(resolved)
	if err != nil {
		t.Fatalf("LoadEffectiveSettings: %v", err)
	}
	if _, ok := vs.Profiles["substrate"]; ok {
		t.Fatalf("effective settings define a \"substrate\" profile; want none for the fallback")
	}
	if err := ValidateOperatorOnlySubstrateProfile(vs, "substrate"); err != nil {
		t.Fatalf("ValidateOperatorOnlySubstrateProfile = %v, want nil for an unresolvable profile", err)
	}

	rt := GetRuntime(projectDir, "substrate")
	er, ok := rt.(*ErrorRuntime)
	if !ok {
		t.Fatalf("GetRuntime = %T, want *ErrorRuntime", rt)
	}
	if !errors.Is(er.Err, ErrSubstrateProfileInvalid) {
		t.Errorf("ErrorRuntime.Err = %v, want it to match ErrSubstrateProfileInvalid", er.Err)
	}
	if len(*built) != 0 {
		t.Errorf("substrate runtime built %d time(s): %+v, want none", len(*built), *built)
	}
}

// TestGetRuntime_EmptyProfileNameComparesSameOperatorProfile checks that,
// for an empty effective profile name, the operator-side comparison uses
// the operator's profile stored under that same empty name, not the
// operator's own active_profile.
func TestGetRuntime_EmptyProfileNameComparesSameOperatorProfile(t *testing.T) {
	rows := []struct {
		name             string
		operatorSettings string
		projectSettings  string
		wantRefused      bool
	}{
		{
			// The operator stores the substrate profile under the empty
			// name but keeps the default active_profile ("local"); the
			// project only selects the empty name.
			name: "operator empty-name substrate selected by project",
			operatorSettings: `schema_version: "1"
runtimes:
  substrate-prod:
    type: substrate
    substrate:
      api_endpoint: api.operator.example.com:443
      router_endpoint: http://router.operator.example.com:80
profiles:
  "":
    runtime: substrate-prod
`,
			projectSettings: `schema_version: "1"
active_profile: ""
`,
			wantRefused: false,
		},
		{
			// The operator's active_profile is its named substrate profile
			// and it stores nothing under the empty name; the project
			// stores its own copy of the operator's runtime under the empty
			// name.
			name:             "project empty-name copy of operator default substrate",
			operatorSettings: operatorSubstrateSettingsYAML + "active_profile: substrate\n",
			projectSettings: `schema_version: "1"
active_profile: ""
runtimes:
  project-copy:
    type: substrate
    substrate:
      api_endpoint: api.operator.example.com:443
      router_endpoint: http://router.operator.example.com:80
profiles:
  "":
    runtime: project-copy
`,
			wantRefused: true,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			built := recordSubstrateBuilds(t)
			projectDir := writeYAMLSettings(t, row.operatorSettings, row.projectSettings)

			rt := GetRuntime(projectDir, "")
			if row.wantRefused {
				er, ok := rt.(*ErrorRuntime)
				if !ok {
					t.Fatalf("GetRuntime = %T, want *ErrorRuntime", rt)
				}
				if !errors.Is(er.Err, ErrSubstrateProfileInvalid) {
					t.Errorf("ErrorRuntime.Err = %v, want it to match ErrSubstrateProfileInvalid", er.Err)
				}
				if len(*built) != 0 {
					t.Errorf("substrate runtime built %d time(s) from a refused profile: %+v", len(*built), *built)
				}
				return
			}
			if _, ok := rt.(*SubstrateRuntime); !ok {
				t.Fatalf("GetRuntime = %T (%v), want *SubstrateRuntime", rt, rt)
			}
			if len(*built) != 1 || (*built)[0].APIEndpoint != "api.operator.example.com:443" {
				t.Errorf("built configs = %+v, want exactly one, from the operator's definition", *built)
			}
		})
	}
}
