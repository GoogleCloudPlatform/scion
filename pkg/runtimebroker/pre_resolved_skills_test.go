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
	"testing"
)

// The Hub sends the skills it resolved at dispatch (#1784) under
// "preResolvedSkills" in the same shape as its /skills/resolve response.
func TestCreateAgentRequest_DecodesPreResolvedSkills(t *testing.T) {
	body := `{
		"id": "a1", "name": "agent", "slug": "agent", "projectId": "p1",
		"preResolvedSkills": {
			"resolved": [{
				"uri": "skill://scion/global/private@latest",
				"name": "private",
				"resolvedVersion": "1.0.0",
				"contentHash": "sha256:abc",
				"files": [{"path": "SKILL.md", "url": "/api/v1/skills/s1/files/SKILL.md?raw=1", "size": 3, "hash": "sha256:f"}]
			}],
			"errors": [{"uri": "skill://scion/global/denied", "code": "forbidden", "message": "denied"}]
		}
	}`
	var req CreateAgentRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pre := req.PreResolvedSkills
	if pre == nil {
		t.Fatal("PreResolvedSkills not decoded")
	}
	if len(pre.Resolved) != 1 || pre.Resolved[0].ResolvedVersion != "1.0.0" || len(pre.Resolved[0].Files) != 1 {
		t.Errorf("unexpected resolved: %+v", pre.Resolved)
	}
	if len(pre.Errors) != 1 || pre.Errors[0].Code != "forbidden" {
		t.Errorf("unexpected errors: %+v", pre.Errors)
	}

	var legacy CreateAgentRequest
	if err := json.Unmarshal([]byte(`{"id":"a1","name":"agent"}`), &legacy); err != nil {
		t.Fatalf("unmarshal legacy: %v", err)
	}
	if legacy.PreResolvedSkills != nil {
		t.Error("requests from older Hubs must leave PreResolvedSkills nil")
	}
}

func TestPreResolvedHubEndpoint(t *testing.T) {
	if got := preResolvedHubEndpoint(&HubConnection{HubEndpoint: "http://conn"}, "http://advertised"); got != "http://conn" {
		t.Errorf("connection endpoint should win, got %q", got)
	}
	if got := preResolvedHubEndpoint(&HubConnection{}, "http://advertised"); got != "http://advertised" {
		t.Errorf("empty connection endpoint should fall back, got %q", got)
	}
	if got := preResolvedHubEndpoint(nil, "http://advertised"); got != "http://advertised" {
		t.Errorf("nil connection should fall back, got %q", got)
	}
}
