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
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

func TestHealthSummary_DecodesAndKeepsRawBody(t *testing.T) {
	// "future_field" is not modelled by the client; Raw must keep it.
	body := `{"status":"degraded","generated_at":"2026-10-12T10:00:00Z",` +
		`"attention":[{"severity":"warning","kind":"broker_offline","subject":{"type":"runtime_broker","id":"b1","name":"east"},"message":"Runtime broker \"east\" is offline"}],` +
		`"hub":{"status":"healthy","instance_id":"i1","version":"1.0.0","instances":{"live":1,"healthy":1,"degraded":0,"unhealthy":0},"unhealthy_checks":[]},` +
		`"runtime_brokers":{"items":[{"id":"b1","name":"east","status":"offline","agents":{"running":0,"attention":0}}],"total":1,"truncated":false},` +
		`"agents":{"total":3,"active":2,"errored":0,"considered":2,"by_phase":[],"problems":[]},` +
		`"dispatch":{"stuck_messages":0,"stuck_broker_dispatch":0,"failed_broker_dispatch_1h":1},` +
		`"integrations":[],"integrations_detail":false,"integration_counts":{"total":0,"healthy":0,"degraded":0,"unhealthy":0,"unknown":0},` +
		`"hub_instances":null,"future_field":{"x":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/admin/health/summary" {
			t.Errorf("got %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.HealthSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "degraded" || len(got.Attention) != 1 || got.Attention[0].Subject.Name != "east" {
		t.Errorf("unexpected summary %+v", got)
	}
	if got.Brokers.Total != 1 || got.Brokers.Items[0].Status != "offline" {
		t.Errorf("unexpected brokers %+v", got.Brokers)
	}
	if got.Agents == nil || got.Agents.Active != 2 || got.Dispatch == nil || got.Dispatch.FailedBrokerDispatch1h != 1 {
		t.Errorf("unexpected agents/dispatch %+v %+v", got.Agents, got.Dispatch)
	}
	if got.HubInstances != nil {
		t.Errorf("hub_instances null should decode to nil, got %+v", got.HubInstances)
	}
	if string(got.Raw) != body {
		t.Errorf("Raw = %s, want the body verbatim", got.Raw)
	}
}

func TestHealthSummary_ErrorStatusesAreAPIErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{name: "forbidden", status: http.StatusForbidden, code: "forbidden"},
		{name: "unavailable", status: http.StatusServiceUnavailable, code: "unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error":{"code":"` + tt.code + `","message":"fixed message"}}`))
			}))
			defer srv.Close()

			c, err := New(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.HealthSummary(context.Background())
			if got != nil {
				t.Errorf("summary = %+v, want nil", got)
			}
			var apiErr *apiclient.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.status {
				t.Fatalf("err = %v, want APIError with status %d", err, tt.status)
			}
			if apiErr.Message != "fixed message" {
				t.Errorf("message = %q", apiErr.Message)
			}
		})
	}
}
