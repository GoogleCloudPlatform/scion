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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// branchPrecedenceCases are the branch precedence cases every create and
// start path must honor: the --branch flag, then the inline config's
// branch, then the default (no branch).
var branchPrecedenceCases = []struct {
	name      string
	flag      string
	cfgBranch string // "" with hasConfig → --config without a branch
	hasConfig bool
	want      string // "" → the default (field absent on the wire)
}{
	{name: "config branch, no flag", cfgBranch: "cfg-branch", hasConfig: true, want: "cfg-branch"},
	{name: "flag wins over config", flag: "flag-branch", cfgBranch: "cfg-branch", hasConfig: true, want: "flag-branch"},
	{name: "flag only", flag: "flag-branch", want: "flag-branch"},
	{name: "config without branch", hasConfig: true},
	{name: "neither", want: ""},
}

func branchCaseConfig(hasConfig bool, cfgBranch string) *api.ScionConfig {
	if !hasConfig {
		return nil
	}
	return &api.ScionConfig{Branch: cfgBranch}
}

// setBranchWorkspaceFlags sets the --branch and --workspace globals and
// restores them after the test.
func setBranchWorkspaceFlags(t *testing.T, b, ws string) {
	t.Helper()
	origBranch, origWorkspace := branch, workspace
	t.Cleanup(func() { branch, workspace = origBranch, origWorkspace })
	branch, workspace = b, ws
}

func assertBodyBranch(t *testing.T, body map[string]interface{}, want string) {
	t.Helper()
	got, present := body["branch"]
	if want == "" {
		assert.False(t, present, "branch must be omitted when neither --branch nor a config branch is given, got %v", got)
		return
	}
	assert.Equal(t, want, got)
}

// ptone/scion#4947: the local create and start paths both pick the branch
// with resolveBranch, so this pins the local-mode precedence that the Hub
// paths below must match.
func TestResolveBranch_Precedence(t *testing.T) {
	for _, tc := range branchPrecedenceCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, resolveBranch(tc.flag, branchCaseConfig(tc.hasConfig, tc.cfgBranch)))
		})
	}
}

// ptone/scion#4947: Hub-mode create uses the inline config's branch when no
// --branch flag is given, as local mode does.
func TestCreateAgentViaHub_BranchPrecedence(t *testing.T) {
	const projectID, agentName = "proj-create-branch", "create-branch-agent"
	for _, tc := range branchPrecedenceCases {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			setBranchWorkspaceFlags(t, tc.flag, "")

			stub := newHubStartStub(t, projectID, agentName, "")
			var err error
			captureStdIO(t, func() {
				err = createAgentViaHub(stub.hubCtx(t, projectID), agentName, "", branchCaseConfig(tc.hasConfig, tc.cfgBranch))
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			assertBodyBranch(t, stub.createBody, tc.want)
		})
	}
}

// ptone/scion#4947: Hub-mode start uses the inline config's branch when no
// --branch flag is given, as local mode does.
func TestStartAgentViaHub_BranchPrecedence(t *testing.T) {
	const projectID, agentName = "proj-start-branch", "start-branch-agent"
	for _, tc := range branchPrecedenceCases {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			setBranchWorkspaceFlags(t, tc.flag, "")

			stub := newHubStartStub(t, projectID, agentName, "")
			var err error
			captureStdIO(t, func() {
				err = startAgentViaHub(nil, stub.hubCtx(t, projectID), agentName, "", false, branchCaseConfig(tc.hasConfig, tc.cfgBranch))
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			assertBodyBranch(t, stub.createBody, tc.want)
		})
	}
}

// ptone/scion#4947: Hub-mode create sends --workspace in the create request,
// as Hub-mode start does, and omits it when the flag is not given.
func TestCreateAgentViaHub_SendsWorkspace(t *testing.T) {
	const projectID, agentName = "proj-create-ws", "create-ws-agent"
	for _, tc := range []struct {
		name, ws string
	}{
		{name: "with --workspace", ws: "services/api"},
		{name: "without --workspace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHubStartGlobals(t)
			setBranchWorkspaceFlags(t, "", tc.ws)

			stub := newHubStartStub(t, projectID, agentName, "")
			var err error
			captureStdIO(t, func() {
				err = createAgentViaHub(stub.hubCtx(t, projectID), agentName, "", nil)
			})
			require.NoError(t, err)
			require.Equal(t, 1, stub.createCalls)
			got, present := stub.createBody["workspace"]
			if tc.ws == "" {
				assert.False(t, present, "workspace must be omitted without --workspace, got %v", got)
				return
			}
			assert.Equal(t, tc.ws, got)
		})
	}
}
