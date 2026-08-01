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

//go:build !no_sqlite

package hub

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// envScopeTestHubID is the hub instance ID used by the scope-precedence tests.
const envScopeTestHubID = "hub-envscope-1"

// envScopeTestAgent returns an agent wired to every scope the hub env resolver
// knows about, so that all four scopes are applicable.
func envScopeTestAgent() *store.Agent {
	return &store.Agent{
		ID:              "agent-envscope-1",
		Name:            "envscope-agent",
		Slug:            "envscope-agent",
		ProjectID:       "project-envscope-1",
		OwnerID:         "user-envscope-1",
		RuntimeBrokerID: "broker-envscope-1",
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
}

// envScopeTestScopeID maps a scope constant to the scope ID used by
// envScopeTestAgent for that scope.
func envScopeTestScopeID(t *testing.T, scope string) string {
	t.Helper()
	switch scope {
	case store.ScopeHub:
		return envScopeTestHubID
	case store.ScopeProject:
		return "project-envscope-1"
	case store.ScopeUser:
		return "user-envscope-1"
	case store.ScopeRuntimeBroker:
		return "broker-envscope-1"
	default:
		t.Fatalf("unknown scope %q", scope)
		return ""
	}
}

// newEnvScopeDispatcher builds a dispatcher over a fresh in-memory store with
// the hub ID set, and seeds key=value pairs in the requested scopes.
func newEnvScopeDispatcher(t *testing.T, key string, valuesByScope map[string]string) (*HTTPAgentDispatcher, store.Store) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)

	for scope, value := range valuesByScope {
		if _, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
			ID:            api.NewUUID(),
			Key:           key,
			Value:         value,
			Scope:         scope,
			ScopeID:       envScopeTestScopeID(t, scope),
			InjectionMode: store.InjectionModeAlways,
		}); err != nil {
			t.Fatalf("seeding %s-scoped env var: %v", scope, err)
		}
	}

	d := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())
	d.SetHubID(envScopeTestHubID)
	return d, memStore
}

// TestEnvScopesInPrecedenceOrder_ListsAllFourScopes guards the extraction
// hazard: the ordering helper replaced four near-identical inline blocks, and a
// scope dropped during that extraction would produce a clean, empty result with
// no error and no log. This test asserts all four scopes are present, by typed
// constant, in the named order.
func TestEnvScopesInPrecedenceOrder_ListsAllFourScopes(t *testing.T) {
	d, _ := newEnvScopeDispatcher(t, "UNUSED", nil)
	agent := envScopeTestAgent()

	want := []store.EnvVarFilter{
		{Scope: store.ScopeRuntimeBroker, ScopeID: "broker-envscope-1"},
		{Scope: store.ScopeHub, ScopeID: envScopeTestHubID},
		{Scope: store.ScopeProject, ScopeID: "project-envscope-1"},
		{Scope: store.ScopeUser, ScopeID: "user-envscope-1"},
	}

	got := d.envScopesInPrecedenceOrder(agent)
	if len(got) != len(want) {
		t.Fatalf("envScopesInPrecedenceOrder returned %d filters (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filter[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestEnvScopesInPrecedenceOrder_OmitsScopesWithoutID pins that a scope whose
// ID is empty for this agent is skipped, and that the hub scope is queried
// regardless (an empty ScopeID means "no scope-ID filter" to the store, which
// is the long-standing behaviour of the hub scope query).
func TestEnvScopesInPrecedenceOrder_OmitsScopesWithoutID(t *testing.T) {
	d, _ := newEnvScopeDispatcher(t, "UNUSED", nil)
	d.SetHubID("")

	agent := envScopeTestAgent()
	agent.ProjectID = ""
	agent.RuntimeBrokerID = ""

	want := []store.EnvVarFilter{
		{Scope: store.ScopeHub, ScopeID: ""},
		{Scope: store.ScopeUser, ScopeID: "user-envscope-1"},
	}
	got := d.envScopesInPrecedenceOrder(agent)
	if len(got) != len(want) {
		t.Fatalf("envScopesInPrecedenceOrder returned %d filters (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("filter[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestEnvScopeSourceLabel covers the scope -> CLI source-name mapping, which is
// not the identity function: runtime_broker is reported as "broker".
func TestEnvScopeSourceLabel(t *testing.T) {
	cases := map[string]string{
		store.ScopeHub:           "hub",
		store.ScopeProject:       "project",
		store.ScopeUser:          "user",
		store.ScopeRuntimeBroker: "broker",
	}
	for scope, want := range cases {
		if got := envScopeSourceLabel(scope); got != want {
			t.Errorf("envScopeSourceLabel(%q) = %q, want %q", scope, got, want)
		}
	}
}

// These two ladders are written out in full rather than read from
// envScopePrecedence, so the collision tests below assert against a ladder they
// control. A test that took its input AND its expectation from the same global
// would pass under any ordering, which is the one thing these must not do: the
// warning they cover exists precisely because the ordering moved.
//
// ladderBrokerLowest is what ships (4-B target (iii)); ladderBrokerHighest is
// the pre-Phase-10 ordering, kept as the negative control that proves the
// collision detector is reading the ladder and not just matching duplicates.
var (
	ladderBrokerHighest = []string{store.ScopeHub, store.ScopeProject, store.ScopeUser, store.ScopeRuntimeBroker}
	ladderBrokerLowest  = []string{store.ScopeRuntimeBroker, store.ScopeHub, store.ScopeProject, store.ScopeUser}
)

// TestEnvScopesOutranking covers the helper that decides who beats whom,
// including the two empty results that mean different things: nil for "this
// scope is not on the ladder at all" and an empty-but-non-nil slice for "this
// scope is the top of the ladder". Collapsing those two would make an unknown
// scope look like the highest-precedence one.
func TestEnvScopesOutranking(t *testing.T) {
	t.Run("mid ladder", func(t *testing.T) {
		got := envScopesOutranking(ladderBrokerHighest, store.ScopeProject)
		want := []string{store.ScopeUser, store.ScopeRuntimeBroker}
		if len(got) != len(want) {
			t.Fatalf("envScopesOutranking(project) = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("top of ladder is empty but not nil", func(t *testing.T) {
		got := envScopesOutranking(ladderBrokerHighest, store.ScopeRuntimeBroker)
		if got == nil {
			t.Fatal("envScopesOutranking returned nil for the top scope, want empty non-nil")
		}
		if len(got) != 0 {
			t.Errorf("envScopesOutranking(runtime_broker) = %v, want empty", got)
		}
	})

	t.Run("bottom of 4-B(iii) ladder is outranked by everything", func(t *testing.T) {
		got := envScopesOutranking(ladderBrokerLowest, store.ScopeRuntimeBroker)
		want := []string{store.ScopeHub, store.ScopeProject, store.ScopeUser}
		if len(got) != len(want) {
			t.Fatalf("envScopesOutranking(runtime_broker) = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("scope absent from ladder is nil", func(t *testing.T) {
		if got := envScopesOutranking(ladderBrokerHighest, "not-a-scope"); got != nil {
			t.Errorf("envScopesOutranking(absent) = %v, want nil", got)
		}
	})
}

// TestEnvScopeCollisions covers the pure core of the startup shadow warning.
//
// The first subtest is the load-bearing one and it is a NEGATIVE CONTROL for
// the rest: under a ladder where nothing outranks runtime_broker, a key defined
// in every scope must produce NO collisions. Without it, a function that simply
// reported every duplicated key would pass all the positive cases below.
func TestEnvScopeCollisions(t *testing.T) {
	brokerVar := func(key, id string) store.EnvVar {
		return store.EnvVar{Key: key, Value: "v", Scope: store.ScopeRuntimeBroker, ScopeID: id}
	}

	t.Run("nothing outranks broker: no collisions even when every scope defines the key", func(t *testing.T) {
		vars := []store.EnvVar{
			brokerVar("SHARED_KEY", "broker-1"),
			{Key: "SHARED_KEY", Value: "v", Scope: store.ScopeHub, ScopeID: "hub-1"},
			{Key: "SHARED_KEY", Value: "v", Scope: store.ScopeProject, ScopeID: "project-1"},
			{Key: "SHARED_KEY", Value: "v", Scope: store.ScopeUser, ScopeID: "user-1"},
		}
		if got := envScopeCollisions(ladderBrokerHighest, store.ScopeRuntimeBroker, vars); len(got) != 0 {
			t.Errorf("envScopeCollisions = %+v, want none (broker is top of this ladder)", got)
		}
	})

	t.Run("same input under 4-B(iii) reports the key", func(t *testing.T) {
		vars := []store.EnvVar{
			brokerVar("SHARED_KEY", "broker-1"),
			{Key: "SHARED_KEY", Value: "v", Scope: store.ScopeHub, ScopeID: "hub-1"},
			{Key: "SHARED_KEY", Value: "v", Scope: store.ScopeUser, ScopeID: "user-1"},
		}
		got := envScopeCollisions(ladderBrokerLowest, store.ScopeRuntimeBroker, vars)
		if len(got) != 1 {
			t.Fatalf("envScopeCollisions returned %d collisions (%+v), want 1", len(got), got)
		}
		if got[0].Key != "SHARED_KEY" {
			t.Errorf("Key = %q, want SHARED_KEY", got[0].Key)
		}
		if len(got[0].ScopeIDs) != 1 || got[0].ScopeIDs[0] != "broker-1" {
			t.Errorf("ScopeIDs = %v, want [broker-1]", got[0].ScopeIDs)
		}
		// Reported in ladder order, not alphabetical: hub before user.
		want := []string{store.ScopeHub, store.ScopeUser}
		if len(got[0].OutrankedBy) != len(want) {
			t.Fatalf("OutrankedBy = %v, want %v", got[0].OutrankedBy, want)
		}
		for i := range want {
			if got[0].OutrankedBy[i] != want[i] {
				t.Errorf("OutrankedBy[%d] = %q, want %q", i, got[0].OutrankedBy[i], want[i])
			}
		}
	})

	t.Run("broker-only key is not a collision", func(t *testing.T) {
		vars := []store.EnvVar{brokerVar("BROKER_ONLY_KEY", "broker-1")}
		if got := envScopeCollisions(ladderBrokerLowest, store.ScopeRuntimeBroker, vars); len(got) != 0 {
			t.Errorf("envScopeCollisions = %+v, want none", got)
		}
	})

	t.Run("higher-scope-only key is not a collision", func(t *testing.T) {
		vars := []store.EnvVar{{Key: "USER_ONLY_KEY", Value: "v", Scope: store.ScopeUser, ScopeID: "user-1"}}
		if got := envScopeCollisions(ladderBrokerLowest, store.ScopeRuntimeBroker, vars); len(got) != 0 {
			t.Errorf("envScopeCollisions = %+v, want none", got)
		}
	})

	t.Run("multiple brokers and deterministic key order", func(t *testing.T) {
		vars := []store.EnvVar{
			brokerVar("ZEBRA_KEY", "broker-2"),
			brokerVar("ZEBRA_KEY", "broker-1"),
			brokerVar("ALPHA_KEY", "broker-1"),
			{Key: "ZEBRA_KEY", Value: "v", Scope: store.ScopeProject, ScopeID: "project-1"},
			{Key: "ALPHA_KEY", Value: "v", Scope: store.ScopeProject, ScopeID: "project-1"},
		}
		got := envScopeCollisions(ladderBrokerLowest, store.ScopeRuntimeBroker, vars)
		if len(got) != 2 {
			t.Fatalf("envScopeCollisions returned %d collisions (%+v), want 2", len(got), got)
		}
		if got[0].Key != "ALPHA_KEY" || got[1].Key != "ZEBRA_KEY" {
			t.Errorf("keys = %q, %q; want ALPHA_KEY, ZEBRA_KEY (sorted)", got[0].Key, got[1].Key)
		}
		if len(got[1].ScopeIDs) != 2 || got[1].ScopeIDs[0] != "broker-1" || got[1].ScopeIDs[1] != "broker-2" {
			t.Errorf("ZEBRA_KEY ScopeIDs = %v, want [broker-1 broker-2] sorted", got[1].ScopeIDs)
		}
	})

	t.Run("identical values still collide: over-reporting is deliberate", func(t *testing.T) {
		vars := []store.EnvVar{
			{Key: "SAME", Value: "identical", Scope: store.ScopeRuntimeBroker, ScopeID: "broker-1"},
			{Key: "SAME", Value: "identical", Scope: store.ScopeUser, ScopeID: "user-1"},
		}
		if got := envScopeCollisions(ladderBrokerLowest, store.ScopeRuntimeBroker, vars); len(got) != 1 {
			t.Errorf("envScopeCollisions = %+v, want 1; matching on key alone is intended", got)
		}
	})
}

// TestListEnvVars_EmptyScopeIDReturnsEveryID pins the store behaviour that
// WarnOutrankedBrokerEnvKeys depends on: one query per scope, with ScopeID left
// empty, must return that scope's vars across every scope ID. If the store ever
// started treating an empty ScopeID as "match the empty ID", the warning would
// go silent with no error — a false green of exactly the kind this workstream
// keeps finding.
func TestListEnvVars_EmptyScopeIDReturnsEveryID(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)

	for _, brokerID := range []string{"broker-a", "broker-b"} {
		if _, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
			ID: api.NewUUID(), Key: "SHARED_KEY", Value: "v",
			Scope: store.ScopeRuntimeBroker, ScopeID: brokerID,
		}); err != nil {
			t.Fatalf("seeding %s: %v", brokerID, err)
		}
	}
	// A var in a different scope, to prove the Scope filter is still applied.
	if _, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "SHARED_KEY", Value: "v",
		Scope: store.ScopeUser, ScopeID: "user-1",
	}); err != nil {
		t.Fatalf("seeding user scope: %v", err)
	}

	got, err := memStore.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeRuntimeBroker})
	if err != nil {
		t.Fatalf("ListEnvVars: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListEnvVars(scope=runtime_broker, no ID) returned %d vars (%+v), want 2", len(got), got)
	}
	for _, v := range got {
		if v.Scope != store.ScopeRuntimeBroker {
			t.Errorf("got a %s-scoped var back from a runtime_broker query: %+v", v.Scope, v)
		}
	}
}

// TestWarnOutrankedBrokerEnvKeys_LogsShadowedKeys exercises the exported entry
// point end to end against real storage and the ladder that actually ships,
// capturing the log rather than trusting that it was written.
//
// It carries its own negative control in the same run: BROKER_ONLY_KEY is set
// at broker scope and nowhere else, so it must NOT be named. Without that, a
// warning that simply listed every broker-scoped key would pass — and the whole
// point of the warning is that it names the keys whose value is about to
// change, not the keys that exist.
func TestWarnOutrankedBrokerEnvKeys_LogsShadowedKeys(t *testing.T) {
	ctx := context.Background()
	d, memStore := newEnvScopeDispatcher(t, "SHADOWED_KEY", map[string]string{
		store.ScopeUser:          "from-user",
		store.ScopeRuntimeBroker: "from-broker",
	})
	if _, err := memStore.UpsertEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "BROKER_ONLY_KEY", Value: "from-broker",
		Scope: store.ScopeRuntimeBroker, ScopeID: envScopeTestScopeID(t, store.ScopeRuntimeBroker),
	}); err != nil {
		t.Fatalf("seeding BROKER_ONLY_KEY: %v", err)
	}

	var buf bytes.Buffer
	d.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if err := d.WarnOutrankedBrokerEnvKeys(ctx); err != nil {
		t.Fatalf("WarnOutrankedBrokerEnvKeys: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "SHADOWED_KEY") {
		t.Errorf("startup warning did not name SHADOWED_KEY, which user scope now outranks.\nlog:\n%s", out)
	}
	if !strings.Contains(out, envScopeTestScopeID(t, store.ScopeRuntimeBroker)) {
		t.Errorf("startup warning did not name the broker ID, so an operator cannot tell which broker to look at.\nlog:\n%s", out)
	}
	if strings.Contains(out, "BROKER_ONLY_KEY") {
		t.Errorf("startup warning named BROKER_ONLY_KEY, which nothing shadows.\nlog:\n%s", out)
	}
}

// TestResolveEnvFromStorage_ScopePrecedence is the all-four-scopes row of
// acceptance criterion 18: one key defined in every storage scope, asserting
// which one wins.
//
// The contract, lowest precedence first:
//
//	runtime_broker  <  hub  <  project  <  user
//
// 🔴 "user wins" HERE MEANS user WINS AMONG THE FOUR STORAGE SCOPES. IT DOES
// NOT MEAN user IS THE TOP OF SCION'S SETTINGS STACK, AND THIS TEST MUST NOT BE
// CITED FOR THAT. Explicit agent config — which carries request and --config
// env — is seeded into ResolvedEnv first and storage fills only what it left
// absent, so config outranks all four scopes including user. That relation is
// not even a plain inequality: an EMPTY config value is a passthrough marker
// that deliberately yields to storage. See TestBuildEnvSources_
// ConfigOutranksStorageScopes for the config rung, and the settings-precedence
// reference doc for the templates, harness overrides, profiles and project
// annotations that sit in between.
//
// This test pins the WINNER. It does not pin the ORDER — `sp-rev2` showed by
// mutation that an all-four case survives both swapping two lower scopes and
// deleting one outright. The order is
// TestResolveEnvFromStorage_PairwisePrecedence.
//
// If the ordering is ever changed deliberately again, this test must be
// UPDATED, not deleted — it is the only place the intended winner is asserted
// end-to-end against real storage.
func TestResolveEnvFromStorage_ScopePrecedence(t *testing.T) {
	ctx := context.Background()
	d, _ := newEnvScopeDispatcher(t, "SHARED_KEY", map[string]string{
		store.ScopeHub:           "from-hub",
		store.ScopeProject:       "from-project",
		store.ScopeUser:          "from-user",
		store.ScopeRuntimeBroker: "from-broker",
	})

	resolved, err := d.resolveEnvFromStorage(ctx, envScopeTestAgent())
	if err != nil {
		t.Fatalf("resolveEnvFromStorage: %v", err)
	}
	if got, want := resolved["SHARED_KEY"], "from-user"; got != want {
		t.Errorf("SHARED_KEY resolved to %q, want %q (precedence runtime_broker < hub < project < user)", got, want)
	}
}

// TestResolveEnvFromStorage_PairwisePrecedence is acceptance criterion 18, and
// it seeds ONLY the two scopes named in each case, so every pair is
// discriminated DIRECTLY rather than held up by transitivity through the rest
// of the ladder. Four scopes, six unordered pairs, all six present.
//
// Why pairwise and not the all-four case: `sp-rev2` mutated the implementation
// and showed that "a key defined in all four scopes resolves to the scope the
// doc comment names" survives BOTH swapping user with project AND deleting user
// from the ladder outright. The all-four case pins the WINNER, not the ORDER —
// everything below the top scope is unconstrained by it. Only a two-scope
// fixture can fail when two scopes swap.
//
// The pairs are deliberately not limited to adjacent rungs. Transitivity is
// free only while the implementation is a total-order ladder, which
// envScopePrecedence is today; the criterion has to outlive that shape.
func TestResolveEnvFromStorage_PairwisePrecedence(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		// invertedByPhase10 marks the three pairs whose winner CHANGED when
		// runtime_broker was demoted to the bottom. Each was red before the
		// ordering commit and green after; the other three must not have moved.
		invertedByPhase10 bool
		values            map[string]string
		want              string
	}{
		{"project beats hub", false, map[string]string{store.ScopeHub: "from-hub", store.ScopeProject: "from-project"}, "from-project"},
		{"user beats hub", false, map[string]string{store.ScopeHub: "from-hub", store.ScopeUser: "from-user"}, "from-user"},
		{"user beats project", false, map[string]string{store.ScopeProject: "from-project", store.ScopeUser: "from-user"}, "from-user"},
		{"hub beats broker", true, map[string]string{store.ScopeHub: "from-hub", store.ScopeRuntimeBroker: "from-broker"}, "from-hub"},
		{"project beats broker", true, map[string]string{store.ScopeProject: "from-project", store.ScopeRuntimeBroker: "from-broker"}, "from-project"},
		{"user beats broker", true, map[string]string{store.ScopeUser: "from-user", store.ScopeRuntimeBroker: "from-broker"}, "from-user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newEnvScopeDispatcher(t, "SHARED_KEY", tc.values)
			resolved, err := d.resolveEnvFromStorage(ctx, envScopeTestAgent())
			if err != nil {
				t.Fatalf("resolveEnvFromStorage: %v", err)
			}
			if got := resolved["SHARED_KEY"]; got != tc.want {
				t.Errorf("SHARED_KEY resolved to %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBuildEnvSources_ReportsBrokerScope covers the provenance reporter's
// blind spot: a key defined ONLY in runtime_broker scope must be reported with
// source "broker", not blank.
func TestBuildEnvSources_ReportsBrokerScope(t *testing.T) {
	ctx := context.Background()
	d, _ := newEnvScopeDispatcher(t, "BROKER_ONLY_KEY", map[string]string{
		store.ScopeRuntimeBroker: "from-broker",
	})
	agent := envScopeTestAgent()

	resolved, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		t.Fatalf("resolveEnvFromStorage: %v", err)
	}
	if got, want := resolved["BROKER_ONLY_KEY"], "from-broker"; got != want {
		t.Fatalf("precondition: BROKER_ONLY_KEY resolved to %q, want %q", got, want)
	}

	sources := d.buildEnvSources(ctx, agent, resolved)
	if got, want := sources["BROKER_ONLY_KEY"], "broker"; got != want {
		t.Errorf("buildEnvSources reported source %q for BROKER_ONLY_KEY, want %q", got, want)
	}
}

// TestEnvSources_AgreesWithResolver is a DRIFT GUARD. IT IS NOT A CORRECTNESS
// CHECK, AND IT MUST NEVER BE COUNTED AS ONE.
//
// For every subset of scopes that may define a key, the source reported by
// buildEnvSources must be the scope the winning value actually came from in
// resolveEnvFromStorage. But it derives its expectation FROM the resolved
// value, so resolver and reporter stay in agreement even when both are wrong
// together — design Class F. `sp-rev2` confirmed this by mutation: all fifteen
// subsets survived an ordering change that a correctness test must catch.
//
// It guards even LESS since the ordering was extracted into
// envScopesInPrecedenceOrder, because both sides now range over the same list:
// the drift it was written to detect is close to structurally impossible. That
// is a reason to keep it cheap and to be honest about what it buys, not a
// reason to delete it — it still catches a reporter that stops consulting the
// shared helper, mislabels a scope, or applies the config rung in the wrong
// place, none of which the ordering extraction prevents.
//
// The correctness of the ORDER is TestResolveEnvFromStorage_PairwisePrecedence
// (criterion 18). If you are looking for the test that would fail if the ladder
// were wrong, it is that one, not this one.
func TestEnvSources_AgreesWithResolver(t *testing.T) {
	ctx := context.Background()

	// value written in each scope -> source label buildEnvSources must report.
	sourceForValue := map[string]string{
		"from-hub":     "hub",
		"from-project": "project",
		"from-user":    "user",
		"from-broker":  "broker",
	}
	valueForScope := map[string]string{
		store.ScopeHub:           "from-hub",
		store.ScopeProject:       "from-project",
		store.ScopeUser:          "from-user",
		store.ScopeRuntimeBroker: "from-broker",
	}
	allScopes := []string{store.ScopeHub, store.ScopeProject, store.ScopeUser, store.ScopeRuntimeBroker}

	// Enumerate every non-empty subset of the four scopes (15 cases).
	for mask := 1; mask < 1<<len(allScopes); mask++ {
		values := make(map[string]string)
		name := ""
		for i, scope := range allScopes {
			if mask&(1<<i) != 0 {
				values[scope] = valueForScope[scope]
				if name != "" {
					name += "+"
				}
				name += scope
			}
		}
		t.Run(name, func(t *testing.T) {
			d, _ := newEnvScopeDispatcher(t, "SHARED_KEY", values)
			agent := envScopeTestAgent()

			resolved, err := d.resolveEnvFromStorage(ctx, agent)
			if err != nil {
				t.Fatalf("resolveEnvFromStorage: %v", err)
			}
			winner, ok := resolved["SHARED_KEY"]
			if !ok {
				t.Fatalf("precondition: SHARED_KEY missing from resolved env")
			}
			wantSource, ok := sourceForValue[winner]
			if !ok {
				t.Fatalf("unexpected resolved value %q", winner)
			}

			sources := d.buildEnvSources(ctx, agent, resolved)
			if got := sources["SHARED_KEY"]; got != wantSource {
				t.Errorf("buildEnvSources reported source %q, but the value %q came from scope %q",
					got, winner, wantSource)
			}
		})
	}
}

// TestBuildEnvSources_ConfigOutranksStorageScopes pins the reporter's existing
// behaviour for agent-config env: explicit agent config outranks all four
// storage scopes, so config is what gets reported.
func TestBuildEnvSources_ConfigOutranksStorageScopes(t *testing.T) {
	ctx := context.Background()
	d, _ := newEnvScopeDispatcher(t, "SHARED_KEY", map[string]string{
		store.ScopeHub:           "from-hub",
		store.ScopeProject:       "from-project",
		store.ScopeUser:          "from-user",
		store.ScopeRuntimeBroker: "from-broker",
	})
	agent := envScopeTestAgent()
	agent.AppliedConfig = &store.AgentAppliedConfig{Env: map[string]string{"SHARED_KEY": "from-config"}}

	resolved := map[string]string{"SHARED_KEY": "from-config"}
	sources := d.buildEnvSources(ctx, agent, resolved)
	if got, want := sources["SHARED_KEY"], "config"; got != want {
		t.Errorf("buildEnvSources reported source %q, want %q", got, want)
	}
}

// TestBuildEnvSources_SkipsKeysNotInResolvedEnv pins that the reporter only
// labels keys that actually made it into the resolved env.
func TestBuildEnvSources_SkipsKeysNotInResolvedEnv(t *testing.T) {
	ctx := context.Background()
	d, _ := newEnvScopeDispatcher(t, "UNUSED_KEY", map[string]string{
		store.ScopeHub:           "from-hub",
		store.ScopeRuntimeBroker: "from-broker",
	})
	sources := d.buildEnvSources(ctx, envScopeTestAgent(), map[string]string{})
	if len(sources) != 0 {
		t.Errorf("buildEnvSources returned %v, want no entries", sources)
	}
}

// TestResolveEnvFromStorage_SkipsInapplicableScopes pins that scopes whose
// scope ID is empty on the agent contribute nothing and cause no error.
func TestResolveEnvFromStorage_SkipsInapplicableScopes(t *testing.T) {
	ctx := context.Background()
	d, _ := newEnvScopeDispatcher(t, "SHARED_KEY", map[string]string{
		store.ScopeHub:           "from-hub",
		store.ScopeProject:       "from-project",
		store.ScopeUser:          "from-user",
		store.ScopeRuntimeBroker: "from-broker",
	})

	agent := envScopeTestAgent()
	agent.OwnerID = ""
	agent.RuntimeBrokerID = ""

	resolved, err := d.resolveEnvFromStorage(ctx, agent)
	if err != nil {
		t.Fatalf("resolveEnvFromStorage: %v", err)
	}
	if got, want := resolved["SHARED_KEY"], "from-project"; got != want {
		t.Errorf("SHARED_KEY resolved to %q, want %q", got, want)
	}

	sources := d.buildEnvSources(ctx, agent, resolved)
	if got, want := sources["SHARED_KEY"], "project"; got != want {
		t.Errorf("buildEnvSources reported source %q, want %q", got, want)
	}
}
