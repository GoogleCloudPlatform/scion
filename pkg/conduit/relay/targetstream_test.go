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

package relay_test

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// acceptingConfig accepts every stream, reports each accepted stream's id
// (the target-side id) on ids, and holds the stream open until the test
// ends.
func acceptingConfig(t *testing.T, ids chan<- uint32) conduit.Config {
	return conduit.Config{
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return err
			}
			ids <- st.ID()
			t.Cleanup(func() { _ = st.Close() })
			return nil
		}),
	}
}

// forgeTargetIDs makes a target put foreign target ids in every
// StreamAccept it sends, as a misbehaving principal session could.
func forgeTargetIDs(cfg conduit.Config) conduit.Config {
	cfg.Interceptor = func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
		if acc := f.GetStreamAccept(); dir == conduit.Outbound && acc != nil {
			acc.TargetSessionId = "forged-session"
			acc.TargetStreamId = 999
		}
		return []*conduitv1.Frame{f}
	}
	return cfg
}

// TestBridgeOpenReturnsTargetSideIDs (design §3.9 Delivery, contracts §3):
// the owner relay's StreamAccept on an internal hop carries the stream's
// target-side session id and stream id, and the opening relay exposes them.
func TestBridgeOpenReturnsTargetSideIDs(t *testing.T) {
	ids := make(chan uint32, 4)
	p := newPair(t, acceptingConfig(t, ids))
	rs := p.remote()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	st, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	targetID := relaytest.Wait(t, ids, "target-side accept")
	got, ok := relay.TargetOf(rs, st)
	want := relay.TargetStream{SessionID: p.rec.SessionID, StreamID: targetID}
	if !ok || got != want {
		t.Fatalf("TargetOf(remote stream) = %+v, %v; want %+v, true", got, ok, want)
	}

	// On the wire: the owner's hop StreamAccept names the target stream.
	c := dialHop(t, p, conduitv1.StreamKind_STREAM_KIND_PTY)
	acc := readHopFrame(t, c).GetStreamAccept()
	hopTargetID := relaytest.Wait(t, ids, "second target-side accept")
	if acc == nil || acc.GetTargetSessionId() != p.rec.SessionID || acc.GetTargetStreamId() != hopTargetID {
		t.Fatalf("hop StreamAccept = %v; want target_session_id %q, target_stream_id %d", acc, p.rec.SessionID, hopTargetID)
	}
}

// TestStreamAcceptTargetIDsIgnoredFromPrincipal (criterion 30): target ids
// in a StreamAccept sent by a principal session are ignored, both on the
// relay holding that session and on a relay reaching it through the hop.
func TestStreamAcceptTargetIDsIgnoredFromPrincipal(t *testing.T) {
	ids := make(chan uint32, 4)
	p := newPair(t, forgeTargetIDs(acceptingConfig(t, ids)))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The relay holding the target session.
	ls, _, ok := p.a.Relay.Local(ctx, p.rec.SessionID)
	if !ok {
		t.Fatal("target session is not local to relay A")
	}
	st, err := ls.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	targetID := relaytest.Wait(t, ids, "target-side accept")
	got, ok := relay.TargetOf(ls, st)
	want := relay.TargetStream{SessionID: p.rec.SessionID, StreamID: targetID}
	if !ok || got != want {
		t.Fatalf("TargetOf(local stream) = %+v, %v; want %+v, true (forged ids must be ignored)", got, ok, want)
	}

	// A relay reaching it through the hop sees the owner's ids, not the
	// target's forged ones.
	rs := p.remote()
	rst, err := rs.OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rst.Close() }()
	hopTargetID := relaytest.Wait(t, ids, "second target-side accept")
	got, ok = relay.TargetOf(rs, rst)
	want = relay.TargetStream{SessionID: p.rec.SessionID, StreamID: hopTargetID}
	if !ok || got != want {
		t.Fatalf("TargetOf(remote stream) = %+v, %v; want %+v, true (forged ids must be ignored)", got, ok, want)
	}
}
