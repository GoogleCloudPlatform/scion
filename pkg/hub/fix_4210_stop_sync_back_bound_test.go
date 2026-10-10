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
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4210: the stop-time workspace sync-back (the upload tunneled
// to the broker, then the download into the hub workspace) is bounded by
// stopSyncBackTimeout, its share of the stop's write budget. A download
// that outlasts it is cut, logged and reported as a warning, and the stop
// goes on and answers within the response's write deadline.

// answerBrokerUploads answers the next n workspace upload requests tunneled
// to broker, each after delay, with an empty manifest, and sends each
// request's path on the returned channel.
func answerBrokerUploads(t *testing.T, broker *fakeBroker, delay time.Duration, n int) <-chan string {
	t.Helper()
	paths := make(chan string, n)
	go func() {
		for answered := 0; answered < n; {
			var env wsprotocol.RequestEnvelope
			if err := broker.ws.ReadJSON(&env); err != nil {
				return
			}
			if env.Type != wsprotocol.TypeRequest {
				continue
			}
			paths <- env.Path
			time.Sleep(delay)
			body, _ := json.Marshal(RuntimeBrokerWorkspaceUploadResponse{Manifest: &transfer.Manifest{Version: "1.0"}})
			_ = broker.ws.WriteJSON(wsprotocol.NewResponseEnvelope(env.RequestID, http.StatusOK,
				map[string]string{"Content-Type": "application/json"}, body))
			answered++
		}
	}()
	return paths
}

// errDownloadNeverCut is returned by a blocking download whose ctx was not
// done within requestCancelWait: the download had no bound.
var errDownloadNeverCut = errors.New("download ctx never done")

func TestStopSyncBack_DownloadOutlastsBound_StopStillAnswers(t *testing.T) {
	for _, action := range []string{"stop", "suspend"} {
		t.Run(action, func(t *testing.T) {
			const (
				syncBound   = 400 * time.Millisecond
				uploadDelay = 50 * time.Millisecond
			)
			t.Setenv("HOME", t.TempDir())
			disp := &slowLaunchDispatcher{delay: 50 * time.Millisecond}
			srv, s, project := setupCreateAgentServer(t, disp) // hub-managed: no GitRemote.
			shortenSyncDispatchTimeout(t, syncBound)
			setSyncDispatchWriteSlack(t, 100*time.Millisecond)
			require.Equal(t, syncBound, stopSyncBackTimeout(), "fixture check: the sync-back bound follows syncDispatchTimeout")
			require.Greater(t, syncBound, slowPathWriteTimeout, "fixture check: the bound must outlast the WriteTimeout")
			setAgentQuotaLimits(t, s)

			srv.SetStorage(newContentMockStorage("test-bucket"))
			downloadErr := make(chan error, 1)
			var downloadFor time.Duration
			srv.setHubWorkspaceDownloader(func(ctx context.Context, _, _, _ string) error {
				// A download far larger than the bound: it runs until its
				// ctx is cut.
				start := time.Now()
				err := errDownloadNeverCut
				if awaitCanceled(ctx) {
					err = ctx.Err()
				}
				downloadFor = time.Since(start)
				downloadErr <- err
				return err
			})
			agent := createSiteAgent(t, s, project, "sync-bound-"+action, state.PhaseRunning, store.RunIntentRunning)

			broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
			uploaded := answerBrokerUploads(t, broker, uploadDelay, 1)

			code, body := serveThroughSlowListener(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil, syncBound)
			select {
			case path := <-uploaded:
				assert.Equal(t, "/api/v1/workspace/upload", path, "fixture check: the sync-back is tunneled")
			default:
				t.Fatal("fixture check: the sync-back never reached the broker")
			}
			select {
			case err := <-downloadErr:
				require.ErrorIs(t, err, context.DeadlineExceeded, "the download must be cut at the sync-back bound")
				assert.Less(t, downloadFor, syncBound, "the bound covers the upload and the download together")
			default:
				t.Fatal("fixture check: the download never ran")
			}
			require.Equal(t, http.StatusOK, code, string(body))
			var resp struct {
				Phase    string   `json:"phase"`
				Warnings []string `json:"warnings"`
			}
			require.NoError(t, json.Unmarshal(body, &resp))
			assert.Contains(t, resp.Warnings, stopSyncBackTimedOutWarning, "the cut sync-back is reported")

			want := string(state.PhaseStopped)
			if action == "suspend" {
				want = string(state.PhaseSuspended)
			}
			assert.Equal(t, want, resp.Phase)
			got, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, want, got.Phase, "the stop goes on after the cut sync-back")
		})
	}
}

// A sync-back that finishes within its bound gives no warning.
func TestStopSyncBack_WithinBound_NoWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	disp := &slowLaunchDispatcher{delay: 10 * time.Millisecond}
	srv, s, project := setupCreateAgentServer(t, disp)
	srv.SetStorage(newContentMockStorage("test-bucket"))
	var downloaded bool
	srv.setHubWorkspaceDownloader(func(context.Context, string, string, string) error {
		downloaded = true
		return nil
	})
	agent := createSiteAgent(t, s, project, "sync-bound-ok", state.PhaseRunning, store.RunIntentRunning)
	broker := connectFakeBroker(t, srv, agent.RuntimeBrokerID)
	answerBrokerUploads(t, broker, 0, 1)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/stop", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, downloaded, "fixture check: the sync-back downloads into the hub workspace")
	assert.NotContains(t, rec.Body.String(), stopSyncBackTimedOutWarning)
}
