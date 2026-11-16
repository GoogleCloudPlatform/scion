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
	"strconv"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
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
		"trigger.history",
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
		{"trigger", "history", "t1"},
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
	help := triggerCreateCmd.Long
	assert.Contains(t, help, "--where")
	assert.Contains(t, help, "--subject")
	assert.Contains(t, help, "literal RE2 patterns only")
	assert.Contains(t, help, "event.attributes.<k>")
	assert.Contains(t, help, "event.payload.<field>")
	assert.Contains(t, help, "not on the whole payload")
	assert.Contains(t, help, "64 KiB")
	assert.Contains(t, help, `skipped ("rule evaluation failed")`)

	whereFlag := triggerCreateCmd.Flags().Lookup("where")
	require.NotNil(t, whereFlag)
	assert.Contains(t, whereFlag.Usage, "event.attributes.<k>")
	assert.Contains(t, whereFlag.Usage, "event.payload.<field>")

	subjectFlag := triggerCreateCmd.Flags().Lookup("subject")
	require.NotNil(t, subjectFlag)
	assert.Contains(t, subjectFlag.Usage, "1024B")
}

func TestTriggerCLI_WhereSubjectCreateAndPositionBearingErrors(t *testing.T) {
	fixedNow := time.Date(2026, 11, 16, 6, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return fixedNow }))

	origFormat := outputFormat
	origName, origWebhook, origAgent, origMsg := triggerName, triggerWebhookRef, triggerAgent, triggerMessage
	origWhere, origSubject := triggerWhereExpr, triggerSubjectExpr
	t.Cleanup(func() {
		outputFormat = origFormat
		triggerName, triggerWebhookRef, triggerAgent, triggerMessage = origName, origWebhook, origAgent, origMsg
		triggerWhereExpr, triggerSubjectExpr = origWhere, origSubject
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
			_ = json.NewEncoder(w).Encode(hubclient.Trigger{
				ID:        trID,
				ProjectID: "proj-1",
				Name:      lastReq.Name,
				Status:    "active",
				Source:    hubclient.TriggerSourceSpec{Kind: "webhook", ID: epID},
				Match: hubclient.TriggerMatchSpec{
					Types:   []string{"webhook.received"},
					Dialect: "cel",
					Expr:    lastReq.Match.Expr,
				},
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
	triggerWhereExpr = `has(event.payload) && event.payload.env == "prod"`
	triggerSubjectExpr = `string(event.payload.service)`

	out := captureStdout(t, func() {
		require.NoError(t, runTriggerCreate(triggerCreateCmd, nil))
	})
	require.NotNil(t, lastReq.Match)
	assert.Equal(t, `has(event.payload) && event.payload.env == "prod"`, lastReq.Match.Expr)
	require.NotNil(t, lastReq.Subject)
	assert.Equal(t, `string(event.payload.service)`, lastReq.Subject.Expr)
	assert.Contains(t, out, "Where:   has(event.payload) && event.payload.env == \"prod\"")
	assert.Contains(t, out, "Subject: string(event.payload.service)")

	// Position-bearing compile error formatted from details when message lacks line/column.
	mode = "compile_err_details_only"
	err := runTriggerCreate(triggerCreateCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid_rule")
	assert.Contains(t, err.Error(), "line 2, column 7")
	assert.Contains(t, err.Error(), "unknown_variable")
	assert.Contains(t, err.Error(), "unknown_var")

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
