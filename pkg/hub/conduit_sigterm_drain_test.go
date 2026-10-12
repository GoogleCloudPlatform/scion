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
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dialRecordingGoAway opens the launched agent's conduit session and
// records the GoAway frames it receives.
func dialRecordingGoAway(t *testing.T, f *relayFixture) (conduit.LocalSession, *conduitv1.Welcome, <-chan *conduitv1.GoAway) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	token := f.agentToken(t, f.launched)
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(f.public.URL, "http") + "/api/v1/conduit",
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"X-Scion-Agent-Token": {token}}, nil
		},
	}
	goAways := make(chan *conduitv1.GoAway, 4)
	cfg := conduit.Config{Clock: clock.Real(), Interceptor: func(dir conduit.Direction, fr *conduitv1.Frame) []*conduitv1.Frame {
		if g := fr.GetGoAway(); dir == conduit.Inbound && g != nil {
			select {
			case goAways <- g:
			default:
			}
		}
		return []*conduitv1.Frame{fr}
	}}
	hello := &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		PrincipalId:   f.launched.ID,
		Capabilities:  &conduitv1.Capabilities{StreamKinds: []string{"pty"}, EndpointIncarnation: f.launched.RunID},
	}
	s, w, err := conduit.Dial(ctx, d, cfg, hello)
	require.NoError(t, err)
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })
	return ls, w, goAways
}

// TestConduitSIGTERMDrain_GoAwayBeforeListenerClose (criterion 26):
// DrainConduitForShutdown, which the SIGTERM handler runs before it
// cancels the server context, returns only once the relay has sent GoAway
// on every session. The listener is closed after it returns, as the
// server does, and the agent still receives the GoAway: with
// shutdown_grace = 10s its drain_deadline_ms is 8000; with shutdown_grace
// unset it is the unchanged 30000.
func TestConduitSIGTERMDrain_GoAwayBeforeListenerClose(t *testing.T) {
	for _, tc := range []struct {
		name     string
		grace    time.Duration
		capDrain bool
		wantMs   uint32
	}{
		{name: "grace 10s", grace: 10 * time.Second, capDrain: true, wantMs: 8000},
		{name: "grace 1s", grace: time.Second, capDrain: true, wantMs: 1},
		{name: "grace unset", wantMs: 30000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayFixture(t, nil)
			target, wel, goAways := dialRecordingGoAway(t, f)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			held, _, ok := f.srv.conduit.Load().relay.Local(ctx, wel.GetSessionId())
			require.True(t, ok, "session not held by the relay")

			f.srv.DrainConduitForShutdown(tc.grace, tc.capDrain)
			// The hook point: GoAway is out before any listener closes.
			require.True(t, held.Info().Draining, "DrainConduitForShutdown returned before GoAway was sent")
			f.public.Close()

			select {
			case g := <-goAways:
				assert.Equal(t, tc.wantMs, g.GetDrainDeadlineMs())
				assert.Equal(t, conduit.CloseRelayRestart, g.GetCode())
			case <-ctx.Done():
				t.Fatal("agent did not receive GoAway")
			}
			select {
			case <-target.GoAwayReceived():
			case <-ctx.Done():
				t.Fatal("agent session did not see GoAway")
			}
		})
	}
}

// TestConduitSIGTERMDrain_NoRelay: without a relay the call is a no-op.
func TestConduitSIGTERMDrain_NoRelay(t *testing.T) {
	f := newConduitFixture(t)
	f.srv.DrainConduitForShutdown(10*time.Second, true)
}
