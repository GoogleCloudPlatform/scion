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

package telemetryemit

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
)

// TestKindOf: each sink maps to its lifecycle_sink_kind (design section 22
// item 4, condition b); a sink outside the three wire kinds is "other".
func TestKindOf(t *testing.T) {
	for _, tt := range []struct {
		name string
		sink Sink
		want string
	}{
		{"nil", nil, SinkKindNone},
		{"cloud logging", &CloudLoggingSink{}, SinkKindCloudLogging},
		{"stdout", NewStdoutSink(io.Discard), SinkKindStdout},
		{"memory", NewMemorySink(), "other"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindOf(tt.sink); got != tt.want {
				t.Errorf("KindOf(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestMemoryCountersSinkKind: SinkKind writes all three wire kinds, 1 for
// the given kind and 0 for the others; a kind outside them writes all
// three as 0. Each key is checked as written, not only read as 0.
func TestMemoryCountersSinkKind(t *testing.T) {
	wire := []string{SinkKindCloudLogging, SinkKindStdout, SinkKindNone}
	for _, kind := range append(append([]string{}, wire...), "other") {
		t.Run(kind, func(t *testing.T) {
			c := NewMemoryCounters()
			c.SinkKind(kind)
			listed := c.String()
			fields := strings.Fields(listed)
			for _, k := range wire {
				want := int64(0)
				if k == kind {
					want = 1
				}
				key := memKey(MetricSinkKind, k)
				if !slices.Contains(fields, fmt.Sprintf("%s=%d", key, want)) {
					t.Fatalf("SinkKind(%q) did not write %s=%d: %s", kind, key, want, listed)
				}
				if got := c.Get(MetricSinkKind, k); got != want {
					t.Errorf("SinkKind(%q): %s = %d, want %d", kind, key, got, want)
				}
			}
			// A later kind replaces the earlier one.
			c.SinkKind(SinkKindNone)
			if c.Get(MetricSinkKind, SinkKindNone) != 1 || (kind != SinkKindNone && c.Get(MetricSinkKind, kind) != 0) {
				t.Errorf("SinkKind(none) after %q: %s", kind, c.String())
			}
		})
	}
}
