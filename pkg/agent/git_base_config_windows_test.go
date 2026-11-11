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

package agent

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestWindowsGitBaseConfig_Unsupported checks that the base git config read
// fails closed on Windows with errors.ErrUnsupported on an existing file, and
// that seedPrivateGitConfig returns the refusal and creates nothing under the
// agent dir.
func TestWindowsGitBaseConfig_Unsupported(t *testing.T) {
	base := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(base, []byte("[core]\n\tbare = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := readBaseGitConfig(base); !errors.Is(err, errors.ErrUnsupported) || data != nil {
		t.Fatalf("readBaseGitConfig = %q, %v; want nil, ErrUnsupported", data, err)
	}

	agentDir := t.TempDir()
	dest, err := seedPrivateGitConfig(base, agentDir, "agent-a")
	if !errors.Is(err, errors.ErrUnsupported) || dest != "" {
		t.Fatalf("seedPrivateGitConfig = %q, %v; want empty, ErrUnsupported", dest, err)
	}
	if _, err := os.Lstat(filepath.Join(agentDir, "git")); !os.IsNotExist(err) {
		t.Fatalf("git dir created after a refusal: %v", err)
	}
}
