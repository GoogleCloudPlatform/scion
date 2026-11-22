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
	"slices"
	"strings"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestScopePrecedence_AgentHighestAndUnknownZero(t *testing.T) {
	if got := scopePrecedence(ScopeRuntimeBroker); got != 1 {
		t.Errorf("broker precedence = %d, want 1", got)
	}
	if got := scopePrecedence(ScopeHub); got != 2 {
		t.Errorf("hub precedence = %d, want 2", got)
	}
	if got := scopePrecedence(ScopeProject); got != 3 {
		t.Errorf("project precedence = %d, want 3", got)
	}
	if got := scopePrecedence(ScopeUser); got != 4 {
		t.Errorf("user precedence = %d, want 4", got)
	}
	if got := scopePrecedence(ScopeAgent); got != 5 {
		t.Errorf("agent precedence = %d, want 5", got)
	}
	if got := scopePrecedence("unknown_future_scope"); got != 0 {
		t.Errorf("unknown scope precedence = %d, want 0", got)
	}
}

func TestDeduplicateByTarget_AgentWinsOverAllScopes(t *testing.T) {
	ascending := []SecretWithValue{
		{SecretMeta: SecretMeta{Name: "d-broker", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeRuntimeBroker}, Value: "broker"},
		{SecretMeta: SecretMeta{Name: "a-hub", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeHub}, Value: "hub"},
		{SecretMeta: SecretMeta{Name: "c-project", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeProject}, Value: "project"},
		{SecretMeta: SecretMeta{Name: "b-user", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeUser}, Value: "user"},
		{SecretMeta: SecretMeta{Name: "e-agent", SecretType: TypeEnvironment, Target: "SHARED_TARGET", Scope: ScopeAgent}, Value: "agent"},
	}
	descending := slices.Clone(ascending)
	slices.Reverse(descending)

	for _, tc := range []struct {
		name  string
		input []SecretWithValue
	}{
		{"lowest scope first", ascending},
		{"highest scope first", descending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DeduplicateByTarget(tc.input)
			if len(got) != 1 {
				t.Fatalf("expected one secret for shared target, got %d: %+v", len(got), got)
			}
			if got[0].Scope != ScopeAgent || got[0].Name != "e-agent" || got[0].Value != "agent" {
				t.Fatalf("expected agent-scope secret to win, got %s (%s)", got[0].Name, got[0].Scope)
			}
		})
	}
}

func TestLocalBackend_ResolveAgentScope(t *testing.T) {
	ctx := context.Background()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	backend := NewLocalBackend(s, "hub-1", "test-signing-key-32-bytes-long!!")

	const (
		userID    = "user-1"
		projectID = "project-1"
		brokerID  = "broker-1"
		agentX    = "agent-x"
		agentY    = "agent-y"
	)

	// Set SHARED at every lower scope and at agent X.
	for _, in := range []*SetSecretInput{
		{Name: "SHARED", Value: "broker-val", Scope: ScopeRuntimeBroker, ScopeID: brokerID},
		{Name: "SHARED", Value: "project-val", Scope: ScopeProject, ScopeID: projectID},
		{Name: "SHARED", Value: "user-val", Scope: ScopeUser, ScopeID: userID},
		{Name: "SHARED", Value: "agent-x-val", Scope: ScopeAgent, ScopeID: agentX, ProjectID: projectID},
		{Name: "ONLY_X", Value: "only-x-val", Scope: ScopeAgent, ScopeID: agentX, ProjectID: projectID},
		{Name: "ONLY_Y", Value: "only-y-val", Scope: ScopeAgent, ScopeID: agentY, ProjectID: projectID},
	} {
		if _, _, err := backend.Set(ctx, in); err != nil {
			t.Fatalf("Set(%s, %s): %v", in.Scope, in.Name, err)
		}
	}

	// Empty AgentID (nil opts or zero ResolveOpts): agent-scoped secrets are not included.
	for _, opts := range []*ResolveOpts{nil, {}} {
		resolved, err := backend.Resolve(ctx, userID, projectID, brokerID, opts)
		if err != nil {
			t.Fatalf("Resolve(empty AgentID): %v", err)
		}
		byName := make(map[string]SecretWithValue, len(resolved))
		for _, sv := range resolved {
			byName[sv.Name] = sv
		}
		if got := byName["SHARED"]; got.Value != "user-val" || got.Scope != ScopeUser {
			t.Fatalf("expected user-val when AgentID is empty, got %+v", got)
		}
		if _, exists := byName["ONLY_X"]; exists {
			t.Fatalf("ONLY_X must not be resolved when AgentID is empty")
		}
	}

	// Non-empty AgentID = agentX: agentX overrides user and excludes agentY.
	resolved, err := backend.Resolve(ctx, userID, projectID, brokerID, &ResolveOpts{AgentID: agentX})
	if err != nil {
		t.Fatalf("Resolve(agentX): %v", err)
	}
	byName := make(map[string]SecretWithValue, len(resolved))
	for _, sv := range resolved {
		byName[sv.Name] = sv
	}
	if got := byName["SHARED"]; got.Value != "agent-x-val" || got.Scope != ScopeAgent || got.ScopeID != agentX {
		t.Fatalf("expected agent-x-val for SHARED, got %+v", got)
	}
	if got := byName["ONLY_X"]; got.Value != "only-x-val" || got.Scope != ScopeAgent {
		t.Fatalf("expected only-x-val for ONLY_X, got %+v", got)
	}
	if _, exists := byName["ONLY_Y"]; exists {
		t.Fatalf("ONLY_Y must not be resolved for agentX")
	}
}

func TestGCPBackend_AgentScopeNamingLabelsResolveAndDelete(t *testing.T) {
	ctx := context.Background()
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	defer func() { _ = s.Close() }()
	mock := newMockSMClient()
	backend := NewGCPBackendWithClient(s, mock, "gcp-proj", "hub-1")
	backend.SetHubName("test-hub")

	const (
		userID    = "user-1"
		projectID = "proj-123"
		brokerID  = "broker-1"
		agentID   = "agent-abc"
	)

	// Store user and agent secrets with same key.
	if _, _, err := backend.Set(ctx, &SetSecretInput{
		Name: "API_TOKEN", Value: "user-token", Scope: ScopeUser, ScopeID: userID,
	}); err != nil {
		t.Fatalf("Set user secret: %v", err)
	}
	created, meta, err := backend.Set(ctx, &SetSecretInput{
		Name: "API_TOKEN", Value: "agent-token", Scope: ScopeAgent, ScopeID: agentID, ProjectID: projectID,
	})
	if err != nil {
		t.Fatalf("Set agent secret: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true")
	}

	// Verify hub-prefixed GCP SM naming and labels.
	expectedSMName := backend.gcpSecretName("API_TOKEN", ScopeAgent, agentID)
	if !strings.HasPrefix(expectedSMName, backend.secretNamePrefix()+"agent-") {
		t.Fatalf("unexpected GCP SM name: %q", expectedSMName)
	}
	fullName := "projects/gcp-proj/secrets/" + expectedSMName
	if meta.SecretRef != "gcpsm:"+fullName {
		t.Fatalf("SecretRef = %q, want %q", meta.SecretRef, "gcpsm:"+fullName)
	}
	smSecret, err := mock.GetSecret(ctx, &smpb.GetSecretRequest{Name: fullName})
	if err != nil {
		t.Fatalf("GetSecret(%s): %v", fullName, err)
	}
	if got := smSecret.Labels["scion-scope"]; got != "agent" {
		t.Errorf("scion-scope label = %q, want agent", got)
	}
	if got := smSecret.Labels["scion-scope-id"]; got != sanitizeLabel(agentID) {
		t.Errorf("scion-scope-id label = %q, want %q", got, sanitizeLabel(agentID))
	}
	if got := smSecret.Labels["scion-project-id"]; got != sanitizeLabel(projectID) {
		t.Errorf("scion-project-id label = %q, want %q", got, sanitizeLabel(projectID))
	}

	// Resolve without AgentID => user-token; with AgentID => agent-token.
	withoutAgent, err := backend.Resolve(ctx, userID, projectID, brokerID, nil)
	if err != nil {
		t.Fatalf("Resolve(nil): %v", err)
	}
	if len(withoutAgent) != 1 || withoutAgent[0].Value != "user-token" {
		t.Fatalf("Resolve(nil) = %+v, want user-token", withoutAgent)
	}
	withAgent, err := backend.Resolve(ctx, userID, projectID, brokerID, &ResolveOpts{AgentID: agentID})
	if err != nil {
		t.Fatalf("Resolve(agentID): %v", err)
	}
	if len(withAgent) != 1 || withAgent[0].Value != "agent-token" || withAgent[0].Scope != ScopeAgent {
		t.Fatalf("Resolve(agentID) = %+v, want agent-token at agent scope", withAgent)
	}

	// Delete removes both GCP SM secret and DB row.
	if err := backend.Delete(ctx, "API_TOKEN", ScopeAgent, agentID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := mock.GetSecret(ctx, &smpb.GetSecretRequest{Name: fullName}); err == nil {
		t.Fatalf("expected GCP SM secret %s to be deleted", fullName)
	}
	if _, err := s.GetSecret(ctx, "API_TOKEN", store.ScopeAgent, agentID); err != store.ErrNotFound {
		t.Fatalf("expected DB secret row to be deleted, got err=%v", err)
	}
}
