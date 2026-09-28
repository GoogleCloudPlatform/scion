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

package brokerclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// TestAgentService_List_SendsProjectIDQueryKey verifies that List sends the
// project filter under the canonical "projectId" query key, and never under
// the legacy "groveId" key.
func TestAgentService_List_SendsProjectIDQueryKey(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotQuery := r.URL.Query()
		if got := gotQuery.Get("projectId"); got != "my-project" {
			t.Errorf("projectId query param = %q, want %q", got, "my-project")
		}
		if _, ok := gotQuery["groveId"]; ok {
			t.Errorf("query params must not include groveId, got %v", gotQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(runtimebroker.ListAgentsResponse{})
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}

	_, err = client.Agents().List(context.Background(), &ListAgentsOptions{ProjectID: "my-project"})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if n := hits.Load(); n != 1 {
		t.Fatalf("List made %d requests to the broker, want 1", n)
	}
}
