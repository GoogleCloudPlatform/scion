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

package projectkeys

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// openCodedProjectIDEnvRead matches a read of the project ID environment
// variable that names it with a string literal instead of going through
// this package (EnvProjectID, ProjectIDFromEnv).
var openCodedProjectIDEnvRead = regexp.MustCompile(`(?:Getenv|LookupEnv|getenv)\(\s*"SCION_PROJECT_ID"\s*\)|const\s+\w+\s*=\s*"SCION_PROJECT_ID"`)

// The CLI binaries read the project ID from the environment only through
// this package (ptone/scion#5387), so the variable name lives in one place.
func TestCmdReadsProjectIDEnvThroughProjectkeys(t *testing.T) {
	root := filepath.Join("..", "..", "cmd")
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("cmd directory not found at %s: %v", root, err)
	}
	scanned := 0
	var offenders []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		for i, line := range strings.Split(string(data), "\n") {
			if openCodedProjectIDEnvRead.MatchString(line) {
				offenders = append(offenders, filepath.ToSlash(path)+":"+strconv.Itoa(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning %s: %v", root, err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files under %s; the walk root is wrong", scanned, root)
	}
	if len(offenders) > 0 {
		t.Errorf("read the project ID through projectkeys.ProjectIDFromEnv or projectkeys.EnvProjectID, not a literal:\n%s",
			strings.Join(offenders, "\n"))
	}
}

func TestOpenCodedProjectIDEnvReadPattern(t *testing.T) {
	for _, line := range []string{
		`id := os.Getenv("SCION_PROJECT_ID")`,
		`v, ok := os.LookupEnv("SCION_PROJECT_ID")`,
		`ProjectID: getenv("SCION_PROJECT_ID"),`,
		`const envProjectID = "SCION_PROJECT_ID"`,
	} {
		if !openCodedProjectIDEnvRead.MatchString(line) {
			t.Errorf("pattern misses %q", line)
		}
	}
	for _, line := range []string{
		`id := ProjectIDFromEnv(os.Getenv)`,
		`const envProjectID = projectkeys.EnvProjectID`,
		`env["SCION_PROJECT_ID"] = id`,
		`os.Getenv("SCION_HUB_PROJECT_ID")`,
	} {
		if openCodedProjectIDEnvRead.MatchString(line) {
			t.Errorf("pattern wrongly matches %q", line)
		}
	}
}
