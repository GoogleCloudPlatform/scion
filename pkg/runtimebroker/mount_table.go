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

// procMountsPath is the Linux mount table read by ReadProcMountTable.
const procMountsPath = "/proc/mounts"

// maxMountTableLine bounds a single /proc/mounts line (long option strings
// can exceed bufio.Scanner's 64 KiB default).
const maxMountTableLine = 1 << 20

// MountTable is a parsed mount table: cleaned, unescaped mountpoint to the
// source mounted there (server:export for NFS). When a path is mounted more
// than once, the last (topmost, visible) entry wins.
type MountTable map[string]string

// Lookup returns the source mounted at path and whether path is a
// mountpoint. path is compared after filepath.Clean.
func (t MountTable) Lookup(path string) (source string, mounted bool) {
	source, mounted = t[filepath.Clean(path)]
	return source, mounted
}

// ReadProcMountTable reads and parses /proc/mounts. It reads the mount
// table only and never stats a mountpoint, so a hung NFS mount cannot block
// it. The broker's NFS reconciler and scion doctor both use it, so they
// agree on what is mounted.
func ReadProcMountTable() (MountTable, error) {
	f, err := os.Open(procMountsPath)
	if err != nil {
		return nil, fmt.Errorf("reading mount table: %w", err)
	}
	defer func() { _ = f.Close() }()
	return ParseMountTable(f)
}

// ParseMountTable parses a /proc/mounts-format table, decoding the
// kernel's octal escapes in the source and mountpoint fields.
func ParseMountTable(r io.Reader) (MountTable, error) {
	t := MountTable{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMountTableLine)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		t[filepath.Clean(unescapeMountField(fields[1]))] = unescapeMountField(fields[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading mount table: %w", err)
	}
	return t, nil
}

// ProcMountSource looks path up in /proc/mounts and returns the source
// mounted there and whether path is a mountpoint.
func ProcMountSource(path string) (source string, mounted bool, err error) {
	t, err := ReadProcMountTable()
	if err != nil {
		return "", false, err
	}
	source, mounted = t.Lookup(path)
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
