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

package harness

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/harnesses"
)

// SharedHarnessHelperSource returns the embedded scion_harness.py contents.
// Tests use this to assert the staged file matches the embedded source.
func SharedHarnessHelperSource() []byte {
	return harnesses.CanonicalHarnessLib
}

// writeSharedHarnessHelper writes the embedded scion_harness.py to dst, creating
// parent directories as needed. The file is mode 0644 — it is not executable
// because provision.py imports it as a Python module.
func writeSharedHarnessHelper(dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("create dir for shared helper: %w", err)
	}
	return os.WriteFile(dst, harnesses.CanonicalHarnessLib, 0644)
}
