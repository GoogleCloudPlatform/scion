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

package hub

import (
	"iter"
	"strings"
)

// matchJSONKey returns the value of the candidate whose name matches key
// the way encoding/json matches an object key to a field name: an exact
// match wins; otherwise the first case-insensitive match in candidates
// order is used. ok is false when no candidate matches.
//
// This is the one rule the admin settings code uses to match JSON keys
// (lookupFold, fieldFold and structFieldByJSONName all call it), so the
// merge, the validator and the unpersisted-field checks agree on which
// key names which field. Callers decide which names are candidates (for
// example how json tags and embedded structs are read); when candidates
// come from a Go map, which of several case-insensitive matches is used
// follows map order.
func matchJSONKey[T any](candidates iter.Seq2[string, T], key string) (v T, ok bool) {
	var folded T
	foundFolded := false
	for name, c := range candidates {
		if name == key {
			return c, true
		}
		if !foundFolded && strings.EqualFold(name, key) {
			folded, foundFolded = c, true
		}
	}
	return folded, foundFolded
}
