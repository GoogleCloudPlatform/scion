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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartAgentViaHub_NoWorkspaceRootWarning(t *testing.T) {
	const want = "no workspace root found for this project; run from the project directory or pass --project"
	require.Equal(t, want, noWorkspaceRootWarning)

	for _, tc := range []struct {
		name          string
		fromWorkspace bool   // run from the workspace root (files are collected)
		gitRemote     string // the hub project's git remote
		devToken      string // a dev token makes the loopback stub the local workstation hub
		wantWarning   bool
	}{
		{name: "no workspace root found", wantWarning: true},
		{name: "files collected", fromWorkspace: true},
		{name: "git project", gitRemote: "https://example.com/org/repo.git"},
		{name: "local workstation hub", devToken: "dev-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			origProjectPath := projectPath
			t.Cleanup(func() { projectPath = origProjectPath })
			projectPath = ""
			for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", projectkeys.EnvProjectID, "SCION_HUB_TOKEN", "SCION_DEV_TOKEN_FILE"} {
				t.Setenv(k, "")
			}
			t.Setenv("SCION_DEV_TOKEN", tc.devToken)

			const projectID, agentName = "proj-root-warn", "root-warn-agent"
			workspace, resolved := setupMarkerLayoutProject(t)
			if tc.fromWorkspace {
				t.Chdir(workspace)
			} else {
				t.Chdir(t.TempDir())
			}
			stub := newHubStartStub(t, projectID, agentName, "")
			stub.project = map[string]interface{}{"id": projectID, "name": "p", "gitRemote": tc.gitRemote}
			hubCtx := stub.hubCtx(t, projectID)
			hubCtx.ProjectPath = resolved

			var err error
			stderr := captureStderr(t, func() {
				_ = captureStdout(t, func() { err = startAgentViaHub(nil, hubCtx, agentName, "", false, nil) })
			})
			require.NoError(t, err)
			if tc.wantWarning {
				assert.Equal(t, 1, strings.Count(stderr, want+"\n"), "warning line in stderr:\n%s", stderr)
			} else {
				assert.NotContains(t, stderr, want)
			}
			if tc.fromWorkspace {
				assert.NotEmpty(t, stub.createBody["workspaceFiles"], "files collected")
			}
		})
	}
}
