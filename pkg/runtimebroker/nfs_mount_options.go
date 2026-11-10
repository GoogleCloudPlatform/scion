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
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// defaultNFSMountOptions are the mount options used when the configuration
// sets none (the same default config.V1WorkspaceStorageConfig.ApplyNFSDefaults
// applies).
const defaultNFSMountOptions = "vers=3,hard,nconnect=4,_netdev"

// requiredNFSMountFlags are the per-mount flags every NFS mount the broker
// makes carries, and that an existing mount must show in the mount table to
// count as mounted correctly. They are not configurable.
var requiredNFSMountFlags = []string{"nosuid", "nodev"}

// HardenNFSMountOptions returns opts with nosuid and nodev appended as the
// last two options, whatever opts contains and whatever its origin.
// mount(8) applies options in order,
// so the last ones win over anything earlier, such as "defaults", "exec" or
// "suid". Every other option is kept, in order. Empty tokens and the tokens
// suid, dev, nosuid and nodev are dropped first, so the result never
// contradicts itself and never repeats a flag. The function is idempotent.
func HardenNFSMountOptions(opts string) string {
	kept := make([]string, 0, 8)
	for _, tok := range strings.Split(opts, ",") {
		tok = strings.TrimSpace(tok)
		switch tok {
		case "", "suid", "dev", "nosuid", "nodev":
			continue
		}
		kept = append(kept, tok)
	}
	kept = append(kept, requiredNFSMountFlags...)
	return strings.Join(kept, ",")
}

// NFSShareTarget returns the path a share is mounted at,
// <mount_root>/<shareID>, with mount_root resolved through symlinks:
// mount(8) records the resolved path in the mount table, so a lexical path
// under a symlinked mount_root would never match it. Only mount_root (a
// local directory) is resolved, never the share's own mount point, so a
// hung NFS mount cannot block it. When mount_root cannot be resolved (it
// does not exist yet), the cleaned lexical path is used.
func NFSShareTarget(mountRoot, shareID string) string {
	root := filepath.Clean(mountRoot)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return filepath.Join(root, shareID)
}

// effectiveNFSMountOptions returns the mount options the reconciler passes to
// every mount: the configured options (or the default when none are set),
// hardened by HardenNFSMountOptions.
func effectiveNFSMountOptions(cfg *config.V1NFSConfig) string {
	opts := ""
	if cfg != nil {
		opts = cfg.MountOptions
	}
	if strings.TrimSpace(opts) == "" {
		opts = defaultNFSMountOptions
	}
	return HardenNFSMountOptions(opts)
}

// MissingNFSMountFlags returns the required flags (nosuid, nodev) absent
// from a mount table options field ("rw,nosuid,nodev,relatime,...").
func MissingNFSMountFlags(options string) []string {
	have := make(map[string]bool)
	for _, tok := range strings.Split(options, ",") {
		have[strings.TrimSpace(tok)] = true
	}
	var missing []string
	for _, f := range requiredNFSMountFlags {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	return missing
}
