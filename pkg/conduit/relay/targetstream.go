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

package relay

import (
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TargetStream identifies a stream on the target's own session: the
// session id and the stream id as the target sees them. A stream renewal
// (AuthRefresh{stream_id}) names the stream this way (design §3.9
// "Delivery").
type TargetStream struct {
	SessionID string
	StreamID  uint32
}

// valid reports whether both ids are set (stream id 0 is never a stream).
func (t TargetStream) valid() bool { return t.SessionID != "" && t.StreamID != 0 }

// targetStreamer is implemented by streams that reach their target through
// another relay: the owner relay reported the target-side ids when it
// accepted the internal hop.
type targetStreamer interface {
	TargetStream() (TargetStream, bool)
}

// TargetOf returns the target-side ids of st, a stream opened on sess.
//
//   - On a session this relay holds (a conduit.LocalSession), the ids are
//     the session's own id and st's id. They are never read from the
//     target's StreamAccept: a principal session's accept carries no
//     trusted target ids, and conduit sessions ignore those fields.
//   - On a stream through another relay, the ids are the ones the owner
//     relay put in the internal hop's StreamAccept.
//
// It returns false when the ids are not known.
func TargetOf(sess conduit.Session, st conduit.Stream) (TargetStream, bool) {
	if ts, ok := st.(targetStreamer); ok {
		return ts.TargetStream()
	}
	if ls, ok := sess.(conduit.LocalSession); ok {
		t := TargetStream{SessionID: ls.Info().SessionID, StreamID: st.ID()}
		return t, t.valid()
	}
	return TargetStream{}, false
}

// hopAcceptTarget reads the target-side ids from an internal hop's
// StreamAccept, sent by the owner relay of session wantSession. Ids naming
// another session are dropped: the owner accepted a stream on the session
// the caller resolved, or it is not reporting that stream.
func hopAcceptTarget(acc *conduitv1.StreamAccept, wantSession string) TargetStream {
	t := TargetStream{SessionID: acc.GetTargetSessionId(), StreamID: acc.GetTargetStreamId()}
	if !t.valid() || t.SessionID != wantSession {
		return TargetStream{}
	}
	return t
}
