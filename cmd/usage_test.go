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
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubUsageHubClient struct {
	hubclient.Client
	usage    *stubUsageService
	projects hubclient.ProjectService
}

func (s *stubUsageHubClient) Usage() hubclient.UsageService { return s.usage }
func (s *stubUsageHubClient) Projects() hubclient.ProjectService {
	return s.projects
}
func (s *stubUsageHubClient) ProjectAgents(string) hubclient.AgentService { return nil }

type stubUsageService struct {
	lastMethod string
	lastID     string
	lastOpts   *hubclient.UsageQueryOptions

	summaryResp *hubclient.UsageSummaryResponse
	agentResp   *hubclient.AgentUsageResponse
	err         error
}

func (s *stubUsageService) GetProject(_ context.Context, projectID string, opts *hubclient.UsageQueryOptions) (*hubclient.UsageSummaryResponse, error) {
	s.lastMethod = "project"
	s.lastID = projectID
	s.lastOpts = opts
	return s.summaryResp, s.err
}

func (s *stubUsageService) GetMe(_ context.Context, opts *hubclient.UsageQueryOptions) (*hubclient.UsageSummaryResponse, error) {
	s.lastMethod = "me"
	s.lastID = ""
	s.lastOpts = opts
	return s.summaryResp, s.err
}

func (s *stubUsageService) GetUser(_ context.Context, userID string, opts *hubclient.UsageQueryOptions) (*hubclient.UsageSummaryResponse, error) {
	s.lastMethod = "user"
	s.lastID = userID
	s.lastOpts = opts
	return s.summaryResp, s.err
}

func (s *stubUsageService) GetGlobal(_ context.Context, opts *hubclient.UsageQueryOptions) (*hubclient.UsageSummaryResponse, error) {
	s.lastMethod = "global"
	s.lastID = ""
	s.lastOpts = opts
	return s.summaryResp, s.err
}

func (s *stubUsageService) GetAgent(_ context.Context, agentID string, opts *hubclient.UsageQueryOptions) (*hubclient.AgentUsageResponse, error) {
	s.lastMethod = "agent"
	s.lastID = agentID
	s.lastOpts = opts
	return s.agentResp, s.err
}

func int64Ptr(v int64) *int64 { return &v }
func strPtr(v string) *string { return &v }

func withUsageTestTarget(t *testing.T, svc *stubUsageService, defaultProjectID string) {
	t.Helper()
	origLoad := usageLoadHubClient
	origResolve := usageResolveProject
	origProject := projectPath
	origProjectSource := projectSource
	origGlobal := globalMode
	origFormat := outputFormat
	origZone := clitime.Zone()

	t.Cleanup(func() {
		usageLoadHubClient = origLoad
		usageResolveProject = origResolve
		projectPath = origProject
		projectSource = origProjectSource
		globalMode = origGlobal
		outputFormat = origFormat
		clitime.SetZone(origZone)
	})

	projectPath = ""
	projectSource = ""
	globalMode = false
	outputFormat = ""
	clitime.SetZone(time.UTC)

	usageLoadHubClient = func() (hubclient.Client, bool, error) {
		return &stubUsageHubClient{usage: svc}, false, nil
	}
	usageResolveProject = func(_ context.Context, _ hubclient.Client, _, _ bool) (resolvedHubProject, error) {
		return resolvedHubProject{ID: defaultProjectID}, nil
	}
}

func TestUsageCLI_ScopeDefaultingAndFlags(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "human")
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	baseSummary := &hubclient.UsageSummaryResponse{
		Scope:             hubclient.UsageScopeRef{Kind: "project", ID: "proj-1"},
		Window:            hubclient.UsageWindow{Since: now.AddDate(0, 0, -6), Until: now, Granularity: "day"},
		MeasurementStatus: "measured",
	}

	t.Run("human mode inside project defaults to project", func(t *testing.T) {
		svc := &stubUsageService{summaryResp: baseSummary}
		withUsageTestTarget(t, svc, "11111111-1111-1111-1111-111111111111")

		cmd := newUsageCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs([]string{"--since", "7d", "--by", "model,day"})
		require.NoError(t, cmd.Execute())

		assert.Equal(t, "project", svc.lastMethod)
		assert.Equal(t, "11111111-1111-1111-1111-111111111111", svc.lastID)
		require.NotNil(t, svc.lastOpts)
		assert.Equal(t, "7d", svc.lastOpts.Since)
		assert.Equal(t, []string{"model", "day"}, svc.lastOpts.GroupBy)
	})

	t.Run("human mode outside project defaults to /me", func(t *testing.T) {
		svc := &stubUsageService{summaryResp: baseSummary}
		withUsageTestTarget(t, svc, "")

		cmd := newUsageCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		require.NoError(t, cmd.Execute())

		assert.Equal(t, "me", svc.lastMethod)
	})

	t.Run("--user me routes to GetMe and --user id routes to GetUser", func(t *testing.T) {
		svc := &stubUsageService{summaryResp: baseSummary}
		withUsageTestTarget(t, svc, "11111111-1111-1111-1111-111111111111")

		cmd := newUsageCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--user", "me"})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, "me", svc.lastMethod)

		cmd2 := newUsageCmd()
		cmd2.SetOut(&bytes.Buffer{})
		cmd2.SetArgs([]string{"--user", "user-42"})
		require.NoError(t, cmd2.Execute())
		assert.Equal(t, "user", svc.lastMethod)
		assert.Equal(t, "user-42", svc.lastID)
	})

	t.Run("--all routes to GetGlobal", func(t *testing.T) {
		svc := &stubUsageService{summaryResp: baseSummary}
		withUsageTestTarget(t, svc, "11111111-1111-1111-1111-111111111111")

		cmd := newUsageCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--all", "--limit", "50"})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, "global", svc.lastMethod)
		assert.Equal(t, 50, svc.lastOpts.Limit)
	})

	t.Run("--agent routes to GetAgent, rejects --by and --limit, and gives clear error on unresolved slug (O3)", func(t *testing.T) {
		svc := &stubUsageService{
			agentResp: &hubclient.AgentUsageResponse{
				AgentID:           "22222222-2222-2222-2222-222222222222",
				ProjectID:         "11111111-1111-1111-1111-111111111111",
				Window:            hubclient.UsageWindow{Since: now, Until: now},
				MeasurementStatus: "measured",
			},
		}
		withUsageTestTarget(t, svc, "")

		cmdBadBy := newUsageCmd()
		cmdBadBy.SetOut(&bytes.Buffer{})
		cmdBadBy.SetArgs([]string{"--agent", "22222222-2222-2222-2222-222222222222", "--by", "model"})
		err := cmdBadBy.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--by is not supported with --agent")

		cmdBadLimit := newUsageCmd()
		cmdBadLimit.SetOut(&bytes.Buffer{})
		cmdBadLimit.SetArgs([]string{"--agent", "22222222-2222-2222-2222-222222222222", "--limit", "10"})
		err = cmdBadLimit.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--limit is not supported with --agent")

		cmdBadSlug := newUsageCmd()
		cmdBadSlug.SetOut(&bytes.Buffer{})
		cmdBadSlug.SetArgs([]string{"--agent", "my-agent-slug"})
		err = cmdBadSlug.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "pass the agent UUID")

		cmdOK := newUsageCmd()
		cmdOK.SetOut(&bytes.Buffer{})
		cmdOK.SetArgs([]string{"--agent", "22222222-2222-2222-2222-222222222222", "--since", "24h"})
		require.NoError(t, cmdOK.Execute())
		assert.Equal(t, "agent", svc.lastMethod)
		assert.Equal(t, "22222222-2222-2222-2222-222222222222", svc.lastID)
	})

	t.Run("mutually exclusive user-typed scope flags fail, while context-injected project allows --user/--all/--agent", func(t *testing.T) {
		svc := &stubUsageService{summaryResp: baseSummary}
		withUsageTestTarget(t, svc, "")

		cmd := newUsageCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--user", "me", "--all"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at most one of")

		// User-typed --project conflicts with --user.
		projectPath = "proj-x"
		projectSource = ""
		cmd2 := newUsageCmd()
		cmd2.SetOut(&bytes.Buffer{})
		cmd2.SetArgs([]string{"--user", "me"})
		err = cmd2.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at most one of")

		// Context-injected project (targetSourceContext) must NOT conflict with --user or --all.
		projectSource = targetSourceContext
		cmd3 := newUsageCmd()
		cmd3.SetOut(&bytes.Buffer{})
		cmd3.SetArgs([]string{"--user", "me"})
		require.NoError(t, cmd3.Execute())
		assert.Equal(t, "me", svc.lastMethod)
	})

	t.Run("--by rejects invalid dimension and day+hour combination", func(t *testing.T) {
		svc := &stubUsageService{summaryResp: baseSummary}
		withUsageTestTarget(t, svc, "")

		cmd := newUsageCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetArgs([]string{"--by", "bogus"})
		err := cmd.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid --by dimension")

		cmd2 := newUsageCmd()
		cmd2.SetOut(&bytes.Buffer{})
		cmd2.SetArgs([]string{"--by", "day,hour"})
		err = cmd2.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both day and hour")
	})
}

// TestUsageCLI_RealProjectResolver drives the real loadHubClientForProjectResolution
// and resolveHubProject against an httptest fake Hub for R4 cases (a), (b), and (c).
func TestUsageCLI_RealProjectResolver(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "human")
	t.Setenv("SCION_HUB_TOKEN", "test-token")

	const (
		ctxProjID      = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		slugProjID     = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
		localUnlinked  = "cccccccc-cccc-cccc-cccc-cccccccccccc"
		targetAgentUID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	)

	var seenPaths []string
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPaths = append(seenPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/my-slug":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": slugProjID, "slug": "my-slug", "name": "My Slug",
			})
		case "/api/v1/projects":
			if r.URL.Query().Get("slug") == "my-slug" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"projects": []map[string]any{
						{"id": slugProjID, "slug": "my-slug", "name": "My Slug"},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"projects": []any{}})
		case "/api/v1/agents/" + targetAgentUID + "/usage":
			_ = json.NewEncoder(w).Encode(hubclient.AgentUsageResponse{
				AgentID:           targetAgentUID,
				ProjectID:         ctxProjID,
				Window:            hubclient.UsageWindow{Since: now, Until: now},
				MeasurementStatus: "measured",
			})
		default:
			_ = json.NewEncoder(w).Encode(hubclient.UsageSummaryResponse{
				Scope:             hubclient.UsageScopeRef{Kind: "project", ID: ctxProjID},
				Window:            hubclient.UsageWindow{Since: now, Until: now, Granularity: "day"},
				MeasurementStatus: "measured",
			})
		}
	}))
	defer srv.Close()

	origProject := projectPath
	origProjectSource := projectSource
	origGlobal := globalMode
	origHubEndpoint := hubEndpoint
	origHubSource := hubSource
	origFormat := outputFormat
	origZone := clitime.Zone()
	origWD, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		projectPath = origProject
		projectSource = origProjectSource
		globalMode = origGlobal
		hubEndpoint = origHubEndpoint
		hubSource = origHubSource
		outputFormat = origFormat
		clitime.SetZone(origZone)
		_ = os.Chdir(origWD)
	})

	clitime.SetZone(time.UTC)
	t.Setenv("SCION_HUB_ENDPOINT", srv.URL)
	t.Setenv("SCION_HUB_PROFILE", "")
	t.Setenv("SCION_PROJECT_ID", "")
	t.Setenv("SCION_HUB_PROJECT_ID", "")

	t.Run("(a) with scion use context project, plain usage uses context project while --all/--user/--agent succeed", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		outsideDir := t.TempDir()
		require.NoError(t, os.Chdir(outsideDir))

		// Simulate resolveInvocationTarget injecting a `scion use` context project.
		projectPath = ctxProjID
		projectSource = targetSourceContext
		globalMode = false
		hubEndpoint = ""
		hubSource = ""
		outputFormat = ""

		// Plain `scion usage` -> context project.
		seenPaths = nil
		cmdPlain := newUsageCmd()
		cmdPlain.SetOut(&bytes.Buffer{})
		require.NoError(t, cmdPlain.Execute())
		assert.Equal(t, []string{"/api/v1/projects/" + ctxProjID + "/usage"}, seenPaths)

		// `scion usage --all` -> /api/v1/usage.
		seenPaths = nil
		cmdAll := newUsageCmd()
		cmdAll.SetOut(&bytes.Buffer{})
		cmdAll.SetArgs([]string{"--all"})
		require.NoError(t, cmdAll.Execute())
		assert.Equal(t, []string{"/api/v1/usage"}, seenPaths)

		// `scion usage --user me` -> /api/v1/me/usage.
		seenPaths = nil
		cmdUser := newUsageCmd()
		cmdUser.SetOut(&bytes.Buffer{})
		cmdUser.SetArgs([]string{"--user", "me"})
		require.NoError(t, cmdUser.Execute())
		assert.Equal(t, []string{"/api/v1/me/usage"}, seenPaths)

		// `scion usage --agent <uuid>` -> /api/v1/agents/<uuid>/usage.
		seenPaths = nil
		cmdAgent := newUsageCmd()
		cmdAgent.SetOut(&bytes.Buffer{})
		cmdAgent.SetArgs([]string{"--agent", targetAgentUID})
		require.NoError(t, cmdAgent.Execute())
		assert.Equal(t, []string{"/api/v1/agents/" + targetAgentUID + "/usage"}, seenPaths)
	})

	t.Run("(b) inside an unlinked local project, never sends local project-id; falls back to /me with stderr note", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		localProjDir := t.TempDir()
		scionDir := filepath.Join(localProjDir, ".scion")
		require.NoError(t, os.MkdirAll(scionDir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(scionDir, "settings.yaml"), []byte("schema_version: \"1\"\nproject_id: "+localUnlinked+"\n"), 0o600))
		require.NoError(t, os.Chdir(localProjDir))

		projectPath = ""
		projectSource = ""
		globalMode = false
		hubEndpoint = ""
		hubSource = ""
		outputFormat = ""

		seenPaths = nil
		var outBuf, errBuf bytes.Buffer
		cmd := newUsageCmd()
		cmd.SetOut(&outBuf)
		cmd.SetErr(&errBuf)
		require.NoError(t, cmd.Execute())

		assert.Equal(t, []string{"/api/v1/me/usage"}, seenPaths)
		assert.NotContains(t, seenPaths, "/api/v1/projects/"+localUnlinked+"/usage")
		assert.Contains(t, errBuf.String(), "not linked to a hub")
	})

	t.Run("(c) --project <slug> from outside any project resolves via the Hub", func(t *testing.T) {
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		outsideDir := t.TempDir()
		require.NoError(t, os.Chdir(outsideDir))

		projectPath = "my-slug"
		projectSource = ""
		globalMode = false
		hubEndpoint = ""
		hubSource = ""
		outputFormat = ""

		seenPaths = nil
		cmd := newUsageCmd()
		cmd.SetOut(&bytes.Buffer{})
		require.NoError(t, cmd.Execute())

		assert.Contains(t, seenPaths, "/api/v1/projects/"+slugProjID+"/usage")
	})
}

func TestUsageCLI_AgentModeBehavior(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	svc := &stubUsageService{
		summaryResp: &hubclient.UsageSummaryResponse{
			Scope:             hubclient.UsageScopeRef{Kind: "project", ID: "11111111-1111-1111-1111-111111111111"},
			Window:            hubclient.UsageWindow{Since: now, Until: now, Granularity: "day"},
			MeasurementStatus: "measured",
		},
	}
	withUsageTestTarget(t, svc, "11111111-1111-1111-1111-111111111111")

	t.Run("defaults to agent own project", func(t *testing.T) {
		cmd := newUsageCmd()
		cmd.SetOut(&bytes.Buffer{})
		require.NoError(t, cmd.Execute())
		assert.Equal(t, "project", svc.lastMethod)
		assert.Equal(t, "11111111-1111-1111-1111-111111111111", svc.lastID)
	})

	t.Run("refuses --user and --all in agent mode", func(t *testing.T) {
		cmdUser := newUsageCmd()
		cmdUser.SetOut(&bytes.Buffer{})
		cmdUser.SetArgs([]string{"--user", "me"})
		err := cmdUser.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--user is available only in human mode")

		cmdAll := newUsageCmd()
		cmdAll.SetOut(&bytes.Buffer{})
		cmdAll.SetArgs([]string{"--all"})
		err = cmdAll.Execute()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--all is available only in human mode")
	})

	t.Run("usage is in agentAllowed and survives applyAgentMode", func(t *testing.T) {
		assert.True(t, agentAllowed["usage"], "agentAllowed must include usage (D10)")
		root, ran := gateTestTree(t)
		applyAgentMode(root)
		root.SetArgs([]string{"usage"})
		require.NoError(t, root.Execute())
		assert.Equal(t, []string{"usage"}, *ran)
	})
}

func TestUsageCLI_TableRendering_NullPartialSaturated(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "human")
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	svc := &stubUsageService{
		summaryResp: &hubclient.UsageSummaryResponse{
			Scope:      hubclient.UsageScopeRef{Kind: "project", ID: "proj-1"},
			Window:     hubclient.UsageWindow{Since: now.AddDate(0, 0, -6), Until: now, Granularity: "day"},
			GroupBy:    []string{"model"},
			Calls:      42,
			ErrorCalls: 1,
			Tokens: hubclient.UsageTokens{
				Input:          int64Ptr(120000),
				Output:         int64Ptr(34500),
				CacheRead:      int64Ptr(0),
				CacheWrite:     nil, // unknown -> must render as "—", never 0
				Reasoning:      nil,
				SummableTokens: int64Ptr(math.MaxInt64),
				UnknownEvents:  &hubclient.UsageBucketUnknownEvents{Output: 3},
				Saturated:      true,
			},
			Groups: []hubclient.UsageGroupRow{
				{
					Model:      strPtr("claude-sonnet-4-6"),
					Calls:      42,
					ErrorCalls: 1,
					Tokens: hubclient.UsageTokens{
						Input:          int64Ptr(120000),
						Output:         int64Ptr(34500),
						CacheRead:      int64Ptr(0),
						CacheWrite:     nil,
						SummableTokens: int64Ptr(154500),
						UnknownEvents:  &hubclient.UsageBucketUnknownEvents{Output: 3},
					},
				},
			},
			Truncated:         true,
			MeasurementStatus: "stale",
			StaleAgents:       1,
			UnmeasuredAgents:  2,
		},
	}
	withUsageTestTarget(t, svc, "proj-1")

	cmd := newUsageCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--by", "model"})
	require.NoError(t, cmd.Execute())

	got := out.String()
	assert.Contains(t, got, "Status:  stale (1 stale, 2 unmeasured)")
	assert.Contains(t, got, "120,000")
	assert.Contains(t, got, "34,500*")
	assert.Contains(t, got, usageUnknownCell)
	assert.Contains(t, got, "9,223,372,036,854,775,807*+")
	assert.Contains(t, got, "claude-sonnet-4-6")
	assert.Contains(t, got, "* partial:")
	assert.Contains(t, got, "+ saturated:")
	assert.Contains(t, got, "truncated")
}

func TestUsageCLI_JSONPassthrough(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "human")
	raw := json.RawMessage(`{"scope":{"kind":"user","id":"u1"},"measurement_status":"measured","extra_field":123}`)
	svc := &stubUsageService{
		summaryResp: &hubclient.UsageSummaryResponse{
			MeasurementStatus: "measured",
			Raw:               raw,
		},
	}
	withUsageTestTarget(t, svc, "")

	cmd := newUsageCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--json"})
	require.NoError(t, cmd.Execute())
	assert.JSONEq(t, string(raw), out.String())
}

func TestUsageCLI_WindowsAllowed(t *testing.T) {
	assert.True(t, windowsAllowed["usage"], "windowsAllowed must include usage (D10)")
	root, ran := gateTestTree(t)
	applyPlatformRestrictions(root, "windows")

	root.SetArgs([]string{"usage"})
	require.NoError(t, root.Execute())
	assert.Equal(t, []string{"usage"}, *ran)
}

func TestUsageCLI_FormatAPIError(t *testing.T) {
	notFound := &apiclient.APIError{StatusCode: http.StatusNotFound, Code: "not_found", Message: "anything"}

	assert.ErrorIs(t, formatUsageAPIError(notFound, false), errUsageExperimentDisabled)

	entityErr := formatUsageAPIError(notFound, true)
	assert.ErrorIs(t, entityErr, notFound)
	assert.Contains(t, entityErr.Error(), "tokenomics.usage_ledger")
}

func TestUsageCLI_TimeZoneFormatting(t *testing.T) {
	origZone := clitime.Zone()
	t.Cleanup(func() { clitime.SetZone(origZone) })

	bucket := time.Date(2026, 11, 27, 23, 0, 0, 0, time.UTC)
	row := hubclient.UsageGroupRow{BucketStart: &bucket}
	win := hubclient.UsageWindow{
		Since:       time.Date(2026, 11, 27, 0, 0, 0, 0, time.UTC),
		Until:       bucket,
		Granularity: "hour",
	}

	clitime.SetZone(time.UTC)
	assert.Equal(t, "2026-11-27 00:00 UTC .. 2026-11-27 23:00 UTC (hour)", formatUsageWindow(win))
	assert.Equal(t, "2026-11-27 UTC", formatGroupDimCell("day", row, "day"))
	assert.Equal(t, "2026-11-27 UTC", formatGroupDimCell("hour", row, "day"))
	assert.Equal(t, "2026-11-27 23:00 UTC", formatGroupDimCell("hour", row, "hour"))

	jst := time.FixedZone("JST", 9*3600)
	clitime.SetZone(jst)
	assert.Equal(t, "2026-11-27 09:00 JST .. 2026-11-28 08:00 JST (hour)", formatUsageWindow(win))
	assert.Equal(t, "2026-11-28 JST", formatGroupDimCell("day", row, "day"))
	assert.Equal(t, "2026-11-28 JST", formatGroupDimCell("hour", row, "day"))
	assert.Equal(t, "2026-11-28 08:00 JST", formatGroupDimCell("hour", row, "hour"))
}
