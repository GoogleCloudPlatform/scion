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
// The file is written with mode 0700: the lifecycle manager runs it as the
// owner, so nothing else needs read or execute access to a script that may
// contain project secrets. The call is idempotent: if the file already exists
// (e.g. on agent restart), it is overwritten with the current script content.
//
// An empty scriptContent means "no hook applies" and removes any previously
// staged script, so a hook deleted at the Hub does not survive on a reused
// agent home.
func WriteProjectPreStartHook(agentHome, scriptContent string) error {
	dir := filepath.Join(agentHome, ".scion", "hooks", "pre-start.d")
	target := filepath.Join(dir, ProjectPreStartHookFilename)

	if scriptContent == "" {
		// Nothing staged (no hook directory at all, or no such file) is the
		// common case and not an error; only a real removal failure is.
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale pre-start hook: %w", err)
		}
		return nil
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create pre-start.d: %w", err)
	}
	if err := os.WriteFile(target, []byte(scriptContent), 0700); err != nil {
		return err
	}
	// WriteFile leaves the mode of a pre-existing file alone, so tighten it
	// explicitly: an agent home staged by an older build may still hold a
	// world-readable 0755 script.
	return os.Chmod(target, 0700)
}
