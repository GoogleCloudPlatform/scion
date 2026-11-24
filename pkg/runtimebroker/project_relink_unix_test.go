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

package runtimebroker

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestRelink_CopyFailureKeepsOldDir: a copy that fails midway (an entry that
// cannot be copied) leaves the old directory and the marker unchanged and
// removes the partial copy.
func TestRelink_CopyFailureKeepsOldDir(t *testing.T) {
	f := newRelinkFixture(t)
	forceCopyPath(t)
	fifo := filepath.Join(f.oldRoot, config.DotScion, "agents", relinkAgent, "home", "zz-fifo")
	if err := syscall.Mkfifo(fifo, 0644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if err := f.relink(""); err == nil {
		t.Fatal("expected the re-link to fail")
	}
	assertState(t, f.oldRoot)
	if _, err := os.Lstat(fifo); err != nil {
		t.Fatalf("old entry removed: %v", err)
	}
	assertAbsent(t, f.newRoot)
	if got := f.markerID(t); got != relinkOldID {
		t.Fatalf("marker rewritten to %q", got)
	}
}
