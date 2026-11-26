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

package secret

import (
	"context"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestShadowedKeys(t *testing.T) {
	var sk ShadowedKeys
	sk.Record("GITHUB_TOKEN", 4) // agent scope
	sk.Record("PROJ_OD", 2)      // project scope

	// Same name at lower scope -> shadowed.
	if !sk.ShadowsScope(SecretMeta{Name: "GITHUB_TOKEN", SecretType: TypeEnvironment}, 2) {
		t.Error("expected same-name lower-scope env secret to be shadowed")
	}
	// Different name, same effective env target at lower scope -> shadowed.
	if !sk.ShadowsScope(SecretMeta{Name: "PROJ_GH", Target: "GITHUB_TOKEN", SecretType: TypeEnvironment}, 2) {
		t.Error("expected different-name same-target lower-scope env secret to be shadowed")
	}
	// File secret with target equal to shadowed key (different name) -> NOT shadowed by target.
	if sk.ShadowsScope(SecretMeta{Name: "SOME_FILE", Target: "GITHUB_TOKEN", SecretType: TypeFile}, 2) {
		t.Error("file secret must not be shadowed by env target when name differs")
	}
	// File secret with same name at lower scope -> shadowed by name.
	if !sk.ShadowsScope(SecretMeta{Name: "GITHUB_TOKEN", Target: "/etc/token", SecretType: TypeFile}, 2) {
		t.Error("file secret with same name at lower scope must be shadowed")
	}
	// Higher-precedence deliverable over lower-precedence shadowed -> NOT shadowed.
	if sk.ShadowsScope(SecretMeta{Name: "USER_OVERRIDE", Target: "PROJ_OD", SecretType: TypeEnvironment}, 3) {
		t.Error("higher-scope deliverable must not be shadowed by lower-scope on_demand")
	}
	// Progeny candidate matching by name OR by effective env target -> shadowed.
	if !sk.ShadowsProgeny(SecretMeta{Name: "GITHUB_TOKEN", SecretType: TypeEnvironment}) {
		t.Error("progeny same-name candidate must be shadowed")
	}
	if !sk.ShadowsProgeny(SecretMeta{Name: "PARENT_GH", Target: "GITHUB_TOKEN", SecretType: TypeEnvironment}) {
		t.Error("progeny different-name same-target candidate must be shadowed")
	}
	if sk.ShadowsProgeny(SecretMeta{Name: "UNRELATED", Target: "UNRELATED", SecretType: TypeEnvironment}) {
		t.Error("unrelated progeny candidate must not be shadowed")
	}
}

type spySecretValueStore struct {
	store.SecretStore
	mu          sync.Mutex
	fetchedKeys []string
}

func (s *spySecretValueStore) GetSecretValue(ctx context.Context, key, scope, scopeID string) (string, error) {
	s.mu.Lock()
	s.fetchedKeys = append(s.fetchedKeys, scope+"/"+key)
	s.mu.Unlock()
	return s.SecretStore.GetSecretValue(ctx, key, scope, scopeID)
}

func (s *spySecretValueStore) fetched(scope, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := scope + "/" + key
	for _, k := range s.fetchedKeys {
		if k == want {
			return true
		}
	}
	return false
}

func runOnDemandResolveShadowingCases(t *testing.T, newBackend func(t *testing.T) (hubScopeResolveBackend, func(scope, key string) bool)) {
	t.Helper()
	ctx := context.Background()
	b, wasFetched := newBackend(t)

	const (
		brokerID  = "broker-shadow"
		projectID = "project-shadow"
		userID    = "user-shadow"
		parentID  = "parent-user-shadow"
		agentID   = "agent-shadow"
	)

	inputs := []*SetSecretInput{
		// 1. Agent-scope on_demand GITHUB_TOKEN shadows:
		//    - project env secret with same name (GITHUB_TOKEN)
		//    - project env secret with different name but target=GITHUB_TOKEN (PROJ_GH)
		//    - progeny env secret with different name and target=GITHUB_TOKEN (PARENT_GH)
		{Name: "GITHUB_TOKEN", Value: "proj-same-name", SecretType: TypeEnvironment, Target: "GITHUB_TOKEN", Scope: ScopeProject, ScopeID: projectID, InjectionMode: store.InjectionModeAlways},
		{Name: "PROJ_GH", Value: "proj-diff-name", SecretType: TypeEnvironment, Target: "GITHUB_TOKEN", Scope: ScopeProject, ScopeID: projectID, InjectionMode: store.InjectionModeAlways},
		{Name: "PARENT_GH", Value: "progeny-diff-name", SecretType: TypeEnvironment, Target: "GITHUB_TOKEN", Scope: ScopeUser, ScopeID: parentID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true, CreatedBy: parentID},
		{Name: "GITHUB_TOKEN", Value: "agent-od-secret", SecretType: TypeOnDemand, Target: "GITHUB_TOKEN", Scope: ScopeAgent, ScopeID: agentID, InjectionMode: store.InjectionModeAsNeeded},

		// 2. Higher-scope deliverable (user env) wins over lower-scope (project) on_demand.
		{Name: "PROJ_OD_WIN", Value: "proj-od-val", SecretType: TypeOnDemand, Target: "PROJ_OD_WIN", Scope: ScopeProject, ScopeID: projectID, InjectionMode: store.InjectionModeAsNeeded},
		{Name: "USER_WINS_TARGET", Value: "user-env-val", SecretType: TypeEnvironment, Target: "PROJ_OD_WIN", Scope: ScopeUser, ScopeID: userID, InjectionMode: store.InjectionModeAlways},

		// 3. Active controls: unshadowed project and progeny env secrets are delivered.
		{Name: "CTRL_PROJ", Value: "ctrl-proj-val", SecretType: TypeEnvironment, Target: "CTRL_PROJ", Scope: ScopeProject, ScopeID: projectID, InjectionMode: store.InjectionModeAlways},
		{Name: "CTRL_PROGENY", Value: "ctrl-progeny-val", SecretType: TypeEnvironment, Target: "CTRL_PROGENY", Scope: ScopeUser, ScopeID: parentID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true, CreatedBy: parentID},

		// 4. Internal row does NOT shadow lower-scope deliverable row.
		{Name: "INT_NO_SHADOW", Value: "broker-val", SecretType: TypeEnvironment, Target: "INT_NO_SHADOW", Scope: ScopeRuntimeBroker, ScopeID: brokerID, InjectionMode: store.InjectionModeAlways},
		{Name: "INT_NO_SHADOW", Value: "hub-internal", SecretType: store.SecretTypeInternal, Target: "INT_NO_SHADOW", Scope: ScopeHub, ScopeID: "test-hub-id", InjectionMode: store.InjectionModeAlways},
	}
	for _, in := range inputs {
		if _, _, err := b.Set(ctx, in); err != nil {
			t.Fatalf("Set %s/%s: %v", in.Scope, in.Name, err)
		}
	}

	opts := &ResolveOpts{
		AgentID:       agentID,
		AgentAncestry: []string{parentID},
		AuthzCheck:    func(SecretMeta) bool { return true },
	}
	resolved, err := b.Resolve(ctx, userID, projectID, brokerID, opts)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	got := make(map[string]SecretWithValue, len(resolved))
	for _, sv := range resolved {
		got[sv.Name] = sv
	}

	for _, forbidden := range []string{"GITHUB_TOKEN", "PROJ_GH", "PARENT_GH", "PROJ_OD_WIN"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("%s was delivered, want excluded/shadowed", forbidden)
		}
	}
	if sv, ok := got["USER_WINS_TARGET"]; !ok || sv.Value != "user-env-val" {
		t.Errorf("USER_WINS_TARGET = %+v (present=%v), want higher-scope user env delivered", sv.SecretMeta, ok)
	}
	if sv, ok := got["CTRL_PROJ"]; !ok || sv.Value != "ctrl-proj-val" {
		t.Errorf("CTRL_PROJ = %+v (present=%v), want delivered", sv.SecretMeta, ok)
	}
	if sv, ok := got["CTRL_PROGENY"]; !ok || sv.Value != "ctrl-progeny-val" {
		t.Errorf("CTRL_PROGENY = %+v (present=%v), want delivered", sv.SecretMeta, ok)
	}
	if sv, ok := got["INT_NO_SHADOW"]; !ok || sv.Scope != ScopeRuntimeBroker || sv.Value != "broker-val" {
		t.Errorf("INT_NO_SHADOW = %+v (present=%v), want broker row unshadowed by internal", sv.SecretMeta, ok)
	}

	if wasFetched != nil {
		for _, pair := range [][2]string{
			{ScopeAgent, "GITHUB_TOKEN"},
			{ScopeProject, "GITHUB_TOKEN"},
			{ScopeProject, "PROJ_GH"},
			{ScopeUser, "PARENT_GH"},
			{ScopeProject, "PROJ_OD_WIN"},
		} {
			if wasFetched(pair[0], pair[1]) {
				t.Errorf("value for %s/%s was fetched, want never fetched", pair[0], pair[1])
			}
		}
	}
}

func TestLocalBackend_ResolveOnDemandExcludesAndShadowsByTarget(t *testing.T) {
	runOnDemandResolveShadowingCases(t, func(t *testing.T) (hubScopeResolveBackend, func(scope, key string) bool) {
		_, baseStore := createTestBackend(t)
		spy := &spySecretValueStore{SecretStore: baseStore}
		b := NewLocalBackend(spy, "test-hub-id", "test-signing-key-32-bytes-long!!")
		return b, spy.fetched
	})
}

func TestGCPBackend_ResolveOnDemandExcludesAndShadowsByTarget(t *testing.T) {
	runOnDemandResolveShadowingCases(t, func(t *testing.T) (hubScopeResolveBackend, func(scope, key string) bool) {
		b, mock := createTestGCPBackend(t)
		scopeIDFor := map[string]string{
			ScopeRuntimeBroker: "broker-shadow",
			ScopeHub:           "test-hub-id",
			ScopeProject:       "project-shadow",
			ScopeUser:          "parent-user-shadow",
			ScopeAgent:         "agent-shadow",
		}
		wasFetched := func(scope, key string) bool {
			wantName := b.smSecretPath(b.gcpSecretName(key, scope, scopeIDFor[scope]))
			mock.mu.Lock()
			defer mock.mu.Unlock()
			for _, accessed := range mock.accessedNames {
				if accessed == wantName {
					return true
				}
			}
			return false
		}
		return b, wasFetched
	})
}

type listHookSecretStore struct {
	store.SecretStore
	onListScope   func(ctx context.Context, filter store.SecretFilter)
	onListProgeny func(ctx context.Context)
}

func (s *listHookSecretStore) ListSecrets(ctx context.Context, filter store.SecretFilter) ([]store.Secret, error) {
	rows, err := s.SecretStore.ListSecrets(ctx, filter)
	if err == nil && s.onListScope != nil {
		s.onListScope(ctx, filter)
	}
	return rows, err
}

func (s *listHookSecretStore) ListProgenySecrets(ctx context.Context, ancestry []string) ([]store.Secret, error) {
	rows, err := s.SecretStore.ListProgenySecrets(ctx, ancestry)
	if err == nil && s.onListProgeny != nil {
		s.onListProgeny(ctx)
	}
	return rows, err
}

// runResolveTypeFlipRaceCases verifies security finding L1: if an environment
// secret (at project scope or progeny) is replaced with on_demand between
// metadata listing and value read, Resolve skips it rather than delivering the
// new on_demand value under the stale listed type.
func runResolveTypeFlipRaceCases(t *testing.T, newBackend func(t *testing.T) (hubScopeResolveBackend, *listHookSecretStore)) {
	t.Helper()
	ctx := context.Background()
	b, hook := newBackend(t)

	const (
		projectID = "proj-l1-race"
		parentID  = "parent-l1-race"
	)

	for _, in := range []*SetSecretInput{
		{Name: "GITHUB_TOKEN", Value: "OLD-ENV", SecretType: TypeEnvironment, Target: "GITHUB_TOKEN", Scope: ScopeProject, ScopeID: projectID, InjectionMode: store.InjectionModeAlways},
		{Name: "CTRL_ENV", Value: "ctrl-val", SecretType: TypeEnvironment, Target: "CTRL_ENV", Scope: ScopeProject, ScopeID: projectID, InjectionMode: store.InjectionModeAlways},
		{Name: "PROGENY_FLIP", Value: "OLD-PROGENY", SecretType: TypeEnvironment, Target: "PROGENY_FLIP", Scope: ScopeUser, ScopeID: parentID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true, CreatedBy: parentID},
		{Name: "CTRL_PROGENY", Value: "ctrl-progeny-val", SecretType: TypeEnvironment, Target: "CTRL_PROGENY", Scope: ScopeUser, ScopeID: parentID, InjectionMode: store.InjectionModeAlways, AllowProgeny: true, CreatedBy: parentID},
	} {
		if _, _, err := b.Set(ctx, in); err != nil {
			t.Fatalf("Set %s/%s: %v", in.Scope, in.Name, err)
		}
	}

	var scopeFired, progenyFired bool
	hook.onListScope = func(ctx context.Context, filter store.SecretFilter) {
		if filter.Scope == ScopeProject && !scopeFired {
			scopeFired = true
			if _, _, err := b.Set(ctx, &SetSecretInput{
				Name: "GITHUB_TOKEN", Value: "NEW-ONDEMAND", SecretType: TypeOnDemand, Target: "GITHUB_TOKEN",
				Scope: ScopeProject, ScopeID: projectID, InjectionMode: store.InjectionModeAsNeeded,
			}); err != nil {
				t.Errorf("hook Set project GITHUB_TOKEN: %v", err)
			}
		}
	}
	hook.onListProgeny = func(ctx context.Context) {
		if !progenyFired {
			progenyFired = true
			if _, _, err := b.Set(ctx, &SetSecretInput{
				Name: "PROGENY_FLIP", Value: "NEW-PROGENY-ONDEMAND", SecretType: TypeOnDemand, Target: "PROGENY_FLIP",
				Scope: ScopeUser, ScopeID: parentID, InjectionMode: store.InjectionModeAsNeeded,
				AllowProgeny: true, CreatedBy: parentID,
			}); err != nil {
				t.Errorf("hook Set progeny PROGENY_FLIP: %v", err)
			}
		}
	}

	resolved, err := b.Resolve(ctx, "", projectID, "", &ResolveOpts{
		AgentAncestry: []string{parentID},
		AuthzCheck:    func(SecretMeta) bool { return true },
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !scopeFired || !progenyFired {
		t.Fatalf("expected both hooks to fire (scope=%v progeny=%v)", scopeFired, progenyFired)
	}

	got := make(map[string]SecretWithValue, len(resolved))
	for _, sv := range resolved {
		got[sv.Name] = sv
	}
	if sv, ok := got["GITHUB_TOKEN"]; ok {
		t.Errorf("GITHUB_TOKEN was delivered after flip to on_demand: type=%q value=%q", sv.SecretType, sv.Value)
	}
	if sv, ok := got["PROGENY_FLIP"]; ok {
		t.Errorf("PROGENY_FLIP was delivered after flip to on_demand: type=%q value=%q", sv.SecretType, sv.Value)
	}
	if sv, ok := got["CTRL_ENV"]; !ok || sv.Value != "ctrl-val" {
		t.Errorf("CTRL_ENV = %+v (present=%v), want delivered", sv.SecretMeta, ok)
	}
	if sv, ok := got["CTRL_PROGENY"]; !ok || sv.Value != "ctrl-progeny-val" {
		t.Errorf("CTRL_PROGENY = %+v (present=%v), want delivered", sv.SecretMeta, ok)
	}
}

func TestLocalBackend_ResolveSkipsRowChangedToOnDemandAfterList(t *testing.T) {
	runResolveTypeFlipRaceCases(t, func(t *testing.T) (hubScopeResolveBackend, *listHookSecretStore) {
		_, baseStore := createTestBackend(t)
		hook := &listHookSecretStore{SecretStore: baseStore}
		b := NewLocalBackend(hook, "test-hub-id", "test-signing-key-32-bytes-long!!")
		return b, hook
	})
}

func TestGCPBackend_ResolveSkipsRowChangedToOnDemandAfterList(t *testing.T) {
	runResolveTypeFlipRaceCases(t, func(t *testing.T) (hubScopeResolveBackend, *listHookSecretStore) {
		base, _ := createTestGCPBackend(t)
		hook := &listHookSecretStore{SecretStore: base.store}
		base.store = hook
		return base, hook
	})
}
