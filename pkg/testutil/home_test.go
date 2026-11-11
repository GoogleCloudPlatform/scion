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

package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsolateHome(t *testing.T) {
	// Restore the original HOME and USERPROFILE afterwards; IsolateHome
	// itself does not.
	t.Setenv("HOME", os.Getenv("HOME"))
	t.Setenv("USERPROFILE", os.Getenv("USERPROFILE"))
	orig, _ := os.UserHomeDir()

	teardown := IsolateHome("scion-testutil-home-*")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	// Guard BEFORE registering any removal: the safety-net cleanup below
	// must never be able to delete the real HOME, even if IsolateHome
	// regressed and left HOME unchanged.
	if home == "" || home == orig {
		t.Fatalf("HOME was not changed (still %q)", home)
	}
	// Both variables must point at the scratch dir: os.UserHomeDir() reads
	// HOME on Linux/macOS and USERPROFILE on Windows.
	if got := os.Getenv("HOME"); got != home {
		t.Fatalf("HOME = %q, want %q", got, home)
	}
	if got := os.Getenv("USERPROFILE"); got != home {
		t.Fatalf("USERPROFILE = %q, want %q", got, home)
	}
	if !strings.HasPrefix(filepath.Base(home), "scion-testutil-home-") {
		t.Fatalf("HOME = %q, want a scion-testutil-home-* scratch dir", home)
	}
	if got, want := filepath.Dir(home), filepath.Clean(os.TempDir()); got != want {
		t.Fatalf("HOME = %q, want a directory directly under %q", home, want)
	}
	// Safety net: remove the scratch dir even if a t.Fatalf below fires
	// before teardown(). RemoveAll on a missing path is a no-op, so the
	// explicit teardown check still holds.
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	if fi, err := os.Stat(home); err != nil || !fi.IsDir() {
		t.Fatalf("scratch HOME %q is not a directory: %v", home, err)
	}

	teardown()
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("teardown did not remove scratch HOME %q (stat err=%v)", home, err)
	}
}
