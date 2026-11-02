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

package provision

import "github.com/GoogleCloudPlatform/scion/pkg/api"

// CheckDispatchCloneURL is the clone-URL check a broker runs before it
// dispatches an agent whose workspace is cloned from cloneURL, on any route
// (the host-side worktree clone, the linked checkout or the in-container
// clone): CheckHostCloneURL's rule, https, ssh or git only. An empty URL
// (nothing is cloned) passes. The only other exception is a local
// repository a running test registered with AllowLocalCloneFixture, which is
// honoured only in a test binary.
func CheckDispatchCloneURL(cloneURL string) error {
	return checkProvisionCloneURL(ProvisionInput{GitClone: &api.GitCloneConfig{URL: cloneURL}})
}
