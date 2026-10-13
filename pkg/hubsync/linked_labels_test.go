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

package hubsync

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinkedProjectLabels(t *testing.T) {
	const remote = "https://github.com/acme/widgets.git"
	worktree := map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}
	tests := []struct {
		name      string
		endpoint  string
		gitRemote string
		devToken  string
		hubToken  string
		want      map[string]string
	}{
		{name: "git repo on the local workstation hub", endpoint: "http://127.0.0.1:8080", gitRemote: remote, devToken: "scion_dev_abc", want: worktree},
		{name: "git repo on localhost", endpoint: "http://localhost:8080", gitRemote: remote, devToken: "scion_dev_abc", want: worktree},
		{name: "no git remote", endpoint: "http://127.0.0.1:8080", devToken: "scion_dev_abc"},
		{name: "remote hub", endpoint: "https://hub.example.com", gitRemote: remote, devToken: "scion_dev_abc"},
		{name: "loopback hub with a real credential", endpoint: "http://127.0.0.1:8080", gitRemote: remote, devToken: "scion_dev_abc", hubToken: "scion_pat_xyz"},
		{name: "loopback hub without dev auth", endpoint: "http://127.0.0.1:8080", gitRemote: remote},
		{name: "no endpoint", gitRemote: remote, devToken: "scion_dev_abc"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearHubCredentialEnv(t)
			t.Setenv("SCION_DEV_TOKEN", tc.devToken)
			t.Setenv("SCION_HUB_TOKEN", tc.hubToken)
			assert.Equal(t, tc.want, LinkedProjectLabels(tc.endpoint, tc.gitRemote))
		})
	}
}

// TestRegisterProject_WorkspaceModeLabel: the auto-link registration labels
// a git project worktree-per-agent only on the local workstation hub. The
// global project and a registration with a non-dev credential send none.
func TestRegisterProject_WorkspaceModeLabel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	newHandler := func() (http.HandlerFunc, *map[string]string, *bool) {
		var got map[string]string
		var called bool
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Labels map[string]string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			got = body.Labels
			called = true
			w.WriteHeader(http.StatusNoContent)
		}, &got, &called
	}
	// chdirGitRepo moves into a fresh git repo with an origin remote, which
	// registerProject reads from the current directory.
	chdirGitRepo := func(t *testing.T) {
		t.Helper()
		dir := t.TempDir()
		for _, args := range [][]string{
			{"init", "-q"},
			{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
		} {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, string(out))
		}
		t.Chdir(dir)
	}

	t.Run("git project on the local workstation hub", func(t *testing.T) {
		handler, got, called := newHandler()
		hubCtx, _ := newHintTestHubCtx(t, handler)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
		chdirGitRepo(t)
		_ = registerProject(context.Background(), hubCtx, "widgets", false)
		require.True(t, *called)
		assert.Equal(t, map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}, *got)
	})

	t.Run("global project", func(t *testing.T) {
		handler, got, called := newHandler()
		hubCtx, _ := newHintTestHubCtx(t, handler)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
		chdirGitRepo(t)
		_ = registerProject(context.Background(), hubCtx, "global", true)
		require.True(t, *called)
		assert.Empty(t, *got)
	})

	t.Run("hub reached with a non-dev credential", func(t *testing.T) {
		handler, got, called := newHandler()
		hubCtx, _ := newHintTestHubCtx(t, handler)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
		t.Setenv("SCION_HUB_TOKEN", "scion_pat_xyz")
		chdirGitRepo(t)
		_ = registerProject(context.Background(), hubCtx, "widgets", false)
		require.True(t, *called)
		assert.Empty(t, *got)
	})

	t.Run("non-git directory", func(t *testing.T) {
		handler, got, called := newHandler()
		hubCtx, _ := newHintTestHubCtx(t, handler)
		t.Setenv("SCION_DEV_TOKEN", "scion_dev_test")
		t.Chdir(t.TempDir())
		_ = registerProject(context.Background(), hubCtx, "plain", false)
		require.True(t, *called)
		assert.Empty(t, *got)
	})
}
