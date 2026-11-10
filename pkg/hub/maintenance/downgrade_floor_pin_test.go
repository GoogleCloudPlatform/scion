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

package maintenance

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/version"
)

// shippedDowngradeFloor is defaultDowngradeFloor as production initializes
// it. Package-level vars, in every file of the package, are initialized
// before any init function runs, so this copy is taken before the pin below.
var shippedDowngradeFloor = defaultDowngradeFloor

// The package's tests run under this floor: the release fixtures are dated
// against it, so the tests do not depend on the shipped
// version.DowngradeFloor, which moves with every destructive migration.
func init() {
	defaultDowngradeFloor = "nightly-20261009"
}

// TestDefaultDowngradeFloorShipsTheConstant: production's
// defaultDowngradeFloor is version.DowngradeFloor, so the seam cannot drift
// from the shipped floor.
func TestDefaultDowngradeFloorShipsTheConstant(t *testing.T) {
	if shippedDowngradeFloor != version.DowngradeFloor {
		t.Fatalf("defaultDowngradeFloor initializes to %q, want version.DowngradeFloor %q", shippedDowngradeFloor, version.DowngradeFloor)
	}
}
