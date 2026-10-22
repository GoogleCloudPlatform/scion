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

package config

import (
	"os"
	"strings"
	"testing"
)

// unparsedURLCase is one line of testdata/url_redact_unparsed.tsv, the
// table of URLs that url.Parse rejects. pkg/apiclient's redact_test.go
// reads the same file for its copy of the rules.
type unparsedURLCase struct{ name, in, want, secret string }

func loadUnparsedURLCases(t *testing.T) []unparsedURLCase {
	t.Helper()
	data, err := os.ReadFile("testdata/url_redact_unparsed.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var cases []unparsedURLCase
	for i, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 {
			t.Fatalf("testdata/url_redact_unparsed.tsv:%d: %d fields, want 4", i+1, len(f))
		}
		cases = append(cases, unparsedURLCase{f[0], f[1], f[2], f[3]})
	}
	if len(cases) == 0 {
		t.Fatal("testdata/url_redact_unparsed.tsv: no cases")
	}
	return cases
}

// checkUnparsedURLCases runs the table against redact: each output is
// exactly the wanted one and shows no 4-byte part of the case's secret.
func checkUnparsedURLCases(t *testing.T, fn string, redact func(string) string) {
	t.Helper()
	for _, tc := range loadUnparsedURLCases(t) {
		got := redact(tc.in)
		if got != tc.want {
			t.Errorf("%s: %s(%q) = %q, want %q", tc.name, fn, tc.in, got, tc.want)
		}
		if tc.secret == "-" {
			continue
		}
		for i := 0; i+4 <= len(tc.secret); i++ {
			if strings.Contains(got, tc.secret[i:i+4]) {
				t.Errorf("%s: %s(%q) = %q, shows %q", tc.name, fn, tc.in, got, tc.secret[i:i+4])
			}
		}
	}
}

// A URL that does not parse, whose user info the fallback rule cannot
// delimit, shows only its scheme and a mask; URLs the rules already
// handled, and ordinary URLs, keep their output.
func TestRedactURL_Unparsed(t *testing.T) {
	checkUnparsedURLCases(t, "RedactURL", RedactURL)
}
