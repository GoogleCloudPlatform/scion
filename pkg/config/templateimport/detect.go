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
	"os"
	"strings"
)

// DetectHarness determines the harness type for a given .md file.
// It uses path-based detection first, then content-based scoring.
// Returns "claude", "gemini", or "" if detection fails.
func DetectHarness(path string) (string, error) {
	// Path-based detection: check parent directories
	if h := detectFromPath(path); h != "" {
		return h, nil
	}

	// Content-based detection: parse front matter and score
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}

	fm, _, err := ParseFrontMatter(data)
	if err != nil {
		return "", err
	}
	if fm == nil {
		return "", nil
	}

	return detectFromFrontMatter(fm), nil
}

// detectFromPath checks for .claude/agents/ or .gemini/agents/ directory components.
func detectFromPath(path string) string {
	// Prepend a separator so relative paths use the same boundary check.
	normalized := "/" + strings.ReplaceAll(path, "\\", "/")
	if strings.Contains(normalized, "/.claude/agents/") {
		return "claude"
	}
	if strings.Contains(normalized, "/.gemini/agents/") {
		return "gemini"
	}
	return ""
}

// detectFromFrontMatter scores front matter fields against known harness signatures.
func detectFromFrontMatter(fm map[string]any) string {
	claudeScore := scoreClaude(fm)
	geminiScore := scoreGemini(fm)

	if claudeScore >= 3 && claudeScore > geminiScore {
		return "claude"
	}
	if geminiScore >= 3 && geminiScore > claudeScore {
		return "gemini"
	}
	return ""
}

// scoreClaude returns a confidence score for Claude Code agent format.
func scoreClaude(fm map[string]any) int {
	score := 0

	// tools as comma-separated string (not array)
	if tools, ok := fm["tools"]; ok {
		if _, isStr := tools.(string); isStr {
			score += 3
		}
	}

	if _, ok := fm["permissionMode"]; ok {
		score += 3
	}
	if _, ok := fm["disallowedTools"]; ok {
		score += 2
	}
	if _, ok := fm["maxTurns"]; ok {
		score += 1
	}
	if _, ok := fm["memory"]; ok {
		score += 1
	}

	return score
}

// scoreGemini returns a confidence score for Gemini CLI agent format.
func scoreGemini(fm map[string]any) int {
	score := 0

	// tools as YAML array (not string)
	if tools, ok := fm["tools"]; ok {
		if _, isSlice := tools.([]any); isSlice {
			score += 3
		}
	}

	if _, ok := fm["kind"]; ok {
		score += 3
	}
	if _, ok := fm["timeout_mins"]; ok {
		score += 2
	}
	if _, ok := fm["temperature"]; ok {
		score += 2
	}
	if _, ok := fm["max_turns"]; ok {
		score += 1
	}

	return score
}
