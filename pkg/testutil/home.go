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

// Package testutil holds small helpers shared by test binaries across
// packages. It must only be imported from _test.go files.
package testutil

import (
	"fmt"
	"os"
)

// IsolateHome points $HOME at a freshly created temporary directory for the
// lifetime of the test binary, so tests keep scion state (~/.scion/...) off
// the real developer/agent HOME. Scion resolves its global config root
// through os.UserHomeDir() (and some code reads os.Getenv("HOME")
// directly); on Linux and macOS both read the same $HOME. os.UserHomeDir()
// uses %USERPROFILE% on Windows instead, so this isolation is Linux/macOS
// scoped.
//
// This does not reach package init code in dependencies that runs before
// TestMain (e.g. rclone's fs/config creating its own config dir).
//
// After pointing $HOME at the scratch directory, IsolateHome asserts that
// os.UserHomeDir() resolves to it, as a cheap sanity check that the override
// took effect before any test runs. Any failure exits the process, since
// running tests against the real HOME is never acceptable.
//
// Call it once from TestMain, before m.Run(). prefix names the scratch
// directory (passed to os.MkdirTemp). Individual tests that need a specific
// HOME can still use t.Setenv("HOME", ...); Go restores the isolated value
// once such a test finishes.
//
// Returns a teardown func that removes the scratch directory. It does not
// restore $HOME, since TestMain calls it right before os.Exit.
func IsolateHome(prefix string) (teardown func()) {
	tmpHome, err := os.MkdirTemp("", prefix)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testutil.IsolateHome: creating scratch HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", tmpHome); err != nil {
		fmt.Fprintf(os.Stderr, "testutil.IsolateHome: setting HOME: %v\n", err)
		os.Exit(1)
	}

	if got, err := os.UserHomeDir(); err != nil || got != tmpHome {
		fmt.Fprintf(os.Stderr, "testutil.IsolateHome: os.UserHomeDir() = %q, err=%v; want %q — HOME override did not take effect in this process\n", got, err, tmpHome)
		os.Exit(1)
	}

	return func() { _ = os.RemoveAll(tmpHome) }
}
