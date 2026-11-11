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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestCreateAuthenticatedDispatcher_SkillRefLister pins the production
// wiring in CreateAuthenticatedDispatcher that gives the dispatcher the
// agent's merged skill references (SetSkillRefLister(s.dispatchSkillRefs)).
//
// Every other provision credential test injects a fake lister, so none of
// them notices if the factory stops installing it: the dispatcher then
// finds no gh:// refs and sends no provision credentials, with no Warn.
// This test builds the dispatcher through the same factory the running Hub
// uses and dispatches a start for an agent whose gh:// ref is declared in
// its inline config, and for one whose gh:// ref is declared only in its
// Hub template. Each must carry exactly the matching project secret, which
// also shows the key set comes from the same merged references the skill
// pre-resolvers read.
func TestCreateAuthenticatedDispatcher_SkillRefLister(t *testing.T) {
	srv, s, alice, _, project := setupSkillAuthzTest(t)
	ctx := context.Background()
	stor := newContentMockStorage("test-bucket")
	srv.SetStorage(stor)

	backend := secret.NewLocalBackend(s, srv.hubID, "test-secret")
	srv.SetSecretBackend(backend)
	for name, value := range map[string]string{
		"GH_ACME":   "ghp_wiredowner0123456789",
		"UNRELATED": "unrelated-value",
	} {
		_, _, err := backend.Set(ctx, &secret.SetSecretInput{
			Name: name, Value: value, SecretType: store.SecretTypeEnvironment, Target: name,
			Scope: store.ScopeProject, ScopeID: project.ID, InjectionMode: store.InjectionModeAsNeeded,
			CreatedBy: alice.ID,
		})
		require.NoError(t, err)
	}

	broker := &store.RuntimeBroker{
		ID:       tid("pc-wire-broker"),
		Name:     "pc-wire-broker",
		Slug:     "pc-wire-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/home/user/projects/pc-wire/.scion",
		Status:     store.BrokerStatusOnline,
	}))
	// The agents' creator is a project member, so they are in good standing
	// for the dispatcher's start check (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, alice.ID)

	tmpl := createDispatchSkillTemplate(t, s, stor, "pc-wire-tmpl", "gh://acme/tools/from-template")

	for _, tc := range []struct {
		name       string
		inline     []string
		templateID string
	}{
		{name: "inline_config", inline: []string{"gh://acme/tools/x"}},
		{name: "hub_template_only", templateID: tmpl.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := dispatchTestAgent(alice.ID, project.ID, tc.inline...)
			agent.AppliedConfig.TemplateID = tc.templateID
			agent.Name = "pc-wired-" + tc.name
			agent.Slug = "pc-wired-" + tc.name
			agent.RuntimeBrokerID = broker.ID
			require.NoError(t, s.CreateAgent(ctx, agent))
			hdEdge(t, s, store.DelegationPrincipalUser, alice.ID, agent.ID, project.ID, nil)

			d := srv.CreateAuthenticatedDispatcher()
			mockClient := &mockRuntimeBrokerClient{}
			d.client = mockClient

			require.NoError(t, d.DispatchAgentStart(ctx, agent, "", false))
			require.True(t, mockClient.startCalled)
			assert.Equal(t, map[string]string{"GH_ACME": "ghp_wiredowner0123456789"},
				mockClient.lastStartExtras.ProvisionCredentials,
				"the production dispatcher must read the agent's merged skill refs")
		})
	}
}
