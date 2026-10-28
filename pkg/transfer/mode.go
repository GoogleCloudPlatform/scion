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

package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// File modes a transferred file can have. Only the executable bit
// survives a transfer: a file is either executable (0755) or not (0644).
// Setuid, setgid, sticky and group/other write bits are never carried
// (ptone/scion#5652).
const (
	ModeRegular    os.FileMode = 0o644
	ModeExecutable os.FileMode = 0o755
)

// NormalizeMode maps a recorded file mode (the octal string a manifest
// carries, e.g. "0755") to the mode a transferred file is written with:
// ModeExecutable when any execute bit is set, otherwise ModeRegular. An
// empty or unparseable mode, as on entries recorded before modes were
// carried, is ModeRegular.
func NormalizeMode(mode string) os.FileMode {
	if mode == "" {
		return ModeRegular
	}
	v, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return ModeRegular
	}
	if v&0o111 != 0 {
		return ModeExecutable
	}
	return ModeRegular
}

// IsExecutableMode reports whether a recorded file mode normalises to
// ModeExecutable.
func IsExecutableMode(mode string) bool {
	return NormalizeMode(mode) == ModeExecutable
}

// FormatMode renders a file mode as the four-digit octal string manifests
// carry (e.g. "0755").
func FormatMode(m os.FileMode) string {
	return fmt.Sprintf("%04o", m.Perm())
}

// NormalizedModeString returns the normalised mode of a recorded mode as
// a manifest string: "0755" or "0644".
func NormalizedModeString(mode string) string {
	return FormatMode(NormalizeMode(mode))
}

// WriteFileInRootMode writes data to rel inside root like WriteFileInRoot
// and then sets the file's mode to exactly perm, so the result does not
// depend on the process umask or on the mode of a file that already
// existed at rel.
func WriteFileInRootMode(root *os.Root, rel string, data []byte, perm os.FileMode) error {
	if err := WriteFileInRoot(root, rel, data, perm); err != nil {
		return err
	}
	return root.Chmod(filepath.FromSlash(rel), perm)
}
