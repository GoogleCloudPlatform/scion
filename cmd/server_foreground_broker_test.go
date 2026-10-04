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

package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
)

func TestRequireImageRegistryForBroker_NoRegistry(t *testing.T) {
	// Ensure no env vars provide a registry.
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")

	// Point HOME at an empty temp dir so settings.yaml is absent.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Chdir(tmpHome)

	err := requireImageRegistryForBroker()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "image_registry is not configured")
	assert.Contains(t, err.Error(), "SCION_IMAGE_REGISTRY")
}

func TestRequireImageRegistryForBroker_EnvVar(t *testing.T) {
	t.Setenv("SCION_IMAGE_REGISTRY", "ghcr.io/test")

	err := requireImageRegistryForBroker()
	assert.NoError(t, err)
}

func TestRequireImageRegistryForBroker_MaintenanceEnvVar(t *testing.T) {
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "ghcr.io/test-maintenance")

	// Point HOME at an empty temp dir so settings.yaml is absent.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	err := requireImageRegistryForBroker()
	assert.NoError(t, err)
}

func TestRequireImageRegistryForBroker_Settings(t *testing.T) {
	// Use t.Setenv to register cleanup (restores original values when test
	// ends), then unset the env vars so the koanf env loader does not
	// override the settings file value with an empty string.
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")
	if err := os.Unsetenv("SCION_IMAGE_REGISTRY"); err != nil {
		t.Fatalf("failed to unsetenv SCION_IMAGE_REGISTRY: %v", err)
	}
	if err := os.Unsetenv("SCION_MAINTENANCE_IMAGE_REGISTRY"); err != nil {
		t.Fatalf("failed to unsetenv SCION_MAINTENANCE_IMAGE_REGISTRY: %v", err)
	}

	// Create a temp home with settings.yaml containing image_registry.
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	scionDir := filepath.Join(tmpHome, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create test .scion dir: %v", err)
	}
	settingsContent := "schema_version: \"1\"\nimage_registry: ghcr.io/from-settings\n"
	if err := os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte(settingsContent), 0644); err != nil {
		t.Fatalf("failed to write test settings: %v", err)
	}

	// Change to the temp home so LoadEffectiveSettings doesn't find
	// the workspace project root's .scion directory.
	t.Chdir(tmpHome)

	err := requireImageRegistryForBroker()
	assert.NoError(t, err)
}

func TestCloudRunLogicalBrokerIDIsDeterministic(t *testing.T) {
	settings := &config.VersionedSettings{
		ActiveProfile: "default",
		Profiles: map[string]config.V1ProfileConfig{
			"default": {Runtime: "cloudrun"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"cloudrun": {
				Type: "cloudrun",
				CloudRun: &config.CloudRunConfig{
					ProjectID: "test-project",
					Location:  "us-central1",
				},
			},
		},
	}
	rt := &scionruntime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	id1, err1 := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err1)
	id2, err2 := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err2)

	assert.NotEmpty(t, id1)
	assert.Equal(t, id1, id2)
}

func TestCloudRunLogicalBrokerIDWorksForCloudRunInstances(t *testing.T) {
	settings := &config.VersionedSettings{
		ActiveProfile: "default",
		Profiles: map[string]config.V1ProfileConfig{
			"default": {Runtime: "cr"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			"cr": {
				Type: "cloudrun-instances",
				CloudRunInstances: &config.V1CloudRunInstancesConfig{
					ProjectID: "instances-project",
					Region:    "us-central1",
				},
			},
		},
	}
	rt := &scionruntime.MockRuntime{NameFunc: func() string { return "cloudrun" }}

	id, err := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err)
	assert.NotEmpty(t, id)

	// Verify deterministic: same config → same ID
	id2, err2 := deriveCloudRunLogicalBrokerID(settings, rt)
	assert.NoError(t, err2)
	assert.Equal(t, id, id2)
}

func TestResolveBrokerIDPrefersConfiguredIDOverDefault(t *testing.T) {
	cfg := &config.GlobalConfig{}
	settings := &config.Settings{
		Hub: &config.HubClientConfig{BrokerID: "configured-broker"},
	}

	got := resolveBrokerID(context.Background(), cfg, settings, nil, t.TempDir(), "cloudrun-default", nil)

	assert.Equal(t, "configured-broker", got)
}

// writeBrokerRuntimeSettings isolates HOME (no operator settings at all)
// and writes projectSettings as a project's .scion/settings.json, returning
// the project directory for runtime.GetRuntime.
func writeBrokerRuntimeSettings(t *testing.T, projectSettings string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	projectDir := t.TempDir()
	scionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scionDir, "settings.json"), []byte(projectSettings), 0644); err != nil {
		t.Fatal(err)
	}
	return projectDir
}

// getRuntimeFor returns a getRuntime function for resolveBrokerDefaultRuntime
// that resolves the default runtime against projectDir's settings.
func getRuntimeFor(projectDir string) func(string, string) scionruntime.Runtime {
	return func(string, string) scionruntime.Runtime {
		return scionruntime.GetRuntime(projectDir, "")
	}
}

// TestResolveBrokerDefaultRuntime_InvalidSubstrateProfileRefusesStart: a
// default runtime that resolves to a substrate profile the operator never
// defined comes back from GetRuntime as an *ErrorRuntime matching
// ErrSubstrateProfileInvalid, and the broker must refuse to start rather
// than come up unable to serve a single Run/Exec/List.
func TestResolveBrokerDefaultRuntime_InvalidSubstrateProfileRefusesStart(t *testing.T) {
	projectDir := writeBrokerRuntimeSettings(t, `{
		"schema_version": "1",
		"active_profile": "substrate",
		"runtimes": {
			"substrate-prod": {
				"type": "substrate",
				"substrate": {
					"api_endpoint": "attacker.net:443",
					"router_endpoint": "http://attacker.net:80"
				}
			}
		},
		"profiles": {"substrate": {"runtime": "substrate-prod"}}
	}`)

	var logged []string
	logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }

	rt, err := resolveBrokerDefaultRuntime(getRuntimeFor(projectDir), logf)
	if err == nil {
		t.Fatalf("expected startup to be refused, got runtime %q", rt.Name())
	}
	assert.ErrorIs(t, err, scionruntime.ErrSubstrateProfileInvalid)
	assert.Contains(t, err.Error(), "substrate-prod")
	assert.Nil(t, rt)
	assert.Empty(t, logged, "a refused startup must not log a runtime as in use")
}

// TestResolveBrokerDefaultRuntime_NonSubstrateErrorRuntimeStartsDegraded is
// the positive control for the substrate-only scope of the refusal: a
// Kubernetes default runtime whose client cannot be constructed is also an
// *ErrorRuntime, but it must not block startup. The broker starts degraded
// and logs the "error" runtime name, as it always has.
func TestResolveBrokerDefaultRuntime_NonSubstrateErrorRuntimeStartsDegraded(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing-kubeconfig"))
	projectDir := writeBrokerRuntimeSettings(t, `{
		"schema_version": "1",
		"active_profile": "k8s",
		"runtimes": {
			"k8s": {"type": "kubernetes", "context": "no-such-context"}
		},
		"profiles": {"k8s": {"runtime": "k8s"}}
	}`)

	var logged []string
	logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }

	rt, err := resolveBrokerDefaultRuntime(getRuntimeFor(projectDir), logf)
	if err != nil {
		t.Fatalf("a non-substrate runtime failure must not refuse startup, got: %v", err)
	}
	er, ok := rt.(*scionruntime.ErrorRuntime)
	if !ok {
		t.Fatalf("test setup: expected an *ErrorRuntime for an unloadable kubeconfig, got %T", rt)
	}
	assert.NotErrorIs(t, er.Err, scionruntime.ErrSubstrateProfileInvalid)
	assert.Equal(t, []string{"Runtime broker using runtime: error"}, logged)
}

// writeOperatorSubstrateSettings isolates HOME and writes operatorSettings as
// the operator-level ~/.scion/settings.json (so a substrate runtime defined
// there passes ValidateOperatorOnlySubstrateProfile), and runs the test from
// an empty working directory so no project settings are picked up. The
// returned getRuntime resolves the default runtime from operator settings
// alone, the way a deployed broker does.
func writeOperatorSubstrateSettings(t *testing.T, operatorSettings string) func(string, string) scionruntime.Runtime {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "settings.json"), []byte(operatorSettings), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	return func(string, string) scionruntime.Runtime {
		return scionruntime.GetRuntime("", "")
	}
}

// operatorSubstrateSettings returns operator settings whose default profile
// is an operator-defined substrate runtime with both required endpoints and
// the given extra substrate fields (a JSON fragment, may be empty).
func operatorSubstrateSettings(extraSubstrateFields string) string {
	return operatorSubstrateSettingsWithBlock(`
					"api_endpoint": "api.ate-system.svc:443",
					"router_endpoint": "http://atenet-router.ate-system.svc:80"` + extraSubstrateFields)
}

// operatorSubstrateSettingsWithBlock returns operator settings whose default
// profile is an operator-defined substrate runtime whose substrate block body
// is exactly substrateFields (a JSON object body without braces), so a test
// can omit a field operatorSubstrateSettings always supplies.
func operatorSubstrateSettingsWithBlock(substrateFields string) string {
	return `{
		"schema_version": "1",
		"active_profile": "substrate",
		"runtimes": {
			"substrate-prod": {
				"type": "substrate",
				"substrate": {` + substrateFields + `
				}
			}
		},
		"profiles": {"substrate": {"runtime": "substrate-prod"}}
	}`
}

// TestResolveBrokerDefaultRuntime_SubstrateConfigValidationFailureRefusesStart:
// an operator-defined substrate default runtime that fails NewSubstrateRuntime's
// deterministic config validation is a settings problem that no retry can
// fix, so the broker must still refuse to start. Each row exercises one
// tagging site: a missing required api_endpoint, a missing required
// router_endpoint, and substrate.Validate rejecting an unsupported
// egress_trust_bundle. The builder is stubbed to prove validation alone
// decides this: nothing is ever built or dialed.
func TestResolveBrokerDefaultRuntime_SubstrateConfigValidationFailureRefusesStart(t *testing.T) {
	tests := []struct {
		name         string
		settings     string
		wantInErrMsg string
	}{
		{
			name: "missing api_endpoint",
			settings: operatorSubstrateSettingsWithBlock(`
					"router_endpoint": "http://atenet-router.ate-system.svc:80"`),
			wantInErrMsg: "api_endpoint is required",
		},
		{
			name: "missing router_endpoint",
			settings: operatorSubstrateSettingsWithBlock(`
					"api_endpoint": "api.ate-system.svc:443"`),
			wantInErrMsg: "router_endpoint is required",
		},
		{
			name: "substrate.Validate rejects egress_trust_bundle",
			settings: operatorSubstrateSettings(`,
					"egress_trust_bundle": "not-a-supported-bundle.example.com"`),
			wantInErrMsg: "egress_trust_bundle",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			built := 0
			restore := scionruntime.SetSubstrateRuntimeBuilderForTest(func(config.V1SubstrateConfig) (*scionruntime.SubstrateRuntime, error) {
				built++
				return nil, fmt.Errorf("builder must not be reached for an invalid config")
			})
			t.Cleanup(restore)

			getRuntime := writeOperatorSubstrateSettings(t, tt.settings)

			var logged []string
			logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }

			rt, err := resolveBrokerDefaultRuntime(getRuntime, logf)
			if err == nil {
				t.Fatalf("expected startup to be refused for a substrate config validation failure, got runtime %q", rt.Name())
			}
			assert.ErrorIs(t, err, scionruntime.ErrSubstrateProfileInvalid)
			assert.Contains(t, err.Error(), tt.wantInErrMsg)
			assert.Nil(t, rt)
			assert.Empty(t, logged, "a refused startup must not log a runtime as in use")
			assert.Zero(t, built, "config validation must fail before the runtime is built")
		})
	}
}

// TestResolveBrokerDefaultRuntime_SubstrateBuildFailureStartsDegraded: a
// valid operator-defined substrate default runtime whose construct-time
// dependencies fail (stubbed substrateRuntimeBuilder, standing in for the
// Kubernetes client build or substrate.Dial failing on an API-server blip at
// boot) must NOT refuse startup — refusing would turn a transient outage into
// a crash loop. The broker starts degraded on an *ErrorRuntime that does not
// match ErrSubstrateProfileInvalid and logs the "error" runtime name.
func TestResolveBrokerDefaultRuntime_SubstrateBuildFailureStartsDegraded(t *testing.T) {
	buildErr := fmt.Errorf("substrate: build Kubernetes client: simulated API server unavailable")
	built := 0
	restore := scionruntime.SetSubstrateRuntimeBuilderForTest(func(config.V1SubstrateConfig) (*scionruntime.SubstrateRuntime, error) {
		built++
		return nil, buildErr
	})
	t.Cleanup(restore)

	getRuntime := writeOperatorSubstrateSettings(t, operatorSubstrateSettings(""))

	var logged []string
	logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }

	rt, err := resolveBrokerDefaultRuntime(getRuntime, logf)
	if err != nil {
		t.Fatalf("a substrate construct-time dependency failure must not refuse startup, got: %v", err)
	}
	assert.Equal(t, 1, built, "test setup: the stubbed builder should have been called once")
	er, ok := rt.(*scionruntime.ErrorRuntime)
	if !ok {
		t.Fatalf("expected a degraded *ErrorRuntime, got %T", rt)
	}
	assert.ErrorIs(t, er.Err, buildErr)
	assert.NotErrorIs(t, er.Err, scionruntime.ErrSubstrateProfileInvalid)
	assert.Equal(t, []string{"Runtime broker using runtime: error"}, logged)
}

// TestResolveBrokerDefaultRuntime_HealthyRuntimeStartsNormally: an ordinary
// runtime is returned as-is and logged.
func TestResolveBrokerDefaultRuntime_HealthyRuntimeStartsNormally(t *testing.T) {
	healthy := scionruntime.NewDockerRuntime()
	var logged []string
	logf := func(format string, args ...interface{}) { logged = append(logged, fmt.Sprintf(format, args...)) }

	rt, err := resolveBrokerDefaultRuntime(func(string, string) scionruntime.Runtime { return healthy }, logf)
	assert.NoError(t, err)
	assert.Same(t, healthy, rt)
	assert.Equal(t, []string{"Runtime broker using runtime: " + healthy.Name()}, logged)
}

// TestRefuseErrorRuntimeAtStartup_OnlySubstrateFailuresRefuse pins the
// helper's scope directly: only an *ErrorRuntime whose error matches
// ErrSubstrateProfileInvalid is refused, and the refusal keeps the
// underlying error in its chain.
func TestRefuseErrorRuntimeAtStartup_OnlySubstrateFailuresRefuse(t *testing.T) {
	substrateErr := fmt.Errorf("wrapped: %w", scionruntime.ErrSubstrateProfileInvalid)
	err := refuseErrorRuntimeAtStartup(&scionruntime.ErrorRuntime{Err: substrateErr})
	assert.ErrorIs(t, err, scionruntime.ErrSubstrateProfileInvalid)

	assert.NoError(t, refuseErrorRuntimeAtStartup(&scionruntime.ErrorRuntime{Err: assert.AnError}))
	assert.NoError(t, refuseErrorRuntimeAtStartup(scionruntime.NewDockerRuntime()))
}
