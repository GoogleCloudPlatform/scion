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

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTriggerPresetCLI_ParityWithDirectCompileAndCreate verifies that creating
// a trigger via `scion trigger preset apply` and creating one via direct
// POST /trigger-presets/compile + POST /triggers produce identical stored
// triggers (Stage 3 §9.2, §16 item 12).
func TestTriggerPresetCLI_ParityWithDirectCompileAndCreate(t *testing.T) {
	resetTriggerPresetFlags(t)
	ctx := context.Background()

	s := newTestStore(t)
	devToken, err := apiclient.GenerateDevToken()
	require.NoError(t, err)

	srv, err := hub.New(hub.ServerConfig{
		HubID:        "preset-parity-test-hub",
		DevAuthToken: devToken,
	}, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ops := hub.NewOperationalSettings(s.(store.HubSettingStore), koanf.New("."), koanf.New("."))
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)

	expDoc := fmt.Sprintf(`{"overrides":{%q:true,%q:true}}`, experiments.Triggers, experiments.GitHubTriggers)
	_, err = ops.Update(ctx, "experiments", json.RawMessage(expDoc), "test-admin", ops.SectionRevision("experiments"), "managed")
	require.NoError(t, err)

	// Create admin user with hub-admin role binding, GitHub installation,
	// associated project, and target agent.
	adminID := uuid.NewString()
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID:          adminID,
		Email:       "preset-parity-admin@example.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
		Created:     time.Now().UTC(),
	}))
	hubAdminRole, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		ID:               uuid.NewString(),
		RoleDefinitionID: hubAdminRole.ID,
		PrincipalType:    "user",
		PrincipalID:      adminID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
	})
	require.NoError(t, err)

	const instID int64 = 998877
	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "octo-org",
		AccountType:    "Organization",
		AppID:          4242,
		Status:         store.GitHubInstallationStatusActive,
		Repositories:   []string{"octo-org/octo-repo"},
	}))

	instCopy := instID
	bindingRaw, err := json.Marshal(map[string]any{
		"installation_id": instID,
		"remote":          "github.com/octo-org/octo-repo",
		"bound_by":        adminID,
		"bound_at":        time.Now().UTC().Format(time.RFC3339),
	})
	require.NoError(t, err)

	proj := &store.Project{
		ID:                   uuid.NewString(),
		Name:                 "preset-parity-project",
		Slug:                 "preset-parity-project",
		GitRemote:            "https://github.com/octo-org/octo-repo.git",
		GitHubInstallationID: &instCopy,
		Annotations: map[string]string{
			"scion.dev/github-installation-binding": string(bindingRaw),
		},
		CreatedBy: adminID,
	}
	require.NoError(t, s.CreateProject(ctx, proj))

	targetAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "lead",
		Name:      "lead",
		ProjectID: proj.ID,
		Phase:     "running",
		OwnerID:   adminID,
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client, err := hubclient.New(ts.URL, hubclient.WithBearerToken(devToken))
	require.NoError(t, err)

	origTriggerFn := triggerHubContextFn
	origPresetFn := triggerPresetHubContextFn
	t.Cleanup(func() {
		triggerHubContextFn = origTriggerFn
		triggerPresetHubContextFn = origPresetFn
	})
	hubCtx := &HubContext{
		Client:         client,
		Endpoint:       ts.URL,
		CredentialKind: hubsync.CredentialKindDevAuto,
	}
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		return hubCtx, proj.ID, c, cancel, nil
	}
	triggerPresetHubContextFn = func() (*HubContext, context.Context, context.CancelFunc, error) {
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		return hubCtx, c, cancel, nil
	}

	// Path A: CLI `scion trigger preset apply`.
	triggerPresetApplyName = "cli-review-trigger"
	triggerPresetApplyDescription = "Created via preset apply"
	triggerPresetApplyFields = []string{
		"command=review",
		"on=pull_request",
		"allowed_associations=OWNER,MEMBER",
		"allow_bots=false",
		"include_review_comments=true",
	}
	triggerPresetApplyAgent = "lead"
	triggerPresetApplyMessage = "Handle /review command"
	triggerPresetApplyWake = true
	triggerPresetApplyDryRun = false

	cliOut := captureStdout(t, func() {
		require.NoError(t, runTriggerPresetApply(triggerPresetApplyCmd, []string{"github.command"}))
	})
	assert.Contains(t, cliOut, "Trigger created:")
	assert.Contains(t, cliOut, "Preset:  github.command (v1)")

	// Path B: Direct POST /trigger-presets/compile + POST /triggers.
	compiled, err := client.TriggerPresets().Compile(ctx, &hubclient.CompileTriggerPresetRequest{
		Preset: "github.command",
		Fields: map[string]any{
			"command":                 "review",
			"on":                      "pull_request",
			"allowed_associations":    []string{"OWNER", "MEMBER"},
			"allow_bots":              false,
			"include_review_comments": true,
		},
	})
	require.NoError(t, err)
	formRaw, err := json.Marshal(compiled.Form)
	require.NoError(t, err)
	directRules := make([]hubclient.TriggerMatchRuleSpec, len(compiled.Match.Rules))
	for i, r := range compiled.Match.Rules {
		directRules[i] = hubclient.TriggerMatchRuleSpec(r)
	}
	directCreated, err := client.Triggers(proj.ID).Create(ctx, &hubclient.CreateTriggerRequest{
		Name:        "direct-review-trigger",
		Description: "Created via preset apply",
		Source:      hubclient.TriggerSourceSpec{Kind: "github", ID: ""},
		Match: &hubclient.TriggerMatchSpec{
			Types: compiled.Match.Types,
			Rules: directRules,
			Form:  json.RawMessage(formRaw),
		},
		Subject: &hubclient.TriggerSubjectSpec{
			Mode: compiled.Subject.Mode,
			Expr: compiled.Subject.Expr,
		},
		Action: hubclient.TriggerActionSpec{
			Type:      "message",
			AgentName: "lead",
			Message:   "Handle /review command",
			Wake:      true,
		},
	})
	require.NoError(t, err)

	// Compare the two stored triggers directly from the database.
	cliStored, err := s.GetTriggerByName(ctx, proj.ID, "cli-review-trigger")
	require.NoError(t, err)
	directStored, err := s.GetTrigger(ctx, directCreated.ID)
	require.NoError(t, err)

	assert.Equal(t, directStored.ProjectID, cliStored.ProjectID)
	assert.Equal(t, directStored.Description, cliStored.Description)
	assert.Equal(t, directStored.SourceKind, cliStored.SourceKind)
	assert.Equal(t, directStored.SourceID, cliStored.SourceID)
	assert.Equal(t, directStored.MatchTypes, cliStored.MatchTypes)
	assert.Equal(t, directStored.MatchDialect, cliStored.MatchDialect)
	assert.Equal(t, directStored.MatchRule, cliStored.MatchRule)
	assert.Equal(t, directStored.MatchRules, cliStored.MatchRules)
	assert.JSONEq(t, directStored.MatchForm, cliStored.MatchForm)
	assert.Equal(t, directStored.ActionType, cliStored.ActionType)
	assert.JSONEq(t, directStored.ActionPayload, cliStored.ActionPayload)
	assert.Equal(t, directStored.SubjectDialect, cliStored.SubjectDialect)
	assert.Equal(t, directStored.SubjectRule, cliStored.SubjectRule)
	assert.Equal(t, directStored.SubjectMode, cliStored.SubjectMode)
	assert.Equal(t, directStored.Status, cliStored.Status)
}
