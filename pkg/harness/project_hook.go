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
)

// ProjectPreStartHookFilename is the name of the staged project hook script
// inside the pre-start.d directory. The numeric prefix 30 places it after the
// harness provisioner (20-harness-provision), ensuring provision.py has already
// run when the project script executes.
const ProjectPreStartHookFilename = "30-project-custom"

// WriteProjectPreStartHook stages a project-owner-supplied script into the
// agent home's pre-start hook directory at the fixed filename
// 30-project-custom.
//
// The file is written with mode 0755 (executable) so the lifecycle manager
// picks it up without additional chmod. The call is idempotent: if the file
// already exists (e.g. on agent restart), it is overwritten with the current
// script content.
//
// A non-empty scriptContent is required. Callers should check
// AppliedConfig.ProjectPreStartHookScript before calling this function.
func WriteProjectPreStartHook(agentHome, scriptContent string) error {
	if scriptContent == "" {
		return nil
	}
	dir := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create pre-start.d: %w", err)
	}
	target := filepath.Join(dir, ProjectPreStartHookFilename)
	return os.WriteFile(target, []byte(scriptContent), 0755)
}
