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
	"encoding/json"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// --- Stream J: eventTargetsAgent tests ---

func TestEventTargetsAgent_MatchByID(t *testing.T) {
	agent := &store.Agent{
		ID:   "agent-123",
		Name: "my-agent",
		Slug: "my-agent",
	}
	payload, _ := json.Marshal(map[string]string{"agentId": "agent-123"})
	evt := store.ScheduledEvent{Payload: string(payload)}

	if !eventTargetsAgent(evt, agent) {
		t.Error("expected eventTargetsAgent to match by agent ID")
	}
}

func TestEventTargetsAgent_MatchByName(t *testing.T) {
	agent := &store.Agent{
		ID:   "agent-123",
		Name: "my-agent",
		Slug: "my-agent-slug",
	}
	payload, _ := json.Marshal(map[string]string{"agentName": "my-agent"})
	evt := store.ScheduledEvent{Payload: string(payload)}

	if !eventTargetsAgent(evt, agent) {
		t.Error("expected eventTargetsAgent to match by agent name")
	}
}

func TestEventTargetsAgent_MatchBySlug(t *testing.T) {
	agent := &store.Agent{
		ID:   "agent-123",
		Name: "my-agent",
		Slug: "my-agent-slug",
	}
	payload, _ := json.Marshal(map[string]string{"agentName": "my-agent-slug"})
	evt := store.ScheduledEvent{Payload: string(payload)}

	if !eventTargetsAgent(evt, agent) {
		t.Error("expected eventTargetsAgent to match by agent slug")
	}
}

func TestEventTargetsAgent_NoMatch(t *testing.T) {
	agent := &store.Agent{
		ID:   "agent-123",
		Name: "my-agent",
		Slug: "my-agent",
	}
	payload, _ := json.Marshal(map[string]string{"agentId": "other-agent", "agentName": "other"})
	evt := store.ScheduledEvent{Payload: string(payload)}

	if eventTargetsAgent(evt, agent) {
		t.Error("expected eventTargetsAgent to NOT match a different agent")
	}
}

func TestEventTargetsAgent_EmptyPayload(t *testing.T) {
	agent := &store.Agent{
		ID:   "agent-123",
		Name: "my-agent",
		Slug: "my-agent",
	}
	evt := store.ScheduledEvent{Payload: "{}"}

	if eventTargetsAgent(evt, agent) {
		t.Error("expected eventTargetsAgent to NOT match empty payload")
	}
}

func TestEventTargetsAgent_MalformedPayload(t *testing.T) {
	agent := &store.Agent{
		ID:   "agent-123",
		Name: "my-agent",
		Slug: "my-agent",
	}
	evt := store.ScheduledEvent{Payload: "not valid json"}

	if eventTargetsAgent(evt, agent) {
		t.Error("expected eventTargetsAgent to return false for malformed payload")
	}
}
