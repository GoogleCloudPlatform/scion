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
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureSlog replaces the default slog logger with one that writes to a
// buffer and returns the buffer. Restores the original logger on cleanup.
//
// Call this BEFORE constructing a Server (or anything else that snapshots
// slog.Default() into a fixed subsystem logger, e.g. logging.Subsystem).
// Capturing after construction can leave the test's absence assertions
// vacuous: see requireLogCaptureLive below.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	original := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// serverConstructionLogLine mirrors the slog.Info call in server.go's
// New(), the construction-time line most positive controls in this
// package assert against.
const serverConstructionLogLine = "Control channel manager initialized"

// requireLogCaptureLive is a positive control for slog-capture regression
// tests. It fails loudly if buf does not contain wantSubstring, a line
// known to be logged after the capture was installed (for example the
// Server's construction line). Without this, a misrouted or broken capture
// -- for example one installed after the Server has already snapshotted
// slog.Default() into a subsystem logger -- would make an absence
// assertion on buf pass vacuously instead of catching the regression it
// exists to guard against.
func requireLogCaptureLive(t *testing.T, buf *bytes.Buffer, wantSubstring string) {
	t.Helper()
	if !strings.Contains(buf.String(), wantSubstring) {
		t.Fatalf("log capture positive control failed: buffer does not contain %q; "+
			"the capture may be misrouted, so the absence assertions below it "+
			"would be vacuous. captured=%q", wantSubstring, buf.String())
	}
}
