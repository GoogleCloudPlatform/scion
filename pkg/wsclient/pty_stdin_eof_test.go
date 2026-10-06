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

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPTYClient_Run_NonTTYStdinEOF_SendsNormalCloseAndExitsZero pins the
// behavior of a non-interactive CLI invocation: when stdin is not a
// terminal and is already at EOF, readFromStdin's io.EOF branch returns nil
// (not an error), so it is the first — and, here, only — sender on errCh.
// Run() then sends a CloseNormalClosure (1000) frame to the server and
// returns nil, all before any server-side rejection could ever arrive. This
// is not a race: with the write end of stdin's pipe closed before Connect
// even runs, the EOF is immediate and unconditional, so the ordering is
// reproduced every time, not just "usually". This is the CLI closing
// normally on its own initiative — a broker that separately refuses the
// same session never gets to decide the code the CLI already sent.
func TestPTYClient_Run_NonTTYStdinEOF_SendsNormalCloseAndExitsZero(t *testing.T) {
	// A pipe whose write end is closed before Run() ever starts: the first
	// read done by readFromStdin's inner reader goroutine returns (0, io.EOF)
	// immediately, deterministically, not depending on timing. Given to the
	// client directly (never the shared os.Stdin package variable), so the
	// leaked inner reader goroutine never races anything once the test ends.
	r, w, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, w.Close())
	t.Cleanup(func() { _ = r.Close() })

	type closeInfo struct {
		code int
		text string
	}
	closeCh := make(chan closeInfo, 1)

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		conn.SetCloseHandler(func(code int, text string) error {
			closeCh <- closeInfo{code, text}
			// Mirror gorilla's default close-handler behavior (echo a close
			// frame back) so the client's own close write does not hang.
			msg := websocket.FormatCloseMessage(code, "")
			_ = conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(time.Second))
			return nil
		})
		// Keep reading so the close handler above actually fires; never send
		// any application data, so the "server rejects" path never races
		// this — there is nothing for it to race against.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	client := NewPTYClient(PTYClientConfig{
		Endpoint: srv.URL,
		Token:    "scion-user-token",
		Slug:     "non-tty-agent",
	})
	client.stdin = r
	require.NoError(t, client.Connect(context.Background()))

	runErr := client.Run()
	assert.NoError(t, runErr, "a non-TTY stdin EOF must be treated as a clean detach (CLI exit 0), not an error")

	select {
	case ci := <-closeCh:
		assert.Equal(t, websocket.CloseNormalClosure, ci.code,
			"the CLI's own stdin-EOF close must arrive as 1000, not left to a later broker rejection")
	case <-time.After(5 * time.Second):
		t.Fatal("server never observed a close frame from the client")
	}
}
