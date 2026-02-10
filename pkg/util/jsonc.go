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

package util

import (
	"encoding/json"
	"os"
)

// ReadJSONC reads a file and unmarshals it, allowing comments (JSONC).
// It strips C-style comments (// and /* ... */) and trailing commas before unmarshalling.
func ReadJSONC(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return UnmarshalJSONC(data, v)
}

// UnmarshalJSONC unmarshals JSON data that may contain comments and trailing commas.
func UnmarshalJSONC(data []byte, v any) error {
	data = StripComments(data)
	data = StripTrailingCommas(data)
	return json.Unmarshal(data, v)
}

// StripComments replaces C-style comments with whitespace to preserve locations,
// or just removes them. For JSON unmarshal, removing them is fine.
// We'll just skip them.
func StripComments(data []byte) []byte {
	var out []byte
	inString := false
	inBlockComment := false
	inLineComment := false
	escaped := false

	for i := 0; i < len(data); i++ {
		c := data[i]

		if inBlockComment {
			if c == '*' && i+1 < len(data) && data[i+1] == '/' {
				inBlockComment = false
				i++ // skip /
			}
			continue
		}

		if inLineComment {
			if c == '\n' {
				inLineComment = false
				out = append(out, c)
			}
			continue
		}

		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			out = append(out, c)
			continue
		}

		// Normal state
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}

		if c == '/' {
			if i+1 < len(data) {
				if data[i+1] == '/' {
					inLineComment = true
					i++
					continue
				}
				if data[i+1] == '*' {
					inBlockComment = true
					i++
					continue
				}
			}
		}

		out = append(out, c)
	}

	return out
}

// StripTrailingCommas removes trailing commas before ] or } in JSON.
// This handles patterns like [1, 2,] or {"a": 1,}.
func StripTrailingCommas(data []byte) []byte {
	var out []byte
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		c := data[i]

		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			out = append(out, c)
			continue
		}

		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}

		if c == ',' {
			// Look ahead to see if this comma is followed by ] or } (with optional whitespace)
			isTrailing := false
			for j := i + 1; j < len(data); j++ {
				next := data[j]
				if next == ' ' || next == '\t' || next == '\n' || next == '\r' {
					continue
				}
				if next == ']' || next == '}' {
					isTrailing = true
				}
				break
			}
			if isTrailing {
				// Skip this trailing comma
				continue
			}
		}

		out = append(out, c)
	}

	return out
}