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

package webassets

import "testing"

func TestNegotiate(t *testing.T) {
	both := map[Encoding]bool{Brotli: true, Gzip: true}
	gzOnly := map[Encoding]bool{Gzip: true}
	none := map[Encoding]bool{}
	cases := []struct {
		name      string
		header    []string
		available map[Encoding]bool
		want      Encoding
	}{
		{"browser list prefers br", []string{"gzip, deflate, br, zstd"}, both, Brotli},
		{"br over gzip at equal q", []string{"gzip, br"}, both, Brotli},
		{"gzip when no br variant", []string{"gzip, deflate, br"}, gzOnly, Gzip},
		{"identity when no variants", []string{"gzip, br"}, none, Identity},
		{"br;q=0 refuses br", []string{"br;q=0, gzip"}, both, Gzip},
		{"BR;Q=0 case-insensitive", []string{"BR;Q=0, GZIP"}, both, Gzip},
		{"br;q=0.000 refuses br", []string{"br;q=0.000, gzip;q=0.5"}, both, Gzip},
		{"higher q wins", []string{"br;q=0.4, gzip;q=0.9"}, both, Gzip},
		{"*;q=0 alone", []string{"*;q=0"}, both, Identity},
		{"*;q=0 with explicit gzip", []string{"gzip, *;q=0"}, both, Gzip},
		{"* allows br", []string{"*"}, both, Brotli},
		{"* allows gzip without br variant", []string{"*"}, gzOnly, Gzip},
		{"missing header", nil, both, Identity},
		{"empty header", []string{""}, both, Identity},
		{"identity only", []string{"identity"}, both, Identity},
		{"identity explicitly preferred", []string{"identity;q=1, br;q=0.5"}, both, Identity},
		{"unknown tokens ignored", []string{"zstd, deflate, compress"}, both, Identity},
		{"unknown tokens beside gzip", []string{"zstd, foo;q=1, gzip;q=0.1"}, both, Gzip},
		{"x-gzip counts as gzip", []string{"x-gzip"}, both, Gzip},
		{"malformed q ignored", []string{"br;q=abc, gzip"}, both, Gzip},
		{"q above 1 ignored", []string{"br;q=2"}, both, Identity},
		{"multiple header lines", []string{"gzip", "br"}, both, Brotli},
		{"whitespace and empty members", []string{" , br ; q=0.8 ,,gzip;q=0.7"}, both, Brotli},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Negotiate(tc.header, tc.available); got != tc.want {
				t.Errorf("Negotiate(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestIsVariantName(t *testing.T) {
	for name, want := range map[string]bool{
		"assets/main.js.br": true,
		"assets/main.js.gz": true,
		"/favicon.svg.GZ":   true,
		"assets/main.js":    false,
		"assets/brand.js":   false,
		"assets/x.gzip":     false,
	} {
		if got := IsVariantName(name); got != want {
			t.Errorf("IsVariantName(%q) = %v, want %v", name, got, want)
		}
	}
}
