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
	"strings"
	"testing"
)

func TestParseMountTable(t *testing.T) {
	table := strings.Join([]string{
		"proc /proc proc rw 0 0",
		"10.0.0.9:/old /mnt/nfs/ws1 nfs rw 0 0",
		"10.0.0.2:/scion-workspaces /mnt/nfs/ws1 nfs rw,vers=3 0 0",
		`10.0.0.3:/with\040space /mnt/nfs/my\040share nfs rw 0 0`,
		"short",
	}, "\n")
	cases := []struct {
		path, wantSource string
		wantMounted      bool
	}{
		{"/mnt/nfs/ws1", "10.0.0.2:/scion-workspaces", true}, // last entry wins
		{"/mnt/nfs/ws1/", "10.0.0.2:/scion-workspaces", true},
		{"/mnt/nfs/my share", "10.0.0.3:/with space", true},
		{"/mnt/nfs/ws2", "", false},
	}
	for _, tc := range cases {
		mt, err := ParseMountTable(strings.NewReader(table))
		if err != nil {
			t.Fatalf("ParseMountTable: %v", err)
		}
		src, mounted := mt.Lookup(tc.path)
		if src != tc.wantSource || mounted != tc.wantMounted {
			t.Errorf("Lookup(%q) = %q, %v; want %q, %v", tc.path, src, mounted, tc.wantSource, tc.wantMounted)
		}
	}
	if got := unescapeMountField(`a\134b\0`); got != `a\b\0` {
		t.Errorf("unescapeMountField = %q", got)
	}
}

// TestParseMountTable_LongLine verifies that a line longer than
// bufio.Scanner's 64 KiB default does not fail the whole table.
func TestParseMountTable_LongLine(t *testing.T) {
	long := "tmpfs /mnt/long tmpfs rw," + strings.Repeat("x", 100*1024) + " 0 0"
	table := long + "\n10.0.0.2:/e /mnt/nfs/ws1 nfs rw 0 0\n"
	mt, err := ParseMountTable(strings.NewReader(table))
	if err != nil {
		t.Fatalf("ParseMountTable = %v", err)
	}
	if src, ok := mt.Lookup("/mnt/nfs/ws1"); !ok || src != "10.0.0.2:/e" {
		t.Errorf("Lookup = %q, %v", src, ok)
	}
}
