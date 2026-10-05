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

package suppgroups

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func withGranted(t *testing.T, granted []int, err error) {
	t.Helper()
	prev := getgroups
	getgroups = func() ([]int, error) { return granted, err }
	t.Cleanup(func() { getgroups = prev })
}

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		granted []int
		want    []uint32
	}{
		{"unset keeps today's empty set", "", []int{0, 1500}, nil},
		{"granted gid is kept", "1500", []int{0, 1500}, []uint32{1500}},
		// The env var alone is not trusted: a gid the runtime did not grant
		// with --group-add (for example set via template or user env) is dropped.
		{"gid in env but not granted is dropped", "1500,2000", []int{0, 1500}, []uint32{1500}},
		{"nothing granted", "2000", []int{0, 10}, nil},
		{"zero is never kept even if granted", "0,1500", []int{0, 1500}, []uint32{1500}},
		{"malformed entries ignored, order kept, no duplicates", "x, 1600 ,1500,1600,-1,", []int{1500, 1600}, []uint32{1600, 1500}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvVar, tt.env)
			withGranted(t, tt.granted, nil)
			assert.Equal(t, tt.want, FromEnv())
		})
	}
}

func TestFromEnv_GetgroupsError(t *testing.T) {
	t.Setenv(EnvVar, "1500")
	withGranted(t, nil, errors.New("boom"))
	assert.Nil(t, FromEnv())
}
