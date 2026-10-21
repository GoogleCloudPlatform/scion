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
// always wins. The metadata-less rows resolve to the plain Generic harness,
// which carries the entry's ready spec too; a declared ready block is itself
// declarative metadata, so a config with only that block is declarative.
func TestResolve_ConfigWithoutReadyGetsBundleReady(t *testing.T) {
	const declarative = "command:\n  base: [tool]\n"
	hook := api.ReadySpec{Strategy: "hook", Settle: 1500 * time.Millisecond, Timeout: 90 * time.Second}
	delay := api.ReadySpec{Strategy: "delay", Settle: 8 * time.Second}
	for _, tc := range []struct {
		name, config string
		generic      bool
		want         api.ReadySpec
	}{
		{"built-in harness, no block", "harness: claude\n" + declarative, false, hook},
		{"custom harness, no block", "harness: some-custom\n" + declarative, false, delay},
		{"built-in harness, explicit block wins", "harness: claude\nready:\n  strategy: delay\n  settle: 3s\n" + declarative, false,
			api.ReadySpec{Strategy: "delay", Settle: 3 * time.Second}},
		{"metadata-less built-in harness, no block", "harness: claude\n", true, hook},
		{"metadata-less custom harness, no block", "harness: some-custom\n", true, delay},
		{"built-in harness, explicit block only", "harness: claude\nready:\n  strategy: delay\n  settle: 3s\n", false,
			api.ReadySpec{Strategy: "delay", Settle: 3 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := resolveHC(t, tc.config+"image: img:test\n", nil)
			if _, isGeneric := resolved.Harness.(*Generic); isGeneric != tc.generic {
				t.Fatalf("harness is %T, want plain Generic = %v", resolved.Harness, tc.generic)
			}
			if got := resolved.Harness.ReadySpec(); got != tc.want {
				t.Errorf("ReadySpec() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestResolve_BuiltInNameWithoutDirKeepsBundleReady: a built-in harness name
// with no installed harness-config dir resolves to the plain Generic harness,
// which carries the bundle's ready block, the readiness the name had when it
// was keyed on the harness name.
func TestResolve_BuiltInNameWithoutDirKeepsBundleReady(t *testing.T) {
	legacyHook := map[string]bool{"claude": true, "gemini-cli": true, "grok-build": true}
	t.Setenv("HOME", t.TempDir())
	for _, name := range append(AllHarnessNames(), "no-such-harness") {
		resolved, err := Resolve(context.Background(), ResolveOptions{Name: name})
		if err != nil {
			t.Fatalf("%s: Resolve: %v", name, err)
		}
		if resolved.ConfigDir != nil {
			t.Fatalf("%s: unexpected config dir %+v", name, resolved.ConfigDir)
		}
		if _, ok := resolved.Harness.(*Generic); !ok {
			t.Fatalf("%s: harness is %T, want plain Generic", name, resolved.Harness)
		}
		want := config.DefaultReadySpec(config.ReadyStrategyDelay)
		if legacyHook[name] {
			want = config.DefaultReadySpec(config.ReadyStrategyHook)
		}
		if got := resolved.Harness.ReadySpec(); got != want {
			t.Errorf("%s: ReadySpec() = %+v, want %+v", name, got, want)
		}
	}
	// The zero Generic (harness.New for an unknown name) keeps the default.
	if got := (&Generic{}).ReadySpec(); got != config.DefaultReadySpec(config.ReadyStrategyDelay) {
		t.Errorf("zero Generic ReadySpec() = %+v", got)
	}
}

// TestEffectiveConfig_ReadyFromBundleMatchesLegacyTable: with no
// harness-config dir, every built-in harness type gets its bundle's ready
// block, which reproduces the legacy name-keyed table.
func TestEffectiveConfig_ReadyFromBundleMatchesLegacyTable(t *testing.T) {
	legacyHook := map[string]bool{"claude": true, "gemini-cli": true, "grok-build": true}
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
