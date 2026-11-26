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
	"bytes"
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeProjectGitHubProjects struct {
	hubclient.ProjectService
	info      *hubclient.ProjectGitHub
	getErr    error
	bindCalls []int64
	bindResp  *hubclient.BindGitHubInstallationResponse
	bindErr   error
	unbindHit bool
	unbindErr error
}

func (f *fakeProjectGitHubProjects) GetGitHub(_ context.Context, _ string) (*hubclient.ProjectGitHub, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.info, nil
}

func (f *fakeProjectGitHubProjects) BindGitHubInstallation(_ context.Context, projectID string, installationID int64) (*hubclient.BindGitHubInstallationResponse, error) {
	f.bindCalls = append(f.bindCalls, installationID)
	if f.bindErr != nil {
		return nil, f.bindErr
	}
	if f.bindResp != nil {
		return f.bindResp, nil
	}
	return &hubclient.BindGitHubInstallationResponse{
		ProjectID:      projectID,
		InstallationID: installationID,
		Status:         "associated",
	}, nil
}

func (f *fakeProjectGitHubProjects) UnbindGitHubInstallation(_ context.Context, _ string) error {
	f.unbindHit = true
	return f.unbindErr
}

type fakeProjectGitHubApp struct {
	hubclient.GitHubAppService
	installations []hubclient.GitHubInstallation
	listErr       error
}

func (f *fakeProjectGitHubApp) ListInstallations(_ context.Context, _ *hubclient.ListGitHubInstallationsOptions) ([]hubclient.GitHubInstallation, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.installations, nil
}

func TestProjectGitHubStatus_Output(t *testing.T) {
	instID := int64(4412)
	basis := "creator_pull"
	projects := &fakeProjectGitHubProjects{
		info: &hubclient.ProjectGitHub{
			ProjectID: "proj-1",
			App: hubclient.ProjectGitHubApp{
				Configured: true,
				Slug:       "scion-bot",
				WebHost:    "github.com",
			},
			Remote: hubclient.ProjectGitHubRemote{
				Owner:     "acme",
				Repo:      "api",
				OnAppHost: true,
			},
			Association: hubclient.ProjectGitHubAssociation{
				InstallationID: &instID,
				AccountLogin:   "acme",
				Basis:          &basis,
				WriteAllowed:   false,
				Authorized:     true,
				Verified:       true,
			},
			Status: &hubclient.GitHubAppProjectStatus{
				State: "ok",
			},
			Permissions: hubclient.ProjectGitHubPermissions{
				Mode:          "custom",
				AppGrantKnown: true,
				Requested: map[string]string{
					"contents": "write",
					"issues":   "write",
				},
				AppGranted: map[string]string{
					"contents": "write",
					"issues":   "read",
				},
				Effective: map[string]string{
					"contents": "read",
					"issues":   "read",
				},
				Capped: []string{"contents", "issues"},
				CapReasons: map[string]string{
					"contents": "association_read_only",
					"issues":   "app_grant",
				},
			},
		},
	}

	prevFmt := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prevFmt })

	var buf bytes.Buffer
	err := runProjectGitHubStatus(context.Background(), projects, "proj-1", &buf)
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "acme/api (github.com)")
	assert.Contains(t, out, "@scion-bot (configured)")
	assert.Contains(t, out, "acme (#4412)")
	assert.Contains(t, out, "creator_pull (read-only)")
	assert.Contains(t, out, "Token Permissions (mode: custom)")
	assert.Contains(t, out, "association is read-only")
	assert.Contains(t, out, "app not granted write")

	// Also verify bound_by_name, unverified state, and unloaded app permissions.
	adminBasis := "admin_binding"
	projects.info.Association = hubclient.ProjectGitHubAssociation{
		InstallationID: &instID,
		AccountLogin:   "acme",
		Basis:          &adminBasis,
		WriteAllowed:   true,
		BoundBy:        "user-uuid",
		BoundByName:    "Dana Admin",
		BoundAt:        "2026-11-20T12:00:00Z",
		Authorized:     true,
		Verified:       true,
	}
	projects.info.Permissions.AppGrantKnown = false
	projects.info.Permissions.AppGranted = nil
	buf.Reset()
	require.NoError(t, runProjectGitHubStatus(context.Background(), projects, "proj-1", &buf))
	assert.Contains(t, buf.String(), "admin_binding (Dana Admin, 2026-11-20T12:00:00Z)")
	assert.Contains(t, buf.String(), "App permissions not yet loaded.")

	projects.info.Association = hubclient.ProjectGitHubAssociation{
		InstallationID: &instID,
		AccountLogin:   "acme",
		Authorized:     false,
		Verified:       false,
		ErrorCode:      "association_unverified",
		Reason:         "check unavailable",
	}
	buf.Reset()
	require.NoError(t, runProjectGitHubStatus(context.Background(), projects, "proj-1", &buf))
	assert.Contains(t, buf.String(), "unverified (check unavailable)")
}

func TestProjectGitHubStatus_StripsControlChars(t *testing.T) {
	prevFmt := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prevFmt })

	instID := int64(4412)
	adminBasis := "admin_binding"
	projects := &fakeProjectGitHubProjects{
		info: &hubclient.ProjectGitHub{
			ProjectID: "proj-1",
			App:       hubclient.ProjectGitHubApp{Configured: true, Slug: "scion-bot", WebHost: "github.com"},
			Remote:    hubclient.ProjectGitHubRemote{Owner: "acme", Repo: "api", OnAppHost: true},
			Association: hubclient.ProjectGitHubAssociation{
				InstallationID: &instID,
				AccountLogin:   "acme\x1b[31m-evil\x1b[0m",
				Basis:          &adminBasis,
				WriteAllowed:   true,
				BoundBy:        "user-uuid",
				BoundByName:    "Dana\x1b]0;pwn\x07 Admin",
				BoundAt:        "2026-11-20T12:00:00Z",
				Authorized:     true,
				Verified:       true,
			},
			Status: &hubclient.GitHubAppProjectStatus{
				State:        "error",
				ErrorMessage: "bad\x1b[2Jtoken\x07",
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, runProjectGitHubStatus(context.Background(), projects, "proj-1", &buf))
	out := buf.String()
	assert.NotContains(t, out, "\x1b", "ANSI/OSC escape sequences must be stripped")
	assert.NotContains(t, out, "\x07", "BEL terminator must be stripped")
	assert.Contains(t, out, "acme[31m-evil[0m (#4412)")
	assert.Contains(t, out, "Dana]0;pwn Admin")
	assert.Contains(t, out, "bad[2Jtoken")

	// Also verify reason field in the unverified/unauthorized branch.
	projects.info.Association = hubclient.ProjectGitHubAssociation{
		InstallationID: &instID,
		AccountLogin:   "acme\x1b]0;title\x07",
		Authorized:     false,
		Verified:       false,
		ErrorCode:      "association_unverified",
		Reason:         "check\x1b[31m failed\x1b[0m",
	}
	buf.Reset()
	require.NoError(t, runProjectGitHubStatus(context.Background(), projects, "proj-1", &buf))
	out = buf.String()
	assert.NotContains(t, out, "\x1b")
	assert.NotContains(t, out, "\x07")
	assert.Contains(t, out, "unverified (check[31m failed[0m)")
}

func TestProjectGitHubBind_AutoSelectAndErrors(t *testing.T) {
	prevFmt := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prevFmt })

	baseInfo := &hubclient.ProjectGitHub{
		ProjectID: "proj-1",
		App:       hubclient.ProjectGitHubApp{Configured: true, WebHost: "github.com"},
		Remote:    hubclient.ProjectGitHubRemote{Owner: "Acme", Repo: "api", OnAppHost: true},
	}

	t.Run("auto-selects single matching active installation (case-insensitive)", func(t *testing.T) {
		projects := &fakeProjectGitHubProjects{info: baseInfo}
		ghApp := &fakeProjectGitHubApp{
			installations: []hubclient.GitHubInstallation{
				{InstallationID: 1001, AccountLogin: "other-org", AccountType: "Organization", Status: "active"},
				{InstallationID: 2002, AccountLogin: "acme", AccountType: "Organization", Status: "suspended"},
				{InstallationID: 4412, AccountLogin: "acme", AccountType: "Organization", Status: "active"},
			},
		}
		var buf bytes.Buffer
		err := runProjectGitHubBind(context.Background(), projects, ghApp, "proj-1", 0, &buf)
		require.NoError(t, err)
		assert.Equal(t, []int64{4412}, projects.bindCalls)
		assert.Contains(t, buf.String(), "acme (#4412)")
	})

	t.Run("errors when zero matching active installations", func(t *testing.T) {
		projects := &fakeProjectGitHubProjects{info: baseInfo}
		ghApp := &fakeProjectGitHubApp{
			installations: []hubclient.GitHubInstallation{
				{InstallationID: 1001, AccountLogin: "other-org", AccountType: "Organization", Status: "active"},
				{InstallationID: 2002, AccountLogin: "acme", AccountType: "Organization", Status: "suspended"},
			},
		}
		var buf bytes.Buffer
		err := runProjectGitHubBind(context.Background(), projects, ghApp, "proj-1", 0, &buf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), `no active GitHub App installation found matching owner "Acme"`)
		assert.Empty(t, projects.bindCalls)
	})

	t.Run("errors listing candidates when multiple active installations match", func(t *testing.T) {
		projects := &fakeProjectGitHubProjects{info: baseInfo}
		ghApp := &fakeProjectGitHubApp{
			installations: []hubclient.GitHubInstallation{
				{InstallationID: 4412, AccountLogin: "acme", AccountType: "Organization", Status: "active"},
				{InstallationID: 5512, AccountLogin: "ACME", AccountType: "Organization", Status: "active"},
			},
		}
		var buf bytes.Buffer
		err := runProjectGitHubBind(context.Background(), projects, ghApp, "proj-1", 0, &buf)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "multiple active GitHub App installations match")
		assert.Contains(t, err.Error(), "4412")
		assert.Contains(t, err.Error(), "5512")
		assert.Contains(t, err.Error(), "--installation")
		assert.Empty(t, projects.bindCalls)
	})

	t.Run("explicit --installation binds directly without listing", func(t *testing.T) {
		projects := &fakeProjectGitHubProjects{info: baseInfo}
		ghApp := &fakeProjectGitHubApp{}
		var buf bytes.Buffer
		err := runProjectGitHubBind(context.Background(), projects, ghApp, "proj-1", 9999, &buf)
		require.NoError(t, err)
		assert.Equal(t, []int64{9999}, projects.bindCalls)
		assert.Contains(t, buf.String(), "#9999")
	})
}

func TestProjectGitHubUnbind(t *testing.T) {
	prevFmt := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = prevFmt })

	projects := &fakeProjectGitHubProjects{}
	var buf bytes.Buffer
	err := runProjectGitHubUnbind(context.Background(), projects, "proj-1", &buf)
	require.NoError(t, err)
	assert.True(t, projects.unbindHit)
	assert.Contains(t, buf.String(), "Removed GitHub App installation from project proj-1.")
}

func TestProjectGitHub_AgentModeAndWindowsGates(t *testing.T) {
	// Agent mode: status allowed, bind and unbind NOT allowed.
	assert.True(t, agentAllowed["project.github"], "project.github must be allowed in agent mode")
	assert.True(t, agentAllowed["project.github.status"], "project.github.status must be allowed in agent mode")
	assert.False(t, agentAllowed["project.github.bind"], "project.github.bind must NOT be allowed in agent mode")
	assert.False(t, agentAllowed["project.github.unbind"], "project.github.unbind must NOT be allowed in agent mode")

	// Windows gate: all three subcommands + parent allowed.
	root, ran := gateTestTree(t)
	applyPlatformRestrictions(root, "windows")
	for _, args := range [][]string{
		{"project", "github", "status"},
		{"project", "github", "bind"},
		{"project", "github", "unbind"},
	} {
		*ran = nil
		root.SetArgs(args)
		err := root.Execute()
		require.NoError(t, err, "expected %v to be allowed on Windows", args)
		assert.NotEmpty(t, *ran, "expected %v to execute on Windows", args)
	}
}
