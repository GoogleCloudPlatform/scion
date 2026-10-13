// Copyright 2026 The Scion Authors.

package commands

import (
	"strings"
	"testing"
)

func TestHostUIDMismatchWarning(t *testing.T) {
	for _, tc := range []struct {
		name               string
		hostUID, hostGID   string
		targetUID, targetG int
		rootless           bool
		euid, egid         int
		want               []string // substrings; nil means no warning
	}{
		{name: "matching drop", hostUID: "1002", hostGID: "1003", targetUID: 1002, targetG: 1003},
		{name: "no ids passed", targetUID: 0, targetG: 0},
		{name: "rootless", hostUID: "1002", hostGID: "1003", rootless: true, euid: 1000, egid: 1000},
		{name: "host root, no drop", hostUID: "0", hostGID: "0"},
		{name: "drop to a different uid", hostUID: "1002", hostGID: "1003", targetUID: 1000, targetG: 1000,
			want: []string{"uid mismatch", "harness will run as uid 1000 (gid 1000)", "agent runtime passed uid 1002 (gid 1003)"}},
		{name: "gid differs", hostUID: "1002", hostGID: "1003", targetUID: 1002, targetG: 1002,
			want: []string{"uid 1002 (gid 1002)", "uid 1002 (gid 1003)"}},
		{name: "no drop, init as root", hostUID: "1002", hostGID: "1003", euid: 0, egid: 0,
			want: []string{"uid 0 (gid 0, no privilege drop)"}},
		{name: "no drop, init not root", hostUID: "1002", hostGID: "1003", euid: 1000, egid: 1000,
			want: []string{"uid 1000 (gid 1000, no privilege drop)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := hostUIDMismatchWarning(tc.hostUID, tc.hostGID, tc.targetUID, tc.targetG, tc.rootless, tc.euid, tc.egid)
			if tc.want == nil {
				if got != "" {
					t.Fatalf("unexpected warning: %q", got)
				}
				return
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("warning %q lacks %q", got, w)
				}
			}
		})
	}
}
