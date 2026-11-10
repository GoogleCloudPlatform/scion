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
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestArtifactCreateLinkShareURL: CreateLink decodes the hub's shareUrl
// (ptone/scion#7107 phase A) next to the hub-relative url, and leaves it
// empty when the hub sends none.
func TestArtifactCreateLinkShareURL(t *testing.T) {
	for _, tc := range []struct{ body, wantURL, wantShare string }{
		{`{"link":{"id":"l1"},"url":"/api/v1/artifacts/shared/tok","shareUrl":"https://links.example.net/api/v1/artifacts/shared/tok"}`,
			"/api/v1/artifacts/shared/tok", "https://links.example.net/api/v1/artifacts/shared/tok"},
		{`{"link":{"id":"l1"},"url":"/api/v1/artifacts/shared/tok"}`, "/api/v1/artifacts/shared/tok", ""},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/v1/artifacts/a1/links" {
				t.Errorf("request %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, tc.body)
		}))
		c, err := New(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		got, err := c.Artifacts().CreateLink(context.Background(), "a1", 0)
		srv.Close()
		if err != nil {
			t.Fatalf("CreateLink: %v", err)
		}
		if got.URL != tc.wantURL || got.ShareURL != tc.wantShare || got.Link.ID != "l1" {
			t.Errorf("CreateLink = %+v, want url %q shareUrl %q", got, tc.wantURL, tc.wantShare)
		}
	}
}
