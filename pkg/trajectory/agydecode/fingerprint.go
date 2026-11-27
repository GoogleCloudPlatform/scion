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

package agydecode

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Fingerprint computes a change fingerprint over dbPath and its companion
// SQLite WAL file (dbPath + "-wal"). Uses Lstat and requires regular files so
// symlinks, FIFOs, and device nodes are never passed to SQLite. Callers can
// compare consecutive fingerprints with FileFingerprint.Equal to skip re-opening
// unchanged files.
func Fingerprint(dbPath string) (FileFingerprint, error) {
	dbInfo, err := os.Lstat(dbPath)
	if err != nil {
		return FileFingerprint{}, fmt.Errorf("lstat db %s: %w", dbPath, err)
	}
	if !dbInfo.Mode().IsRegular() {
		return FileFingerprint{}, fmt.Errorf("agy db %s is not a regular file (mode %s)", dbPath, dbInfo.Mode())
	}
	fp := FileFingerprint{
		DBSize:      dbInfo.Size(),
		DBModTimeNS: dbInfo.ModTime().UnixNano(),
	}
	for _, suffix := range []string{"-wal", "-journal", "-shm"} {
		compPath := dbPath + suffix
		compInfo, err := os.Lstat(compPath)
		if err == nil {
			if !compInfo.Mode().IsRegular() {
				return FileFingerprint{}, fmt.Errorf("agy companion %s is not a regular file (mode %s)", compPath, compInfo.Mode())
			}
			if suffix == "-wal" {
				fp.WALPresent = true
				fp.WALSize = compInfo.Size()
				fp.WALModTimeNS = compInfo.ModTime().UnixNano()
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return FileFingerprint{}, fmt.Errorf("lstat companion %s: %w", compPath, err)
		}
	}
	return fp, nil
}
