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

package agent

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// reservedHomeFilePaths lists every file and directory under .scion/ in an
// agent home that pkg/agent, pkg/harness, sciontool, or broker/CLI config and
// credential management owns. Kept as defense in depth behind the exact
// allow-list in pkg/api (api.ValidateHomeFiles), and matched case-insensitively
// so a future allow-list entry can never collide with a broker-owned file even
// on a case-insensitive filesystem.
var reservedHomeFilePaths = []string{
	path.Join(".scion", agentConfigFileName),
	path.Join(".scion", agentInfoFileName),
	path.Join(".scion", servicesFileName),
	path.Join(".scion", resolvedSkillsInputName),
	TaskFileRel,
	agentTokenFileRel,
	agentTokenTmpFileRel,
	path.Join(".scion", transportauth.TransportTokenFileName),
	path.Join(".scion", transportauth.TransportTokenFileName+".tmp"),
	path.Join(".scion", telemetryGCPCredentialsFileName),
	// Retired path, reserved so stale tooling or a template cannot reuse it with different meaning.
	".scion/secrets.json",
	homeprep.LinksRecordPath,
	path.Join(".scion", config.SettingsFileName),
	path.Join(".scion", projectkeys.ProjectIDFile),
	".scion/ssh",    // cmd/ssh_config_files.go (sshKnownHostsDirRel; cmd imports pkg/agent)
	".scion/vscode", // cmd/ssh_config_files.go (~/.scion/vscode; cmd imports pkg/agent)
	".scion/hooks",  // cmd/sciontool/commands/init.go registers all of $HOME/.scion/hooks
	harness.GenericSystemPromptRel,
	harness.StagedBundleRel,
	harness.PreStartHookDirRel,
	harness.GenericSkillsDir,
}

// ValidateHomeFiles enforces shared HomeFiles shape/size/allow-list invariants
// and, as defense in depth, refuses any path that collides (case-insensitively)
// with a broker- or harness-owned file or subtree under .scion/. Error messages
// never include file Content.
func ValidateHomeFiles(files []api.HomeFile) error {
	if err := api.ValidateHomeFiles(files); err != nil {
		return err
	}
	for i, f := range files {
		if reserved, collides := collidesWithReservedHomePath(f.Path); collides {
			return fmt.Errorf("homeFiles[%d]: path %q collides with reserved broker path %q", i, f.Path, reserved)
		}
	}
	return nil
}

func collidesWithReservedHomePath(p string) (string, bool) {
	pl := strings.ToLower(p)
	for _, r := range reservedHomeFilePaths {
		rl := strings.ToLower(r)
		if pl == rl || strings.HasPrefix(pl, rl+"/") || strings.HasPrefix(rl, pl+"/") {
			return r, true
		}
	}
	return "", false
}

// writeProvisionHomeFiles writes create-time HomeFiles into agentHome under
// .scion/, confined to agentHome. Re-validates files as defense in depth
// before touching disk.
//
// Each validated path is an exact hub-owned registered file (currently only
// .scion/trigger-event.json). On a fresh create, any leftover entry at that
// registered path (for example from a reused soft-deleted agent directory or
// a planted leaf symlink) is first unlinked via removeHomeFile without
// following symlinks, and the new file is then created with create-exclusive
// semantics (writeHomeFileExclusive: O_CREAT|O_EXCL|O_NOFOLLOW). This prevents
// a new agent from reading a previous agent's event body while refusing any
// concurrent race or symlinked parent directory.
func writeProvisionHomeFiles(agentHome string, files []api.HomeFile) error {
	if len(files) == 0 {
		return nil
	}
	if err := ValidateHomeFiles(files); err != nil {
		return fmt.Errorf("invalid home files: %w", err)
	}
	for _, f := range files {
		if err := removeHomeFile(agentHome, f.Path); err != nil {
			return fmt.Errorf("remove leftover home file %q: %w", f.Path, err)
		}
		if err := writeHomeFileExclusive(agentHome, f.Path, f.Content, os.FileMode(f.Mode)); err != nil {
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("write home file %q: file already exists in agent home: %w", f.Path, err)
			}
			return fmt.Errorf("write home file %q: %w", f.Path, err)
		}
	}
	return nil
}

// hasHomeFile reports whether files contains an entry for rel.
func hasHomeFile(files []api.HomeFile, rel string) bool {
	for _, f := range files {
		if f.Path == rel {
			return true
		}
	}
	return false
}

// resolvedTriggerEventFilePath returns the container-side absolute POSIX path
// for api.TriggerEventHomeFilePath under unixUsername's home directory.
func resolvedTriggerEventFilePath(unixUsername string) string {
	if unixUsername == "" {
		unixUsername = "root"
	}
	return path.Join(filepath.ToSlash(util.GetHomeDir(unixUsername)), api.TriggerEventHomeFilePath)
}
