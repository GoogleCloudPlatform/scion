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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// TestStreamKindsMatchGrant pins the one stream-kind list: each kind's
// wire string, its grant string and its proto enum, and that the session
// layer and the grant layer accept exactly the same kinds.
func TestStreamKindsMatchGrant(t *testing.T) {
	tests := []struct {
		kind  StreamKind
		wire  string
		grant string
		proto conduitv1.StreamKind
	}{
		{StreamTCP, "tcp", grant.StreamKindTCP, conduitv1.StreamKind_STREAM_KIND_TCP},
		{StreamPTY, "pty", grant.StreamKindPTY, conduitv1.StreamKind_STREAM_KIND_PTY},
		{StreamSSH, "ssh", grant.StreamKindSSH, conduitv1.StreamKind_STREAM_KIND_SSH},
		{StreamLogs, "logs", grant.StreamKindLogs, conduitv1.StreamKind_STREAM_KIND_LOGS},
		{StreamEvents, "events", grant.StreamKindEvents, conduitv1.StreamKind_STREAM_KIND_EVENTS},
	}
	for _, tt := range tests {
		t.Run(tt.wire, func(t *testing.T) {
			if string(tt.kind) != tt.wire || tt.grant != tt.wire {
				t.Fatalf("kind %q, grant %q, want %q", tt.kind, tt.grant, tt.wire)
			}
			if got := tt.kind.Proto(); got != tt.proto {
				t.Fatalf("Proto() = %v, want %v", got, tt.proto)
			}
			if !grant.IsStreamKind(string(tt.kind)) {
				t.Fatalf("grant.IsStreamKind(%q) = false", tt.kind)
			}
		})
	}
	// Every wire enum except UNSPECIFIED maps to a kind the grant layer
	// accepts, and the table above lists them all.
	n := 0
	for v := range conduitv1.StreamKind_name {
		p := conduitv1.StreamKind(v)
		if p == conduitv1.StreamKind_STREAM_KIND_UNSPECIFIED {
			continue
		}
		n++
		k, err := StreamKindFromProto(p)
		if err != nil {
			t.Fatalf("StreamKindFromProto(%v): %v", p, err)
		}
		if !grant.IsStreamKind(string(k)) {
			t.Fatalf("grant.IsStreamKind(%q) = false for wire kind %v", k, p)
		}
	}
	if n != len(tests) || len(streamToProto) != len(tests) {
		t.Fatalf("wire kinds %d, mapped kinds %d, table %d: keep the lists in step", n, len(streamToProto), len(tests))
	}
}
