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

package runtimebroker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	brokerTestHubID   = "7c1d2e3f-4a5b-4c6d-8e9f-0a1b2c3d4e5f"
	brokerTestLocalID = "2b3c4d5e-6f70-4182-93a4-b5c6d7e8f901"
	brokerTestSlug    = "remote-clone"
)

func notBusy() (bool, error) { return false, nil }

// seedBrokerWorkspace creates ~/.scion/projects/<slug> under a fresh HOME
// with the given identity (project-id file or marker) and a populated
// project config directory for it. It returns the workspace path and that
// project config directory.
func seedBrokerWorkspace(t *testing.T, id string, markerForm bool) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace := filepath.Join(home, config.GlobalDir, "projects", brokerTestSlug)
	require.NoError(t, os.MkdirAll(workspace, 0755))
	scionPath := filepath.Join(workspace, config.DotScion)
	if markerForm {
		require.NoError(t, config.WriteProjectMarker(scionPath, &config.ProjectMarker{
			ProjectID: id, ProjectName: brokerTestSlug, ProjectSlug: brokerTestSlug,
		}))
	} else {
		require.NoError(t, os.MkdirAll(scionPath, 0755))
		require.NoError(t, config.WriteProjectID(scionPath, id))
	}
	root, ok := config.ConfinedProjectConfigRoot(brokerTestSlug, id)
	require.True(t, ok)
	require.NoError(t, os.MkdirAll(filepath.Join(root, config.DotScion, "agents", "a1"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, config.DotScion, "agents", "a1", "notes.txt"), []byte("a1"), 0644))
	return workspace, root
}

func writeBrokerRecord(t *testing.T, content string) {
	t.Helper()
	path, err := config.BrokerWorkspaceRecordPath(brokerTestSlug)
	require.NoError(t, err)
	require.NoError(t, config.WriteWorkspaceRecord(path, content))
}

func writeHubRecord(t *testing.T, content string) {
	t.Helper()
	path, err := config.HubWorkspaceRecordPath(brokerTestSlug)
	require.NoError(t, err)
	require.NoError(t, config.WriteWorkspaceRecord(path, content))
}

// brokerConfigRoot resolves the workspace's project config directory from
// its .scion entry, as project resolution does.
func brokerConfigRoot(t *testing.T, workspace string) string {
	t.Helper()
	scionPath := filepath.Join(workspace, config.DotScion)
	var ext string
	var err error
	if config.IsProjectMarkerFile(scionPath) {
		ext, err = config.ResolveProjectMarker(scionPath)
	} else {
		ext, err = config.GetGitProjectExternalConfigDir(scionPath)
	}
	require.NoError(t, err)
	return filepath.Dir(ext)
}

func identityForms(t *testing.T, fn func(t *testing.T, markerForm bool)) {
	for _, tc := range []struct {
		name       string
		markerForm bool
	}{{"project-id file", false}, {"marker file", true}} {
		t.Run(tc.name, func(t *testing.T) { fn(t, tc.markerForm) })
	}
}

func TestRecordHubProjectIdentity_BrokerCopyUsesHubIDDir(t *testing.T) {
	identityForms(t, func(t *testing.T, markerForm bool) {
		workspace, previous := seedBrokerWorkspace(t, brokerTestLocalID, markerForm)
		writeBrokerRecord(t, brokerTestHubID)

		outcome, err := recordHubProjectIdentity(workspace, brokerTestSlug, brokerTestHubID, notBusy)
		require.NoError(t, err)
		assert.Equal(t, hubIdentityRecorded, outcome)

		want, ok := config.ConfinedProjectConfigRoot(brokerTestSlug, brokerTestHubID)
		require.True(t, ok)
		assert.Equal(t, want, brokerConfigRoot(t, workspace))
		assert.Equal(t, "remote-clone__7c1d2e3f", filepath.Base(want))
		assert.DirExists(t, previous, "the previous directory is not moved")

		// A second run is a no-op.
		outcome, err = recordHubProjectIdentity(workspace, brokerTestSlug, brokerTestHubID, notBusy)
		require.NoError(t, err)
		assert.Equal(t, hubIdentityMatching, outcome)
	})
}

func TestRecordHubProjectIdentity_HubOwnWorkspaceUnchanged(t *testing.T) {
	// A hub on this host (in the same process or a separate process with
	// the same HOME) records its own workspaces under hub-workspaces.
	for _, tc := range []struct {
		name, brokerRecord string
	}{
		{"without broker record", ""},
		{"with broker record", brokerTestHubID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identityForms(t, func(t *testing.T, markerForm bool) {
				workspace, previous := seedBrokerWorkspace(t, brokerTestLocalID, markerForm)
				writeHubRecord(t, brokerTestHubID)
				if tc.brokerRecord != "" {
					writeBrokerRecord(t, tc.brokerRecord)
				}

				outcome, err := recordHubProjectIdentity(workspace, brokerTestSlug, brokerTestHubID, notBusy)
				require.NoError(t, err)
				assert.Equal(t, hubIdentityNotBrokerCopy, outcome)
				assert.Equal(t, previous, brokerConfigRoot(t, workspace))
			})
		})
	}
}

func TestRecordHubProjectIdentity_RequiresExactBrokerRecord(t *testing.T) {
	for _, tc := range []struct {
		name   string
		record *string
	}{
		{"no record", nil},
		{"other project", ptr(brokerTestLocalID)},
		{"trailing newline", ptr(brokerTestHubID + "\n")},
		{"upper case", ptr("7C1D2E3F-4A5B-4C6D-8E9F-0A1B2C3D4E5F")},
		{"empty", ptr("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, previous := seedBrokerWorkspace(t, brokerTestLocalID, false)
			if tc.record != nil {
				writeBrokerRecord(t, *tc.record)
			}
			outcome, err := recordHubProjectIdentity(workspace, brokerTestSlug, brokerTestHubID, notBusy)
			require.NoError(t, err)
			assert.Equal(t, hubIdentityNotBrokerCopy, outcome)
			assert.Equal(t, previous, brokerConfigRoot(t, workspace))
		})
	}
}

func TestRecordHubProjectIdentity_OtherProjectPathUnchanged(t *testing.T) {
	_, _ = seedBrokerWorkspace(t, brokerTestLocalID, false)
	writeBrokerRecord(t, brokerTestHubID)

	linked := filepath.Join(t.TempDir(), brokerTestSlug)
	require.NoError(t, os.MkdirAll(filepath.Join(linked, config.DotScion), 0755))
	require.NoError(t, config.WriteProjectID(filepath.Join(linked, config.DotScion), brokerTestLocalID))

	outcome, err := recordHubProjectIdentity(linked, brokerTestSlug, brokerTestHubID, notBusy)
	require.NoError(t, err)
	assert.Equal(t, hubIdentityNotBrokerCopy, outcome)
	id, err := config.ReadProjectID(filepath.Join(linked, config.DotScion))
	require.NoError(t, err)
	assert.Equal(t, brokerTestLocalID, id)
}

func TestRecordHubProjectIdentity_PopulatedHubIDDirSkips(t *testing.T) {
	workspace, previous := seedBrokerWorkspace(t, brokerTestLocalID, false)
	writeBrokerRecord(t, brokerTestHubID)
	want, ok := config.ConfinedProjectConfigRoot(brokerTestSlug, brokerTestHubID)
	require.True(t, ok)
	require.NoError(t, os.MkdirAll(want, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(want, "settings.yaml"), []byte("x"), 0644))

	outcome, err := recordHubProjectIdentity(workspace, brokerTestSlug, brokerTestHubID, notBusy)
	require.NoError(t, err)
	assert.Equal(t, hubIdentityTargetExists, outcome)
	assert.Equal(t, previous, brokerConfigRoot(t, workspace))
}

func TestRecordHubProjectIdentity_AgentsInUseSkip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inUse func() (bool, error)
	}{
		{"in use", func() (bool, error) { return true, nil }},
		{"listing failed", func() (bool, error) { return false, errors.New("list failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace, previous := seedBrokerWorkspace(t, brokerTestLocalID, false)
			writeBrokerRecord(t, brokerTestHubID)
			outcome, err := recordHubProjectIdentity(workspace, brokerTestSlug, brokerTestHubID, tc.inUse)
			require.NoError(t, err)
			assert.Equal(t, hubIdentityInUse, outcome)
			assert.Equal(t, previous, brokerConfigRoot(t, workspace))
		})
	}
}

func TestRecordHubProjectIdentity_IdentityOutsideExpectedFormSkips(t *testing.T) {
	workspace, _ := seedBrokerWorkspace(t, brokerTestLocalID, false)
	writeBrokerRecord(t, brokerTestHubID)
	scionPath := filepath.Join(workspace, config.DotScion)
	require.NoError(t, config.WriteProjectID(scionPath, "../../other"))

	outcome, err := recordHubProjectIdentity(workspace, brokerTestSlug, brokerTestHubID, notBusy)
	require.NoError(t, err)
	assert.Equal(t, hubIdentityUnexpectedForm, outcome)
	id, err := config.ReadProjectID(scionPath)
	require.NoError(t, err)
	assert.Equal(t, "../../other", id)
}

func TestOtherProjectAgentsInUse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		agents []api.AgentInfo
		err    error
		want   bool
	}{
		{"none", nil, nil, false},
		{"only the starting agent", []api.AgentInfo{{ID: "self", Phase: "running"}}, nil, false},
		{"stopped", []api.AgentInfo{{ID: "other", Phase: "stopped"}}, nil, false},
		{"running", []api.AgentInfo{{ID: "other", Phase: "running"}}, nil, true},
		{"created", []api.AgentInfo{{ID: "other", Phase: "created"}}, nil, true},
		{"other project", []api.AgentInfo{{ID: "other", Phase: "running", Labels: map[string]string{"scion.project_id": "different"}}}, nil, false},
		{"listing failed", nil, errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := New(DefaultServerConfig(), &mockManager{agents: tc.agents, listErr: tc.err}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})
			got, _ := srv.otherProjectAgentsInUse(context.Background(), brokerTestHubID, "self")
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRecordBrokerWorkspaceCopy(t *testing.T) {
	t.Run("fresh workspace", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		require.NoError(t, recordBrokerWorkspaceCopy(brokerTestSlug, brokerTestHubID, false))
		path, err := config.BrokerWorkspaceRecordPath(brokerTestSlug)
		require.NoError(t, err)
		got, err := config.ReadWorkspaceRecord(path)
		require.NoError(t, err)
		assert.Equal(t, brokerTestHubID, got)
	})
	t.Run("workspace present before", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		require.NoError(t, recordBrokerWorkspaceCopy(brokerTestSlug, brokerTestHubID, true))
		path, err := config.BrokerWorkspaceRecordPath(brokerTestSlug)
		require.NoError(t, err)
		assert.NoFileExists(t, path)
	})
	t.Run("hub workspace on this host", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		writeHubRecord(t, brokerTestHubID)
		require.NoError(t, recordBrokerWorkspaceCopy(brokerTestSlug, brokerTestHubID, false))
		path, err := config.BrokerWorkspaceRecordPath(brokerTestSlug)
		require.NoError(t, err)
		assert.NoFileExists(t, path)
	})
}

func TestAlignHubManagedProjectIdentity_StartPathUsesHubIDDir(t *testing.T) {
	workspace, _ := seedBrokerWorkspace(t, brokerTestLocalID, false)
	writeBrokerRecord(t, brokerTestHubID)
	srv := New(DefaultServerConfig(), &mockManager{}, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	srv.alignHubManagedProjectIdentity(context.Background(), "agent-1", workspace, brokerTestSlug, brokerTestHubID)

	want, ok := config.ConfinedProjectConfigRoot(brokerTestSlug, brokerTestHubID)
	require.True(t, ok)
	assert.Equal(t, want, brokerConfigRoot(t, workspace))
}

func ptr(s string) *string { return &s }
