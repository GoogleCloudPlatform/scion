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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCreateTriggerRequest_MessageOfflineAndTemplateAllow(t *testing.T) {
	reset := func() {
		triggerName = "msg-p2b"
		triggerDescription = ""
		triggerWebhookRef = "deploys"
		triggerGitHubSource = false
		triggerMatchTypes = nil
		triggerWhereRules = nil
		triggerWhereFile = ""
		triggerAction = "message"
		triggerAgent = "lead"
		triggerMessage = "Deploy ${event.attributes.env} (${event.payload.sha})"
		triggerInterrupt = false
		triggerWake = false
		triggerPlain = false
		triggerOffline = ""
		triggerQueueTTL = ""
		triggerDispatch = dispatchAgentFlagValues{}
		triggerSubjectExpr = ""
		triggerOnSubject = ""
	}
	t.Cleanup(reset)

	t.Run("derives untrustedVars and sets offline queue + queue-ttl", func(t *testing.T) {
		reset()
		triggerOffline = "queue"
		triggerQueueTTL = "30m"

		req, derived, err := buildCreateTriggerRequestWithAllow(false)
		require.NoError(t, err)
		assert.Equal(t, "message", req.Action.Type)
		assert.Equal(t, "queue", req.Action.Offline)
		assert.Equal(t, 1800, req.Action.QueueTTLSeconds)
		assert.Equal(t, []string{"event.attributes.env", "event.payload.sha"}, derived)
		assert.Equal(t, []string{"event.attributes.env", "event.payload.sha"}, req.Action.UntrustedVars)
	})

	t.Run("explicit --allow overrides derivation", func(t *testing.T) {
		reset()
		triggerDispatch.allow = []string{"event.attributes.*", "event.payload.*"}

		req, derived, err := buildCreateTriggerRequestWithAllow(true)
		require.NoError(t, err)
		assert.Empty(t, derived)
		assert.Equal(t, []string{"event.attributes.*", "event.payload.*"}, req.Action.UntrustedVars)
	})

	t.Run("queue-ttl without offline queue fails", func(t *testing.T) {
		reset()
		triggerOffline = "skip"
		triggerQueueTTL = "30m"

		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--queue-ttl requires --offline queue")
	})

	t.Run("queue-ttl out of range fails", func(t *testing.T) {
		reset()
		triggerOffline = "queue"
		triggerQueueTTL = "30s"

		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "between 60s and 48h")
	})

	t.Run("invalid offline value fails", func(t *testing.T) {
		reset()
		triggerOffline = "drop"

		_, _, err := buildCreateTriggerRequestWithAllow(false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid --offline")
	})
}

func TestTriggerHistory_ShowsWaitingExpiredAndWaitUntil(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	waitUntil := now.Add(2 * time.Hour)
	finishedAt := now.Add(1 * time.Hour)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := hubclient.ListTriggerFiresResponse{
			Fires: []hubclient.TriggerFire{
				{
					ID:        "11111111-1111-1111-1111-111111111111",
					TriggerID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					EventID:   "22222222-2222-2222-2222-222222222222",
					ProjectID: "proj-1",
					Subject:   "deploy:1",
					Status:    "waiting",
					WaitUntil: &waitUntil,
					Attempts:  1,
					Error:     "target not running",
					StartedAt: now,
				},
				{
					ID:         "33333333-3333-3333-3333-333333333333",
					TriggerID:  "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					EventID:    "44444444-4444-4444-4444-444444444444",
					ProjectID:  "proj-1",
					Subject:    "deploy:2",
					Status:     "expired",
					WaitUntil:  &waitUntil,
					Attempts:   5,
					Error:      "offline queue expired",
					StartedAt:  now,
					FinishedAt: &finishedAt,
				},
			},
			TotalCount: 2,
			ServerTime: now,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	origFn := triggerHubContextFn
	origJSON := triggerOutputJSON
	t.Cleanup(func() {
		triggerHubContextFn = origFn
		triggerOutputJSON = origJSON
	})
	triggerOutputJSON = false
	triggerHubContextFn = func() (*HubContext, string, context.Context, context.CancelFunc, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		return &HubContext{Client: client, Endpoint: srv.URL}, "proj-1", ctx, cancel, nil
	}

	origStdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	runErr := runTriggerHistory(triggerHistoryCmd, []string{"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"})
	_ = w.Close()
	os.Stdout = origStdout
	require.NoError(t, runErr)

	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	out := buf.String()

	assert.Contains(t, out, "WAIT UNTIL")
	assert.Contains(t, out, "waiting")
	assert.Contains(t, out, "expired")
	assert.Contains(t, out, waitUntil.UTC().Format(time.RFC3339))
	assert.Contains(t, out, "offline queue expired")
}
