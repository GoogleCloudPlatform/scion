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
// SQLite WAL file (dbPath + "-wal"). Callers can compare consecutive
// fingerprints with FileFingerprint.Equal to skip re-opening unchanged files.
func Fingerprint(dbPath string) (FileFingerprint, error) {
	dbInfo, err := os.Stat(dbPath)
	if err != nil {
		return FileFingerprint{}, fmt.Errorf("stat db %s: %w", dbPath, err)
	}
	fp := FileFingerprint{
		DBSize:      dbInfo.Size(),
		DBModTimeNS: dbInfo.ModTime().UnixNano(),
	}
	walPath := dbPath + "-wal"
	walInfo, err := os.Stat(walPath)
	if err == nil {
		fp.WALPresent = true
		fp.WALSize = walInfo.Size()
		fp.WALModTimeNS = walInfo.ModTime().UnixNano()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return FileFingerprint{}, fmt.Errorf("stat wal %s: %w", walPath, err)
	}
	return fp, nil
}
