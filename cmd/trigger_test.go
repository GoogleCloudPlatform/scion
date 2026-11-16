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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubsync"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerCLIModesAndWindowsGate(t *testing.T) {
	paths := []string{
		"trigger",
		"trigger.create",
		"trigger.list",
		"trigger.delete",
		"trigger.pause",
		"trigger.resume",
		"trigger.webhook",
		"trigger.webhook.create",
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
		{"trigger", "create", "--name", "t1", "--webhook", "wh1", "--agent", "lead", "--message", "hi"},
		{"trigger", "delete", "t1"},
		{"trigger", "pause", "t1"},
		{"trigger", "resume", "t1"},
		{"trigger", "webhook", "list"},
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
}

func TestBuildCreateTriggerRequest(t *testing.T) {
	origName, origDesc, origWebhook := triggerName, triggerDescription, triggerWebhookRef
	origTypes, origWhere := triggerMatchTypes, triggerWhereExpr
	origAgent, origMessage := triggerAgent, triggerMessage
	origInt, origWake, origPlain := triggerInterrupt, triggerWake, triggerPlain
	origSubj, origOnSubj := triggerSubjectExpr, triggerOnSubject
	t.Cleanup(func() {
		triggerName, triggerDescription, triggerWebhookRef = origName, origDesc, origWebhook
		triggerMatchTypes, triggerWhereExpr = origTypes, origWhere
		triggerAgent, triggerMessage = origAgent, origMessage
		triggerInterrupt, triggerWake, triggerPlain = origInt, origWake, origPlain
		triggerSubjectExpr, triggerOnSubject = origSubj, origOnSubj
	})

	resetFlags := func() {
		triggerName = "deploy-hook"
		triggerDescription = "Deploy trigger"
		triggerWebhookRef = "deploys"
		triggerMatchTypes = nil
		triggerWhereExpr = ""
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
		assert.Contains(t, err.Error(), "--webhook is required")

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

	t.Run("optional match, subject, and action flags", func(t *testing.T) {
		resetFlags()
		triggerMatchTypes = []string{"webhook.deploy.*", "webhook.release"}
		triggerWhereExpr = `data.env == "prod"`
		triggerSubjectExpr = `data.service`
		triggerOnSubject = "skip"
		triggerInterrupt = true
		triggerWake = true
		triggerPlain = true

		req, err := buildCreateTriggerRequest()
		require.NoError(t, err)
		require.NotNil(t, req.Match)
		assert.Equal(t, []string{"webhook.deploy.*", "webhook.release"}, req.Match.Types)
		assert.Equal(t, `data.env == "prod"`, req.Match.Expr)
		require.NotNil(t, req.Subject)
		assert.Equal(t, `data.service`, req.Subject.Expr)
		assert.Equal(t, "skip", req.Subject.Mode)
		assert.True(t, req.Action.Interrupt)
		assert.True(t, req.Action.Wake)
		assert.True(t, req.Action.Plain)
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
