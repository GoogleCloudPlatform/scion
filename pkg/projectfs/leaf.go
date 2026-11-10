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

package projectfs

import "path/filepath"

// ProvisionStateDir resolves a project's provisioning state directory on a
// shared backend: <ProjectRoot>/provision, beside workspace/ and inside no
// Mount.
func (l Layout) ProvisionStateDir(projectID string) (Resolved, error) {
	pr, err := l.ProjectRoot(projectID)
	if err != nil {
		return Resolved{}, err
	}
	return l.shared(filepath.Join(pr.RelPath, provisionDir), Mount{}), nil
}
