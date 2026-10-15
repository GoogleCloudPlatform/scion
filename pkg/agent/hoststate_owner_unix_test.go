// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build unix

package agent

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// A .scion that is neither a file nor a directory (here a FIFO) gives no
// root, and nothing is written.
func TestHostStateRootFor_ScionNotFileOrDirRefused(t *testing.T) {
	l := newOwnerLayout(t)
	p := filepath.Join(l.src, "fifo")
	mkdirOwnerDirs(t, p)
	if err := syscall.Mkfifo(filepath.Join(p, config.DotScion), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	target := RunOwnerTarget{ProjectPath: p, AgentName: ownerAgent, HubProjectID: ownerHubID}
	if got, err := HostStateRootFor(target); !errors.Is(err, errHostStateRootRefused) {
		t.Fatalf("HostStateRootFor = %q, %v; want a refusal", got, err)
	}
	if err := RecordRunOwner(target, "run-1"); !errors.Is(err, errHostStateRootRefused) {
		t.Fatalf("RecordRunOwner = %v, want a refusal", err)
	}
	if files := hostStateFilesUnder(t, filepath.Dir(l.home)); len(files) != 0 {
		t.Errorf("host state files written: %v", files)
	}
}
