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
	"fmt"
	"io"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
)

// procMountsPath is the Linux mount table read by ReadProcMountTable.
const procMountsPath = "/proc/mounts"

// MountEntry is projectfs.MountEntry, the parsed mount table entry shared
// with the runtimes.
type MountEntry = projectfs.MountEntry

// MountTable is projectfs.MountTable.
type MountTable = projectfs.MountTable

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

// ParseMountTable parses a /proc/mounts-format table; see
// projectfs.ParseMountTable.
func ParseMountTable(r io.Reader) (MountTable, error) {
	return projectfs.ParseMountTable(r)
}

// ProcMountEntry looks path up in /proc/mounts and returns the entry
// mounted there and whether path is a mountpoint.
func ProcMountEntry(path string) (entry MountEntry, mounted bool, err error) {
	t, err := ReadProcMountTable()
	if err != nil {
		return MountEntry{}, false, err
	}
	entry, mounted = t.LookupEntry(path)
	return entry, mounted, nil
}
