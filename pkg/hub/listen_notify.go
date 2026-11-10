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
	"fmt"
	"log/slog"
	"net"
)

// listenThenNotify binds the TCP address addr and, once it is bound, runs
// onListening (when set) in its own goroutine, so a slow, failing or
// panicking hook never delays serving or crashes the process. A bind failure returns the error and never runs
// the hook. Server.Start and WebServer.Start use it in place of
// http.Server.ListenAndServe (which is net.Listen then Serve).
func listenThenNotify(addr string, onListening func()) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if onListening != nil {
		go runOnListening(onListening)
	}
	return ln, nil
}

// runOnListening runs an on-listening hook. A panic in it is logged and
// recovered: it runs outside Serve, so an unrecovered panic would end the
// process.
func runOnListening(hook func()) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("On-listening hook panicked", "panic", fmt.Sprint(r))
		}
	}()
	hook()
}

// SetOnListening sets a hook run once the hub API listener is bound
// (standalone mode). It must be set before Start.
func (s *Server) SetOnListening(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onListening = f
}

// SetOnListening sets a hook run once the web listener (which also serves
// the hub API in combined mode) is bound. It must be set before Start.
func (ws *WebServer) SetOnListening(f func()) {
	ws.onListening = f
}
