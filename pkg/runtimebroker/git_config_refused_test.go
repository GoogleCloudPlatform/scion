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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// A start refused because the repository's git config sets keys agents may
// not be given is answered 422 git_config_refused with the refusal's own text
// on the synchronous create, start and restart, and reported with the same
// code and text by the async launch, instead of the fixed "Failed to start
// agent". The text names every refused key and its remedy; it carries no
// value and none of the wrapping (the agent directory).
const gitConfigRefusedTestDir = "/var/lib/scion/agents/dirmarker-g7"

// gitConfigRefusedErr returns the refusal's own text and a real seed refusal of
// two keys, wrapped as the agent start wraps it.
func gitConfigRefusedErr(t *testing.T) (string, error) {
	t.Helper()
	_, _, _, refusal := util.PrepareSeededGitConfig([]byte("[core]\n\teditor = vi-valmarker-g8\n[mytool]\n\tsetting = 8\n"), util.RepoPolicyLinked, nil, false)
	if refusal == nil {
		t.Fatal("fixture: the config was not refused")
	}
	return refusal.Error(), fmt.Errorf("seed private git config for agent %q (%s): seed private git config: %w", "test-agent-1", gitConfigRefusedTestDir, refusal)
}

func checkGitConfigRefusedText(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("message = %q, want the refusal text %q", got, want)
	}
	if !strings.Contains(got, "core.editor") || !strings.Contains(got, "mytool.setting") {
		t.Errorf("message does not name every refused key: %q", got)
	}
	if strings.Contains(got, "dirmarker-g7") || strings.Contains(got, "valmarker-g8") || strings.Contains(got, "Failed to") {
		t.Errorf("message carries the wrapping, a value or the generic text: %q", got)
	}
}

func TestGitConfigRefused_SyncPaths(t *testing.T) {
	for _, tc := range []struct{ name, path, body string }{
		{"create", "/api/v1/agents", `{"name": "gitcfg-agent", "config": {"template": "claude"}}`},
		{"start", "/api/v1/agents/test-agent-1/start", `{}`},
		{"restart", "/api/v1/agents/test-agent-1/restart", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, startErr := gitConfigRefusedErr(t)
			srv := newTestServer(t)
			srv.manager.(*mockManager).startErr = startErr
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("%s = %d %s, want 422", tc.name, w.Code, w.Body.String())
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode %s: %v", w.Body.String(), err)
			}
			if body.Error.Code != api.BrokerErrCodeGitConfigRefused {
				t.Errorf("code = %q, want %q", body.Error.Code, api.BrokerErrCodeGitConfigRefused)
			}
			checkGitConfigRefusedText(t, body.Error.Message, want)
		})
	}
}

func TestClassifyStartError_GitConfigRefused(t *testing.T) {
	for _, kind := range []string{store.LaunchKindCreate, store.LaunchKindStart} {
		want, startErr := gitConfigRefusedErr(t)
		code, step, message := classifyStartError(context.Background(), startErr, "", api.LaunchStepPreClean, kind)
		if code != api.LaunchCodeGitConfigRefused {
			t.Errorf("%s: code = %q, want %q", kind, code, api.LaunchCodeGitConfigRefused)
		}
		if step != api.LaunchStepPreClean {
			t.Errorf("%s: step = %q, want the last step %q", kind, step, api.LaunchStepPreClean)
		}
		checkGitConfigRefusedText(t, message, want)
	}
}
