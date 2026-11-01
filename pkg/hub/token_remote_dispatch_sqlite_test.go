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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On create and start the dispatcher sends the project's recorded remote as
// the token remote, for a per-agent clone whose URL comes from a
// clone-url label and whose stored config names the label's host.
func TestHTTPAgentDispatcher_TokenRemoteIsProjectRecord(t *testing.T) {
	labels := map[string]string{store.LabelCloneURL: "https://mirror.example/test/repo.git"}
	dispatcher, client, agent := newSharedCloneDispatchFixture(t, "github.com/test/repo", labels)
	depth := 1
	agent.AppliedConfig.GitClone = &api.GitCloneConfig{
		URL: "https://mirror.example/test/repo.git", Branch: "main", Depth: &depth,
		TokenRemote: "mirror.example/test/repo",
	}

	_, err := dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	cfg := client.lastCreateReq.Config
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.GitClone)
	assert.Equal(t, "https://mirror.example/test/repo.git", cfg.GitClone.URL)
	assert.Equal(t, "github.com/test/repo", cfg.GitClone.TokenRemote, "create: the record, never the label")

	require.NoError(t, dispatcher.DispatchAgentStart(context.Background(), agent, "", false))
	ws := client.lastStartExtras.Workspace
	require.NotNil(t, ws.GitClone)
	assert.Equal(t, "github.com/test/repo", ws.GitClone.TokenRemote, "start: the record, never the label")
}

// A shared-plain project's workspace clone settings carry the recorded remote
// on create and start, with a clone-url label naming another host.
func TestHTTPAgentDispatcher_SharedWorkspaceCloneTokenRemoteIsProjectRecord(t *testing.T) {
	labels := map[string]string{
		store.LabelWorkspaceMode: store.WorkspaceModeShared,
		store.LabelCloneURL:      "https://mirror.example/test/shared.git",
	}
	dispatcher, client, agent := newSharedCloneDispatchFixture(t, "github.com/test/shared", labels)
	agent.AppliedConfig.Workspace = "/home/user/.scion/projects/shared-clone"

	_, err := dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.NoError(t, err)
	cfg := client.lastCreateReq.Config
	require.NotNil(t, cfg)
	require.NotNil(t, cfg.SharedWorkspaceClone)
	assert.Equal(t, "github.com/test/shared", cfg.SharedWorkspaceClone.TokenRemote)

	require.NoError(t, dispatcher.DispatchAgentStart(context.Background(), agent, "", false))
	require.NotNil(t, client.lastStartExtras.Workspace.SharedWorkspaceClone)
	assert.Equal(t, "github.com/test/shared", client.lastStartExtras.Workspace.SharedWorkspaceClone.TokenRemote)
}
