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
	"testing"

	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestHopAcceptTarget: the opening relay keeps the hop accept's target ids
// only when both are set and they name the session it resolved.
func TestHopAcceptTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		acc  *conduitv1.StreamAccept
		want TargetStream
	}{
		{"both set, resolved session", &conduitv1.StreamAccept{TargetSessionId: "s1", TargetStreamId: 7}, TargetStream{SessionID: "s1", StreamID: 7}},
		{"another session", &conduitv1.StreamAccept{TargetSessionId: "s2", TargetStreamId: 7}, TargetStream{}},
		{"no stream id", &conduitv1.StreamAccept{TargetSessionId: "s1"}, TargetStream{}},
		{"no session id", &conduitv1.StreamAccept{TargetStreamId: 7}, TargetStream{}},
		{"neither (an owner that does not report them)", &conduitv1.StreamAccept{}, TargetStream{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hopAcceptTarget(tc.acc, "s1"); got != tc.want {
				t.Fatalf("hopAcceptTarget = %+v, want %+v", got, tc.want)
			}
		})
	}
}
