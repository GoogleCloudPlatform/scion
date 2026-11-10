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

package apierr

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// The control and non-ASCII runes below are built from code points, so this
// source file stays pure ASCII.
var (
	runeESC      = string(rune(0x1b))
	runeCR       = string(rune(0x0d))
	runeLF       = string(rune(0x0a))
	runeRLO      = string(rune(0x202e)) // RIGHT-TO-LEFT OVERRIDE, a bidi control
	runeRepl     = string(utf8.RuneError)
	runeEllipsis = string(rune(0x2026))
	backslash    = string(rune(0x5c))
)

// A 5xx from WriteErrorFromErr answers the fixed "Internal server error" and
// logs the error once, rendered: control and bidi runes replaced, the text
// capped at apiLogDetailCap, and a large error sliced before it is scanned
// (ptone/scion#7498).
//
// Must not run in parallel: it swaps the default logger.
func TestWriteErrorFromErr_5xxLogRenderedAndCapped(t *testing.T) {
	const marker = "APIERR-MARKER-7498"
	prefix := marker + runeESC + "[2J" + runeCR + runeLF + "level=INFO msg=forged" + runeRLO
	tests := []struct {
		name    string
		errText string
	}{
		{name: "padded error", errText: prefix + strings.Repeat("A", 4000)},
		{name: "1 MiB error", errText: prefix + strings.Repeat("A", 1<<20)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			rec := httptest.NewRecorder()
			WriteErrorFromErr(rec, errors.New(tc.errText), "req-1")

			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "Internal server error") {
				t.Errorf("body %q lacks the fixed message", body)
			}
			if strings.Contains(body, marker) {
				t.Errorf("the error text reached the client: %q", body)
			}

			out := logs.String()
			if n := strings.Count(out, "API Error from Go error"); n != 1 {
				t.Fatalf("got %d 5xx log records, want 1", n)
			}
			if n := strings.Count(out, marker); n != 1 {
				t.Errorf("marker appears %d times in the log, want 1", n)
			}
			// One record is one line: no forged line break reached the log.
			if n := strings.Count(out, runeLF); n != 1 {
				t.Errorf("log has %d line feeds, want exactly the record's own 1", n)
			}
			// Rendered text carries U+FFFD in place of each control rune; the
			// text handler would otherwise have escaped them, so neither the
			// raw runes nor their escaped forms may appear.
			for _, bad := range []string{
				runeESC, runeCR, runeRLO,
				backslash + "x1b", backslash + "u001b", backslash + "r", backslash + "n", backslash + "u202e",
			} {
				if strings.Contains(out, bad) {
					t.Errorf("log carries unrendered %q", bad)
				}
			}
			if !strings.Contains(out, runeRepl) {
				t.Error("log lacks the U+FFFD replacement")
			}
			if n := strings.Count(out, "A"); n > apiLogDetailCap {
				t.Errorf("log carries %d bytes of padding, want at most %d", n, apiLogDetailCap)
			}
			if !strings.Contains(out, runeEllipsis) {
				t.Error("log lacks the cut marker")
			}
		})
	}
}

// renderLogDetail slices before it sanitises: the result for a huge input
// equals the result for its first 4*apiLogDetailCap bytes.
func TestRenderLogDetail_SlicesBeforeScanning(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	if got, want := renderLogDetail(huge), renderLogDetail(huge[:4*apiLogDetailCap]); got != want {
		t.Errorf("renderLogDetail(1 MiB) = %q, want %q", got, want)
	}
	if got := renderLogDetail(huge); len(got) > apiLogDetailCap+len(runeEllipsis) {
		t.Errorf("rendered %d bytes, want at most %d", len(got), apiLogDetailCap+len(runeEllipsis))
	}
}

// A nil error maps to the fallback 500. Its 5xx log line must render it as
// "<nil>" rather than panic on err.Error().
func TestWriteErrorFromErr_NilErrorLogsNil(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := httptest.NewRecorder()
	WriteErrorFromErr(rec, nil, "req-nil")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Internal server error") {
		t.Errorf("body %q lacks the fixed message", body)
	}
	if out := logs.String(); !strings.Contains(out, "error=<nil>") {
		t.Errorf("log %q lacks error=<nil>", out)
	}
}
