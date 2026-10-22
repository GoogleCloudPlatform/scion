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
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// badgeEventCases are agents whose record alone names the harness (or
// does not), with the badge keys their events must carry ("" = omitted).
var badgeEventCases = []struct {
	name         string
	ac           *store.AgentAppliedConfig
	vendor, size string
}{
	{"stamped", &store.AgentAppliedConfig{Harness: "claude", ModelSize: "large"}, "anthropic", "large"},
	{"inline harness, canonicalized", &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Harness: "gemini"}}, "google", ""},
	{"multi-vendor", &store.AgentAppliedConfig{Harness: "hermes", ModelSize: "small"}, "hermes", "small"},
	{"custom harness", &store.AgentAppliedConfig{Harness: "my-harness", ModelSize: "medium"}, "", "medium"},
	// No store access in events: a reference-only harness gets no vendor.
	{"harness-config reference only", &store.AgentAppliedConfig{HarnessConfig: "team-default"}, "", ""},
	{"no applied config", nil, "", ""},
}

// assertBadgeKeys checks the badge keys of one event payload. An empty
// harnessVendor is always omitted. An empty modelSize is omitted on a
// created event, and sent as "" on a status event (sizeAlwaysSent), so the
// web's delta merge clears a size a PATCH or reincarnation removed.
func assertBadgeKeys(t *testing.T, data []byte, vendor, size string, sizeAlwaysSent bool) {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &m))
	raw, present := m["harnessVendor"]
	if vendor == "" {
		assert.False(t, present, "harnessVendor must be omitted, got %s", raw)
	} else {
		assert.JSONEq(t, `"`+vendor+`"`, string(raw), "harnessVendor")
	}
	raw, present = m["modelSize"]
	if size == "" && !sizeAlwaysSent {
		assert.False(t, present, "modelSize must be omitted, got %s", raw)
		return
	}
	require.True(t, present, "modelSize must be sent")
	assert.JSONEq(t, `"`+size+`"`, string(raw), "modelSize")
}

func receiveBadgeEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()
	select {
	case evt := <-ch:
		return evt
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
		return Event{}
	}
}

// TestAgentBadges_CreatedAndStatusEventsCarryBadges pins the badge keys on
// the SSE agent created, restored and status events, on both the agent and
// the project subjects.
func TestAgentBadges_CreatedAndStatusEventsCarryBadges(t *testing.T) {
	for _, tc := range badgeEventCases {
		t.Run(tc.name, func(t *testing.T) {
			pub := NewChannelEventPublisher()
			defer pub.Close()
			ch, unsub := pub.Subscribe("agent.a1.>", "project.p1.agent.>")
			defer unsub()
			agent := &store.Agent{ID: "a1", ProjectID: "p1", Name: "a1", Slug: "a1", Phase: "running", AppliedConfig: tc.ac}

			pub.PublishAgentCreated(context.Background(), agent)
			pub.PublishAgentRestored(context.Background(), agent, time.Now())
			pub.PublishAgentStatus(context.Background(), agent)
			for i := 0; i < 6; i++ {
				evt := receiveBadgeEvent(t, ch)
				t.Run(evt.Subject, func(t *testing.T) {
					assertBadgeKeys(t, evt.Data, tc.vendor, tc.size, strings.HasSuffix(evt.Subject, ".status"))
				})
			}
		})
	}
}

// TestAgentBadges_StatusEventClearsModelSize pins that a status event for
// an agent with no size carries "modelSize":"" verbatim, so a size the web
// merged earlier is cleared.
func TestAgentBadges_StatusEventClearsModelSize(t *testing.T) {
	pub := NewChannelEventPublisher()
	defer pub.Close()
	ch, unsub := pub.Subscribe("agent.a1.status")
	defer unsub()
	agent := &store.Agent{ID: "a1", ProjectID: "p1", Phase: "running",
		AppliedConfig: &store.AgentAppliedConfig{Harness: "claude", Model: "claude-opus-4-8"}}
	pub.PublishAgentStatus(context.Background(), agent)
	evt := receiveBadgeEvent(t, ch)
	assert.Contains(t, string(evt.Data), `"modelSize":""`)
	assert.Contains(t, string(evt.Data), `"harnessVendor":"anthropic"`)
}

// TestAgentBadges_ListBadgesNeedNoStoreForRecordHarness pins that list
// badges for agents whose record names the harness read no store: the
// server here has none, so a lookup would panic.
func TestAgentBadges_ListBadgesNeedNoStoreForRecordHarness(t *testing.T) {
	srv := &Server{}
	agents := []AgentWithCapabilities{
		{Agent: store.Agent{ID: "a", AppliedConfig: &store.AgentAppliedConfig{Harness: "codex", ModelSize: "extra-large"}}},
		{Agent: store.Agent{ID: "b", AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Harness: "grok-build"}}}},
		{Agent: store.Agent{ID: "c", AppliedConfig: &store.AgentAppliedConfig{Harness: "muse-code", ModelSize: "s"}}},
	}
	srv.setAgentListBadges(context.Background(), agents)
	assert.Equal(t, "openai", agents[0].HarnessVendor)
	assert.Equal(t, "extra-large", agents[0].ModelSize)
	assert.Equal(t, "xai", agents[1].HarnessVendor)
	assert.Empty(t, agents[1].ModelSize)
	assert.Equal(t, "meta", agents[2].HarnessVendor)
	assert.Equal(t, "s", agents[2].ModelSize, "the stored size is copied as is")

	// The compact item copies both from the full item.
	c := toCompact(agents[0])
	assert.Equal(t, "openai", c.HarnessVendor)
	assert.Equal(t, "extra-large", c.ModelSize)
}
