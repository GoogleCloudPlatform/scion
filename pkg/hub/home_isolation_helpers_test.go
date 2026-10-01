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
	"path/filepath"
)

// realHomeBeforeIsolation is captured by isolateTestHome before $HOME is
// pointed at a scratch directory, so the post-run guard in
// verifyRealHomeUntouched can confirm nothing leaked to it. It is only ever
// written once, from TestMain, before any test runs.
var realHomeBeforeIsolation string

// isolateTestHome points $HOME at a freshly created temporary directory for
// the lifetime of the test binary. pkg/hub tests resolve the scion config
// root (~/.scion/projects, the remote-templates cache, etc.) exclusively
// through os.UserHomeDir(), so overriding $HOME here is sufficient to keep
// the whole suite off the real developer/agent HOME.
//
// Call this once from TestMain, before m.Run(). Individual tests that need a
// specific HOME continue to use t.Setenv("HOME", ...); Go restores the
// isolated value set here once such a test finishes, so they keep working
// unchanged.
//
// Returns a teardown func that removes the scratch directory. It does not
// restore $HOME, since TestMain calls it right before os.Exit.
func isolateTestHome() (teardown func()) {
	realHomeBeforeIsolation = os.Getenv("HOME")

	tmpHome, err := os.MkdirTemp("", "scion-hub-test-home-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "isolateTestHome: creating scratch HOME: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", tmpHome); err != nil {
		fmt.Fprintf(os.Stderr, "isolateTestHome: setting HOME: %v\n", err)
		os.Exit(1)
	}

	return func() { _ = os.RemoveAll(tmpHome) }
}

// verifyRealHomeUntouched is the regression guard for
// https://github.com/ptone/scion/issues/2417: it fails the test run if the
// real HOME (captured by isolateTestHome before the override) gained the
// directories the issue called out during the run. A non-nil result means
// some code path resolved the config root without honouring the $HOME
// override installed by isolateTestHome.
func verifyRealHomeUntouched() error {
	realHome := realHomeBeforeIsolation
	if realHome == "" {
		// Nothing was captured (isolateTestHome never ran, or the
		// environment had no HOME to begin with); nothing to check.
		return nil
	}

	suspects := []string{
		filepath.Join(realHome, ".scion", "projects"),
		filepath.Join(realHome, ".scion", "cache", "remote-templates"),
	}
	for _, p := range suspects {
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("HOME isolation regressed: real HOME %s gained %s during the pkg/hub test run", realHome, p)
		}
	}
	return nil
}
