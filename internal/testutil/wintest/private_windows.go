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

package wintest

import "testing"

// Helpers for the Windows private-file site tests: each refusing site
// test makes a file too open, and each writing site test checks the
// result is private.

// MakeUsersReadable gives path a protected DACL granting the current user
// and SYSTEM full access and BUILTIN\Users read access: a file that fails
// privatefs.CheckPrivate.
func MakeUsersReadable(t testing.TB, path string) {
	t.Helper()
	u := UserSID(t)
	SetDACL(t, path, "D:P(A;;FA;;;"+u+")(A;;FA;;;"+SIDSystem+")(A;;FR;;;"+SIDUsers+")")
	if !ReadSD(t, path).HasAllowACE(SIDUsers) {
		t.Fatalf("setup: %s did not get the Users ACE", path)
	}
}

// AssertPrivateSD fails the test unless path's security descriptor is the
// privatefs private one: owner the current user, a protected DACL, and
// allow ACEs for the current user and SYSTEM only.
func AssertPrivateSD(t testing.TB, path string) {
	t.Helper()
	u := UserSID(t)
	sd := ReadSD(t, path)
	if sd.Owner != u || !sd.Protected || sd.NullDACL || len(sd.ACEs) == 0 {
		t.Fatalf("%s: SD %+v; want owner %s and a protected owner-only DACL", path, sd, u)
	}
	for _, a := range sd.ACEs {
		if a.SID != u && a.SID != SIDSystem {
			t.Fatalf("%s: SD %+v grants %s; want the current user and SYSTEM only", path, sd, a.SID)
		}
	}
}
