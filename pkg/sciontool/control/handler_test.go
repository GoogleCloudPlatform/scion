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

package control

import (
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func newCounting() (*Handler, *int) {
	n := new(int)
	return New(Options{KickTokenRefresh: func() { *n++ }}), n
}

// TestHandlerMethodAndPathRules: POST only (405 with Allow: POST
// otherwise), 404 for any path that is not an implemented route,
// including the deferred wake and reload-config.
func TestHandlerMethodAndPathRules(t *testing.T) {
	tests := []struct {
		method, path string
		want         int
		kick         bool
	}{
		{"POST", "/v1/control/rotate-token", 202, true},
		{"GET", "/v1/control/rotate-token", 405, false},
		{"HEAD", "/v1/control/rotate-token", 405, false},
		{"PUT", "/v1/control/rotate-token", 405, false},
		{"PATCH", "/v1/control/rotate-token", 405, false},
		{"DELETE", "/v1/control/rotate-token", 405, false},
		{"OPTIONS", "/v1/control/rotate-token", 405, false},
		{"POST", "/v1/control/wake", 404, false},
		{"POST", "/v1/control/reload-config", 404, false},
		{"POST", "/v1/control/unknown", 404, false},
		{"POST", "/v1/control/", 404, false},
		{"POST", "/v1/control/rotate-token/", 404, false},
		{"POST", "/v1/control/rotate-token/x", 404, false},
		{"POST", "/scion/v1/exec", 404, false},
		{"POST", "/", 404, false},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			h, kicks := newCounting()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d", rec.Code, tt.want)
			}
			if (*kicks == 1) != tt.kick || *kicks > 1 {
				t.Fatalf("kicks = %d, want kick %v", *kicks, tt.kick)
			}
			if tt.want == 405 && rec.Header().Get("Allow") != "POST" {
				t.Fatalf("Allow = %q, want POST", rec.Header().Get("Allow"))
			}
			if tt.want == 202 && rec.Body.Len() != 0 {
				t.Fatalf("202 body = %q, want empty", rec.Body.String())
			}
		})
	}
}

// TestRotateTokenKicksPerRequest: each request kicks once and answers
// 202; coalescing is the refresh loop's job. The request body is ignored
// and nothing is written back.
func TestRotateTokenKicksPerRequest(t *testing.T) {
	h, kicks := newCounting()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	for i := 1; i <= 3; i++ {
		resp, err := srv.Client().Post(srv.URL+"/v1/control/rotate-token", "application/json", strings.NewReader(`{"ignored":true}`))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 202 || len(body) != 0 {
			t.Fatalf("status %d body %q, want 202 and empty", resp.StatusCode, body)
		}
		if *kicks != i {
			t.Fatalf("kicks = %d, want %d", *kicks, i)
		}
	}
}

// TestRoutesExactlyImplemented: the advertised routes are exactly the
// implemented ones, and callers cannot modify the handler's list.
func TestRoutesExactlyImplemented(t *testing.T) {
	h, _ := newCounting()
	got := h.Routes()
	if !slices.Equal(got, []string{RouteRotateToken}) {
		t.Fatalf("Routes = %v, want [%s]", got, RouteRotateToken)
	}
	got[0] = "wake"
	if !slices.Equal(h.Routes(), []string{RouteRotateToken}) {
		t.Fatal("Routes returned the handler's own slice")
	}
	for _, r := range h.Routes() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", Prefix+r, nil))
		if rec.Code == 404 {
			t.Fatalf("advertised route %q is not served", r)
		}
	}
}

func TestNewRequiresKick(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New without KickTokenRefresh did not panic")
		}
	}()
	New(Options{})
}
