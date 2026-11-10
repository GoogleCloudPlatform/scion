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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenCollectedFile_StreamsUnchanged pins that an unchanged collected
// file streams byte for byte and ends with io.EOF.
func TestOpenCollectedFile_StreamsUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "hello\n")
	root, fi := collectOne(t, dir, "a.txt")

	rc, err := OpenCollectedFile(root, fi)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("got %q, %v; want %q", data, err, "hello\n")
	}
}

// TestOpenCollectedFile_ChangedWhileOpenFailsAtEnd pins that content
// changed after the open, which passes the up-front checks, fails the read
// that reaches the end instead of returning io.EOF.
func TestOpenCollectedFile_ChangedWhileOpenFailsAtEnd(t *testing.T) {
	cases := map[string]struct {
		change  func(f *os.File) error
		wantErr string
	}{
		"same size": {
			change:  func(f *os.File) error { _, err := f.WriteAt([]byte("HELLO\n"), 0); return err },
			wantErr: "content hash differs",
		},
		"grown": {
			change:  func(f *os.File) error { _, err := f.WriteAt([]byte("hello\nmore\n"), 0); return err },
			wantErr: "read more than the collected",
		},
		"shrunk": {
			change:  func(f *os.File) error { return f.Truncate(3) },
			wantErr: "read 3 bytes, collected 6",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeReadTestFile(t, dir, "a.txt", "hello\n")
			root, fi := collectOne(t, dir, "a.txt")

			rc, err := OpenCollectedFile(root, fi)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = rc.Close() }()

			w, err := os.OpenFile(filepath.Join(dir, "a.txt"), os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.change(w); err != nil {
				t.Fatal(err)
			}
			_ = w.Close()

			_, err = io.ReadAll(rc)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestOpenCollectedFile_SizeCheckedAtOpen pins that a size change before
// the open is refused by the open itself.
func TestOpenCollectedFile_SizeCheckedAtOpen(t *testing.T) {
	dir := t.TempDir()
	writeReadTestFile(t, dir, "a.txt", "hello\n")
	root, fi := collectOne(t, dir, "a.txt")
	writeReadTestFile(t, dir, "a.txt", "hello, world\n")

	rc, err := OpenCollectedFile(root, fi)
	if err == nil {
		_ = rc.Close()
		t.Fatal("open succeeded, want a size error")
	}
	if !strings.Contains(err.Error(), "changed since it was collected (size 13, collected 6)") {
		t.Fatalf("err = %v", err)
	}
}
