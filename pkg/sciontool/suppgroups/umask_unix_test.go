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

//go:build unix

package suppgroups

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// withUmaskStub stubs the umask call with a fake process umask starting at
// initial, so tests never change the test process's own umask. It returns
// a pointer to the fake current value and the list of calls.
func withUmaskStub(t *testing.T, initial int) (current *int, calls *[]int) {
	t.Helper()
	cur := initial
	var c []int
	prev := umask
	umask = func(mask int) int { c = append(c, mask); old := cur; cur = mask; return old }
	t.Cleanup(func() { umask = prev })
	return &cur, &c
}

func TestApplySharedDirUmask_Gate(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		granted []int
		gerr    error
		applied bool
	}{
		{"granted group applies", "1500", []int{0, 1500}, nil, true},
		{"env unset leaves umask unchanged", "", []int{0, 1500}, nil, false},
		{"invalid env leaves umask unchanged", "x,-1,0", []int{0, 1500}, nil, false},
		{"ungranted gid leaves umask unchanged", "2000", []int{0, 1500}, nil, false},
		{"getgroups error leaves umask unchanged", "1500", nil, errors.New("boom"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvVar, tt.env)
			withGranted(t, tt.granted, tt.gerr)
			cur, calls := withUmaskStub(t, 0o022)

			applied, previous, current := ApplySharedDirUmask()
			assert.Equal(t, tt.applied, applied)
			assert.Equal(t, tt.applied, ShouldApplyUmask())
			if tt.applied {
				assert.Equal(t, 0o022, previous)
				assert.Equal(t, 0o002, current)
				assert.Equal(t, 0o002, *cur)
			} else {
				assert.Empty(t, *calls, "umask must not be touched")
				assert.Equal(t, 0o022, *cur)
			}
		})
	}
}

// Only the group bits are cleared, so a stricter image umask keeps its
// other-bits.
func TestApplySharedDirUmask_ClearsOnlyGroupBits(t *testing.T) {
	for _, tc := range []struct{ from, want int }{
		{0o022, 0o002},
		{0o077, 0o007},
		{0o027, 0o007},
		{0o002, 0o002},
		{0o000, 0o000},
	} {
		t.Setenv(EnvVar, "1500")
		withGranted(t, []int{1500}, nil)
		cur, _ := withUmaskStub(t, tc.from)
		applied, previous, current := ApplySharedDirUmask()
		assert.True(t, applied)
		assert.Equal(t, tc.from, previous)
		assert.Equal(t, tc.want, current, "from %04o", tc.from)
		assert.Equal(t, tc.want, *cur, "from %04o", tc.from)
	}
}

// Reading the current umask must never set a looser mask, even for a
// moment: a child forked in between (substrate-serve's exec endpoint runs
// concurrently with RunInit) would keep it for life. The first call sets
// the strictest mask, 0777, never 0.
func TestApplySharedDirUmask_ReadNeverLoosens(t *testing.T) {
	t.Setenv(EnvVar, "1500")
	withGranted(t, []int{1500}, nil)
	_, calls := withUmaskStub(t, 0o022)

	applied, _, _ := ApplySharedDirUmask()
	assert.True(t, applied)
	if assert.Len(t, *calls, 2) {
		assert.NotZero(t, (*calls)[0], "first umask call must not set 0")
		assert.Equal(t, 0o777, (*calls)[0], "first umask call must set the strictest mask")
		assert.Equal(t, 0o002, (*calls)[1])
	}
}
