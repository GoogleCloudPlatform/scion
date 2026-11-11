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

package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

// OpenNoFollow refuses a junction or symlink at any component, leaf
// included. Control: the same walk opens a real file and a real directory.
func TestOpenNoFollow_RefusesReparsePoints(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wintest.MustJunction(t, outside, filepath.Join(dir, "jdir"))
	wintest.MustSymlink(t, filepath.Join(outside, "f"), filepath.Join(dir, "slink"))
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := treeAt(t, dir)
	for _, rel := range []string{"jdir", "jdir/f", "slink"} {
		if f, err := tr.OpenNoFollow(rel); !errors.Is(err, ErrReparsePoint) {
			if f != nil {
				_ = f.Close()
			}
			t.Errorf("OpenNoFollow(%s) = %v, want ErrReparsePoint", rel, err)
		}
	}
	for _, rel := range []string{"real", "real/f"} {
		f, err := tr.OpenNoFollow(rel)
		if err != nil {
			t.Fatalf("control: OpenNoFollow(%s): %v", rel, err)
		}
		owned, err := OwnedByCurrentUser(f)
		_ = f.Close()
		if err != nil || !owned {
			t.Fatalf("control: OwnedByCurrentUser(%s) = %v, %v; want true", rel, owned, err)
		}
	}
}

// OpenNoFollow makes no privacy decision, and its handle reads no content:
// a file open to Users opens, CheckPrivate on the handle refuses it, and a
// read through the handle fails. A file owned by SYSTEM opens and reads as
// not owned. Control: a private file passes CheckPrivate on the handle.
func TestOpenNoFollow_MetadataOnly(t *testing.T) {
	dir := t.TempDir()
	tr := treeAt(t, dir)
	user := wintest.UserSID(t)
	open := mustCreate(t, tr, "open", Private, "secret")
	wintest.SetDACL(t, open, privateDACL(user)+"(A;;FR;;;"+wintest.SIDUsers+")")
	f, err := tr.OpenNoFollow("open")
	if err != nil {
		t.Fatalf("OpenNoFollow of a non-private file: %v (it must make no privacy decision)", err)
	}
	if err := CheckPrivate(f); !errors.Is(err, ErrNotPrivate) {
		t.Errorf("CheckPrivate on the OpenNoFollow handle of a Users-readable file = %v, want ErrNotPrivate", err)
	}
	if n, err := f.Read(make([]byte, 8)); err == nil {
		t.Errorf("read %d bytes through the metadata-only handle", n)
	}
	_ = f.Close()

	foreign := mustCreate(t, tr, "foreign", Private, "")
	wintest.SetOwner(t, foreign, wintest.SIDSystem)
	f, err = tr.OpenNoFollow("foreign")
	if err != nil {
		t.Fatalf("OpenNoFollow of a SYSTEM-owned file: %v", err)
	}
	owned, err := OwnedByCurrentUser(f)
	_ = f.Close()
	if err != nil || owned {
		t.Fatalf("OwnedByCurrentUser(SYSTEM-owned) = %v, %v; want false", owned, err)
	}

	mustCreate(t, tr, "private", Private, "")
	f, err = tr.OpenNoFollow("private")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := CheckPrivate(f); err != nil {
		t.Fatalf("control: CheckPrivate on the OpenNoFollow handle of a private file: %v", err)
	}
}
