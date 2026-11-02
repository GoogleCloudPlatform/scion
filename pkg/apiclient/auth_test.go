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

func TestAuth_NilReceivers(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://hub.example/api", nil)

	var b *BearerAuth
	if err := b.ApplyAuth(req); err != nil {
		t.Errorf("BearerAuth(nil).ApplyAuth: %v", err)
	}

	var br *BrokerTokenAuth
	if err := br.ApplyAuth(req); err != nil {
		t.Errorf("BrokerTokenAuth(nil).ApplyAuth: %v", err)
	}

	var ag *AgentTokenAuth
	if err := ag.ApplyAuth(req); err != nil {
		t.Errorf("AgentTokenAuth(nil).ApplyAuth: %v", err)
	}
}

func TestAuth_NilHeaders(t *testing.T) {
	req1 := &http.Request{}
	b := &BearerAuth{Token: "bearer-token"}
	if err := b.ApplyAuth(req1); err != nil {
		t.Fatalf("BearerAuth.ApplyAuth with nil header: %v", err)
	}
	if got := req1.Header.Get("Authorization"); got != "Bearer bearer-token" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer bearer-token")
	}

	req2 := &http.Request{}
	br := &BrokerTokenAuth{Token: "broker-token"}
	if err := br.ApplyAuth(req2); err != nil {
		t.Fatalf("BrokerTokenAuth.ApplyAuth with nil header: %v", err)
	}
	if got := req2.Header.Get("X-Scion-Broker-Token"); got != "broker-token" {
		t.Errorf("X-Scion-Broker-Token = %q, want %q", got, "broker-token")
	}

	req3 := &http.Request{}
	ag := &AgentTokenAuth{Token: "agent-token", RunID: "run-42"}
	if err := ag.ApplyAuth(req3); err != nil {
		t.Fatalf("AgentTokenAuth.ApplyAuth with nil header: %v", err)
	}
	if got := req3.Header.Get("X-Scion-Agent-Token"); got != "agent-token" {
		t.Errorf("X-Scion-Agent-Token = %q, want %q", got, "agent-token")
	}
	if got := req3.Header.Get("X-Scion-Run-Id"); got != "run-42" {
		t.Errorf("X-Scion-Run-Id = %q, want %q", got, "run-42")
	}
}

func TestAuth_NilRequest(t *testing.T) {
	b := &BearerAuth{Token: "tok"}
	if err := b.ApplyAuth(nil); err == nil {
		t.Error("BearerAuth.ApplyAuth(nil) want error, got nil")
	}

	br := &BrokerTokenAuth{Token: "tok"}
	if err := br.ApplyAuth(nil); err == nil {
		t.Error("BrokerTokenAuth.ApplyAuth(nil) want error, got nil")
	}

	ag := &AgentTokenAuth{Token: "tok"}
	if err := ag.ApplyAuth(nil); err == nil {
		t.Error("AgentTokenAuth.ApplyAuth(nil) want error, got nil")
	}
}
