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

package hubclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

func TestOutboundMessageRequest_CommandRequestJSON(t *testing.T) {
	plain, err := json.Marshal(&OutboundMessageRequest{Recipient: "user:alice@example.com", Msg: "hi"})
	require.NoError(t, err)
	var plainMap map[string]any
	require.NoError(t, json.Unmarshal(plain, &plainMap))
	assert.NotContains(t, plainMap, "command_request")
	assert.NotContains(t, plainMap, "command_request_update")

	raw := json.RawMessage(`{"key":"deploy","script":"echo ok"}`)
	enc, err := json.Marshal(&OutboundMessageRequest{
		Recipient:            "user:alice@example.com",
		CommandRequest:       raw,
		CommandRequestUpdate: true,
	})
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(enc, &m))
	assert.Equal(t, true, m["command_request_update"])
	assert.Contains(t, m, "command_request")
}

func TestCommandRequestService_GetApproveList(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var gotApproveBody messages.ApproveCommandRequestBody
	var gotListKey string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/command-requests/row-1":
			_ = json.NewEncoder(w).Encode(messages.CommandRequestDetailResponse{
				Request: messages.CommandRequestView{
					RowID:         "row-1",
					Key:           "deploy",
					Version:       1,
					Script:        "gcloud config list",
					TimeoutS:      600,
					CommandSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Status:        "pending",
					CreatedAt:     now,
					ExpiresAt:     now.Add(24 * time.Hour),
				},
				CanApprove: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/command-requests/row-1/approve":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotApproveBody))
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(messages.ApproveCommandRequestResponse{
				Request: messages.CommandRequestView{
					RowID:   "row-1",
					Key:     "deploy",
					Version: 1,
					Status:  "approved",
				},
				Run: messages.CommandRequestRunView{
					RunID:     "run-1",
					RequestID: "row-1",
					Status:    "queued",
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/command-requests/row-1/output":
			w.Header().Set("Cache-Control", "no-store")
			outBytes := int64(11)
			_ = json.NewEncoder(w).Encode(messages.CommandRequestOutputResponse{
				RunID:        "run-1",
				CaptureMode:  "tail",
				OutputStatus: "captured",
				OutputBytes:  &outBytes,
				Output:       "hello world",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agent/command-requests":
			gotListKey = r.URL.Query().Get("key")
			_ = json.NewEncoder(w).Encode(messages.AgentCommandRequestListResponse{
				Items: []messages.CommandRequestView{
					{
						RowID:   "row-1",
						Key:     "deploy",
						Version: 1,
						Status:  "approved",
						Run: &messages.CommandRequestRunView{
							RunID:       "run-1",
							Status:      "succeeded",
							ShareOutput: true,
							OutputTail:  "hello world",
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c, err := New(ts.URL)
	require.NoError(t, err)

	ctx := context.Background()
	detail, err := c.CommandRequests().Get(ctx, "row-1")
	require.NoError(t, err)
	assert.Equal(t, "deploy", detail.Request.Key)
	assert.True(t, detail.CanApprove)

	shareTrue := true
	app, err := c.CommandRequests().Approve(ctx, "row-1", &ApproveCommandRequestBody{
		RowID:          "row-1",
		CommandSHA256:  detail.Request.CommandSHA256,
		ShareOutput:    &shareTrue,
		Surface:        "cli",
		IdempotencyKey: "k-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "run-1", app.Run.RunID)
	assert.Equal(t, "cli", gotApproveBody.Surface)
	require.NotNil(t, gotApproveBody.ShareOutput)
	assert.True(t, *gotApproveBody.ShareOutput)

	outResp, err := c.CommandRequests().GetOutput(ctx, "row-1")
	require.NoError(t, err)
	assert.Equal(t, "run-1", outResp.RunID)
	assert.Equal(t, "captured", outResp.OutputStatus)
	assert.Equal(t, "hello world", outResp.Output)

	list, err := c.CommandRequests().List(ctx, &ListAgentCommandRequestsOptions{Key: "deploy"})
	require.NoError(t, err)
	assert.Equal(t, "deploy", gotListKey)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "succeeded", list.Items[0].Run.Status)
	assert.Equal(t, "hello world", list.Items[0].Run.OutputTail)
}

func TestCommandRequestService_DismissAndWithdraw(t *testing.T) {
	var gotDismissBody messages.DismissCommandRequestBody
	var gotWithdrawBody messages.WithdrawCommandRequestBody

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/command-requests/row-2/dismiss":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotDismissBody))
			_ = json.NewEncoder(w).Encode(messages.DismissCommandRequestResponse{
				Request: messages.CommandRequestView{
					RowID:   "row-2",
					Key:     "deploy",
					Version: 1,
					Status:  "dismissed",
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agent/command-requests/withdraw":
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotWithdrawBody))
			_ = json.NewEncoder(w).Encode(messages.WithdrawCommandRequestResponse{
				Request: messages.CommandRequestView{
					RowID:   "row-3",
					Key:     "cleanup",
					Version: 2,
					Status:  "withdrawn",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	c, err := New(ts.URL)
	require.NoError(t, err)
	ctx := context.Background()

	dis, err := c.CommandRequests().Dismiss(ctx, "row-2", &DismissCommandRequestBody{
		Reason:         "not needed",
		Surface:        "cli",
		IdempotencyKey: "dis-key-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "dismissed", dis.Request.Status)
	assert.Equal(t, "not needed", gotDismissBody.Reason)
	assert.Equal(t, "cli", gotDismissBody.Surface)

	wd, err := c.CommandRequests().Withdraw(ctx, &WithdrawCommandRequestBody{
		Key:    "cleanup",
		Reason: "no longer needed",
	})
	require.NoError(t, err)
	assert.Equal(t, "withdrawn", wd.Request.Status)
	assert.Equal(t, "cleanup", gotWithdrawBody.Key)
	assert.Equal(t, "no longer needed", gotWithdrawBody.Reason)
}
