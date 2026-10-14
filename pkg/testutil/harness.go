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

package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// HarnessDialectPath returns the path of harnesses/<harness>/dialect.yaml,
// the dialect file staged into agents of that harness, resolved from this
// source file's location so it does not depend on the test's working
// directory. It fails the test if the file does not exist.
func HarnessDialectPath(t testing.TB, harness string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	path := filepath.Join(root, "harnesses", harness, "dialect.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("staged dialect file for harness %q: %v", harness, err)
	}
	return path
}
