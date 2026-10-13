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
	"errors"
	"testing"
	"time"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

func nextAuthRefresh(t *testing.T, h *fakeHub) *conduitv1.AuthRefresh {
	t.Helper()
	select {
	case ar := <-h.authRefreshes:
		return ar
	case <-time.After(waitTimeout):
		t.Fatal("no AuthRefresh")
		return nil
	}
}

// TestAgentRefreshAuthBeforeSession: with no session yet there is
// nothing to send to.
func TestAgentRefreshAuthBeforeSession(t *testing.T) {
	a, err := New(Options{HubURL: "http://127.0.0.1:1", AgentID: testAgentID, ProjectID: testProjectID,
		Token: func() string { return "token-1" }})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RefreshAuth("token-2", 1); !errors.Is(err, ErrNoLiveSession) {
		t.Fatalf("RefreshAuth = %v, want ErrNoLiveSession", err)
	}
}

// TestAgentRefreshAuthSendsOnLiveSession: a refreshed token is sent as
// AuthRefresh with no stream id on the live session, which stays up.
func TestAgentRefreshAuthSendsOnLiveSession(t *testing.T) {
	h := newFakeHub(t)
	ready := make(chan struct{}, 4)
	a, _ := startAgent(t, h, func(o *Options) {
		o.OnSession = func(*conduitv1.Welcome) { ready <- struct{}{} }
	})
	s := h.nextSession(t)
	select {
	case <-ready:
	case <-time.After(waitTimeout):
		t.Fatal("session not reported")
	}

	for i, tok := range []string{"token-2", "token-3"} {
		if err := a.RefreshAuth(tok, uint64(i+1)); err != nil {
			t.Fatalf("RefreshAuth(%s): %v", tok, err)
		}
		ar := nextAuthRefresh(t, h)
		if string(ar.GetCredential()) != tok || ar.GetStreamId() != 0 {
			t.Fatalf("AuthRefresh = {%q, stream %d}, want {%q, 0}", ar.GetCredential(), ar.GetStreamId(), tok)
		}
	}
	select {
	case <-s.Done():
		t.Fatalf("session ended after AuthRefresh: %v", s.Err())
	default:
	}
	if n := h.conduitHits.Load(); n != 1 {
		t.Fatalf("conduit dialed %d times, want 1", n)
	}
}

// TestAgentRefreshAuthOrdersBySeq: tokens are sent in refresh order
// (Seq), even when two refreshes produce tokens with the same expiry,
// and results delivered out of refresh order never send an older token
// after a newer one.
func TestAgentRefreshAuthOrdersBySeq(t *testing.T) {
	h := newFakeHub(t)
	ready := make(chan struct{}, 4)
	a, _ := startAgent(t, h, func(o *Options) {
		o.OnSession = func(*conduitv1.Welcome) { ready <- struct{}{} }
	})
	h.nextSession(t)
	select {
	case <-ready:
	case <-time.After(waitTimeout):
		t.Fatal("session not reported")
	}
	sent := func(want string) {
		t.Helper()
		if ar := nextAuthRefresh(t, h); string(ar.GetCredential()) != want {
			t.Fatalf("AuthRefresh = %q, want %q", ar.GetCredential(), want)
		}
	}

	// In refresh order: both are sent, the second last. (Two refreshes
	// in the same second give tokens with the same expiry; only Seq
	// tells them apart.)
	for i, tok := range []string{"first", "second"} {
		if err := a.RefreshAuth(tok, uint64(i+1)); err != nil {
			t.Fatal(err)
		}
		sent(tok)
	}

	// Out of refresh order: the newer result arrives first and is sent;
	// the older one, and a repeat of the newest, are dropped.
	if err := a.RefreshAuth("fourth", 4); err != nil {
		t.Fatal(err)
	}
	sent("fourth")
	for _, seq := range []uint64{3, 4} {
		if err := a.RefreshAuth("stale", seq); err != nil {
			t.Fatalf("RefreshAuth(stale, %d) = %v, want nil", seq, err)
		}
	}
	// The next token the hub sees is the next newer one: nothing stale
	// was sent in between.
	if err := a.RefreshAuth("fifth", 5); err != nil {
		t.Fatal(err)
	}
	sent("fifth")
}
