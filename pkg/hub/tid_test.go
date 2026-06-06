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
	"github.com/google/uuid"
)

// tid deterministically maps a human-readable test identifier (e.g. "user-1")
// to a stable UUID string. The Ent-backed store uses UUID primary keys, so test
// fixtures cannot use arbitrary strings as IDs; wrapping a readable name in tid
// preserves test legibility and cross-reference consistency (tid("user-1")
// always returns the same UUID) while satisfying the UUID requirement.
func tid(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}
