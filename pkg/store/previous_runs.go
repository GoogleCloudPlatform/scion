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

// MaxPreviousRunIDs bounds Agent.PreviousRunIDs. The list only grows across
// consecutive run-ID writes whose runs never settled (each start failed
// without the broker reporting what its runtime holds), so reaching the cap
// means that many unsettled starts in a row; the oldest run is dropped.
const MaxPreviousRunIDs = 8

// AppendPreviousRunID returns the previous-run list after a run-ID write
// that replaced the run replaced with current (ptone/scion#3097): replaced
// is appended unless it is empty, current, or already listed, and any run
// equal to current is dropped. When the result exceeds MaxPreviousRunIDs
// the oldest runs are dropped and returned as dropped, for the caller to
// log. prev is not modified.
func AppendPreviousRunID(prev []string, replaced, current string) (next, dropped []string) {
	next = make([]string, 0, len(prev)+1)
	seen := make(map[string]bool, len(prev)+1)
	keep := func(r string) {
		if r == "" || r == current || seen[r] {
			return
		}
		seen[r] = true
		next = append(next, r)
	}
	for _, r := range prev {
		keep(r)
	}
	keep(replaced)
	if over := len(next) - MaxPreviousRunIDs; over > 0 {
		dropped = append([]string(nil), next[:over]...)
		next = next[over:]
	}
	if len(next) == 0 {
		next = nil
	}
	return next, dropped
}
