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
	"errors"
	"fmt"
	"io/fs"
	"path"

	"github.com/GoogleCloudPlatform/scion/pkg/agenthome"
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
//
// The write and the removal are confined to the agent home (pkg/agenthome):
// a link at the hook is replaced or removed itself, never followed, and a
// linked directory component is refused.
func WriteProjectPreStartHook(agentHome, scriptContent string) error {
	target := path.Join(hookDirRel, ProjectPreStartHookFilename)

	if scriptContent == "" {
		// Nothing staged (no home, no hook directory at all, or no such
		// file) is the common case and not an error; only a real removal
		// failure is.
		home, err := agenthome.Open(agentHome)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { _ = home.Close() }()
		if err := home.Remove(target); err != nil {
			return fmt.Errorf("remove stale pre-start hook: %w", err)
		}
		return nil
	}

	home, err := agenthome.OpenCreate(agentHome, 0755)
	if err != nil {
		return err
	}
	defer func() { _ = home.Close() }()
	if err := home.MkdirAll(hookDirRel, 0755); err != nil {
		return fmt.Errorf("create pre-start.d: %w", err)
	}
	// WriteFile installs a new file with exactly this mode, so a script
	// staged world-readable by an older build is replaced by a 0700 one.
	return home.WriteFile(target, []byte(scriptContent), 0700)
}
