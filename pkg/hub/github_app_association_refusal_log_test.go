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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

func countRefusalLogs(logs string) int {
	return strings.Count(logs, "GitHub App token refused: installation association not authorized")
}

func TestGitHubAssociationRefusalLog_LogOncePerStateChange(t *testing.T) {
	srv, st := testServer(t)
	ctx := context.Background()

	// Fake GitHub API that serves App metadata and installation token mints so we can test recovery.
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   1,
				"slug": "fake-scion-app",
				"permissions": map[string]string{
					"contents":      "write",
					"pull_requests": "write",
					"issues":        "write",
					"metadata":      "read",
				},
			})
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token":      "ghs_recovered_token",
				"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(gh.Close)

	srv.mu.Lock()
	srv.config.GitHubAppConfig.APIBaseURL = gh.URL
	srv.config.GitHubAppConfig.AppID = 1
	srv.config.GitHubAppConfig.PrivateKey = generateTestGitHubAppKey(t)
	srv.mu.Unlock()

	var logBuf lockedBuffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	const instID int64 = 91001
	const instID2 int64 = 91002
	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Repositories:   []string{"acme/widget"},
		Status:         store.GitHubInstallationStatusActive,
	}))
	require.NoError(t, st.CreateGitHubInstallation(ctx, &store.GitHubInstallation{
		InstallationID: instID2,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          1,
		Repositories:   []string{"acme/widget"},
		Status:         store.GitHubInstallationStatusActive,
	}))

	remote := gh.URL + "/acme/widget"
	idCopy := instID
	proj := &store.Project{
		ID:                   tid("refusal-log-proj"),
		Name:                 "refusal-log-proj",
		Slug:                 tid("refusal-log-proj"),
		GitRemote:            remote,
		CreatedBy:            "", // automation / no creator GitHub identity
		GitHubInstallationID: &idCopy,
		Created:              time.Now(),
		Updated:              time.Now(),
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	// 1. Repeated identical refusals log once, while refusal behavior is unchanged.
	for i := 0; i < 4; i++ {
		tok, _, err := srv.mintGitHubAppToken(ctx, proj)
		require.Error(t, err)
		assert.ErrorIs(t, err, errGitHubAssociationUnauthorized)
		assert.Empty(t, tok)
		require.NotNil(t, proj.GitHubAppStatus)
		assert.Equal(t, store.GitHubAppStateError, proj.GitHubAppStatus.State)
		assert.Equal(t, githubAppErrAssociationUnauthorized, proj.GitHubAppStatus.ErrorCode)
	}
	assert.Equal(t, 1, countRefusalLogs(logBuf.String()), "identical repeated refusals should log once")

	// 2. Changed refusal reason (installation suspended) logs again, then suppresses repeats.
	inst, err := st.GetGitHubInstallation(ctx, instID)
	require.NoError(t, err)
	inst.Status = store.GitHubInstallationStatusSuspended
	require.NoError(t, st.UpdateGitHubInstallation(ctx, inst))

	for i := 0; i < 3; i++ {
		_, _, err := srv.mintGitHubAppToken(ctx, proj)
		require.Error(t, err)
		assert.ErrorIs(t, err, errGitHubAssociationUnauthorized)
	}
	assert.Equal(t, 2, countRefusalLogs(logBuf.String()), "changed refusal reason should log once more")

	// Restore installation to active; reason returns to original (missing admin binding / creator) -> logs again.
	inst.Status = store.GitHubInstallationStatusActive
	require.NoError(t, st.UpdateGitHubInstallation(ctx, inst))
	_, _, err = srv.mintGitHubAppToken(ctx, proj)
	require.Error(t, err)
	assert.Equal(t, 3, countRefusalLogs(logBuf.String()), "returning to previous reason after another reason should log again")

	// 3. Different installation ID on the same project logs again.
	idCopy2 := instID2
	proj.GitHubInstallationID = &idCopy2
	require.NoError(t, st.UpdateProject(ctx, proj))
	for i := 0; i < 2; i++ {
		_, _, err := srv.mintGitHubAppToken(ctx, proj)
		require.Error(t, err)
	}
	assert.Equal(t, 4, countRefusalLogs(logBuf.String()), "changed installation ID should log again")

	// 4. Recovery (valid admin binding) followed by re-refusal with the same reason logs again.
	admin := &store.User{
		ID:          tid("refusal-log-admin"),
		Email:       "refusal-log-admin@test.com",
		DisplayName: "Refusal Log Admin",
		Role:        store.UserRoleAdmin,
		Status:      store.UserStatusActive,
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	rd, err := st.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = st.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	rawBinding, err := json.Marshal(githubInstallationBinding{
		InstallationID: instID2,
		Remote:         util.NormalizeGitRemote(remote),
		BoundBy:        admin.ID,
		BoundAt:        time.Now(),
	})
	require.NoError(t, err)
	proj.Annotations = map[string]string{githubInstallationBindingAnnotation: string(rawBinding)}
	require.NoError(t, st.UpdateProject(ctx, proj))

	tok, _, err := srv.mintGitHubAppToken(ctx, proj)
	require.NoError(t, err)
	assert.Equal(t, "ghs_recovered_token", tok)
	assert.Equal(t, 4, countRefusalLogs(logBuf.String()), "recovery should not log a refusal")

	// Demote/deactivate the binder -> same (project, instID2, reason) is refused again and MUST log.
	admin.Status = store.UserStatusSuspended
	require.NoError(t, st.UpdateUser(ctx, admin))
	for i := 0; i < 3; i++ {
		_, _, err := srv.mintGitHubAppToken(ctx, proj)
		require.Error(t, err)
		assert.ErrorIs(t, err, errGitHubAssociationUnauthorized)
	}
	assert.Equal(t, 5, countRefusalLogs(logBuf.String()), "re-refusal after recovery should log once")
}
