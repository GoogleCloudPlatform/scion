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
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// tzTestEnvVar seeds one TZ env var into the store.
func tzTestEnvVar(t *testing.T, s store.Store, v store.EnvVar) {
	t.Helper()
	v.ID = api.NewUUID()
	if v.Key == "" {
		v.Key = agentTZEnvKey
	}
	if v.InjectionMode == "" {
		v.InjectionMode = store.InjectionModeAlways
	}
	if _, err := s.UpsertEnvVar(context.Background(), &v); err != nil {
		t.Fatalf("seeding %s-scoped %s: %v", v.Scope, v.Key, err)
	}
}

func tzTestDispatcher(t *testing.T, hubDefault string) (*HTTPAgentDispatcher, store.Store) {
	t.Helper()
	d, s := newEnvScopeDispatcher(t, "UNUSED", nil)
	d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		return opsettings.AgentDefaultsSettings{DefaultTimezone: hubDefault}
	})
	return d, s
}

// TestResolveAgentTZ_StorageScopeOrder checks the storage rung follows
// envScopePrecedence (user > project > hub > broker) and reports the winning
// scope as the source.
func TestResolveAgentTZ_StorageScopeOrder(t *testing.T) {
	values := map[string]string{
		store.ScopeRuntimeBroker: "Europe/Berlin",
		store.ScopeHub:           "Europe/London",
		store.ScopeProject:       "America/New_York",
		store.ScopeUser:          "America/Denver",
	}
	wantSource := map[string]string{
		store.ScopeRuntimeBroker: TZSourceBroker,
		store.ScopeHub:           TZSourceHub,
		store.ScopeProject:       TZSourceProject,
		store.ScopeUser:          TZSourceUser,
	}
	// Seed the scopes cumulatively from lowest to highest; after each step
	// the newest (highest) scope must win.
	order := []string{store.ScopeRuntimeBroker, store.ScopeHub, store.ScopeProject, store.ScopeUser}
	d, s := tzTestDispatcher(t, "Asia/Tokyo")
	agent := envScopeTestAgent()
	for _, scope := range order {
		tzTestEnvVar(t, s, store.EnvVar{Value: values[scope], Scope: scope, ScopeID: envScopeTestScopeID(t, scope)})
		got := d.resolveAgentTZ(context.Background(), agent, false)
		want := agentTZ{TZ: values[scope], Source: wantSource[scope]}
		if got != want {
			t.Fatalf("after seeding %s: resolveAgentTZ() = %+v, want %+v", scope, got, want)
		}
	}
}

// TestResolveAgentTZ_ExplicitBeatsStorage checks rung 1 outranks the
// strongest storage scope and the hub default.
func TestResolveAgentTZ_ExplicitBeatsStorage(t *testing.T) {
	d, s := tzTestDispatcher(t, "Asia/Tokyo")
	tzTestEnvVar(t, s, store.EnvVar{Value: "America/Denver", Scope: store.ScopeUser, ScopeID: envScopeTestScopeID(t, store.ScopeUser)})
	agent := envScopeTestAgent()
	agent.AppliedConfig.ExplicitTimezone = "Europe/Paris"

	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Europe/Paris", Source: TZSourceExplicit}); got != want {
		t.Fatalf("resolveAgentTZ() = %+v, want %+v", got, want)
	}
	agent.AppliedConfig.ExplicitTimezoneLegacy = true
	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Europe/Paris", Source: TZSourceLegacy}); got != want {
		t.Fatalf("legacy: resolveAgentTZ() = %+v, want %+v", got, want)
	}
}

// TestResolveAgentTZ_SkipsAsNeededAndEmpty checks that as_needed storage
// entries are not a rung, and that an empty value at a higher scope does not
// shadow a non-empty value at a lower scope.
func TestResolveAgentTZ_SkipsAsNeededAndEmpty(t *testing.T) {
	d, s := tzTestDispatcher(t, "Asia/Tokyo")
	tzTestEnvVar(t, s, store.EnvVar{Value: "America/Denver", Scope: store.ScopeUser, ScopeID: envScopeTestScopeID(t, store.ScopeUser), InjectionMode: store.InjectionModeAsNeeded})
	tzTestEnvVar(t, s, store.EnvVar{Value: "", Scope: store.ScopeProject, ScopeID: envScopeTestScopeID(t, store.ScopeProject)})
	agent := envScopeTestAgent()

	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault}); got != want {
		t.Fatalf("no usable storage: resolveAgentTZ() = %+v, want %+v", got, want)
	}

	tzTestEnvVar(t, s, store.EnvVar{Value: "Europe/Berlin", Scope: store.ScopeRuntimeBroker, ScopeID: envScopeTestScopeID(t, store.ScopeRuntimeBroker)})
	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Europe/Berlin", Source: TZSourceBroker}); got != want {
		t.Fatalf("empty project value must not shadow broker: resolveAgentTZ() = %+v, want %+v", got, want)
	}
}

// TestResolveAgentTZ_OtherKeysIgnored checks that storage vars for other
// keys do not feed the TZ rung.
func TestResolveAgentTZ_OtherKeysIgnored(t *testing.T) {
	d, s := tzTestDispatcher(t, "")
	tzTestEnvVar(t, s, store.EnvVar{Key: "TZDIR", Value: "/usr/share/zoneinfo", Scope: store.ScopeUser, ScopeID: envScopeTestScopeID(t, store.ScopeUser)})
	agent := envScopeTestAgent()

	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "", Source: TZSourceNone}); got != want {
		t.Fatalf("resolveAgentTZ() = %+v, want %+v", got, want)
	}
}

// TestResolveAgentTZ_Progeny checks that an ancestor's allowProgeny
// user-scope TZ applies below every storage scope, labelled progeny.
func TestResolveAgentTZ_Progeny(t *testing.T) {
	d, s := tzTestDispatcher(t, "Asia/Tokyo")
	tzTestEnvVar(t, s, store.EnvVar{
		Value:        "Asia/Kathmandu",
		Scope:        store.ScopeUser,
		ScopeID:      "user-ancestor-1",
		AllowProgeny: true,
		CreatedBy:    "user-ancestor-1",
	})
	agent := envScopeTestAgent()
	agent.Ancestry = []string{"user-ancestor-1", "agent-parent-1"}

	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Asia/Kathmandu", Source: TZSourceProgeny}); got != want {
		t.Fatalf("progeny only: resolveAgentTZ() = %+v, want %+v", got, want)
	}

	tzTestEnvVar(t, s, store.EnvVar{Value: "Europe/Berlin", Scope: store.ScopeRuntimeBroker, ScopeID: envScopeTestScopeID(t, store.ScopeRuntimeBroker)})
	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Europe/Berlin", Source: TZSourceBroker}); got != want {
		t.Fatalf("broker beats progeny: resolveAgentTZ() = %+v, want %+v", got, want)
	}
}

// TestResolveAgentTZ_HubDefaultLiveAndNone checks that the hub default is
// read at resolve time (an edit reaches the next resolution) and that the
// chain ends in no TZ, or UTC for a gather answer.
func TestResolveAgentTZ_HubDefaultLiveAndNone(t *testing.T) {
	d, _ := newEnvScopeDispatcher(t, "UNUSED", nil)
	hubDefault := "Asia/Tokyo"
	d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		return opsettings.AgentDefaultsSettings{DefaultTimezone: hubDefault}
	})
	agent := envScopeTestAgent()
	ctx := context.Background()

	if got, want := d.resolveAgentTZ(ctx, agent, false), (agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault}); got != want {
		t.Fatalf("resolveAgentTZ() = %+v, want %+v", got, want)
	}
	hubDefault = "Asia/Kathmandu"
	if got, want := d.resolveAgentTZ(ctx, agent, false), (agentTZ{TZ: "Asia/Kathmandu", Source: TZSourceHubDefault}); got != want {
		t.Fatalf("after default edit: resolveAgentTZ() = %+v, want %+v", got, want)
	}
	hubDefault = ""
	if got, want := d.resolveAgentTZ(ctx, agent, false), (agentTZ{TZ: "", Source: TZSourceNone}); got != want {
		t.Fatalf("no rungs: resolveAgentTZ() = %+v, want %+v", got, want)
	}
	if got, want := d.resolveAgentTZ(ctx, agent, true), (agentTZ{TZ: "UTC", Source: TZSourceNone}); got != want {
		t.Fatalf("no rungs, gather answer: resolveAgentTZ() = %+v, want %+v", got, want)
	}

	// No provider at all (e.g. file-less test hubs) behaves like an empty default.
	bare, _ := newEnvScopeDispatcher(t, "UNUSED", nil)
	if got, want := bare.resolveAgentTZ(ctx, agent, false), (agentTZ{TZ: "", Source: TZSourceNone}); got != want {
		t.Fatalf("no provider: resolveAgentTZ() = %+v, want %+v", got, want)
	}
	if got, want := bare.resolveAgentTZ(ctx, nil, false), (agentTZ{TZ: "", Source: TZSourceNone}); got != want {
		t.Fatalf("nil agent: resolveAgentTZ() = %+v, want %+v", got, want)
	}
}

// TestExplicitTimezone_StoreRoundTrip checks the three fields survive an
// agent write and read through the SQLite store.
func TestExplicitTimezone_StoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := createTestStore(t)
	project := &store.Project{ID: api.NewUUID(), Name: "tz-rt", Slug: "tz-rt"}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Name:      "tz-rt",
		Slug:      "tz-rt",
		ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			ExplicitTimezone:         "Asia/Kathmandu",
			ExplicitTimezoneLegacy:   true,
			ExplicitTimezoneUnpinned: true,
		},
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	got, err := s.GetAgent(ctx, agent.ID)
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if got.AppliedConfig == nil {
		t.Fatal("AppliedConfig is nil after read")
	}
	if got.AppliedConfig.ExplicitTimezone != "Asia/Kathmandu" || !got.AppliedConfig.ExplicitTimezoneLegacy || !got.AppliedConfig.ExplicitTimezoneUnpinned {
		t.Fatalf("read back %+v; want the three timezone fields preserved", got.AppliedConfig)
	}
}

// TestResolveAgentTZ_ProfileIsNotARung checks a runtime-profile timezone
// never reaches the resolver, even with a profile provider set on the
// dispatcher. Task 13 (profile retirement) deletes this case together with
// SetProfileTimezoneProvider.
func TestResolveAgentTZ_ProfileIsNotARung(t *testing.T) {
	d, _ := tzTestDispatcher(t, "")
	d.SetProfileTimezoneProvider(func(string) string { return "Europe/Rome" })
	agent := envScopeTestAgent()
	agent.AppliedConfig = &store.AgentAppliedConfig{Profile: "default"}

	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "", Source: TZSourceNone}); got != want {
		t.Fatalf("resolveAgentTZ() = %+v, want %+v", got, want)
	}
}

// TestResolveAgentTZ_UnadoptedLegacyEnvTZ checks a TZ still sitting in the
// agent's env records is not a rung, and that resolving it unadopted logs a
// warning.
func TestResolveAgentTZ_UnadoptedLegacyEnvTZ(t *testing.T) {
	tests := []struct {
		name string
		ac   *store.AgentAppliedConfig
	}{
		{name: "env", ac: &store.AgentAppliedConfig{Env: map[string]string{"TZ": "Europe/Paris"}}},
		{name: "inline config env only", ac: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Env: map[string]string{"TZ": "Europe/Paris"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := tzTestDispatcher(t, "Asia/Tokyo")
			var buf bytes.Buffer
			d.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			agent := envScopeTestAgent()
			agent.AppliedConfig = tt.ac

			if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault}); got != want {
				t.Fatalf("resolveAgentTZ() = %+v, want %+v", got, want)
			}
			if !strings.Contains(buf.String(), "unadopted legacy TZ") {
				t.Fatalf("expected an unadopted legacy TZ warning, got log %q", buf.String())
			}
		})
	}

	// An adopted (pinned) agent logs nothing.
	d, _ := tzTestDispatcher(t, "")
	var buf bytes.Buffer
	d.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	agent := envScopeTestAgent()
	agent.AppliedConfig = &store.AgentAppliedConfig{ExplicitTimezone: "Europe/Paris", ExplicitTimezoneLegacy: true}
	if got, want := d.resolveAgentTZ(context.Background(), agent, false), (agentTZ{TZ: "Europe/Paris", Source: TZSourceLegacy}); got != want {
		t.Fatalf("pinned: resolveAgentTZ() = %+v, want %+v", got, want)
	}
	if buf.Len() != 0 {
		t.Fatalf("pinned agent logged %q", buf.String())
	}
}

// tzErrStore fails the env-var reads the TZ storage rung makes: ListEnvVars
// for the scopes in failScopes (every scope when failScopes is nil), and
// ListProgenyEnvVars always.
type tzErrStore struct {
	store.Store
	failScopes map[string]bool
}

func (s *tzErrStore) ListEnvVars(ctx context.Context, filter store.EnvVarFilter) ([]store.EnvVar, error) {
	if s.failScopes == nil || s.failScopes[filter.Scope] {
		return nil, errors.New("simulated env var read failure")
	}
	return s.Store.ListEnvVars(ctx, filter)
}

func (s *tzErrStore) ListProgenyEnvVars(context.Context, []string) ([]store.EnvVar, error) {
	return nil, errors.New("simulated progeny env var read failure")
}

// TestResolveAgentTZ_StoreErrorsWithNilLog checks a failed storage read is
// skipped, never a panic, on a dispatcher without a logger, and that the
// resolver falls through to the next rung.
func TestResolveAgentTZ_StoreErrorsWithNilLog(t *testing.T) {
	newAgent := func() *store.Agent {
		agent := envScopeTestAgent()
		// Ancestry of more than one entry makes the resolver read progeny vars.
		agent.Ancestry = []string{"user-ancestor-1", "agent-parent-1"}
		// An unadopted legacy env TZ exercises the third warning site.
		agent.AppliedConfig = &store.AgentAppliedConfig{Env: map[string]string{"TZ": "Europe/Paris"}}
		return agent
	}

	for _, tc := range []struct {
		name       string
		hubDefault string
		want       agentTZ
	}{
		{name: "falls through to the hub default", hubDefault: "Asia/Tokyo", want: agentTZ{TZ: "Asia/Tokyo", Source: TZSourceHubDefault}},
		{name: "falls through to none", want: agentTZ{Source: TZSourceNone}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, s := tzTestDispatcher(t, tc.hubDefault)
			tzTestEnvVar(t, s, store.EnvVar{Value: "Europe/Berlin", Scope: store.ScopeUser, ScopeID: envScopeTestScopeID(t, store.ScopeUser)})
			d.store = &tzErrStore{Store: s}
			d.log = nil
			if got := d.resolveAgentTZ(context.Background(), newAgent(), false); got != tc.want {
				t.Fatalf("resolveAgentTZ() = %+v, want %+v", got, tc.want)
			}
		})
	}

	// Scopes are walked broker, hub, project, user, and a later scope
	// overrides an earlier one. Failing a scope that is not the last one walked
	// shows the walk continues past the error rather than stopping there.
	for _, tc := range []struct {
		name    string
		project string
		user    string
		fail    string
		want    agentTZ
	}{
		{
			name:    "a failed project read is skipped and the user scope still wins",
			project: "Europe/Berlin", user: "America/Denver", fail: store.ScopeProject,
			want: agentTZ{TZ: "America/Denver", Source: TZSourceUser},
		},
		{
			name:    "a failed higher-precedence scope falls back to a lower one",
			project: "America/Denver", user: "Europe/Berlin", fail: store.ScopeUser,
			want: agentTZ{TZ: "America/Denver", Source: TZSourceProject},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, s := tzTestDispatcher(t, "Asia/Tokyo")
			tzTestEnvVar(t, s, store.EnvVar{Value: tc.project, Scope: store.ScopeProject, ScopeID: envScopeTestScopeID(t, store.ScopeProject)})
			tzTestEnvVar(t, s, store.EnvVar{Value: tc.user, Scope: store.ScopeUser, ScopeID: envScopeTestScopeID(t, store.ScopeUser)})
			d.store = &tzErrStore{Store: s, failScopes: map[string]bool{tc.fail: true}}
			d.log = nil
			if got := d.resolveAgentTZ(context.Background(), newAgent(), false); got != tc.want {
				t.Fatalf("resolveAgentTZ() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTZDropWarnings_NilLog checks the TZ-targeted secret and caller-env TZ
// drops skip the log, never a panic, on a dispatcher without a logger, and
// still drop TZ and relay the dispatch warning.
func TestTZDropWarnings_NilLog(t *testing.T) {
	d, _ := tzTestDispatcher(t, "")
	d.log = nil
	agent := envScopeTestAgent()

	t.Run("a secret that targets TZ is dropped", func(t *testing.T) {
		ctx, warns := withDispatchWarnings(context.Background())
		got := d.dropTZTargetedSecrets(ctx, agent, []ResolvedSecret{
			{Name: "tz", Type: "environment", Target: "TZ", Value: "Asia/Tokyo", Source: "user"},
			{Name: "keep", Type: "environment", Target: "API_KEY", Value: "x", Source: "user"},
		})
		if len(got) != 1 || got[0].Name != "keep" {
			t.Fatalf("dropTZTargetedSecrets() = %+v, want only the API_KEY secret", got)
		}
		if len(warns.Warnings()) != 1 {
			t.Fatalf("warnings = %q, want one", warns.Warnings())
		}
	})

	t.Run("TZ in the caller env is dropped", func(t *testing.T) {
		ctx, warns := withDispatchWarnings(context.Background())
		got := d.withoutCallerTZ(ctx, agent, map[string]string{"TZ": "Asia/Tokyo", "FOO": "bar"})
		if _, ok := got["TZ"]; ok || got["FOO"] != "bar" {
			t.Fatalf("withoutCallerTZ() = %v, want FOO only", got)
		}
		if len(warns.Warnings()) != 1 {
			t.Fatalf("warnings = %q, want one", warns.Warnings())
		}
	})
}
