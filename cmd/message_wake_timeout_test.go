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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSendMessage_SlowWakeExceedsDefaultClientTimeout verifies that a direct
// message send to an agent whose wake takes longer than the hub client's
// default 30s timeout (shortened here to 150ms) does not time out prematurely
// and receives the hub's accepted response (ptone/scion#9757).
func TestSendMessage_SlowWakeExceedsDefaultClientTimeout(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "sender")
	msgAttach = nil
	msgAttachUpload = nil

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(hubclient.MessageResponse{
			Status:    "delivered",
			MessageID: "msg-slow-wake-1",
		})
	}))
	defer srv.Close()

	// Default client timeout is shorter than the simulated wake delay.
	client, err := hubclient.New(srv.URL, hubclient.WithTimeout(150*time.Millisecond))
	require.NoError(t, err)

	hubCtx := &HubContext{
		Client:    client,
		Endpoint:  srv.URL,
		ProjectID: "proj-wake",
	}

	var sendErr error
	stdout, _ := captureStdoutStderr(t, func() {
		sendErr = sendMessageViaHub(hubCtx, "builder", "wake up and run", false, true)
	})
	require.NoError(t, sendErr)
	assert.Contains(t, stdout, "Message accepted")
}

// TestSendMessage_TimeoutErrorStatesMessageNotDelivered verifies that when a
// message send does time out on the client, the returned error clearly states
// that the message was not delivered (ptone/scion#9757).
func TestSendMessage_TimeoutErrorStatesMessageNotDelivered(t *testing.T) {
	t.Run("agentMessageSendError_deadline_exceeded", func(t *testing.T) {
		err := agentMessageSendError("builder", context.DeadlineExceeded)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Contains(t, err.Error(), "message was not delivered")
		assert.True(t, errors.Is(err, context.DeadlineExceeded))
	})

	t.Run("agentMessageSendError_api_error_unchanged", func(t *testing.T) {
		apiErr := &apiclient.APIError{
			StatusCode: http.StatusGatewayTimeout,
			Code:       "gateway_timeout",
			Message:    "broker unreachable",
		}
		err := agentMessageSendError("builder", apiErr)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "timed out sending message")
	})

	t.Run("sendMessageViaHub_real_timeout", func(t *testing.T) {
		t.Setenv("SCION_AGENT_NAME", "sender")
		msgAttach = nil
		msgAttachUpload = nil

		origWake := msgWakeSendTimeout
		msgWakeSendTimeout = 100 * time.Millisecond
		t.Cleanup(func() { msgWakeSendTimeout = origWake })

		unblock := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-unblock:
			case <-r.Context().Done():
			}
		}))
		defer func() {
			close(unblock)
			srv.Close()
		}()

		client, err := hubclient.New(srv.URL)
		require.NoError(t, err)
		hubCtx := &HubContext{
			Client:    client,
			Endpoint:  srv.URL,
			ProjectID: "proj-wake",
		}

		var sendErr error
		_, _ = captureStdoutStderr(t, func() {
			sendErr = sendMessageViaHub(hubCtx, "builder", "hello", false, true)
		})
		require.Error(t, sendErr)
		assert.Contains(t, sendErr.Error(), "timed out")
		assert.Contains(t, sendErr.Error(), "message was not delivered")
	})
}

// TestMsgWakeSendTimeout_ExceedsHubDMWakeBudget verifies that the CLI's
// wake-capable send timeout exceeds the hub's DM wake write budget
// (syncDispatchTimeout 180s + wakeReadyTimeout 30s + syncDispatchWriteSlack 30s = 240s).
func TestMsgWakeSendTimeout_ExceedsHubDMWakeBudget(t *testing.T) {
	const hubDMWakeBudget = 240 * time.Second
	assert.Greater(t, msgWakeSendTimeout, hubDMWakeBudget,
		"msgWakeSendTimeout must outlast the hub's DM wake write budget (240s)")
}
