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

package hub

import (
	"context"
	"testing"
)

// Compile-time check: *FederatedAgentIdentity must satisfy AgentIdentity.
var _ AgentIdentity = (*FederatedAgentIdentity)(nil)

func TestFederatedAgentIdentity_ID(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub-a.example.com", "agent-123", "proj-456", "my-agent", "user-root",
		[]string{"user-root", "parent-agent"}, []AgentTokenScope{ScopeAgentStatusUpdate},
	)

	want := "https://hub-a.example.com:agent-123"
	if got := f.ID(); got != want {
		t.Errorf("ID() = %q, want %q", got, want)
	}
}

func TestFederatedAgentIdentity_Type(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub.example.com", "agent-1", "proj-1", "agent", "user-1",
		nil, nil,
	)

	if got := f.Type(); got != "federated_agent" {
		t.Errorf("Type() = %q, want %q", got, "federated_agent")
	}
}

func TestFederatedAgentIdentity_ProjectID(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub.example.com", "agent-1", "proj-1", "agent", "user-1",
		nil, nil,
	)

	if got := f.ProjectID(); got != "" {
		t.Errorf("ProjectID() = %q, want empty string", got)
	}
}

func TestFederatedAgentIdentity_RemoteProjectID(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub.example.com", "agent-1", "proj-remote", "agent", "user-1",
		nil, nil,
	)

	if got := f.RemoteProjectID(); got != "proj-remote" {
		t.Errorf("RemoteProjectID() = %q, want %q", got, "proj-remote")
	}
}

func TestFederatedAgentIdentity_HasScope(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub.example.com", "agent-1", "proj-1", "agent", "user-1",
		nil, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentLogAppend},
	)

	tests := []struct {
		scope AgentTokenScope
		want  bool
	}{
		{ScopeAgentStatusUpdate, true},
		{ScopeAgentLogAppend, true},
		{ScopeProjectSecretRead, false},
		{ScopeAgentCreate, false},
	}

	for _, tt := range tests {
		t.Run(string(tt.scope), func(t *testing.T) {
			if got := f.HasScope(tt.scope); got != tt.want {
				t.Errorf("HasScope(%q) = %v, want %v", tt.scope, got, tt.want)
			}
		})
	}
}

func TestFederatedAgentIdentity_Ancestry(t *testing.T) {
	ancestry := []string{"user-root", "parent-agent", "grandparent-agent"}
	f := NewFederatedAgentIdentity(
		"https://hub.example.com", "agent-1", "proj-1", "agent", "user-root",
		ancestry, nil,
	)

	got := f.Ancestry()
	if len(got) != len(ancestry) {
		t.Fatalf("Ancestry() returned %d elements, want %d", len(got), len(ancestry))
	}
	for i, v := range got {
		if v != ancestry[i] {
			t.Errorf("Ancestry()[%d] = %q, want %q", i, v, ancestry[i])
		}
	}
}

func TestFederatedAgentIdentity_OriginUserID(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub.example.com", "agent-1", "proj-1", "agent", "user-root",
		[]string{"user-root"}, nil,
	)

	if got := f.OriginUserID(); got != "user-root" {
		t.Errorf("OriginUserID() = %q, want %q", got, "user-root")
	}
}

func TestFederatedAgentIdentity_AgentName(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub.example.com", "agent-1", "proj-1", "my-cool-agent", "user-1",
		nil, nil,
	)

	if got := f.AgentName(); got != "my-cool-agent" {
		t.Errorf("AgentName() = %q, want %q", got, "my-cool-agent")
	}
}

func TestFederatedAgentIdentity_ContextIntegration(t *testing.T) {
	f := NewFederatedAgentIdentity(
		"https://hub-a.example.com", "agent-42", "proj-99", "test-agent", "user-origin",
		[]string{"user-origin"}, []AgentTokenScope{ScopeAgentStatusUpdate},
	)

	ctx := context.Background()
	ctx = contextWithIdentity(ctx, f)

	// GetAgentIdentityFromContext should return the federated identity.
	agentID := GetAgentIdentityFromContext(ctx)
	if agentID == nil {
		t.Fatal("GetAgentIdentityFromContext() returned nil, expected FederatedAgentIdentity")
	}
	if agentID.ID() != "https://hub-a.example.com:agent-42" {
		t.Errorf("agentIdentity.ID() = %q, want %q", agentID.ID(), "https://hub-a.example.com:agent-42")
	}
	if agentID.Type() != "federated_agent" {
		t.Errorf("agentIdentity.Type() = %q, want %q", agentID.Type(), "federated_agent")
	}

	// GetUserIdentityFromContext should return nil.
	userID := GetUserIdentityFromContext(ctx)
	if userID != nil {
		t.Errorf("GetUserIdentityFromContext() = %v, want nil", userID)
	}

	// GetAgentFromContext should return nil (it only returns AgentTokenClaims).
	claims := GetAgentFromContext(ctx)
	if claims != nil {
		t.Errorf("GetAgentFromContext() = %v, want nil", claims)
	}
}
