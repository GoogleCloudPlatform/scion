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

package delegation

// socketMode is the delegation socket's permission mode.
const socketMode = 0o600

// socketConfig is where the delegation socket is created and who owns it.
type socketConfig struct {
	path string
	// uid and gid own the socket when uid > 0; uid is also accepted as a
	// peer, besides sciontool's own user.
	uid, gid int
}
