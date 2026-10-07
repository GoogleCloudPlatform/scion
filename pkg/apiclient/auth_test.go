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

package apiclient

import (
	"net/http"
	"testing"
)

func TestAgentTokenAuthApplyAuth(t *testing.T) {
	tests := []struct {
		name      string
		auth      AgentTokenAuth
		wantToken string
		wantRunID string
	}{
		{name: "token and run id", auth: AgentTokenAuth{Token: "tok", RunID: "run-1"}, wantToken: "tok", wantRunID: "run-1"},
		{name: "token only", auth: AgentTokenAuth{Token: "tok"}, wantToken: "tok"},
		{name: "run id without token", auth: AgentTokenAuth{RunID: "run-1"}},
		{name: "empty", auth: AgentTokenAuth{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, "http://hub.example/api/v1/agents", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.auth.ApplyAuth(req); err != nil {
				t.Fatalf("ApplyAuth: %v", err)
			}
			if got := req.Header.Get("X-Scion-Agent-Token"); got != tt.wantToken {
				t.Errorf("X-Scion-Agent-Token = %q, want %q", got, tt.wantToken)
			}
			_, present := req.Header["X-Scion-Run-Id"]
			if got := req.Header.Get("X-Scion-Run-Id"); got != tt.wantRunID || present != (tt.wantRunID != "") {
				t.Errorf("X-Scion-Run-Id = %q (present %v), want %q", got, present, tt.wantRunID)
			}
		})
	}
}
