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

//go:build windows

package tunnelclient

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// checkPrivatePath runs CheckPrivate on a fresh handle of path. The handle
// shares delete access: a live lock file is held by a handle with DELETE
// access, which os.Open (no FILE_SHARE_DELETE) would collide with.
func checkPrivatePath(t *testing.T, path string) error {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	f := os.NewFile(uintptr(h), path)
	defer func() { _ = f.Close() }()
	return privatefs.CheckPrivate(f)
}

// End to end on Windows: the lock is created and locked under a temporary
// name and renamed into place while held (design F open question 4), the
// record is replaced by handle, and every file and the directory carry the
// owner-only DACL.
func TestFileStore_OpenWriteListClose(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPrivatePath(t, st.Dir); err != nil {
		t.Fatalf("state dir: %v", err)
	}
	if sd := wintest.ReadSD(t, st.Dir); !sd.Protected {
		t.Fatalf("state dir DACL not protected: %+v", sd)
	}
	rec := sampleRecord("https://hub.example.com", "u1", "s1", "a1")
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatalf("second Write (replace by handle): %v", err)
	}
	got, err := st.List()
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], rec) {
		t.Fatalf("List = %+v, %v; want the record", got, err)
	}
	entries, _ := os.ReadDir(st.Dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if err := checkPrivatePath(t, filepath.Join(st.Dir, e.Name())); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
	}
	locks, records := 0, 0
	for _, n := range names {
		switch {
		case strings.HasSuffix(n, lockSuffix):
			locks++
		case strings.HasSuffix(n, recordSuffix):
			records++
		}
	}
	if len(names) != 2 || locks != 1 || records != 1 {
		t.Fatalf("files %v, want one lock and one record", names)
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

func TestFileStore_RefusesUsersReadableRecord(t *testing.T) {
	st := testStore(t)
	w, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Write(sampleRecord("https://hub.example.com", "u1", "s1")); err != nil {
		t.Fatal(err)
	}
	// Control: the private record lists.
	if got, err := st.List(); err != nil || len(got) != 1 {
		t.Fatalf("List of a private record = %v, %v", got, err)
	}
	recs, _ := filepath.Glob(filepath.Join(st.Dir, "*"+recordSuffix))
	if len(recs) != 1 {
		t.Fatalf("records %v", recs)
	}
	wintest.Icacls(t, recs[0], "/grant", "*"+wintest.SIDUsers+":R")
	if _, err := st.List(); !errors.Is(err, privatefs.ErrNotPrivate) || !strings.Contains(err.Error(), wintest.SIDUsers) {
		t.Fatalf("List of a Users-readable record: %v, want a refusal naming %s", err, wintest.SIDUsers)
	}
	// A Users-readable state directory is refused by List and Open alike.
	wintest.Icacls(t, recs[0], "/remove:g", "*"+wintest.SIDUsers)
	if _, err := st.List(); err != nil {
		t.Fatalf("control: List after removing the grant: %v", err)
	}
	wintest.Icacls(t, st.Dir, "/grant", "*"+wintest.SIDUsers+":R")
	if _, err := st.List(); !errors.Is(err, privatefs.ErrNotPrivate) {
		t.Fatalf("List of a Users-readable directory: %v, want ErrNotPrivate", err)
	}
	if _, err := st.Open(); !errors.Is(err, privatefs.ErrNotPrivate) {
		t.Fatalf("Open with a Users-readable directory: %v, want ErrNotPrivate", err)
	}
}

func TestFileStore_RefusesJunctionedStateDir(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "scion")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	st := FileStore{Dir: filepath.Join(parent, "tunnels")}
	wintest.MustJunction(t, elsewhere, st.Dir)
	if _, err := st.Open(); !errors.Is(err, privatefs.ErrReparsePoint) {
		t.Fatalf("Open with a junctioned state dir: %v, want ErrReparsePoint", err)
	}
	if _, err := st.List(); !errors.Is(err, privatefs.ErrReparsePoint) {
		t.Fatalf("List with a junctioned state dir: %v, want ErrReparsePoint", err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Fatalf("files written behind the junction: %v", entries)
	}
	// Control: a real state directory in the same parent works.
	ok := FileStore{Dir: filepath.Join(parent, "tunnels-real")}
	w, err := ok.Open()
	if err != nil {
		t.Fatalf("control: Open with a real state dir: %v", err)
	}
	_ = w.Close()
}

func TestFileStore_PrunesDeadProcess(t *testing.T) {
	st := testStore(t)
	live, err := st.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	if err := live.Write(sampleRecord("https://hub.example.com", "u1", "s-live")); err != nil {
		t.Fatal(err)
	}
	// A dead process: a record and a lock file nobody holds. Files created
	// in the private directory inherit its owner-only ACEs.
	dead := filepath.Join(st.Dir, "999-dead")
	data, _ := json.Marshal(sampleRecord("https://hub.example.com", "u1", "s-dead"))
	if err := os.WriteFile(dead+recordSuffix, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dead+lockSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].SessionIDs, []string{"s-live"}) {
		t.Fatalf("List = %+v, want only the live record", got)
	}
	for _, p := range []string{dead + recordSuffix, dead + lockSuffix} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Fatalf("%s not pruned: %v", p, err)
		}
	}
	// Control: the live process's lock and record are kept.
	if locks, _ := filepath.Glob(filepath.Join(st.Dir, "*"+lockSuffix)); len(locks) != 1 {
		t.Fatalf("live lock files after List = %v, want one", locks)
	}
}
