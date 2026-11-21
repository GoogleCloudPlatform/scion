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

package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/GoogleCloudPlatform/scion/pkg/util/rootopen"
)

func TestReadRegularInRoot_OpenPolicy(t *testing.T) {
	dir := t.TempDir()
	secDir := filepath.Join(dir, ".scion", "harness", "secrets")
	if err := os.MkdirAll(secDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secFile := filepath.Join(secDir, "API_KEY")
	if err := os.WriteFile(secFile, []byte("secret-val"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(dir, "other.txt")
	if err := os.WriteFile(otherFile, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()

	rel := ".scion/harness/secrets/API_KEY"
	fi, err := root.Lstat(rel)
	if err != nil {
		t.Fatal(err)
	}

	got, err := readRegularInRoot(root, rel, fi, 1024)
	if err != nil || string(got) != "secret-val" {
		t.Fatalf("readRegularInRoot(regular) = (%q, %v)", got, err)
	}

	t.Run("swapped_fifo_does_not_block", func(t *testing.T) {
		fifoTarget := filepath.Join(secDir, "FIFO_KEY")
		if err := os.WriteFile(fifoTarget, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		fifoRel := ".scion/harness/secrets/FIFO_KEY"
		fifoInfo, err := root.Lstat(fifoRel)
		if err != nil {
			t.Fatal(err)
		}
		restore := rootopen.SetBeforeOpenHookForTest(func(name string) {
			if name == fifoRel {
				_ = os.Remove(fifoTarget)
				_ = unix.Mkfifo(fifoTarget, 0o600)
			}
		})
		defer restore()

		done := make(chan error, 1)
		go func() {
			_, err := readRegularInRoot(root, fifoRel, fifoInfo, 1024)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("readRegularInRoot succeeded on swapped FIFO")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("readRegularInRoot blocked on swapped FIFO")
		}
	})

	t.Run("in_root_intermediate_symlink_refused", func(t *testing.T) {
		altDir := filepath.Join(dir, "alt")
		if err := os.MkdirAll(altDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(altDir, "KEY"), []byte("alt"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("alt", filepath.Join(dir, "linkdir")); err != nil {
			t.Fatal(err)
		}
		linkRel := "linkdir/KEY"
		linkInfo, err := root.Lstat(linkRel)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := readRegularInRoot(root, linkRel, linkInfo, 1024); err == nil {
			t.Fatal("readRegularInRoot succeeded through in-root intermediate symlink")
		}
	})
}
