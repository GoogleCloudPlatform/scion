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

import "github.com/GoogleCloudPlatform/scion/pkg/version"

// defaultDowngradeFloor is the floor a BinaryUpdateExecutor uses when
// BinaryUpdateOptions.DowngradeFloor is empty. It is a test seam, always
// equal to version.DowngradeFloor in production, so the package's tests can
// pin a fixture floor (downgrade_floor_pin_test.go). Nothing outside the
// tests sets it.
var defaultDowngradeFloor = version.DowngradeFloor
