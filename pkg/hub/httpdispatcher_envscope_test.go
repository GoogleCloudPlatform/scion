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
	"context"
	"log/slog"
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
			ID:      api.NewUUID(),
			Key:     key,
			Value:   value,
			Scope:   scope,
			ScopeID: envScopeTestScopeID(t, scope),
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
		{Scope: store.ScopeHub, ScopeID: envScopeTestHubID},
		{Scope: store.ScopeProject, ScopeID: "project-envscope-1"},
		{Scope: store.ScopeUser, ScopeID: "user-envscope-1"},
		{Scope: store.ScopeRuntimeBroker, ScopeID: "broker-envscope-1"},
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

// TestResolveEnvFromStorage_ScopePrecedence is the executable statement of the
// hub env-var scope precedence contract: one key defined in all four scopes,
// asserting which scope wins.
//
// The contract, lowest precedence first:
//
//	hub  <  project  <  user  <  runtime_broker
//
// If the ordering is ever changed deliberately (see design §3.4 variant 4-B),
// this test must be UPDATED, not deleted — it is the only place the intended
// winner is asserted end-to-end against real storage.
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
	if got, want := resolved["SHARED_KEY"], "from-broker"; got != want {
		t.Errorf("SHARED_KEY resolved to %q, want %q (precedence hub < project < user < runtime_broker)", got, want)
	}
}

// TestResolveEnvFromStorage_PairwisePrecedence pins each adjacent rung of the
// precedence ladder independently, so a reordering cannot be masked by the
// all-four-scopes case alone.
func TestResolveEnvFromStorage_PairwisePrecedence(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		values map[string]string
		want   string
	}{
		{"project beats hub", map[string]string{store.ScopeHub: "from-hub", store.ScopeProject: "from-project"}, "from-project"},
		{"user beats project", map[string]string{store.ScopeProject: "from-project", store.ScopeUser: "from-user"}, "from-user"},
		{"broker beats user", map[string]string{store.ScopeUser: "from-user", store.ScopeRuntimeBroker: "from-broker"}, "from-broker"},
		{"broker beats hub", map[string]string{store.ScopeHub: "from-hub", store.ScopeRuntimeBroker: "from-broker"}, "from-broker"},
		{"user beats hub", map[string]string{store.ScopeHub: "from-hub", store.ScopeUser: "from-user"}, "from-user"},
		{"broker beats project", map[string]string{store.ScopeProject: "from-project", store.ScopeRuntimeBroker: "from-broker"}, "from-broker"},
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

// TestEnvSources_AgreesWithResolver is the anti-drift test: for every subset of
// scopes that may define a key, the source reported by buildEnvSources must be
// the scope the winning value actually came from in resolveEnvFromStorage.
//
// It derives the expected source from the resolved VALUE rather than from a
// hard-coded ordering, so it keeps holding if the ordering is deliberately
// changed — the two functions are required to agree, whatever the order is.
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
