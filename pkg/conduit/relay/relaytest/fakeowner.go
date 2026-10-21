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

package relaytest

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// FakeOwner stands in for an owner relay's internal API. It accepts every
// stream hop with a StreamAccept that carries Accept's target ids, as an
// older owner (no ids) or a misbehaving one (ids for another session)
// would, and reports what the caller sent. It checks no relay-peer
// signature: it exists to test what the caller does with the accept.
type FakeOwner struct {
	// URL is the internal endpoint to register for the owner relay.
	URL string
	// Opens receives the StreamOpen of each hop.
	Opens chan *conduitv1.StreamOpen
	// Closes receives the StreamClose the caller sent on each hop after
	// the accept (nil when the link ended without one).
	Closes chan *conduitv1.StreamClose

	accept atomic.Pointer[conduitv1.StreamAccept]
}

// StartFakeOwner starts a FakeOwner whose accepts carry targetSession and
// targetStream (both empty: no target ids).
func StartFakeOwner(t testing.TB, targetSession string, targetStream uint32) *FakeOwner {
	t.Helper()
	f := &FakeOwner{
		Opens:  make(chan *conduitv1.StreamOpen, 16),
		Closes: make(chan *conduitv1.StreamClose, 16),
	}
	f.SetAccept(targetSession, targetStream)
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	f.URL = srv.URL
	return f
}

// SetAccept changes the target ids later accepts carry.
func (f *FakeOwner) SetAccept(targetSession string, targetStream uint32) {
	f.accept.Store(&conduitv1.StreamAccept{TargetSessionId: targetSession, TargetStreamId: targetStream})
}

func (f *FakeOwner) serve(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, relay.InternalPathPrefix) || !strings.HasSuffix(r.URL.Path, "/stream") {
		http.NotFound(w, r)
		return
	}
	conn, err := ws.Upgrade(w, r, nil, ws.Options{})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	b, err := conn.ReadFrame()
	if err != nil {
		return
	}
	in := &conduitv1.Frame{}
	if proto.Unmarshal(b, in) != nil || in.GetStreamOpen() == nil {
		return
	}
	f.Opens <- in.GetStreamOpen()
	tmpl := f.accept.Load()
	acc := &conduitv1.StreamAccept{
		StreamId:        in.GetStreamOpen().GetStreamId(),
		InitialWindow:   1 << 20,
		TargetSessionId: tmpl.GetTargetSessionId(),
		TargetStreamId:  tmpl.GetTargetStreamId(),
	}
	out, err := proto.Marshal(&conduitv1.Frame{Body: &conduitv1.Frame_StreamAccept{StreamAccept: acc}})
	if err != nil || conn.WriteFrame(out) != nil {
		return
	}
	for {
		b, err := conn.ReadFrame()
		if err != nil {
			f.Closes <- nil
			return
		}
		fr := &conduitv1.Frame{}
		if proto.Unmarshal(b, fr) != nil {
			f.Closes <- nil
			return
		}
		if c := fr.GetStreamClose(); c != nil {
			f.Closes <- c
			return
		}
	}
}
