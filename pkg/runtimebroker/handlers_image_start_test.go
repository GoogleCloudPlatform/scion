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

package runtimebroker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStartAndRestart_ExplicitImageBecomesTopTier pins ptone/scion#1799 on
// the broker: the hub sends the user's explicit image as the start/restart
// body's "image", and the broker must apply it as opts.Image (the top tier,
// the same slot create's Config.Image fills). A body without it (no explicit
// image, or an older hub) leaves opts.Image empty, so Start resolves the
// image from the template / profile override / settings tiers.
func TestStartAndRestart_ExplicitImageBecomesTopTier(t *testing.T) {
	for _, path := range []string{"/api/v1/agents/test-agent-1/start", "/api/v1/agents/test-agent-1/restart"} {
		for _, tc := range []struct {
			name string
			body string
			want string
		}{
			{"explicit image", `{"resolvedEnv": {"FOO": "bar"}, "image": "user-image:v2"}`, "user-image:v2"},
			{"no image", `{"resolvedEnv": {"FOO": "bar"}}`, ""},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				srv := newTestServer(t)
				mgr := srv.manager.(*mockManager)

				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusAccepted {
					t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
				}
				if mgr.startCalls != 1 {
					t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
				}
				if got := mgr.lastStartOpts.Image; got != tc.want {
					t.Errorf("opts.Image = %q, want %q", got, tc.want)
				}
			})
		}
	}
}
