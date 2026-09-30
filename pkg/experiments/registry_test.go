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

package experiments

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"testing"
)

// valid returns a minimally valid Experiment with every required field set,
// so individual invariant tests can zero out exactly the field under test.
func valid(name string, layers ...Layer) Experiment {
	return Experiment{
		Name:        name,
		Title:       "Title",
		Description: "Description.",
		Layers:      layers,
		Stage:       StageBeta,
		Issue:       "ptone/scion#1",
		Owner:       "team",
		ReviewBy:    "2026-12-31",
	}
}

func TestNewRegistry_Valid(t *testing.T) {
	r, err := NewRegistry([]Experiment{valid("hub.foo", LayerServer)}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	e, ok := r.Lookup("hub.foo")
	if !ok || e.Name != "hub.foo" {
		t.Fatalf("Lookup(hub.foo) = %+v, %v", e, ok)
	}
	if _, ok := r.Lookup("web.unknown"); ok {
		t.Error("Lookup of an unregistered name must return false")
	}
}

func TestNewRegistry_InvariantViolations(t *testing.T) {
	tests := []struct {
		name    string
		active  []Experiment
		retired []string
	}{
		{name: "invalid name: no dot", active: []Experiment{valid("webterminal", LayerWeb)}},
		{name: "invalid name: uppercase", active: []Experiment{valid("web.Terminal", LayerWeb)}},
		{name: "duplicate name", active: []Experiment{valid("web.a", LayerWeb), valid("web.a", LayerWeb)}},
		{name: "active name also retired", active: []Experiment{valid("web.a", LayerWeb)}, retired: []string{"web.a"}},
		{name: "missing title", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.Title = "" })}},
		{name: "missing description", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.Description = "" })}},
		{name: "missing issue", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.Issue = "" })}},
		{name: "missing owner", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.Owner = "" })}},
		{name: "missing review_by", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.ReviewBy = "" })}},
		{name: "malformed review_by: bad month", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.ReviewBy = "2026-13-01" })}},
		{name: "malformed review_by: wrong format", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.ReviewBy = "31/12/2026" })}},
		{name: "invalid stage", active: []Experiment{mutate(valid("web.a", LayerWeb), func(e *Experiment) { e.Stage = "betta" })}},
		{name: "no layers", active: []Experiment{valid("web.a")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRegistry(tt.active, tt.retired); err == nil {
				t.Fatalf("NewRegistry(%q): expected an error, got nil", tt.name)
			}
		})
	}
}

func mutate(e Experiment, f func(*Experiment)) Experiment {
	f(&e)
	return e
}

func TestDefault_PassesInvariantsAndHasNoTestOnlyEntries(t *testing.T) {
	// Default() must not panic, and re-running NewRegistry against the exact
	// compiled lists must succeed independently of the sync.OnceValue cache.
	r := Default()
	if _, err := NewRegistry(compiled, compiledRetired); err != nil {
		t.Fatalf("compiled registry fails its own invariants: %v", err)
	}
	for _, e := range r.All() {
		if e.Name == "hub.test_gate" {
			t.Fatal("production registry must not contain test-only entries")
		}
	}
}

func TestIsRetired(t *testing.T) {
	r, err := NewRegistry(nil, []string{"web.gone"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if !r.IsRetired("web.gone") {
		t.Error("IsRetired(web.gone) = false, want true")
	}
	if r.IsRetired("web.never_existed") {
		t.Error("IsRetired(web.never_existed) = true, want false")
	}
}

func TestHasLayerAndStableOrder(t *testing.T) {
	e := valid("hub.both", LayerServer, LayerWeb)
	if !e.HasLayer(LayerServer) || !e.HasLayer(LayerWeb) {
		t.Fatalf("HasLayer: %+v should have both layers", e)
	}
	if (Experiment{}).HasLayer(LayerWeb) {
		t.Error("zero-value Experiment must not report any layer")
	}

	r, err := NewRegistry([]Experiment{valid("web.b", LayerWeb), valid("web.a", LayerWeb)}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	all := r.All()
	if len(all) != 2 || all[0].Name != "web.a" || all[1].Name != "web.b" {
		t.Fatalf("All() = %v, want [web.a, web.b] (stable order by name)", all)
	}
}

// TestAll_ReturnsIndependentCopies proves that mutating a slice returned by
// All(), or a Layers slice passed into NewRegistry, cannot reach the
// Registry's internal state (design invariant: immutable, no shared mutable
// state).
func TestAll_ReturnsIndependentCopies(t *testing.T) {
	layers := []Layer{LayerWeb}
	r, err := NewRegistry([]Experiment{valid("web.a", layers...)}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Mutating the caller's original Layers slice after construction must
	// not affect the stored entry.
	layers[0] = LayerServer
	if e, _ := r.Lookup("web.a"); !e.HasLayer(LayerWeb) || e.HasLayer(LayerServer) {
		t.Fatalf("Lookup after mutating caller's Layers slice: %+v, want unaffected (LayerWeb only)", e)
	}

	// Mutating a slice returned by All() must not affect a later All() call.
	first := r.All()
	first[0].Name = "corrupted"
	second := r.All()
	if second[0].Name != "web.a" {
		t.Fatalf("All() after mutating a previous All() result: got %q, want %q", second[0].Name, "web.a")
	}
}

// --- DEFAULT_ON_FLAGS consistency (ptone/scion#2217) ---

var (
	defaultOnFlagsLiteral = regexp.MustCompile(`DEFAULT_ON_FLAGS\s*=\s*new Set\(\[([^\]]*)\]\)`)
	flagStringLiteral     = regexp.MustCompile(`'([^']+)'`)
	// nativeChatAllowlist names are explicitly allowed in DEFAULT_ON_FLAGS even
	// though they are not registered experiments (ptone/scion#2217).
	nativeChatAllowlist = []string{"web.native_chat", "web.native_chat_v2"}
)

// defaultOnFlagsFromTS extracts the string literals inside the
// DEFAULT_ON_FLAGS Set literal in feature-flags.ts. The regex only sees
// literals, which is why DEFAULT_ON_FLAGS entries must stay string literals
// (not exported constants) in that file.
func defaultOnFlagsFromTS(src string) ([]string, error) {
	m := defaultOnFlagsLiteral.FindStringSubmatch(src)
	if m == nil {
		return nil, fmt.Errorf("DEFAULT_ON_FLAGS literal not found")
	}
	var flags []string
	for _, mm := range flagStringLiteral.FindAllStringSubmatch(m[1], -1) {
		flags = append(flags, mm[1])
	}
	return flags, nil
}

// checkDefaultOnFlagsConsistency reports a diff between the TS
// DEFAULT_ON_FLAGS set and {web-layer experiments with Default=true} ∪
// nativeChatAllowlist.
func checkDefaultOnFlagsConsistency(tsFlags []string, reg *Registry) error {
	want := map[string]bool{}
	for _, name := range nativeChatAllowlist {
		want[name] = true
	}
	for _, e := range reg.All() {
		if e.Default && e.HasLayer(LayerWeb) {
			want[e.Name] = true
		}
	}
	got := map[string]bool{}
	for _, f := range tsFlags {
		got[f] = true
	}

	var missing, extra []string
	for name := range want {
		if !got[name] {
			missing = append(missing, name)
		}
	}
	for name := range got {
		if !want[name] {
			extra = append(extra, name)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(extra)
	return fmt.Errorf("DEFAULT_ON_FLAGS drifted from the registry: missing %v, extra %v", missing, extra)
}

func TestDefaultOnFlagsConsistency(t *testing.T) {
	content, err := os.ReadFile("../../web/src/utils/feature-flags.ts")
	if err != nil {
		t.Fatalf("read feature-flags.ts: %v", err)
	}
	tsFlags, err := defaultOnFlagsFromTS(string(content))
	if err != nil {
		t.Fatalf("extract DEFAULT_ON_FLAGS: %v", err)
	}
	if err := checkDefaultOnFlagsConsistency(tsFlags, Default()); err != nil {
		t.Fatal(err)
	}
}

// TestDefaultOnFlagsConsistency_DetectsMissingEntry proves the consistency
// check actually fails when a default-on web experiment is missing from the
// TS set (ptone/scion#2217), without touching the real TS file.
func TestDefaultOnFlagsConsistency_DetectsMissingEntry(t *testing.T) {
	e := valid("web.new_default_on_thing", LayerWeb)
	e.Default = true
	reg, err := NewRegistry([]Experiment{e}, nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	tsFlags := append([]string{}, nativeChatAllowlist...) // missing e.Name
	if err := checkDefaultOnFlagsConsistency(tsFlags, reg); err == nil {
		t.Fatal("expected consistency check to fail when a default-on web experiment is missing from DEFAULT_ON_FLAGS")
	}
}
