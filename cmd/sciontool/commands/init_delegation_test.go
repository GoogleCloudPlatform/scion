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

package commands

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/delegation"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
)

func TestStartDelegationHelper_ServesSocketNextToTokenFile(t *testing.T) {
	home := t.TempDir()
	t.Cleanup(hub.SetTokenHome(home))
	want := filepath.Join(home, ".scion", delegation.SocketName)
	if got := delegationSocketPath(); got != want {
		t.Fatalf("delegationSocketPath() = %q, want %q", got, want)
	}

	h := startDelegationHelper(hub.NewClientWithConfig("http://127.0.0.1:1", "agent-jwt", "agent-1"), 0, 0)
	if h == nil {
		t.Fatal("startDelegationHelper returned nil with an isolated token home")
	}
	fi, err := os.Lstat(want)
	if err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want a socket with mode 0600", fi.Mode())
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(want); !os.IsNotExist(err) {
		t.Fatalf("socket still present after Close (err=%v)", err)
	}
}
