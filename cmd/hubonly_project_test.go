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
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
)

const (
	// hubOnlyLocalID is a local project_id the hub never knows.
	hubOnlyLocalID = "local-0000-4000-8000-000000000009"
	// unlinkedMessage is the error for a directory project named myrepo
	// that is not linked to a hub.
	unlinkedMessage = "directory project myrepo is not linked to a hub; pass --project or run scion hub link"
)

// makeHubOnlyDirProject creates the directory project <tmp>/myrepo with
// settings as its .scion/settings.yaml, and moves into it.
func makeHubOnlyDirProject(t *testing.T, settings string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "myrepo")
	dotScion := filepath.Join(root, ".scion")
	require.NoError(t, os.MkdirAll(dotScion, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dotScion, "settings.yaml"), []byte(settings), 0o644))
	t.Chdir(root)
}

// The project of tunnel, ssh and ssh-config follows resolveInvocationTarget:
// --project, then the directory project, then the current context.
func TestResolveHubOnlyProject(t *testing.T) {
	tests := []struct {
		name      string
		noContext bool
		setup     func(t *testing.T)
		want      hubOnlyProject
		wantHub   string
	}{
		{
			name:    "the context project applies without a flag",
			want:    hubOnlyProject{ref: ctxTestProjectID, label: "foo", source: hubOnlySourceContext},
			wantHub: ctxTestHub,
		},
		{
			name:    "--project overrides the context",
			setup:   func(t *testing.T) { projectPath = "bar" },
			want:    hubOnlyProject{ref: "bar", label: "bar", source: hubOnlySourceFlag},
			wantHub: ctxTestHub,
		},
		{
			name:    "--hub overrides the context and its project",
			setup:   func(t *testing.T) { hubEndpoint = "https://other.example.com" },
			want:    hubOnlyProject{},
			wantHub: "https://other.example.com",
		},
		{
			name: "global settings are not a directory project: the context applies and the local id is never used",
			setup: func(t *testing.T) {
				writeHomeGlobalSettings(t, "project_id: "+hubOnlyLocalID+"\nhub:\n  endpoint: "+ctxTestHub+"\n")
			},
			want:    hubOnlyProject{ref: ctxTestProjectID, label: "foo", source: hubOnlySourceContext},
			wantHub: ctxTestHub,
		},
		{
			name:      "global settings without a context give no project",
			noContext: true,
			setup: func(t *testing.T) {
				writeHomeGlobalSettings(t, "project_id: "+hubOnlyLocalID+"\nhub:\n  endpoint: "+ctxTestHub+"\n")
			},
			want: hubOnlyProject{},
		},
		{
			name: "a directory project with hub.projectId beats the context",
			setup: func(t *testing.T) {
				makeHubOnlyDirProject(t, "project_id: "+hubOnlyLocalID+"\nhub:\n  projectId: hub-dir-id\n")
			},
			want:    hubOnlyProject{ref: "hub-dir-id", label: "myrepo", source: hubOnlySourceDirectory},
			wantHub: ctxTestHub,
		},
		{
			name:      "a directory project works without a context",
			noContext: true,
			setup: func(t *testing.T) {
				makeHubOnlyDirProject(t, "hub:\n  projectId: hub-dir-id\n")
			},
			want: hubOnlyProject{ref: "hub-dir-id", label: "myrepo", source: hubOnlySourceDirectory},
		},
		{
			name: "a linked directory project without hub.projectId uses its project_id",
			setup: func(t *testing.T) {
				makeHubOnlyDirProject(t, "project_id: linked-dir-id\nhub:\n  linked: true\n")
			},
			want:    hubOnlyProject{ref: "linked-dir-id", label: "myrepo", source: hubOnlySourceDirectory},
			wantHub: ctxTestHub,
		},
		{
			name: "an unlinked directory project hides the context and sends no local id",
			setup: func(t *testing.T) {
				makeHubOnlyDirProject(t, "project_id: "+hubOnlyLocalID+"\n")
			},
			want:    hubOnlyProject{unlinkedDir: "myrepo"},
			wantHub: ctxTestHub,
		},
		{
			name: "a global hub.projectId never stands in for an unlinked directory project",
			setup: func(t *testing.T) {
				writeHomeGlobalSettings(t, "hub:\n  projectId: global-hub-id\n")
				makeHubOnlyDirProject(t, "project_id: "+hubOnlyLocalID+"\n")
			},
			want:    hubOnlyProject{unlinkedDir: "myrepo"},
			wantHub: ctxTestHub,
		},
		{
			name: "a linked versioned directory project uses its hub.project_id",
			setup: func(t *testing.T) {
				makeHubOnlyDirProject(t, "schema_version: \"1\"\nhub:\n  linked: true\n  project_id: v1-dir-id\n")
			},
			want:    hubOnlyProject{ref: "v1-dir-id", label: "myrepo", source: hubOnlySourceDirectory},
			wantHub: ctxTestHub,
		},
		{
			name: "an unlinked versioned directory project sends no id",
			setup: func(t *testing.T) {
				makeHubOnlyDirProject(t, "schema_version: \"1\"\nhub:\n  project_id: "+hubOnlyLocalID+"\n")
			},
			want:    hubOnlyProject{unlinkedDir: "myrepo"},
			wantHub: ctxTestHub,
		},
		{
			name:      "no directory project and no context give no project",
			noContext: true,
			want:      hubOnlyProject{},
		},
	}
	for _, command := range []string{"tunnel", "ssh", "ssh-config"} {
		for _, tt := range tests {
			t.Run(command+": "+tt.name, func(t *testing.T) {
				setupContextTest(t)
				if !tt.noContext {
					saveTestContext(t, ctxTestHub, ctxTestProjectID)
				}
				if tt.setup != nil {
					tt.setup(t)
				}
				require.NoError(t, resolveInvocationTarget(findCommand(t, command), ModeHuman))
				got := resolveHubOnlyProject()
				assert.Equal(t, tt.want, got)
				assert.NotEqual(t, hubOnlyLocalID, got.ref, "a local project id must never be sent")
				assert.NotEqual(t, "global-hub-id", got.ref, "the global settings must never be used")
				assert.Equal(t, tt.wantHub, hubEndpoint, "hubEndpoint")
			})
		}
	}
}

func TestHubOnlyProjectDescribe(t *testing.T) {
	tests := []struct {
		p    hubOnlyProject
		want string
	}{
		{hubOnlyProject{ref: "p1", label: "p1", source: hubOnlySourceFlag}, "project p1 (from --project)"},
		{hubOnlyProject{ref: ctxTestProjectID, label: "foo", source: hubOnlySourceContext}, "project foo (from the scion use context)"},
		{hubOnlyProject{ref: ctxTestProjectID, source: hubOnlySourceContext}, "the project selected with scion use"},
		{hubOnlyProject{ref: "hub-dir-id", label: "myrepo", source: hubOnlySourceDirectory}, "project myrepo (from the directory project)"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, tt.p.describe())
		assert.NotContains(t, tt.p.notFoundError("https://hub.example.com").Error(), ctxTestProjectID)
	}
	err := hubOnlyProject{ref: ctxTestProjectID, label: "foo", source: hubOnlySourceContext}.notFoundError("https://hub.example.com")
	assert.Equal(t, "project foo (from the scion use context) not found on hub https://hub.example.com", err.Error())
	assert.Equal(t, tunnelclient.ExitAgentNotFound, tunnelclient.CodeOf(err))
}

// ssh and ssh-config take their project from the current context.
func TestDefaultSSHCommandEnv_UsesContextProject(t *testing.T) {
	setupContextTest(t)
	saveTestContext(t, ctxTestHub, ctxTestProjectID)
	writeHomeGlobalSettings(t, "project_id: "+hubOnlyLocalID+"\n")
	for _, command := range []string{"ssh", "ssh-config"} {
		require.NoError(t, resolveInvocationTarget(findCommand(t, command), ModeHuman))
		env, err := defaultSSHCommandEnv()
		require.NoError(t, err)
		assert.Equal(t, hubOnlyProject{ref: ctxTestProjectID, label: "foo", source: hubOnlySourceContext}, env.project, command)
		assert.Equal(t, ctxTestHub, env.endpoint, command)
		resetContextTargets()
	}
}

// A project the hub does not know is named by slug and source, never by
// the id sent; the exit codes are unchanged.
func TestSSH_UnknownDefaultProjectNamesSource(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	missing := hubOnlyProject{ref: ctxTestProjectID, label: "foo", source: hubOnlySourceContext}
	want := "project foo (from the scion use context) not found on hub " + hub.srv.URL

	t.Run("ssh-config", func(t *testing.T) {
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		env.project = missing
		assert.Equal(t, tunnelclient.ExitAgentNotFound, runSSHConfig(context.Background(), env, []string{"c"}, false))
		assert.Contains(t, errb.String(), want)
		assert.NotContains(t, errb.String(), ctxTestProjectID)
	})
	t.Run("ssh", func(t *testing.T) {
		installFakeSSHClient(t, "0")
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		env.project = missing
		assert.Equal(t, sshExitFailure, runSSH(context.Background(), env, []string{"c"}))
		assert.Contains(t, errb.String(), want)
		assert.NotContains(t, errb.String(), ctxTestProjectID)
	})
}

// An unlinked directory project is a usage error for a bare agent in all
// three commands; a <project>/<agent> reference still works in ssh and
// ssh-config. (scion ssh reports every local failure with 255.)
func TestHubOnly_UnlinkedDirectoryProject(t *testing.T) {
	unlinked := hubOnlyProject{unlinkedDir: "myrepo"}

	t.Run("tunnel", func(t *testing.T) {
		code, stderr := tunnelRunCLIWith(t, nil, func() {
			t.Setenv("SCION_AGENT_ID", "") // a hub-managed agent ignores the context
			saveTestContext(t, "http://127.0.0.1:1", ctxTestProjectID)
			makeHubOnlyDirProject(t, "project_id: "+hubOnlyLocalID+"\n")
		}, "tunnel", "--stdio", "--port", "22", "--hub", "http://127.0.0.1:1", "a1")
		t.Cleanup(resetContextTargets)
		assert.Equal(t, tunnelclient.ExitUsage, code, "stderr %q", stderr)
		assert.Contains(t, stderr, unlinkedMessage)
		tunnelAssertOneLine(t, stderr)
	})
	t.Run("ssh-config", func(t *testing.T) {
		hub := twoProjectHub(t, "tok")
		env, out, errb := newSSHTestEnv(t, t.TempDir(), hub)
		env.project = unlinked
		assert.Equal(t, tunnelclient.ExitUsage, runSSHConfig(context.Background(), env, []string{"b-c"}, false))
		assert.Contains(t, errb.String(), unlinkedMessage)
		errb.Reset()
		require.Equal(t, tunnelclient.ExitOK, runSSHConfig(context.Background(), env, []string{"a/b-c"}, false), errb.String())
		assert.Contains(t, out.String(), "Host scion."+hub.label(t)+".a.b-c\n")
	})
	t.Run("ssh", func(t *testing.T) {
		hub := twoProjectHub(t, "tok")
		installFakeSSHClient(t, "0")
		env, _, errb := newSSHTestEnv(t, t.TempDir(), hub)
		env.project = unlinked
		assert.Equal(t, sshExitFailure, runSSH(context.Background(), env, []string{"b-c"}))
		assert.Contains(t, errb.String(), unlinkedMessage)
		errb.Reset()
		assert.Equal(t, tunnelclient.ExitOK, runSSH(context.Background(), env, []string{"a/b-c"}), errb.String())
	})
}

// A hub.projectId in the global settings never stands in for an unlinked
// directory project: tunnel exits 2 with the link hint and sends nothing.
func TestTunnelCLI_UnlinkedDirectoryIgnoresGlobalHubProjectID(t *testing.T) {
	code, stderr := tunnelRunCLIWith(t, nil, func() {
		writeHomeGlobalSettings(t, "hub:\n  projectId: global-hub-id\n")
		makeHubOnlyDirProject(t, "project_id: "+hubOnlyLocalID+"\n")
	}, "tunnel", "--stdio", "--port", "22", "--hub", "http://127.0.0.1:1", "a1")
	t.Cleanup(resetContextTargets)
	assert.Equal(t, tunnelclient.ExitUsage, code, "stderr %q", stderr)
	assert.Contains(t, stderr, unlinkedMessage)
	assert.NotContains(t, stderr, "global-hub-id")
}

// tunnel takes its project from the current context: the request passes
// validation and fails later, on the missing credential.
func TestTunnelCLI_UsesContextProject(t *testing.T) {
	code, stderr := tunnelRunCLIWith(t, nil, func() {
		t.Setenv("SCION_AGENT_ID", "") // a hub-managed agent ignores the context
		saveTestContext(t, "http://127.0.0.1:1", ctxTestProjectID)
		writeHomeGlobalSettings(t, "project_id: "+hubOnlyLocalID+"\n")
	}, "tunnel", "--stdio", "--port", "22", "a1")
	t.Cleanup(resetContextTargets)
	assert.Equal(t, tunnelclient.ExitCredential, code, "stderr %q", stderr)
	assert.NotContains(t, stderr, hubOnlyLocalID)
}

// Without --project, a directory project or a context, tunnel still asks
// for --project.
func TestTunnelCLI_NoProjectAnywhere(t *testing.T) {
	code, stderr := tunnelRunCLIWith(t, nil, func() {
		writeHomeGlobalSettings(t, "project_id: "+hubOnlyLocalID+"\n")
	}, "tunnel", "--stdio", "--port", "22", "--hub", "http://127.0.0.1:1", "a1")
	assert.Equal(t, tunnelclient.ExitUsage, code, "stderr %q", stderr)
	assert.Contains(t, stderr, "no project: pass --project")
}

// The tunnel's agent lookup records a missing project or agent, and only
// then is the error reworded to name the project and its source.
func TestTunnelHub_LookupMissNamesSource(t *testing.T) {
	hub := twoProjectHub(t, "tok")
	cred := tunnelclient.Credential{Token: "tok", Kind: tunnelclient.CredentialToken}
	for _, tc := range []struct {
		name, project, agent string
		missed               bool
	}{
		{"found", "a", "b-c", false},
		{"unknown project", ctxTestProjectID, "b-c", true},
		{"unknown agent", "a", "nope", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := &tunnelHub{endpoint: hub.srv.URL}
			_, err := th.ResolveAgent(context.Background(), cred, tc.project, tc.agent)
			assert.Equal(t, tc.missed, err != nil)
			assert.Equal(t, tc.missed, th.lookupMissed.Load())
		})
	}

	target := hubOnlyProject{ref: ctxTestProjectID, label: "foo", source: hubOnlySourceContext}
	raw := tunnelclient.Errorf(tunnelclient.ExitAgentNotFound, "agent %q not found in project %q", "a1", ctxTestProjectID)
	got := tunnelNotFoundError(raw, true, "a1", target)
	assert.Equal(t, "agent a1 not found in project foo (from the scion use context)", got.Error())
	assert.Equal(t, tunnelclient.ExitAgentNotFound, got.Code)
	assert.Same(t, raw, tunnelNotFoundError(raw, false, "a1", target), "no lookup miss: unchanged")
	other := tunnelclient.Errorf(tunnelclient.ExitHubUnavailable, "down")
	assert.Same(t, other, tunnelNotFoundError(other, true, "a1", target), "other codes: unchanged")
}
