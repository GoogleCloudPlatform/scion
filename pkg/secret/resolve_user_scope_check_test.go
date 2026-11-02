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
	"sort"
	"testing"
)

// userScopeCheckBackend is the subset of a backend the UserScopeCheck tests
// drive.
type userScopeCheckBackend interface {
	Set(ctx context.Context, input *SetSecretInput) (bool, *SecretMeta, error)
	Resolve(ctx context.Context, userID, projectID, brokerID string, opts *ResolveOpts) ([]SecretWithValue, error)
}

// runUserScopeCheckCases pins ResolveOpts.UserScopeCheck on one backend:
//   - it is called for the user-scope rows of userID only (not the project
//     rows, not the progeny rows);
//   - a row it rejects is skipped, so a lower-scope row with the same key
//     stays in effect, and a row with no lower-scope peer is absent;
//   - a nil check, or nil opts, leaves resolution unchanged.
func runUserScopeCheckCases(t *testing.T, newBackend func(t *testing.T) userScopeCheckBackend) {
	t.Helper()
	ctx := context.Background()
	seed := func(t *testing.T, b userScopeCheckBackend) {
		t.Helper()
		for _, in := range []*SetSecretInput{
			{Name: "SHARED_KEY", Value: "user-shared", SecretType: TypeEnvironment, Scope: ScopeUser, ScopeID: "owner-1", AllowProgeny: true, CreatedBy: "owner-1"},
			{Name: "SHARED_KEY", Value: "project-shared", SecretType: TypeEnvironment, Scope: ScopeProject, ScopeID: "project-1"},
			{Name: "PRIVATE_KEY", Value: "user-private", SecretType: TypeEnvironment, Scope: ScopeUser, ScopeID: "owner-1", CreatedBy: "owner-1"},
			{Name: "ONLY_USER_KEY", Value: "user-only", SecretType: TypeEnvironment, Scope: ScopeUser, ScopeID: "owner-1", AllowProgeny: true, CreatedBy: "owner-1"},
			{Name: "PROGENY_KEY", Value: "progeny", SecretType: TypeEnvironment, Scope: ScopeUser, ScopeID: "root-1", AllowProgeny: true, CreatedBy: "root-1"},
		} {
			if _, _, err := b.Set(ctx, in); err != nil {
				t.Fatalf("Set %s/%s: %v", in.Scope, in.Name, err)
			}
		}
	}
	values := func(resolved []SecretWithValue) map[string]string {
		out := make(map[string]string, len(resolved))
		for _, sv := range resolved {
			out[sv.Name] = sv.Value
		}
		return out
	}

	t.Run("rejecting shared rows", func(t *testing.T) {
		b := newBackend(t)
		seed(t, b)
		var seen []string
		opts := &ResolveOpts{
			AgentAncestry: []string{"root-1", "agent-1"},
			AuthzCheck:    func(SecretMeta) bool { return true },
			UserScopeCheck: func(m SecretMeta) bool {
				if m.Scope != ScopeUser || m.ScopeID != "owner-1" {
					t.Errorf("UserScopeCheck called for %s/%s/%s", m.Scope, m.ScopeID, m.Name)
				}
				seen = append(seen, m.Name)
				return !m.AllowProgeny
			},
		}
		resolved, err := b.Resolve(ctx, "owner-1", "project-1", "", opts)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		got := values(resolved)
		if got["SHARED_KEY"] != "project-shared" {
			t.Errorf("SHARED_KEY = %q, want the project row once the user row is rejected", got["SHARED_KEY"])
		}
		if got["PRIVATE_KEY"] != "user-private" {
			t.Errorf("PRIVATE_KEY = %q, want the accepted user row", got["PRIVATE_KEY"])
		}
		if _, ok := got["ONLY_USER_KEY"]; ok {
			t.Errorf("ONLY_USER_KEY resolved although its only row was rejected")
		}
		if got["PROGENY_KEY"] != "progeny" {
			t.Errorf("PROGENY_KEY = %q, want the progeny row (not subject to UserScopeCheck)", got["PROGENY_KEY"])
		}
		sort.Strings(seen)
		want := []string{"ONLY_USER_KEY", "PRIVATE_KEY", "SHARED_KEY"}
		if len(seen) != len(want) {
			t.Fatalf("UserScopeCheck saw %v, want %v", seen, want)
		}
		for i := range want {
			if seen[i] != want[i] {
				t.Fatalf("UserScopeCheck saw %v, want %v", seen, want)
			}
		}
	})

	t.Run("nil check and nil opts are unchanged", func(t *testing.T) {
		b := newBackend(t)
		seed(t, b)
		for name, opts := range map[string]*ResolveOpts{"nil opts": nil, "nil check": {}} {
			resolved, err := b.Resolve(ctx, "owner-1", "project-1", "", opts)
			if err != nil {
				t.Fatalf("%s: Resolve: %v", name, err)
			}
			got := values(resolved)
			if got["SHARED_KEY"] != "user-shared" || got["PRIVATE_KEY"] != "user-private" || got["ONLY_USER_KEY"] != "user-only" {
				t.Errorf("%s: user rows changed: %v", name, got)
			}
		}
	})
}

func TestLocalBackend_ResolveUserScopeCheck(t *testing.T) {
	runUserScopeCheckCases(t, func(t *testing.T) userScopeCheckBackend {
		b, _ := createTestBackend(t)
		return b
	})
}

func TestGCPBackend_ResolveUserScopeCheck(t *testing.T) {
	runUserScopeCheckCases(t, func(t *testing.T) userScopeCheckBackend {
		b, _ := createTestGCPBackend(t)
		return b
	})
}
