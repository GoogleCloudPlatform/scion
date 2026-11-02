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

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime/cloudrun"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// rollbackTestRun is a Cloud Run start on a temp share with the real NFS
// provisioning (the filesystem check stubbed): it returns the runtime, the
// start's config and the agent directory agents/<slug>.
func rollbackTestRun(t *testing.T, fake *fakeInstancesClient, clonePerAgent bool) (*CloudRunRuntime, RunConfig, string) {
	t.Helper()
	orig := cloudRunRequireNFSFilesystem
	cloudRunRequireNFSFilesystem = func(string) error { return nil }
	t.Cleanup(func() { cloudRunRequireNFSFilesystem = orig })
	rt := newFakeCloudRunRuntime(t, fake)
	rt.provisionNFS = nil
	rt.config.NFSServer, rt.config.NFSExport = "10.0.0.2", "/scion-workspaces"
	share := t.TempDir()
	cfg := runConfigForTest()
	cfg.ProjectID = "proj-123"
	cfg.Workspace = filepath.Join(share, "projects", "proj-123", "workspace")
	cfg.Labels = map[string]string{"agent_id": crTestAgentID, "scion.name": crTestSlug, api.LabelRunID: "run-1"}
	if clonePerAgent {
		cfg.Env = clonePerAgentEnv
		cfg.GitClone = &api.GitCloneConfig{URL: "file:///nonexistent/scion-test-repo.git"}
	} else {
		cfg.Env = []string{"SCION_WORKSPACE_MODE=shared-plain"}
	}
	return rt, cfg, filepath.Join(share, "projects", "proj-123", "agents", crTestSlug)
}

// A start whose instance create the server refuses (here the missing actAs
// permission of ptone/scion#7100) removes the agent directories it
// provisioned on the share: home, secrets, the clone-per-agent workspace
// and agents/<slug>. The project's shared workspace is kept.
func TestCloudRunRun_RejectedCreateRemovesProvisionedAgentDirs(t *testing.T) {
	for name, clone := range map[string]bool{"clone-per-agent": true, "shared-plain": false} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeInstancesClient{getErr: notFoundErr(), createErr: status.Error(codes.PermissionDenied, "iam.serviceaccounts.actAs denied")}
			rt, cfg, agentDir := rollbackTestRun(t, fake, clone)
			marker := filepath.Join(cfg.Workspace, "README.md")
			require.NoError(t, os.MkdirAll(cfg.Workspace, 0o755))
			require.NoError(t, os.WriteFile(marker, []byte("x"), 0o644))

			_, err := rt.Run(context.Background(), cfg)
			require.Error(t, err)
			assert.NoDirExists(t, agentDir, "nothing of the failed start is left in agents/<slug>")
			assert.FileExists(t, marker, "the project's shared workspace is not the start's to remove")
		})
	}
}

// Only directories this start created are removed: an existing agent's
// home (a restart) stays with its files, its existing secrets directory
// stays even when empty (secrets reach Cloud Run through the env), and so
// does agents/<slug>; the workspace this start created goes.
func TestCloudRunRun_RejectedCreateKeepsExistingHome(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr(), createErr: status.Error(codes.PermissionDenied, "denied")}
	rt, cfg, agentDir := rollbackTestRun(t, fake, true)
	home := filepath.Join(agentDir, "home-"+crTestAgentID)
	secrets := filepath.Join(agentDir, "secrets-"+crTestAgentID)
	require.NoError(t, os.MkdirAll(home, 0o770))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".profile"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(secrets, 0o770))

	_, err := rt.Run(context.Background(), cfg)
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(home, ".profile"))
	assert.DirExists(t, secrets, "an existing (empty) secrets directory is not the start's")
	assert.NoDirExists(t, filepath.Join(agentDir, "workspace"), "the workspace this start created goes")
}

// A create that may still land (an error the server did not return as a
// refusal) keeps the directories: the instance can still appear and use
// them.
func TestCloudRunRun_CreateInDoubtKeepsAgentDirs(t *testing.T) {
	fake := &fakeInstancesClient{getErr: notFoundErr(), createErr: errors.New("connection reset")}
	rt, cfg, agentDir := rollbackTestRun(t, fake, true)

	_, err := rt.Run(context.Background(), cfg)
	require.Error(t, err)
	for _, leaf := range []string{"home-" + crTestAgentID, "secrets-" + crTestAgentID, "workspace"} {
		assert.DirExists(t, filepath.Join(agentDir, leaf))
	}
}

// No client means no create was ever issued: the directories go.
func TestCloudRunRun_NoClientRemovesProvisionedAgentDirs(t *testing.T) {
	rt, cfg, agentDir := rollbackTestRun(t, &fakeInstancesClient{}, true)
	rt.newClient = func(context.Context) (cloudrun.InstancesAPI, error) { return nil, errors.New("no credentials") }

	_, err := rt.Run(context.Background(), cfg)
	require.Error(t, err)
	assert.NoDirExists(t, agentDir)
}

// Control: a start whose create succeeds keeps everything it provisioned.
func TestCloudRunRun_SuccessfulCreateKeepsAgentDirs(t *testing.T) {
	rt, cfg, agentDir := rollbackTestRun(t, &fakeInstancesClient{getErr: notFoundErr()}, true)

	_, err := rt.Run(context.Background(), cfg)
	require.NoError(t, err)
	for _, leaf := range []string{"home-" + crTestAgentID, "secrets-" + crTestAgentID, "workspace"} {
		assert.DirExists(t, filepath.Join(agentDir, leaf))
	}
}

// An async launch stopped at the gate right before CreateInstance never
// issued a create: the agent directories it provisioned go.
func TestCloudRunRun_CreateGateAbortRemovesProvisionedAgentDirs(t *testing.T) {
	rt, cfg, agentDir := rollbackTestRun(t, &fakeInstancesClient{getErr: notFoundErr()}, true)
	rec := &sandboxHookRecorder{abortAt: CheckpointStepInstanceCreate}
	rec.install(&cfg)

	_, err := rt.Run(context.Background(), cfg)
	require.ErrorIs(t, err, errTestAbort)
	assert.NoDirExists(t, agentDir)
}

// A provision that fails part-way (here the secrets path is taken by a
// file) removes what it created (the home) and keeps what it did not:
// the existing agent directory and the file.
func TestCloudRunProvisionNFS_PartialFailureRollsBack(t *testing.T) {
	rt, cfg, agentDir := rollbackTestRun(t, &fakeInstancesClient{getErr: notFoundErr()}, true)
	require.NoError(t, os.MkdirAll(agentDir, 0o770))
	blocker := filepath.Join(agentDir, "secrets-"+crTestAgentID)
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o644))

	_, err := rt.provisionCloudRunNFS(context.Background(), cfg, crTestAgentID, os.Getuid(), os.Getgid())
	require.Error(t, err)
	assert.NoDirExists(t, filepath.Join(agentDir, "home-"+crTestAgentID), "the home this provision created goes")
	assert.NoDirExists(t, filepath.Join(agentDir, "workspace"), "the workspace this provision created goes")
	assert.FileExists(t, blocker)
	assert.DirExists(t, agentDir, "the agent directory existed before")
}
