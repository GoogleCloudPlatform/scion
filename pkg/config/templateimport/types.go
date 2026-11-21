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

// ImportedAgent represents a parsed agent definition from a harness-specific format.
type ImportedAgent struct {
	Name           string         // Agent/template name (from front matter)
	Description    string         // What this agent does
	Harness        string         // Source harness: "claude" or "gemini"
	Model          string         // Model specification (passed through as-is)
	SystemPrompt   string         // Markdown body (the system prompt / instructions)
	Tools          []string       // Tool list (harness-native names)
	MaxTurns       int            // Max turns/iterations (0 = unset)
	Temperature    float64        // Temperature (Gemini only, 0 = unset)
	RawFrontMatter map[string]any // Full front matter for pass-through
	SourcePath     string         // Original file path
	ScionFormat    bool           // True if this is a native scion template (copy, don't convert)
	ParseError     error          // Non-nil when discovery found a template whose config could not be parsed
}

// Importer parses harness-specific agent definitions.
type Importer interface {
	// Detect returns a confidence score (0-10) that the file matches this harness format.
	Detect(path string, frontMatter map[string]any) int
	// Parse reads a single .md file and returns an ImportedAgent.
	Parse(path string) (*ImportedAgent, error)
	// ParseDir scans a directory for importable agent definitions.
	ParseDir(dir string) ([]*ImportedAgent, error)
}
