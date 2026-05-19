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

package playback

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/extras/agent-viz/internal/logparser"
)

func TestNewEngine(t *testing.T) {
	result := &logparser.ParseResult{
		Manifest: logparser.PlaybackManifest{
			Type: "manifest",
			TimeRange: logparser.TimeRange{
				Start: "2026-03-22T16:30:00.000Z",
				End:   "2026-03-22T16:35:00.000Z",
			},
			Agents: []logparser.AgentInfo{
				{ID: "a1", Name: "alpha", Harness: "gemini", Color: "#4e79a7"},
			},
		},
		Events: []logparser.PlaybackEvent{
			{
				Type:      "agent_state",
				Timestamp: "2026-03-22T16:30:01.000Z",
				Data: logparser.AgentStateEvent{
					AgentID:  "a1",
					Phase:    "running",
					Activity: "working",
				},
			},
			{
				Type:      "agent_state",
				Timestamp: "2026-03-22T16:30:05.000Z",
				Data: logparser.AgentStateEvent{
					AgentID:  "a1",
					Activity: "thinking",
				},
			},
		},
	}

	engine, err := NewEngine(result)
	if err != nil {
		t.Fatalf("NewEngine failed: %v", err)
	}
	defer engine.Close()

	// Verify manifest
	m := engine.Manifest()
	if m.Type != "manifest" {
		t.Errorf("expected manifest type, got %s", m.Type)
	}
	if len(m.Agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(m.Agents))
	}
}

func TestPlaybackSpeed(t *testing.T) {
	result := &logparser.ParseResult{
		Manifest: logparser.PlaybackManifest{
			Type: "manifest",
			TimeRange: logparser.TimeRange{
				Start: "2026-03-22T16:30:00.000Z",
				End:   "2026-03-22T16:35:00.000Z",
			},
		},
		Events: []logparser.PlaybackEvent{
			{
				Type:      "agent_state",
				Timestamp: "2026-03-22T16:30:00.000Z",
				Data:      logparser.AgentStateEvent{AgentID: "a1", Activity: "thinking"},
			},
			{
				Type:      "agent_state",
				Timestamp: "2026-03-22T16:30:01.000Z",
				Data:      logparser.AgentStateEvent{AgentID: "a1", Activity: "working"},
			},
		},
	}

	engine, err := NewEngine(result)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	// Set high speed
	engine.SetSpeed(100)

	// Play and collect events
	engine.Play()

	// Should receive events quickly at 100x
	timeout := time.After(2 * time.Second)
	received := 0
	for received < 2 {
		select {
		case <-engine.Events():
			received++
		case <-timeout:
			t.Fatalf("timed out waiting for events, received %d", received)
		}
	}

	if received != 2 {
		t.Errorf("expected 2 events, got %d", received)
	}
}

func TestEngineFilters(t *testing.T) {
	result := &logparser.ParseResult{
		Manifest: logparser.PlaybackManifest{
			Type: "manifest",
			TimeRange: logparser.TimeRange{
				Start: "2026-03-22T16:30:00.000Z",
				End:   "2026-03-22T16:35:00.000Z",
			},
		},
		Events: []logparser.PlaybackEvent{
			{
				Type:      "agent_state",
				Timestamp: "2026-03-22T16:30:00.000Z",
				Data:      logparser.AgentStateEvent{AgentID: "a1", Activity: "thinking"},
			},
			{
				Type:      "message",
				Timestamp: "2026-03-22T16:30:01.000Z",
				Data:      logparser.MessageEvent{Sender: "alpha", Recipient: "beta", MsgType: "instruction"},
			},
			{
				Type:      "agent_state",
				Timestamp: "2026-03-22T16:30:02.000Z",
				Data:      logparser.AgentStateEvent{AgentID: "a2", Activity: "thinking"},
			},
		},
	}

	engine, err := NewEngine(result)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	// Filter to only agent a1
	engine.SetAgentFilter([]string{"a1"})
	engine.SetSpeed(100)
	engine.Play()

	timeout := time.After(2 * time.Second)
	received := 0
	for {
		select {
		case evt := <-engine.Events():
			received++
			// Messages don't have an agentID extracted by the filter, so they pass through
			if evt.Type == "agent_state" {
				data := evt.Data.(logparser.AgentStateEvent)
				if data.AgentID == "a2" {
					t.Error("should not receive events for filtered agent a2")
				}
			}
		case <-timeout:
			goto done
		}
	}
done:
	if received == 0 {
		t.Error("expected at least some events")
	}
}

func TestSeek(t *testing.T) {
	result := &logparser.ParseResult{
		Manifest: logparser.PlaybackManifest{
			Type: "manifest",
			TimeRange: logparser.TimeRange{
				Start: "2026-03-22T16:30:00.000Z",
				End:   "2026-03-22T16:35:00.000Z",
			},
		},
		Events: []logparser.PlaybackEvent{
			{Type: "agent_state", Timestamp: "2026-03-22T16:30:00.000Z", Data: logparser.AgentStateEvent{AgentID: "a1"}},
			{Type: "agent_state", Timestamp: "2026-03-22T16:31:00.000Z", Data: logparser.AgentStateEvent{AgentID: "a1"}},
			{Type: "agent_state", Timestamp: "2026-03-22T16:32:00.000Z", Data: logparser.AgentStateEvent{AgentID: "a1"}},
			{Type: "agent_state", Timestamp: "2026-03-22T16:33:00.000Z", Data: logparser.AgentStateEvent{AgentID: "a1"}},
		},
	}

	engine, err := NewEngine(result)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	// Seek to middle
	engine.Seek("2026-03-22T16:31:30.000Z")

	// Position should be at index 1 (the last event before the seek timestamp)
	engine.mu.Lock()
	pos := engine.position
	engine.mu.Unlock()

	if pos != 1 {
		t.Errorf("expected position 1 after seek, got %d", pos)
	}
}
