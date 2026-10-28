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
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// loadStartStorageSettings loads the global settings (with any hub
// overlay) an agent start checks for storage refusals. It is a variable so
// tests can count or replace the load.
var loadStartStorageSettings = config.LoadGlobalSettingsWithOverlayFor

// checkStartStorageRefusals refuses an agent start, before any side effect,
// whose storage would be placed differently than the operator configured
// it before server.project_storage (ptone/scion#6371):
//
//   - the start's profile (or that profile's runtime entry) sets a removed
//     storage override (config.VersionedSettings.RemovedStorageOverridesForProfile);
//   - the agent's per-agent shared-dir backend record from an earlier
//     version, live or already moved aside, pins shared dirs to nfs while
//     the shared-dir layout is not shared: those dirs' data is on the
//     export and would silently resolve to local storage.
//
// Both return a *config.StorageMigrationRequiredError. Settings that
// cannot be loaded or resolved refuse nothing here: the shared-dir and
// home paths of the start fail closed on them as before. agentDir is the
// agent's state dir ("" when unknown: no record check).
func checkStartStorageRefusals(ctx context.Context, agentDir, profile string) error {
	vs, _, err := loadStartStorageSettings(ctx)
	if err != nil || vs == nil {
		return nil
	}
	if err := vs.RemovedStorageOverridesForProfile(profile); err != nil {
		return err
	}
	if agentDir == "" {
		return nil
	}
	layout, _, err := config.SharedDirStorageLayout(vs.Server)
	if err != nil {
		return nil
	}
	var sd *config.V1SharedDirStorageConfig
	if vs.Server != nil {
		sd = vs.Server.SharedDirStorage
	}
	return sharedDirRecordPinRefusal(agentDir, layout, sd)
}

// CheckProfileStorageRefusal is the profile half of the agent-start
// storage refusal, for callers that must refuse before any preparation of
// the start (the runtime broker's create admission, before the workspace
// bootstrap, the download and provisioning): a
// *config.StorageMigrationRequiredError when the profile (else the active
// profile) or its runtime entry, in the global settings with any hub
// overlay as ctx's view reads them, carries a removed storage override.
// Settings that cannot be loaded refuse nothing here. Start repeats the
// check, with the per-agent record check, as a backstop.
func CheckProfileStorageRefusal(ctx context.Context, profile string) error {
	return checkStartStorageRefusals(ctx, "", profile)
}

// sharedDirRecordPinRefusal returns a *config.StorageMigrationRequiredError
// when the agent's per-agent shared-dir backend record
// (legacySharedDirStorageRecordFile, or the copy retireSharedDirRecord
// moved aside) pins any shared dir to nfs and layout is not shared. The
// record is left in place. A pin to local under a shared layout is not
// refused: retireSharedDirRecord warns once and the agent follows the
// layout.
func sharedDirRecordPinRefusal(agentDir string, layout projectfs.Layout, sd *config.V1SharedDirStorageConfig) error {
	if layout.Backend.Shared() {
		return nil
	}
	for _, name := range []string{legacySharedDirStorageRecordFile, legacySharedDirStorageRecordFile + retiredSharedDirRecordSuffix} {
		pinned, ok, err := nfsPinnedSharedDirs(filepath.Join(agentDir, name))
		if err != nil {
			// A link, a non-regular file or an oversized record: fail closed,
			// with a fixed public text (the full error, with the path, is for
			// the broker log only).
			return config.NewStorageMigrationRequiredError(
				fmt.Sprintf("cannot check this agent's shared-dir storage record: %v", err),
				"cannot check this agent's shared-dir storage record from an earlier version (it is not a regular file, "+
					"is a link, or is too large); the broker log names it. Repair or remove the record, then start the agent again")
		}
		if !ok {
			continue
		}
		head := fmt.Sprintf("this agent's shared dirs (%s) were pinned to the nfs export by an earlier version (record %s, kept), "+
			"but the shared-dir layout is now %q: they would resolve to local storage and their data on the export would not be visible, "+
			"so the start is refused. If the data was deliberately left behind or copied elsewhere, removing the record "+
			"also lets the agent start on the current layout. ", pinned, name, layout.Backend)
		return config.NewStorageMigrationRequiredError(
			head+config.SharedDirMigrationText(sd),
			head+config.SharedDirMigrationText(nil))
	}
	return nil
}

// maxSharedDirRecordSize bounds the read of a per-agent shared-dir backend
// record. Earlier versions wrote a backend and a small per-dir map.
const maxSharedDirRecordSize = 64 << 10

// nfsPinnedSharedDirs reads a per-agent shared-dir backend record (bounded,
// readSharedDirRecord) and returns which dirs it pins to nfs ("all" for the
// record's default backend, else the dir names) and whether it pins any. A
// missing record, or one that does not parse, pins nothing; a record that
// cannot be read safely (a link, not a regular file, over the size bound)
// is an error.
func nfsPinnedSharedDirs(path string) (string, bool, error) {
	data, err := readSharedDirRecord(path)
	if err != nil {
		return "", false, err
	}
	if data == nil {
		return "", false, nil
	}
	var rec struct {
		Backend string            `json:"backend"`
		Dirs    map[string]string `json:"dirs,omitempty"`
	}
	if json.Unmarshal(data, &rec) != nil {
		return "", false, nil
	}
	if rec.Backend == config.WorkspaceStorageBackendNFS {
		return "all", true, nil
	}
	var names []string
	for name, b := range rec.Dirs {
		if b == config.WorkspaceStorageBackendNFS {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "", false, nil
	}
	sort.Strings(names)
	return strings.Join(names, ", "), true, nil
}
