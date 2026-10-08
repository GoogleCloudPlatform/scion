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

package cmd

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// collectWorkspaceFiles collects the files of a local workspace for a hub
// workspace transfer: the non-git workspace bootstrap upload, `scion sync to`,
// and the local comparison set for `scion sync from` (so it matches what an
// upload would send). It applies transfer.DefaultExcludePatterns, which keep
// .git and the workspace-root .scion entry out of the transfer, plus extra.
//
// The root .scion entry is project metadata for the host it lives on and is
// never part of a workspace transfer. If a collected path is the root .scion
// entry or lies under it (for example because the defaults were changed),
// the collect fails instead of returning it.
//
// Every workspace collect in this package goes through here; a guard test
// fails if a call site uses transfer.CollectFiles or a ManifestBuilder
// directly.
func collectWorkspaceFiles(root string, extra []string) ([]transfer.FileInfo, error) {
	// transfer.CollectFiles seeds the defaults; extra only adds to them.
	files, err := transfer.CollectFiles(root, extra)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if f.Path == config.DotScion || strings.HasPrefix(f.Path, config.DotScion+"/") {
			return nil, fmt.Errorf("workspace collect included %q: the workspace-root %s entry must be excluded", f.Path, config.DotScion)
		}
	}
	return files, nil
}
