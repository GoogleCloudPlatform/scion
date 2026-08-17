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

//go:build !no_sqlite

package hub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestChatNotificationSubject_CrossReplicaChannel closes the loop the R1
// commit opened. Moving chat notifications off notification.created changed
// the subject, and the Postgres publisher routes by subject: it maps each one
// to a channel, and a subscriber independently maps its pattern to a channel.
// If those two disagree the event is published where nobody is listening —
// which no single-replica test would notice, because the in-process channel
// publisher never consults a channel map at all.
//
// It resolves by rule rather than by table (only project prefixes get a
// per-project channel; everything else goes global), so no entry was needed for
// the new subject. Measured here rather than reasoned about.
func TestChatNotificationSubject_CrossReplicaChannel(t *testing.T) {
	const subject = "user.user-alice.notification"

	assert.Equal(t, []string{pgGlobalChannel}, channelsForSubject(subject),
		"publisher must put the chat notification on the global channel")
	assert.Equal(t, []string{pgGlobalChannel}, channelsForPattern(subject),
		"a subscriber to that subject must LISTEN on the same channel, or the event lands where nobody is")

	p := newTestPostgresPublisher(nil)
	ch, unsub := p.Subscribe(subject)
	defer unsub()

	// A second replica's session belonging to somebody else. It resolves to
	// the same global channel — the channel is a firehose, and the scoping is
	// done by pattern matching inside fanout, not by channel separation.
	otherCh, otherUnsub := p.Subscribe("user.user-eve.notification")
	defer otherUnsub()

	evt := Event{Subject: subject, Data: []byte(`{"status":"DM_RECEIVED","preview":"quiet please"}`)}
	for _, channel := range channelsForSubject(subject) {
		p.fanout(channel, evt)
	}

	select {
	case got := <-ch:
		assert.Equal(t, subject, got.Subject)
	case <-time.After(time.Second):
		t.Fatal("chat notification never crossed replicas: no subscriber on the channel it was published to")
	}

	select {
	case got := <-otherCh:
		t.Fatalf("chat notification crossed replicas to the wrong user: %q", got.Subject)
	case <-time.After(100 * time.Millisecond):
	}
}
