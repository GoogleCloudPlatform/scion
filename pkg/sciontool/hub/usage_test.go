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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

func TestClient_ProbeUsageCapabilities(t *testing.T) {
	t.Run("positive probe", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", r.Method)
			}
			if r.URL.Path != "/api/v1/agents/agent-1/usage/capabilities" {
				t.Errorf("path = %s, want /api/v1/agents/agent-1/usage/capabilities", r.URL.Path)
			}
			if got := r.Header.Get("X-Scion-Agent-Token"); got != "tok-1" {
				t.Errorf("X-Scion-Agent-Token = %q, want tok-1", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageCapabilitiesResponse{
				UsageIngestEnabled:   true,
				SuppressDirectOTel:   true,
				MaxBatchEvents:       500,
				MaxBatchBytes:        262144,
				HeartbeatIntervalSec: 60,
				DrainIntervalSec:     10,
				HubID:                "hub-1",
				RunID:                "run-1",
			})
		}))
		defer srv.Close()

		c := NewClientWithConfig(srv.URL, "tok-1", "agent-1")
		caps, err := c.ProbeUsageCapabilities(context.Background())
		if err != nil {
			t.Fatalf("ProbeUsageCapabilities: %v", err)
		}
		if !caps.UsageIngestEnabled || caps.MaxBatchEvents != 500 || caps.HubID != "hub-1" || caps.RunID != "run-1" {
			t.Errorf("unexpected caps: %+v", caps)
		}
	})

	t.Run("404 unsupported", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		c := NewClientWithConfig(srv.URL, "tok-1", "agent-1")
		_, err := c.ProbeUsageCapabilities(context.Background())
		if !errors.Is(err, ErrUsageIngestUnsupported) {
			t.Fatalf("err = %v, want ErrUsageIngestUnsupported", err)
		}
	})

	t.Run("unconfigured client", func(t *testing.T) {
		c := NewClientWithConfig("", "", "")
		if _, err := c.ProbeUsageCapabilities(context.Background()); err == nil {
			t.Fatal("expected error for unconfigured client")
		}
	})
}

func TestClient_PostUsageEvents(t *testing.T) {
	t.Run("success and heartbeat", func(t *testing.T) {
		var gotReq telemetrycontract.UsageIngestRequest
		var gotRaw map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Errorf("method = %s, want POST", r.Method)
			}
			if r.URL.Path != "/api/v1/agents/agent-1/usage/events" {
				t.Errorf("path = %s, want /api/v1/agents/agent-1/usage/events", r.URL.Path)
			}
			if got := r.Header.Get("X-Scion-Agent-Token"); got != "tok-1" {
				t.Errorf("X-Scion-Agent-Token = %q, want tok-1", got)
			}
			dec := json.NewDecoder(r.Body)
			if err := dec.Decode(&gotRaw); err != nil {
				t.Fatalf("decode raw: %v", err)
			}
			b, _ := json.Marshal(gotRaw)
			_ = json.Unmarshal(b, &gotReq)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telemetrycontract.UsageIngestResponse{
				Accepted:   1,
				Duplicates: 0,
				Rejected: []telemetrycontract.UsageIngestRejection{
					{Index: 1, EventID: "bad-1", Code: telemetrycontract.UsageRejectInvalidEvent, Reason: "bad"},
				},
			})
		}))
		defer srv.Close()

		c := NewClientWithConfig(srv.URL, "tok-1", "agent-1")
		resp, err := c.PostUsageEvents(context.Background(), telemetrycontract.UsageIngestRequest{
			ProducerEpoch:    "ep-1",
			ProducerSeq:      5,
			SupportedSignals: []string{"calls", "input"},
		})
		if err != nil {
			t.Fatalf("PostUsageEvents: %v", err)
		}
		if resp.Accepted != 1 || len(resp.Rejected) != 1 || resp.Rejected[0].Index != 1 {
			t.Errorf("unexpected resp: %+v", resp)
		}
		// Ensure nil Events is serialized as [] rather than null.
		eventsVal, ok := gotRaw["events"].([]any)
		if !ok || len(eventsVal) != 0 {
			t.Errorf("events JSON = %#v, want empty array", gotRaw["events"])
		}
		if gotReq.ProducerEpoch != "ep-1" || gotReq.ProducerSeq != 5 {
			t.Errorf("unexpected req: %+v", gotReq)
		}
	})

	t.Run("http error classification", func(t *testing.T) {
		for _, tc := range []struct {
			code          int
			wantPermanent bool
			wantTooLarge  bool
			wantNotFound  bool
		}{
			{http.StatusBadRequest, true, false, false},
			{http.StatusRequestEntityTooLarge, false, true, false},
			{http.StatusNotFound, false, false, true},
			{http.StatusUnprocessableEntity, true, false, false},
			{http.StatusUnauthorized, false, false, false},
			{http.StatusTooManyRequests, false, false, false},
			{http.StatusServiceUnavailable, false, false, false},
		} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte("err"))
			}))
			c := NewClientWithConfig(srv.URL, "tok-1", "agent-1")
			_, err := c.PostUsageEvents(context.Background(), telemetrycontract.UsageIngestRequest{})
			srv.Close()
			var httpErr *UsageIngestHTTPError
			if !errors.As(err, &httpErr) {
				t.Fatalf("code %d: err = %v, want *UsageIngestHTTPError", tc.code, err)
			}
			if httpErr.IsPermanent() != tc.wantPermanent {
				t.Errorf("code %d: IsPermanent() = %v, want %v", tc.code, httpErr.IsPermanent(), tc.wantPermanent)
			}
			if httpErr.IsPayloadTooLarge() != tc.wantTooLarge {
				t.Errorf("code %d: IsPayloadTooLarge() = %v, want %v", tc.code, httpErr.IsPayloadTooLarge(), tc.wantTooLarge)
			}
			if httpErr.IsNotFound() != tc.wantNotFound {
				t.Errorf("code %d: IsNotFound() = %v, want %v", tc.code, httpErr.IsNotFound(), tc.wantNotFound)
			}
		}
	})
}
