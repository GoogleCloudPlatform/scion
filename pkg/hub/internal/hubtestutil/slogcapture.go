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

package hubtestutil

import (
	"bytes"
	"log/slog"
	"testing"
)

// CaptureSlog replaces the default slog logger with one that writes to a
// buffer and returns the buffer. Restores the original logger on cleanup.
//
// Call this BEFORE constructing a Server (or anything else that snapshots
// slog.Default() into a fixed subsystem logger, e.g. logging.Subsystem).
// Capturing after construction can leave the test's absence assertions
// vacuous: see pkg/hub's requireLogCaptureLive.
func CaptureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	original := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(original) })
	return &buf
}

// CaptureSlogDefault swaps slog's default logger for a text handler writing
// to the returned buffer, and restores the previous default on test cleanup.
func CaptureSlogDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// InstallSentinelLogCapture points the slog default logger at an in-memory
// buffer, at Debug level so the debug-only logging of pkg/hub's brokerHTTPTransport is
// captured too, and restores the previous default logger on test cleanup.
func InstallSentinelLogCapture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// RecordAttrs flattens a slog.Record's top-level attributes (including nested
// groups, dotted) into a map for easy lookup in assertions.
func RecordAttrs(r slog.Record) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, a slog.Attr)
	walk = func(prefix string, a slog.Attr) {
		key := a.Key
		if prefix != "" {
			key = prefix + "." + a.Key
		}
		// Resolve LogValuer values (e.g. principal.CredentialDecoration.LogValue())
		// before inspecting Kind: a captured record from a bare handler like
		// pkg/hub's capturingHandler never resolves them itself (only the standard
		// Text/JSON handlers do that internally when formatting).
		a.Value = a.Value.Resolve()
		if a.Value.Kind() == slog.KindGroup {
			for _, ga := range a.Value.Group() {
				walk(key, ga)
			}
			return
		}
		out[key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool {
		walk("", a)
		return true
	})
	return out
}

// FindRecord returns the last record in records whose message is msg.
func FindRecord(records []slog.Record, msg string) (slog.Record, bool) {
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].Message == msg {
			return records[i], true
		}
	}
	return slog.Record{}, false
}

// RecordAttr reads a single attribute off a captured record.
func RecordAttr(r slog.Record, key string) (slog.Value, bool) {
	var found slog.Value
	var ok bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found, ok = a.Value, true
			return false
		}
		return true
	})
	return found, ok
}
