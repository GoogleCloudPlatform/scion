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

package conduit

import (
	"context"
	"testing"
	"time"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestWelcomeFromContext: a dialer-side StreamHandler sees the Welcome of
// the session the StreamOpen arrived on; relay-side handlers and unrelated
// contexts see nil.
func TestWelcomeFromContext(t *testing.T) {
	if w := WelcomeFromContext(context.Background()); w != nil {
		t.Fatalf("background context: %v", w)
	}
	type seen struct{ w *conduitv1.Welcome }
	dialerSeen := make(chan seen, 1)
	relaySeen := make(chan seen, 1)
	capture := func(ch chan<- seen) StreamHandler {
		return StreamHandlerFunc(func(ctx context.Context, _ *conduitv1.StreamOpen, ps PendingStream) error {
			ch <- seen{WelcomeFromContext(ctx)}
			_, err := ps.Accept()
			return err
		})
	}
	p := newPair(t, Config{StreamHandler: capture(dialerSeen)}, Config{StreamHandler: capture(relaySeen)})

	if _, err := p.relay.OpenStream(context.Background(), tcpOpen()); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-dialerSeen:
		if s.w.GetSessionId() != "sess-1" || s.w.GetConnectionEpoch() != 7 {
			t.Fatalf("dialer handler saw %v, want the session's Welcome", s.w)
		}
	case <-time.After(waitTimeout):
		t.Fatal("dialer handler not called")
	}

	if _, err := p.dialer.OpenStream(context.Background(), tcpOpen()); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-relaySeen:
		if s.w != nil {
			t.Fatalf("relay handler saw %v, want nil", s.w)
		}
	case <-time.After(waitTimeout):
		t.Fatal("relay handler not called")
	}
}
