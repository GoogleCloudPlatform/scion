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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// workspaceCheckDispatcher is a lifecycle dispatcher whose exec answers the
// pre-stop workspace check with execOutput, or execErr, or (block) waits for
// the exec context to end.
type workspaceCheckDispatcher struct {
	quotaLifecycleDispatcher
	mu         sync.Mutex
	execCalls  int
	execOutput string
	execErr    error
	block      bool
	// stopErr, when set, is returned by DispatchAgentStop.
	stopErr error
}

const workAt23 = "scion-workspace-check commits=2 files=3\n"

func newWorkspaceCheckServer(t *testing.T, disp *workspaceCheckDispatcher) (*Server, store.Store, *store.RuntimeBroker, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	srv.SetDispatcher(disp)
	broker, project := newQuotaTestBrokerAndProject(t, s, "wscheck")
	return srv, s, broker, project
}

// newWorkspaceAgent creates an agent of runtime rt with workspace placement
// placement (empty: none reported) in phase.
func newWorkspaceAgent(t *testing.T, s store.Store, broker *store.RuntimeBroker, project *store.Project, name, rt, placement string, phase state.Phase) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID:              tid("agent-" + name),
		Slug:            name,
		Name:            name,
		ProjectID:       project.ID,
		RuntimeBrokerID: broker.ID,
		Runtime:         rt,
		Phase:           string(phase),
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	if placement != "" {
		require.NoError(t, s.SetAgentWorkspacePlacement(context.Background(), a.ID, placement))
	}
	return a
}

func workspaceAnnotation(t *testing.T, s store.Store, id string) string {
	t.Helper()
	got, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return got.Annotations[workspaceAtStopAnnotation]
}

const (
	stopWarning23  = "Workspace is ephemeral and will be re-cloned on next start; 2 unpushed commits and 3 changed files will be lost. Push first to keep them."
	startWarning23 = "Workspace is ephemeral and was re-cloned; 2 unpushed commits and 3 changed files from the previous run were lost."
)

func (d *workspaceCheckDispatcher) DispatchAgentStop(ctx context.Context, agent *store.Agent) error {
	if d.stopErr != nil {
		return d.stopErr
	}
	return d.quotaLifecycleDispatcher.DispatchAgentStop(ctx, agent)
}

func (d *workspaceCheckDispatcher) DispatchAgentExec(ctx context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	d.mu.Lock()
	d.execCalls++
	out, err, block := d.execOutput, d.execErr, d.block
	d.mu.Unlock()
	if block {
		<-ctx.Done()
		return "", 0, ctx.Err()
	}
	return out, 0, err
}

func (d *workspaceCheckDispatcher) calls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.execCalls
}
