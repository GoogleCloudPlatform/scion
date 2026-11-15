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

package hub

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/liveness"
)

// The heartbeat loop sends the LiveWork callback's result as "liveWork",
// and omits the field when the callback returns nil, so an older Hub sees
// exactly the heartbeat it always did (ptone/scion#7855).
func TestClient_StartHeartbeat_LiveWork(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]json.RawMessage
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")
	calls := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := client.StartHeartbeat(ctx, &HeartbeatConfig{
		Interval: 10 * time.Millisecond,
		Timeout:  time.Second,
		LiveWork: func() *liveness.LiveWork {
			calls++
			if calls == 1 {
				return &liveness.LiveWork{Kind: liveness.KindProcess, SuppressStall: true, Detail: "go (pid 7, 1.0s cpu)"}
			}
			if calls == 2 {
				cancel()
			}
			return nil
		},
	})
	<-done

	mu.Lock()
	defer mu.Unlock()
	require.GreaterOrEqual(t, len(bodies), 1)
	assert.JSONEq(t, `true`, string(bodies[0]["heartbeat"]))
	assert.JSONEq(t, `{"kind":"process","suppressStall":true,"detail":"go (pid 7, 1.0s cpu)"}`, string(bodies[0]["liveWork"]))
	for _, b := range bodies[1:] {
		_, has := b["liveWork"]
		assert.False(t, has, "a heartbeat without live work omits the field")
	}
}

func TestClient_Heartbeat_OmitsLiveWork(t *testing.T) {
	var body map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClientWithConfig(server.URL, "test-token", "agent-123")
	require.NoError(t, client.Heartbeat(context.Background()))
	assert.Equal(t, map[string]json.RawMessage{"heartbeat": json.RawMessage(`true`)}, body)
}
