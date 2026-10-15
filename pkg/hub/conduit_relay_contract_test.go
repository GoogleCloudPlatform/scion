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
	"log/slog"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"
	"github.com/GoogleCloudPlatform/scion/pkg/store/relaydb"
)

// TestSeparateRelayEventContract: a separate relay, which does not import
// this package, re-checks on exactly the hub's revocation subjects, re-reads
// hub.conduit on the hub's settings event, and LISTENs on the conduit
// channel, where the hub publishes exactly the subjects the relay acts on.
func TestSeparateRelayEventContract(t *testing.T) {
	want := append(append([]string(nil), conduitAuthzEventPatterns...), conduitBrokerChangedSubject)
	got := append([]string(nil), relayserver.RevocationSubjects...)
	sort.Strings(want)
	sort.Strings(got)
	assert.Equal(t, want, got)
	assert.Equal(t, settingsUpdatedSubject, relayserver.SettingsUpdatedSubject)

	relayActsOn := append(append([]string(nil), relayserver.RevocationSubjects...), relayserver.SettingsUpdatedSubject)
	published := conduitChannelSubjects()
	sort.Strings(relayActsOn)
	sort.Strings(published)
	assert.Equal(t, relayActsOn, published, "the conduit channel carries exactly what the relay acts on")
	assert.Equal(t, pgConduitChannel, relaydb.ConduitChannel)
	assert.NotEqual(t, pgGlobalChannel, relaydb.ConduitChannel)
}

// TestSeparateRelayListenerReceivesHubEvents: the relay's LISTEN source
// delivers the hub publisher's revocation and settings events, from the
// conduit channel, and nothing else. Needs SCION_TEST_POSTGRES_DSN.
func TestSeparateRelayListenerReceivesHubEvents(t *testing.T) {
	dsn := os.Getenv("SCION_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set SCION_TEST_POSTGRES_DSN to run the relay listener contract test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pub, err := NewPostgresEventPublisher(ctx, dsn, nil, nil)
	require.NoError(t, err)
	t.Cleanup(pub.Close)

	subjects := append(append([]string(nil), relayserver.RevocationSubjects...), relayserver.SettingsUpdatedSubject)
	l, err := relaydb.NewListener(dsn, subjects, slog.Default())
	require.NoError(t, err)
	t.Cleanup(l.Close)
	listening := make(chan struct{}, 1)
	l.AddOnListen(func() {
		select {
		case listening <- struct{}{}:
		default:
		}
	})
	events, unsubscribe := l.Subscribe(subjects...)
	defer unsubscribe()
	l.Start()
	select {
	case <-listening:
	case <-ctx.Done():
		t.Fatal("the relay listener never listened")
	}

	// A lost LISTEN connection reconnects and re-runs the listen hooks,
	// which re-check everything (a notice missed meanwhile is covered).
	// This terminates every backend with the listener's application_name,
	// so SCION_TEST_POSTGRES_DSN must name a dedicated test database.
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(context.Background()) }()
	_, err = conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = $1 AND pid <> pg_backend_pid()`, relaydb.ListenerApplicationName)
	require.NoError(t, err)
	select {
	case <-listening:
	case <-ctx.Done():
		t.Fatal("the relay listener did not reconnect")
	}

	pub.PublishRaw("agent.a1.status", map[string]string{"x": "y"}) // not delivered
	for _, subject := range []string{"agent.a1.deleted", "project.p1.deleted", conduitAuthzChangedSubject, conduitBrokerChangedSubject, settingsUpdatedSubject} {
		pub.PublishRaw(subject, map[string]string{"x": "y"})
		select {
		case got := <-events:
			assert.Equal(t, subject, got)
		case <-ctx.Done():
			t.Fatalf("%s not delivered", subject)
		}
	}
}
