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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestIsAuthTypeSatisfied_GCPServiceAccountSkipsEnvCheck verifies that when a
// verified GCP service account is assigned, vertex-ai env requirements
// (GOOGLE_CLOUD_PROJECT, GOOGLE_CLOUD_REGION) are treated as satisfied because
// the broker's resolveAuthEnvOverlay and GCP metadata will provide them at
// runtime. This prevents false NoAuth triggers for vertex-ai deployments (#1165).
func TestIsAuthTypeSatisfied_GCPServiceAccountSkipsEnvCheck(t *testing.T) {
	srv, memStore := testServer(t)
	ctx := context.Background()

	projectID := tid("gcp-proj-1")
	project := &store.Project{
		ID:   projectID,
		Name: "vertex-test-project",
		Slug: "vertex-test-project",
	}
	if err := memStore.CreateProject(ctx, project); err != nil {
		t.Fatalf("CreateProject failed: %v", err)
	}

	// Register a verified GCP service account for the project
	sa := &store.GCPServiceAccount{
		ID:                 tid("sa-1"),
		Scope:              "project",
		ScopeID:            projectID,
		Email:              "agent@gcp-project.iam.gserviceaccount.com",
		ProjectID:          "gcp-project",
		Verified:           true,
		VerifiedAt:         time.Now(),
		VerificationStatus: "verified",
		CreatedBy:          tid("user-1"),
		CreatedAt:          time.Now(),
	}
	if err := memStore.CreateGCPServiceAccount(ctx, sa); err != nil {
		t.Fatalf("CreateGCPServiceAccount failed: %v", err)
	}

	// Simulate the Claude harness config for vertex-ai
	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]api.HarnessAuthTypeMetadata{
			"vertex-ai": {
				RequiredEnv: []api.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
					{AnyOf: []string{"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"}},
				},
				RequiredFiles: []api.HarnessAuthFileRequirement{
					{
						Name:                                 "gcloud-adc",
						Type:                                 "file",
						Field:                                "GoogleAppCredentials",
						AlternativeEnvKeys:                   []string{"GOOGLE_APPLICATION_CREDENTIALS"},
						SkippedWhenGCPServiceAccountAssigned: true,
						Required:                             true,
					},
				},
			},
			"api-key": {
				RequiredEnv: []api.HarnessAuthEnvRequirement{
					{AnyOf: []string{"ANTHROPIC_API_KEY"}},
				},
			},
		},
	}

	t.Run("vertex-ai satisfied when GCP SA assigned even without Hub-stored env vars", func(t *testing.T) {
		agent := &store.Agent{
			ID:            tid("agent-vertex"),
			Name:          "vertex-agent",
			Slug:          "vertex-agent",
			OwnerID:       tid("user-1"),
			ProjectID:     projectID,
			AppliedConfig: &store.AgentAppliedConfig{},
		}

		// With GCP SA assigned, vertex-ai should be satisfied even though
		// GOOGLE_CLOUD_PROJECT and GOOGLE_CLOUD_REGION are not in Hub storage.
		// They will be provided at runtime by broker env overlay / GCP metadata.
		satisfied, err := srv.isAuthTypeSatisfied(ctx, agent, authMeta, "vertex-ai", true)
		if err != nil {
			t.Fatalf("isAuthTypeSatisfied failed: %v", err)
		}
		if !satisfied {
			t.Error("expected vertex-ai to be satisfied when GCP SA is assigned (env vars come from runtime)")
		}
	})

	t.Run("vertex-ai not satisfied without GCP SA and without Hub-stored env vars", func(t *testing.T) {
		agent := &store.Agent{
			ID:            tid("agent-vertex-nosa"),
			Name:          "vertex-no-sa",
			Slug:          "vertex-no-sa",
			OwnerID:       tid("user-1"),
			ProjectID:     projectID,
			AppliedConfig: &store.AgentAppliedConfig{},
		}

		// Without GCP SA, vertex-ai should not be satisfied because
		// GOOGLE_CLOUD_PROJECT is not in Hub storage.
		satisfied, err := srv.isAuthTypeSatisfied(ctx, agent, authMeta, "vertex-ai", false)
		if err != nil {
			t.Fatalf("isAuthTypeSatisfied failed: %v", err)
		}
		if satisfied {
			t.Error("expected vertex-ai to NOT be satisfied without GCP SA and without Hub-stored env vars")
		}
	})

	t.Run("api-key not affected by GCP SA (non-GCP auth type)", func(t *testing.T) {
		agent := &store.Agent{
			ID:            tid("agent-apikey-gcp"),
			Name:          "apikey-gcp",
			Slug:          "apikey-gcp",
			OwnerID:       tid("user-1"),
			ProjectID:     projectID,
			AppliedConfig: &store.AgentAppliedConfig{},
		}

		// api-key has no files with SkippedWhenGCPServiceAccountAssigned,
		// so GCP SA should not affect its env-var check.
		satisfied, err := srv.isAuthTypeSatisfied(ctx, agent, authMeta, "api-key", true)
		if err != nil {
			t.Fatalf("isAuthTypeSatisfied failed: %v", err)
		}
		if satisfied {
			t.Error("expected api-key to NOT be satisfied without ANTHROPIC_API_KEY, even with GCP SA")
		}
	})

	t.Run("hasRequiredAuthCredentials returns true for vertex-ai with GCP SA", func(t *testing.T) {
		agent := &store.Agent{
			ID:            tid("agent-hasreq"),
			Name:          "hasreq-vertex",
			Slug:          "hasreq-vertex",
			OwnerID:       tid("user-1"),
			ProjectID:     projectID,
			AppliedConfig: &store.AgentAppliedConfig{},
		}

		// When iterating all auth types, vertex-ai should be satisfied
		// because of the GCP SA, preventing false NoAuth fallback.
		hasCreds, err := srv.hasRequiredAuthCredentials(ctx, agent, "claude", authMeta)
		if err != nil {
			t.Fatalf("hasRequiredAuthCredentials failed: %v", err)
		}
		if !hasCreds {
			t.Error("expected hasRequiredAuthCredentials to return true — vertex-ai should be satisfied with GCP SA")
		}
	})
}
