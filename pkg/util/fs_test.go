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

package util

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFile(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	srcFile := filepath.Join(srcDir, "test.txt")
	content := []byte("hello world")
	// Use 0644 permissions
	if err := os.WriteFile(srcFile, content, 0644); err != nil {
		t.Fatal(err)
	}

	dstFile := filepath.Join(dstDir, "test_copy.txt")

	if err := CopyFile(srcFile, dstFile); err != nil {
		t.Fatalf("CopyFile failed: %v", err)
	}

	// Verify content
	got, err := os.ReadFile(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: got %q, want %q", string(got), string(content))
	}

	// Verify permissions
	info, err := os.Stat(dstFile)
	if err != nil {
		t.Fatal(err)
	}
	// Check specifically for user read/write (0600 part) as umask might affect group/world
	if info.Mode()&0600 != 0600 {
		t.Errorf("permission mismatch: got %v, expected at least 0600", info.Mode())
	}
}

func TestCopyDir(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Create structure:
	// src/
	//   file1.txt
	//   subdir/
	//     file2.txt

	if err := os.WriteFile(filepath.Join(srcDir, "file1.txt"), []byte("file1"), 0644); err != nil {
		t.Fatal(err)
	}

	subDir := filepath.Join(srcDir, "subdir")
	if err := os.Mkdir(subDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(subDir, "file2.txt"), []byte("file2"), 0644); err != nil {
		t.Fatal(err)
	}

	targetDir := filepath.Join(dstDir, "target")

	if err := CopyDir(srcDir, targetDir); err != nil {
		t.Fatalf("CopyDir failed: %v", err)
	}

	// Verify file1
	got1, err := os.ReadFile(filepath.Join(targetDir, "file1.txt"))
	if err != nil {
		t.Errorf("file1 not found: %v", err)
	} else if string(got1) != "file1" {
		t.Errorf("file1 content mismatch")
	}

	// Verify file2
	got2, err := os.ReadFile(filepath.Join(targetDir, "subdir", "file2.txt"))
	if err != nil {
		t.Errorf("file2 not found: %v", err)
	} else if string(got2) != "file2" {
		t.Errorf("file2 content mismatch")
	}
}

func TestMakeWritableRecursive(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a read-only file
	readOnlyFile := filepath.Join(tmpDir, "readonly.txt")
	if err := os.WriteFile(readOnlyFile, []byte("readonly"), 0400); err != nil {
		t.Fatal(err)
	}

	// Create a subdirectory
	readOnlySubDir := filepath.Join(tmpDir, "readonlydir")
	if err := os.Mkdir(readOnlySubDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Create a file inside that directory
	fileInDir := filepath.Join(readOnlySubDir, "file.txt")
	if err := os.WriteFile(fileInDir, []byte("file"), 0400); err != nil {
		t.Fatal(err)
	}

	// NOW make the directory read-only
	if err := os.Chmod(readOnlySubDir, 0500); err != nil {
		t.Fatal(err)
	}

	// Ensure they are indeed read-only (u+w is NOT set)
	info, _ := os.Stat(readOnlyFile)
	if info.Mode().Perm()&0200 != 0 {
		t.Fatal("file should be read-only")
	}

	// Run the function
	if err := MakeWritableRecursive(tmpDir); err != nil {
		t.Fatalf("MakeWritableRecursive failed: %v", err)
	}

	// Verify they are now writable
	info, _ = os.Stat(readOnlyFile)
	if info.Mode().Perm()&0200 == 0 {
		t.Error("file should be writable now")
	}

	info, _ = os.Stat(readOnlySubDir)
	if info.Mode().Perm()&0200 == 0 {
		t.Error("subdir should be writable now")
	}

	info, _ = os.Stat(fileInDir)
	if info.Mode().Perm()&0200 == 0 {
		t.Error("file in subdir should be writable now")
	}

	// Verify we can now remove all
	if err := os.RemoveAll(tmpDir); err != nil {
		t.Errorf("os.RemoveAll failed even after MakeWritableRecursive: %v", err)
	}
}
