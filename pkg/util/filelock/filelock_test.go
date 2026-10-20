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

package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquire_CreatesExcludesAndKeepsFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Created {
		t.Error("Created = false for a new lock file")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}

	// A second Acquire on the same path, even in this process, waits for
	// its deadline and fails: the lock is not re-entrant.
	start := time.Now()
	if _, err := Acquire(path, 0600, 100*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("second Acquire: err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d < 90*time.Millisecond || d > 5*time.Second {
		t.Errorf("waited %s, want about 100ms", d)
	}

	l.Unlock()
	l.Unlock() // idempotent
	l2, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatalf("Acquire after Unlock: %v", err)
	}
	if l2.Created {
		t.Error("Created = true for an existing lock file")
	}
	l2.Unlock()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file removed after Unlock: %v", err)
	}
}

func TestAcquire_WaitsForRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		l.Unlock()
	}()
	l2, err := Acquire(path, 0600, 5*time.Second)
	if err != nil {
		t.Fatalf("Acquire while the holder releases: %v", err)
	}
	l2.Unlock()
}

func TestAcquire_RefusesSymlinkAndDirectory(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(link, 0600, 0); !errors.Is(err, ErrNotRegular) {
		t.Errorf("symlink: err = %v, want ErrNotRegular", err)
	}
	sub := filepath.Join(dir, "sub.lock")
	if err := os.Mkdir(sub, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(sub, 0600, 0); err == nil {
		t.Error("directory: Acquire succeeded")
	}
}

// An existing lock file this user may not write (another user's, or mode
// 0444) is opened read-only and still excludes other holders.
func TestAcquire_ReadOnlyLockFileStillLocks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens any file for writing")
	}
	path := filepath.Join(t.TempDir(), "x.lock")
	if err := os.WriteFile(path, nil, 0444); err != nil {
		t.Fatal(err)
	}
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatalf("Acquire of a read-only lock file: %v", err)
	}
	defer l.Unlock()
	if l.Created {
		t.Error("Created = true for an existing lock file")
	}
	if _, err := Acquire(path, 0600, 50*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Errorf("second Acquire: err = %v, want ErrTimeout (the read-only lock must exclude)", err)
	}
}
