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

import "testing"

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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasRetiredRawField([]byte(tt.body), tt.nested...); got != tt.want {
				t.Fatalf("HasRetiredRawField(%q, %v) = %v, want %v", tt.body, tt.nested, got, tt.want)
			}
		})
	}
}
