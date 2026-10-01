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
	"testing"
	"time"
)

// TestFormatUTCTimestamp is the tz-refactor task 1 table test for the
// "literal timestamp formatting" defect class (design §2.1.7/§2.2): a
// time.Time in a non-UTC location, formatted without first converting to
// UTC, prints the wrong instant under a trailing "Z" or "Z07:00". Each row
// below is one of the sites this task fixes:
//   - "github token expiry" is handlers_github_app_webhook.go:808
//     (previously .Format("2006-01-02T15:04:05Z"), no .UTC() at all).
//   - "invite audit-log expires_at" is admin_invites.go:177 (previously
//     .Format(time.RFC3339), no .UTC()).
//
// The three agent-event sites that share the RFC3339 layout — events.go
// :491 (lastActivityEvent), :502 (startedAt) and :534 (created) — go through
// eventBuilder, not this helper; they are covered by
// TestChannelEventPublisher_AgentEventTimestampsAreInstantCorrect in
// events_utc_test.go, with the same non-UTC-location fixture and the same
// instant-equality assertion.
func TestFormatUTCTimestamp(t *testing.T) {
	// A fixed zone 9 hours ahead of UTC (Tokyo's standard offset), chosen so
	// the test is deterministic and needs no tzdata lookup. This stands in
	// for a hub whose host TZ (e.g. Asia/Tokyo, Asia/Kathmandu) has not been
	// pinned to UTC, or any time.Time sourced from a non-UTC location.
	loc := time.FixedZone("JST", 9*60*60)
	in := time.Date(2026, 3, 7, 23, 15, 30, 0, loc) // 14:15:30Z

	tests := []struct {
		name   string
		layout string
		want   string
	}{
		{
			name:   "github token expiry (handlers_github_app_webhook.go:808)",
			layout: "2006-01-02T15:04:05Z",
			want:   "2026-03-07T14:15:30Z",
		},
		{
			name:   "invite audit-log expires_at (admin_invites.go:177)",
			layout: time.RFC3339,
			want:   "2026-03-07T14:15:30Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatUTCTimestamp(in, tt.layout)
			if got != tt.want {
				t.Fatalf("formatUTCTimestamp(%v, %q) = %q, want %q (wrong instant, not just a format nit)", in, tt.layout, got, tt.want)
			}

			// Instant equality, not just string shape: round-trip and
			// compare to the original instant.
			parsed, err := time.Parse(tt.layout, got)
			if err != nil {
				t.Fatalf("parsing %q with layout %q: %v", got, tt.layout, err)
			}
			if !parsed.Equal(in) {
				t.Fatalf("round-tripped to a different instant: got %v, want %v", parsed, in)
			}
		})
	}
}
