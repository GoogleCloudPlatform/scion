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
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
)

func deliverTestReq() *agentcontrol.DeliverRequest {
	text := "hi"
	return &agentcontrol.DeliverRequest{V: 1, DeliveryID: "d1", MessageIDs: []string{"m1"}, Op: agentcontrol.OpPaste,
		Text: &text, DeadlineMS: 92000, SubmitKeys: []string{"Enter"}}
}

// TestDeliverMessage_OutlastsClientTimeout (review R2): the hub may hold a
// /deliver call for its whole budget (readiness wait, then paste), so the
// request is not cut off by the client's shorter configured timeout, but
// the caller's own deadline still applies.
func TestDeliverMessage_OutlastsClientTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-time.After(600 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(agentcontrol.DeliverResponse{Outcome: agentcontrol.OutcomeDelivered})
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithTimeout(100*time.Millisecond), WithRetry(3, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	status, resp, err := c.RuntimeBrokers().DeliverMessage(context.Background(), "b1", "a1", deliverTestReq())
	if err != nil || status != http.StatusOK || resp.Outcome != agentcontrol.OutcomeDelivered {
		t.Fatalf("= %d %+v %v; want 200 delivered past the 100ms client timeout", status, resp, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err = c.RuntimeBrokers().DeliverMessage(ctx, "b1", "a1", deliverTestReq())
	if err == nil || !errors.Is(err, context.DeadlineExceeded) && time.Since(start) > 500*time.Millisecond {
		t.Fatalf("caller deadline not honoured: err=%v after %s", err, time.Since(start))
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("caller deadline not honoured: %s", time.Since(start))
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("server saw %d calls, want 2 (sent once each, never retried)", n)
	}
}

// TestDeliverMessage_NoRetryNoRedirect: a 503 is answered as is (sent once,
// even with retries configured) and a redirect is not followed.
func TestDeliverMessage_NoRetryNoRedirect(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/elsewhere" {
			t.Error("redirect followed")
		}
		if r.URL.Query().Get("x") == "" && calls.Load() == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"outcome":"not_reached"}`))
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c, err := New(srv.URL, WithRetry(3, time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	status, resp, err := c.RuntimeBrokers().DeliverMessage(context.Background(), "b1", "a1", deliverTestReq())
	if err != nil || status != http.StatusServiceUnavailable || resp.Outcome != "not_reached" || calls.Load() != 1 {
		t.Fatalf("= %d %+v %v calls=%d", status, resp, err, calls.Load())
	}
	status, _, err = c.RuntimeBrokers().DeliverMessage(context.Background(), "b1", "a1", deliverTestReq())
	if err != nil || status != http.StatusTemporaryRedirect {
		t.Fatalf("redirect: status %d err %v", status, err)
	}
}
