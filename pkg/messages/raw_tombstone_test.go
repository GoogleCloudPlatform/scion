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

package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
)

func TestHasRetiredRawField(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		nested []string
		want   bool
	}{
		// Top-level spelling, every value shape.
		{"top true", `{"msg":"hi","raw":true}`, nil, true},
		{"top false", `{"raw":false,"msg":"hi"}`, nil, true},
		{"top null", `{"raw":null}`, nil, true},
		{"top string", `{"raw":"yes"}`, nil, true},
		{"top number", `{"raw":1}`, nil, true},
		{"top array", `{"raw":[true]}`, nil, true},
		{"top object", `{"raw":{}}`, nil, true},
		{"top malformed value", `{"raw":tru}`, nil, true},
		{"top truncated value", `{"raw":`, nil, true},
		{"top case folded", `{"RAW":true}`, nil, true},
		{"top mixed case", `{"Raw":false}`, nil, true},
		{"top duplicate later", `{"msg":"a","msg":"b","raw":true}`, nil, true},

		// Nested spelling.
		{"nested true", `{"structured_message":{"msg":"hi","raw":true}}`, []string{"structured_message"}, true},
		{"nested false", `{"structured_message":{"raw":false}}`, []string{"structured_message"}, true},
		{"nested null", `{"message":{"raw":null}}`, []string{"message"}, true},
		{"nested wrong type", `{"message":{"raw":"true"}}`, []string{"message"}, true},
		{"nested malformed", `{"message":{"raw":nul}}`, []string{"message"}, true},
		{"nested case folded name", `{"Message":{"rAw":true}}`, []string{"message"}, true},
		{"nested second duplicate", `{"message":{"msg":"a"},"message":{"raw":true}}`, []string{"message"}, true},
		{"nested after other members", `{"a":[1,{"raw":1}],"message":{"x":{"y":1},"raw":true}}`, []string{"message"}, true},
		{"top raw beside nested", `{"message":{"msg":"hi"},"raw":true}`, []string{"message"}, true},

		// No match.
		{"no raw", `{"msg":"hi","plain":true,"urgent":true}`, nil, false},
		{"nested raw not requested", `{"structured_message":{"raw":true}}`, nil, false},
		{"raw inside unrelated object", `{"meta":{"raw":true}}`, []string{"message"}, false},
		{"raw deeper than one level", `{"message":{"inner":{"raw":true}}}`, []string{"message"}, false},
		{"raw as a value", `{"msg":"raw","type":"raw"}`, nil, false},
		{"raw-like names", `{"rawish":true,"raw_input":1,"draw":2}`, nil, false},
		{"nested scalar", `{"message":"raw"}`, []string{"message"}, false},
		{"nested array", `{"message":[{"raw":true}]}`, []string{"message"}, false},
		{"nested null value", `{"message":null}`, []string{"message"}, false},
		{"empty body", ``, nil, false},
		{"empty object", `{}`, nil, false},
		{"top-level array", `[{"raw":true}]`, nil, false},
		{"top-level string", `"raw"`, nil, false},
		{"malformed before raw", `{"msg":,"raw":true}`, nil, false},
		{"trailing value ignored", `{"msg":"hi"} {"raw":true}`, nil, false},

		// Values skipped before a raw member: a well-formed value of any
		// shape still lets the later raw member be found.
		{"skip out-of-range number", `{"a":1e400,"raw":true}`, nil, true},
		{"skip negative zero", `{"a":-0,"raw":true}`, nil, true},
		{"skip lone surrogate escape", `{"a":"\ud800","raw":true}`, nil, true},
		{"skip invalid utf8 byte", "{\"a\":\"\xff\",\"raw\":true}", nil, true},
		{"skip deep nesting", `{"a":[[[[{"b":[{}]}]]]],"raw":true}`, nil, true},
		{"skip nested raw elsewhere", `{"meta":{"raw":1},"raw":true}`, []string{"message"}, true},
		{"skip array beside nested", `{"message":[1,{"x":2}],"raw":true}`, []string{"message"}, true},

		// Malformed or truncated skipped values: rejected before the raw
		// member is reached, so the caller's decode reports the bad body.
		{"skip bad escape", `{"a":"\x","raw":true}`, nil, false},
		{"skip control char in string", "{\"a\":\"\x01\",\"raw\":true}", nil, false},
		{"skip leading zero", `{"a":01,"raw":true}`, nil, false},
		{"skip bad literal", `{"a":tru,"raw":true}`, nil, false},
		{"skip trailing comma in array", `{"a":[1,2,],"raw":true}`, nil, false},
		{"skip missing comma in array", `{"a":[1 2],"raw":true}`, nil, false},
		{"skip missing colon", `{"a":{"b" 1},"raw":true}`, nil, false},
		{"skip trailing comma in object", `{"a":{"b":1,},"raw":true}`, nil, false},
		{"skip mismatched delimiters", `{"a":{"b":[}]},"raw":true}`, nil, false},
		{"skip nested bad literal", `{"a":{"b":{"c":nul}},"raw":true}`, nil, false},
		{"skip truncated array", `{"a":[1,2`, nil, false},
		{"skip truncated string", `{"a":"abc`, nil, false},
		{"skip truncated after nested", `{"message":{"x":1},"a":{"b":`, []string{"message"}, false},
		{"nested skip malformed", `{"message":{"x":[1,],"raw":true}}`, []string{"message"}, false},
		{"nested array element malformed", `{"message":[tru],"raw":true}`, []string{"message"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasRetiredRawField([]byte(tt.body), tt.nested...); got != tt.want {
				t.Fatalf("HasRetiredRawField(%q, %v) = %v, want %v", tt.body, tt.nested, got, tt.want)
			}
		})
	}
}

// TestSkipJSONValueMatchesRawMessage pins that skipJSONValue accepts and
// rejects exactly the values a json.RawMessage decode does, and leaves the
// decoder at the same position with the same next token, so the probe's
// behaviour does not depend on which destination the skip decodes into.
func TestSkipJSONValueMatchesRawMessage(t *testing.T) {
	values := []string{
		`1`, `-0`, `1e400`, `-1.5E-7`, `01`, `1.`, `-`, `.5`,
		`"a"`, `"\ud800"`, `"\x"`, "\"\xff\"", "\"\x01\"", `"abc`,
		`true`, `false`, `null`, `tru`, `nul`, `nulll`,
		`[]`, `[1,2]`, `[1,2,]`, `[1 2]`, `[1,2`, `[[[[]]]]`, `[}`,
		`{}`, `{"b":1}`, `{"b" 1}`, `{"b":1,}`, `{"b":[}]}`, `{"b":{"c":nul}}`, `{"b":`,
		``, ` `, `,`, `]`, `}`, `:`,
	}
	for _, v := range values {
		// Each value is followed by more members, as in a real probe, so
		// the comparison also covers where the decoder resumes.
		body := `{"k":` + v + `,"next":true}`
		t.Run(v, func(t *testing.T) {
			got := newSkipDecoder(t, body)
			want := newSkipDecoder(t, body)

			gotOK := skipJSONValue(got)
			var rm json.RawMessage
			wantOK := want.Decode(&rm) == nil
			if gotOK != wantOK {
				t.Fatalf("skipJSONValue(%s) = %v, RawMessage decode ok = %v", v, gotOK, wantOK)
			}
			if !gotOK {
				return
			}
			if g, w := got.InputOffset(), want.InputOffset(); g != w {
				t.Fatalf("offset after skip = %d, want %d", g, w)
			}
			gt, gerr := got.Token()
			wt, werr := want.Token()
			if gt != wt || (gerr == nil) != (werr == nil) {
				t.Fatalf("next token = %v (%v), want %v (%v)", gt, gerr, wt, werr)
			}
		})
	}
}

// newSkipDecoder returns a decoder positioned just before the value of the
// first member of body.
func newSkipDecoder(t *testing.T, body string) *json.Decoder {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(body))
	for i := 0; i < 2; i++ { // '{' then the member name
		if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("setup token %d for %q: %v", i, body, err)
		}
	}
	return dec
}

// TestSkipJSONValueKeepsNoCopy pins the allocation saving: skipping a large
// value must not allocate a copy of it on top of the decoder's own buffer.
func TestSkipJSONValueKeepsNoCopy(t *testing.T) {
	const size = 1 << 20
	body := []byte(`{"msg":"` + strings.Repeat("x", size) + `","plain":true}`)
	perRun := func(skip func(*json.Decoder) bool) float64 {
		const runs = 5
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		for i := 0; i < runs; i++ {
			dec := json.NewDecoder(bytes.NewReader(body))
			_, _ = dec.Token()
			_, _ = dec.Token()
			if !skip(dec) {
				t.Fatal("skip failed")
			}
		}
		runtime.ReadMemStats(&after)
		return float64(after.TotalAlloc-before.TotalAlloc) / runs
	}
	discard := perRun(skipJSONValue)
	raw := perRun(func(dec *json.Decoder) bool {
		var rm json.RawMessage
		return dec.Decode(&rm) == nil
	})
	if raw-discard < size*0.9 {
		t.Fatalf("skip allocated %.0f B/op vs %.0f B/op for a RawMessage decode; want at least %d B less", discard, raw, size*9/10)
	}
}

func BenchmarkHasRetiredRawField(b *testing.B) {
	big := 2<<20 - 64
	bodies := []struct {
		name string
		body []byte
	}{
		{"small", []byte(`{"msg":"hello","plain":true,"urgent":false,"meta":{"a":[1,2,3]}}`)},
		{"large_string", []byte(`{"msg":"` + strings.Repeat("x", big) + `","plain":true}`)},
		{"large_array", []byte(`{"meta":[` + strings.Repeat("1,", big/2) + `1],"plain":true}`)},
	}
	for _, bb := range bodies {
		b.Run(bb.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if HasRetiredRawField(bb.body, "structured_message") {
					b.Fatal("unexpected raw match")
				}
			}
		})
	}
}
