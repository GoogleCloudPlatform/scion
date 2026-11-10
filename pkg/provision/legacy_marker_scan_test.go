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

package provision

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// AC-L8: outside the one-shot promotion step (legacy_promote.go), no
// production code reads, writes or git-excludes <workspace>/.scion-provisioned,
// or creates or checks <workspace>/.scion-provision.lock*. The scan covers the
// non-test Go sources of pkg/, cmd/ and extras/ and flags:
//
//   - a filepath.Join of a workspace path (an argument named like ws,
//     workspace, hostPath, dest or rel, not wrapped in ProjectStateDir) with
//     the sentinel or lock name;
//   - any "/.scion-provisioned" git exclude pattern.
//
// Only the promotion step's own file is allow-listed.

// legacyMarkerAllowList is the repository-relative path of the promotion
// step, the only file that may name the workspace-root marker and lock.
var legacyMarkerAllowList = map[string]bool{
	filepath.Join("pkg", "provision", "legacy_promote.go"): true,
}

var (
	// markerNames matches the sentinel and lock names, as constants or
	// literals.
	markerNames = `(?:provision\.)?(?:ProvisionSentinelFile|provisionFileLockName)|"\.scion-provisioned"|"\.scion-provision\.lock[^"]*"`
	// joinWithMarker captures the arguments before the name in a
	// filepath.Join (or path.Join) whose last argument is a marker name.
	joinWithMarker = regexp.MustCompile(`(?:filepath|path)\.Join\(([^;]*?),\s*(?:` + markerNames + `)\s*\)`)
	// workspaceArg matches an argument naming a workspace path.
	workspaceArg = regexp.MustCompile(`(?i)(?:^|[^a-z])(?:ws|workspace\w*|hostpath|dest|rel|resolved\.hostpath|res\.hostpath)(?:$|[^a-z])`)
	// excludePattern matches a git exclude of the workspace-root marker.
	excludePattern = regexp.MustCompile(`"/"\s*\+\s*(?:provision\.)?ProvisionSentinelFile|"/\.scion-provisioned"`)
)

// legacyMarkerViolation reports whether line names the workspace-root
// marker or lock outside the state directory.
func legacyMarkerViolation(line string) bool {
	if excludePattern.MatchString(line) {
		return true
	}
	for _, m := range joinWithMarker.FindAllStringSubmatch(line, -1) {
		args := m[1]
		if strings.Contains(args, "ProjectStateDir(") || strings.Contains(args, "ProvisionStateDirName") {
			continue
		}
		for _, arg := range strings.Split(args, ",") {
			if workspaceArg.MatchString(strings.TrimSpace(arg)) {
				return true
			}
		}
	}
	return false
}

// The matcher itself: it must flag the shapes the dropped legacy code had,
// and pass the state-directory shapes current code uses.
func TestLegacyMarkerViolation_Matcher(t *testing.T) {
	flagged := []string{
		`if _, err := os.Lstat(filepath.Join(ws, ProvisionSentinelFile)); err != nil {`,
		`p := filepath.Join(workspace, provision.ProvisionSentinelFile)`,
		`lock := filepath.Join(in.Resolved.HostPath, provisionFileLockName)`,
		`filepath.Join(hostBase, rel, provision.ProvisionSentinelFile)`,
		`filepath.Join(dest, ".scion-provision.lock")`,
		`filepath.Join(workspacePath, ".scion-provisioned")`,
		`appendGitExclude(ws, "/"+ProvisionSentinelFile)`,
		`patterns = append(patterns, "/.scion-provisioned")`,
	}
	for _, line := range flagged {
		if !legacyMarkerViolation(line) {
			t.Errorf("not flagged: %s", line)
		}
	}
	passed := []string{
		`sentinelPath := filepath.Join(sentinelDir, ProvisionSentinelFile)`,
		`lockPath := filepath.Join(dir, provisionFileLockName)`,
		`sentinel := filepath.Join(agentDir, ProvisionSentinelFile)`,
		`os.Lstat(filepath.Join(hostBase, provision.ProjectStateDir(rel), provision.ProvisionSentinelFile))`,
		`filepath.Join(stateDir, provision.ProvisionSentinelFile)`,
	}
	for _, line := range passed {
		if legacyMarkerViolation(line) {
			t.Errorf("flagged: %s", line)
		}
	}
}

func TestNoWorkspaceRootMarkerOutsidePromotion(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repository root not found at %s: %v", root, err)
	}
	scanned := 0
	for _, top := range []string{"pkg", "cmd", "extras"} {
		dir := filepath.Join(root, top)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", "testdata", ".git":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			if legacyMarkerAllowList[rel] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			for i, line := range strings.Split(string(data), "\n") {
				if legacyMarkerViolation(line) {
					t.Errorf("%s:%d names the workspace-root provisioning marker or lock outside the promotion step: %s", rel, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned == 0 {
		t.Fatal("no Go sources scanned")
	}
}
