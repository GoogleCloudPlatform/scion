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

package runtime

import "github.com/GoogleCloudPlatform/scion/pkg/projectfs"

// realizeUserVolume places a `volumes:` entry in a container of runtime
// kind through the one Mount path (design §P5, ptone/scion#5837): the entry
// is an Owner=user, Scope=agent projectfs Mount, resolved and realized by
// projectfs.Layout like the workspace and shared dirs. source and target
// are the entry's, already expanded by the runtime; for a gcs volume source
// is the bucket. The result is the mount exactly as the entry declares it.
func realizeUserVolume(source, target string, readOnly bool, kind projectfs.RuntimeKind) (projectfs.Realized, error) {
	// A user volume is on no backend of the project's Layout, so the zero
	// Layout resolves and realizes it.
	var layout projectfs.Layout
	r, err := layout.Resolve(projectfs.Project{}, "", projectfs.UserVolumeMount(source, target, readOnly))
	if err != nil {
		return projectfs.Realized{}, err
	}
	return layout.Realize(r, kind)
}
