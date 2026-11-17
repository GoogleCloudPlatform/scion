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
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/homeprep"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// reservedHomeFilePaths lists every file and directory under .scion/ in an
// agent home that pkg/agent, pkg/harness, sciontool, or broker/CLI config and
// credential management owns. Derived from the authoritative package constants
// where available (avoiding import cycles into cmd/) so the collision gate has
// one source of truth.
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
	path.Join(".scion", stagedsecrets.VariableSecretsFileName),
	homeprep.LinksRecordPath,
	path.Join(".scion", config.SettingsFileName),
	path.Join(".scion", projectkeys.ProjectIDFile),
	".scion/ssh",    // cmd/ssh_config_files.go (sshKnownHostsDirRel; cmd imports pkg/agent)
	".scion/vscode", // cmd/ssh_config_files.go (~/.scion/vscode; cmd imports pkg/agent)
	harness.GenericSystemPromptRel,
	harness.StagedBundleRel,
	harness.PreStartHookDirRel,
	harness.GenericSkillsDir,
}

// ValidateHomeFiles enforces shared HomeFiles shape/size invariants and refuses
// any path that collides with a broker- or harness-owned file or subtree under
// .scion/. Error messages never include file Content.
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
	for _, r := range reservedHomeFilePaths {
		if p == r || strings.HasPrefix(p, r+"/") || strings.HasPrefix(r, p+"/") {
			return r, true
		}
	}
	return "", false
}

// writeProvisionHomeFiles writes create-time HomeFiles into agentHome under
// .scion/, confined to agentHome via writeHomeFile. Re-validates files as
// defense-in-depth before touching disk.
func writeProvisionHomeFiles(agentHome string, files []api.HomeFile) error {
	if len(files) == 0 {
		return nil
	}
	if err := ValidateHomeFiles(files); err != nil {
		return fmt.Errorf("invalid home files: %w", err)
	}
	for _, f := range files {
		if err := writeHomeFile(agentHome, f.Path, f.Content, os.FileMode(f.Mode)); err != nil {
			return fmt.Errorf("write home file %q: %w", f.Path, err)
		}
	}
	return nil
}
