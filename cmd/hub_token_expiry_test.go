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

package cmd

import (
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/stretchr/testify/assert"
)

func TestPrintTokenExpiry(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	t.Cleanup(clitime.SetNow(func() time.Time { return now }))
	clitime.SetZone(time.UTC)
	t.Cleanup(func() { clitime.SetZone(nil) })

	tests := []struct {
		name   string
		expiry time.Time
		want   string
	}{
		{"future hours", now.Add(23*time.Hour + 59*time.Minute), "Expires:    2026-10-04 11:59:00 UTC (in 23h)\n"},
		{"future minutes", now.Add(59*time.Minute + 30*time.Second), "Expires:    2026-10-03 12:59:30 UTC (in 59m)\n"},
		{"future under a minute", now.Add(20 * time.Second), "Expires:    2026-10-03 12:00:20 UTC (in <1m)\n"},
		{"expired hours ago", now.Add(-2*time.Hour - 10*time.Minute), "Expires:    2026-10-03 09:50:00 UTC (EXPIRED 2h ago)\n"},
		{"expired just now", now.Add(-10 * time.Second), "Expires:    2026-10-03 11:59:50 UTC (EXPIRED just now)\n"},
		{"expires exactly now", now, "Expires:    2026-10-03 12:00:00 UTC (EXPIRED just now)\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() { printTokenExpiry(tc.expiry) })
			assert.Equal(t, tc.want, out)
		})
	}
}
