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

//go:build unix

package cmd

import (
	"fmt"
	"io"
	"os"
)

// warnIfSecretsFileOverlyPermissive warns on Unix when a --secrets-file is
// readable by group or others (mode & 0o077 != 0).
func warnIfSecretsFileOverlyPermissive(w io.Writer, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	perm := info.Mode().Perm()
	if perm&0o077 != 0 {
		_, _ = fmt.Fprintf(w, "Warning: secrets file %s has permissions %04o (group/world-readable); consider running chmod 600 %s\n", path, perm, path)
	}
}
