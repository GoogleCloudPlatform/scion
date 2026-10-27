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

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ValidateNotExportRoot ensures that hostPath is a proper subdirectory of
// hostBase, never equal to it. This is the critical isolation guard from
// design §9.4: bind-mount only projects/<pid>/..., NEVER the export root
// <MountRoot>/<shareID> itself.
//
// Returns nil if hostBase is empty (local backend — no guard needed).
func ValidateNotExportRoot(hostPath, hostBase string) error {
	if hostBase == "" {
		return nil // local backend — no export root to guard against
	}

	cleanPath := filepath.Clean(hostPath)
	cleanBase := filepath.Clean(hostBase)

	if cleanPath == cleanBase {
		return fmt.Errorf("isolation violation: bind path %q equals export root %q — "+
			"must bind a project subtree, never the export root", hostPath, hostBase)
	}

	if !strings.HasPrefix(cleanPath, cleanBase+"/") {
		return fmt.Errorf("isolation violation: bind path %q is not under export root %q",
			hostPath, hostBase)
	}

	return nil
}
