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
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExec records the statements a publish issues.
type recordingExec struct {
	mu    sync.Mutex
	stmts []recordedStmt
}

type recordedStmt struct {
	sql  string
	args []any
}

func (r *recordingExec) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stmts = append(r.stmts, recordedStmt{sql: sql, args: args})
	return pgconn.CommandTag{}, nil
}

// conduitNotices returns the conduit channel payloads of the recorded
// statements, and whether each was in the same statement as the global
// notice.
func (r *recordingExec) conduitNotices(t *testing.T) (notices []string, withGlobal []bool) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, st := range r.stmts {
		for i := 0; i+1 < len(st.args); i += 2 {
			if st.args[i] == pgConduitChannel {
				notices = append(notices, st.args[i+1].(string))
				global := false
				for j := 0; j+1 < len(st.args); j += 2 {
					global = global || st.args[j] == pgGlobalChannel
				}
				withGlobal = append(withGlobal, global)
			}
		}
	}
	return notices, withGlobal
}

// bodyWithSecrets is an event body a conduit notice must never carry.
type bodyWithSecrets struct {
	Name  string `json:"name"`
	Token string `json:"token"`
}

// TestConduitChannelPublishesRelaySubjects (design §3.9 "Conduit channel"):
// every subject a relay acts on is notified on the conduit channel in the
// same statement as the global notice, with identifiers only; other
// subjects are not.
func TestConduitChannelPublishesRelaySubjects(t *testing.T) {
	body := bodyWithSecrets{Name: "an entity body", Token: "tok-should-never-cross"}
	for _, tc := range []struct {
		subject string
		event   interface{}
		want    conduitNotice
	}{
		{conduitAuthzChangedSubject, conduitAuthzMatch{UserID: "u1", ProjectID: "p1"}, conduitNotice{Subject: conduitAuthzChangedSubject, UserID: "u1", ProjectID: "p1"}},
		{"agent.a1.deleted", body, conduitNotice{Subject: "agent.a1.deleted", AgentID: "a1"}},
		{"agent.a1.ports", body, conduitNotice{Subject: "agent.a1.ports", AgentID: "a1"}},
		{"project.p1.deleted", body, conduitNotice{Subject: "project.p1.deleted", ProjectID: "p1"}},
		{conduitBrokerChangedSubject, conduitBrokerChange{BrokerID: "b1"}, conduitNotice{Subject: conduitBrokerChangedSubject, BrokerID: "b1"}},
		{settingsUpdatedSubject, SettingsUpdatedEvent{Section: "experiments", Revision: 3}, conduitNotice{Subject: settingsUpdatedSubject, Section: "experiments"}},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			p := newTestPostgresPublisher(nil)
			exec := &recordingExec{}
			require.NoError(t, p.buildAndNotify(context.Background(), exec, tc.subject, tc.event))
			notices, withGlobal := exec.conduitNotices(t)
			require.Len(t, notices, 1, "one conduit notice")
			assert.True(t, withGlobal[0], "in the same statement as the global notice")
			assert.Less(t, len(notices[0]), 1024)
			assert.NotContains(t, notices[0], "tok-should-never-cross")
			assert.NotContains(t, notices[0], "an entity body")
			var got conduitNotice
			dec := json.NewDecoder(strings.NewReader(notices[0]))
			dec.DisallowUnknownFields()
			require.NoError(t, dec.Decode(&got))
			assert.Equal(t, tc.want, got)
		})
	}

	for _, subject := range []string{"agent.a1.status", "agent.a1.message", "user.u1.chat.message.deleted", "project.p1.agent.deleted", "notification.created"} {
		t.Run("not "+subject, func(t *testing.T) {
			p := newTestPostgresPublisher(nil)
			exec := &recordingExec{}
			require.NoError(t, p.buildAndNotify(context.Background(), exec, subject, body))
			notices, _ := exec.conduitNotices(t)
			assert.Empty(t, notices)
		})
	}
}

// TestConduitChannelCoversRelayRechecks: every subject pattern the hub's
// stream re-check acts on, and the broker change subject, is a conduit
// channel subject.
func TestConduitChannelCoversRelayRechecks(t *testing.T) {
	for _, s := range []string{conduitAuthzChangedSubject, "agent.x.deleted", "agent.x.ports", "project.x.deleted", conduitBrokerChangedSubject, settingsUpdatedSubject} {
		assert.True(t, conduitChannelSubject(s), s)
	}
	for _, p := range conduitAuthzEventPatterns {
		concrete := strings.ReplaceAll(p, "*", "x")
		assert.True(t, conduitChannelSubject(concrete), p)
	}
}
