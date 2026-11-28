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
// message send to an agent whose wake takes longer than both the non-waking
// msgSendTimeout (shortened here to 50ms) and the hub client's default timeout
// (shortened here to 150ms) uses msgWakeSendTimeout with WithRequestTimeout
// and receives the hub's accepted response (ptone/scion#9757).
func TestSendMessage_SlowWakeExceedsDefaultClientTimeout(t *testing.T) {
	t.Setenv("SCION_AGENT_NAME", "sender")
	msgAttach = nil
	msgAttachUpload = nil

	// Shorten msgSendTimeout below the simulated wake delay while leaving
	// msgWakeSendTimeout at its default (4m30s), so falling back to the
	// non-wake send timeout fails this test.
	origSend := msgSendTimeout
	msgSendTimeout = 50 * time.Millisecond
	t.Cleanup(func() { msgSendTimeout = origSend })

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

// TestSendMessage_TimeoutErrorStatesUnknownDelivery verifies that when a
// message send times out on the client, the returned error reports that
// delivery status is unknown and advises checking the conversation before
// resending, without falsely claiming the message was not delivered
// (ptone/scion#9757).
func TestSendMessage_TimeoutErrorStatesUnknownDelivery(t *testing.T) {
	t.Run("agentMessageSendError_deadline_exceeded", func(t *testing.T) {
		err := agentMessageSendError("builder", context.DeadlineExceeded)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Contains(t, err.Error(), "delivery status is unknown")
		assert.Contains(t, err.Error(), "Check the conversation before resending")
		assert.NotContains(t, err.Error(), "was not delivered")
		assert.True(t, errors.Is(err, context.DeadlineExceeded))
	})

	t.Run("outboundMessageSendError_deadline_exceeded", func(t *testing.T) {
		err := outboundMessageSendError("user@example.com", context.DeadlineExceeded)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "timed out")
		assert.Contains(t, err.Error(), "delivery status is unknown")
		assert.Contains(t, err.Error(), "Check the conversation before resending")
		assert.NotContains(t, err.Error(), "was not delivered")
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
		assert.NotContains(t, err.Error(), "timed out waiting for Hub confirmation")
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
		assert.Contains(t, sendErr.Error(), "delivery status is unknown")
		assert.Contains(t, sendErr.Error(), "Check the conversation before resending")
		assert.NotContains(t, sendErr.Error(), "was not delivered")
	})
}

// TestUploadThenWakeSendContext_UsesWakeTimeout pins that msgWakeSendTimeout
// is 4m30s (outlasting the hub's 240s DM wake write budget) and that
// uploadThenWakeSendContext applies that deadline to the returned context.
func TestUploadThenWakeSendContext_UsesWakeTimeout(t *testing.T) {
	msgAttachUpload = nil
	const wantTimeout = 4*time.Minute + 30*time.Second
	assert.Equal(t, wantTimeout, msgWakeSendTimeout)

	before := time.Now()
	_, ctx, cancel, err := uploadThenWakeSendContext(context.Background())
	require.NoError(t, err)
	defer cancel()

	dl, ok := ctx.Deadline()
	require.True(t, ok, "wake send context must have a deadline")
	assert.Greater(t, time.Until(dl), 4*time.Minute, "wake send context must use msgWakeSendTimeout")
	assert.LessOrEqual(t, dl, before.Add(wantTimeout+5*time.Second))
}
