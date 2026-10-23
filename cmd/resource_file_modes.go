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
	"os"
	"runtime"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// modeOnlyChanges returns, sorted, the paths of local files whose content
// matches the Hub's copy but whose executable bit differs from it
// (ptone/scion#5652). The content hash does not cover file modes, so sync
// would otherwise report such a template or harness-config as up to date
// and never send the new mode. These files need no upload: finalizing with
// the local manifest records their mode.
//
// A Hub from before ptone/scion#5652 sends no modes; its files count as
// 0644, so an executable local file is reported each time (a finalize that
// changes nothing on such a Hub).
//
// On Windows nothing is reported: its file modes carry no executable bit,
// so every executable on the Hub would look changed, and a sync without a
// content change would strip the bits.
func modeOnlyChanges(local []hubclient.FileInfo, remote []hubclient.DownloadURLInfo) []string {
	remoteFiles := make([]hubclient.TemplateFile, len(remote))
	for i, f := range remote {
		remoteFiles[i] = hubclient.TemplateFile{Path: f.Path, Hash: f.Hash, Mode: f.Mode}
	}
	return modeOnlyChangesInManifest(local, remoteFiles)
}

// modeOnlyChangesInManifest is modeOnlyChanges against a recorded manifest
// (a template's or harness-config's Files).
func modeOnlyChangesInManifest(local []hubclient.FileInfo, remote []hubclient.TemplateFile) []string {
	if !localModesCarryExecBit() {
		return nil
	}
	remoteByPath := make(map[string]hubclient.TemplateFile, len(remote))
	for _, f := range remote {
		remoteByPath[f.Path] = f
	}
	var changed []string
	for _, f := range local {
		r, ok := remoteByPath[f.Path]
		if !ok || r.Hash != f.Hash {
			continue // a content change or new file is uploaded anyway
		}
		if transfer.IsExecutableMode(f.Mode) != transfer.IsExecutableMode(r.Mode) {
			changed = append(changed, f.Path)
		}
	}
	sort.Strings(changed)
	return changed
}

// localMatchesHub reports whether local files are the files the Hub records
// for a template: the same paths and per-file content hashes, and, where
// this platform's file modes carry an executable bit, the same executable
// bits (on Windows the Hub's bits are taken as they are; see
// modeOnlyChangesInManifest).
//
// It compares manifests rather than content hashes, because the Hub's
// ContentHash is ComputeContentHashV2 (paths and modes included) on a Hub
// with ptone/scion#5658 and a row committed since, and the
// ComputeContentHashV1 of an older Hub or an older row, which does not see
// a rename. A Hub response without a manifest falls back to comparing the
// content hash against both versions.
func localMatchesHub(local []hubclient.FileInfo, remote []hubclient.TemplateFile, hubContentHash string) bool {
	if len(remote) == 0 {
		if hubContentHash == "" {
			return false
		}
		return hubContentHash == transfer.ComputeContentHashV2(local) ||
			hubContentHash == transfer.ComputeContentHashV1(local)
	}
	if len(local) != len(remote) {
		return false
	}
	remoteByPath := make(map[string]hubclient.TemplateFile, len(remote))
	for _, f := range remote {
		remoteByPath[f.Path] = f
	}
	if len(remoteByPath) != len(remote) {
		return false
	}
	for _, f := range local {
		r, ok := remoteByPath[f.Path]
		if !ok || r.Hash != f.Hash {
			return false
		}
	}
	return len(modeOnlyChangesInManifest(local, remote)) == 0
}

// localModesCarryExecBit reports whether this platform's file modes record
// an executable bit (false on Windows). A variable so tests can override it.
var localModesCarryExecBit = func() bool { return runtime.GOOS != "windows" }

// writePulledFile writes a file downloaded from the Hub to rel inside root
// (creating parent directories) with the normalised mode the Hub recorded
// for it: "0755" keeps an executable (a plugin bin/ command, a provision
// script) runnable; an empty mode, from a Hub before ptone/scion#5652, is
// 0644. Used by `templates pull` and `harness-config pull`.
func writePulledFile(root *os.Root, rel string, content []byte, mode string) error {
	if err := transfer.WriteFileInRootMode(root, rel, content, transfer.NormalizeMode(mode)); err != nil {
		return fmt.Errorf("write %s: %w", rel, err)
	}
	return nil
}
