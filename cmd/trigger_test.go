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
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerCLIModesAndWindowsGate(t *testing.T) {
	paths := []string{
		"trigger",
		"trigger.create",
		"trigger.get",
		"trigger.list",
		"trigger.delete",
		"trigger.pause",
		"trigger.resume",
		"trigger.history",
		"trigger.event",
		"trigger.event.list",
		"trigger.event.get",
		"trigger.webhook",
		"trigger.webhook.create",
		"trigger.webhook.get",
		"trigger.webhook.list",
		"trigger.webhook.delete",
	}

	for _, path := range paths {
		assert.True(t, agentAllowed[path], "agentAllowed[%q]", path)
		assert.True(t, windowsAllowed[path], "windowsAllowed[%q]", path)
		assert.NotNil(t, resolveCommandPath(rootCmd, path), "rootCmd must contain %q", path)
	}

	realTrigger := resolveCommandPath(rootCmd, "trigger")
	require.NotNil(t, realTrigger)

	for _, mode := range []CLIMode{ModeHuman, ModeAgent} {
		root := &cobra.Command{Use: "scion"}
		root.AddCommand(cloneCommandShape(realTrigger))
		applyModeRestrictions(root, mode)
		remaining := collectCommandNames(root)
		for _, path := range paths {
			assert.Contains(t, remaining, path, "%s in %s mode", path, mode)
		}
	}

	winTree, ran := gateTestTree(t)
	applyPlatformRestrictions(winTree, "windows")
	for _, args := range [][]string{
		{"trigger", "list"},
		{"trigger", "get", "t1"},
		{"trigger", "create", "--name", "t1", "--webhook", "wh1", "--agent", "lead", "--message", "hi"},
		{"trigger", "delete", "t1"},
		{"trigger", "pause", "t1"},
		{"trigger", "resume", "t1"},
		{"trigger", "history", "t1"},
		{"trigger", "event", "list"},
		{"trigger", "event", "get", "evt-1"},
		{"trigger", "webhook", "list"},
		{"trigger", "webhook", "get", "wh1"},
		{"trigger", "webhook", "create", "--name", "wh1"},
		{"trigger", "webhook", "delete", "wh1"},
	} {
		*ran = nil
		winTree.SetArgs(args)
		winTree.SetOut(&bytes.Buffer{})
		winTree.SetErr(&bytes.Buffer{})
		require.NoError(t, winTree.Execute(), "args=%v", args)
		require.Len(t, *ran, 1, "args=%v", args)
	}
}

func TestTriggerWebhookCreateHelp_DisclosuresAndSigningExample(t *testing.T) {
	help := triggerWebhookCreateCmd.Long
	assert.Contains(t, help, "anyone with read access to this project's triggers can read bodies sent here")
	assert.Contains(t, help, "64 KiB")
	assert.Contains(t, help, "agent's\n    transcript")
	assert.Contains(t, help, "X-Scion-Signature")
	assert.Contains(t, help, "X-Scion-Delivery")
	assert.Contains(t, help, "X-Scion-Event")
	assert.Contains(t, help, "X-Scion-Subject")
	assert.Contains(t, help, "openssl dgst -sha256 -hmac")
	assert.Contains(t, help, "--to-self")
	assert.Contains(t, help, "--agent")
	assert.Contains(t, help, "--message")
	assert.Contains(t, help, "X-Scion-Signature: t=<unix_seconds>,v1=<hex_hmac_sha256>")
}

func TestBuildCreateTriggerRequest(t *testing.T) {
	origName, origDesc, origWebhook, origGitHub := triggerName, triggerDescription, triggerWebhookRef, triggerGitHubSource
	origTypes, origWhere, origWhereFile := triggerMatchTypes, triggerWhereRules, triggerWhereFile
	origAgent, origMessage := triggerAgent, triggerMessage
	origInt, origWake, origPlain := triggerInterrupt, triggerWake, triggerPlain
	origSubj, origOnSubj := triggerSubjectExpr, triggerOnSubject
	t.Cleanup(func() {
		triggerName, triggerDescription, triggerWebhookRef, triggerGitHubSource = origName, origDesc, origWebhook, origGitHub
		triggerMatchTypes, triggerWhereRules, triggerWhereFile = origTypes, origWhere, origWhereFile
		triggerAgent, triggerMessage = origAgent, origMessage
		triggerInterrupt, triggerWake, triggerPlain = origInt, origWake, origPlain
		triggerSubjectExpr, triggerOnSubject = origSubj, origOnSubj
	})

	resetFlags := func() {
		triggerName = "deploy-hook"
		triggerDescription = "Deploy trigger"
		triggerWebhookRef = "deploys"
		triggerGitHubSource = false
		triggerMatchTypes = nil
		triggerWhereRules = nil
		triggerWhereFile = ""
		triggerAgent = "lead"
		triggerMessage = "check deploy"
		triggerInterrupt = false
		triggerWake = false
		triggerPlain = false
		triggerSubjectExpr = ""
		triggerOnSubject = ""
	}

	t.Run("missing required flags", func(t *testing.T) {
		resetFlags()
		triggerName = ""
		_, err := buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--name is required")

		resetFlags()
		triggerWebhookRef = ""
		_, err = buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "either --webhook or --github is required")

		resetFlags()
		triggerAgent = ""
		_, err = buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--agent is required")

		resetFlags()
		triggerMessage = "   "
		_, err = buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--message is required")
	})

	t.Run("github source validation and request shape", func(t *testing.T) {
		resetFlags()
		triggerGitHubSource = true
		triggerWebhookRef = "deploys"
		_, err := buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--github and --webhook are mutually exclusive")

		resetFlags()
		triggerGitHubSource = true
		triggerWebhookRef = ""
		triggerMatchTypes = nil
		_, err = buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--type is required when --github is set")

		resetFlags()
		triggerGitHubSource = true
		triggerWebhookRef = ""
		triggerMatchTypes = []string{"github.issue_comment.created"}
		req, err := buildCreateTriggerRequest()
		require.NoError(t, err)
		assert.Equal(t, hubclient.TriggerSourceSpec{Kind: "github", ID: ""}, req.Source)
		require.NotNil(t, req.Match)
		assert.Equal(t, []string{"github.issue_comment.created"}, req.Match.Types)
	})

	t.Run("agent name vs agent UUID", func(t *testing.T) {
		resetFlags()
		req, err := buildCreateTriggerRequest()
		require.NoError(t, err)
		assert.Equal(t, "lead", req.Action.AgentName)
		assert.Empty(t, req.Action.AgentID)

		resetFlags()
		agentUUID := "11111111-2222-4333-8444-555555555555"
		triggerAgent = agentUUID
		req, err = buildCreateTriggerRequest()
		require.NoError(t, err)
		assert.Equal(t, agentUUID, req.Action.AgentID)
		assert.Empty(t, req.Action.AgentName)
	})

	t.Run("optional match, repeatable where, registered dialect prefix, subject, and action flags", func(t *testing.T) {
		resetFlags()
		// Exercise real Cobra flag parsing so StringArrayVar comma preservation and order are verified.
		require.NoError(t, triggerCreateCmd.Flags().Parse([]string{
			"--name", "deploy-hook",
			"--webhook", "deploys",
			"--agent", "lead",
			"--message", "check deploy",
			"--type", "webhook.deploy.*,webhook.release",
			"--where", `event.type in ["webhook.deploy.started", "webhook.deploy.finished"]`,
			"--where", `cel:event.attributes["subject"] == "svc:api"`,
			"--where", `system_one:Does this change touch billing?`,
			"--where", `unknown_prefix:still_cel_text`,
			"--subject", `data.service`,
			"--on-subject", "skip",
			"--interrupt",
			"--wake",
			"--plain",
		}))

		req, err := buildCreateTriggerRequest()
		require.NoError(t, err)
		require.NotNil(t, req.Match)
		assert.Equal(t, []string{"webhook.deploy.*", "webhook.release"}, req.Match.Types)
		assert.Empty(t, req.Match.Expr)
		assert.Equal(t, []hubclient.TriggerMatchRuleSpec{
			{Dialect: "cel", Expr: `event.type in ["webhook.deploy.started", "webhook.deploy.finished"]`},
			{Dialect: "cel", Expr: `event.attributes["subject"] == "svc:api"`},
			{Dialect: "system_one", Expr: `Does this change touch billing?`},
			{Dialect: "cel", Expr: `unknown_prefix:still_cel_text`},
		}, req.Match.Rules)
		require.NotNil(t, req.Subject)
		assert.Equal(t, `data.service`, req.Subject.Expr)
		assert.Equal(t, "skip", req.Subject.Mode)
		assert.True(t, req.Action.Interrupt)
		assert.True(t, req.Action.Wake)
		assert.True(t, req.Action.Plain)
	})

	t.Run("where-file JSON array and conflict with --where", func(t *testing.T) {
		resetFlags()
		dir := t.TempDir()
		validFile := filepath.Join(dir, "rules.json")
		require.NoError(t, os.WriteFile(validFile, []byte(`[
			{"expr": "event.type == 'webhook.received'"},
			{"dialect": "cel", "expr": "has(event.payload.pr)"},
			{"dialect": "system_one", "expr": "Is this security-relevant?"}
		]`), 0o600))

		triggerWhereFile = validFile
		req, err := buildCreateTriggerRequest()
		require.NoError(t, err)
		require.NotNil(t, req.Match)
		assert.Equal(t, []hubclient.TriggerMatchRuleSpec{
			{Dialect: "cel", Expr: "event.type == 'webhook.received'"},
			{Dialect: "cel", Expr: "has(event.payload.pr)"},
			{Dialect: "system_one", Expr: "Is this security-relevant?"},
		}, req.Match.Rules)

		// Passing both --where and --where-file is rejected as a usage error.
		triggerWhereRules = []string{"true"}
		_, err = buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--where and --where-file cannot be used together")

		// Non-array JSON or unknown fields in --where-file are rejected.
		resetFlags()
		badObjFile := filepath.Join(dir, "bad-obj.json")
		require.NoError(t, os.WriteFile(badObjFile, []byte(`{"dialect":"cel","expr":"true"}`), 0o600))
		triggerWhereFile = badObjFile
		_, err = buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected JSON array")

		badUnknownFile := filepath.Join(dir, "bad-field.json")
		require.NoError(t, os.WriteFile(badUnknownFile, []byte(`[{"dialect":"cel","expression":"true"}]`), 0o600))
		triggerWhereFile = badUnknownFile
		_, err = buildCreateTriggerRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown field")
	})
}

func withMockTriggerHub(t *testing.T, handler http.HandlerFunc, cred hubsync.CredentialKind) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origFn := triggerHubContextFn
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{
			Client:         client,
			Endpoint:       srv.URL,
			ProjectID:      "proj-1",
			CredentialKind: cred,
		}, "proj-1", ctx, cancel, nil
	}
	t.Cleanup(func() {
		triggerHubContextFn = origFn
	})
}

func TestTriggerWebhookCLI_CreateListDelete(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origWhName, origWhDesc, origWhStatus := triggerWebhookName, triggerWebhookDescription, triggerWebhookListStatus
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerWebhookName, triggerWebhookDescription, triggerWebhookListStatus = origWhName, origWhDesc, origWhStatus
	})

	ep1ID := "aaaaaaaa-1111-4222-8333-444444444444"
	ep2ID := "aaaaaaaa-9999-4222-8333-555555555555"
	lastRecv := fixedNow.Add(-10 * time.Minute)
	var deletedIDs []string

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			var body hubclient.CreateWebhookEndpointRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.WebhookEndpointCreated{
				WebhookEndpoint: hubclient.WebhookEndpoint{
					ID:            ep1ID,
					ProjectID:     "proj-1",
					Name:          body.Name,
					Description:   body.Description,
					URL:           "https://hub.example.com/api/v1/hooks/" + ep1ID,
					Status:        "active",
					SignatureMode: "scion_v1",
					CreatedAt:     fixedNow,
				},
				Secret: "one-time-hmac-secret-abc",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{
				Scope: hubclient.TriggerListScopeCaller,
				Endpoints: []hubclient.WebhookEndpoint{
					{
						ID:             ep1ID,
						ProjectID:      "proj-1",
						Name:           "deploys",
						URL:            "https://hub.example.com/api/v1/hooks/" + ep1ID,
						Status:         "active",
						SignatureMode:  "scion_v1",
						ReceiveCount:   4,
						LastReceivedAt: &lastRecv,
						CreatedAt:      fixedNow,
					},
					{
						ID:            ep2ID,
						ProjectID:     "proj-1",
						Name:          "alerts",
						URL:           "https://hub.example.com/api/v1/hooks/" + ep2ID,
						Status:        "active",
						SignatureMode: "scion_v1",
						CreatedAt:     fixedNow,
					},
				},
				TotalCount: 2,
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints/"+ep1ID:
			deletedIDs = append(deletedIDs, ep1ID)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindAgentToken)

	// 1. Create (human-readable output includes secret, one-time warning, and F11 visibility note)
	outputFormat = ""
	triggerWebhookName = "deploys"
	triggerWebhookDescription = "CI deploy hooks"
	createOut := captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookCreate(triggerWebhookCreateCmd, nil))
	})
	assert.Contains(t, createOut, "one-time-hmac-secret-abc")
	assert.Contains(t, createOut, "shown only once")
	assert.Contains(t, createOut, "anyone with read access to this project's triggers can read bodies sent here")

	// 2. List (caller-scoped notice + relative time via clitime.Ago)
	triggerWebhookListStatus = ""
	listOut := captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookList(triggerWebhookListCmd, nil))
	})
	assert.Contains(t, listOut, "owned by this agent")
	assert.Contains(t, listOut, "deploys")
	assert.Contains(t, listOut, "10m ago")

	// 3. Delete by exact name resolves to ep1ID; ambiguous 8-char prefix "aaaaaaaa" is rejected
	_, _ = captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookDelete(triggerWebhookDeleteCmd, []string{"deploys"}))
	}), deletedIDs
	assert.Equal(t, []string{ep1ID}, deletedIDs)

	err := runTriggerWebhookDelete(triggerWebhookDeleteCmd, []string{"aaaaaaaa"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous webhook endpoint ID prefix")
}

func TestTriggerCLI_CreateListPauseResumeDelete(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origName, origWebhook, origAgent, origMsg := triggerName, triggerWebhookRef, triggerAgent, triggerMessage
	origListSource, origListWebhook, origListStatus := triggerListSource, triggerListWebhook, triggerListStatus
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerName, triggerWebhookRef, triggerAgent, triggerMessage = origName, origWebhook, origAgent, origMsg
		triggerListSource, triggerListWebhook, triggerListStatus = origListSource, origListWebhook, origListStatus
	})

	epID := "bbbbbbbb-1111-4222-8333-444444444444"
	trID := "cccccccc-1111-4222-8333-444444444444"
	lastFire := fixedNow.Add(-5 * time.Minute)
	var seenActions []string
	var lastSourceIDQuery string

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{
				Endpoints: []hubclient.WebhookEndpoint{
					{ID: epID, ProjectID: "proj-1", Name: "deploys", Status: "active", SignatureMode: "scion_v1"},
				},
				TotalCount: 1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			var req hubclient.CreateTriggerRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			seenActions = append(seenActions, "create:"+req.Name)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:                    trID,
				ProjectID:             "proj-1",
				Name:                  req.Name,
				Status:                "active",
				AuthorizationRevision: 1,
				Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
				Match:                 hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}},
				Action:                req.Action,
				CreatedAt:             fixedNow,
				UpdatedAt:             fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			lastSourceIDQuery = r.URL.Query().Get("sourceId")
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{
				Triggers: []hubclient.Trigger{
					{
						ID:                    trID,
						ProjectID:             "proj-1",
						Name:                  "deploy-notify",
						Status:                "paused",
						StatusReason:          "source_deleted",
						AuthorizationRevision: 2,
						Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
						Match:                 hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}},
						Action:                hubclient.TriggerActionSpec{Type: "message", AgentName: "lead", Message: "hi"},
						FireCount:             3,
						LastFireAt:            &lastFire,
						LastFireStatus:        "fired",
						CreatedAt:             fixedNow,
						UpdatedAt:             fixedNow,
					},
				},
				TotalCount: 1,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/pause":
			seenActions = append(seenActions, "pause:"+trID)
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{ID: trID, Name: "deploy-notify", Status: "paused"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/resume":
			seenActions = append(seenActions, "resume:"+trID)
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{ID: trID, Name: "deploy-notify", Status: "active"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID:
			seenActions = append(seenActions, "delete:"+trID)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	outputFormat = ""
	triggerName = "deploy-notify"
	triggerWebhookRef = "deploys"
	triggerAgent = "lead"
	triggerMessage = "check deploy"
	createOut := captureStdout(t, func() {
		require.NoError(t, runTriggerCreate(triggerCreateCmd, nil))
	})
	assert.Contains(t, createOut, "Trigger created: "+trID)
	assert.Contains(t, createOut, "webhook.received")

	// List with --webhook <name> resolves endpoint name -> UUID before querying triggers
	triggerListSource = ""
	triggerListWebhook = "deploys"
	triggerListStatus = ""
	listOut := captureStdout(t, func() {
		require.NoError(t, runTriggerList(triggerListCmd, nil))
	})
	assert.Equal(t, epID, lastSourceIDQuery)
	assert.Contains(t, listOut, "deploy-notify")
	assert.Contains(t, listOut, "paused (source_deleted)")
	assert.Contains(t, listOut, "5m ago (fired)")

	// Pause by exact name, resume by 8-char ID prefix, delete by full UUID
	triggerListWebhook = ""
	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerPause(triggerPauseCmd, []string{"deploy-notify"}))
		require.NoError(t, runTriggerResume(triggerResumeCmd, []string{"cccccccc"}))
		require.NoError(t, runTriggerDelete(triggerDeleteCmd, []string{trID}))
	})
	assert.Equal(t, []string{
		"create:deploy-notify",
		"pause:" + trID,
		"resume:" + trID,
		"delete:" + trID,
	}, seenActions)
}

func TestTriggerCLI_PaginationAndResolutionBeyondFirstPage(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origListSource, origListWebhook, origListStatus := triggerListSource, triggerListWebhook, triggerListStatus
	origWebhookListStatus := triggerWebhookListStatus
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerListSource, triggerListWebhook, triggerListStatus = origListSource, origListWebhook, origListStatus
		triggerWebhookListStatus = origWebhookListStatus
	})

	// Seed 120 triggers (served in 50-row pages regardless of requested limit to
	// simulate server-capped pagination) and 15 webhook endpoints (served in
	// 5-row pages).
	const totalTriggers = 120
	triggers := make([]hubclient.Trigger, totalTriggers)
	for i := 0; i < totalTriggers; i++ {
		triggers[i] = hubclient.Trigger{
			ID:        fmt.Sprintf("%08x-1111-4222-8333-444444444444", 0x10000000+i),
			ProjectID: "proj-1",
			Name:      fmt.Sprintf("trig-%03d", i),
			Status:    "active",
			Source:    hubclient.TriggerSourceSpec{Kind: "webhook", ID: "ep-main"},
			Match:     hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}},
			Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "lead", Message: "hi"},
			CreatedAt: fixedNow,
			UpdatedAt: fixedNow,
		}
	}
	const totalEndpoints = 15
	endpoints := make([]hubclient.WebhookEndpoint, totalEndpoints)
	for i := 0; i < totalEndpoints; i++ {
		endpoints[i] = hubclient.WebhookEndpoint{
			ID:            fmt.Sprintf("%08x-9999-4222-8333-555555555555", 0x20000000+i),
			ProjectID:     "proj-1",
			Name:          fmt.Sprintf("wh-%02d", i),
			URL:           fmt.Sprintf("https://hub.example.com/api/v1/hooks/%02d", i),
			Status:        "active",
			SignatureMode: "scion_v1",
			CreatedAt:     fixedNow,
			UpdatedAt:     fixedNow,
		}
	}

	var triggerGetQueries []string
	var seenMutations []string

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			triggerGetQueries = append(triggerGetQueries, r.URL.RawQuery)
			if name := r.URL.Query().Get("name"); name != "" {
				var matched []hubclient.Trigger
				for _, tr := range triggers {
					if tr.Name == name {
						matched = append(matched, tr)
					}
				}
				_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{
					Triggers:   matched,
					TotalCount: len(matched),
				})
				return
			}
			offset := 0
			if c := r.URL.Query().Get("cursor"); c != "" {
				var err error
				offset, err = strconv.Atoi(c)
				require.NoError(t, err)
			}
			end := offset + 50
			next := ""
			if end < len(triggers) {
				next = strconv.Itoa(end)
			} else {
				end = len(triggers)
			}
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{
				Triggers:   triggers[offset:end],
				NextCursor: next,
				TotalCount: len(triggers),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			if name := r.URL.Query().Get("name"); name != "" {
				var matched []hubclient.WebhookEndpoint
				for _, ep := range endpoints {
					if ep.Name == name {
						matched = append(matched, ep)
					}
				}
				_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{
					Endpoints:  matched,
					TotalCount: len(matched),
				})
				return
			}
			offset := 0
			if c := r.URL.Query().Get("cursor"); c != "" {
				var err error
				offset, err = strconv.Atoi(c)
				require.NoError(t, err)
			}
			end := offset + 5
			next := ""
			if end < len(endpoints) {
				next = strconv.Itoa(end)
			} else {
				end = len(endpoints)
			}
			_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{
				Endpoints:  endpoints[offset:end],
				NextCursor: next,
				TotalCount: len(endpoints),
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+triggers[115].ID+"/pause":
			seenMutations = append(seenMutations, "pause:"+triggers[115].ID)
			_ = json.NewEncoder(w).Encode(triggers[115])
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+triggers[118].ID+"/resume":
			seenMutations = append(seenMutations, "resume:"+triggers[118].ID)
			_ = json.NewEncoder(w).Encode(triggers[118])
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints/"+endpoints[13].ID:
			seenMutations = append(seenMutations, "delete-ep:"+endpoints[13].ID)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints/"+endpoints[14].ID:
			seenMutations = append(seenMutations, "delete-ep:"+endpoints[14].ID)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// 1. `scion trigger list` walks all 3 pages and prints row 119 (beyond page 1's 50).
	outputFormat = ""
	triggerListSource, triggerListWebhook, triggerListStatus = "", "", ""
	listOut := captureStdout(t, func() {
		require.NoError(t, runTriggerList(triggerListCmd, nil))
	})
	assert.Contains(t, listOut, "trig-000")
	assert.Contains(t, listOut, "trig-065")
	assert.Contains(t, listOut, "trig-119")

	// JSON output returns all 120 items with nextCursor cleared.
	outputFormat = "json"
	jsonOut := captureStdout(t, func() {
		require.NoError(t, runTriggerList(triggerListCmd, nil))
	})
	var parsed hubclient.ListTriggersResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
	assert.Len(t, parsed.Triggers, totalTriggers)
	assert.Empty(t, parsed.NextCursor)

	// 2. Exact name resolution on page 3 (`trig-115`) uses `?name=trig-115` in a single GET!
	outputFormat = ""
	triggerGetQueries = nil
	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerPause(triggerPauseCmd, []string{"trig-115"}))
	})
	require.Len(t, triggerGetQueries, 1, "exact name match should resolve via ?name= without full-page scan")
	assert.Contains(t, triggerGetQueries[0], "name=trig-115")

	// 3. Unique ID prefix on page 3 (`10000076`) paginates across all 3 pages after ?name= misses!
	triggerGetQueries = nil
	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerResume(triggerResumeCmd, []string{triggers[118].ID[:8]}))
	})
	assert.Len(t, triggerGetQueries, 4, "1 exact-name probe + 3 cursor pages")

	// 4. `scion trigger webhook list` and `scion trigger webhook delete` paginate beyond page 1.
	triggerWebhookListStatus = ""
	epListOut := captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookList(triggerWebhookListCmd, nil))
	})
	assert.Contains(t, epListOut, "wh-00")
	assert.Contains(t, epListOut, "wh-14")

	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookDelete(triggerWebhookDeleteCmd, []string{"wh-13"}))
		require.NoError(t, runTriggerWebhookDelete(triggerWebhookDeleteCmd, []string{endpoints[14].ID[:8]}))
	})

	assert.Equal(t, []string{
		"pause:" + triggers[115].ID,
		"resume:" + triggers[118].ID,
		"delete-ep:" + endpoints[13].ID,
		"delete-ep:" + endpoints[14].ID,
	}, seenMutations)
}

type fakeNilListHubClient struct {
	hubclient.Client
	triggers  hubclient.TriggerService
	endpoints hubclient.WebhookEndpointService
}

func (f *fakeNilListHubClient) Triggers(string) hubclient.TriggerService {
	return f.triggers
}

func (f *fakeNilListHubClient) WebhookEndpoints(string) hubclient.WebhookEndpointService {
	return f.endpoints
}

type fakeTriggerService struct {
	hubclient.TriggerService
	listFn func(ctx context.Context, opts *hubclient.ListTriggersOptions) (*hubclient.ListTriggersResponse, error)
}

func (f *fakeTriggerService) List(ctx context.Context, opts *hubclient.ListTriggersOptions) (*hubclient.ListTriggersResponse, error) {
	return f.listFn(ctx, opts)
}

type fakeWebhookEndpointService struct {
	hubclient.WebhookEndpointService
	listFn func(ctx context.Context, opts *hubclient.ListWebhookEndpointsOptions) (*hubclient.ListWebhookEndpointsResponse, error)
}

func (f *fakeWebhookEndpointService) List(ctx context.Context, opts *hubclient.ListWebhookEndpointsOptions) (*hubclient.ListWebhookEndpointsResponse, error) {
	return f.listFn(ctx, opts)
}

func TestTriggerCLI_NilListResponseGuard(t *testing.T) {
	withFakeClient := func(t *testing.T, trSvc hubclient.TriggerService, epSvc hubclient.WebhookEndpointService) *HubContext {
		t.Helper()
		hubCtx := &HubContext{
			Client: &fakeNilListHubClient{
				triggers:  trSvc,
				endpoints: epSvc,
			},
			Endpoint:       "https://hub.example.com",
			ProjectID:      "proj-1",
			CredentialKind: hubsync.CredentialKindOAuth,
		}
		origFn := triggerHubContextFn
		triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			return hubCtx, "proj-1", ctx, cancel, nil
		}
		t.Cleanup(func() { triggerHubContextFn = origFn })
		return hubCtx
	}

	t.Run("listAllTriggers nil first page and nil subsequent page", func(t *testing.T) {
		origSource, origWebhook, origStatus := triggerListSource, triggerListWebhook, triggerListStatus
		t.Cleanup(func() {
			triggerListSource, triggerListWebhook, triggerListStatus = origSource, origWebhook, origStatus
		})
		triggerListSource, triggerListWebhook, triggerListStatus = "", "", ""

		withFakeClient(t, &fakeTriggerService{
			listFn: func(context.Context, *hubclient.ListTriggersOptions) (*hubclient.ListTriggersResponse, error) {
				return nil, nil
			},
		}, nil)
		err := runTriggerList(triggerListCmd, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty response from hub")

		calls := 0
		_, err = listAllTriggers(context.Background(), &fakeTriggerService{
			listFn: func(context.Context, *hubclient.ListTriggersOptions) (*hubclient.ListTriggersResponse, error) {
				calls++
				if calls == 1 {
					return &hubclient.ListTriggersResponse{NextCursor: "p2"}, nil
				}
				return nil, nil
			},
		}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty response from hub")
	})

	t.Run("resolveTriggerIDOrName nil name-probe response", func(t *testing.T) {
		withFakeClient(t, &fakeTriggerService{
			listFn: func(context.Context, *hubclient.ListTriggersOptions) (*hubclient.ListTriggersResponse, error) {
				return nil, nil
			},
		}, nil)
		err := runTriggerDelete(triggerDeleteCmd, []string{"deploy-notify"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty response from hub")
	})

	t.Run("listAllWebhookEndpoints nil first page and nil subsequent page", func(t *testing.T) {
		origStatus := triggerWebhookListStatus
		t.Cleanup(func() { triggerWebhookListStatus = origStatus })
		triggerWebhookListStatus = ""

		withFakeClient(t, nil, &fakeWebhookEndpointService{
			listFn: func(context.Context, *hubclient.ListWebhookEndpointsOptions) (*hubclient.ListWebhookEndpointsResponse, error) {
				return nil, nil
			},
		})
		err := runTriggerWebhookList(triggerWebhookListCmd, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty response from hub")

		calls := 0
		_, err = listAllWebhookEndpoints(context.Background(), &fakeWebhookEndpointService{
			listFn: func(context.Context, *hubclient.ListWebhookEndpointsOptions) (*hubclient.ListWebhookEndpointsResponse, error) {
				calls++
				if calls == 1 {
					return &hubclient.ListWebhookEndpointsResponse{NextCursor: "p2"}, nil
				}
				return nil, nil
			},
		}, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty response from hub")
	})

	t.Run("resolveWebhookEndpointIDOrName nil name-probe response", func(t *testing.T) {
		withFakeClient(t, nil, &fakeWebhookEndpointService{
			listFn: func(context.Context, *hubclient.ListWebhookEndpointsOptions) (*hubclient.ListWebhookEndpointsResponse, error) {
				return nil, nil
			},
		})
		err := runTriggerWebhookDelete(triggerWebhookDeleteCmd, []string{"deploys"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "empty response from hub")
	})
}

func TestTriggerWebhookCreateHelp_SenderContractAndLimits(t *testing.T) {
	helpText := triggerWebhookCreateCmd.Long
	assert.Contains(t, helpText, "anyone with read access to this project's triggers can read bodies sent here")
	assert.Contains(t, helpText, "at most 64 KiB (HTTP 413 above")
	assert.Contains(t, helpText, `openssl dgst -sha256 -hmac "$SECRET"`)
	assert.Contains(t, helpText, `printf 'v1\n%s\n%s\n%s\n%s\n%s' "$TS" "$DELIVERY" "$EVENT" "$SUBJECT" "$BODY" | openssl`)
	assert.NotContains(t, helpText, `SIGNED="$(`, "help example must pipe directly into openssl so command substitution never strips trailing newlines from BODY")

	// Prove the documented canonical string + ASCII secret HMAC formula passes
	// VerifyScionV1Signature, including when BODY ends with a trailing newline.
	now := time.Date(2026, 11, 16, 19, 30, 0, 0, time.UTC)
	ts := strconv.FormatInt(now.Unix(), 10)
	secret := "dGVzdC1zZWNyZXQtYmFzZTY0dXJsLWFzY2lpLWtleQ"
	delivery := "550e8400-e29b-41d4-a716-446655440000"
	event := "deploy.finished"
	subject := "service/api"
	body := []byte("{\"status\":\"ok\",\"commit\":\"abc1234\"}\n")

	canonical := fmt.Sprintf("v1\n%s\n%s\n%s\n%s\n%s", ts, delivery, event, subject, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	sigHeader := fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))

	assert.True(t, trigger.VerifyScionV1Signature(secret, now, sigHeader, delivery, event, subject, body))
}

func TestTriggerWebhookCreateHelp_CurlExampleFiresDefaultTrigger(t *testing.T) {
	helpText := triggerWebhookCreateCmd.Long
	idx := strings.Index(helpText, "Example (curl + openssl")
	require.NotEqual(t, -1, idx, "expected curl + openssl example section in triggerWebhookCreateCmd.Long")
	exampleBlock := helpText[idx:]

	eventAssign := regexp.MustCompile(`(?m)^\s*EVENT="([^"]*)"`).FindStringSubmatch(exampleBlock)
	require.Len(t, eventAssign, 2, "expected EVENT=\"...\" variable in curl example:\n%s", exampleBlock)
	eventVar := eventAssign[1]

	// Determine the X-Scion-Event header actually sent by curl in the example.
	sentEventHeader := ""
	headerMatches := regexp.MustCompile(`-H\s+"([^"]+)"`).FindAllStringSubmatch(exampleBlock, -1)
	require.NotEmpty(t, headerMatches, "expected -H headers in curl example")
	for _, m := range headerMatches {
		hdr := m[1]
		if strings.HasPrefix(hdr, trigger.HeaderEvent+":") {
			val := strings.TrimSpace(strings.TrimPrefix(hdr, trigger.HeaderEvent+":"))
			val = strings.ReplaceAll(val, "${EVENT}", eventVar)
			val = strings.ReplaceAll(val, "$EVENT", eventVar)
			sentEventHeader = val
		}
	}
	assert.Empty(t, sentEventHeader, "printed curl example should omit X-Scion-Event so it fires a default webhook.received trigger")
	assert.Equal(t, sentEventHeader, eventVar, "signed EVENT field must match the X-Scion-Event header sent by curl")

	// Derive the real Hub event.type from the example's X-Scion-Event header and assert
	// it matches the default trigger match.types (["webhook.received"]).
	derivedEventType := trigger.WebhookEventType(sentEventHeader)
	assert.Equal(t, "webhook.received", derivedEventType)
	assert.True(t, triggerrule.MatchTypes([]string{"webhook.received"}, derivedEventType),
		"help curl example produces event.type %q, which would not match default trigger match.types [webhook.received]", derivedEventType)

	// Verify the canonical signature signed with EVENT="" passes VerifyScionV1Signature when X-Scion-Event is omitted.
	now := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	ts := strconv.FormatInt(now.Unix(), 10)
	secret := "dGVzdC1zZWNyZXQtYmFzZTY0dXJsLWFzY2lpLWtleQ"
	delivery := "550e8400-e29b-41d4-a716-446655440000"
	subject := "service/api"
	body := []byte(`{"status":"ok","commit":"abc1234"}`)
	canonical := fmt.Sprintf("v1\n%s\n%s\n%s\n%s\n%s", ts, delivery, eventVar, subject, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	sigHeader := fmt.Sprintf("t=%s,v1=%s", ts, hex.EncodeToString(mac.Sum(nil)))
	assert.True(t, trigger.VerifyScionV1Signature(secret, now, sigHeader, delivery, sentEventHeader, subject, body))

	flatHelp := strings.Join(strings.Fields(helpText), " ")
	assert.Contains(t, flatHelp, "X-Scion-Event: <name> sets event.type to webhook.<name>; match it with scion trigger create --type webhook.<name>")
}

func TestTriggerHistoryCLI_NameAndUUIDResolutionFlagsAndFormats(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origLimit, origCursor := triggerHistoryLimit, triggerHistoryCursor
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerHistoryLimit, triggerHistoryCursor = origLimit, origCursor
	})

	trID := "11111111-2222-4333-8444-555555555555"
	emptyID := "22222222-2222-4333-8444-555555555555"
	started := fixedNow.Add(-2 * time.Minute)
	finished := fixedNow.Add(-90 * time.Second)
	var seenHistoryQuery string

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{
				Triggers: []hubclient.Trigger{
					{
						ID:        trID,
						ProjectID: "proj-1",
						Name:      "deploy-hook",
						Status:    "active",
					},
				},
				TotalCount: 1,
				ServerTime: fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/history":
			seenHistoryQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerFiresResponse{
				Fires: []hubclient.TriggerFire{
					{
						ID:         "33333333-4444-4555-8666-777777777777",
						TriggerID:  trID,
						EventID:    "44444444-5555-4666-8777-888888888888",
						ProjectID:  "proj-1",
						Subject:    "webhook:ep-1/payments",
						Status:     "fired",
						StartedAt:  started,
						FinishedAt: &finished,
					},
				},
				NextCursor: "cursor-next-1",
				TotalCount: 3,
				ServerTime: fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+emptyID+"/history":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerFiresResponse{
				Fires:      []hubclient.TriggerFire{},
				TotalCount: 0,
				ServerTime: fixedNow,
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindAgentToken)

	// 1. Negative --limit -> usage error.
	triggerHistoryLimit = -1
	triggerHistoryCursor = ""
	err := runTriggerHistory(triggerHistoryCmd, []string{trID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--limit must be non-negative")

	// 2. Resolve by name + --limit/--cursor + human table output.
	outputFormat = ""
	triggerHistoryLimit = 1
	triggerHistoryCursor = "cursor-prev-0"
	tableOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{"deploy-hook"}))
	})
	assert.Contains(t, seenHistoryQuery, "limit=1")
	assert.Contains(t, seenHistoryQuery, "cursor=cursor-prev-0")
	assert.Contains(t, tableOut, "FIRE HISTORY (1 of 3 fires)")
	assert.Contains(t, tableOut, "33333333")
	assert.Contains(t, tableOut, "44444444")
	assert.Contains(t, tableOut, "fired")
	assert.Contains(t, tableOut, "webhook:ep-1/payments")
	assert.Contains(t, tableOut, "Next cursor: cursor-next-1")

	// 3. JSON output by UUID.
	outputFormat = "json"
	triggerHistoryLimit = 0
	triggerHistoryCursor = ""
	jsonOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{trID}))
	})
	var parsed hubclient.ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
	assert.Equal(t, 3, parsed.TotalCount)
	assert.Equal(t, "cursor-next-1", parsed.NextCursor)
	require.Len(t, parsed.Fires, 1)
	assert.Equal(t, "fired", parsed.Fires[0].Status)

	// 4. Empty history message.
	outputFormat = ""
	emptyOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{emptyID}))
	})
	assert.Contains(t, emptyOut, "No fire history found.")
}

func TestTriggerCreateHelp_RegexAndCELAuthorGuidance(t *testing.T) {
	help := strings.Join(strings.Fields(triggerCreateCmd.Long), " ")
	assert.Contains(t, help, "--where")
	assert.Contains(t, help, "--where-file")
	assert.Contains(t, help, "--subject")
	assert.Contains(t, help, "Put cheap CEL guards first")
	assert.Contains(t, help, "short-circuit")
	assert.Contains(t, help, "source.kind")
	assert.Contains(t, help, "source.id")
	assert.Contains(t, help, "occurred_at")
	assert.Contains(t, help, "received_at")
	assert.Contains(t, help, "trigger.{id,name,project_id}")
	assert.Contains(t, help, "has(")
	assert.Contains(t, help, "missing_input")
	assert.NotContains(t, help, "event.time")
	assert.NotContains(t, help, `"branch"`)
	assert.Contains(t, help, "literal RE2 patterns only")
	assert.Contains(t, help, "event.attributes.<k>")
	assert.Contains(t, help, "event.payload.<field>")
	assert.Contains(t, help, "not on the whole payload")
	assert.Contains(t, help, "64 KiB")
	assert.Contains(t, help, `skipped ("rule evaluation failed")`)

	whereFlag := triggerCreateCmd.Flags().Lookup("where")
	require.NotNil(t, whereFlag)
	assert.Contains(t, whereFlag.Usage, "cheap CEL guards first")
	assert.Contains(t, whereFlag.Usage, "event.attributes.<k>")
	assert.Contains(t, whereFlag.Usage, "event.payload.<field>")

	whereFileFlag := triggerCreateCmd.Flags().Lookup("where-file")
	require.NotNil(t, whereFileFlag)

	subjectFlag := triggerCreateCmd.Flags().Lookup("subject")
	require.NotNil(t, subjectFlag)
	assert.Contains(t, subjectFlag.Usage, "1024B")
}

func TestTriggerCreateHelp_ExampleCompilesAndEvaluatesOnWebhookEvent(t *testing.T) {
	help := triggerCreateCmd.Long

	whereMatches := regexp.MustCompile(`--where '([^']+)'`).FindAllStringSubmatch(help, -1)
	require.GreaterOrEqual(t, len(whereMatches), 2, "expected repeatable --where '<cel>' example in triggerCreateCmd.Long")

	subjectMatch := regexp.MustCompile(`--subject '([^']+)'`).FindStringSubmatch(help)
	require.Len(t, subjectMatch, 2, "expected --subject '<cel>' example in triggerCreateCmd.Long")
	subjectExpr := subjectMatch[1]

	rules, err := triggerrule.NewDefault()
	require.NoError(t, err)

	ctx := context.Background()
	whereRules := make([]triggerrule.Rule, len(whereMatches))
	for i, m := range whereMatches {
		parsed := parseTriggerWhereArg(m[1])
		whereRules[i] = triggerrule.Rule{Dialect: parsed.Dialect, Expr: parsed.Expr}
		require.NoError(t, rules.CompileMatch(ctx, whereRules[i]), "element %d", i)
	}
	subjectRule := triggerrule.Rule{Dialect: "cel", Expr: subjectExpr}
	require.NoError(t, rules.CompileSubject(ctx, subjectRule))

	now := time.Date(2026, 11, 17, 0, 30, 0, 0, time.UTC)
	trRef := hubfire.FireTriggerRef{
		ID:   "dddddddd-1111-4222-8333-444444444444",
		Name: "prod-service-deploy",
	}
	scope := celengine.CacheScope{
		Consumer: triggerrule.ConsumerName,
		ID:       trRef.ID,
		Revision: 1,
	}

	// §8.2-shaped webhook event matching the help example (X-Scion-Subject: release/v1.4.0, env=prod, service=billing).
	matchingPayload := []byte(`{"env":"prod","service":"billing"}`)
	matchingEvent := hubfire.FireEvent{
		ID:         "eeeeeeee-1111-4222-8333-444444444444",
		ProjectID:  "proj-1",
		SourceKind: "webhook",
		SourceID:   "bbbbbbbb-1111-4222-8333-444444444444",
		Type:       "webhook.received",
		Subject:    "webhook:bbbbbbbb-1111-4222-8333-444444444444/release/v1.4.0",
		OccurredAt: now,
		ReceivedAt: now,
		Attributes: map[string]string{
			"endpoint_name":  "deploys",
			"event":          "received",
			"delivery":       "deliv-101",
			"subject":        "release/v1.4.0",
			"signature_mode": "scion_v1",
			"content_type":   "application/json",
			"payload_bytes":  strconv.Itoa(len(matchingPayload)),
			"remote_ip":      "203.0.113.10",
		},
		Payload: matchingPayload,
	}
	matchingAct, err := triggerrule.NewActivation(matchingEvent, trRef)
	require.NoError(t, err)

	matchRes, err := rules.MatchAll(ctx, scope, whereRules, matchingAct)
	require.NoError(t, err)
	assert.True(t, matchRes.Matched, "help --where list should match §8.2 prod release event")
	assert.Equal(t, len(whereRules), matchRes.Through)
	assert.Equal(t, triggerrule.MatchClassMatched, matchRes.Class)

	derivedSubj, err := rules.Subject(ctx, scope, subjectRule, matchingAct)
	require.NoError(t, err)
	assert.Equal(t, "billing", derivedSubj)

	// §8.2-shaped webhook event with missing optional JSON keys short-circuits on element 1 cleanly to false (not missing_input).
	sparseEvent := matchingEvent
	sparseEvent.ID = "ffffffff-1111-4222-8333-444444444444"
	sparseEvent.Payload = []byte(`{}`)
	sparseAct, err := triggerrule.NewActivation(sparseEvent, trRef)
	require.NoError(t, err)

	sparseRes, err := rules.MatchAll(ctx, scope, whereRules, sparseAct)
	require.NoError(t, err, "missing optional payload keys must evaluate cleanly to false, not missing_input")
	assert.False(t, sparseRes.Matched)
	assert.Equal(t, 1, sparseRes.Through, "cheap first CEL guard must short-circuit before element 2")
	assert.Equal(t, triggerrule.MatchClassFalse, sparseRes.Class)

	fallbackSubj, err := rules.Subject(ctx, scope, subjectRule, sparseAct)
	require.NoError(t, err)
	assert.Equal(t, matchingEvent.Subject, fallbackSubj)
}

func TestTriggerCLI_WhereSubjectCreateAndPositionBearingErrors(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origName, origWebhook, origAgent, origMsg := triggerName, triggerWebhookRef, triggerAgent, triggerMessage
	origWhere, origWhereFile, origSubject := triggerWhereRules, triggerWhereFile, triggerSubjectExpr
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerName, triggerWebhookRef, triggerAgent, triggerMessage = origName, origWebhook, origAgent, origMsg
		triggerWhereRules, triggerWhereFile, triggerSubjectExpr = origWhere, origWhereFile, origSubject
	})

	const (
		epID = "bbbbbbbb-1111-4222-8333-444444444444"
		trID = "dddddddd-1111-4222-8333-444444444444"
	)

	var lastReq hubclient.CreateTriggerRequest
	mode := "ok"

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/projects/proj-1/triggers" {
			http.NotFound(w, r)
			return
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&lastReq))
		switch mode {
		case "ok":
			w.WriteHeader(http.StatusCreated)
			matchResp := hubclient.TriggerMatchSpec{
				Types: []string{"webhook.received"},
				Rules: lastReq.Match.Rules,
			}
			if len(lastReq.Match.Rules) == 1 {
				matchResp.Dialect = lastReq.Match.Rules[0].Dialect
				matchResp.Expr = lastReq.Match.Rules[0].Expr
			}
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:        trID,
				ProjectID: "proj-1",
				Name:      lastReq.Name,
				Status:    "active",
				Source:    hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
				Match:     matchResp,
				Subject: hubclient.TriggerSubjectSpec{
					Dialect: "cel",
					Expr:    lastReq.Subject.Expr,
					Mode:    "new",
				},
				Action:    lastReq.Action,
				CreatedAt: fixedNow,
				UpdatedAt: fixedNow,
			})
		case "compile_err_details_only":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "invalid_rule",
					"message": "undeclared reference to 'unknown_var' (in container '')",
					"details": map[string]any{
						"field":    "match",
						"kind":     "filter",
						"category": "unknown_variable",
						"message":  "undeclared reference to 'unknown_var' (in container '')",
						"line":     2,
						"column":   7,
					},
				},
			})
		case "compile_err_multi_elements":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "invalid_rule",
					"message": "multiple invalid elements",
					"details": map[string]any{
						"field": "match",
						"elements": []map[string]any{
							{
								"element":  1,
								"category": "syntax",
								"message":  "Syntax error",
								"line":     1,
								"column":   4,
							},
							{
								"element": 2,
								"dialect": "system_one",
								"reason":  "dialect_not_allowed",
							},
						},
					},
				},
			})
		case "compile_err_full_hub":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "invalid_rule",
					"message": "invalid subject rule at line 1, column 1 (result_type): expression evaluates to bool, want string",
					"details": map[string]any{
						"field":    "subject",
						"kind":     "subject",
						"category": "result_type",
						"message":  "expression evaluates to bool, want string",
						"line":     1,
						"column":   1,
					},
				},
			})
		case "dialect_err":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    "invalid_rule",
					"message": `unsupported match dialect "system_one"`,
					"details": map[string]any{
						"field":    "match",
						"dialect":  "system_one",
						"consumer": "trigger",
						"reason":   "dialect_not_allowed",
					},
				},
			})
		}
	}, hubsync.CredentialKindOAuth)

	outputFormat = ""
	triggerName = "cel-trigger"
	triggerWebhookRef = "deploys"
	triggerAgent = "lead"
	triggerMessage = "check deploy"
	triggerWhereFile = ""
	triggerWhereRules = []string{`has(event.payload) && event.payload.env == "prod"`}
	triggerSubjectExpr = `string(event.payload.service)`

	out := captureStdout(t, func() {
		require.NoError(t, runTriggerCreate(triggerCreateCmd, nil))
	})
	require.NotNil(t, lastReq.Match)
	assert.Equal(t, []hubclient.TriggerMatchRuleSpec{
		{Dialect: "cel", Expr: `has(event.payload) && event.payload.env == "prod"`},
	}, lastReq.Match.Rules)
	require.NotNil(t, lastReq.Subject)
	assert.Equal(t, `string(event.payload.service)`, lastReq.Subject.Expr)
	assert.Contains(t, out, fmt.Sprintf("Where:   1/1 [cel] %q", `has(event.payload) && event.payload.env == "prod"`))
	assert.Contains(t, out, fmt.Sprintf("Subject: %q", `string(event.payload.service)`))

	// Position-bearing compile error formatted from details when message lacks line/column.
	mode = "compile_err_details_only"
	err := runTriggerCreate(triggerCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_rule")
	assert.Contains(t, err.Error(), "line 2, column 7")
	assert.Contains(t, err.Error(), "unknown_variable")
	assert.Contains(t, err.Error(), "unknown_var")

	// Multi-element details.elements[] error reports every failing element with 1-based index, position, and dialect reason.
	mode = "compile_err_multi_elements"
	err = runTriggerCreate(triggerCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[element 1] at line 1, column 4 (syntax)")
	assert.Contains(t, err.Error(), `[element 2]: dialect_not_allowed`)

	// Position-bearing compile error from Hub message preserved without duplicating position.
	mode = "compile_err_full_hub"
	err = runTriggerCreate(triggerCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid subject rule at line 1, column 1 (result_type)")
	assert.Equal(t, 1, bytes.Count([]byte(err.Error()), []byte("line 1, column 1")))

	// Unsupported dialect error surfaces UnsupportedDialectError.Reason clearly.
	mode = "dialect_err"
	err = runTriggerCreate(triggerCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "dialect_not_allowed")
}

func TestTriggerCLI_GetNumberedElementsAndJSON(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	t.Cleanup(func() { outputFormat = origFormat })

	const (
		epID     = "bbbbbbbb-1111-4222-8333-444444444444"
		multiID  = "cccccccc-1111-4222-8333-444444444444"
		emptyID  = "dddddddd-1111-4222-8333-444444444444"
		brokenID = "eeeeeeee-1111-4222-8333-444444444444"
	)

	multiTr := hubclient.Trigger{
		ID:                    multiID,
		ProjectID:             "proj-1",
		Name:                  "ordered-guard",
		Description:           "Cheap CEL before semantic",
		Status:                "active",
		AuthorizationRevision: 3,
		Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
		Match: hubclient.TriggerMatchSpec{
			Types: []string{"webhook.received"},
			Rules: []hubclient.TriggerMatchRuleSpec{
				{Dialect: "cel", Expr: `event.attributes["event"] == "pull_request"`},
				{Dialect: "cel", Expr: `has(event.payload.pr)`},
				{Dialect: "system_one", Expr: `Does this PR touch auth?`},
			},
		},
		Subject: hubclient.TriggerSubjectSpec{
			Dialect: "cel",
			Expr:    `event.subject`,
			Mode:    "new",
		},
		Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "lead", Message: "review PR"},
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
	}

	emptyTr := hubclient.Trigger{
		ID:                    emptyID,
		ProjectID:             "proj-1",
		Name:                  "types-only",
		Status:                "active",
		AuthorizationRevision: 1,
		Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
		Match: hubclient.TriggerMatchSpec{
			Types: []string{"webhook.received"},
			Rules: []hubclient.TriggerMatchRuleSpec{},
		},
		Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "lead", Message: "ping"},
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
	}

	brokenTr := hubclient.Trigger{
		ID:                    brokenID,
		ProjectID:             "proj-1",
		Name:                  "corrupt-rules",
		Status:                "paused",
		AuthorizationRevision: 1,
		Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
		Match: hubclient.TriggerMatchSpec{
			Types:       []string{"webhook.received"},
			Rules:       []hubclient.TriggerMatchRuleSpec{},
			RulesBroken: true,
		},
		Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "lead", Message: "ping"},
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
	}

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			switch r.URL.Query().Get("name") {
			case "ordered-guard":
				_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{Triggers: []hubclient.Trigger{multiTr}, TotalCount: 1})
			case "types-only":
				_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{Triggers: []hubclient.Trigger{emptyTr}, TotalCount: 1})
			case "corrupt-rules":
				_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{Triggers: []hubclient.Trigger{brokenTr}, TotalCount: 1})
			default:
				_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{Triggers: []hubclient.Trigger{multiTr, emptyTr, brokenTr}, TotalCount: 3})
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+multiID:
			_ = json.NewEncoder(w).Encode(multiTr)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+emptyID:
			_ = json.NewEncoder(w).Encode(emptyTr)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+brokenID:
			_ = json.NewEncoder(w).Encode(brokenTr)
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// 1. Human-readable `scion trigger get <name>` numbers elements 1/N..N/N in evaluation order.
	outputFormat = ""
	humanOut := captureStdout(t, func() {
		require.NoError(t, runTriggerGet(triggerGetCmd, []string{"ordered-guard"}))
	})
	idx1 := strings.Index(humanOut, fmt.Sprintf("1/3 [cel] %q", `event.attributes["event"] == "pull_request"`))
	idx2 := strings.Index(humanOut, fmt.Sprintf("2/3 [cel] %q", `has(event.payload.pr)`))
	idx3 := strings.Index(humanOut, fmt.Sprintf("3/3 [system_one] %q", `Does this PR touch auth?`))
	require.NotEqual(t, -1, idx1, humanOut)
	require.NotEqual(t, -1, idx2, humanOut)
	require.NotEqual(t, -1, idx3, humanOut)
	assert.Less(t, idx1, idx2)
	assert.Less(t, idx2, idx3)

	// 2. Types-only trigger shows "none (types only)" in human output and `"rules": []` in --json.
	emptyHumanOut := captureStdout(t, func() {
		require.NoError(t, runTriggerGet(triggerGetCmd, []string{"types-only"}))
	})
	assert.Contains(t, emptyHumanOut, "Rules:       none (types only)")

	outputFormat = "json"
	emptyJSONOut := captureStdout(t, func() {
		require.NoError(t, runTriggerGet(triggerGetCmd, []string{emptyID}))
	})
	var rawMap map[string]any
	require.NoError(t, json.Unmarshal([]byte(emptyJSONOut), &rawMap))
	matchObj := rawMap["match"].(map[string]any)
	assert.Equal(t, []any{}, matchObj["rules"])

	// 3. Corrupt stored rules render a content-free broken state instead of "none (types only)".
	outputFormat = ""
	brokenHumanOut := captureStdout(t, func() {
		require.NoError(t, runTriggerGet(triggerGetCmd, []string{"corrupt-rules"}))
	})
	assert.Contains(t, brokenHumanOut, "Rules:       broken (stored match rules cannot be parsed)")
	assert.NotContains(t, brokenHumanOut, "none (types only)")
}

func TestTriggerCLI_RuleTextQuotedEscapesSpoofedElementLines(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origName, origWebhook, origAgent, origMsg := triggerName, triggerWebhookRef, triggerAgent, triggerMessage
	origWhere, origWhereFile, origSubject := triggerWhereRules, triggerWhereFile, triggerSubjectExpr
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerName, triggerWebhookRef, triggerAgent, triggerMessage = origName, origWebhook, origAgent, origMsg
		triggerWhereRules, triggerWhereFile, triggerSubjectExpr = origWhere, origWhereFile, origSubject
	})

	const (
		epID = "bbbbbbbb-1111-4222-8333-444444444444"
		trID = "ffffffff-1111-4222-8333-444444444444"
	)

	// Element 1 embeds a newline followed by a forged "    2/2 [cel] false" element line;
	// subject embeds a newline + control escape to attempt spoofing subsequent detail fields.
	spoofRule1 := "true &&\n    2/2 [cel] false"
	realRule2 := `event.payload.allow == true`
	spoofSubject := "string(event.subject) +\n  Agent:       attacker\x1b[31m"

	stored := hubclient.Trigger{
		ID:                    trID,
		ProjectID:             "proj-1",
		Name:                  "spoof-guard",
		Status:                "active",
		AuthorizationRevision: 1,
		Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
		Match: hubclient.TriggerMatchSpec{
			Types: []string{"webhook.received"},
			Rules: []hubclient.TriggerMatchRuleSpec{
				{Dialect: "cel", Expr: spoofRule1},
				{Dialect: "cel", Expr: realRule2},
			},
		},
		Subject: hubclient.TriggerSubjectSpec{
			Dialect: "cel",
			Expr:    spoofSubject,
			Mode:    "new",
		},
		Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "lead", Message: "ok"},
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
	}

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(stored)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID:
			_ = json.NewEncoder(w).Encode(stored)
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	assertNoSpoofedLines := func(t *testing.T, out string) {
		t.Helper()
		var elem2Lines []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "    2/2 ") {
				elem2Lines = append(elem2Lines, line)
			}
			assert.False(t, strings.HasPrefix(line, "  Agent:       attacker"), "newline in subject must not spoof Agent field: %s", out)
			assert.NotContains(t, line, "\x1b[", "control characters in rule text must be escaped: %s", out)
		}
		require.Equal(t, []string{fmt.Sprintf("    2/2 [cel] %q", realRule2)}, elem2Lines,
			"a newline inside rule 1 must never produce a fake 2/2 line:\n%s", out)
		assert.Contains(t, out, fmt.Sprintf("    1/2 [cel] %q", spoofRule1))
		assert.Contains(t, out, fmt.Sprintf("%q", spoofSubject))
	}

	outputFormat = ""
	triggerName = "spoof-guard"
	triggerWebhookRef = "deploys"
	triggerGitHubSource = false
	triggerAgent = "lead"
	triggerMessage = "ok"
	triggerWhereFile = ""
	triggerWhereRules = []string{spoofRule1, realRule2}
	triggerSubjectExpr = spoofSubject

	createOut := captureStdout(t, func() {
		require.NoError(t, runTriggerCreate(triggerCreateCmd, nil))
	})
	assertNoSpoofedLines(t, createOut)

	getOut := captureStdout(t, func() {
		require.NoError(t, runTriggerGet(triggerGetCmd, []string{trID}))
	})
	assertNoSpoofedLines(t, getOut)
}

func TestTriggerEventCLI_ListGetPayloadAndJSON(t *testing.T) {
	fixedNow := time.Date(2026, 11, 18, 12, 10, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))
	prevZone := clitime.Zone()
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(prevZone) })

	origFormat := outputFormat
	origEvSource, origEvWebhook, origEvStatus := triggerEventListSource, triggerEventListWebhook, triggerEventListStatus
	origEvSince, origEvLimit, origEvCursor := triggerEventListSince, triggerEventListLimit, triggerEventListCursor
	origEvPayload := triggerEventPayloadOnly
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerEventListSource, triggerEventListWebhook, triggerEventListStatus = origEvSource, origEvWebhook, origEvStatus
		triggerEventListSince, triggerEventListLimit, triggerEventListCursor = origEvSince, origEvLimit, origEvCursor
		triggerEventPayloadOnly = origEvPayload
	})

	epID := "bbbbbbbb-1111-4222-8333-444444444444"
	evID := "eeeeeeee-1111-4222-8333-444444444444"
	receivedAt := fixedNow.Add(-3 * time.Minute)
	processedAt := fixedNow.Add(-2 * time.Minute)
	var seenEventQuery string

	summary := hubclient.TriggerEventSummary{
		ID:         evID,
		ProjectID:  "proj-1",
		Source:     hubclient.TriggerSourceSpec{Kind: "github", ID: "777001"},
		Type:       "github.issue_comment.created",
		Subject:    "github:acme/widgets#42",
		DedupKey:   "github:550e8400-e29b-41d4-a716-446655440000",
		OccurredAt: receivedAt,
		ReceivedAt: receivedAt,
		Attributes: map[string]string{
			"command":      "ping",
			"command_args": "hello-stage3",
			"mention":      "true",
			"repo":         "acme/widgets",
		},
		Lists: map[string][]string{
			"assignees": {},
			"labels":    {"bug", "triggers"},
		},
		PayloadBytes: 96,
		Status:       "processed",
		MatchCount:   1,
		FireCount:    1,
		ProcessedAt:  &processedAt,
		CreatedAt:    receivedAt,
	}

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{
				Scope: hubclient.TriggerListScopeCaller,
				Endpoints: []hubclient.WebhookEndpoint{{
					ID:        epID,
					ProjectID: "proj-1",
					Name:      "deploys",
					Status:    "active",
					CreatedAt: fixedNow,
					UpdatedAt: fixedNow,
				}},
				TotalCount: 1,
				ServerTime: fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events":
			seenEventQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerEventsResponse{
				Scope:      hubclient.TriggerListScopeCaller,
				Events:     []hubclient.TriggerEventSummary{summary},
				TotalCount: 1,
				ServerTime: fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events/"+evID:
			_ = json.NewEncoder(w).Encode(hubclient.TriggerEventDetail{
				TriggerEventSummary: summary,
				Headers:             map[string]string{"x-github-event": "issue_comment"},
				Payload:             json.RawMessage(`{"action":"created","comment":{"body":"@scion /ping hello-stage3"}}`),
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindAgentToken)

	// 1. List trigger events (--source github --since ...).
	outputFormat = ""
	triggerEventListSource = "github"
	triggerEventListWebhook = ""
	triggerEventListStatus = "processed"
	triggerEventListSince = "2026-11-18T12:00:00Z"
	triggerEventListLimit = 0
	triggerEventListCursor = ""
	evListOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventList(triggerEventListCmd, nil))
	})
	assert.Contains(t, seenEventQuery, "source=github")
	assert.Contains(t, seenEventQuery, "status=processed")
	assert.Contains(t, seenEventQuery, "since=2026-11-18T12%3A00%3A00Z")
	assert.Contains(t, seenEventQuery, "limit=50")
	assert.Contains(t, evListOut, "visible to this agent")
	assert.Contains(t, evListOut, "eeeeeeee")
	assert.Contains(t, evListOut, "github:777001")
	assert.Contains(t, evListOut, "github:acme/widgets#42")
	assert.Contains(t, evListOut, "3m ago")

	// 2. List trigger events with --webhook resolves endpoint name and --json outputs ListTriggerEventsResponse.
	outputFormat = "json"
	triggerEventListSource = ""
	triggerEventListWebhook = "deploys"
	evListJSON := captureStdout(t, func() {
		require.NoError(t, runTriggerEventList(triggerEventListCmd, nil))
	})
	assert.Contains(t, seenEventQuery, "source=webhook")
	assert.Contains(t, seenEventQuery, "sourceId="+epID)
	var parsedList hubclient.ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal([]byte(evListJSON), &parsedList))
	assert.Equal(t, hubclient.TriggerListScopeCaller, parsedList.Scope)
	require.Len(t, parsedList.Events, 1)
	assert.Equal(t, evID, parsedList.Events[0].ID)

	// 3. Get trigger event by short ID prefix renders attributes, lists, headers, and projected payload.
	outputFormat = ""
	triggerEventPayloadOnly = false
	evGetOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventGet(triggerEventGetCmd, []string{"eeeeeeee"}))
	})
	assert.Contains(t, evGetOut, "Event:        "+evID)
	assert.Contains(t, evGetOut, "Type:         github.issue_comment.created")
	assert.Contains(t, evGetOut, "Source:       github:777001")
	assert.Contains(t, evGetOut, "Attributes:\n  command: ping\n  command_args: hello-stage3\n  mention: true\n  repo: acme/widgets")
	assert.Contains(t, evGetOut, "Lists:\n  assignees: []\n  labels: [\"bug\",\"triggers\"]")
	assert.Contains(t, evGetOut, "Headers:\n  x-github-event: issue_comment")
	assert.Contains(t, evGetOut, "\"@scion /ping hello-stage3\"")

	// 4. Get trigger event with --payload prints only valid pretty JSON payload.
	triggerEventPayloadOnly = true
	payloadOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventGet(triggerEventGetCmd, []string{evID}))
	})
	assert.JSONEq(t, `{"action":"created","comment":{"body":"@scion /ping hello-stage3"}}`, payloadOut)

	// 5. Get trigger event with --format json round-trips Lists and Attributes.
	triggerEventPayloadOnly = false
	outputFormat = "json"
	evJSONOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventGet(triggerEventGetCmd, []string{evID}))
	})
	var parsedDetail hubclient.TriggerEventDetail
	require.NoError(t, json.Unmarshal([]byte(evJSONOut), &parsedDetail))
	assert.Equal(t, "ping", parsedDetail.Attributes["command"])
	assert.Equal(t, []string{"bug", "triggers"}, parsedDetail.Lists["labels"])
}

func TestTriggerEventCLI_SinglePageDefaultCursorHintAndEarlyStopPrefixAmbiguity(t *testing.T) {
	fixedNow := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origEvSource, origEvWebhook, origEvStatus := triggerEventListSource, triggerEventListWebhook, triggerEventListStatus
	origEvSince, origEvLimit, origEvCursor := triggerEventListSince, triggerEventListLimit, triggerEventListCursor
	origEvPayload := triggerEventPayloadOnly
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerEventListSource, triggerEventListWebhook, triggerEventListStatus = origEvSource, origEvWebhook, origEvStatus
		triggerEventListSince, triggerEventListLimit, triggerEventListCursor = origEvSince, origEvLimit, origEvCursor
		triggerEventPayloadOnly = origEvPayload
	})

	ev1 := "abcd1111-1111-4222-8333-444444444444"
	ev2 := "abcd2222-1111-4222-8333-444444444444"
	var seenQueries []string

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/api/v1/projects/proj-1/trigger-events", r.URL.Path)
		seenQueries = append(seenQueries, r.URL.RawQuery)

		if r.URL.Query().Get("cursor") != "" {
			t.Fatalf("unexpected pagination request with cursor=%q", r.URL.Query().Get("cursor"))
		}
		_ = json.NewEncoder(w).Encode(hubclient.ListTriggerEventsResponse{
			Events: []hubclient.TriggerEventSummary{
				{
					ID:         ev1,
					ProjectID:  "proj-1",
					Source:     hubclient.TriggerSourceSpec{Kind: "github", ID: "777001"},
					Type:       "github.issue_comment.created",
					Subject:    "github:acme/widgets#1",
					ReceivedAt: fixedNow.Add(-time.Minute),
					Status:     "processed",
				},
				{
					ID:         ev2,
					ProjectID:  "proj-1",
					Source:     hubclient.TriggerSourceSpec{Kind: "github", ID: "777001"},
					Type:       "github.issue_comment.created",
					Subject:    "github:acme/widgets#2",
					ReceivedAt: fixedNow.Add(-2 * time.Minute),
					Status:     "processed",
				},
			},
			NextCursor: "page-2-token",
			TotalCount: 250,
			ServerTime: fixedNow,
		})
	}, hubsync.CredentialKindOAuth)

	// 1. Bare `scion trigger event list` fetches a single page (--limit 50) and prints next-cursor hint.
	outputFormat = ""
	triggerEventListSource, triggerEventListWebhook, triggerEventListStatus, triggerEventListSince = "", "", "", ""
	triggerEventListLimit = defaultTriggerEventListLimit
	triggerEventListCursor = ""
	listOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventList(triggerEventListCmd, nil))
	})
	require.Len(t, seenQueries, 1, "event list must default to a single page")
	assert.Contains(t, seenQueries[0], "limit=50")
	assert.Contains(t, listOut, "Next cursor: page-2-token")
	assert.Contains(t, listOut, "--cursor page-2-token")

	// 2. Ambiguous prefix in `scion trigger event get abcd` stops immediately on page 1 without fetching page 2.
	seenQueries = nil
	triggerEventPayloadOnly = false
	err := runTriggerEventGet(triggerEventGetCmd, []string{"abcd"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous trigger event ID prefix")
	require.Len(t, seenQueries, 1, "prefix resolution must stop as soon as a second match is seen")
}

func TestTriggerWebhookCLI_GetAndOneCallCreate(t *testing.T) {
	fixedNow := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))
	prevZone := clitime.Zone()
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(prevZone) })

	origFormat := outputFormat
	origJSON := triggerOutputJSON
	origWhName, origWhDesc := triggerWebhookName, triggerWebhookDescription
	origWhAgent, origWhToSelf, origWhMsg := triggerWebhookAgent, triggerWebhookToSelf, triggerWebhookMessage
	origWhInt, origWhWake := triggerWebhookInterrupt, triggerWebhookWake
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerOutputJSON = origJSON
		triggerWebhookName, triggerWebhookDescription = origWhName, origWhDesc
		triggerWebhookAgent, triggerWebhookToSelf, triggerWebhookMessage = origWhAgent, origWhToSelf, origWhMsg
		triggerWebhookInterrupt, triggerWebhookWake = origWhInt, origWhWake
	})

	const (
		epID      = "aaaaaaaa-2222-4333-8444-555555555555"
		trID      = "cccccccc-2222-4333-8444-555555555555"
		agentUUID = "dddddddd-2222-4333-8444-555555555555"
	)
	lastRecv := fixedNow.Add(-4 * time.Minute)
	var seenCreateReqs []hubclient.CreateWebhookEndpointRequest

	endpointDetail := hubclient.WebhookEndpoint{
		ID:             epID,
		ProjectID:      "proj-1",
		Name:           "github-ci",
		Description:    "Signed CI ingress",
		URL:            "https://hub.example.com/api/v1/hooks/" + epID,
		SignatureMode:  "scion_v1",
		SecretVersion:  2,
		Status:         "active",
		LastReceivedAt: &lastRecv,
		ReceiveCount:   7,
		RejectCount:    1,
		CreatedAt:      fixedNow.Add(-time.Hour),
		UpdatedAt:      fixedNow,
	}

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			var req hubclient.CreateWebhookEndpointRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			seenCreateReqs = append(seenCreateReqs, req)
			w.WriteHeader(http.StatusCreated)
			resp := hubclient.WebhookEndpointCreated{
				WebhookEndpoint: endpointDetail,
				Secret:          "secret-shown-once-xyz",
			}
			resp.Name = req.Name
			if req.DefaultTrigger != nil {
				targetName := req.DefaultTrigger.AgentName
				if req.DefaultTrigger.ToSelf {
					targetName = "authenticated-caller-agent"
				}
				resp.TriggerID = trID
				resp.Trigger = &hubclient.Trigger{
					ID:        trID,
					ProjectID: "proj-1",
					Name:      req.Name,
					Status:    "active",
					Source:    hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
					Match:     hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}, Rules: []hubclient.TriggerMatchRuleSpec{}},
					Subject:   hubclient.TriggerSubjectSpec{Mode: "new"},
					Action: hubclient.TriggerActionSpec{
						Type:      "message",
						AgentID:   req.DefaultTrigger.AgentID,
						AgentName: targetName,
						Message:   req.DefaultTrigger.Message,
						Interrupt: req.DefaultTrigger.Interrupt,
						Wake:      req.DefaultTrigger.Wake,
					},
					CreatedAt: fixedNow,
					UpdatedAt: fixedNow,
				}
			}
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			var list []hubclient.WebhookEndpoint
			if n := r.URL.Query().Get("name"); n == "" || n == endpointDetail.Name {
				if n != "hidden-other" {
					list = []hubclient.WebhookEndpoint{endpointDetail}
				}
			}
			_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{
				Scope:      hubclient.TriggerListScopeCaller,
				Endpoints:  list,
				TotalCount: len(list),
				ServerTime: fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints/"+epID:
			_ = json.NewEncoder(w).Encode(endpointDetail)
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindAgentToken)

	resetCreateFlags := func() {
		outputFormat = ""
		triggerOutputJSON = false
		triggerWebhookName = "github-ci"
		triggerWebhookDescription = "Signed CI ingress"
		triggerWebhookAgent = ""
		triggerWebhookToSelf = false
		triggerWebhookMessage = ""
		triggerWebhookInterrupt = false
		triggerWebhookWake = false
	}

	// 1. Flag validation errors for one-call webhook create.
	t.Run("flag validation", func(t *testing.T) {
		resetCreateFlags()
		triggerWebhookAgent = "lead"
		triggerWebhookToSelf = true
		triggerWebhookMessage = "hello"
		_, err := buildCreateWebhookEndpointRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--agent and --to-self cannot be used together")

		resetCreateFlags()
		triggerWebhookAgent = "lead"
		_, err = buildCreateWebhookEndpointRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--message is required")

		resetCreateFlags()
		triggerWebhookToSelf = true
		_, err = buildCreateWebhookEndpointRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--message is required")

		resetCreateFlags()
		triggerWebhookMessage = "orphan message"
		_, err = buildCreateWebhookEndpointRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--agent or --to-self is required")

		resetCreateFlags()
		triggerWebhookWake = true
		_, err = buildCreateWebhookEndpointRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--agent or --to-self is required")
	})

	// 2. --to-self resolves strictly via Hub identity (toSelf=true), NEVER reading SCION_AGENT_* env vars.
	t.Run("one-call create with --to-self ignores SCION_AGENT env vars", func(t *testing.T) {
		resetCreateFlags()
		t.Setenv("SCION_AGENT_NAME", "spoofed-other-agent")
		t.Setenv("SCION_AGENT_ID", "99999999-8888-4777-8666-555555555555")

		triggerWebhookToSelf = true
		triggerWebhookMessage = "Handle incoming CI hook"
		triggerWebhookInterrupt = true
		triggerWebhookWake = true

		humanOut := captureStdout(t, func() {
			require.NoError(t, runTriggerWebhookCreate(triggerWebhookCreateCmd, nil))
		})
		require.Len(t, seenCreateReqs, 1)
		req := seenCreateReqs[0]
		require.NotNil(t, req.DefaultTrigger)
		assert.True(t, req.DefaultTrigger.ToSelf)
		assert.Empty(t, req.DefaultTrigger.AgentName, "--to-self must not populate agentName from env")
		assert.Empty(t, req.DefaultTrigger.AgentID, "--to-self must not populate agentId from env")
		assert.Equal(t, "Handle incoming CI hook", req.DefaultTrigger.Message)
		assert.True(t, req.DefaultTrigger.Interrupt)
		assert.True(t, req.DefaultTrigger.Wake)

		assert.Contains(t, humanOut, epID)
		assert.Contains(t, humanOut, "secret-shown-once-xyz")
		assert.Contains(t, humanOut, "Trigger:        "+trID)
		assert.Contains(t, humanOut, "authenticated-caller-agent")
		assert.NotContains(t, humanOut, "spoofed-other-agent")
	})

	// 3. One-call create with --agent <uuid> and --json returns both id and triggerId.
	t.Run("one-call create with --agent UUID and --json", func(t *testing.T) {
		resetCreateFlags()
		seenCreateReqs = nil
		triggerOutputJSON = true
		triggerWebhookAgent = agentUUID
		triggerWebhookMessage = "Deploy hook arrived"

		jsonOut := captureStdout(t, func() {
			require.NoError(t, runTriggerWebhookCreate(triggerWebhookCreateCmd, nil))
		})
		require.Len(t, seenCreateReqs, 1)
		require.NotNil(t, seenCreateReqs[0].DefaultTrigger)
		assert.Equal(t, agentUUID, seenCreateReqs[0].DefaultTrigger.AgentID)
		assert.Empty(t, seenCreateReqs[0].DefaultTrigger.AgentName)
		assert.False(t, seenCreateReqs[0].DefaultTrigger.ToSelf)

		var created hubclient.WebhookEndpointCreated
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &created))
		assert.Equal(t, epID, created.ID)
		assert.Equal(t, trID, created.TriggerID)
		assert.Equal(t, "secret-shown-once-xyz", created.Secret)
		require.NotNil(t, created.Trigger)
		assert.Equal(t, agentUUID, created.Trigger.Action.AgentID)
	})

	// 4. `scion trigger webhook get <name|id>` renders endpoint detail without secrets, supports --json, and scopes missing errors in agent mode.
	t.Run("webhook get by name, prefix, UUID, --json, and caller-scoped miss", func(t *testing.T) {
		resetCreateFlags()
		getOut := captureStdout(t, func() {
			require.NoError(t, runTriggerWebhookGet(triggerWebhookGetCmd, []string{"github-ci"}))
		})
		assert.Contains(t, getOut, "Webhook Endpoint: "+epID)
		assert.Contains(t, getOut, "Name:           github-ci")
		assert.Contains(t, getOut, "Description:    Signed CI ingress")
		assert.Contains(t, getOut, "Project:        proj-1")
		assert.Contains(t, getOut, "Status:         active")
		assert.Contains(t, getOut, "Signature Mode: scion_v1")
		assert.Contains(t, getOut, "Secret Version: 2")
		assert.Contains(t, getOut, "Received:       7 (rejected: 1)")
		assert.NotContains(t, getOut, "secret-shown-once-xyz")

		triggerOutputJSON = true
		getJSON := captureStdout(t, func() {
			require.NoError(t, runTriggerWebhookGet(triggerWebhookGetCmd, []string{epID[:8]}))
		})
		var gotEP hubclient.WebhookEndpoint
		require.NoError(t, json.Unmarshal([]byte(getJSON), &gotEP))
		assert.Equal(t, epID, gotEP.ID)
		assert.Equal(t, "github-ci", gotEP.Name)
		assert.Equal(t, 2, gotEP.SecretVersion)

		triggerOutputJSON = false
		err := runTriggerWebhookGet(triggerWebhookGetCmd, []string{"hidden-other"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "owned by this agent")
	})

	// 5. One-call Target Agent and webhook get Name/Description sanitize terminal control and Cf runes (O2).
	t.Run("one-call Target Agent and webhook get Name/Description sanitize terminal controls", func(t *testing.T) {
		resetCreateFlags()
		prevEndpoint := endpointDetail
		t.Cleanup(func() { endpointDetail = prevEndpoint })

		endpointDetail.Name = "ci-\x1b[31mhook\u202e"
		endpointDetail.Description = "line1\n  Secret Version: 99\x1b[32m\u202e"

		triggerWebhookName = "ci-hook"
		triggerWebhookAgent = "target-\x1b[31magent\u202e\n  forged: 1"
		triggerWebhookMessage = "wake up"

		createOut := captureStdout(t, func() {
			require.NoError(t, runTriggerWebhookCreate(triggerWebhookCreateCmd, nil))
		})
		assert.NotContains(t, createOut, "\x1b[")
		assert.NotContains(t, createOut, "\u202e")
		assert.Contains(t, createOut, `Target Agent:   target-\x1b[31magent\u{202e}\x0a  forged: 1`)

		getOut := captureStdout(t, func() {
			require.NoError(t, runTriggerWebhookGet(triggerWebhookGetCmd, []string{epID}))
		})
		assert.NotContains(t, getOut, "\x1b[")
		assert.NotContains(t, getOut, "\u202e")
		assert.NotContains(t, getOut, "\n  Secret Version: 99")
		assert.Contains(t, getOut, `Name:           ci-\x1b[31mhook\u{202e}`)
		assert.Contains(t, getOut, `Description:    line1\x0a  Secret Version: 99\x1b[32m\u{202e}`)
	})
}

func TestTriggerCLI_JSONFlagEverywhere(t *testing.T) {
	fixedNow := time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origJSON := triggerOutputJSON
	origName, origWebhook, origAgent, origMsg := triggerName, triggerWebhookRef, triggerAgent, triggerMessage
	origWhere, origWhereFile, origSubj := triggerWhereRules, triggerWhereFile, triggerSubjectExpr
	origWhName := triggerWebhookName
	origEvPayload := triggerEventPayloadOnly
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerOutputJSON = origJSON
		triggerName, triggerWebhookRef, triggerAgent, triggerMessage = origName, origWebhook, origAgent, origMsg
		triggerWhereRules, triggerWhereFile, triggerSubjectExpr = origWhere, origWhereFile, origSubj
		triggerWebhookName = origWhName
		triggerEventPayloadOnly = origEvPayload
	})

	const (
		epID = "aaaaaaaa-3333-4333-8444-555555555555"
		trID = "bbbbbbbb-3333-4333-8444-555555555555"
		evID = "cccccccc-3333-4333-8444-555555555555"
	)

	trObj := hubclient.Trigger{
		ID:        trID,
		ProjectID: "proj-1",
		Name:      "t1",
		Status:    "active",
		Source:    hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
		Match:     hubclient.TriggerMatchSpec{Types: []string{"webhook.received"}, Rules: []hubclient.TriggerMatchRuleSpec{}},
		Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "lead", Message: "hi"},
		CreatedAt: fixedNow,
		UpdatedAt: fixedNow,
	}
	epObj := hubclient.WebhookEndpoint{
		ID:            epID,
		ProjectID:     "proj-1",
		Name:          "wh1",
		URL:           "https://hub.example.com/api/v1/hooks/" + epID,
		Status:        "active",
		SignatureMode: "scion_v1",
		SecretVersion: 1,
		CreatedAt:     fixedNow,
		UpdatedAt:     fixedNow,
	}
	evSummary := hubclient.TriggerEventSummary{
		ID:         evID,
		ProjectID:  "proj-1",
		Source:     hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
		Type:       "webhook.received",
		Subject:    "webhook:" + epID,
		OccurredAt: fixedNow,
		ReceivedAt: fixedNow,
		Status:     "processed",
		CreatedAt:  fixedNow,
	}

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(trObj)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{Triggers: []hubclient.Trigger{trObj}, TotalCount: 1, ServerTime: fixedNow})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID:
			_ = json.NewEncoder(w).Encode(trObj)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/pause":
			_ = json.NewEncoder(w).Encode(trObj)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/resume":
			_ = json.NewEncoder(w).Encode(trObj)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/history":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerFiresResponse{Fires: []hubclient.TriggerFire{}, TotalCount: 0, ServerTime: fixedNow})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerEventsResponse{Events: []hubclient.TriggerEventSummary{evSummary}, TotalCount: 1, ServerTime: fixedNow})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events/"+evID:
			_ = json.NewEncoder(w).Encode(hubclient.TriggerEventDetail{TriggerEventSummary: evSummary, Payload: json.RawMessage(`{"ok":true}`)})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.WebhookEndpointCreated{WebhookEndpoint: epObj, Secret: "sec-1"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints":
			_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{Endpoints: []hubclient.WebhookEndpoint{epObj}, TotalCount: 1, ServerTime: fixedNow})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints/"+epID:
			_ = json.NewEncoder(w).Encode(epObj)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/webhook-endpoints/"+epID:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// Every leaf subcommand under `scion trigger` must expose `--json` via inherited/local flags
	// and emit valid JSON when `triggerOutputJSON = true` (even when `outputFormat == ""`).
	outputFormat = ""
	triggerOutputJSON = true
	triggerName = "t1"
	triggerWebhookRef = epID
	triggerAgent = "lead"
	triggerMessage = "hi"
	triggerWhereRules = nil
	triggerWhereFile = ""
	triggerSubjectExpr = ""
	triggerWebhookName = "wh1"

	cases := []struct {
		name string
		cmd  *cobra.Command
		run  func() error
	}{
		{"trigger create", triggerCreateCmd, func() error { return runTriggerCreate(triggerCreateCmd, nil) }},
		{"trigger get", triggerGetCmd, func() error { return runTriggerGet(triggerGetCmd, []string{trID}) }},
		{"trigger list", triggerListCmd, func() error { return runTriggerList(triggerListCmd, nil) }},
		{"trigger delete", triggerDeleteCmd, func() error { return runTriggerDelete(triggerDeleteCmd, []string{trID}) }},
		{"trigger pause", triggerPauseCmd, func() error { return runTriggerPause(triggerPauseCmd, []string{trID}) }},
		{"trigger resume", triggerResumeCmd, func() error { return runTriggerResume(triggerResumeCmd, []string{trID}) }},
		{"trigger history", triggerHistoryCmd, func() error { return runTriggerHistory(triggerHistoryCmd, []string{trID}) }},
		{"trigger event list", triggerEventListCmd, func() error { return runTriggerEventList(triggerEventListCmd, nil) }},
		{"trigger event get", triggerEventGetCmd, func() error {
			triggerEventPayloadOnly = false
			return runTriggerEventGet(triggerEventGetCmd, []string{evID})
		}},
		{"trigger event get --payload", triggerEventGetCmd, func() error {
			triggerEventPayloadOnly = true
			return runTriggerEventGet(triggerEventGetCmd, []string{evID})
		}},
		{"trigger webhook create", triggerWebhookCreateCmd, func() error { return runTriggerWebhookCreate(triggerWebhookCreateCmd, nil) }},
		{"trigger webhook get", triggerWebhookGetCmd, func() error { return runTriggerWebhookGet(triggerWebhookGetCmd, []string{epID}) }},
		{"trigger webhook list", triggerWebhookListCmd, func() error { return runTriggerWebhookList(triggerWebhookListCmd, nil) }},
		{"trigger webhook delete", triggerWebhookDeleteCmd, func() error { return runTriggerWebhookDelete(triggerWebhookDeleteCmd, []string{epID}) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.cmd.InheritedFlags().Lookup("json")
			if f == nil {
				f = tc.cmd.Flags().Lookup("json")
			}
			require.NotNil(t, f, "%s must accept --json", tc.name)

			out := captureStdout(t, func() {
				require.NoError(t, tc.run())
			})
			var decoded any
			require.NoError(t, json.Unmarshal([]byte(out), &decoded), "%s --json must emit valid JSON, got:\n%s", tc.name, out)
		})
	}
}

func TestTriggerEventAndHistoryCLI_SanitizesTerminalControlAndBidiCharacters(t *testing.T) {
	fixedNow := time.Date(2026, 11, 19, 14, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))
	prevZone := clitime.Zone()
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(prevZone) })

	origIsTTY := textOutputIsTerminal
	textOutputIsTerminal = func() bool { return false }
	t.Cleanup(func() { textOutputIsTerminal = origIsTTY })

	origFormat := outputFormat
	origJSON := triggerOutputJSON
	origEvPayload := triggerEventPayloadOnly
	origEvSource, origEvWebhook, origEvStatus, origEvSince := triggerEventListSource, triggerEventListWebhook, triggerEventListStatus, triggerEventListSince
	origEvLimit, origEvCursor := triggerEventListLimit, triggerEventListCursor
	origHistLimit, origHistCursor := triggerHistoryLimit, triggerHistoryCursor
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerOutputJSON = origJSON
		triggerEventPayloadOnly = origEvPayload
		triggerEventListSource, triggerEventListWebhook, triggerEventListStatus, triggerEventListSince = origEvSource, origEvWebhook, origEvStatus, origEvSince
		triggerEventListLimit, triggerEventListCursor = origEvLimit, origEvCursor
		triggerHistoryLimit, triggerHistoryCursor = origHistLimit, origHistCursor
	})

	const (
		trID = "11111111-4444-4333-8444-555555555555"
		evID = "22222222-4444-4333-8444-555555555555"
	)

	// Hostile strings containing C0 ANSI/OSC escapes, CR, LF line-forging, DEL, C1 CSI, and Unicode Cf bidi overrides.
	hostileSubject := "webhook:ep/\x1b[31mPWN\x1b[0m\rFAKE\u202egnp.exe\u2069"
	hostileAttrVal := "title-\x1b]52;c;aGVsbG8=\x07-\u009b31m-\u202ebidi\n  forged_attr: evil\x7f"
	hostileAttrKey := "key\x1b[2J\u202e"
	hostileListVal := "label-\x1b[32m-\u202e-override"
	hostileHeaderVal := "hdr-\x1b[33m-\u2066isolate\r\n  x-forged: 1"
	hostileError := "dispatch failed: \x1b[31mboom\u202e\rspoof"
	hostileCursor := "tok-\x1b[35m-\u202e"

	payloadMap := map[string]any{
		"title": "PR \x1b[31m#99\x1b[0m \u202eevil-bidi\u2069",
		"body":  "line1\nline2\t\x7f\u009b31mC1\u2028sep",
	}
	payloadBytes, err := json.Marshal(payloadMap)
	require.NoError(t, err)

	evSummary := hubclient.TriggerEventSummary{
		ID:        evID,
		ProjectID: "proj-1",
		Source:    hubclient.TriggerSourceSpec{Kind: "webhook", ID: "ep-\x1b[31m1"},
		Type:      "webhook.\x1b[31mreceived\u202e",
		Subject:   hostileSubject,
		DedupKey:  "dedup-\x1b[31m-\u202e",
		Attributes: map[string]string{
			"clean":        "ok",
			hostileAttrKey: hostileAttrVal,
		},
		Lists: map[string][]string{
			"labels": {"clean-label", hostileListVal},
		},
		OccurredAt:   fixedNow,
		ReceivedAt:   fixedNow,
		PayloadBytes: len(payloadBytes),
		Status:       "failed",
		Error:        hostileError,
		CreatedAt:    fixedNow,
	}

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerEventsResponse{
				Events:     []hubclient.TriggerEventSummary{evSummary},
				NextCursor: hostileCursor,
				TotalCount: 2,
				ServerTime: fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events/"+evID:
			_ = json.NewEncoder(w).Encode(hubclient.TriggerEventDetail{
				TriggerEventSummary: evSummary,
				Headers:             map[string]string{"x-scion-subject": hostileHeaderVal},
				Payload:             json.RawMessage(payloadBytes),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/history":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerFiresResponse{
				Fires: []hubclient.TriggerFire{{
					ID:        "44444444-4444-4333-8444-555555555555",
					TriggerID: trID,
					EventID:   evID,
					ProjectID: "proj-1",
					Subject:   hostileSubject,
					Status:    "failed",
					Error:     hostileError,
					StartedAt: fixedNow,
				}},
				NextCursor: hostileCursor,
				TotalCount: 2,
				ServerTime: fixedNow,
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	assertTerminalSafe := func(t *testing.T, label, out string) {
		t.Helper()
		require.Truef(t, utf8.ValidString(out), "%s contains invalid UTF-8:\n%q", label, out)
		for i, r := range out {
			if r == '\n' || r == '\t' {
				continue
			}
			require.Falsef(t, r < 0x20 || r == 0x7f, "%s contains raw C0/DEL control U+%04X at byte %d:\n%q", label, r, i, out)
			require.Falsef(t, r >= 0x80 && r <= 0x9f, "%s contains raw C1 control U+%04X at byte %d:\n%q", label, r, i, out)
			require.Falsef(t, unicode.Is(unicode.Cf, r), "%s contains raw Unicode Cf format/bidi rune U+%04X at byte %d:\n%q", label, r, i, out)
			require.Falsef(t, r == '\u2028' || r == '\u2029', "%s contains raw line/paragraph separator U+%04X at byte %d:\n%q", label, r, i, out)
		}
		for _, line := range strings.Split(out, "\n") {
			assert.False(t, strings.HasPrefix(line, "  forged_attr:"), "%s allowed newline spoofing:\n%s", label, out)
			assert.False(t, strings.HasPrefix(line, "  x-forged:"), "%s allowed header newline spoofing:\n%s", label, out)
		}
	}

	// 1. `scion trigger event get <id>` (human mode) escapes ANSI, CR/LF, DEL, C1, and Cf bidi overrides in subject, attributes, lists, headers, error, and payload.
	outputFormat = ""
	triggerOutputJSON = false
	triggerEventPayloadOnly = false
	evGetOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventGet(triggerEventGetCmd, []string{evID}))
	})
	assertTerminalSafe(t, "event get", evGetOut)
	assert.Contains(t, evGetOut, `\x1b[31m`)
	assert.Contains(t, evGetOut, `\u{202e}`)
	assert.Contains(t, evGetOut, `\u202e`)

	// 2. `scion trigger event get <id> --payload` emits terminal-safe valid JSON whose unmarshaled values round-trip cleanly.
	triggerEventPayloadOnly = true
	payloadOnlyOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventGet(triggerEventGetCmd, []string{evID}))
	})
	assertTerminalSafe(t, "event get --payload", payloadOnlyOut)
	var decodedPayload map[string]any
	require.NoError(t, json.Unmarshal([]byte(payloadOnlyOut), &decodedPayload))
	assert.Equal(t, payloadMap["title"], decodedPayload["title"])
	assert.Equal(t, payloadMap["body"], decodedPayload["body"])

	// Also unit-test formatTriggerEventPayload directly on malformed non-JSON bytes with multi-line \n/\t + ANSI + bidi override.
	rawFallback := formatTriggerEventPayload(json.RawMessage("not-json:\n\t\x1b[31mRED\u202e\rFAKE"), "  ")
	assertTerminalSafe(t, "formatTriggerEventPayload fallback", rawFallback)
	assert.Contains(t, rawFallback, "\n  \t")
	assert.Contains(t, rawFallback, `\x1b[31m`)
	assert.Contains(t, rawFallback, `\u{202e}`)

	// 3. `scion trigger event list` (human mode) escapes hostile subject, source, type, and next-cursor.
	triggerEventPayloadOnly = false
	triggerEventListSource, triggerEventListWebhook, triggerEventListStatus, triggerEventListSince = "", "", "", ""
	triggerEventListLimit = defaultTriggerEventListLimit
	triggerEventListCursor = ""
	evListOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventList(triggerEventListCmd, nil))
	})
	assertTerminalSafe(t, "event list", evListOut)
	assert.Contains(t, evListOut, `\x1b[31m`)
	assert.Contains(t, evListOut, `\u{202e}`)

	// 4. `scion trigger history <id>` (human mode) escapes hostile subject, error, and next-cursor.
	triggerHistoryLimit = 0
	triggerHistoryCursor = ""
	histOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{trID}))
	})
	assertTerminalSafe(t, "trigger history", histOut)
	assert.Contains(t, histOut, `\x1b[31m`)
	assert.Contains(t, histOut, `\u{202e}`)

	// 5. `--json` output on event get, event list, and trigger history ALSO escapes DEL, C1, Cf, and U+2028/U+2029 as \uXXXX, normalises invalid UTF-8, and round-trips original strings via json.Unmarshal.
	triggerOutputJSON = true

	jsonGetOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventGet(triggerEventGetCmd, []string{evID}))
	})
	assertTerminalSafe(t, "event get --json", jsonGetOut)
	assert.Contains(t, jsonGetOut, `\u007f`)
	assert.Contains(t, jsonGetOut, `\u009b`)
	assert.Contains(t, jsonGetOut, `\u202e`)
	assert.Contains(t, jsonGetOut, `\u2028`)
	var gotDetail hubclient.TriggerEventDetail
	require.NoError(t, json.Unmarshal([]byte(jsonGetOut), &gotDetail))
	assert.Equal(t, hostileSubject, gotDetail.Subject)
	assert.Equal(t, hostileAttrVal, gotDetail.Attributes[hostileAttrKey])
	assert.Equal(t, []string{"clean-label", hostileListVal}, gotDetail.Lists["labels"])

	jsonListOut := captureStdout(t, func() {
		require.NoError(t, runTriggerEventList(triggerEventListCmd, nil))
	})
	assertTerminalSafe(t, "event list --json", jsonListOut)
	assert.Contains(t, jsonListOut, `\u202e`)
	var gotList hubclient.ListTriggerEventsResponse
	require.NoError(t, json.Unmarshal([]byte(jsonListOut), &gotList))
	require.Len(t, gotList.Events, 1)
	assert.Equal(t, hostileSubject, gotList.Events[0].Subject)
	assert.Equal(t, hostileCursor, gotList.NextCursor)

	jsonHistOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{trID}))
	})
	assertTerminalSafe(t, "trigger history --json", jsonHistOut)
	assert.Contains(t, jsonHistOut, `\u202e`)
	var gotHist hubclient.ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal([]byte(jsonHistOut), &gotHist))
	require.Len(t, gotHist.Fires, 1)
	assert.Equal(t, hostileSubject, gotHist.Fires[0].Subject)
	assert.Equal(t, hostileError, gotHist.Fires[0].Error)
	assert.Equal(t, hostileCursor, gotHist.NextCursor)

	// 6. Invalid UTF-8 bytes and raw UTF-8 U+2028/U+2029/supplementary Cf inside json.RawMessage are normalised and \uXXXX-escaped (O1).
	rawInvalidUTF8AndSeparators := json.RawMessage("{\"bad\":\"\xff\",\"sep\":\"\xe2\x80\xa8\xe2\x80\xa9\",\"tag\":\"\U000e0001\"}")
	indentedInvalid := formatTriggerEventPayload(rawInvalidUTF8AndSeparators, "")
	assertTerminalSafe(t, "formatTriggerEventPayload invalid UTF-8 + U+2028/U+2029 + supplementary Cf", indentedInvalid)
	assert.Contains(t, indentedInvalid, `\ufffd`)
	assert.Contains(t, indentedInvalid, `\u2028`)
	assert.Contains(t, indentedInvalid, `\u2029`)
	assert.Contains(t, indentedInvalid, `\udb40\udc01`)
	var decodedInvalid map[string]string
	require.NoError(t, json.Unmarshal([]byte(indentedInvalid), &decodedInvalid))
	assert.Equal(t, "\ufffd", decodedInvalid["bad"])
	assert.Equal(t, "\u2028\u2029", decodedInvalid["sep"])
	assert.Equal(t, "\U000e0001", decodedInvalid["tag"])
}

func TestTriggerGetCLI_SanitizesTerminalControlAndBidiCharacters(t *testing.T) {
	fixedNow := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))
	prevZone := clitime.Zone()
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(prevZone) })

	origIsTTY := textOutputIsTerminal
	textOutputIsTerminal = func() bool { return false }
	t.Cleanup(func() { textOutputIsTerminal = origIsTTY })

	origFormat := outputFormat
	origJSON := triggerOutputJSON
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerOutputJSON = origJSON
	})

	const trID = "33333333-4444-4333-8444-555555555555"

	lastFireAt := fixedNow.Add(-5 * time.Minute)
	tr := hubclient.Trigger{
		ID:                    trID,
		ProjectID:             "proj-1",
		Name:                  "trig-\x1b[31mred\u202e",
		Description:           "desc \x1b]52;c;aGVsbG8=\x07 \x1b[32mCSI\u009b31m \u202ebidi\u2069\r\n  Forged: line\x7f",
		Status:                "paused",
		StatusReason:          "reason-\x1b[33m-\u2066iso\u2069",
		AuthorizationRevision: 2,
		Source:                hubclient.TriggerSourceSpec{Kind: "webhook", ID: "ep-\x1b[34m1\u202e"},
		Match: hubclient.TriggerMatchSpec{
			Types: []string{"webhook.\x1b[35mreceived\u202e"},
		},
		Subject: hubclient.TriggerSubjectSpec{Mode: "single"},
		Action: hubclient.TriggerActionSpec{
			AgentName: "agent-\x1b[36m1\u202e",
			Message:   "line1 \x1b[31mCSI\x1b]0;title\x07 \u009b32mC1 \u202ebidi\u2069\r\nline2\tkeep\u2028sep\x7f",
		},
		FireCount:      3,
		ErrorCount:     1,
		LastFireAt:     &lastFireAt,
		LastFireStatus: "failed\x1b[31m",
		LastFireError:  "err \x1b[31mCSI \x1b]52;c;Zg==\x07 \u009b33mC1 \u202ebidi\u2066iso\u2069\r\n  ForgedErr: 1\x7f",
	}

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID {
			_ = json.NewEncoder(w).Encode(tr)
			return
		}
		http.NotFound(w, r)
	}, hubsync.CredentialKindOAuth)

	outputFormat = ""
	triggerOutputJSON = false
	out := captureStdout(t, func() {
		require.NoError(t, runTriggerGet(triggerGetCmd, []string{trID}))
	})

	require.Truef(t, utf8.ValidString(out), "trigger get contains invalid UTF-8:\n%q", out)
	for i, r := range out {
		if r == '\n' || r == '\t' {
			continue
		}
		require.Falsef(t, r < 0x20 || r == 0x7f, "trigger get contains raw C0/DEL control U+%04X at byte %d:\n%q", r, i, out)
		require.Falsef(t, r >= 0x80 && r <= 0x9f, "trigger get contains raw C1 control U+%04X at byte %d:\n%q", r, i, out)
		require.Falsef(t, unicode.Is(unicode.Cf, r), "trigger get contains raw Unicode Cf format/bidi rune U+%04X at byte %d:\n%q", r, i, out)
		require.Falsef(t, r == '\u2028' || r == '\u2029', "trigger get contains raw line/paragraph separator U+%04X at byte %d:\n%q", r, i, out)
	}
	for _, line := range strings.Split(out, "\n") {
		assert.False(t, strings.HasPrefix(line, "  Forged:"), "description allowed newline spoofing:\n%s", out)
		assert.False(t, strings.HasPrefix(line, "  ForgedErr:"), "lastFireError allowed newline spoofing:\n%s", out)
	}

	// Verify each of Description, Message, and Last Error carries the escaped forms.
	assert.Contains(t, out, `Description: desc \x1b]52;c;aGVsbG8=\x07 \x1b[32mCSI\u{009b}31m \u{202e}bidi\u{2069}\x0d\x0a  Forged: line\x7f`)
	assert.Contains(t, out, "Message:     line1 \\x1b[31mCSI\\x1b]0;title\\x07 \\u{009b}32mC1 \\u{202e}bidi\\u{2069}\\x0d\nline2\tkeep\\u{2028}sep\\x7f")
	assert.Contains(t, out, `Last Error:  err \x1b[31mCSI \x1b]52;c;Zg==\x07 \u{009b}33mC1 \u{202e}bidi\u{2066}iso\u{2069}\x0d\x0a  ForgedErr: 1\x7f`)
	assert.Contains(t, out, `Name:        trig-\x1b[31mred\u{202e}`)
	assert.Contains(t, out, `Source:      webhook:ep-\x1b[34m1\u{202e}`)
	assert.Contains(t, out, `Agent:       agent-\x1b[36m1\u{202e}`)
}

func TestTriggerEventCLI_ResolvePrefixScanBoundAndFullUUIDSkipsList(t *testing.T) {
	origFormat := outputFormat
	origJSON := triggerOutputJSON
	origEvPayload := triggerEventPayloadOnly
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerOutputJSON = origJSON
		triggerEventPayloadOnly = origEvPayload
	})
	outputFormat = ""
	triggerOutputJSON = false
	triggerEventPayloadOnly = false

	const fullUUID = "aaaaaaaa-4444-4333-8444-555555555555"
	fixedNow := time.Date(2026, 11, 20, 11, 0, 0, 0, time.UTC)

	var (
		listCalls       int
		getCalls        int
		includeFirstHit bool
	)
	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events/"+fullUUID:
			getCalls++
			_ = json.NewEncoder(w).Encode(hubclient.TriggerEventDetail{
				TriggerEventSummary: hubclient.TriggerEventSummary{
					ID:         fullUUID,
					ProjectID:  "proj-1",
					Source:     hubclient.TriggerSourceSpec{Kind: "webhook", ID: "ep-1"},
					Type:       "webhook.received",
					Status:     "received",
					OccurredAt: fixedNow,
					ReceivedAt: fixedNow,
					CreatedAt:  fixedNow,
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/trigger-events":
			listCalls++
			events := []hubclient.TriggerEventSummary{
				{ID: fmt.Sprintf("11111111-0000-4000-8000-%012d", listCalls*2-1), Status: "received"},
				{ID: fmt.Sprintf("11111111-0000-4000-8000-%012d", listCalls*2), Status: "received"},
			}
			if includeFirstHit && listCalls == 1 {
				events[0].ID = fullUUID
			}
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerEventsResponse{
				Events:     events,
				NextCursor: fmt.Sprintf("cursor-%d", listCalls),
				TotalCount: 9999,
				ServerTime: fixedNow,
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindOAuth)

	// 1. Full UUID performs zero List calls and fetches directly.
	out := captureStdout(t, func() {
		require.NoError(t, runTriggerEventGet(triggerEventGetCmd, []string{fullUUID}))
	})
	assert.Equal(t, 0, listCalls, "full UUID resolution must not list trigger events")
	assert.Equal(t, 1, getCalls)
	assert.Contains(t, out, fullUUID)

	// 2. Missing prefix stops after triggerEventPrefixScanMaxPages pages with a clear error.
	listCalls = 0
	includeFirstHit = false
	err := runTriggerEventGet(triggerEventGetCmd, []string{"deadbeef"})
	require.Error(t, err)
	assert.Equal(t, triggerEventPrefixScanMaxPages, listCalls)
	assert.Contains(t, err.Error(), fmt.Sprintf("not found in the newest %d events; pass the full event UUID", triggerEventPrefixScanMaxPages*2))

	// 3. Single prefix match with unscanned older pages fails safely rather than risking ambiguity.
	listCalls = 0
	includeFirstHit = true
	err = runTriggerEventGet(triggerEventGetCmd, []string{"aaaaaaaa"})
	require.Error(t, err)
	assert.Equal(t, triggerEventPrefixScanMaxPages, listCalls)
	assert.Contains(t, err.Error(), fullUUID)
	assert.Contains(t, err.Error(), "older events were not scanned; pass the full event UUID")
}

func TestTriggerHistoryCLI_MatchedThroughDecidedDialectAndVerboseTrace(t *testing.T) {
	fixedNow := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origLimit, origCursor, origVerbose := triggerHistoryLimit, triggerHistoryCursor, triggerHistoryVerbose
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerHistoryLimit, triggerHistoryCursor, triggerHistoryVerbose = origLimit, origCursor, origVerbose
	})

	trID := "11111111-2222-4333-8444-555555555555"
	started := fixedNow.Add(-1 * time.Minute)
	finished := fixedNow.Add(-55 * time.Second)
	remMs := int64(992)

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID+"/history" {
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggerFiresResponse{
				Fires: []hubclient.TriggerFire{
					{
						ID:             "aaaaaaaa-1111-4222-8333-444444444444",
						TriggerID:      trID,
						EventID:        "bbbbbbbb-1111-4222-8333-444444444444",
						ProjectID:      "proj-1",
						Subject:        "svc:payments",
						Status:         "fired",
						MatchedThrough: "2/2",
						DecidedDialect: "cel",
						RuleTrace: &hubclient.TriggerRuleTrace{
							Elements: []hubclient.TriggerRuleTraceElement{
								{Index: 1, Dialect: "cel", Class: "matched", DurationMs: 1},
								{Index: 2, Dialect: "cel", Class: "matched", DurationMs: 2},
							},
							BudgetRemainingMs: &remMs,
						},
						StartedAt:  started,
						FinishedAt: &finished,
					},
					{
						ID:             "cccccccc-1111-4222-8333-444444444444",
						TriggerID:      trID,
						EventID:        "dddddddd-1111-4222-8333-444444444444",
						ProjectID:      "proj-1",
						Subject:        "webhook:ep-1",
						Status:         "skipped",
						Error:          "rule uncertain",
						MatchedThrough: "2/3",
						DecidedDialect: "system_one",
						RuleTrace: &hubclient.TriggerRuleTrace{
							Elements: []hubclient.TriggerRuleTraceElement{
								{Index: 1, Dialect: "cel", Class: "matched", DurationMs: 0},
								{Index: 2, Dialect: "system_one", Class: "uncertain", Reason: "abstain", DurationMs: 14, Provenance: "verdict=abstain\n\x1b[31mspoof"},
							},
							Unexecuted:        []int{3},
							BudgetRemainingMs: &remMs,
						},
						StartedAt:  started,
						FinishedAt: &finished,
					},
				},
				TotalCount: 2,
				ServerTime: fixedNow,
			})
			return
		}
		http.NotFound(w, r)
	}, hubsync.CredentialKindOAuth)

	// 1. Default table shows MATCHED and DIALECT columns, hiding rule_trace behind --verbose.
	outputFormat = ""
	triggerHistoryLimit = 0
	triggerHistoryCursor = ""
	triggerHistoryVerbose = false
	defaultOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{trID}))
	})
	assert.Contains(t, defaultOut, "MATCHED")
	assert.Contains(t, defaultOut, "DIALECT")
	assert.Contains(t, defaultOut, "2/2")
	assert.Contains(t, defaultOut, "2/3")
	assert.Contains(t, defaultOut, "cel")
	assert.Contains(t, defaultOut, "system_one")
	assert.NotContains(t, defaultOut, "unexecuted", "rule_trace must stay behind --verbose")

	// 2. --verbose renders per-element trace, unexecuted indices, and sanitizes control bytes.
	triggerHistoryVerbose = true
	verboseOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{trID}))
	})
	assert.Contains(t, verboseOut, "1 [cel] matched (1ms)")
	assert.Contains(t, verboseOut, "2 [system_one] uncertain (14ms) reason=abstain")
	assert.Contains(t, verboseOut, "unexecuted: [3]")
	assert.NotContains(t, verboseOut, "\x1b[", "control characters in provenance must never reach terminal raw")

	// 3. --format json includes matchedThrough, decidedDialect, and ruleTrace.
	outputFormat = "json"
	jsonOut := captureStdout(t, func() {
		require.NoError(t, runTriggerHistory(triggerHistoryCmd, []string{trID}))
	})
	var parsed hubclient.ListTriggerFiresResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
	require.Len(t, parsed.Fires, 2)
	assert.Equal(t, "2/2", parsed.Fires[0].MatchedThrough)
	assert.Equal(t, "cel", parsed.Fires[0].DecidedDialect)
	require.NotNil(t, parsed.Fires[0].RuleTrace)
	assert.Len(t, parsed.Fires[0].RuleTrace.Elements, 2)
	assert.Equal(t, []int{3}, parsed.Fires[1].RuleTrace.Unexecuted)

func TestTriggerCLI_GitHubCreateGetAndList(t *testing.T) {
	fixedNow := time.Date(2026, 11, 18, 12, 10, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))
	prevZone := clitime.Zone()
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(prevZone) })

	origFormat := outputFormat
	origName, origWebhook, origGitHub := triggerName, triggerWebhookRef, triggerGitHubSource
	origTypes, origWhere, origWhereFile := triggerMatchTypes, triggerWhereRules, triggerWhereFile
	origAgent, origMsg := triggerAgent, triggerMessage
	origListSource, origListWebhook, origListStatus := triggerListSource, triggerListWebhook, triggerListStatus
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerName, triggerWebhookRef, triggerGitHubSource = origName, origWebhook, origGitHub
		triggerMatchTypes, triggerWhereRules, triggerWhereFile = origTypes, origWhere, origWhereFile
		triggerAgent, triggerMessage = origAgent, origMsg
		triggerListSource, triggerListWebhook, triggerListStatus = origListSource, origListWebhook, origListStatus
	})

	trID := "dddddddd-1111-4222-8333-444444444444"
	var seenCreateReq hubclient.CreateTriggerRequest

	withMockTriggerHub(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&seenCreateReq))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:        trID,
				ProjectID: "proj-1",
				Name:      seenCreateReq.Name,
				Status:    "active",
				Source:    seenCreateReq.Source,
				Match:     *seenCreateReq.Match,
				Action:    seenCreateReq.Action,
				CreatedAt: fixedNow,
				UpdatedAt: fixedNow,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers":
			_ = json.NewEncoder(w).Encode(hubclient.ListTriggersResponse{
				Scope: hubclient.TriggerListScopeCaller,
				Triggers: []hubclient.Trigger{{
					ID:        trID,
					ProjectID: "proj-1",
					Name:      "gh-ping",
					Status:    "active",
					Source:    hubclient.TriggerSourceSpec{Kind: "github", ID: ""},
					Match:     hubclient.TriggerMatchSpec{Types: []string{"github.issue_comment.created"}},
					Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "ping-target", Message: "hi"},
					CreatedAt: fixedNow,
					UpdatedAt: fixedNow,
				}},
				TotalCount: 1,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/triggers/"+trID:
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:                    trID,
				ProjectID:             "proj-1",
				Name:                  "gh-ping",
				Status:                "active",
				AuthorizationRevision: 1,
				Source:                hubclient.TriggerSourceSpec{Kind: "github", ID: ""},
				Match: hubclient.TriggerMatchSpec{
					Types: []string{"github.issue_comment.created"},
					Rules: []hubclient.TriggerMatchRuleSpec{
						{Dialect: "cel", Expr: "event.attributes.command == 'ping'"},
					},
				},
				Action:    hubclient.TriggerActionSpec{Type: "message", AgentName: "ping-target", Message: "hi"},
				CreatedAt: fixedNow,
				UpdatedAt: fixedNow,
			})
		default:
			http.NotFound(w, r)
		}
	}, hubsync.CredentialKindAgentToken)

	// 1. Create GitHub trigger (--github --type ... --where ...).
	outputFormat = ""
	triggerName = "gh-ping"
	triggerWebhookRef = ""
	triggerGitHubSource = true
	triggerMatchTypes = []string{"github.issue_comment.created"}
	triggerWhereFile = ""
	triggerWhereRules = []string{"event.attributes.command == 'ping'"}
	triggerAgent = "ping-target"
	triggerMessage = "Received GitHub comment trigger event"

	createOut := captureStdout(t, func() {
		require.NoError(t, runTriggerCreate(triggerCreateCmd, nil))
	})
	assert.Equal(t, hubclient.TriggerSourceSpec{Kind: "github", ID: ""}, seenCreateReq.Source)
	require.NotNil(t, seenCreateReq.Match)
	assert.Equal(t, []hubclient.TriggerMatchRuleSpec{{Dialect: "cel", Expr: "event.attributes.command == 'ping'"}}, seenCreateReq.Match.Rules)
	assert.Contains(t, createOut, "Source:  github\n")
	assert.Contains(t, createOut, "github.issue_comment.created")
	assert.Contains(t, createOut, `1/1 [cel] "event.attributes.command == 'ping'"`)

	// 2. Get GitHub trigger renders source "github" without a trailing colon and shows numbered rules.
	trGetOut := captureStdout(t, func() {
		require.NoError(t, runTriggerGet(triggerGetCmd, []string{trID}))
	})
	assert.Contains(t, trGetOut, "Source:      github\n")
	assert.NotContains(t, trGetOut, "github:")
	assert.Contains(t, trGetOut, `1/1 [cel] "event.attributes.command == 'ping'"`)

	// 3. List GitHub triggers renders source "github" without a trailing colon.
	triggerListSource = "github"
	triggerListWebhook = ""
	triggerListStatus = ""
	listOut := captureStdout(t, func() {
		require.NoError(t, runTriggerList(triggerListCmd, nil))
	})
	assert.Contains(t, listOut, "gh-ping")
	assert.NotContains(t, listOut, "github:")
}
