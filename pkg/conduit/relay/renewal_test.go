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
	"io"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay/relaytest"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// renewalAt is one renewal the target received.
type renewalAt struct {
	streamID   uint32
	credential string
}

// TestRemoteRenewalForwarded: a renewal sent on a stream bridged from
// relay B to the owner relay A reaches the target as
// AuthRefresh{stream_id, credential}, with the target's own stream id and
// the credential bytes unchanged, and the stream keeps working (design
// v2.21.17 §3.5, "Target-side deadlines").
func TestRemoteRenewalForwarded(t *testing.T) {
	accepted := make(chan uint32, 1)
	renewals := make(chan renewalAt, 4)
	cfg := conduit.Config{
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return err
			}
			accepted <- st.ID()
			go func() { _, _ = io.Copy(st, st); _ = st.Close() }()
			return nil
		}),
		OnStreamRenewal: func(_ context.Context, streamID uint32, credential []byte) {
			renewals <- renewalAt{streamID, string(credential)}
		},
	}
	p := newPair(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := p.remote().OpenStream(ctx, &conduitv1.StreamOpen{Kind: conduitv1.StreamKind_STREAM_KIND_PTY})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	targetID := relaytest.Wait(t, accepted, "target accept")

	r, ok := st.(conduit.StreamRenewer)
	if !ok {
		t.Fatal("a bridged stream does not implement StreamRenewer")
	}
	if err := r.Renew([]byte("renewal-grant")); err != nil {
		t.Fatal(err)
	}
	got := relaytest.Wait(t, renewals, "forwarded renewal")
	if got.streamID != targetID || got.credential != "renewal-grant" {
		t.Fatalf("target got %+v, want stream %d with the credential unchanged", got, targetID)
	}

	// The stream still carries data both ways.
	go func() { _, _ = st.Write([]byte("ping")) }()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo after the renewal: %q, %v", buf, err)
	}
}
