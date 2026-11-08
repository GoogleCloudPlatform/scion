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
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

// upstreamMarker is the text the upstream-text tests plant in an upstream
// response: the tests assert it reaches the server log (neutralised) and
// never the client.
const upstreamMarker = "UPSTREAM-MARKER-7504"

// upstreamMarkerBody is an upstream response body carrying upstreamMarker,
// terminal control sequences, a forged log line and far more than
// upstreamTextMaxBytes of padding.
var upstreamMarkerBody = upstreamMarker + "\x1b[2J\r\nlevel=INFO msg=forged\u202e" + strings.Repeat("A", 8000)

// assertUpstreamTextRendered asserts that s carries upstreamMarker rendered
// by upstreamText: no raw control, line-break or bidi runes from
// upstreamMarkerBody, and the padding cut to the cap.
func assertUpstreamTextRendered(t *testing.T, s string) {
	t.Helper()
	if !strings.Contains(s, upstreamMarker) {
		t.Errorf("text does not carry the upstream marker: %q", s)
	}
	for _, raw := range []string{"\x1b", "\r", "\n", "\u202e"} {
		if strings.Contains(s, raw) {
			t.Errorf("text carries raw %q: %q", raw, s)
		}
	}
	if n := strings.Count(s, "A"); n > upstreamTextMaxBytes {
		t.Errorf("text carries %d bytes of padding, want at most %d", n, upstreamTextMaxBytes)
	}
}

func TestUpstreamText(t *testing.T) {
	const ellipsis = "…"
	long := strings.Repeat("x", 10*upstreamTextMaxBytes)
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "plain text unchanged", in: `{"error":"invalid_grant"}`, want: `{"error":"invalid_grant"}`},
		{name: "at cap unchanged", in: long[:upstreamTextMaxBytes], want: long[:upstreamTextMaxBytes]},
		{name: "over cap cut and marked", in: long[:upstreamTextMaxBytes+1], want: long[:upstreamTextMaxBytes] + ellipsis},
		{name: "large body cut and marked", in: long, want: long[:upstreamTextMaxBytes] + ellipsis},
		{name: "control chars replaced", in: "a\x1b[31mb\x00c\x07d", want: "a�[31mb�c�d"},
		{name: "line breaks replaced", in: "a\r\nb\u2028c", want: "a��b�c"},
		{name: "bidi override replaced", in: "a\u202eb", want: "a�b"},
		{name: "invalid UTF-8 replaced", in: "a\xffb\xc3", want: "a�b�"},
		{name: "multibyte kept", in: "café ✓", want: "café ✓"},
		{
			// A rune straddling the cap is dropped whole, never split.
			name: "rune at cap not split",
			in:   long[:upstreamTextMaxBytes-1] + "é" + long,
			want: long[:upstreamTextMaxBytes-1] + ellipsis,
		},
		{
			// The cap applies to the rendered output, not the input: each
			// replaced byte grows to the three bytes of U+FFFD, and the cut
			// lands on a rune boundary at or before the cap.
			name: "replaced runes capped on output",
			in:   strings.Repeat("\x00", 600),
			want: strings.Repeat("�", upstreamTextMaxBytes/utf8.RuneLen(utf8.RuneError)) + ellipsis,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := upstreamText(tc.in)
			if got != tc.want {
				t.Errorf("upstreamText(%q) = %q, want %q", truncateForMessage(tc.in), truncateForMessage(got), truncateForMessage(tc.want))
			}
			if !utf8.ValidString(got) {
				t.Errorf("upstreamText output is not valid UTF-8: %q", got)
			}
			if len(got) > upstreamTextMaxBytes+len(ellipsis) {
				t.Errorf("upstreamText output is %d bytes, want at most %d", len(got), upstreamTextMaxBytes+len(ellipsis))
			}
		})
	}
}

// TestUpstreamText_MarkerBody checks the shared marker body the handler
// tests use renders as assertUpstreamTextRendered expects.
func TestUpstreamText_MarkerBody(t *testing.T) {
	assertUpstreamTextRendered(t, upstreamText(upstreamMarkerBody))
}

// captureUpstreamLog routes the default slog logger to a JSON buffer for the
// rest of the test. Tests using it must not run in parallel.
func captureUpstreamLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// upstreamLogErrors returns the "error" attribute of every captured log
// record whose message is msg.
func upstreamLogErrors(t *testing.T, buf *bytes.Buffer, msg string) []string {
	t.Helper()
	var out []string
	for _, line := range bytes.Split(buf.Bytes(), []byte("\n")) {
		var rec struct {
			Msg   string `json:"msg"`
			Error string `json:"error"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.Msg == msg {
			out = append(out, rec.Error)
		}
	}
	return out
}

// assertLoggedOnceRendered asserts exactly one captured record with message
// msg, whose error attribute carries upstreamMarker rendered by
// upstreamText.
func assertLoggedOnceRendered(t *testing.T, buf *bytes.Buffer, msg string) {
	t.Helper()
	errs := upstreamLogErrors(t, buf, msg)
	if len(errs) != 1 {
		t.Fatalf("got %d %q log records, want 1; log:\n%s", len(errs), msg, buf.String())
	}
	assertUpstreamTextRendered(t, errs[0])
}

// truncateForMessage shortens s for a test failure message.
func truncateForMessage(s string) string {
	if len(s) > 80 {
		return s[:40] + "..." + s[len(s)-20:]
	}
	return s
}
