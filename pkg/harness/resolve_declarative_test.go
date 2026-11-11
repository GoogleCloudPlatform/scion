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
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// resolveHC writes config.yaml for a harness-config named "hc" under a fresh
// HOME and resolves it.
func resolveHC(t *testing.T, configYAML string, settings *config.VersionedSettings) *ResolvedHarness {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	hcDir := filepath.Join(home, ".scion", "harness-configs", "hc")
	writeFile(t, filepath.Join(hcDir, "config.yaml"), configYAML)
	resolved, err := Resolve(context.Background(), ResolveOptions{Name: "hc", Settings: settings})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.ConfigDir == nil || resolved.ConfigDir.Path != hcDir {
		t.Fatalf("resolved config dir = %+v, want %s", resolved.ConfigDir, hcDir)
	}
	return resolved
}

// TestResolve_ReadyOrDeliveryOnlyConfigIsDeclarative: a custom
// harness-config whose only declarative metadata is a ready and/or delivery
// block resolves to the declarative generic harness and gets the declared
// specs, whether the block comes from config.yaml or a settings overlay. As
// a declarative harness it reports the config's harness type as its name and
// returns other declared fields such as interrupt_key. This is the harness
// Resolve returns; host delivery and interrupt do not use it yet
// (ptone/scion#5002).
func TestResolve_ReadyOrDeliveryOnlyConfigIsDeclarative(t *testing.T) {
	hook := config.DefaultReadySpec(config.ReadyStrategyHook)
	delay := config.DefaultReadySpec(config.ReadyStrategyDelay)
	defaultDelivery := config.DefaultDeliverySpec()
	customDelivery := api.DeliverySpec{SubmitKeys: []string{"C-m"}, ExtraSubmits: 2, ExtraSubmitDelay: time.Second}
	overlay := &config.VersionedSettings{HarnessConfigs: map[string]config.HarnessConfigEntry{
		"hc": {Delivery: &config.HarnessDeliveryConfig{SubmitKeys: []string{"C-m"}, ExtraSubmits: intPtr(2), ExtraSubmitDelay: "1s"}},
	}}
	const deliveryBlock = "delivery:\n  submit_keys: [C-m]\n  extra_submits: 2\n  extra_submit_delay: 1s\n"

	for _, tc := range []struct {
		name, config  string
		settings      *config.VersionedSettings
		wantReady     api.ReadySpec
		wantDelivery  api.DeliverySpec
		wantInterrupt string
	}{
		{"image and ready only", "harness: some-custom\nimage: img:test\nready:\n  strategy: hook\n", nil, hook, defaultDelivery, "C-c"},
		{"image and delivery only", "harness: some-custom\nimage: img:test\n" + deliveryBlock, nil, delay, customDelivery, "C-c"},
		{"image, ready and delivery", "harness: some-custom\nimage: img:test\nready:\n  strategy: hook\n" + deliveryBlock, nil, hook, customDelivery, "C-c"},
		{"delivery from settings overlay", "harness: some-custom\nimage: img:test\n", overlay, delay, customDelivery, "C-c"},
		{"ready and interrupt_key", "harness: some-custom\nimage: img:test\ninterrupt_key: Escape\nready:\n  strategy: hook\n", nil, hook, defaultDelivery, "Escape"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolveHC(t, tc.config, tc.settings)
			if _, ok := resolved.Harness.(*DeclarativeGenericHarness); !ok {
				t.Fatalf("harness is %T, want *DeclarativeGenericHarness", resolved.Harness)
			}
			if resolved.Implementation != "generic" {
				t.Errorf("Implementation = %q, want generic", resolved.Implementation)
			}
			// The harness Resolve returns reports the config's harness type
			// and its other declared fields, unlike the plain Generic. Host
			// delivery and interrupt do not use it yet (ptone/scion#5002).
			if got := resolved.Harness.Name(); got != "some-custom" {
				t.Errorf("Name() = %q, want some-custom", got)
			}
			if got := resolved.Harness.GetInterruptKey(); got != tc.wantInterrupt {
				t.Errorf("GetInterruptKey() = %q, want %q", got, tc.wantInterrupt)
			}
			if got := resolved.Harness.ReadySpec(); got != tc.wantReady {
				t.Errorf("ReadySpec() = %+v, want %+v", got, tc.wantReady)
			}
			if got := resolved.Harness.DeliverySpec(); !reflect.DeepEqual(got, tc.wantDelivery) {
				t.Errorf("DeliverySpec() = %+v, want %+v", got, tc.wantDelivery)
			}
			// Nothing else is declared, so the generic defaults still apply.
			if got := resolved.Harness.DefaultConfigDir(); got != ".scion" {
				t.Errorf("DefaultConfigDir() = %q, want .scion", got)
			}
			if got := resolved.Harness.SkillsDir(); got != GenericSkillsDir {
				t.Errorf("SkillsDir() = %q, want %q", got, GenericSkillsDir)
			}
			if got := resolved.Harness.GetCommand("task", false, []string{"tool"}); !reflect.DeepEqual(got, []string{"tool", "task"}) {
				t.Errorf("GetCommand() = %q, want [tool task]", got)
			}
		})
	}
}

// TestResolve_NoReadyDeliveryKeepsGenericChoices: configs that declare no
// ready or delivery block resolve exactly as before. Metadata-less configs,
// including ones for a built-in harness type whose ready block comes from
// the bundle, stay on the plain Generic harness; configs with other
// declarative metadata stay declarative with the same config dir, skills dir
// and command.
func TestResolve_NoReadyDeliveryKeepsGenericChoices(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		generic      bool
		configDir    string
		skillsDir    string
		command      []string
	}{
		{"metadata-less custom harness", "harness: some-custom\n", true, ".scion", GenericSkillsDir, []string{"tool", "task"}},
		{"metadata-less built-in harness", "harness: claude\n", true, ".scion", GenericSkillsDir, []string{"tool", "task"}},
		{"metadata-less built-in harness (opencode warning path)", "harness: opencode\n", true, ".scion", GenericSkillsDir, []string{"tool", "task"}},
		{"config_dir only", "harness: some-custom\nconfig_dir: .custom\n", false, ".custom", GenericSkillsDir, []string{"tool", "task"}},
		{"skills_dir only", "harness: some-custom\nskills_dir: .custom/skills\n", false, ".scion", ".custom/skills", []string{"tool", "task"}},
		{"command only", "harness: some-custom\ncommand:\n  base: [run]\n  task_flag: --task\n", false, ".scion", GenericSkillsDir, []string{"run", "tool", "--task", "task"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolveHC(t, tc.config+"image: img:test\n", nil)
			_, isGeneric := resolved.Harness.(*Generic)
			if isGeneric != tc.generic {
				t.Fatalf("harness is %T, want plain Generic = %v", resolved.Harness, tc.generic)
			}
			if !tc.generic {
				if _, ok := resolved.Harness.(*DeclarativeGenericHarness); !ok {
					t.Fatalf("harness is %T, want *DeclarativeGenericHarness", resolved.Harness)
				}
			}
			if got := resolved.Harness.DefaultConfigDir(); got != tc.configDir {
				t.Errorf("DefaultConfigDir() = %q, want %q", got, tc.configDir)
			}
			if got := resolved.Harness.SkillsDir(); got != tc.skillsDir {
				t.Errorf("SkillsDir() = %q, want %q", got, tc.skillsDir)
			}
			if got := SkillsDirForEntry(resolved.Config); got != tc.skillsDir {
				t.Errorf("SkillsDirForEntry() = %q, want %q", got, tc.skillsDir)
			}
			if got := resolved.Harness.GetCommand("task", false, []string{"tool"}); !reflect.DeepEqual(got, tc.command) {
				t.Errorf("GetCommand() = %q, want %q", got, tc.command)
			}
			if got := resolved.Harness.DeliverySpec(); !reflect.DeepEqual(got, config.DefaultDeliverySpec()) {
				t.Errorf("DeliverySpec() = %+v, want default", got)
			}
		})
	}
}

// TestResolve_BuiltInBundlesStayContainerScript: every built-in harness
// bundle has a provisioner, so it resolves to the container-script harness
// before the declarative check is reached.
func TestResolve_BuiltInBundlesStayContainerScript(t *testing.T) {
	// Find the bundles before HOME changes: the repo root lookup runs git,
	// which reads the user's git config from HOME.
	paths := map[string]string{}
	for _, name := range AllHarnessNames() {
		paths[name] = bundlePath(t, name)
	}
	t.Setenv("HOME", t.TempDir())
	for _, name := range AllHarnessNames() {
		resolved, err := Resolve(context.Background(), ResolveOptions{Name: name, ConfigDirPath: paths[name]})
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		if resolved.Implementation != "container-script" {
			t.Errorf("%s: Implementation = %q, want container-script", name, resolved.Implementation)
		}
	}
}
