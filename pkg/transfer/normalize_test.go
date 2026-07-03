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

package transfer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNormalizeFileContent_CRLF(t *testing.T) {
	input := []byte("line1\r\nline2\r\nline3\r\n")
	want := []byte("line1\nline2\nline3\n")
	got := NormalizeFileContent(input)
	if string(got) != string(want) {
		t.Errorf("NormalizeFileContent CRLF:\n got %q\nwant %q", got, want)
	}
}

func TestNormalizeFileContent_LF(t *testing.T) {
	input := []byte("line1\nline2\nline3\n")
	got := NormalizeFileContent(input)
	if string(got) != string(input) {
		t.Errorf("NormalizeFileContent should not modify LF-only content:\n got %q\nwant %q", got, input)
	}
}

func TestNormalizeFileContent_Binary(t *testing.T) {
	input := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00}
	got := NormalizeFileContent(input)
	if string(got) != string(input) {
		t.Errorf("NormalizeFileContent should not modify binary content")
	}
}

func TestNormalizeFileContent_Empty(t *testing.T) {
	got := NormalizeFileContent(nil)
	if len(got) != 0 {
		t.Errorf("NormalizeFileContent(nil) should return empty, got %q", got)
	}
}

func TestNormalizeFileContent_MixedLineEndings(t *testing.T) {
	input := []byte("line1\r\nline2\nline3\r\n")
	want := []byte("line1\nline2\nline3\n")
	got := NormalizeFileContent(input)
	if string(got) != string(want) {
		t.Errorf("NormalizeFileContent mixed:\n got %q\nwant %q", got, want)
	}
}

func TestNormalizeDir(t *testing.T) {
	dir := t.TempDir()

	crlfContent := []byte("hello\r\nworld\r\n")
	lfContent := []byte("already\nnormalized\n")
	binContent := []byte{0x00, 0x01, '\r', '\n', 0x02}

	if err := os.WriteFile(filepath.Join(dir, "crlf.txt"), crlfContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lf.txt"), lfContent, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "binary.bin"), binContent, 0644); err != nil {
		t.Fatal(err)
	}

	if err := NormalizeDir(dir); err != nil {
		t.Fatalf("NormalizeDir failed: %v", err)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "crlf.txt"))
	if string(got) != "hello\nworld\n" {
		t.Errorf("crlf.txt not normalized: %q", got)
	}

	got, _ = os.ReadFile(filepath.Join(dir, "lf.txt"))
	if string(got) != string(lfContent) {
		t.Errorf("lf.txt was modified unexpectedly: %q", got)
	}

	got, _ = os.ReadFile(filepath.Join(dir, "binary.bin"))
	if string(got) != string(binContent) {
		t.Errorf("binary.bin was modified: %q", got)
	}
}

func TestNormalizeDir_SameHashAcrossPaths(t *testing.T) {
	content := "harness: claude\r\nimage: test:latest\r\n"

	// Simulate the embedded path: normalize content, then hash
	normalizedData := NormalizeFileContent([]byte(content))
	embeddedHash := HashBytes(normalizedData)

	// Simulate the import path: write CRLF to disk, normalize dir, then hash
	dir := t.TempDir()
	filePath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(filePath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeDir(dir); err != nil {
		t.Fatal(err)
	}
	importHash, err := HashFile(filePath)
	if err != nil {
		t.Fatal(err)
	}

	if embeddedHash != importHash {
		t.Errorf("hash mismatch between paths:\n  embedded: %s\n  import:   %s", embeddedHash, importHash)
	}
}
