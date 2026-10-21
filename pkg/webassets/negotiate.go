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

import (
	"strconv"
	"strings"
)

// Encoding is a content coding the asset server can send.
type Encoding string

// The codings the server knows, in server preference order.
const (
	Brotli   Encoding = "br"
	Gzip     Encoding = "gzip"
	Identity Encoding = "identity"
)

// variantSuffix is the file name suffix of a precompressed sibling.
var variantSuffix = map[Encoding]string{
	Brotli: ".br",
	Gzip:   ".gz",
}

// etagSuffix is appended inside the quotes of an encoded variant's ETag, so
// every representation of a file has its own validator.
var etagSuffix = map[Encoding]string{
	Brotli: "-br",
	Gzip:   "-gz",
}

// IsVariantName reports whether name (a URL path or file name) is a
// precompressed sibling (*.br or *.gz). Such names are never servable by
// their own name: they are only sent as an encoded representation of the
// file without the suffix. Trailing slashes are ignored ("x.br/" is a
// variant name too).
func IsVariantName(name string) bool {
	lower := strings.ToLower(strings.TrimRight(name, "/"))
	return strings.HasSuffix(lower, ".br") || strings.HasSuffix(lower, ".gz")
}

// Negotiate picks the coding for a response from the request's
// Accept-Encoding values and the encoded variants available. The coding
// with the highest q-value wins; ties go to br, then gzip. Identity is
// chosen over them only when the client lists it with a higher q-value.
// A coding with q=0, or one covered only by "*;q=0", is never chosen. A
// missing or empty header, or no acceptable variant, yields Identity
// (identity is sent even when the client refused it, rather than a 406).
// Unknown codings are ignored; "x-gzip" counts as gzip (RFC 9110 8.4.1.3).
func Negotiate(acceptEncoding []string, available map[Encoding]bool) Encoding {
	explicit := map[Encoding]float64{}
	star, hasStar := 0.0, false
	for _, header := range acceptEncoding {
		for _, item := range strings.Split(header, ",") {
			token, q, ok := parseCoding(item)
			if !ok {
				continue
			}
			switch token {
			case "*":
				star, hasStar = q, true
			case "br":
				explicit[Brotli] = q
			case "gzip", "x-gzip":
				explicit[Gzip] = q
			case "identity":
				explicit[Identity] = q
			}
		}
	}
	qOf := func(enc Encoding) float64 {
		if q, ok := explicit[enc]; ok {
			return q
		}
		if hasStar {
			return star
		}
		return 0
	}

	best, bestQ := Identity, 0.0
	for _, enc := range []Encoding{Brotli, Gzip} {
		if !available[enc] {
			continue
		}
		if q := qOf(enc); q > bestQ {
			best, bestQ = enc, q
		}
	}
	// Identity wins over the chosen coding only when the client gave it an
	// explicitly higher q-value; otherwise it is the last resort.
	if q, ok := explicit[Identity]; ok && best != Identity && q > bestQ {
		return Identity
	}
	return best
}

// parseCoding parses one Accept-Encoding list member ("gzip;q=0.5"). It
// returns the lower-cased coding and its q-value (1 when absent). A member
// with a malformed q-value is reported as not ok and ignored.
func parseCoding(item string) (string, float64, bool) {
	parts := strings.Split(item, ";")
	token := strings.ToLower(strings.TrimSpace(parts[0]))
	if token == "" {
		return "", 0, false
	}
	q := 1.0
	for _, param := range parts[1:] {
		name, value, found := strings.Cut(strings.TrimSpace(param), "=")
		if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || v < 0 || v > 1 {
			return "", 0, false
		}
		q = v
	}
	return token, q, true
}
