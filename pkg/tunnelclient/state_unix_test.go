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

//go:build unix

package tunnelclient

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// POSIX-mode assertions for FileStore. The Windows equivalents, which read
// the DACL instead of mode bits, are in state_windows_test.go.

func TestFileStoreOpenWriteListClose(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(st.Dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	rec := sampleRecord("https://hub.example.com", "u1", "s1", "a1")
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], rec) {
		t.Fatalf("List = %+v, %v; want the record", got, err)
	}
	entries, _ := os.ReadDir(st.Dir)
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v, want 0600", e.Name(), info.Mode().Perm())
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := st.List(); err != nil || len(got) != 0 {
		t.Fatalf("List after Close = %+v, %v", got, err)
	}
	if entries, _ := os.ReadDir(st.Dir); len(entries) != 0 {
		t.Fatalf("files left after Close: %v", entries)
	}
}

func TestFileStoreRefusesGroupOrWorldAccess(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Write(sampleRecord("https://hub.example.com", "u1", "s1")); err != nil {
		t.Fatal(err)
	}

	for _, mode := range []os.FileMode{0o750, 0o707} {
		if err := os.Chmod(st.Dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := st.List(); err == nil || !strings.Contains(err.Error(), "group- or world-accessible") {
			t.Fatalf("dir mode %o: List = %v, want a refusal", mode, err)
		}
		if _, err := st.Open(); err == nil {
			t.Fatalf("dir mode %o: Open accepted", mode)
		}
	}
	if err := os.Chmod(st.Dir, 0o700); err != nil {
		t.Fatal(err)
	}

	recs, _ := filepath.Glob(filepath.Join(st.Dir, "*"+recordSuffix))
	if len(recs) != 1 {
		t.Fatalf("records %v", recs)
	}
	for _, mode := range []os.FileMode{0o640, 0o604} {
		if err := os.Chmod(recs[0], mode); err != nil {
			t.Fatal(err)
		}
		if _, err := st.List(); err == nil || !strings.Contains(err.Error(), "group- or world-accessible") {
			t.Fatalf("record mode %o: List = %v, want a refusal", mode, err)
		}
	}
	if err := os.Chmod(recs[0], 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := st.List(); err != nil || len(got) != 1 {
		t.Fatalf("List after restoring modes = %v, %v", got, err)
	}
}

// A FIFO planted as a record is refused by List rather than blocking it.
func TestFileStoreRefusesFIFORecord(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Write(sampleRecord("https://hub.example.com", "u1", "s1")); err != nil {
		t.Fatal(err)
	}
	// Control: the store lists before the FIFO is planted.
	if got, err := st.List(); err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v", got, err)
	}
	if err := syscall.Mkfifo(filepath.Join(st.Dir, "999-fifo"+recordSuffix), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := st.List()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("List with a FIFO record: %v, want a refusal", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("List blocked on a FIFO record")
	}
}
