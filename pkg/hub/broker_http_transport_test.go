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
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestDoRequest_PropagatesContextCancellation confirms, for ptone/scion#1886,
// that the co-located ("stateless local broker") HTTP dispatch path already
// propagates the caller's context cancellation down to the broker's HTTP
// handler via ordinary net/http semantics: doRequest builds its outbound
// request with http.NewRequestWithContext, and when the client gives up
// mid-flight net/http closes the underlying connection, which cancels the
// server-side handler's r.Context().
//
// This is the opposite path from the control-channel tunnel (fixed
// separately in this change with an explicit "cancel" wsprotocol message):
// that path dispatches through httptest.NewRecorder rather than a real HTTP
// connection, so it has no such built-in cancellation signal and needed one
// added. Here, a real server confirms no fix is needed -- if this test ever
// starts failing (e.g. after a transport refactor), the co-located path
// would need the same treatment as the control-channel path.
func TestDoRequest_PropagatesContextCancellation(t *testing.T) {
	handlerStarted := make(chan struct{})
	handlerCancelled := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		select {
		case <-r.Context().Done():
			close(handlerCancelled)
		case <-time.After(10 * time.Second):
			// If we get here, the outer test timeout will catch it below.
		}
	}))
	defer srv.Close()

	transport := newBrokerHTTPTransport(false, nil)

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Expected to fail once we cancel ctx -- we only care that the
		// server-side handler observed the cancellation.
		_, _ = transport.doRequest(ctx, "broker-1", http.MethodPost, srv.URL, nil)
	}()

	select {
	case <-handlerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("broker handler never started")
	}

	cancel()

	select {
	case <-handlerCancelled:
		// Success: the real HTTP transport propagated cancellation to the
		// handler, confirming the co-located path needs no explicit cancel
		// message alongside the control-channel one added for #1886.
	case <-time.After(5 * time.Second):
		t.Fatal("broker handler did not observe caller context cancellation within timeout; " +
			"the co-located HTTP transport does not propagate cancellation and needs a fix")
	}

	wg.Wait()
}
