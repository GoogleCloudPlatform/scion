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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cspDirective returns the source list of one CSP directive, or nil if
// the policy does not contain it.
func cspDirective(policy, name string) []string {
	for _, d := range strings.Split(policy, ";") {
		fields := strings.Fields(d)
		if len(fields) > 0 && fields[0] == name {
			return fields[1:]
		}
	}
	return nil
}

func containsSource(sources []string, want string) bool {
	for _, s := range sources {
		if s == want {
			return true
		}
	}
	return false
}

// TestSecurityHeadersCSPAllowsBlobImages guards the chat file preview,
// which renders fetched image bytes as <img src="blob:...">. Without
// blob: in img-src the browser blocks every such image.
func TestSecurityHeadersCSPAllowsBlobImages(t *testing.T) {
	ws := &WebServer{}
	h := ws.securityHeadersMiddleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/chat", nil))

	policy := rec.Header().Get("Content-Security-Policy")
	if policy == "" {
		t.Fatal("Content-Security-Policy header not set")
	}

	img := cspDirective(policy, "img-src")
	for _, want := range []string{"'self'", "data:", "blob:", "https:"} {
		if !containsSource(img, want) {
			t.Errorf("img-src %v is missing %s", img, want)
		}
	}

	// blob: is for images only. Script and the default fallback must
	// not pick it up.
	for _, name := range []string{"default-src", "script-src"} {
		if src := cspDirective(policy, name); containsSource(src, "blob:") {
			t.Errorf("%s must not allow blob:, got %v", name, src)
		}
	}
}
