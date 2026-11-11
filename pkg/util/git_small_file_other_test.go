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

//go:build !unix

package util

import (
	"os"
	"path/filepath"
	"testing"
)

// swapOpenForTest makes the non-unix open step open other instead of the
// path it was given, as if the open were redirected (for example through a
// link) to a different file than the one at path.
func swapOpenForTest(t *testing.T, other string) {
	t.Helper()
	orig := openForLstatVerify
	openForLstatVerify = func(string) (*os.File, error) { return os.Open(other) }
	t.Cleanup(func() { openForLstatVerify = orig })
}

func writeSwapFixtures(t *testing.T) (target, other string) {
	t.Helper()
	dir := t.TempDir()
	target = filepath.Join(dir, "target")
	other = filepath.Join(dir, "other")
	if err := os.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	return target, other
}

func TestReadSmallRegularFile_RefusesSwapBeforeOpen(t *testing.T) {
	target, other := writeSwapFixtures(t)
	swapOpenForTest(t, other)
	if data, err := readSmallRegularFile(target, 64); err == nil {
		t.Fatalf("got %q; want refusal of a file swapped in before open", data)
	}
}

func TestReadRegularFilePrefix_RefusesSwapBeforeOpen(t *testing.T) {
	target, other := writeSwapFixtures(t)
	swapOpenForTest(t, other)
	if data, ok := readRegularFilePrefix(target, 64); ok {
		t.Fatalf("got %q; want refusal of a file swapped in before open", data)
	}
}
