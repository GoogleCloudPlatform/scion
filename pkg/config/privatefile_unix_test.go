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

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// On unix, writeConflictBackup creates the backup with privatefs
// Tree.Create(excl, Private), which chmods it to 0600, so a umask stricter
// than 077 cannot leave it unwritable by its owner. Control: under the
// default umask it is 0600.
func TestWriteConflictBackup_ChmodsUnderStrictUmask(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.yaml")

	old := syscall.Umask(0o277)
	strict, err := writeConflictBackup(path, []byte("x"))
	syscall.Umask(old)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(strict); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode under umask 0277 = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	plain, err := writeConflictBackup(path, []byte("y"))
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(plain); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("control: backup mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
}
