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

package runtimebroker

import (
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// warnHarnessConfigDrift logs a WARN when the broker provisions harness-config
// name from the hub-hydrated copy at hydratedPath while a broker-local
// directory of the same name (project or global harness-configs/<name>)
// exists with different content. The hydrated copy is the one used; the WARN
// only makes the divergence visible, since the local copy is what local
// starts, unstamped dispatches and an older broker would use
// (ptone/scion#611). Nothing is logged when no local copy exists or the
// contents match.
//
// Both copies are hashed with hubCompatibleContentHash: the hub's content
// hash algorithm (transfer.ComputeContentHash) over line-ending-normalized
// files, excluding transient files. The values are therefore comparable with
// each other, but are not necessarily the hub record's stored content_hash
// (a record uploaded with CRLF content or transient files hashes
// differently); dispatchHash, the record hash the dispatch carried, is
// logged alongside when present.
//
// Coverage limits, by design:
//   - Only the project and global harness-configs/<name> directories are
//     compared (FindHarnessConfigDir with no template paths); a copy bundled
//     in a template is not.
//   - The content hash covers file contents, not file paths (the hub's
//     algorithm), so a difference that is only a rename does not WARN.
func (s *Server) warnHarnessConfigDrift(agentID, name, hydratedPath, projectPath, dispatchHash string) {
	if hydratedPath == "" {
		return
	}
	if name == "" {
		s.agentLifecycleLog.Debug("Harness-config drift check skipped: the dispatch names no harness-config",
			"agent_id", agentID, "path", hydratedPath)
		return
	}
	local, err := config.FindHarnessConfigDir(name, harnessConfigProjectDir(projectPath))
	if err != nil || local == nil || local.Path == "" {
		return
	}
	if sameDir(local.Path, hydratedPath) {
		return
	}
	hydratedHash, err := hubCompatibleContentHash(hydratedPath)
	if err != nil {
		s.agentLifecycleLog.Debug("Harness-config drift check skipped: cannot hash hydrated copy",
			"agent_id", agentID, "harness_config", name, "path", hydratedPath, "error", err)
		return
	}
	localHash, err := hubCompatibleContentHash(local.Path)
	if err != nil {
		s.agentLifecycleLog.Debug("Harness-config drift check skipped: cannot hash on-disk copy",
			"agent_id", agentID, "harness_config", name, "path", local.Path, "error", err)
		return
	}
	if hydratedHash == localHash {
		return
	}
	attrs := []any{
		"agent_id", agentID,
		"harness_config", name,
		"hub_hydrated_path", hydratedPath,
		"hydrated_content_hash", hydratedHash,
		"on_disk_path", local.Path,
		"on_disk_content_hash", localHash,
	}
	if dispatchHash != "" {
		attrs = append(attrs, "dispatch_content_hash", dispatchHash)
	}
	s.agentLifecycleLog.Warn("Harness-config drift: the on-disk copy differs from the hub copy used for this agent; local starts and dispatches without a hub harness-config use the on-disk copy", attrs...)
}

// hubCompatibleContentHash returns the hub content hash algorithm's value
// over the files in dir, line-ending-normalized, skipping transient files
// with the same exclude patterns harness-config sync and the hub resource
// store use (config.HarnessConfigTransientPatterns). Files are read and
// normalized in memory; dir is not modified.
func hubCompatibleContentHash(dir string) (string, error) {
	files, err := transfer.CollectFiles(dir, config.HarnessConfigTransientPatterns)
	if err != nil {
		return "", err
	}
	for i := range files {
		data, err := os.ReadFile(files[i].FullPath)
		if err != nil {
			return "", err
		}
		files[i].Hash = transfer.HashBytes(transfer.NormalizeFileContent(data))
	}
	return transfer.ComputeContentHash(files), nil
}

func sameDir(a, b string) bool {
	if absA, err := filepath.Abs(a); err == nil {
		a = absA
	}
	if absB, err := filepath.Abs(b); err == nil {
		b = absB
	}
	if a == b {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}
