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

package filelock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquire_CreatesWithMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	l, err := Acquire(path, 0600, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0600 {
		t.Fatalf("lock file: %v %v", fi, err)
	}
}
