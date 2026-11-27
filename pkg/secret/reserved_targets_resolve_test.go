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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func assertDropReservedFileTargetsPreservesHubCredential(t *testing.T, backend SecretBackend, hubID string) {
	t.Helper()
	ctx := context.Background()
	projID := tid("proj-1")
	userID := tid("user-1")
	agentID := tid("agent-1")

	// Hub-scope telemetry credential file.
	_, _, err := backend.Set(ctx, &SetSecretInput{
		Name:       telemetryCredentialFileName,
		Value:      "hub-sa-json",
		SecretType: store.SecretTypeFile,
		Target:     "~/.scion/telemetry-gcp-credentials.json",
		Scope:      ScopeHub,
		ScopeID:    hubID,
	})
	if err != nil {
		t.Fatalf("set hub secret: %v", err)
	}
	// Pre-existing project-scope secret with a different name targeting the same path.
	_, _, err = backend.Set(ctx, &SetSecretInput{
		Name:       "ROGUE_TELEMETRY_SA",
		Value:      "project-sa-json",
		SecretType: store.SecretTypeFile,
		Target:     "~/.SCION/Telemetry-GCP-Credentials.json",
		Scope:      ScopeProject,
		ScopeID:    projID,
	})
	if err != nil {
		t.Fatalf("set project secret: %v", err)
	}
	// Pre-existing project-scope secret with the reserved name but a non-reserved target.
	_, _, err = backend.Set(ctx, &SetSecretInput{
		Name:       telemetryCredentialFileName,
		Value:      "project-evil-json",
		SecretType: store.SecretTypeFile,
		Target:     "~/.config/evil.json",
		Scope:      ScopeProject,
		ScopeID:    projID,
	})
	if err != nil {
		t.Fatalf("set project same-name secret: %v", err)
	}
	// Pre-existing user-scope secret with the same well-known name targeting the path.
	_, _, err = backend.Set(ctx, &SetSecretInput{
		Name:       telemetryCredentialFileName,
		Value:      "user-sa-json",
		SecretType: store.SecretTypeFile,
		Target:     "/home/scion/.scion/telemetry-gcp-credentials.json",
		Scope:      ScopeUser,
		ScopeID:    userID,
	})
	if err != nil {
		t.Fatalf("set user secret: %v", err)
	}
	// Pre-existing agent-scope on_demand secret with the reserved name (must not shadow hub row).
	_, _, err = backend.Set(ctx, &SetSecretInput{
		Name:          telemetryCredentialFileName,
		Value:         "agent-ondemand-shadow",
		SecretType:    store.SecretTypeOnDemand,
		Target:        telemetryCredentialFileName,
		InjectionMode: store.InjectionModeAsNeeded,
		Scope:         ScopeAgent,
		ScopeID:       agentID,
	})
	if err != nil {
		t.Fatalf("set agent on_demand secret: %v", err)
	}

	var skipped []string
	resolved, err := backend.Resolve(ctx, userID, projID, "", &ResolveOpts{
		AgentID:                 agentID,
		DropReservedFileTargets: true,
		OnReservedFileTargetSkipped: func(m SecretMeta) {
			skipped = append(skipped, m.Scope+":"+m.Name)
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved) != 1 {
		t.Fatalf("expected 1 resolved secret (hub telemetry credential), got %d: %+v", len(resolved), resolved)
	}
	if resolved[0].Scope != ScopeHub || resolved[0].Name != telemetryCredentialFileName || resolved[0].Value != "hub-sa-json" {
		t.Fatalf("unexpected resolved secret: %+v", resolved[0])
	}
	if len(skipped) != 4 {
		t.Fatalf("expected 4 skipped secrets, got %d: %v", len(skipped), skipped)
	}
}

func TestLocalBackendResolve_DropReservedFileTargetsPreservesHubCredential(t *testing.T) {
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hubID := tid("hub-1")
	backend := NewLocalBackend(st, hubID, "test-secret")
	assertDropReservedFileTargetsPreservesHubCredential(t, backend, hubID)
}

func TestGCPBackendResolve_DropReservedFileTargetsPreservesHubCredential(t *testing.T) {
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	hubID := tid("hub-gcp-1")
	mock := newMockSMClient()
	backend := NewGCPBackendWithClient(st, mock, "test-project", hubID)
	assertDropReservedFileTargetsPreservesHubCredential(t, backend, hubID)
}
