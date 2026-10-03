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
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// procMountsPath is the Linux mount table read by ProcMountSource.
const procMountsPath = "/proc/mounts"

// ProcMountSource looks path up in /proc/mounts and returns the source
// mounted there (server:export for NFS) and whether path is a mountpoint.
// It reads the mount table only and never stats path, so a hung NFS mount
// cannot block it. The broker's NFS reconciler and scion doctor both use
// it, so they agree on what is mounted.
func ProcMountSource(path string) (source string, mounted bool, err error) {
	f, err := os.Open(procMountsPath)
	if err != nil {
		return "", false, fmt.Errorf("reading mount table: %w", err)
	}
	defer func() { _ = f.Close() }()
	return FindMountSource(f, path)
}

// FindMountSource scans a /proc/mounts-format table for path. Paths are
// compared after decoding the kernel's octal escapes and filepath.Clean.
// When path is mounted more than once, the last (topmost, visible) entry
// wins.
func FindMountSource(r io.Reader, path string) (source string, mounted bool, err error) {
	clean := filepath.Clean(path)
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		if filepath.Clean(unescapeMountField(fields[1])) == clean {
			source, mounted = unescapeMountField(fields[0]), true
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("reading mount table: %w", err)
	}
	return source, mounted, nil
}

// unescapeMountField decodes the octal escapes (\040 for space, etc.) the
// kernel uses in /proc/mounts fields.
func unescapeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
