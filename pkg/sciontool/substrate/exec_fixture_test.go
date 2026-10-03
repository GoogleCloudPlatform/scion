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

package substrate

import (
	"os"
	"os/user"
	"strconv"
	"testing"
)

// withExecUserAsCurrent makes the exec user "scion" resolve to this test
// process's own uid/gid, with a fresh temporary directory as its home, for
// the rest of the test. It returns that home directory.
//
// Tests that only need "a runnable exec" use it so they run the same way on
// every host: whether or not a real "scion" account exists (CI runners have
// none), and whatever that account's own home directory looks like. Because
// the mapped uid/gid equal this process's own euid/egid, runExec takes the
// same-identity path and requests no credential drop, so no privilege is
// needed. Any other name resolves as an unknown user.
func withExecUserAsCurrent(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	withExecUserHome(t, home)
	return home
}

// withExecUserHome is withExecUserAsCurrent with a caller-chosen home
// directory, which need not exist.
func withExecUserHome(t *testing.T, home string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: the current identity is uid 0, which exec refuses")
	}
	mapped := &user.User{
		Uid:      strconv.Itoa(os.Geteuid()),
		Gid:      strconv.Itoa(os.Getegid()),
		Username: "scion",
		HomeDir:  home,
	}
	restore := SetExecUserLookupForTest(func(username string) (*user.User, error) {
		if username != "scion" {
			return nil, user.UnknownUserError(username)
		}
		return mapped, nil
	})
	t.Cleanup(restore)
}
