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

package fsutil

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// mountInfoPath is the file CheckMountSource reads. Not exposed for
// injection; tests instead call checkMountSourceReader directly with
// fabricated content.
const mountInfoPath = "/proc/self/mountinfo"

// CheckMountSource inspects the current process's mount table and, if root
// is itself a mount point, refuses when the mount's bind source (the path
// within its own filesystem that was exposed at root — mountinfo field 4)
// names a critical system directory.
//
// This exists for container-side callers where a bind mount can expose a
// subdirectory of the host filesystem — e.g. the host's /usr bind-mounted at
// the container's /workspace — at a path whose own name gives no indication
// of that (CheckRoot's critical-path-by-name and content-heuristic checks
// both operate on `root`'s own path, not on where its content actually comes
// from). A bind source of "/" is deliberately NOT refused here: dedicated
// volumes and disks legitimately report "/" as their source, and the
// whole-root case is already caught by CheckRoot's content heuristic.
//
// If root is not a mount point, or the mount table is missing entirely
// (e.g. not running on Linux, or no /proc), this is a no-op (nil) — it is a
// defence-in-depth addition on top of CheckRoot, not a replacement for it,
// and its absence must not be treated as a positive safety signal on its
// own. If the mount table exists but fails to parse (a scan error), this
// fails closed and returns that error, rather than silently treating an
// unparseable table the same as an absent one.
func CheckMountSource(root string) error {
	f, err := os.Open(mountInfoPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	return checkMountSourceReader(root, f)
}

// checkMountSourceReader is CheckMountSource's testable core: it parses
// mountinfo-formatted content from r instead of the real mount table.
func checkMountSourceReader(root string, r io.Reader) error {
	cleanedRoot := filepath.Clean(root)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		// Fixed-position fields per proc(5): (1) mount ID (2) parent ID
		// (3) major:minor (4) root (5) mount point (6) mount options,
		// followed by a variable number of optional fields. We only need
		// (4) and (5), always at fixed indices 3 and 4.
		if len(fields) < 5 {
			continue
		}
		bindSource := decodeMountField(fields[3])
		mountPoint := decodeMountField(fields[4])
		if filepath.Clean(mountPoint) != cleanedRoot {
			continue
		}
		if bindSource == "/" {
			// Legitimate for a dedicated volume/disk; whole-root is already
			// caught by CheckRoot's content heuristic.
			continue
		}
		cleanedSource := filepath.Clean(bindSource)
		if criticalSystemPaths[cleanedSource] {
			return fmt.Errorf("%w: %q is mounted from %q", ErrCriticalMountSource, root, cleanedSource)
		}
	}
	return scanner.Err()
}

// decodeMountField reverses the kernel's octal encoding of space, tab,
// newline and backslash in /proc/*/mountinfo path fields (see proc(5)).
func decodeMountField(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
