// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type recordingGitHubAppMinter struct {
	projectID string
	called    bool
}

func (m *recordingGitHubAppMinter) MintGitHubAppTokenForProject(_ context.Context, project *store.Project) (string, string, error) {
	m.called = true
	m.projectID = project.ID
	return "github-app-token", "2026-09-14T09:00:00Z", nil
}

func TestInjectLifecycleGitHubToken(t *testing.T) {
	ctx := t.Context()
	st := createTestStore(t)
	installID := int64(12345)
	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "test-org",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))

	sourceProject := &store.Project{
		ID:                   tid("github-token-source"),
		Name:                 "token-source",
		Slug:                 "token-source",
		GitHubInstallationID: &installID,
	}
	require.NoError(t, st.CreateProject(ctx, sourceProject))
	targetProject := &store.Project{
		ID:   tid("github-token-target"),
		Name: "token-target",
		Slug: "token-target",
	}
	require.NoError(t, st.CreateProject(ctx, targetProject))

	minter := &recordingGitHubAppMinter{}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, &mockRuntimeBrokerClient{}, false, slog.Default())
	dispatcher.SetGitHubAppMinter(minter)
	agent := &store.Agent{
		ProjectID: targetProject.ID,
		Labels: map[string]string{
			"scion.dev/github-token-source-project": sourceProject.ID,
		},
	}
	resolvedEnv := make(map[string]string)
	var classifications map[string]api.EnvKind

	dispatcher.injectLifecycleGitHubToken(ctx, agent, resolvedEnv, &classifications, "DispatchAgentStart")

	assert.True(t, minter.called)
	assert.Equal(t, sourceProject.ID, minter.projectID)
	assert.Equal(t, "github-app-token", resolvedEnv["GITHUB_TOKEN"])
	assert.Equal(t, "true", resolvedEnv["SCION_GITHUB_APP_ENABLED"])
	assert.Equal(t, "2026-09-14T09:00:00Z", resolvedEnv["SCION_GITHUB_TOKEN_EXPIRY"])
	assert.Equal(t, "/tmp/.github-token", resolvedEnv["SCION_GITHUB_TOKEN_PATH"])
	assert.Equal(t, api.EnvKindSecretInjected, classifications["GITHUB_TOKEN"])
	assert.Equal(t, api.EnvKindPlain, classifications["SCION_GITHUB_APP_ENABLED"])
	assert.Equal(t, api.EnvKindPlain, classifications["SCION_GITHUB_TOKEN_EXPIRY"])
	assert.Equal(t, api.EnvKindPlain, classifications["SCION_GITHUB_TOKEN_PATH"])
}

func TestInjectLifecycleGitHubTokenPreservesUserToken(t *testing.T) {
	ctx := t.Context()
	st := createTestStore(t)
	installID := int64(12345)
	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: installID,
		AccountLogin:   "test-org",
		AccountType:    "Organization",
		AppID:          1,
		Status:         store.GitHubInstallationStatusActive,
	}))
	project := &store.Project{
		ID:                   tid("github-user-token"),
		Name:                 "user-token",
		Slug:                 "user-token",
		GitHubInstallationID: &installID,
	}
	require.NoError(t, st.CreateProject(ctx, project))

	minter := &recordingGitHubAppMinter{}
	dispatcher := NewHTTPAgentDispatcherWithClient(st, &mockRuntimeBrokerClient{}, false, slog.Default())
	dispatcher.SetGitHubAppMinter(minter)
	resolvedEnv := map[string]string{"GITHUB_TOKEN": "user-token"}
	var classifications map[string]api.EnvKind

	dispatcher.injectLifecycleGitHubToken(ctx, &store.Agent{ProjectID: project.ID}, resolvedEnv, &classifications, "DispatchAgentRestart")

	assert.False(t, minter.called)
	assert.Equal(t, "user-token", resolvedEnv["GITHUB_TOKEN"])
	assert.Equal(t, "true", resolvedEnv["SCION_USER_GITHUB_TOKEN"])
	assert.Equal(t, "true", resolvedEnv["SCION_GITHUB_APP_ENABLED"])
	assert.Equal(t, api.EnvKindPlain, classifications["SCION_USER_GITHUB_TOKEN"])
	assert.Equal(t, api.EnvKindPlain, classifications["SCION_GITHUB_APP_ENABLED"])
}
