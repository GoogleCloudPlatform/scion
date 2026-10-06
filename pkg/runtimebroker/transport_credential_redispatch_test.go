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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// The hub mints a fresh transport credential on every start, resume and
// restart dispatch. These tests pin that the broker hands the value from each
// request to Manager.Start, which calls Runtime.Run; on Kubernetes, Run then
// writes it into the per-agent Secret.
func transportRedispatchBody(t *testing.T, value string, resume bool) string {
	t.Helper()
	body := map[string]any{
		"resolvedEnv": map[string]string{transportauth.EnvTransportToken: value},
		"envClassifications": map[string]api.EnvKind{
			transportauth.EnvTransportToken: api.EnvKindSecretBootstrap,
		},
	}
	if resume {
		body["resume"] = true
	}
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTransportCredential_RedispatchReachesStart(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		resume bool
	}{
		{"start", "/api/v1/agents/test-agent-1/start", false},
		{"resume", "/api/v1/agents/test-agent-1/start", true},
		{"restart", "/api/v1/agents/test-agent-1/restart", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			mgr := srv.manager.(*mockManager)
			fresh := "fresh-" + tc.name

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(transportRedispatchBody(t, fresh, tc.resume)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusAccepted {
				t.Fatalf("expected status %d, got %d: %s", http.StatusAccepted, w.Code, w.Body.String())
			}
			if mgr.startCalls != 1 {
				t.Fatalf("expected Start to be called once, got %d", mgr.startCalls)
			}
			if got := mgr.lastStartOpts.Env[transportauth.EnvTransportToken]; got != fresh {
				t.Errorf("Start received %s=%q, want the value from this request %q", transportauth.EnvTransportToken, got, fresh)
			}
			if tc.resume && !mgr.lastStartOpts.Resume {
				t.Error("expected Resume to be forwarded to Start")
			}
		})
	}
}
