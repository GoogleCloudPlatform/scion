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
	"net/http/httptest"
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	deleteTestIDA = "0b9a4c1e-2f3d-4e5a-8b6c-7d8e9f0a1b2c"
	deleteTestIDB = "5f1e2d3c-4b5a-4987-8a6b-1c2d3e4f5a6b"
	// deleteTestIDC is the ID of a project whose name is another
	// project's ID (deleteTestIDA).
	deleteTestIDC = "7a6b5c4d-3e2f-4a1b-9c8d-0e1f2a3b4c5d"
	// deleteTestNamedUUID is a UUID-shaped name that is no project's ID.
	deleteTestNamedUUID = "2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f"
	deleteTestIDD       = "3d4e5f6a-7b8c-4d9e-8f0a-2b3c4d5e6f7a"
)

// setupProjectsDeleteTest wires a fake hub for `scion hub projects delete`
// and resets the flags the command reads.
func setupProjectsDeleteTest(t *testing.T, confirm bool) *fakeProjectsHub {
	t.Helper()
	origHome := os.Getenv("HOME")
	origProjectPath := projectPath
	origFormat, origYes, origNonInteractive := outputFormat, autoConfirm, nonInteractive
	t.Cleanup(func() {
		_ = os.Setenv("HOME", origHome)
		projectPath = origProjectPath
		outputFormat, autoConfirm, nonInteractive = origFormat, origYes, origNonInteractive
	})

	hub := &fakeProjectsHub{projects: []hubclient.Project{
		{ID: deleteTestIDA, Name: "My Project", Slug: "my-project"},
		{ID: deleteTestIDB, Name: "tools", Slug: "tools"},
		{ID: deleteTestIDC, Name: deleteTestIDA, Slug: "decoy"},
		{ID: deleteTestIDD, Name: deleteTestNamedUUID, Slug: "named-uuid"},
	}}
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	projectPath = setupEnvProject(t, tmpHome, srv.URL)

	outputFormat, autoConfirm, nonInteractive = "", confirm, false
	return hub
}

// TestRunHubProjectsDelete_Resolve covers ptone/scion#3792: `scion hub
// projects delete` accepts a project UUID as well as a name, resolving it
// exactly as `scion hub projects info` does, and an unknown UUID never
// reaches the delete call.
func TestRunHubProjectsDelete_Resolve(t *testing.T) {
	tests := []struct {
		name       string
		arg        string
		wantDelete []string
		wantErr    string
		wantReqs   []string
	}{
		{
			name:       "uuid deletes that project",
			arg:        deleteTestIDA,
			wantDelete: []string{deleteTestIDA},
			wantReqs: []string{
				"/api/v1/projects/" + deleteTestIDA,
				"/api/v1/projects/" + deleteTestIDA + "/providers",
				"/api/v1/projects/" + deleteTestIDA,
			},
		},
		{
			name:       "name deletes via name lookup only",
			arg:        "My Project",
			wantDelete: []string{deleteTestIDA},
			wantReqs: []string{
				"/api/v1/projects?name=My+Project",
				"/api/v1/projects/" + deleteTestIDA + "/providers",
				"/api/v1/projects/" + deleteTestIDA,
			},
		},
		{
			name:       "slug equal to name deletes as before",
			arg:        "tools",
			wantDelete: []string{deleteTestIDB},
			wantReqs: []string{
				"/api/v1/projects?name=tools",
				"/api/v1/projects/" + deleteTestIDB + "/providers",
				"/api/v1/projects/" + deleteTestIDB,
			},
		},
		{
			name:    "unknown uuid reports not found and deletes nothing",
			arg:     "9d8c7b6a-5f4e-4d3c-9b2a-1f0e9d8c7b6a",
			wantErr: "project '9d8c7b6a-5f4e-4d3c-9b2a-1f0e9d8c7b6a' not found",
		},
		{
			name:    "unknown name reports not found and deletes nothing",
			arg:     "missing",
			wantErr: "project 'missing' not found",
		},
		{
			// The ID match wins: the project whose name is that string is
			// not touched.
			name:       "uuid matching an ID and another project's name deletes the ID match only",
			arg:        deleteTestIDA,
			wantDelete: []string{deleteTestIDA},
		},
		{
			// Same as `hub projects info` and as delete before the change:
			// a UUID-shaped string that is no project's ID still matches a
			// project whose name is exactly that string.
			name:       "uuid-shaped name with no ID match resolves like info",
			arg:        deleteTestNamedUUID,
			wantDelete: []string{deleteTestIDD},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := setupProjectsDeleteTest(t, true)

			err := runHubProjectsDelete(hubProjectsDeleteCmd, []string{tt.arg})
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantDelete, hub.deletes, "delete calls")
			if tt.wantReqs != nil {
				assert.Equal(t, tt.wantReqs, hub.recorded())
			}

			// Delete picks the same project that info shows.
			if tt.wantErr == "" {
				client, cerr := hubclient.New(os.Getenv("SCION_HUB_ENDPOINT"))
				require.NoError(t, cerr)
				got, rerr := resolveProjectNameOrID(t.Context(), client, tt.arg)
				require.NoError(t, rerr)
				assert.Equal(t, tt.wantDelete, []string{got.ID})
			}
		})
	}
}

// TestRunHubProjectsDelete_ConfirmationUnchanged checks a project given by
// UUID still goes through the confirmation prompt: declining deletes
// nothing, accepting deletes the project.
func TestRunHubProjectsDelete_ConfirmationUnchanged(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		hub := setupProjectsDeleteTest(t, false)
		withStdin(t, "n\n")

		err := runHubProjectsDelete(hubProjectsDeleteCmd, []string{deleteTestIDB})
		require.Error(t, err)
		assert.Equal(t, "deletion cancelled", err.Error())
		assert.Empty(t, hub.deletes)
	})

	t.Run("accepted", func(t *testing.T) {
		hub := setupProjectsDeleteTest(t, false)
		withStdin(t, "y\n")

		require.NoError(t, runHubProjectsDelete(hubProjectsDeleteCmd, []string{deleteTestIDA}))
		assert.Equal(t, []string{deleteTestIDA}, hub.deletes)
	})
}

// TestHubProjectsDeleteHelpMentionsID checks the usage and help text
// advertise project ID lookup.
func TestHubProjectsDeleteHelpMentionsID(t *testing.T) {
	assert.Contains(t, hubProjectsDeleteCmd.Use, "project-name-or-id")
	assert.Contains(t, hubProjectsDeleteCmd.Long, "project ID (UUID)")
}
