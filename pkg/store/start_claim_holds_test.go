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

package store

import (
	"testing"
	"time"
)

func TestStartClaimHolds_For(t *testing.T) {
	h := StartClaimHolds{Default: 17 * time.Minute, Create: 16 * time.Minute, Stop: 5 * time.Minute}
	for kind, want := range map[StartClaimKind]time.Duration{
		StartClaimCreate:   16 * time.Minute,
		StartClaimStop:     5 * time.Minute,
		StartClaimUser:     17 * time.Minute,
		StartClaimRecovery: 17 * time.Minute,
	} {
		if got := h.For(kind); got != want {
			t.Errorf("For(%s) = %s, want %s", kind, got, want)
		}
	}
	// Without a stop hold, a stop claim takes the create hold, as before.
	if got := (StartClaimHolds{Default: time.Hour, Create: 3 * time.Minute}).For(StartClaimStop); got != 3*time.Minute {
		t.Errorf("For(stop) with no Stop = %s, want the create hold", got)
	}
}
