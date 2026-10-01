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

package hub

import (
	"fmt"
	"os"
)

// isolateTestHome points $HOME at a freshly created temporary directory for
// the lifetime of the test binary. pkg/hub tests resolve the scion config
// root (~/.scion/projects, the remote-templates cache, etc.) through $HOME
// (most code calls os.UserHomeDir(), but some reads os.Getenv("HOME")
// directly). On Linux and macOS both read the same $HOME, so overriding it
// here keeps scion state (~/.scion/...) off the real developer/agent HOME.
// os.UserHomeDir() uses %USERPROFILE% on Windows instead, so this isolation
// is Linux/macOS scoped.
//
// This does not reach package init code in dependencies that runs before
// TestMain (e.g. rclone's fs/config creating its own config dir) — that is
// out of scope here.
//
// After pointing $HOME at the scratch directory, this also asserts that
// os.UserHomeDir() resolves to it in this process, as a cheap sanity check
// that the override actually took effect before any test runs.
//
// Call this once from TestMain, before m.Run(). Individual tests that need a
// specific HOME continue to use t.Setenv("HOME", ...); Go restores the
// isolated value set here once such a test finishes, so they keep working
// unchanged.
//
// Returns a teardown func that removes the scratch directory. It does not
// restore $HOME, since TestMain calls it right before os.Exit.
func isolateTestHome() (teardown func()) {
	tmpHome, err := os.MkdirTemp("", "scion-hub-test-home-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolateTestHome: creating scratch HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", tmpHome); err != nil {
		fmt.Fprintf(os.Stderr, "isolateTestHome: setting HOME: %v\n", err)
		os.Exit(1)
	}

	if got, err := os.UserHomeDir(); err != nil || got != tmpHome {
		fmt.Fprintf(os.Stderr, "isolateTestHome: os.UserHomeDir() = %q, err=%v; want %q — HOME override did not take effect in this process\n", got, err, tmpHome)
		os.Exit(1)
	}

	return func() { _ = os.RemoveAll(tmpHome) }
}
