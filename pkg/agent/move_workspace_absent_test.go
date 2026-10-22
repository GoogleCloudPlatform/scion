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

package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// moveAbsentFixture is a project whose agent can be provisioned and started
// against a mock runtime named runtimeName, with workspace_storage nfs
// mounted at mountRoot when nfs is set.
type moveAbsentFixture struct {
	mgr         *AgentManager
	projectPath string
	mountRoot   string
}

func newMoveAbsentFixture(t *testing.T, runtimeName string, nfs bool) moveAbsentFixture {
	t.Helper()
	f := newSharedDirStorageRunFixture(t)
	mountRoot := filepath.Join(t.TempDir(), "nfs")
	if nfs {
		require.NoError(t, os.MkdirAll(filepath.Join(mountRoot, "share-1"), 0o755))
		f.writeGlobalSettings(t, fmt.Sprintf(nfsWorkspaceStartYAML, mountRoot))
	}
	mgr := NewManager(&runtime.MockRuntime{
		NameFunc: func() string { return runtimeName },
		RunFunc:  func(context.Context, runtime.RunConfig) (string, error) { return "mock-id", nil },
	}).(*AgentManager)
	return moveAbsentFixture{mgr: mgr, projectPath: f.projectScionDir, mountRoot: mountRoot}
}

// moveAbsentOpts are the options of a forced move's provision of a
// clone-per-agent agent (or an empty-per-agent one), as the broker builds
// them for a create.
func moveAbsentOpts(projectPath, name string, emptyPerAgent bool) api.StartOptions {
	env := map[string]string{"SCION_PROJECT_ID": testNFSWorkspaceProjectID}
	opts := api.StartOptions{Name: name, ProjectPath: projectPath, NoAuth: true, Env: env, FreshProvision: true}
	if emptyPerAgent {
		env["SCION_WORKSPACE_MODE"] = "empty-per-agent"
		opts.EmptyPerAgentWorkspace = true
	} else {
		env["SCION_WORKSPACE_MODE"] = "clone-per-agent"
		opts.GitClone = testGitClone
	}
	return opts
}

// Path parity, local-disk branch: before provision nothing is reported;
// the directory the check names is exactly the one provision then creates,
// and a second forced move is refused there.
func TestCheckMoveWorkspaceAbsent_LocalDiskParity(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("emptyPerAgent=%v", empty), func(t *testing.T) {
			fx := newMoveAbsentFixture(t, "docker", false)
			opts := moveAbsentOpts(fx.projectPath, "test-agent", empty)

			paths, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), opts)
			require.NoError(t, err)
			require.Len(t, paths, 1, "a local-disk runtime has no export directory: %v", paths)
			assert.NoDirExists(t, paths[0])

			_, err = fx.mgr.Provision(context.Background(), opts)
			require.NoError(t, err)
			created := filepath.Join(fx.projectPath, "agents", "test-agent")
			require.DirExists(t, created)
			assert.Equal(t, created, paths[0], "the check must name the directory provision creates")

			again, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), opts)
			require.True(t, errors.Is(err, ErrMoveWorkspacePresent), "want ErrMoveWorkspacePresent, got %v", err)
			assert.Equal(t, paths, again)
			assert.Contains(t, err.Error(), created)
		})
	}
}

// Path parity, Kubernetes on an NFS-backed broker: the check also names
// the agent's own directory on the export, exactly where a provision and
// start create it, and refuses on that directory alone.
func TestCheckMoveWorkspaceAbsent_KubernetesNFSParity(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("emptyPerAgent=%v", empty), func(t *testing.T) {
			fx := newMoveAbsentFixture(t, "kubernetes", true)
			opts := moveAbsentOpts(fx.projectPath, "test-agent", empty)

			paths, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), opts)
			require.NoError(t, err)
			require.Len(t, paths, 2, "want the agent dir and its export dir: %v", paths)
			for _, p := range paths {
				assert.NoDirExists(t, p)
			}

			_, err = fx.mgr.Provision(context.Background(), opts)
			require.NoError(t, err)
			startOpts := opts
			startOpts.FreshProvision = false
			startOpts.Env = moveAbsentOpts(fx.projectPath, "test-agent", empty).Env
			_, err = fx.mgr.Start(context.Background(), startOpts)
			require.NoError(t, err)

			exportDir := nfsTestAgentDir(fx.mountRoot, "test-agent")
			require.DirExists(t, exportDir, "start creates the agent's directory on the export")
			assert.Equal(t, filepath.Join(fx.projectPath, "agents", "test-agent"), paths[0])
			assert.Equal(t, exportDir, paths[1], "the check must name the export directory start creates")

			// The export directory alone refuses a second forced move.
			require.NoError(t, os.RemoveAll(paths[0]))
			again, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), opts)
			require.True(t, errors.Is(err, ErrMoveWorkspacePresent), "want ErrMoveWorkspacePresent, got %v", err)
			assert.Equal(t, paths, again)
			assert.Contains(t, err.Error(), exportDir)
		})
	}
}

// Anything at either path refuses: an empty directory, a regular file and
// a dangling symlink each count as the agent's files.
func TestCheckMoveWorkspaceAbsent_AnyEntryRefuses(t *testing.T) {
	makers := map[string]func(t *testing.T, path string){
		"empty directory": func(t *testing.T, path string) { require.NoError(t, os.MkdirAll(path, 0o755)) },
		"regular file": func(t *testing.T, path string) {
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, nil, 0o644))
		},
		"dangling symlink": func(t *testing.T, path string) {
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "missing"), path))
		},
	}
	for name, mk := range makers {
		t.Run("agent dir/"+name, func(t *testing.T) {
			fx := newMoveAbsentFixture(t, "docker", false)
			mk(t, filepath.Join(fx.projectPath, "agents", "test-agent"))
			_, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), moveAbsentOpts(fx.projectPath, "test-agent", false))
			require.True(t, errors.Is(err, ErrMoveWorkspacePresent), "want ErrMoveWorkspacePresent, got %v", err)
		})
		t.Run("export dir/"+name, func(t *testing.T) {
			fx := newMoveAbsentFixture(t, "kubernetes", true)
			mk(t, nfsTestAgentDir(fx.mountRoot, "test-agent"))
			_, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), moveAbsentOpts(fx.projectPath, "test-agent", false))
			require.True(t, errors.Is(err, ErrMoveWorkspacePresent), "want ErrMoveWorkspacePresent, got %v", err)
		})
	}
}

// A sibling agent's directories do not refuse the move.
func TestCheckMoveWorkspaceAbsent_SiblingIgnored(t *testing.T) {
	fx := newMoveAbsentFixture(t, "kubernetes", true)
	require.NoError(t, os.MkdirAll(filepath.Join(fx.projectPath, "agents", "other-agent"), 0o755))
	require.NoError(t, os.MkdirAll(nfsTestAgentDir(fx.mountRoot, "other-agent"), 0o755))
	_, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), moveAbsentOpts(fx.projectPath, "test-agent", false))
	require.NoError(t, err)
}

// Kubernetes on NFS storage with the export not mounted here: nothing on
// the export can be confirmed absent, so the check fails closed.
func TestCheckMoveWorkspaceAbsent_ExportNotMountedFailsClosed(t *testing.T) {
	fx := newMoveAbsentFixture(t, "kubernetes", true)
	require.NoError(t, os.RemoveAll(filepath.Join(fx.mountRoot, "share-1")))
	_, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), moveAbsentOpts(fx.projectPath, "test-agent", false))
	require.True(t, errors.Is(err, ErrMoveWorkspaceUnconfirmed), "want ErrMoveWorkspaceUnconfirmed, got %v", err)
	assert.Contains(t, err.Error(), "not mounted")
}

// On a non-Kubernetes runtime an NFS-backed clone-per-agent agent keeps its
// workspace in its local agent directory, so only that path is checked.
func TestCheckMoveWorkspaceAbsent_NFSDockerChecksLocalOnly(t *testing.T) {
	fx := newMoveAbsentFixture(t, "docker", true)
	require.NoError(t, os.MkdirAll(nfsTestAgentDir(fx.mountRoot, "test-agent"), 0o755))
	paths, err := fx.mgr.CheckMoveWorkspaceAbsent(context.Background(), moveAbsentOpts(fx.projectPath, "test-agent", false))
	require.NoError(t, err)
	assert.Len(t, paths, 1)
}
