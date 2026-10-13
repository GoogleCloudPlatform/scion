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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answerBrokerUploadWith answers the next workspace upload request tunneled
// to broker with status and body, and sends the request's decoded upload
// request on the returned channel.
func answerBrokerUploadWith(t *testing.T, broker *fakeBroker, status int, body []byte) <-chan RuntimeBrokerWorkspaceUploadRequest {
	t.Helper()
	reqs := make(chan RuntimeBrokerWorkspaceUploadRequest, 1)
	go func() {
		for {
			var env wsprotocol.RequestEnvelope
			if err := broker.ws.ReadJSON(&env); err != nil {
				return
			}
			if env.Type != wsprotocol.TypeRequest {
				continue
			}
			var req RuntimeBrokerWorkspaceUploadRequest
			_ = json.Unmarshal(env.Body, &req)
			reqs <- req
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, status,
				map[string]string{"Content-Type": "application/json"}, body))
			return
		}
	}()
	return reqs
}

// ptone/scion#4245: a broker that answers the sync-from upload with a 2xx
// status but no manifest gets a 502 with a clear message, not a handler
// panic, and the hub workspace is not synced back from it.
func TestWorkspaceSyncFrom_BrokerReplyWithoutManifest_BadGateway(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	srv, s, project := setupCreateAgentServer(t, &slowLaunchDispatcher{})
	shortenSyncDispatchTimeout(t, 3*time.Second)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	var downloaded bool
	srv.setHubWorkspaceDownloader(func(context.Context, string, string, string) error {
		downloaded = true
		return nil
	})
	agent := createSiteAgent(t, s, project, "sync-from-no-manifest", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	uploaded := answerBrokerUploadWith(t, broker, http.StatusOK, []byte(`{}`))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/workspace/sync-from", nil)
	select {
	case <-uploaded:
	default:
		t.Fatal("fixture check: the upload never reached the broker")
	}
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp), rec.Body.String())
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "no workspace manifest")
	assert.False(t, downloaded, "no sync-back after a broker reply without a manifest")
}
