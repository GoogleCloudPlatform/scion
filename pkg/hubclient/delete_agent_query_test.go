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
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// TestDeleteAgent_ProjectAndAgentScopedSendSameQuery checks that the
// agent-scoped and project-scoped DeleteAgent calls encode the same
// DeleteAgentOptions into the same query, so neither drops force or the
// request to keep files or the branch.
func TestDeleteAgent_ProjectAndAgentScopedSendSameQuery(t *testing.T) {
	var (
		mu    sync.Mutex
		paths []string
		raws  []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("unexpected method %s", r.Method)
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		raws = append(raws, r.URL.RawQuery)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client, err := New(server.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		name string
		opts *DeleteAgentOptions
		want url.Values
	}{
		{name: "nil", opts: nil, want: url.Values{}},
		{name: "zero value", opts: &DeleteAgentOptions{}, want: url.Values{
			"deleteFiles":  {"false"},
			"removeBranch": {"false"},
		}},
		{name: "all true", opts: &DeleteAgentOptions{DeleteFiles: true, RemoveBranch: true, Force: true}, want: url.Values{
			"force": {"true"},
		}},
		{name: "force only", opts: &DeleteAgentOptions{Force: true}, want: url.Values{
			"deleteFiles":  {"false"},
			"removeBranch": {"false"},
			"force":        {"true"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mu.Lock()
			paths, raws = nil, nil
			mu.Unlock()

			ctx := context.Background()
			if err := client.Agents().Delete(ctx, "a1", tc.opts); err != nil {
				t.Fatalf("Agents().Delete: %v", err)
			}
			if err := client.Projects().DeleteAgent(ctx, "p1", "a1", tc.opts); err != nil {
				t.Fatalf("Projects().DeleteAgent: %v", err)
			}

			mu.Lock()
			defer mu.Unlock()
			if len(raws) != 2 {
				t.Fatalf("got %d requests, want 2", len(raws))
			}
			if paths[1] != "/api/v1/projects/p1/agents/a1" {
				t.Errorf("project-scoped path = %q", paths[1])
			}
			if raws[0] != raws[1] {
				t.Errorf("queries differ: agent-scoped %q, project-scoped %q", raws[0], raws[1])
			}
			got, err := url.ParseQuery(raws[1])
			if err != nil {
				t.Fatalf("parse query %q: %v", raws[1], err)
			}
			if got.Encode() != tc.want.Encode() {
				t.Errorf("query = %q, want %q", got.Encode(), tc.want.Encode())
			}
		})
	}
}
