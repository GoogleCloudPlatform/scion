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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// agentPolicyErr is an agent policy refusal from Manager.Start, wrapped as a
// caller might.
func agentPolicyErr(t *testing.T) error {
	t.Helper()
	return fmt.Errorf("start: %w", agent.NewAgentPolicyError("image", "agent_policy.allowed_image_prefixes",
		"image %q is not from an allowed registry or repository", "docker.io/library/postgres:18-alpine"))
}

// An agent policy refusal is answered with 400 agent_policy_refused on every
// synchronous start path, carrying the bounded public message naming the
// field and the policy setting.
func TestAgentPolicyError_DispatchPathsAnswer400(t *testing.T) {
	cases := []struct{ name, path, body, op string }{
		{"create sync start", "/api/v1/agents", `{"name": "p1", "id": "id-p1", "slug": "p1", "requestId": "req-p1", "config": {"template": "none"}}`, "create agent"},
		{"start", "/api/v1/agents/p2/start", `{"runId": "run-p2"}`, "start agent"},
		{"restart", "/api/v1/agents/p3/restart", `{"runId": "run-p3"}`, "restart agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := dispatchTestEnv(t, true)
			srv.manager = &unusableManager{at: "start", err: agentPolicyErr(t)}
			code, body := postJSON(t, srv, tc.path, tc.body)
			if code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", code, body)
			}
			var resp ErrorResponse
			if err := json.Unmarshal([]byte(body), &resp); err != nil {
				t.Fatalf("not an ErrorResponse: %v: %s", err, body)
			}
			if resp.Error.Code != api.LaunchCodeAgentPolicyRefused {
				t.Errorf("code = %q, want %q", resp.Error.Code, api.LaunchCodeAgentPolicyRefused)
			}
			if !strings.HasPrefix(resp.Error.Message, "Failed to "+tc.op+": agent policy: ") ||
				!strings.Contains(resp.Error.Message, "agent_policy.allowed_image_prefixes") {
				t.Errorf("message = %q, want the public agent policy refusal", resp.Error.Message)
			}
		})
	}
}

func TestClassifyStartError_AgentPolicy(t *testing.T) {
	for _, kind := range []string{store.LaunchKindCreate, store.LaunchKindStart} {
		code, _, msg := classifyStartError(context.Background(), agentPolicyErr(t), "", "", kind)
		if code != api.LaunchCodeAgentPolicyRefused {
			t.Errorf("%s: code = %q, want %q", kind, code, api.LaunchCodeAgentPolicyRefused)
		}
		if !strings.HasPrefix(msg, "agent policy: ") || !strings.Contains(msg, "agent_policy.allowed_image_prefixes") {
			t.Errorf("%s: message = %q", kind, msg)
		}
		if strings.Contains(msg, "start: ") {
			t.Errorf("%s: message %q leaked outer wrapper", kind, msg)
		}
	}
}
