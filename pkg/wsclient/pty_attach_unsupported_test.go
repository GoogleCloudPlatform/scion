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

package wsclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadFromWebSocket_AttachUnsupportedCloseCode_MapsToExplicitError
// covers the client-side half of the 4501/attach_unsupported wire contract
// (pkg/wsprotocol.ClosePTYAttachUnsupported): a broker that refuses attach
// after the WebSocket has already been upgraded (the control-channel gate's
// StreamClose, passed through unchanged by the Hub) must surface as the
// same explicit, actionable error attachUnsupportedErr gives pre-dial in
// cmd/attach.go — not the raw "websocket: close 4501 (unknown): ..." a
// caller would otherwise see — and Run() must return a non-nil error so the
// CLI exits non-zero.
func TestReadFromWebSocket_AttachUnsupportedCloseCode_MapsToExplicitError(t *testing.T) {
	// Give the client its own, never-closed pipe as stdin (never the shared
	// os.Stdin package variable) so readFromStdin blocks instead of racing
	// the WebSocket goroutine with an EOF of its own; the websocket close
	// below must be what decides Run()'s error. The read end is left open
	// for the test's duration; the leaked inner reader goroutine (blocked on
	// a read syscall against a file only this test holds) is harmless.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(wsprotocol.ClosePTYAttachUnsupported, wsprotocol.CloseReasonAttachUnsupported),
			time.Now().Add(time.Second))
	}))
	defer srv.Close()

	client := NewPTYClient(PTYClientConfig{
		Endpoint: srv.URL,
		Token:    "scion-user-token",
		Slug:     "unsupported-agent",
	})
	client.stdin = r
	require.NoError(t, client.Connect(context.Background()))

	err = client.Run()
	require.Error(t, err, "expected a non-nil error so the CLI exits non-zero")
	assert.Equal(t, "attach is not supported for this agent's runtime", err.Error())
}
