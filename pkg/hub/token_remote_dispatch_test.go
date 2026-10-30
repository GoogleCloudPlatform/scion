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

package hub

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The dispatched GitClone's token remote is always the project record
// resolved for this dispatch: a stored value (an old record, or one that
// arrived with the config) is replaced, an empty record clears it, and the
// agent's stored config is not modified.
func TestWorkspaceSpecFor_TokenRemoteFromProjectRecord(t *testing.T) {
	depth := 1
	stored := &api.GitCloneConfig{URL: "https://mirror.example/org/repo.git", Branch: "main", Depth: &depth, TokenRemote: "mirror.example/org/repo"}
	agent := &store.Agent{AppliedConfig: &store.AgentAppliedConfig{GitClone: stored, Branch: "b"}}

	spec := workspaceSpecFor(agent, projectDispatchInfo{gitRemote: "github.com/org/repo"})
	require.NotNil(t, spec.GitClone)
	assert.Equal(t, "github.com/org/repo", spec.GitClone.TokenRemote, "the record, never the stored value")
	assert.Equal(t, stored.URL, spec.GitClone.URL)
	assert.Equal(t, "main", spec.GitClone.Branch)
	assert.Equal(t, "mirror.example/org/repo", stored.TokenRemote, "the stored config is not modified")

	spec = workspaceSpecFor(agent, projectDispatchInfo{})
	require.NotNil(t, spec.GitClone)
	assert.Empty(t, spec.GitClone.TokenRemote, "no record, no token remote")

	spec = workspaceSpecFor(&store.Agent{AppliedConfig: &store.AgentAppliedConfig{}}, projectDispatchInfo{gitRemote: "github.com/org/repo"})
	assert.Nil(t, spec.GitClone, "no GitClone stays nil")
}

// A shared-plain project's workspace clone settings bind the token to the
// recorded remote, never to the clone-url label.
func TestSharedWorkspaceCloneConfig_TokenRemoteIsRecord(t *testing.T) {
	gc := sharedWorkspaceCloneConfig(&store.Project{
		GitRemote: "github.com/test/shared",
		Labels: map[string]string{
			store.LabelWorkspaceMode: store.WorkspaceModeShared,
			store.LabelCloneURL:      "https://mirror.example/test/shared.git",
		},
	})
	require.NotNil(t, gc)
	assert.Equal(t, "https://mirror.example/test/shared.git", gc.URL)
	assert.Equal(t, "github.com/test/shared", gc.TokenRemote)
}

// The token remote reaches the start/restart wire (both transports build
// their payload with applyStartExtras) as gitClone.tokenRemote and
// sharedWorkspaceClone.tokenRemote.
func TestApplyStartExtras_TokenRemoteOnWire(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{Workspace: WorkspaceDispatchSpec{
		GitClone:             &api.GitCloneConfig{URL: "https://github.com/org/repo.git", TokenRemote: "github.com/org/repo"},
		SharedWorkspaceClone: &api.GitCloneConfig{URL: "https://github.com/org/shared.git", TokenRemote: "github.com/org/shared"},
	}})
	data, err := json.Marshal(payload)
	require.NoError(t, err)
	var wire struct {
		GitClone             api.GitCloneConfig `json:"gitClone"`
		SharedWorkspaceClone api.GitCloneConfig `json:"sharedWorkspaceClone"`
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	assert.Equal(t, "github.com/org/repo", wire.GitClone.TokenRemote)
	assert.Equal(t, "github.com/org/shared", wire.SharedWorkspaceClone.TokenRemote)
	assert.True(t, strings.Contains(string(data), `"tokenRemote"`))
}
