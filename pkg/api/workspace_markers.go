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

package api

import (
	"os"
	"path/filepath"
	"strings"
)

// workspaceMarkers are the marker entry names, as Scion creates them.
var workspaceMarkers = []string{".scion", ".scion-volumes", ".agents"}

// IsWorkspaceMarker reports whether name is one of the marker entry names
// (.scion, .scion-volumes, .agents): the provisioning marker directories and
// the mount points of shared directories. It is a NAME-only check: it looks
// at no file and says nothing about what kind of entry carries the name.
// Callers that read an entry from a directory must use
// IsWorkspaceMarkerEntry, which also checks that the entry is a real
// directory.
func IsWorkspaceMarker(name string) bool {
	for _, m := range workspaceMarkers {
		if name == m {
			return true
		}
	}
	return false
}

// IsWorkspaceMarkerEntry reports whether the entry name, read from dir, is a
// workspace marker. The entry is judged by Lstat (a symlink is never
// followed), and only a real directory counts: a symlink, a regular file, a
// dangling link or any other kind of entry with a marker name is content,
// not a marker, and so is an entry that cannot be Lstat'ed. An exact marker
// name that is a real directory is a marker. A name that differs from a
// marker only in case is one only when it is a real directory and the
// filesystem resolves it to the same file as the marker name, as a
// case-insensitive filesystem does (an entry created there under another
// case keeps that case). On a case-sensitive filesystem such a name is a
// different entry and is not a marker. The clone step in sciontool init,
// the clone-per-agent provisioning step and the workspace provisioning on a
// shared volume all decide whether a workspace is populated with this rule.
func IsWorkspaceMarkerEntry(dir, name string) bool {
	return isWorkspaceMarkerEntry(dir, name, os.Lstat)
}

func isWorkspaceMarkerEntry(dir, name string, lstat func(string) (os.FileInfo, error)) bool {
	for _, m := range workspaceMarkers {
		if !strings.EqualFold(name, m) {
			continue
		}
		entry, err := lstat(filepath.Join(dir, name))
		if err != nil || !isRealDir(entry) {
			return false
		}
		if name == m {
			return true
		}
		marker, err := lstat(filepath.Join(dir, m))
		if err != nil || !isRealDir(marker) {
			return false
		}
		return os.SameFile(entry, marker)
	}
	return false
}

// isRealDir reports whether fi, from Lstat, is a directory and not a
// symlink.
func isRealDir(fi os.FileInfo) bool {
	return fi.IsDir() && fi.Mode()&os.ModeSymlink == 0
}
