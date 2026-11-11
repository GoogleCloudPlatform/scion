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

// TestStartRestart_TemplateNameIsNamingOnly checks that the templateName
// sent on start and restart sets StartOptions.TemplateName and never the
// template that is loaded (StartOptions.Template stays empty, so a broker
// without the agent directory provisions exactly as it would without the
// field). A content hash is not taken as a name.
func TestStartRestart_TemplateNameIsNamingOnly(t *testing.T) {
	hash := "sha256:" + strings.Repeat("ef", 32)
	for _, verb := range []string{"start", "restart"} {
		for _, tt := range []struct {
			name, sent, want string
		}{
			{name: "slug", sent: "web-dev", want: "web-dev"},
			{name: "content hash", sent: hash, want: ""},
			{name: "absent", sent: "", want: ""},
		} {
			t.Run(verb+"/"+tt.name, func(t *testing.T) {
				srv := newTestServer(t)
				mgr := srv.manager.(*mockManager)

				body := `{}`
				if tt.sent != "" {
					body = `{"templateName":"` + tt.sent + `"}`
				}
				req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/test-agent-1/"+verb, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.Handler().ServeHTTP(w, req)

				if w.Code != http.StatusAccepted {
					t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
				}
				if mgr.StartCalls() != 1 {
					t.Fatalf("Start calls = %d, want 1", mgr.StartCalls())
				}
				opts := mgr.LastStartOpts()
				if opts.TemplateName != tt.want {
					t.Errorf("opts.TemplateName = %q, want %q", opts.TemplateName, tt.want)
				}
				if opts.Template != "" {
					t.Errorf("opts.Template = %q, want empty: the slug must not select a template to load", opts.Template)
				}
			})
		}
	}
}
