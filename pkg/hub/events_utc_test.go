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

// TestChannelEventPublisher_AgentEventTimestampsAreInstantCorrect covers
// tz-refactor task 1 (design §2.1.7/§2.2, "agent events" row):
// events.go:491,502,534 used to call .Format("2006-01-02T15:04:05Z07:00")
// on agent.LastActivityEvent/StartedAt/Created without first converting to
// UTC. Before the fix, a non-UTC time.Time printed its local digits
// followed by the correct numeric offset (e.g. "...+09:00") -- a correct
// instant, but one that breaks the §2.2 wire contract (every timestamp on
// the wire is UTC with a literal "Z"). This test proves instant equality
// survives a round trip through the publisher under several non-UTC
// *time.Time locations, which is what callers hand these methods on a hub
// that has not pinned time.Local (e.g. before the U1 pin runs, or for any
// value sourced from a non-UTC location).
func TestChannelEventPublisher_AgentEventTimestampsAreInstantCorrect(t *testing.T) {
	for _, fx := range locationsUnderTest() {
		t.Run(fx.label, func(t *testing.T) {
			loc := fx.loc

			// A wall clock in a non-UTC zone: the bug class this guards
			// against is "format as if it were already UTC", which silently
			// drops the offset.
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
		})
	}
}

// TestChannelEventPublisher_UserMessageCreatedAtIsInstantCorrect covers the
// "Message SSE event createdAt" row (design §2.1.7/§2.2). Before the fix,
// events.go:725 appended a literal ".000Z" to a non-UTC msg.CreatedAt
// without first converting to UTC: unlike the agent-event sites above, that
// printed the *wrong instant* (local wall-clock digits mislabelled as UTC),
// not merely a contract violation, because the literal "Z" discarded the
// real offset instead of reporting it.
func TestChannelEventPublisher_UserMessageCreatedAtIsInstantCorrect(t *testing.T) {
	for _, fx := range locationsUnderTest() {
		t.Run(fx.label, func(t *testing.T) {
			loc := fx.loc
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
		})
	}
}

// tzFixture names a *time.Location for a t.Run subtest label.
type tzFixture struct {
	label string
	loc   *time.Location
}

// locationsUnderTest returns the non-UTC *time.Time locations the two tests
// above run against: a named zone with an hour offset, a nameless zone with
// a sub-hour offset (Kathmandu-style: a nameless, sub-hour zone prints a
// four-digit numeric abbreviation such as "+0545", which a naive
// "-0700 MST"-style parser rejects), and time.Local itself.
//
// A fixed-zone *time.Time's location is independent of the process's TZ
// environment variable, so the two fixed zones give fixed, CI-independent
// coverage of the bug class under any TZ, including plain `go test` with no
// TZ override. Including time.Local as a third case additionally exercises
// the production shape: a value from an unpinned time.Now(), or from store
// read-back before the SQLite store boundary fix (tz-refactor task 2)
// lands -- both of which depend on the process's TZ.
func locationsUnderTest() []tzFixture {
	return []tzFixture{
		{"JST(+9h,named)", time.FixedZone("JST", 9*60*60)},
		{"Kathmandu-style(+5:45,nameless)", time.FixedZone("", 5*60*60+45*60)},
		{"time.Local", time.Local},
	}
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
