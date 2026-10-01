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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestChannelEventPublisher_AgentEventTimestampsAreInstantCorrect covers tz
// task #1 (design §2.1.7, findings "Message SSE event"/"agent events" rows):
// before the fix, events.go:491,502,534 formatted a non-UTC time.Time with a
// layout ending in "Z07:00" without first converting to UTC. For a wall
// clock in a zone ahead of UTC that prints the *local* digits followed by
// the correct numeric offset (fine), but once something upstream assumed the
// value was already UTC and only appended a literal "Z" the printed digits
// were the local wall clock mislabelled as UTC — the wrong instant, not just
// a formatting nit. This test proves instant equality survives a round trip
// through the publisher under a non-UTC *time.Time location, which is what
// callers hand these methods on a hub that has not pinned time.Local (e.g.
// before U1's pin runs, or for any value sourced from a non-UTC location).
// Run this suite under TZ=Asia/Tokyo and TZ=Asia/Kathmandu (dev-common.md);
// both exercise a zone whose offset would otherwise corrupt the instant.
func TestChannelEventPublisher_AgentEventTimestampsAreInstantCorrect(t *testing.T) {
	loc := nonUTCTestLocation(t)

	// A wall clock in a non-UTC zone: the bug class this guards against is
	// "format as if it were already UTC", which silently drops the offset.
	lastActivity := time.Date(2026, 3, 7, 9, 30, 0, 0, loc)
	startedAt := time.Date(2026, 3, 7, 8, 15, 0, 0, loc)
	created := time.Date(2026, 3, 7, 7, 0, 0, 0, loc)

	pub := NewChannelEventPublisher()
	defer pub.Close()

	statusCh, unsub1 := pub.Subscribe("agent.a1.status")
	defer unsub1()
	createdCh, unsub2 := pub.Subscribe("agent.a1.created")
	defer unsub2()

	agent := &store.Agent{
		ID:                "a1",
		ProjectID:         "g1",
		LastActivityEvent: lastActivity,
		StartedAt:         startedAt,
		Created:           created,
	}

	pub.PublishAgentStatus(context.Background(), agent)
	pub.PublishAgentCreated(context.Background(), agent)

	var statusEvt AgentStatusEvent
	select {
	case evt := <-statusCh:
		if err := json.Unmarshal(evt.Data, &statusEvt); err != nil {
			t.Fatalf("unmarshal status event: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for agent status event")
	}
	assertSameInstantAndUTCSuffix(t, "lastActivityEvent", statusEvt.LastActivityEvent, lastActivity)
	if statusEvt.Detail == nil {
		t.Fatal("expected detail to be set")
	}
	assertSameInstantAndUTCSuffix(t, "startedAt", statusEvt.Detail.StartedAt, startedAt)

	var createdEvt AgentCreatedEvent
	select {
	case evt := <-createdCh:
		if err := json.Unmarshal(evt.Data, &createdEvt); err != nil {
			t.Fatalf("unmarshal created event: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for agent created event")
	}
	assertSameInstantAndUTCSuffix(t, "created", createdEvt.Created, created)
}

// TestChannelEventPublisher_UserMessageCreatedAtIsInstantCorrect covers the
// "Message SSE event createdAt" row (design §2.1.7/§2.2): events.go:725 used
// to append a literal ".000Z" to a non-UTC msg.CreatedAt, which is the wrong
// instant, not merely the wrong number of fraction digits.
func TestChannelEventPublisher_UserMessageCreatedAtIsInstantCorrect(t *testing.T) {
	loc := nonUTCTestLocation(t)
	createdAt := time.Date(2026, 3, 7, 12, 34, 56, 789000000, loc)

	pub := NewChannelEventPublisher()
	defer pub.Close()

	ch, unsub := pub.Subscribe("agent.a1.message")
	defer unsub()

	msg := &store.Message{
		ID:          "m1",
		ProjectID:   "g1",
		Sender:      "agent:coder",
		SenderID:    "a1",
		Recipient:   "user:alice",
		RecipientID: "u1",
		AgentID:     "a1",
		CreatedAt:   createdAt,
	}

	pub.PublishUserMessage(context.Background(), msg, nil)

	var evtData UserMessageEvent
	select {
	case evt := <-ch:
		if err := json.Unmarshal(evt.Data, &evtData); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for user message event")
	}

	if evtData.CreatedAt == "" || evtData.CreatedAt[len(evtData.CreatedAt)-1] != 'Z' {
		t.Fatalf("createdAt = %q, want a Z-suffixed RFC3339Nano string", evtData.CreatedAt)
	}
	got, err := time.Parse(time.RFC3339Nano, evtData.CreatedAt)
	if err != nil {
		t.Fatalf("parsing createdAt %q: %v", evtData.CreatedAt, err)
	}
	if !got.Equal(createdAt) {
		t.Fatalf("createdAt round-tripped to a different instant: got %v, want %v", got, createdAt)
	}
}

// nonUTCTestLocation returns a fixed zone 9 hours ahead of UTC (Tokyo's
// standard offset, with no DST to worry about), so these tests exercise the
// same defect class as TZ=Asia/Tokyo/TZ=Asia/Kathmandu runs without
// depending on the process's TZ environment variable or tzdata lookups.
func nonUTCTestLocation(t *testing.T) *time.Location {
	t.Helper()
	return time.FixedZone("JST", 9*60*60)
}

// assertSameInstantAndUTCSuffix parses a "2006-01-02T15:04:05Z07:00"
// formatted field, requires it ends in a literal Z (i.e. it is UTC, not
// merely correctly offset), and asserts it names the same instant as want.
func assertSameInstantAndUTCSuffix(t *testing.T, field, got string, want time.Time) {
	t.Helper()
	if got == "" {
		t.Fatalf("%s: empty, want a formatted timestamp", field)
	}
	if got[len(got)-1] != 'Z' {
		t.Fatalf("%s = %q, want a Z-suffixed (UTC) timestamp", field, got)
	}
	parsed, err := time.Parse("2006-01-02T15:04:05Z07:00", got)
	if err != nil {
		t.Fatalf("%s: parsing %q: %v", field, got, err)
	}
	if !parsed.Equal(want) {
		t.Fatalf("%s round-tripped to a different instant: got %v, want %v", field, parsed, want)
	}
}
