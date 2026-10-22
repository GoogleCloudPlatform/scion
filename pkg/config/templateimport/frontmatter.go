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

package templateimport

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"
)

var frontMatterDelimiter = []byte("---")

// ParseFrontMatter splits a markdown file into YAML front matter and the remaining body.
// Front matter delimiters must occupy their own lines.
// Returns the parsed front matter as a map, the markdown body, and any error.
// If the file has no front matter, returns nil map and the full content as body.
func ParseFrontMatter(data []byte) (map[string]any, string, error) {
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	opening, rest, ok := bytes.Cut(trimmed, []byte("\n"))
	if !ok || !bytes.Equal(bytes.TrimRight(opening, " \t\r"), frontMatterDelimiter) {
		return nil, string(data), nil
	}

	// Match a whole closing delimiter line. Leading whitespace is significant:
	// an indented --- can be content in a YAML block scalar.
	closingIdx := 0
	for line := range bytes.Lines(rest) {
		if bytes.Equal(bytes.TrimRight(line, " \t\r\n"), frontMatterDelimiter) {
			break
		}
		closingIdx += len(line)
	}
	if closingIdx == len(rest) {
		return nil, "", fmt.Errorf("unclosed front matter: missing closing ---")
	}

	yamlData := rest[:closingIdx]
	body := rest[closingIdx+len(frontMatterDelimiter):]

	// Skip the rest of the closing delimiter line
	if idx := bytes.IndexByte(body, '\n'); idx >= 0 {
		body = body[idx+1:]
	} else {
		body = nil
	}

	// Parse YAML
	var fm map[string]any
	if err := yaml.Unmarshal(yamlData, &fm); err != nil {
		return nil, "", fmt.Errorf("failed to parse front matter YAML: %w", err)
	}

	// An empty YAML document produces a nil map
	if fm == nil {
		fm = map[string]any{}
	}

	return fm, string(body), nil
}
