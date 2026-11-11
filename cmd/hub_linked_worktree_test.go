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

package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegisterProjectOnHub_WorkspaceModeLabel: 'scion hub link' labels a git
// project worktree-per-agent only on the local workstation hub.
func TestRegisterProjectOnHub_WorkspaceModeLabel(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	var gotLabels map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Labels map[string]string `json:"labels"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotLabels = body.Labels
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"project": map[string]string{"id": "p1", "name": "widgets"},
			"created": true,
		})
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://github.com/acme/widgets.git"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	t.Chdir(repo)

	tests := []struct {
		name     string
		endpoint string
		hubToken string
		isGlobal bool
		want     map[string]string
	}{
		{name: "local workstation hub", endpoint: srv.URL, want: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent}},
		{name: "remote hub", endpoint: "https://hub.example.com"},
		{name: "loopback hub with a non-dev credential", endpoint: srv.URL, hubToken: "scion_pat_xyz"},
		{name: "global project", endpoint: srv.URL, isGlobal: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("SCION_DEV_TOKEN", "scion_dev_abc")
			t.Setenv("SCION_DEV_TOKEN_FILE", "")
			t.Setenv("SCION_AUTH_TOKEN", "")
			t.Setenv("SCION_HUB_TOKEN", tc.hubToken)
			gotLabels = nil
			_, err := registerProjectOnHub(context.Background(), client, tc.endpoint, "p1", "widgets", repo+"/.scion", tc.isGlobal)
			require.NoError(t, err)
			assert.Equal(t, tc.want, gotLabels)
		})
	}
}

func TestPrintHubWorkspaceSource_Modes(t *testing.T) {
	const remote = "github.com/acme/widgets"
	tests := []struct {
		name      string
		labels    map[string]string
		contains  []string
		forbidden []string
	}{
		{
			name:      "worktree-per-agent",
			labels:    map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeWorktreePerAgent},
			contains:  []string{"git worktree per agent for repo " + remote, "for a checkout linked on this machine", ".scion/agents/<name>/workspace", "otherwise the runtime broker makes per-agent worktrees of its own clone"},
			forbidden: []string{"cloning", "local worktrees are not used"},
		},
		{
			name:     "shared",
			labels:   map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared},
			contains: []string{"shared workspace for repo " + remote},
		},
		{
			name:     "unlabelled (clone per agent)",
			contains: []string{"cloning repo", "local worktrees are not used"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() {
				printHubWorkspaceSource(&hubclient.Project{GitRemote: remote, Labels: tc.labels})
			})
			for _, s := range tc.contains {
				assert.Contains(t, out, s)
			}
			for _, s := range tc.forbidden {
				assert.NotContains(t, out, s)
			}
		})
	}
}
