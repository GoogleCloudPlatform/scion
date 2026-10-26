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

package hubprofile

// Windows site tests for the hub profile files (run cache, entries,
// profiles). Every refusal has its positive control in the same test.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// assertPrivateSD: path's security descriptor is the private one, owner
// the user, a protected DACL granting FILE_ALL_ACCESS to exactly the user
// and SYSTEM. (An object that merely inherits the temp directory's ACL
// would pass CheckPrivate; it fails here.)
func assertPrivateSD(t *testing.T, path string) {
	t.Helper()
	user := wintest.UserSID(t)
	sd := wintest.ReadSD(t, path)
	require.Equal(t, user, sd.Owner, "%s: owner", path)
	require.True(t, sd.Protected, "%s: DACL not protected", path)
	require.Len(t, sd.ACEs, 2, "%s: ACEs %+v, want exactly {user, SYSTEM}", path, sd.ACEs)
	seen := map[string]bool{}
	for _, a := range sd.ACEs {
		require.Equal(t, uint8(windows.ACCESS_ALLOWED_ACE_TYPE), a.Type, "%s: ACE %+v", path, a)
		require.Equal(t, uint32(wintest.FileAllAccess), a.Mask, "%s: ACE %+v", path, a)
		seen[a.SID] = true
	}
	require.True(t, seen[user] && seen[wintest.SIDSystem], "%s: ACEs %+v, want the user and SYSTEM", path, sd.ACEs)
}

func assertPrivateFile(t *testing.T, path string) { t.Helper(); assertPrivateSD(t, path) }
func assertPrivateDir(t *testing.T, path string)  { t.Helper(); assertPrivateSD(t, path) }

// homeWithUsersRead returns a home directory that grants Users read
// inheritably, so anything that merely inherits is visibly too open.
func homeWithUsersRead(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	wintest.Icacls(t, home, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	return home
}

// The run cache directories and the entry are created with the
// private descriptor although home grants Users read inheritably. Control:
// a file created plainly in the same home carries the Users ACE.
func TestReserveEntry_PrivateDespiteInheritableUsersACE(t *testing.T) {
	home := homeWithUsersRead(t)
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	require.NoError(t, res.Fill(sampleEntry()))
	assertPrivateSD(t, res.Path())
	for _, d := range []string{filepath.Dir(res.Path()), filepath.Dir(filepath.Dir(res.Path())), Dir(home), filepath.Join(home, ".scion")} {
		assertPrivateSD(t, d)
	}

	plain := filepath.Join(home, "plain.txt")
	require.NoError(t, os.WriteFile(plain, []byte("x"), 0o600))
	assert.True(t, wintest.ReadSD(t, plain).HasAllowACE(wintest.SIDUsers), "control: the Users ACE is inheritable")
}

// Refuse + control: an entry and a profile that grant Users read are
// refused on read, naming the SID; with the private DACL they are read.
func TestReadRefusesUsersReadableACL(t *testing.T) {
	home := t.TempDir()
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	require.NoError(t, res.Fill(sampleEntry()))
	require.NoError(t, Save(home, validProfile(), false))
	profile := filepath.Join(Dir(home), "t1.yaml")

	u := wintest.UserSID(t)
	open := fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDUsers)
	private := fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)", u, wintest.SIDSystem)

	wintest.SetDACL(t, res.Path(), open)
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.ErrorIs(t, err, privatefs.ErrNotPrivate)
	assert.Contains(t, err.Error(), wintest.SIDUsers)
	assert.Contains(t, err.Error(), "owner-only ACL")
	wintest.SetDACL(t, res.Path(), private)
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.NoError(t, err, "control: the private entry is read")

	wintest.SetDACL(t, profile, open)
	_, err = Load(home, "t1")
	require.ErrorIs(t, err, privatefs.ErrNotPrivate)
	wintest.SetDACL(t, profile, private)
	_, err = Load(home, "t1")
	require.NoError(t, err, "control: the private profile is read")
}

// Refuse + control: a junction at the run directory or at the profile
// cache directory is refused (it leads outside the tree) and nothing is
// written where it points; with real directories the reservation works.
func TestReserveEntry_RefusesJunctionedDirs(t *testing.T) {
	for _, at := range []string{"run", "profile"} {
		t.Run(at, func(t *testing.T) {
			home := t.TempDir()
			elsewhere := filepath.Join(home, "elsewhere")
			require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, "run-r1"), 0o700))
			base := Dir(home)
			require.NoError(t, os.MkdirAll(filepath.Join(base, "t1"), 0o700))
			link := filepath.Join(base, "t1", "run-r1")
			target := filepath.Join(elsewhere, "run-r1")
			if at == "profile" {
				require.NoError(t, os.Remove(filepath.Join(base, "t1")))
				link, target = filepath.Join(base, "t1"), elsewhere
			}
			wintest.MustJunction(t, target, link)

			_, err := ReserveEntry(home, "t1", "r1", "owner")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "junction")
			entries, _ := os.ReadDir(filepath.Join(elsewhere, "run-r1"))
			assert.Empty(t, entries, "nothing was written through the junction")

			// Control: the same layout with a real directory.
			require.NoError(t, os.Remove(link))
			res, err := ReserveEntry(home, "t1", "r1", "owner")
			require.NoError(t, err, "control: real directories")
			require.NoError(t, res.Fill(sampleEntry()))
		})
	}
}

// The anchor itself (~/.scion/hub-profiles) and the components above
// it are followed when they are links: a redirected profile is legitimate.
func TestReserveEntry_FollowsJunctionAboveAnchor(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(t.TempDir(), "scion-real")
	require.NoError(t, os.MkdirAll(real, 0o700))
	wintest.MustJunction(t, real, filepath.Join(home, ".scion"))
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	require.NoError(t, res.Fill(sampleEntry()))
	_, err = os.Stat(filepath.Join(real, "hub-profiles", "t1", "run-r1", "owner.json"))
	require.NoError(t, err, "the entry is under the junction's target")
}

// Replacing an entry whose DACL the user loosened gives a private
// file (atomic replace repairs), with the new content and no temporary
// file left. Control: the read before the replace is refused, so the entry
// really started too open.
func TestReplaceEntry_RepairsTooOpenEntry(t *testing.T) {
	home := t.TempDir()
	res, err := ReserveEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	e := sampleEntry()
	require.NoError(t, res.Fill(e))
	u := wintest.UserSID(t)
	wintest.SetDACL(t, res.Path(), fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDUsers))
	_, err = ReadEntry(home, "t1", "r1", "owner")
	require.True(t, errors.Is(err, privatefs.ErrNotPrivate), "precondition: too open: %v", err)

	ne := *e
	ne.Token = "FIXTURE-TOKEN-2"
	require.NoError(t, ReplaceEntry(home, &ne))
	assertPrivateSD(t, res.Path())
	got, err := ReadEntry(home, "t1", "r1", "owner")
	require.NoError(t, err)
	assert.Equal(t, "FIXTURE-TOKEN-2", got.Token)
	entries, _ := os.ReadDir(filepath.Dir(res.Path()))
	assert.Len(t, entries, 1, "no temporary file is left behind")
}
