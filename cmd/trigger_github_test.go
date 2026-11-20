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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerGitHubCLIModesAndWindowsGate(t *testing.T) {
	allPaths := []string{
		"trigger.github",
		"trigger.github.status",
		"trigger.github.settings",
		"trigger.github.primary",
		"trigger.github.primary.list",
		"trigger.github.primary.set",
		"trigger.github.primary.unset",
	}
	agentAllowedPaths := map[string]bool{
		"trigger.github":        true,
		"trigger.github.status": true,
	}

	for _, path := range allPaths {
		assert.True(t, windowsAllowed[path], "windowsAllowed[%q]", path)
		assert.Equal(t, agentAllowedPaths[path], agentAllowed[path], "agentAllowed[%q]", path)
		assert.NotNil(t, resolveCommandPath(rootCmd, path), "rootCmd must contain %q", path)
	}

	realTrigger := resolveCommandPath(rootCmd, "trigger")
	require.NotNil(t, realTrigger)

	// Human mode exposes every GitHub trigger subcommand.
	humanRoot := &cobra.Command{Use: "scion"}
	humanRoot.AddCommand(cloneCommandShape(realTrigger))
	applyModeRestrictions(humanRoot, ModeHuman)
	humanRemaining := collectCommandNames(humanRoot)
	for _, path := range allPaths {
		assert.Contains(t, humanRemaining, path, "human mode must include %s", path)
	}

	// Agent mode exposes only trigger.github and trigger.github.status; settings and primary.* are stripped.
	agentRoot := &cobra.Command{Use: "scion"}
	agentRoot.AddCommand(cloneCommandShape(realTrigger))
	applyModeRestrictions(agentRoot, ModeAgent)
	agentRemaining := collectCommandNames(agentRoot)
	for _, path := range allPaths {
		if agentAllowedPaths[path] {
			assert.Contains(t, agentRemaining, path, "agent mode must include %s", path)
		} else {
			assert.NotContains(t, agentRemaining, path, "agent mode must exclude %s", path)
		}
	}

	// Windows platform gate allows all GitHub trigger subcommands.
	winTree, ran := gateTestTree(t)
	applyPlatformRestrictions(winTree, "windows")
	for _, args := range [][]string{
		{"trigger", "github", "status"},
		{"trigger", "github", "settings"},
		{"trigger", "github", "primary", "list"},
		{"trigger", "github", "primary", "set", "acme/widgets", "--project", "proj-1"},
		{"trigger", "github", "primary", "unset", "acme/widgets"},
	} {
		*ran = nil
		winTree.SetArgs(args)
		winTree.SetOut(&bytes.Buffer{})
		winTree.SetErr(&bytes.Buffer{})
		require.NoError(t, winTree.Execute(), "windows gate must allow args=%v", args)
		require.Len(t, *ran, 1, "args=%v", args)
	}
}

func withStubTriggerGitHubContexts(
	t *testing.T,
	serverURL string,
	projectID string,
	credKind hubsync.CredentialKind,
) {
	t.Helper()
	client, err := hubclient.New(serverURL)
	require.NoError(t, err)

	origProjFn := triggerHubContextFn
	origAdminFn := triggerGitHubAdminHubContextFn
	origJSON := triggerOutputJSON
	origFmt := outputFormat
	t.Cleanup(func() {
		triggerHubContextFn = origProjFn
		triggerGitHubAdminHubContextFn = origAdminFn
		triggerOutputJSON = origJSON
		outputFormat = origFmt
	})

	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{
			Client:         client,
			Endpoint:       serverURL,
			CredentialKind: credKind,
		}, projectID, ctx, cancel, nil
	}
	triggerGitHubAdminHubContextFn = func() (*HubContext, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{
			Client:         client,
			Endpoint:       serverURL,
			CredentialKind: credKind,
		}, ctx, cancel, nil
	}
}

func TestTriggerGitHubStatus_NonAdminAdminAndSanitisation(t *testing.T) {
	const projectID = "proj-gh-1"
	instID := int64(4201)
	checkedAt := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	t.Run("non-admin sees project status and stale_primary without failing on 403", func(t *testing.T) {
		var appConfigCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/trigger-sources/github":
				_ = json.NewEncoder(w).Encode(hubclient.ProjectGitHubTriggerSettings{
					UnmatchedCommandReply: true,
					UnmatchedCommandText:  "Try /help\x1b[31mRED\x1b[0m\u202eevil",
					Readiness: hubclient.GitHubTriggersProjectReadiness{
						Ready:              false,
						MissingEvents:      []string{"issue_comment"},
						MissingPermissions: []string{"issues:write"},
						AppSlug:            "scion-bot\x1b[2J",
						CheckedAt:          &checkedAt,
						Error:              "stale\x7ferror",
					},
					Routing: hubclient.ProjectGitHubTriggerRoutingView{
						PrimaryProject: nil,
						IsPrimary:      false,
						OtherProjects:  []string{"bravo\u2028line"},
						Status:         "stale_primary",
					},
					Warnings: []string{"multi_project_repo"},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
				_ = json.NewEncoder(w).Encode(hubclient.Project{
					ID:                   projectID,
					Name:                 "Alpha\x1b[31mProj",
					GitRemote:            "https://github.com/acme/widgets.git",
					GitHubInstallationID: &instID,
					GitHubAppStatus: &hubclient.GitHubAppProjectStatus{
						State:        "error",
						ErrorCode:    "repo_not_accessible",
						ErrorMessage: "Removed\x1b[0m repo",
						LastChecked:  checkedAt,
					},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/github-app":
				appConfigCalls++
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "forbidden", "message": "admin only"},
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, projectID, hubsync.CredentialKindOAuth)
		triggerOutputJSON = false
		outputFormat = ""

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubStatus(triggerGitHubStatusCmd, nil))
		})
		assert.Equal(t, 1, appConfigCalls)
		assert.Contains(t, out, "App Readiness:")
		assert.Contains(t, out, "Status:              not ready")
		assert.Contains(t, out, `Missing Events:      ["issue_comment"]`)
		assert.Contains(t, out, `Missing Permissions: ["issues:write"]`)
		assert.Contains(t, out, "Installation ID:     4201")
		assert.Contains(t, out, "repo_not_accessible")
		assert.Contains(t, out, "stale_primary")
		assert.Contains(t, out, `Warnings:            ["multi_project_repo"]`)
		assert.Contains(t, out, "Unmatched Command Reply: on")
		assert.NotContains(t, out, "Webhooks Enabled:", "non-admin output must not include admin webhook config")
		// Terminal control characters must be escaped.
		assert.NotContains(t, out, "\x1b")
		assert.NotContains(t, out, "\x7f")
		assert.NotContains(t, out, "\u202e")
		assert.NotContains(t, out, "\u2028")

		// JSON output must also escape control/bidi characters while staying valid JSON.
		triggerOutputJSON = true
		jsonOut := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubStatus(triggerGitHubStatusCmd, nil))
		})
		assert.NotContains(t, jsonOut, "\x1b")
		assert.NotContains(t, jsonOut, "\x7f")
		assert.NotContains(t, jsonOut, "\u202e")
		assert.NotContains(t, jsonOut, "\u2028")
		var decoded triggerGitHubStatusOutput
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &decoded))
		assert.Equal(t, "stale_primary", decoded.Routing.Status)
		assert.Nil(t, decoded.App)
	})

	t.Run("admin caller includes Hub GitHub App readiness details", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/trigger-sources/github":
				_ = json.NewEncoder(w).Encode(hubclient.ProjectGitHubTriggerSettings{
					Readiness: hubclient.GitHubTriggersProjectReadiness{
						Ready:   true,
						AppSlug: "scion-app",
					},
					Routing: hubclient.ProjectGitHubTriggerRoutingView{
						PrimaryProject: &hubclient.ProjectGitHubTriggerPrimaryRef{ID: projectID, Name: "alpha"},
						IsPrimary:      true,
						OtherProjects:  []string{"beta"},
						Status:         "ok",
					},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
				_ = json.NewEncoder(w).Encode(hubclient.Project{
					ID:                   projectID,
					Name:                 "alpha",
					GitRemote:            "https://github.com/acme/widgets.git",
					GitHubInstallationID: &instID,
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/github-app":
				_ = json.NewEncoder(w).Encode(hubclient.GitHubAppConfig{
					AppID:            9001,
					Configured:       true,
					WebhooksEnabled:  true,
					HasWebhookSecret: true,
					Triggers: &hubclient.GitHubTriggersReadiness{
						Ready:            true,
						WebhooksEnabled:  true,
						HasWebhookSecret: true,
						AppSlug:          "scion-app",
					},
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, projectID, hubsync.CredentialKindOAuth)
		triggerOutputJSON = false
		outputFormat = ""

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubStatus(triggerGitHubStatusCmd, nil))
		})
		assert.Contains(t, out, "Status:              ready")
		assert.Contains(t, out, "App ID:              9001")
		assert.Contains(t, out, "Webhooks Enabled:    true")
		assert.Contains(t, out, "Webhook Secret:      configured")
		assert.Contains(t, out, "Primary Project:     alpha (proj-gh-1) (this project)")
		assert.Contains(t, out, "Other Projects:      beta")
	})

	t.Run("agent credential skips admin GET /api/v1/github-app", func(t *testing.T) {
		var appConfigCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/trigger-sources/github":
				_ = json.NewEncoder(w).Encode(hubclient.ProjectGitHubTriggerSettings{
					Readiness: hubclient.GitHubTriggersProjectReadiness{Ready: true},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID:
				_ = json.NewEncoder(w).Encode(hubclient.Project{ID: projectID, Name: "alpha"})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/github-app":
				appConfigCalls++
				w.WriteHeader(http.StatusForbidden)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, projectID, hubsync.CredentialKindAgentToken)
		triggerOutputJSON = false
		outputFormat = ""

		_ = captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubStatus(triggerGitHubStatusCmd, nil))
		})
		assert.Zero(t, appConfigCalls)
	})
}

func TestTriggerGitHubSettings(t *testing.T) {
	const projectID = "proj-gh-settings"

	resetSettingsCmd := func(t *testing.T) *cobra.Command {
		t.Helper()
		origReply, origText := triggerGitHubUnmatchedReply, triggerGitHubUnmatchedText
		t.Cleanup(func() {
			triggerGitHubUnmatchedReply, triggerGitHubUnmatchedText = origReply, origText
		})
		triggerGitHubUnmatchedReply = ""
		triggerGitHubUnmatchedText = ""
		c := &cobra.Command{Use: "settings", RunE: runTriggerGitHubSettings}
		c.Flags().StringVar(&triggerGitHubUnmatchedReply, "unmatched-command-reply", "", "")
		c.Flags().StringVar(&triggerGitHubUnmatchedText, "unmatched-command-text", "", "")
		return c
	}

	t.Run("no flags performs GET only", func(t *testing.T) {
		var getCalls, putCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(hubclient.ProjectGitHubTriggerSettings{
					UnmatchedCommandReply: true,
					UnmatchedCommandText:  "See docs\x1b[31m!",
				})
			case http.MethodPut:
				putCalls++
			}
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, projectID, hubsync.CredentialKindOAuth)
		triggerOutputJSON = false
		c := resetSettingsCmd(t)

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubSettings(c, nil))
		})
		assert.Equal(t, 1, getCalls)
		assert.Equal(t, 0, putCalls)
		assert.Contains(t, out, "Unmatched Command Reply: on")
		assert.NotContains(t, out, "\x1b")
	})

	t.Run("partial update preserves unspecified field", func(t *testing.T) {
		var gotPut hubclient.UpdateProjectGitHubTriggerSettingsRequest
		var getCalls, putCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				getCalls++
				_ = json.NewEncoder(w).Encode(hubclient.ProjectGitHubTriggerSettings{
					UnmatchedCommandReply: false,
					UnmatchedCommandText:  "Existing suffix",
				})
			case http.MethodPut:
				putCalls++
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotPut))
				_ = json.NewEncoder(w).Encode(hubclient.ProjectGitHubTriggerSettings{
					UnmatchedCommandReply: gotPut.UnmatchedCommandReply,
					UnmatchedCommandText:  gotPut.UnmatchedCommandText,
				})
			}
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, projectID, hubsync.CredentialKindOAuth)
		triggerOutputJSON = false
		c := resetSettingsCmd(t)
		require.NoError(t, c.Flags().Set("unmatched-command-reply", "on"))

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubSettings(c, nil))
		})
		assert.Equal(t, 1, getCalls)
		assert.Equal(t, 1, putCalls)
		assert.True(t, gotPut.UnmatchedCommandReply)
		assert.Equal(t, "Existing suffix", gotPut.UnmatchedCommandText)
		assert.Contains(t, out, "Updated GitHub trigger source settings:")
		assert.Contains(t, out, "Unmatched Command Reply: on")
		assert.Contains(t, out, "Existing suffix")
	})

	t.Run("both flags skip GET and allow clearing text", func(t *testing.T) {
		var gotPut hubclient.UpdateProjectGitHubTriggerSettingsRequest
		var getCalls, putCalls int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.Method {
			case http.MethodGet:
				getCalls++
			case http.MethodPut:
				putCalls++
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotPut))
				_ = json.NewEncoder(w).Encode(hubclient.ProjectGitHubTriggerSettings{
					UnmatchedCommandReply: gotPut.UnmatchedCommandReply,
					UnmatchedCommandText:  gotPut.UnmatchedCommandText,
				})
			}
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, projectID, hubsync.CredentialKindOAuth)
		triggerOutputJSON = true
		c := resetSettingsCmd(t)
		require.NoError(t, c.Flags().Set("unmatched-command-reply", "off"))
		require.NoError(t, c.Flags().Set("unmatched-command-text", ""))

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubSettings(c, nil))
		})
		assert.Equal(t, 0, getCalls)
		assert.Equal(t, 1, putCalls)
		assert.False(t, gotPut.UnmatchedCommandReply)
		assert.Equal(t, "", gotPut.UnmatchedCommandText)
		var decoded triggerGitHubSourceSettingsOutput
		require.NoError(t, json.Unmarshal([]byte(out), &decoded))
		assert.False(t, decoded.UnmatchedCommandReply)
		assert.Equal(t, "", decoded.UnmatchedCommandText)
	})

	t.Run("invalid reply value returns usage error", func(t *testing.T) {
		c := resetSettingsCmd(t)
		require.NoError(t, c.Flags().Set("unmatched-command-reply", "maybe"))
		err := runTriggerGitHubSettings(c, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be on or off")
	})
}

func TestTriggerGitHubPrimary(t *testing.T) {
	p1 := "11111111-1111-4111-8111-111111111111"
	p2 := "22222222-2222-4222-8222-222222222222"

	t.Run("primary list with installation filter and sanitisation", func(t *testing.T) {
		var gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodGet && r.URL.Path == "/api/v1/github-app/repo-routing" {
				gotQuery = r.URL.RawQuery
				_ = json.NewEncoder(w).Encode([]hubclient.GitHubRepoRoutingEntry{
					{
						InstallationID:   500,
						Repo:             "acme/widgets\x1b[31m",
						PrimaryProjectID: &p1,
						Status:           "ok",
						Projects: []hubclient.GitHubRepoRoutingProject{
							{ID: p1, Name: "alpha\u202e", HasGitHubTriggers: true},
							{ID: p2, Name: "beta", HasGitHubTriggers: false},
						},
					},
					{
						InstallationID:   500,
						Repo:             "acme/stale",
						PrimaryProjectID: &p2,
						Status:           "stale_primary",
						Projects:         []hubclient.GitHubRepoRoutingProject{},
					},
				})
				return
			}
			http.NotFound(w, r)
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, "", hubsync.CredentialKindOAuth)
		triggerOutputJSON = false

		origInst := triggerGitHubPrimaryListInstallation
		t.Cleanup(func() { triggerGitHubPrimaryListInstallation = origInst })
		c := &cobra.Command{Use: "list", RunE: runTriggerGitHubPrimaryList}
		c.Flags().Int64Var(&triggerGitHubPrimaryListInstallation, "installation", 0, "")
		require.NoError(t, c.Flags().Set("installation", "500"))

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubPrimaryList(c, nil))
		})
		assert.Equal(t, "installation=500", gotQuery)
		assert.Contains(t, out, "INSTALLATION")
		assert.Contains(t, out, "stale_primary")
		assert.Contains(t, out, "beta")
		assert.NotContains(t, out, "\x1b")
		assert.NotContains(t, out, "\u202e")
	})

	t.Run("primary set resolves slug and auto-detects installation", func(t *testing.T) {
		var gotPutPath string
		var gotPutBody hubclient.SetGitHubRepoPrimaryRequest
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/alpha-slug":
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"error": map[string]any{"code": "not_found", "message": "not found"},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects" && r.URL.Query().Get("slug") == "alpha-slug":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"projects": []hubclient.Project{
						{ID: p1, Name: "Alpha", Slug: "alpha-slug"},
					},
				})
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/github-app/repo-routing":
				_ = json.NewEncoder(w).Encode([]hubclient.GitHubRepoRoutingEntry{
					{
						InstallationID: 777,
						Repo:           "acme/widgets",
						Status:         "none",
						Projects: []hubclient.GitHubRepoRoutingProject{
							{ID: p1, Name: "Alpha"},
							{ID: p2, Name: "Beta"},
						},
					},
				})
			case r.Method == http.MethodPut && r.URL.Path == "/api/v1/github-app/repo-routing/Acme/Widgets":
				gotPutPath = r.URL.Path
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gotPutBody))
				_ = json.NewEncoder(w).Encode(hubclient.GitHubRepoRoutingEntry{
					InstallationID:   gotPutBody.InstallationID,
					Repo:             "acme/widgets",
					PrimaryProjectID: &gotPutBody.PrimaryProjectID,
					Status:           "ok",
					Projects: []hubclient.GitHubRepoRoutingProject{
						{ID: p1, Name: "Alpha", HasGitHubTriggers: true},
						{ID: p2, Name: "Beta"},
					},
				})
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, "", hubsync.CredentialKindOAuth)
		triggerOutputJSON = false

		origProj, origInst := triggerGitHubPrimarySetProject, triggerGitHubPrimarySetInstallation
		t.Cleanup(func() {
			triggerGitHubPrimarySetProject = origProj
			triggerGitHubPrimarySetInstallation = origInst
		})
		triggerGitHubPrimarySetProject = ""
		triggerGitHubPrimarySetInstallation = 0
		c := &cobra.Command{Use: "set", RunE: runTriggerGitHubPrimarySet}
		c.Flags().StringVar(&triggerGitHubPrimarySetProject, "project", "", "")
		c.Flags().Int64Var(&triggerGitHubPrimarySetInstallation, "installation", 0, "")
		require.NoError(t, c.Flags().Set("project", "alpha-slug"))

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubPrimarySet(c, []string{"Acme/Widgets"}))
		})
		assert.Equal(t, "/api/v1/github-app/repo-routing/Acme/Widgets", gotPutPath)
		assert.Equal(t, int64(777), gotPutBody.InstallationID)
		assert.Equal(t, p1, gotPutBody.PrimaryProjectID)
		assert.Contains(t, out, "Set primary project for acme/widgets (installation 777):")
		assert.Contains(t, out, "Alpha ("+p1+")")
		assert.Contains(t, out, "Status:   ok")
	})

	t.Run("primary set rejects invalid owner/repo", func(t *testing.T) {
		c := &cobra.Command{Use: "set", RunE: runTriggerGitHubPrimarySet}
		c.Flags().StringVar(&triggerGitHubPrimarySetProject, "project", "", "")
		c.Flags().Int64Var(&triggerGitHubPrimarySetInstallation, "installation", 0, "")
		err := runTriggerGitHubPrimarySet(c, []string{"not-a-repo"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected <owner>/<repo>")
	})

	t.Run("primary unset sends DELETE with optional installation", func(t *testing.T) {
		var gotMethod, gotPath, gotQuery string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			gotPath = r.URL.Path
			gotQuery = r.URL.RawQuery
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		withStubTriggerGitHubContexts(t, srv.URL, "", hubsync.CredentialKindOAuth)
		triggerOutputJSON = true

		origInst := triggerGitHubPrimaryUnsetInstallation
		t.Cleanup(func() { triggerGitHubPrimaryUnsetInstallation = origInst })
		c := &cobra.Command{Use: "unset", RunE: runTriggerGitHubPrimaryUnset}
		c.Flags().Int64Var(&triggerGitHubPrimaryUnsetInstallation, "installation", 0, "")
		require.NoError(t, c.Flags().Set("installation", "888"))

		out := captureStdout(t, func() {
			require.NoError(t, runTriggerGitHubPrimaryUnset(c, []string{"Acme/Widgets"}))
		})
		assert.Equal(t, http.MethodDelete, gotMethod)
		assert.Equal(t, "/api/v1/github-app/repo-routing/Acme/Widgets", gotPath)
		assert.Equal(t, "installation=888", gotQuery)
		assert.True(t, strings.Contains(out, `"repo": "acme/widgets"`))
		assert.True(t, strings.Contains(out, `"installation_id": 888`))
	})
}
