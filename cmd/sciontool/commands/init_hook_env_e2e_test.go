//go:build unix

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

package commands

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/envpolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/initenv"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/rootexec"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/supervisor"
)

// TestHookEnvVerticalSlice_BundleKeyReachesChildEnvNotPreStartHook exercises
// the container-side half of the docker vertical slice end to end:
//   - initenv.Load() reads and unsets SCION_HOOK_WITHHELD_ENV;
//   - a pre-start hook script runs via LifecycleManager without the bundle key
//     or static denylist keys (and with PATH reset when withheld), while
//     unrelated operator keys survive;
//   - supervisor.ChildEnv still receives the bundle key and operator key,
//     without the unset marker.
func TestHookEnvVerticalSlice_BundleKeyReachesChildEnvNotPreStartHook(t *testing.T) {
	t.Setenv(envpolicy.HookWithheldEnvVar, envpolicy.FormatWithheldMarker([]string{"BUNDLE_ONLY_KEY", "PATH"}))
	t.Setenv("BUNDLE_ONLY_KEY", "bundle-secret-value")
	t.Setenv("OPERATOR_KEY", "operator-value")
	t.Setenv("LD_PRELOAD", "/tmp/from-bundle.so")
	t.Setenv("PATH", "/custom/bundle/bin")

	pol := initenv.Load()

	if _, present := os.LookupEnv(envpolicy.HookWithheldEnvVar); present {
		t.Fatalf("%s must be unset after initenv.Load()", envpolicy.HookWithheldEnvVar)
	}

	hooksDir := t.TempDir()
	agentHome := t.TempDir()
	outFile := filepath.Join(hooksDir, "hook-env.out")
	preStartDir := filepath.Join(hooksDir, "pre-start.d")
	if err := os.MkdirAll(preStartDir, 0o755); err != nil {
		t.Fatalf("mkdir pre-start.d: %v", err)
	}
	scriptPath := filepath.Join(preStartDir, "10-probe")
	script := "#!/bin/sh\n" +
		"printf 'BUNDLE_ONLY_KEY=%s\\n' \"${BUNDLE_ONLY_KEY-UNSET}\" > " + outFile + "\n" +
		"printf 'OPERATOR_KEY=%s\\n' \"${OPERATOR_KEY-UNSET}\" >> " + outFile + "\n" +
		"printf 'LD_PRELOAD=%s\\n' \"${LD_PRELOAD-UNSET}\" >> " + outFile + "\n" +
		"printf 'MARKER=%s\\n' \"${SCION_HOOK_WITHHELD_ENV-UNSET}\" >> " + outFile + "\n" +
		"printf 'PATH=%s\\n' \"${PATH-UNSET}\" >> " + outFile + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe hook: %v", err)
	}

	lm := &hooks.LifecycleManager{
		HooksDirs: []string{hooksDir},
		Handlers:  map[string][]hooks.Handler{},
		AgentHome: agentHome,
		Withheld:  pol.Withheld,
	}
	if err := lm.RunPreStart(); err != nil {
		t.Fatalf("RunPreStart: %v", err)
	}

	hookOut, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read hook output: %v", err)
	}
	wantHookOut := "BUNDLE_ONLY_KEY=UNSET\n" +
		"OPERATOR_KEY=operator-value\n" +
		"LD_PRELOAD=UNSET\n" +
		"MARKER=UNSET\n" +
		"PATH=" + strings.Join(rootexec.SearchPath, ":") + "\n"
	if string(hookOut) != wantHookOut {
		t.Errorf("pre-start hook env =\n%s\nwant:\n%s", hookOut, wantHookOut)
	}

	childEnv, err := supervisor.ChildEnv(supervisor.Config{
		Username: "scion",
		Rootless: true,
	}, os.Environ())
	if err != nil {
		t.Fatalf("ChildEnv: %v", err)
	}

	childMap := make(map[string]string, len(childEnv))
	for _, e := range childEnv {
		if k, v, ok := strings.Cut(e, "="); ok {
			childMap[k] = v
		}
	}

	if got := childMap["BUNDLE_ONLY_KEY"]; got != "bundle-secret-value" {
		t.Errorf("ChildEnv[BUNDLE_ONLY_KEY] = %q, want %q", got, "bundle-secret-value")
	}
	if got := childMap["OPERATOR_KEY"]; got != "operator-value" {
		t.Errorf("ChildEnv[OPERATOR_KEY] = %q, want %q", got, "operator-value")
	}
	if _, present := childMap[envpolicy.HookWithheldEnvVar]; present {
		t.Errorf("ChildEnv unexpectedly contains %s", envpolicy.HookWithheldEnvVar)
	}
}

// TestRunInit_HookWithheldEnvSurvivesReExecAndWiresLifecycleManager drives
// RunInit with a bootstrap transport token (which triggers reExecWithCleanEnv)
// and SCION_HOOK_WITHHELD_ENV set. It verifies:
//  1. When reExecWithCleanEnv runs, SCION_HOOK_WITHHELD_ENV is still present in
//     os.Environ() so the re-exec'd init process receives it;
//  2. After the re-exec block, RunInit loads and unsets SCION_HOOK_WITHHELD_ENV
//     and wires the resulting withheld set onto the LifecycleManager it builds.
func TestRunInit_HookWithheldEnvSurvivesReExecAndWiresLifecycleManager(t *testing.T) {
	setupRunInitAsRootlessScion(t, t.TempDir())
	stubRunInitSideEffects(t)
	setTransportTokenEnv(t)

	const markerVal = "BUNDLE_ONLY_KEY,PATH"
	t.Setenv(envpolicy.HookWithheldEnvVar, markerVal)
	t.Setenv("BUNDLE_ONLY_KEY", "bundle-val")
	t.Setenv("OPERATOR_KEY", "operator-val")

	var reExecCalls atomic.Int32
	var markerAtReExec string
	origReExec := reExecWithCleanEnv
	reExecWithCleanEnv = func() error {
		reExecCalls.Add(1)
		markerAtReExec = os.Getenv(envpolicy.HookWithheldEnvVar)
		return nil
	}
	t.Cleanup(func() { reExecWithCleanEnv = origReExec })

	var wiredLM *hooks.LifecycleManager
	origNewLM := runNewLifecycleManager
	runNewLifecycleManager = func(agentHome string, targetUID, targetGID int, requirePrivilegeDrop bool) (*hooks.LifecycleManager, bool) {
		lm, enforce := origNewLM(agentHome, targetUID, targetGID, requirePrivilegeDrop)
		wiredLM = lm
		return lm, enforce
	}
	t.Cleanup(func() { runNewLifecycleManager = origNewLM })

	_ = RunInit([]string{"sh", "-c", "true"}, InitRunOptions{
		DisableTermSignalForwarding: true,
	})

	if reExecCalls.Load() == 0 {
		t.Fatal("expected reExecWithCleanEnv to be called for transport token staging")
	}
	if markerAtReExec != markerVal {
		t.Errorf("%s at reExecWithCleanEnv = %q, want %q (must survive environ-purge re-exec)", envpolicy.HookWithheldEnvVar, markerAtReExec, markerVal)
	}
	if _, present := os.LookupEnv(envpolicy.HookWithheldEnvVar); present {
		t.Errorf("%s was not unset after RunInit loaded it", envpolicy.HookWithheldEnvVar)
	}
	if wiredLM == nil {
		t.Fatal("runNewLifecycleManager was never called")
	}
	for _, want := range []string{"BUNDLE_ONLY_KEY", "PATH", "LD_PRELOAD"} {
		if !wiredLM.Withheld.Withholds(want) {
			t.Errorf("LifecycleManager.Withheld.Withholds(%q) = false, want true", want)
		}
	}
	if wiredLM.Withheld.Withholds("OPERATOR_KEY") {
		t.Error("LifecycleManager.Withheld.Withholds(OPERATOR_KEY) = true, want false")
	}
}
