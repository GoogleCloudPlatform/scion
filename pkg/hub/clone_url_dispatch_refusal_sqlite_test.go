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
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A clone-per-agent agent whose clone URL provisioning refuses is never
// dispatched: create, start and restart fail before any broker request.
func TestHTTPAgentDispatcher_RefusedCloneURLNotDispatched(t *testing.T) {
	labels := map[string]string{store.LabelCloneURL: "http://refused.example/test/repo.git"}
	dispatcher, client, agent := newSharedCloneDispatchFixture(t, "github.com/test/repo", labels)
	depth := 1
	agent.AppliedConfig.GitClone = &api.GitCloneConfig{URL: "http://refused.example/test/repo.git", Branch: "main", Depth: &depth}

	_, err := dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.ErrorIs(t, err, provision.ErrHostCloneURLRefused)
	assert.False(t, client.createCalled, "create must not reach the broker")
	assert.Nil(t, client.lastCreateReq)

	err = dispatcher.DispatchAgentStart(context.Background(), agent, "", false)
	require.ErrorIs(t, err, provision.ErrHostCloneURLRefused)
	assert.False(t, client.startCalled, "start must not reach the broker")

	err = dispatcher.DispatchAgentRestart(context.Background(), agent)
	require.ErrorIs(t, err, provision.ErrHostCloneURLRefused)
	assert.False(t, client.restartCalled, "restart must not reach the broker")
}

// A shared-plain project whose workspace clone URL provisioning refuses is
// not dispatched either.
func TestHTTPAgentDispatcher_RefusedSharedWorkspaceCloneURLNotDispatched(t *testing.T) {
	labels := map[string]string{
		store.LabelWorkspaceMode: store.WorkspaceModeShared,
		store.LabelCloneURL:      "file:///srv/refused.example/shared",
	}
	dispatcher, client, agent := newSharedCloneDispatchFixture(t, "github.com/test/shared", labels)
	agent.AppliedConfig.Workspace = "/home/user/.scion/projects/shared-clone"

	_, err := dispatcher.DispatchAgentCreate(context.Background(), agent)
	require.ErrorIs(t, err, provision.ErrHostCloneURLRefused)
	assert.False(t, client.createCalled)

	err = dispatcher.DispatchAgentStart(context.Background(), agent, "", false)
	require.ErrorIs(t, err, provision.ErrHostCloneURLRefused)
	assert.False(t, client.startCalled)
}
