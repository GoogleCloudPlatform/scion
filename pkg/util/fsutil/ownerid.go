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

package fsutil

import "fmt"

// MaxOwnerID is the largest uid or gid ValidateOwnerID accepts. uid_t and
// gid_t are 32-bit unsigned on Linux, and the all-ones value 4294967295 is
// (uid_t)-1, which chown and lchown read as "leave unchanged", so it is
// excluded.
const MaxOwnerID = 4294967294

// ValidateOwnerID returns an error naming field unless id is in
// [0, MaxOwnerID]. Negative values are rejected, including -1, which chown
// would otherwise read as "leave unchanged". Zero is accepted: callers that
// treat it as "use the default" apply that themselves.
func ValidateOwnerID(field string, id int) error {
	if id < 0 || int64(id) > MaxOwnerID {
		return fmt.Errorf("%s must be between 0 and %d (got %d)", field, int64(MaxOwnerID), id)
	}
	return nil
}
