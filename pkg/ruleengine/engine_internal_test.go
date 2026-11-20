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

package ruleengine

import (
	"testing"
	"time"
)

func TestEngineDeadlineMargin(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{-5 * time.Millisecond, 0},
		{0, 0},
		{time.Nanosecond, time.Nanosecond},
		{10 * time.Nanosecond, time.Nanosecond},
		{80 * time.Millisecond, 4 * time.Millisecond},
		{200 * time.Millisecond, 10 * time.Millisecond},
		{time.Second, 10 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := engineDeadlineMargin(tc.in); got != tc.want {
			t.Errorf("engineDeadlineMargin(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
