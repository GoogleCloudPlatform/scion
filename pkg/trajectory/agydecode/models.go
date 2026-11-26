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

package agydecode

import (
	"regexp"
	"strings"
)

// Static fallback table for Antigravity protobuf model IDs when a record does
// not carry a concrete model name. Concrete names learned from scanned rows
// take precedence over this table.
//
// Informed by pingdotgg/t3code (MIT) antigravityUsageReader.ts and verified
// live against agy 1.5.18.
var staticModelIDs = map[int64]string{
	246:  "gemini-2.5-pro",
	281:  "claude-sonnet-4",
	282:  "claude-sonnet-4",
	290:  "claude-opus-4",
	291:  "claude-opus-4",
	312:  "gemini-2.5-flash",
	313:  "gemini-2.5-flash-thinking",
	329:  "gemini-2.5-flash-thinking",
	330:  "gemini-2.5-flash-lite",
	333:  "claude-sonnet-4-5",
	334:  "claude-sonnet-4-5",
	340:  "claude-haiku-4-5",
	341:  "claude-haiku-4-5",
	342:  "gpt-oss-120b-medium",
	1016: "gemini-3.1-pro",
	1018: "gemini-3-flash-preview",
	1020: "gemini-3.5-flash-medium",
	1021: "gemini-3.1-flash-image",
	1026: "claude-opus-4-6",
	1035: "claude-sonnet-4-6",
	1036: "gemini-3.1-pro",
	1037: "gemini-3.1-pro",
	1047: "gemini-3-flash-preview",
	1050: "gemini-3.1-flash-lite",
	1071: "gemini-3.6-flash-high",
	1072: "gemini-3.6-flash-medium",
	1073: "gemini-3.6-flash-low",
	1084: "gemini-3-flash-preview",
	1132: "gemini-3.5-flash-high",
	1133: "gemini-3.5-flash-high",
	1187: "gemini-3.5-flash-extra-low",
	1196: "gemini-3.6-flash",
	1266: "gemini-3.6-flash",
	1298: "gemini-3.7-flash-high",
	1299: "gemini-3.7-flash-medium",
	1300: "gemini-3.7-flash-low",
	1301: "gemini-3.7-flash-tiered",
	1318: "gemini-3.8-flash-high",
	1319: "gemini-3.8-flash-medium",
	1320: "gemini-3.8-flash-low",
	1322: "gemini-3.8-flash-tiered",
	1402: "claude-opus-5-5",
	1417: "gemini-4-argon",
	1418: "gemini-4-argon",
	1419: "gemini-4-argon",
	1466: "gemini-4-neon",
	1537: "gemini-pro-orchestrator-neon",
}

var (
	parenSuffixRe   = regexp.MustCompile(`\s*\(([^)]*)\)\s*$`)
	whitespaceRe    = regexp.MustCompile(`\s+`)
	claudeReorderRe = regexp.MustCompile(`^claude-(\d+(?:\.\d+)?)-(sonnet|opus|haiku)`)
	concreteModelRe = regexp.MustCompile(`^(?:gemini-\d|claude-(?:sonnet|opus|haiku)-\d|gpt-)`)
	flashAliasRe    = regexp.MustCompile(`^gemini-3-flash-(?:agent(?:-[ab])?|[ab])$`)
	modelPrefixRe   = regexp.MustCompile(`^model[_-]`)
)

// NormalizeModelName converts an Antigravity display or wire model name into a
// canonical lowercase slug (e.g. "Gemini 4 Neon (Medium)" -> "gemini-4-neon-medium",
// "Claude 4.5 Sonnet" -> "claude-sonnet-4-5").
func NormalizeModelName(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return ""
	}
	normalized := strings.ToLower(s)
	normalized = parenSuffixRe.ReplaceAllString(normalized, "-$1")
	normalized = whitespaceRe.ReplaceAllString(normalized, "-")
	if strings.HasPrefix(normalized, "claude-") {
		normalized = claudeReorderRe.ReplaceAllString(normalized, "claude-$2-$1")
		normalized = strings.ReplaceAll(normalized, ".", "-")
	}
	return normalized
}

// IsConcreteModel reports whether model is a specific provider model name
// rather than a generic routing alias or placeholder.
func IsConcreteModel(model string) bool {
	return concreteModelRe.MatchString(model) && !flashAliasRe.MatchString(model)
}

func isUsableFallbackName(model string) bool {
	return model != "" && !modelPrefixRe.MatchString(model)
}

// LookupStaticModelID returns the static fallback model name for a numeric
// protobuf model ID, if known.
func LookupStaticModelID(id int64) (string, bool) {
	name, ok := staticModelIDs[id]
	return name, ok
}
