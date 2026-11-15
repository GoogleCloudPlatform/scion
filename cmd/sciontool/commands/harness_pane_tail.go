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

package commands

import (
	"context"
	"strings"
	"time"
)

// Pane-tail capture for running-error evidence (ptone/scion#7858 design
// §4.4, last bullet): the last lines the running harness shows in its tmux
// window, attached to the first-turn watchdog's harness_error reason
// (Q-B2 = a). The process-exit tail in the tmux wrapper is separate.
const (
	// harnessPaneTarget is the harness's tmux window (pkg/runtime
	// tmuxAgentWindowCmd: session scion, window agent).
	harnessPaneTarget = "scion:agent"
	// harnessPaneCaptureLines is how far back capture-pane starts.
	harnessPaneCaptureLines = 40
	// harnessPaneTailLines is how many non-empty lines are kept.
	harnessPaneTailLines = 5
	// harnessPaneCaptureTimeout bounds one capture.
	harnessPaneCaptureTimeout = 5 * time.Second
)

// paneCaptureFunc runs `tmux capture-pane -p -J -t <target> -S -<lines>`
// as the agent user and returns its output.
type paneCaptureFunc func(ctx context.Context, uid, gid int) ([]byte, error)

// harnessPaneTail returns the last harnessPaneTailLines non-empty lines of
// the harness pane, joined with " | ", or "" when the capture fails. The
// text is raw: the caller redacts and truncates it with the harness_error
// reason (handlers.FormatHarnessErrorReason).
func harnessPaneTail(ctx context.Context, capture paneCaptureFunc, uid, gid int) string {
	if capture == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, harnessPaneCaptureTimeout)
	defer cancel()
	out, err := capture(ctx, uid, gid)
	if err != nil {
		return ""
	}
	return lastNonEmptyLines(string(out), harnessPaneTailLines)
}

// lastNonEmptyLines returns the last n lines of s that are not blank,
// each trimmed, joined with " | ".
func lastNonEmptyLines(s string, n int) string {
	var kept []string
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			kept = append(kept, line)
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, " | ")
}
