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

//go:build !unix

package util

// unlinkAt is unavailable on this platform: it attempts nothing and returns
// errUnlinkAtUnsupported, so removeSymlinkSafe fails closed instead of
// falling back to path-based removal.
func unlinkAt(_, _ string) (attempted bool, err error) {
	return false, errUnlinkAtUnsupported
}
