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

package runtime

import (
	"context"
	"log"
	"log/slog"
	"sync"
	"testing"
)

// setRuntimeLogHandler installs h as the slog default for the rest of the
// test and restores the previous default (and the stdlib log output that
// slog.SetDefault redirects) on cleanup. runtimeLog() resolves slog.Default()
// at each call, so this is how tests capture runtime log lines. Callers must
// not run in parallel with other tests that change the slog default.
func setRuntimeLogHandler(t *testing.T, h slog.Handler) {
	t.Helper()
	prev := slog.Default()
	origWriter, origFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
	})
}

// recordedLog is one record seen by recordingHandler, with the handler's
// accumulated With attributes followed by the record's own attributes.
type recordedLog struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// recordingHandler records every record that passes its level gate.
type recordingHandler struct {
	level slog.Level
	attrs []slog.Attr
	mu    *sync.Mutex
	recs  *[]recordedLog
}

func newRecordingHandler(level slog.Level) *recordingHandler {
	return &recordingHandler{level: level, mu: &sync.Mutex{}, recs: &[]recordedLog{}}
}

func (h *recordingHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.recs = append(*h.recs, recordedLog{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h *recordingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), as...)
	return &c
}

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) records() []recordedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedLog(nil), *h.recs...)
}

// findRecord returns the first record with the given message.
func (h *recordingHandler) findRecord(msg string) (recordedLog, bool) {
	for _, r := range h.records() {
		if r.msg == msg {
			return r, true
		}
	}
	return recordedLog{}, false
}

// TestRuntimeLog_ReachesHandlerInstalledAfterInit is the regression test for
// #1339: a handler installed with slog.SetDefault after package init (as
// logging setup does for --debug and SCION_LOG_LEVEL) must receive runtime
// log lines at debug level, with the subsystem attribute and the call's own
// attributes as structured attributes rather than one pre-formatted message.
//
// Mutation proof: restoring the package-level capture
// (a var initialised from slog.Default().With(...) at init) makes this test
// fail, because the init-time handler drops the debug records and flattens
// the info record into a single message with no attributes.
func TestRuntimeLog_ReachesHandlerInstalledAfterInit(t *testing.T) {
	h := newRecordingHandler(slog.LevelDebug)
	setRuntimeLogHandler(t, h)

	runtimeLog().Debug("runtime-log-debug-probe", "probe_key", "probe_val")
	runtimeLog().Info("runtime-log-info-probe", "extra_key", "extra_val")

	cases := []struct {
		msg   string
		level slog.Level
		key   string
		val   string
	}{
		{"runtime-log-debug-probe", slog.LevelDebug, "probe_key", "probe_val"},
		{"runtime-log-info-probe", slog.LevelInfo, "extra_key", "extra_val"},
	}
	for _, c := range cases {
		rec, ok := h.findRecord(c.msg)
		if !ok {
			t.Errorf("record %q did not reach the handler installed after init; got %+v", c.msg, h.records())
			continue
		}
		if rec.level != c.level {
			t.Errorf("record %q: level = %v, want %v", c.msg, rec.level, c.level)
		}
		if got := rec.attrs["subsystem"]; got != "runtime" {
			t.Errorf("record %q: subsystem attr = %q, want %q (attrs %v)", c.msg, got, "runtime", rec.attrs)
		}
		if got := rec.attrs[c.key]; got != c.val {
			t.Errorf("record %q: %s attr = %q, want %q (attrs %v)", c.msg, c.key, got, c.val, rec.attrs)
		}
	}
}

// TestRunSimpleCommand_DebugLogReachesCurrentHandler checks the same through
// a real runtime call site: runSimpleCommand's debug line reaches the
// current default handler with its structured attributes.
func TestRunSimpleCommand_DebugLogReachesCurrentHandler(t *testing.T) {
	h := newRecordingHandler(slog.LevelDebug)
	setRuntimeLogHandler(t, h)

	if _, err := runSimpleCommand(context.Background(), "true", "a", "b"); err != nil {
		t.Fatalf("runSimpleCommand(true): %v", err)
	}

	rec, ok := h.findRecord("Executing command")
	if !ok {
		t.Fatalf("runSimpleCommand debug line did not reach the current handler; got %+v", h.records())
	}
	if rec.level != slog.LevelDebug {
		t.Errorf("level = %v, want %v", rec.level, slog.LevelDebug)
	}
	want := map[string]string{"subsystem": "runtime", "cmd": "true", "argc": "2"}
	for k, v := range want {
		if got := rec.attrs[k]; got != v {
			t.Errorf("attr %s = %q, want %q (attrs %v)", k, got, v, rec.attrs)
		}
	}
}
