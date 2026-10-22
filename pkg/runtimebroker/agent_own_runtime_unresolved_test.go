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

package runtimebroker

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for an existing agent whose saved profile does not resolve in the
// loaded settings (ptone/scion#5055): the failure is memoised per profile
// catalog and logged at warn level once per agent, then at debug level.

const savedProfileUnresolvedMsg = "Agent's saved profile no longer resolves"

// The unresolved saved profile in these tests is "gke-gone": it must be
// absent from the embedded default settings too, which define "local" and
// "remote".

// unresolvedFixture is an ownRTFixture whose agent log keeps debug records
// and whose profile resolutions are counted.
type unresolvedFixture struct {
	*ownRTFixture
	resolves atomic.Int32
}

func newUnresolvedFixture(t *testing.T, savedProfile string) *unresolvedFixture {
	t.Helper()
	f := &unresolvedFixture{ownRTFixture: newOwnRTFixture(t, savedProfile, true)}
	f.srv.agentLifecycleLog = slog.New(slog.NewJSONHandler(&f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.srv.resolveProfileRuntime = func(vs *config.VersionedSettings, profile string) (config.V1RuntimeConfig, string, error) {
		f.resolves.Add(1)
		return vs.ResolveRuntime(profile)
	}
	return f
}

// ensure runs ensureAgentOwnRuntime for agent id and returns the own runtime
// it attached, or nil.
func (f *unresolvedFixture) ensure(id string) *agentOwnRuntime {
	return agentOwnRuntimeFrom(f.srv.ensureAgentOwnRuntime(context.Background(), id, ownRTProjectID, ""))
}

// levels returns the levels of the saved-profile fallback records for agent
// id, in order.
func (f *unresolvedFixture) levels(t *testing.T, id string) []string {
	t.Helper()
	var out []string
	for _, rec := range f.logRecords(t, savedProfileUnresolvedMsg) {
		if rec["agent_id"] == id {
			lvl, _ := rec["level"].(string)
			out = append(out, lvl)
		}
	}
	return out
}

// editSettings replaces old with repl exactly once in the project's settings.
func (f *unresolvedFixture) editSettings(t *testing.T, old, repl string) {
	t.Helper()
	p := filepath.Join(f.projectDir, "settings.yaml")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), old); n != 1 {
		t.Fatalf("settings contain %q %d times, want 1", old, n)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(data), old, repl, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addAgent adds another agent provisioned with savedProfile to the
// fixture's project.
func (f *unresolvedFixture) addAgent(t *testing.T, name, savedProfile string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.projectDir, "agents", name), 0o755); err != nil {
		t.Fatal(err)
	}
	agentHome := config.GetAgentHomePath(f.projectDir, name)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	info, _ := json.Marshal(api.AgentInfo{Name: name, Profile: savedProfile, HarnessConfig: "default"})
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), info, 0o644); err != nil {
		t.Fatal(err)
	}
	if savedProfile != "" {
		writeProvisionedProfile(t, f.projectDir, name, savedProfile)
	}
}

func memoEntries(m interface{ Range(func(any, any) bool) }) int {
	n := 0
	m.Range(func(any, any) bool { n++; return true })
	return n
}

// N requests for an agent whose saved profile is not in the settings give
// one warn record and N-1 debug records, and resolve the profile once. A
// second agent with the same saved profile gets its own warn record but no
// further resolution.
func TestEnsureAgentOwnRuntime_UnresolvedProfileWarnsOnceResolvesOnce(t *testing.T) {
	f := newUnresolvedFixture(t, "gke-gone")
	const n = 5
	for i := 0; i < n; i++ {
		if own := f.ensure(ownRTAgent); own != nil {
			t.Fatalf("request %d: unresolved profile produced an own runtime", i)
		}
	}
	want := []string{"WARN", "DEBUG", "DEBUG", "DEBUG", "DEBUG"}
	if got := f.levels(t, ownRTAgent); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fallback log levels = %v, want %v", got, want)
	}
	if got := f.resolves.Load(); got != 1 {
		t.Fatalf("profile resolved %d times for %d requests, want 1", got, n)
	}

	f.addAgent(t, "other-agent", "gke-gone")
	f.ensure("other-agent")
	f.ensure("other-agent")
	if got := f.levels(t, "other-agent"); strings.Join(got, ",") != "WARN,DEBUG" {
		t.Fatalf("second agent's fallback log levels = %v, want [WARN DEBUG]", got)
	}
	if got := f.resolves.Load(); got != 1 {
		t.Fatalf("profile resolved %d times after the second agent, want 1", got)
	}
	if got := memoEntries(&f.srv.savedProfileFailures); got != 1 {
		t.Fatalf("failure memo entries = %d, want 1 (one project and profile)", got)
	}
	if got := f.resolverCalls.Load(); got != 0 {
		t.Fatalf("auxiliary runtime resolved %d times, want 0", got)
	}
}

// A settings change that adds the saved profile makes the next request
// resolve it, register and memoise its runtime, and clear the failure.
func TestEnsureAgentOwnRuntime_UnresolvedProfileAddedResolves(t *testing.T) {
	f := newUnresolvedFixture(t, "gke-gone")
	f.ensure(ownRTAgent)
	f.ensure(ownRTAgent)
	if got := f.resolves.Load(); got != 1 {
		t.Fatalf("profile resolved %d times before the settings change, want 1", got)
	}

	f.editSettings(t, "profiles:\n", "profiles:\n    gke-gone:\n        runtime: k8s-agents\n")

	first := f.ensure(ownRTAgent)
	if first == nil {
		t.Fatal("request after adding the profile did not resolve the own runtime")
	}
	if ns := ownRuntimeNamespace(first.rt); ns != ownRTProfileNS {
		t.Fatalf("own runtime namespace = %q, want %q", ns, ownRTProfileNS)
	}
	if again := f.ensure(ownRTAgent); again != first {
		t.Fatalf("next request did not reuse the memoised own runtime: %p %p", first, again)
	}
	if got := f.resolverCalls.Load(); got != 1 {
		t.Fatalf("auxiliary runtime resolved %d times, want 1", got)
	}
	if got := memoEntries(&f.srv.savedProfileFailures); got != 0 {
		t.Fatalf("failure memo entries = %d after the profile resolved, want 0", got)
	}
	if got := f.levels(t, ownRTAgent); strings.Join(got, ",") != "WARN,DEBUG" {
		t.Fatalf("fallback log levels = %v, want [WARN DEBUG]", got)
	}
}

// A profile that resolves again and then disappears once more, back to a
// catalog whose failure was already logged, is logged at warn level again.
func TestEnsureAgentOwnRuntime_ProfileLostAgainWarnsAgain(t *testing.T) {
	f := newUnresolvedFixture(t, "gke-gone")
	f.ensure(ownRTAgent)
	f.editSettings(t, "profiles:\n", "profiles:\n    gke-gone:\n        runtime: k8s-agents\n")
	if own := f.ensure(ownRTAgent); own == nil {
		t.Fatal("request after adding the profile did not resolve the own runtime")
	}
	f.editSettings(t, "profiles:\n    gke-gone:\n        runtime: k8s-agents\n", "profiles:\n")
	if own := f.ensure(ownRTAgent); own != nil {
		t.Fatal("request after removing the profile produced an own runtime")
	}
	if got := f.levels(t, ownRTAgent); strings.Join(got, ",") != "WARN,WARN" {
		t.Fatalf("fallback log levels = %v, want [WARN WARN]", got)
	}
}

// A profile that names a missing runtime resolves once its runtime name is
// fixed, though the profile and runtime names are unchanged: the catalog
// fingerprint covers the profile's runtime.
func TestEnsureAgentOwnRuntime_ProfileRuntimeFixedResolves(t *testing.T) {
	f := newUnresolvedFixture(t, "agents")
	f.editSettings(t, "runtime: k8s-agents\n", "runtime: k8s-missing\n")
	f.ensure(ownRTAgent)
	f.ensure(ownRTAgent)
	if got := f.resolves.Load(); got != 1 {
		t.Fatalf("profile resolved %d times before the fix, want 1", got)
	}
	f.editSettings(t, "runtime: k8s-missing\n", "runtime: k8s-agents\n")
	if own := f.ensure(ownRTAgent); own == nil {
		t.Fatal("request after fixing the profile's runtime did not resolve the own runtime")
	}
	if got := f.resolves.Load(); got != 2 {
		t.Fatalf("profile resolved %d times, want 2", got)
	}
}

// A flat instance returns before reading the saved profile: no resolution,
// no log record, no memo entry.
func TestEnsureAgentOwnRuntime_FlatInstanceSkipsSavedProfile(t *testing.T) {
	f := newUnresolvedFixture(t, "gke-gone")
	f.srv.config.FlatInstance = &FlatInstanceConfig{Identity: &brokeridentity.Identity{RuntimeBrokerID: "rb-flat"}, HubInProcess: true}
	if !f.srv.isFlat() {
		t.Fatal("fixture is not a flat instance")
	}
	for i := 0; i < 3; i++ {
		if own := f.ensure(ownRTAgent); own != nil {
			t.Fatalf("request %d: flat instance attached an own runtime", i)
		}
	}
	if got := f.resolves.Load(); got != 0 {
		t.Fatalf("flat instance resolved the profile %d times, want 0", got)
	}
	if got := f.levels(t, ownRTAgent); len(got) != 0 {
		t.Fatalf("flat instance logged the fallback: %v", got)
	}
	if got := memoEntries(&f.srv.savedProfileFailures); got != 0 {
		t.Fatalf("flat instance stored %d failure memo entries, want 0", got)
	}
}
