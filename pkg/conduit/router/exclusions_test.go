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

package router

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
)

// TestExclusions: an excluded session rules out that session only; an
// excluded relay rules out every session it holds.
func TestExclusions(t *testing.T) {
	x := exclusions{sessions: map[string]bool{"s1": true}, relays: map[string]bool{"relay-a": true}}
	for _, tc := range []struct {
		session, relay string
		want           bool
	}{
		{session: "s1", relay: "relay-b", want: true},
		{session: "s2", relay: "relay-a", want: true},
		{session: "s3", relay: "relay-a", want: true},
		{session: "s2", relay: "relay-b", want: false},
	} {
		rec := registry.SessionRecord{SessionID: tc.session, RelayInstanceID: tc.relay}
		if got := x.excluded(rec); got != tc.want {
			t.Errorf("excluded(%s on %s) = %v, want %v", tc.session, tc.relay, got, tc.want)
		}
	}
	if (exclusions{}).excluded(registry.SessionRecord{SessionID: "s1", RelayInstanceID: "relay-a"}) {
		t.Error("the zero exclusions excluded a session")
	}
}
