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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectCreateMockHub is a minimal Hub that records project create bodies
// and validates workspaceMode like the real server (400 for unknown modes
// and for worktree-per-agent without a git remote).
type projectCreateMockHub struct {
	mu      sync.Mutex
	creates []map[string]interface{}
}

func (m *projectCreateMockHub) lastCreate(t *testing.T) map[string]interface{} {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.NotEmpty(t, m.creates, "no project create request reached the hub")
	return m.creates[len(m.creates)-1]
}

func (m *projectCreateMockHub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"projects": []interface{}{}, "totalCount": 0})
		case r.URL.Path == "/api/v1/projects" && r.Method == http.MethodPost:
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
			}
			m.mu.Lock()
			m.creates = append(m.creates, body)
			m.mu.Unlock()

			mode, _ := body["workspaceMode"].(string)
			remote, _ := body["gitRemote"].(string)
			msg := ""
			switch mode {
			case "", "shared", "per-agent":
			case "worktree-per-agent":
				if remote == "" {
					msg = `workspace mode "worktree-per-agent" requires a git remote`
				}
			default:
				msg = `invalid workspace mode "` + mode + `": must be one of "shared", "per-agent", "worktree-per-agent"`
			}
			if msg != "" {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{"code": "validation_error", "message": msg},
				})
				return
			}
			labels := map[string]string{}
			if mode != "" {
				labels["scion.dev/workspace-mode"] = mode
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "p-1", "name": body["name"], "slug": body["slug"], "gitRemote": remote, "labels": labels,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// setupProjectCreateTest wires a mock hub and resets the create flags.
func setupProjectCreateTest(t *testing.T) *projectCreateMockHub {
	t.Helper()
	origHome := os.Getenv("HOME")
	origProjectPath := projectPath
	origSlug, origName, origBranch, origMode := hubProjectCreateSlug, hubProjectCreateName, hubProjectCreateBranch, hubProjectCreateMode
	origJSON, origFormat, origYes, origNonInteractive := hubOutputJSON, outputFormat, autoConfirm, nonInteractive
	t.Cleanup(func() {
		_ = os.Setenv("HOME", origHome)
		projectPath = origProjectPath
		hubProjectCreateSlug, hubProjectCreateName, hubProjectCreateBranch, hubProjectCreateMode = origSlug, origName, origBranch, origMode
		hubOutputJSON, outputFormat, autoConfirm, nonInteractive = origJSON, origFormat, origYes, origNonInteractive
	})

	mock := &projectCreateMockHub{}
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)

	tmpHome := t.TempDir()
	_ = os.Setenv("HOME", tmpHome)
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	projectPath = setupEnvProject(t, tmpHome, server.URL)

	hubProjectCreateSlug, hubProjectCreateName, hubProjectCreateBranch, hubProjectCreateMode = "", "", "", ""
	hubOutputJSON, autoConfirm, nonInteractive = false, true, true
	return mock
}

func TestHubProjectCreateCmd_Args(t *testing.T) {
	assert.Equal(t, "create [git-url]", hubProjectCreateCmd.Use)
	assert.NoError(t, hubProjectCreateCmd.Args(hubProjectCreateCmd, nil), "no URL must be accepted")
	assert.NoError(t, hubProjectCreateCmd.Args(hubProjectCreateCmd, []string{"https://github.com/acme/widgets.git"}))
	assert.Error(t, hubProjectCreateCmd.Args(hubProjectCreateCmd, []string{"a", "b"}))

	f := hubProjectCreateCmd.Flags().Lookup("workspace-mode")
	require.NotNil(t, f, "--workspace-mode flag must exist")
	assert.Equal(t, "", f.DefValue)
}

func TestRunHubProjectCreate_NoURL_HubManaged(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateName = "Scratch Pad"
	hubProjectCreateMode = "per-agent"

	require.NoError(t, runHubProjectCreate(hubProjectCreateCmd, nil))

	body := mock.lastCreate(t)
	assert.Equal(t, "Scratch Pad", body["name"])
	assert.Equal(t, "scratch-pad", body["slug"])
	assert.Equal(t, "per-agent", body["workspaceMode"])
	assert.NotContains(t, body, "gitRemote", "hub-managed create must not send a git remote")
	assert.NotContains(t, body, "labels", "hub-managed create must not send git labels")
}

func TestRunHubProjectCreate_NoURL_DefaultModeOmitted(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateName = "notes"

	require.NoError(t, runHubProjectCreate(hubProjectCreateCmd, nil))
	assert.NotContains(t, mock.lastCreate(t), "workspaceMode", "no flag must leave the mode to the server default")
}

func TestRunHubProjectCreate_NoURL_RequiresName(t *testing.T) {
	mock := setupProjectCreateTest(t)
	err := runHubProjectCreate(hubProjectCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--name is required")
	assert.Empty(t, mock.creates)
}

func TestRunHubProjectCreate_NoURL_RejectsBranch(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateName = "notes"
	hubProjectCreateBranch = "main"
	err := runHubProjectCreate(hubProjectCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--branch requires a git URL")
	assert.Empty(t, mock.creates)
}

func TestRunHubProjectCreate_URLWithWorkspaceMode(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateBranch = "main" // skip git ls-remote
	hubProjectCreateMode = "worktree-per-agent"

	require.NoError(t, runHubProjectCreate(hubProjectCreateCmd, []string{"https://github.com/acme/widgets.git"}))

	body := mock.lastCreate(t)
	assert.Equal(t, "worktree-per-agent", body["workspaceMode"])
	assert.Equal(t, "github.com/acme/widgets", body["gitRemote"])
	labels, _ := body["labels"].(map[string]interface{})
	assert.Equal(t, "main", labels["scion.dev/default-branch"])
	assert.NotContains(t, labels, "scion.dev/workspace-mode", "the mode travels in workspaceMode, not as a raw label")
}

func TestRunHubProjectCreate_InvalidModePassedThroughToServer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		mode    string
		wantErr string
	}{
		{name: "unknown mode", args: nil, mode: "bogus", wantErr: `invalid workspace mode "bogus"`},
		{name: "worktree without git", args: nil, mode: "worktree-per-agent", wantErr: "requires a git remote"},
		{name: "unknown mode with URL", args: []string{"https://github.com/acme/widgets.git"}, mode: "clone", wantErr: `invalid workspace mode "clone"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupProjectCreateTest(t)
			hubProjectCreateName = "notes"
			hubProjectCreateBranch = ""
			if len(tc.args) > 0 {
				hubProjectCreateBranch = "main"
			}
			hubProjectCreateMode = tc.mode

			err := runHubProjectCreate(hubProjectCreateCmd, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "failed to create project")
			assert.Contains(t, err.Error(), tc.wantErr, "the hub's 400 message must reach the user")
			assert.Equal(t, tc.mode, mock.lastCreate(t)["workspaceMode"], "the CLI must not filter the mode itself")
		})
	}
}

func TestRunHubProjectCreate_URLWithoutFlagSendsNoWorkspaceMode(t *testing.T) {
	mock := setupProjectCreateTest(t)
	hubProjectCreateBranch = "main" // skip git ls-remote

	require.NoError(t, runHubProjectCreate(hubProjectCreateCmd, []string{"https://github.com/acme/widgets.git"}))

	body := mock.lastCreate(t)
	assert.NotContains(t, body, "workspaceMode", "existing 'create <git-url>' must send the same body as before")
	assert.Equal(t, "github.com/acme/widgets", body["gitRemote"])
	assert.Equal(t, map[string]interface{}{
		"scion.dev/default-branch": "main",
		"scion.dev/clone-url":      "https://github.com/acme/widgets.git",
		"scion.dev/source-url":     "https://github.com/acme/widgets.git",
	}, body["labels"])
}

func TestWorkspaceBootstrapNotice(t *testing.T) {
	ignored := api.WarningEmptyPerAgentWorkspaceFilesIgnored
	tz := "TZ in config.env is ignored"
	for _, tc := range []struct {
		name          string
		sent, urls    int
		warnings      []string
		wantLines     []string
		wantRemaining []string
	}{
		{name: "no files sent", sent: 0, urls: 0, warnings: []string{ignored}, wantRemaining: []string{ignored}},
		{name: "uploading", sent: 3, urls: 3, warnings: []string{tz}, wantRemaining: []string{tz}},
		{name: "local broker workspace", sent: 3, urls: 0, wantLines: []string{"Using local workspace on broker."}},
		{name: "local broker workspace with unrelated warning", sent: 3, urls: 0, warnings: []string{tz},
			wantLines: []string{"Using local workspace on broker."}, wantRemaining: []string{tz}},
		{name: "files ignored by hub", sent: 3, urls: 0, warnings: []string{ignored},
			wantLines: []string{"Warning: " + ignored}},
		{name: "files ignored plus unrelated warning", sent: 3, urls: 0, warnings: []string{tz, ignored},
			wantLines: []string{"Warning: " + ignored}, wantRemaining: []string{tz}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, remaining := workspaceBootstrapNotice(tc.sent, tc.urls, tc.warnings)
			assert.Equal(t, tc.wantLines, lines)
			assert.Equal(t, tc.wantRemaining, remaining)
		})
	}
}

func TestWorkspaceFinalizeNotice(t *testing.T) {
	ignored := api.WarningEmptyPerAgentWorkspaceFilesIgnored
	tz := "TZ in config.env is ignored"
	assert.Equal(t, []string{"Workspace uploaded: 3 files"}, workspaceFinalizeNotice(3, nil))
	assert.Equal(t, []string{"Workspace uploaded: 3 files", "Warning: " + tz}, workspaceFinalizeNotice(3, []string{tz}))
	assert.Equal(t, []string{"Warning: " + ignored}, workspaceFinalizeNotice(0, []string{ignored}))
	assert.Equal(t, []string{"Warning: " + ignored, "Warning: " + tz}, workspaceFinalizeNotice(0, []string{tz, ignored}))
}

// TestStartAgentViaHub_WorkspaceFilesWarningWiring drives startAgentViaHub
// from a local non-git project against a mock hub and checks what reaches
// stderr, both when the create response has no upload URLs and when the
// files are uploaded and the sync-to finalize response carries warnings.
func TestStartAgentViaHub_WorkspaceFilesWarningWiring(t *testing.T) {
	ignored := api.WarningEmptyPerAgentWorkspaceFilesIgnored
	tz := "TZ in config.env is ignored"
	for _, tc := range []struct {
		name             string // the CLI sends notes.txt; the root .scion is excluded
		createWarnings   []string
		upload           bool
		finalizeWarnings []string
		want             []string
		notWant          []string
	}{
		{name: "create: files ignored", createWarnings: []string{ignored, tz},
			want: []string{"Warning: " + ignored, "Warning: " + tz}, notWant: []string{"Using local workspace on broker."}},
		{name: "create: local workspace", createWarnings: []string{tz},
			want: []string{"Using local workspace on broker.", "Warning: " + tz}},
		{name: "finalize: files ignored", upload: true, finalizeWarnings: []string{ignored, tz},
			want: []string{"Warning: " + ignored, "Warning: " + tz}, notWant: []string{"Workspace uploaded:"}},
		{name: "finalize: uploaded", upload: true,
			want: []string{"Workspace uploaded: 1 files"}, notWant: []string{"Warning:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"SCION_HUB_ENDPOINT", "SCION_HUB_URL", "SCION_PROJECT_ID"} {
				t.Setenv(k, "")
				_ = os.Unsetenv(k)
			}
			origOutputFormat, origTemplateName := outputFormat, templateName
			origHarnessConfigFlag, origRuntimeBrokerID := harnessConfigFlag, runtimeBrokerID
			t.Cleanup(func() {
				outputFormat, templateName = origOutputFormat, origTemplateName
				harnessConfigFlag, runtimeBrokerID = origHarnessConfigFlag, origRuntimeBrokerID
			})
			outputFormat = ""
			templateName = ""
			harnessConfigFlag = "codex"
			runtimeBrokerID = "broker-1"

			projectDir := t.TempDir() // not a git repo
			scionDir := filepath.Join(projectDir, ".scion")
			require.NoError(t, os.MkdirAll(scionDir, 0755))
			require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("hub:\n  enabled: true\n"), 0644))
			require.NoError(t, os.WriteFile(filepath.Join(projectDir, "notes.txt"), []byte("local file"), 0644))

			projectID := "project-epa"
			agentPath := "/api/v1/projects/" + projectID + "/agents/agent-1"
			var (
				mu        sync.Mutex
				created   bool
				finalized bool
				sentFiles []transfer.FileInfo
			)
			var serverURL string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				running := &hubclient.Agent{
					ID: "agent-1", Slug: "agent-1", Name: "agent-1",
					Status: "running", Phase: "running", RuntimeBrokerID: "broker-1",
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/agents":
					var req hubclient.CreateAgentRequest
					if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
						t.Errorf("decode create agent: %v", err)
					}
					created = true
					sentFiles = req.WorkspaceFiles
					resp := &hubclient.CreateAgentResponse{Agent: running, Warnings: tc.createWarnings}
					if tc.upload {
						resp.Agent = &hubclient.Agent{ID: "agent-1", Slug: "agent-1", Name: "agent-1", Phase: "provisioning"}
						for _, f := range req.WorkspaceFiles {
							resp.UploadURLs = append(resp.UploadURLs, transfer.UploadURLInfo{
								Path: f.Path, URL: serverURL + "/upload/" + f.Path, Method: http.MethodPut,
							})
						}
					}
					_ = json.NewEncoder(w).Encode(resp)
				case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/upload/"):
					w.WriteHeader(http.StatusOK)
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/agent-1/workspace/sync-to/finalize":
					finalized = true
					_ = json.NewEncoder(w).Encode(&hubclient.SyncToFinalizeResponse{
						Applied: true, FilesApplied: len(sentFiles), Warnings: tc.finalizeWarnings,
					})
				case r.Method == http.MethodGet && r.URL.Path == agentPath:
					if !created {
						http.NotFound(w, r)
						return
					}
					_ = json.NewEncoder(w).Encode(running)
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": projectID, "name": "epa"})
				default:
					http.NotFound(w, r)
				}
			}))
			serverURL = server.URL
			t.Cleanup(server.Close)

			client, err := hubclient.New(server.URL)
			require.NoError(t, err)
			hubCtx := &HubContext{
				Client:      client,
				Endpoint:    server.URL,
				ProjectID:   projectID,
				ProjectPath: scionDir,
				BrokerID:    "broker-1",
			}

			var runErr error
			stderr := captureStderr(t, func() {
				runErr = startAgentViaHub(nil, hubCtx, "agent-1", "do it", false, nil)
			})
			require.NoError(t, runErr, stderr)
			require.NotEmpty(t, sentFiles, "the CLI must have sent the local non-git files")
			var sentPaths []string
			for _, f := range sentFiles {
				sentPaths = append(sentPaths, f.Path)
			}
			assert.Equal(t, []string{"notes.txt"}, sentPaths, "the workspace-root .scion must not be sent")
			assert.Equal(t, tc.upload, finalized, "finalize must run exactly on the upload path")
			for _, w := range tc.want {
				assert.Equal(t, 1, strings.Count(stderr, w), "want %q exactly once in:\n%s", w, stderr)
			}
			for _, w := range tc.notWant {
				assert.NotContains(t, stderr, w)
			}
		})
	}
}

func TestSyncToResultLines(t *testing.T) {
	ignored := api.WarningEmptyPerAgentWorkspaceFilesIgnored
	tz := "TZ in config.env is ignored"
	resp := func(applied bool, n int, w ...string) *hubclient.SyncToFinalizeResponse {
		return &hubclient.SyncToFinalizeResponse{Applied: applied, FilesApplied: n, Warnings: w}
	}
	for _, tc := range []struct {
		name            string
		uploaded        int
		skipped         int
		nothingToUpload bool
		resp            *hubclient.SyncToFinalizeResponse
		want            []string
	}{
		{name: "nothing to upload", nothingToUpload: true, resp: resp(true, 2),
			want: []string{"Workspace sync applied to agent."}},
		{name: "nothing to upload, files ignored", nothingToUpload: true, resp: resp(true, 0, tz, ignored),
			want: []string{"Warning: " + ignored, "Warning: " + tz}},
		{name: "uploaded", uploaded: 1, skipped: 1, resp: resp(true, 2, tz),
			want: []string{"Warning: " + tz, "Sync complete: 1 files uploaded, 10 B transferred", "Skipped 1 unchanged files", "Applied 2 files to agent workspace"}},
		{name: "uploaded, files ignored", uploaded: 1, skipped: 1, resp: resp(true, 0, ignored),
			want: []string{"Warning: " + ignored}},
		{name: "nil finalize response", uploaded: 1, resp: nil,
			want: []string{"Sync complete: 1 files uploaded, 10 B transferred"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, syncToResultLines(tc.uploaded, 10, tc.skipped, tc.nothingToUpload, tc.resp))
		})
	}
}

func TestSplitFilesIgnoredWarning(t *testing.T) {
	ignored := api.WarningEmptyPerAgentWorkspaceFilesIgnored

	// Without the warning the input slice is returned as is, not copied.
	in := []string{"a", "b"}
	got, rest := splitFilesIgnoredWarning(in)
	assert.False(t, got)
	assert.Equal(t, in, rest)
	assert.Same(t, &in[0], &rest[0])

	got, rest = splitFilesIgnoredWarning(nil)
	assert.False(t, got)
	assert.Nil(t, rest)

	in = []string{"a", ignored, "b"}
	got, rest = splitFilesIgnoredWarning(in)
	assert.True(t, got)
	assert.Equal(t, []string{"a", "b"}, rest)
	assert.Equal(t, []string{"a", ignored, "b"}, in, "input is not modified")

	got, rest = splitFilesIgnoredWarning([]string{ignored})
	assert.True(t, got)
	assert.Nil(t, rest)
}

func TestHubProjectCloneURLLabel(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://github.com/org/repo", "https://github.com/org/repo.git"},
		{"https://github.com/org/repo.git?ref=main", "https://github.com/org/repo.git"},
		{"github.com/org/repo#readme", "https://github.com/org/repo.git"},
		{"git@github.com:org/repo.git?x=1", "https://github.com/org/repo.git"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, hubProjectCloneURLLabel(tt.in), tt.in)
	}
}

func TestHubProjectGitSourceLabels_NoCredentials(t *testing.T) {
	const pw = "FAKE-KEY-SENTINEL-not-a-real-credential"
	tests := []struct {
		name, in, wantClone, wantSource string
	}{
		{"https userinfo", "https://user:" + pw + "@github.com/org/repo", "https://github.com/org/repo.git", "https://github.com/org/repo"},
		{"https token-only", "https://" + pw + "@github.com/org/repo.git", "https://github.com/org/repo.git", "https://github.com/org/repo.git"},
		{"scp userinfo in path", "git@user:" + pw + "@host:org/repo", "", ""},
		{"query token", "https://github.com/org/repo?access_token=" + pw, "https://github.com/org/repo.git", "https://github.com/org/repo"},
		{"clean scp", "git@github.com:org/repo.git", "https://github.com/org/repo.git", "git@github.com:org/repo.git"},
		{"scp custom login", "deploy@host:org/repo", "https://host/org/repo.git", "deploy@host:org/repo"},
		{"ssh port", "ssh://git@host:22/org/repo", "https://host/org/repo.git", "ssh://git@host:22/org/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := hubProjectGitSourceLabels(tt.in, "main")
			assert.Equal(t, "main", labels[store.LabelDefaultBranch])
			gotClone, cloneOK := labels[store.LabelCloneURL]
			gotSource, sourceOK := labels[store.LabelSourceURL]
			assert.Equal(t, tt.wantClone, gotClone)
			assert.Equal(t, tt.wantSource, gotSource)
			assert.Equal(t, tt.wantClone != "", cloneOK, "clone-url omitted when it cannot be sanitized")
			assert.Equal(t, tt.wantSource != "", sourceOK, "source-url omitted when it cannot be sanitized")
			for k, v := range labels {
				assert.NotContains(t, v, pw, "credential survived in %s", k)
			}
		})
	}
}

// TestRunHubProjectCreate_CredentialedURLNotSent drives `hub project create`
// with credential-bearing URLs and checks that neither the git remote nor the
// clone-url/source-url labels sent to the hub keep the credential.
func TestRunHubProjectCreate_CredentialedURLNotSent(t *testing.T) {
	const pw = "FAKE-KEY-SENTINEL-not-a-real-credential"
	for _, tc := range []struct {
		name, url   string
		checkRemote bool // false: the CLI refuses the URL before sending anything
	}{
		{"https userinfo", "https://user:" + pw + "@github.com/acme/widgets.git", true},
		{"query token", "https://github.com/acme/widgets.git?access_token=" + pw, true},
		{"fragment token", "https://github.com/acme/widgets.git#" + pw, true},
		{"scp userinfo in path", "git@user:" + pw + "@host:acme/widgets", false},
		{"query char inside password", "https://user:" + pw + "?W@github.com/acme/widgets.git", false},
		{"fragment char inside password", "https://user:" + pw + "#W@github.com/acme/widgets.git", false},
		{"invalid URL not echoed", "https://user:" + pw + "@host", false},
		{"query char inside password with path-like prefix", "https://user:" + pw + "/x?W@github.com/acme/widgets.git", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := setupProjectCreateTest(t)
			hubProjectCreateName = "widgets"
			hubProjectCreateBranch = "main" // skip git ls-remote

			err := runHubProjectCreate(hubProjectCreateCmd, []string{tc.url})
			if !tc.checkRemote {
				require.Error(t, err)
				assert.NotContains(t, strings.ToLower(err.Error()), strings.ToLower(pw), "the error must not echo the credential")
				mock.mu.Lock()
				defer mock.mu.Unlock()
				assert.Empty(t, mock.creates, "nothing may be sent to the hub")
				return
			}
			require.NoError(t, err)

			body := mock.lastCreate(t)
			labels, _ := body["labels"].(map[string]interface{})
			for k, v := range labels {
				s, _ := v.(string)
				assert.NotContains(t, strings.ToLower(s), strings.ToLower(pw), "credential sent in label %s", k)
			}
			remote, _ := body["gitRemote"].(string)
			assert.NotContains(t, strings.ToLower(remote), strings.ToLower(pw), "credential sent in gitRemote")
		})
	}
}
