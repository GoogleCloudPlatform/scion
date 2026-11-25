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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerWebhookP3_CreateBindUpdateDisableRotate(t *testing.T) {
	const (
		projID = "11111111-1111-4111-8111-111111111111"
		epID   = "22222222-2222-4222-8222-222222222222"
		agID   = "33333333-3333-4333-8333-333333333333"
	)

	// Save & restore package-level flags.
	origName, origDesc := triggerWebhookName, triggerWebhookDescription
	origAgent, origToSelf := triggerWebhookAgent, triggerWebhookToSelf
	origBind, origPersistent := triggerWebhookBind, triggerWebhookPersistent
	origMsg, origInt, origWake := triggerWebhookMessage, triggerWebhookInterrupt, triggerWebhookWake
	origListBound := triggerWebhookListBound
	origUpdDesc, origUpdStatus := triggerWebhookUpdateDescription, triggerWebhookUpdateStatus
	origUpdBind, origUpdUnbind := triggerWebhookUpdateBind, triggerWebhookUpdateUnbind
	origUpdPers, origUpdAgLife := triggerWebhookUpdatePersistent, triggerWebhookUpdateAgentLife
	origHubFn := triggerHubContextFn
	t.Cleanup(func() {
		triggerWebhookName, triggerWebhookDescription = origName, origDesc
		triggerWebhookAgent, triggerWebhookToSelf = origAgent, origToSelf
		triggerWebhookBind, triggerWebhookPersistent = origBind, origPersistent
		triggerWebhookMessage, triggerWebhookInterrupt, triggerWebhookWake = origMsg, origInt, origWake
		triggerWebhookListBound = origListBound
		triggerWebhookUpdateDescription, triggerWebhookUpdateStatus = origUpdDesc, origUpdStatus
		triggerWebhookUpdateBind, triggerWebhookUpdateUnbind = origUpdBind, origUpdUnbind
		triggerWebhookUpdatePersistent, triggerWebhookUpdateAgentLife = origUpdPers, origUpdAgLife
		triggerHubContextFn = origHubFn
	})

	resetCreate := func() {
		triggerWebhookName = "ci-hook"
		triggerWebhookDescription = ""
		triggerWebhookAgent = ""
		triggerWebhookToSelf = false
		triggerWebhookBind = ""
		triggerWebhookPersistent = false
		triggerWebhookMessage = ""
		triggerWebhookInterrupt = false
		triggerWebhookWake = false
	}

	// 1. Flag validation for --bind and --persistent.
	t.Run("bind and persistent flag validation", func(t *testing.T) {
		resetCreate()
		triggerWebhookPersistent = true
		_, err := buildCreateWebhookEndpointRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--persistent requires --to-self or --bind")

		resetCreate()
		triggerWebhookBind = "worker-1"
		triggerWebhookToSelf = true
		_, err = buildCreateWebhookEndpointRequest()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--bind and --to-self cannot be used together")

		resetCreate()
		triggerWebhookToSelf = true
		triggerWebhookPersistent = true
		req, err := buildCreateWebhookEndpointRequest()
		require.NoError(t, err)
		assert.Equal(t, "self", req.BoundAgentID)
		assert.Equal(t, "persistent", req.BoundLifetime)
		require.NotNil(t, req.DefaultTrigger)
		assert.Equal(t, defaultToSelfWebhookMessage, req.DefaultTrigger.Message)

		resetCreate()
		triggerWebhookBind = "worker-1"
		req, err = buildCreateWebhookEndpointRequest()
		require.NoError(t, err)
		assert.Equal(t, "worker-1", req.BoundAgentID)
		assert.Equal(t, "agent", req.BoundLifetime)
		assert.Nil(t, req.DefaultTrigger)
	})

	// 2. End-to-end HTTP mock for update, disable, enable, rotate-secret, and list --bound.
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	var (
		lastPatchReq   *hubclient.UpdateWebhookEndpointRequest
		rotatedCalled  bool
		lastListBoundQ string
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projID+"/webhook-endpoints":
			lastListBoundQ = r.URL.Query().Get("boundAgentId")
			_ = json.NewEncoder(w).Encode(hubclient.ListWebhookEndpointsResponse{
				Endpoints: []hubclient.WebhookEndpoint{{
					ID:            epID,
					ProjectID:     projID,
					Name:          "ci-hook",
					URL:           "https://hub.example.com/api/v1/hooks/" + epID,
					SignatureMode: "scion_v1",
					SecretVersion: 1,
					Status:        "active",
					BoundAgent:    &hubclient.WebhookEndpointBoundAgent{ID: agID, Name: "worker-1"},
					BoundLifetime: "agent",
					CreatedAt:     now,
					UpdatedAt:     now,
				}},
				TotalCount: 1,
				ServerTime: now,
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/projects/"+projID+"/webhook-endpoints/"+epID:
			var req hubclient.UpdateWebhookEndpointRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			lastPatchReq = &req
			st := "active"
			if req.Status != nil {
				st = *req.Status
			}
			_ = json.NewEncoder(w).Encode(hubclient.WebhookEndpoint{
				ID:            epID,
				ProjectID:     projID,
				Name:          "ci-hook",
				URL:           "https://hub.example.com/api/v1/hooks/" + epID,
				SignatureMode: "scion_v1",
				SecretVersion: 1,
				Status:        st,
				CreatedAt:     now,
				UpdatedAt:     now,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projID+"/webhook-endpoints/"+epID+"/rotate-secret":
			rotatedCalled = true
			_ = json.NewEncoder(w).Encode(hubclient.WebhookEndpointRotated{
				WebhookEndpoint: hubclient.WebhookEndpoint{
					ID:              epID,
					ProjectID:       projID,
					Name:            "ci-hook",
					URL:             "https://hub.example.com/api/v1/hooks/" + epID,
					SignatureMode:   "scion_v1",
					SecretVersion:   2,
					SecretRotatedAt: &now,
					Status:          "active",
					CreatedAt:       now,
					UpdatedAt:       now,
				},
				Secret:             "new-rotated-secret-xyz",
				GraceWindowExpires: now.Add(24 * time.Hour),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer ts.Close()

	client, err := hubclient.New(ts.URL)
	require.NoError(t, err)
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client}, projID, ctx, cancel, nil
	}

	// list --bound
	triggerWebhookListBound = "worker-1"
	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookList(triggerWebhookListCmd, nil))
	})
	assert.Equal(t, "worker-1", lastListBoundQ)

	// disable
	lastPatchReq = nil
	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookDisable(triggerWebhookDisableCmd, []string{epID}))
	})
	require.NotNil(t, lastPatchReq)
	require.NotNil(t, lastPatchReq.Status)
	assert.Equal(t, "disabled", *lastPatchReq.Status)

	// enable
	lastPatchReq = nil
	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookEnable(triggerWebhookEnableCmd, []string{epID}))
	})
	require.NotNil(t, lastPatchReq)
	require.NotNil(t, lastPatchReq.Status)
	assert.Equal(t, "active", *lastPatchReq.Status)

	// update --bind without lifetime flag -> local usage error with hint (N-1)
	require.NoError(t, triggerWebhookUpdateCmd.Flags().Set("bind", "worker-1"))
	triggerWebhookUpdatePersistent = false
	triggerWebhookUpdateAgentLife = false
	triggerWebhookUpdateUnbind = false
	err = runTriggerWebhookUpdate(triggerWebhookUpdateCmd, []string{epID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--bind requires --persistent or --agent-lifetime")

	// update --bind with --persistent -> sends PATCH with both fields
	lastPatchReq = nil
	triggerWebhookUpdatePersistent = true
	_ = captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookUpdate(triggerWebhookUpdateCmd, []string{epID}))
	})
	require.NotNil(t, lastPatchReq)
	require.NotNil(t, lastPatchReq.BoundAgentID)
	require.NotNil(t, lastPatchReq.BoundLifetime)
	assert.Equal(t, "worker-1", *lastPatchReq.BoundAgentID)
	assert.Equal(t, "persistent", *lastPatchReq.BoundLifetime)

	// rotate-secret
	out := captureStdout(t, func() {
		require.NoError(t, runTriggerWebhookRotateSecret(triggerWebhookRotateSecretCmd, []string{epID}))
	})
	assert.True(t, rotatedCalled)
	assert.Contains(t, out, "new-rotated-secret-xyz")
	assert.Contains(t, out, "Secret Version: 2")
}
