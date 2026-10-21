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

package brokercredentials

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/util/filelock"
)

func lockTestCreds(name string) *BrokerCredentials {
	return &BrokerCredentials{
		Name:        name,
		BrokerID:    "broker-123",
		SecretKey:   base64.StdEncoding.EncodeToString([]byte("secret")),
		HubEndpoint: "https://hub.example.com",
		AuthMode:    AuthModeHMAC,
	}
}

// Save writes atomically under the directory lock: the file has mode 0600,
// no temporary file is left, the lock file stays, and List ignores it.
func TestMultiStore_SaveIsAtomicAndLocked(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)
	if err := store.Save(lockTestCreds("hub-a")); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(lockTestCreds("hub-a")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != lockFileName+",hub-a.json" {
		t.Errorf("files = %v", names)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "hub-a.json")); fi.Mode().Perm() != FileMode {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	list, err := store.List()
	if err != nil || len(list) != 1 {
		t.Errorf("List = %v, %v", list, err)
	}
}

// While another holder has the directory lock, Save fails after the bounded
// wait with a clear error and writes nothing.
func TestMultiStore_SaveTimesOutWhileLocked(t *testing.T) {
	dir := t.TempDir()
	store := NewMultiStore(dir)
	l, err := filelock.Acquire(filepath.Join(dir, lockFileName), FileMode, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	old := lockWait
	lockWait = 100 * time.Millisecond
	t.Cleanup(func() { lockWait = old })

	err = store.Save(lockTestCreds("hub-a"))
	if !errors.Is(err, filelock.ErrTimeout) || !strings.Contains(err.Error(), "try again") {
		t.Fatalf("err = %v, want a lock timeout", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hub-a.json")); !os.IsNotExist(err) {
		t.Errorf("credentials written despite the timeout: %v", err)
	}
	if err := store.MigrateFromLegacy(filepath.Join(t.TempDir(), "missing.json")); err != nil {
		t.Errorf("MigrateFromLegacy with no legacy file must not need the lock: %v", err)
	}
}

// A credentials lock file created inside a git work tree is kept out of git.
func TestMultiStore_LockFileIsGitIgnored(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	store := NewMultiStore(filepath.Join(repo, "hub-credentials"))
	if err := store.Save(lockTestCreds("hub-a")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v\n%s", err, out)
	}
	if strings.Contains(string(out), lockFileName) {
		t.Errorf("lock file shows up in git status:\n%s", out)
	}
	if !strings.Contains(string(out), "hub-a.json") {
		t.Errorf("only the lock file should be excluded:\n%s", out)
	}
}
