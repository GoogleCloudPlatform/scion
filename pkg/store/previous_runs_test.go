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

package store

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAppendPreviousRunID(t *testing.T) {
	for _, tc := range []struct {
		name              string
		prev              []string
		replaced, current string
		want, dropped     []string
	}{
		{"first run", nil, "", "r1", nil, nil},
		{"append", nil, "r1", "r2", []string{"r1"}, nil},
		{"keeps order", []string{"r1"}, "r2", "r3", []string{"r1", "r2"}, nil},
		{"no duplicate", []string{"r1"}, "r1", "r2", []string{"r1"}, nil},
		{"never the current run", []string{"r1", "r2"}, "r3", "r1", []string{"r2", "r3"}, nil},
		{"replaced is current", []string{"r1"}, "r2", "r2", []string{"r1"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]string(nil), tc.prev...)
			got, dropped := AppendPreviousRunID(tc.prev, tc.replaced, tc.current)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.dropped, dropped)
			assert.Equal(t, before, tc.prev, "prev is not modified")
		})
	}
}

func TestAppendPreviousRunID_Cap(t *testing.T) {
	var prev []string
	for i := 0; i < MaxPreviousRunIDs; i++ {
		prev = append(prev, fmt.Sprintf("r%d", i))
	}
	got, dropped := AppendPreviousRunID(prev, "new", "cur")
	assert.Len(t, got, MaxPreviousRunIDs)
	assert.Equal(t, []string{"r0"}, dropped, "the oldest run is dropped")
	assert.Equal(t, "r1", got[0])
	assert.Equal(t, "new", got[MaxPreviousRunIDs-1])
}
