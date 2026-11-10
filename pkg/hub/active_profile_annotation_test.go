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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestActiveProfileAnnotationKeyMatchesStore pins that the hub's
// activeProfile project setting key (a literal, as the settings registry
// guard requires) is the key readers outside pkg/hub use
// (store.AnnotationProjectActiveProfile), so the rebuild preflight reads
// the annotation the hub writes.
func TestActiveProfileAnnotationKeyMatchesStore(t *testing.T) {
	if projectSettingActiveProfile != store.AnnotationProjectActiveProfile {
		t.Fatalf("projectSettingActiveProfile = %q, store.AnnotationProjectActiveProfile = %q; they must be the same key",
			projectSettingActiveProfile, store.AnnotationProjectActiveProfile)
	}
}
