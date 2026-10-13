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

func TestMergeHarnessConfigEntries_ReplacesReadyDeliveryBlocks(t *testing.T) {
	base := config.HarnessConfigEntry{
		Harness:  "x",
		Ready:    &config.HarnessReadyConfig{Strategy: "hook", Settle: "2s", Timeout: "60s"},
		Delivery: &config.HarnessDeliveryConfig{SubmitKeys: []string{"Enter", "C-m"}, ExtraSubmits: intPtr(3), ExtraSubmitDelay: "1s"},
	}

	t.Run("overlay without blocks keeps base", func(t *testing.T) {
		got := mergeHarnessConfigEntries(base, config.HarnessConfigEntry{Image: "img"})
		if got.Ready != base.Ready || got.Delivery != base.Delivery {
			t.Fatalf("blocks changed: ready=%+v delivery=%+v", got.Ready, got.Delivery)
		}
	})

	t.Run("overlay replaces whole blocks", func(t *testing.T) {
		overlayReady := &config.HarnessReadyConfig{Strategy: "delay"}
		overlayDelivery := &config.HarnessDeliveryConfig{ExtraSubmits: intPtr(0)}
		got := mergeHarnessConfigEntries(base, config.HarnessConfigEntry{Ready: overlayReady, Delivery: overlayDelivery})
		if !reflect.DeepEqual(got.Ready, overlayReady) {
			t.Fatalf("Ready = %+v, want %+v (no field-level merge)", got.Ready, overlayReady)
		}
		if !reflect.DeepEqual(got.Delivery, overlayDelivery) {
			t.Fatalf("Delivery = %+v, want %+v (no field-level merge)", got.Delivery, overlayDelivery)
		}
		wantReady := api.ReadySpec{Strategy: "delay", Settle: 8 * time.Second}
		if r := got.ResolvedReady(); r != wantReady {
			t.Fatalf("ResolvedReady() = %+v, want %+v", r, wantReady)
		}
		wantDelivery := api.DeliverySpec{SubmitKeys: []string{"Enter"}, ExtraSubmits: 0, ExtraSubmitDelay: 300 * time.Millisecond}
		if d := got.ResolvedDelivery(); !reflect.DeepEqual(d, wantDelivery) {
			t.Fatalf("ResolvedDelivery() = %+v, want %+v", d, wantDelivery)
		}
	})
}

func TestHarnessDeliverySpec_Implementations(t *testing.T) {
	defaults := config.DefaultDeliverySpec()
	custom := &config.HarnessDeliveryConfig{SubmitKeys: []string{"C-m"}, ExtraSubmits: intPtr(1), ExtraSubmitDelay: "0s"}
	wantCustom := api.DeliverySpec{SubmitKeys: []string{"C-m"}, ExtraSubmits: 1}

	if got := (&Generic{}).DeliverySpec(); !reflect.DeepEqual(got, defaults) {
		t.Errorf("Generic.DeliverySpec() = %+v, want %+v", got, defaults)
	}
	if got := NewDeclarativeGenericHarness(config.HarnessConfigEntry{Harness: "x"}).DeliverySpec(); !reflect.DeepEqual(got, defaults) {
		t.Errorf("DeclarativeGenericHarness without block = %+v, want %+v", got, defaults)
	}
	if got := NewDeclarativeGenericHarness(config.HarnessConfigEntry{Harness: "x", Delivery: custom}).DeliverySpec(); !reflect.DeepEqual(got, wantCustom) {
		t.Errorf("DeclarativeGenericHarness with block = %+v, want %+v", got, wantCustom)
	}
	csh := &ContainerScriptHarness{entry: config.HarnessConfigEntry{Harness: "x", Delivery: custom}}
	if got := csh.DeliverySpec(); !reflect.DeepEqual(got, wantCustom) {
		t.Errorf("ContainerScriptHarness with block = %+v, want %+v", got, wantCustom)
	}
	if got := (&ContainerScriptHarness{}).DeliverySpec(); !reflect.DeepEqual(got, defaults) {
		t.Errorf("ContainerScriptHarness without block = %+v, want %+v", got, defaults)
	}
}

func intPtr(n int) *int { return &n }

// TestResolve_ConfigWithoutReadyGetsBundleReady covers harness-configs
// written before the ready block existed: one for a built-in harness type
// resolves to that bundle's ready block (hook for claude), one for a harness
// with no built-in bundle keeps the delay default, and an explicit block
// always wins.
func TestResolve_ConfigWithoutReadyGetsBundleReady(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		want         api.ReadySpec
	}{
		{"built-in harness, no block", "harness: claude\n",
			api.ReadySpec{Strategy: "hook", Settle: 1500 * time.Millisecond, Timeout: 90 * time.Second}},
		{"custom harness, no block", "harness: some-custom\n",
			api.ReadySpec{Strategy: "delay", Settle: 8 * time.Second}},
		{"built-in harness, explicit block wins", "harness: claude\nready:\n  strategy: delay\n  settle: 3s\n",
			api.ReadySpec{Strategy: "delay", Settle: 3 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			hcDir := filepath.Join(home, ".scion", "harness-configs", "hc")
			writeFile(t, filepath.Join(hcDir, "config.yaml"), tc.config+"image: img:test\ncommand:\n  base: [tool]\n")

			resolved, err := Resolve(context.Background(), ResolveOptions{Name: "hc"})
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if resolved.ConfigDir == nil || resolved.ConfigDir.Path != hcDir {
				t.Fatalf("resolved config dir = %+v, want %s", resolved.ConfigDir, hcDir)
			}
			if got := resolved.Harness.ReadySpec(); got != tc.want {
				t.Errorf("ReadySpec() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestEffectiveConfig_ReadyFromBundleMatchesLegacyTable: with no
// harness-config dir, every built-in harness type gets its bundle's ready
// block, which reproduces the legacy name-keyed table.
func TestEffectiveConfig_ReadyFromBundleMatchesLegacyTable(t *testing.T) {
	legacyHook := map[string]bool{"claude": true, "gemini-cli": true, "copilot": true, "grok-build": true}
	for _, name := range AllHarnessNames() {
		entry := EffectiveConfig(name, nil, nil, "")
		if entry.Ready == nil {
			t.Errorf("%s: no ready block", name)
			continue
		}
		want := config.DefaultReadySpec(config.ReadyStrategyDelay)
		if legacyHook[name] {
			want = config.DefaultReadySpec(config.ReadyStrategyHook)
		}
		if got := entry.ResolvedReady(); got != want {
			t.Errorf("%s: ResolvedReady() = %+v, want %+v", name, got, want)
		}
	}
	// The block is a copy: changing it does not change the bundle's.
	first := EffectiveConfig("claude", nil, nil, "")
	first.Ready.Strategy = "delay"
	if again := EffectiveConfig("claude", nil, nil, ""); again.Ready.Strategy != "hook" {
		t.Errorf("bundle block mutated through a returned entry: %+v", again.Ready)
	}
}
