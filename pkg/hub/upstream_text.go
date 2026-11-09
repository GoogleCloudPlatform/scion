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

import "unicode/utf8"

const (
	// upstreamTextMaxBytes caps upstream response text (an OAuth or token
	// endpoint body, an object-store error) rendered into an error or log
	// line.
	upstreamTextMaxBytes = 512

	// upstreamErrorBodyReadLimit bounds how much of an upstream error
	// response body the hub reads. Only upstreamTextMaxBytes of it are ever
	// rendered.
	upstreamErrorBodyReadLimit = 4 << 10
)

// upstreamText renders text received from an upstream service for an error
// or server log line: sanitizeForLog replaces control, format and line
// separator runes and invalid UTF-8 with U+FFFD and cuts the result to
// upstreamTextMaxBytes on a rune boundary, marking a cut with "…".
//
// s is sliced before sanitizing so a large body is never fully scanned. The
// slice keeps one whole rune past the cap: every input byte renders to at
// least one output byte, so the rendered prefix is the same as for the full
// input and a cut is still marked.
func upstreamText(s string) string {
	if limit := upstreamTextMaxBytes + utf8.UTFMax; len(s) > limit {
		s = s[:limit]
	}
	return sanitizeForLog(s, upstreamTextMaxBytes)
}
