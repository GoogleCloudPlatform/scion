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

package agent

// Fixed reason phrases for skill resolution errors (ptone/scion#5061,
// ptone/scion#5079). The agent runtime's stored text and the Hub's skill
// resolve errors both show the phrase for an error code in place of the
// resolver's own text; the full text goes to the server-side log.

// Error codes of the Hub's federated skill registry lookups.
const (
	SkillErrCodeUnknownRegistry   = "unknown_registry"
	SkillErrCodeRegistryDisabled  = "registry_disabled"
	SkillErrCodeWrongRegistryType = "wrong_registry_type"
	SkillErrCodeFederationError   = "federation_error"
	SkillErrCodeTrustViolation    = "trust_violation"
	// SkillErrCodeInternalError has no phrase of its own (see
	// SkillResolutionDefaultPhrase).
	SkillErrCodeInternalError = "internal_error"
)

// SkillErrCodeStorageError is the code of a Hub registry skill whose
// stored files cannot be served.
const SkillErrCodeStorageError = "storage_error"

// skillResolutionPhrases is the fixed reason phrase for each skill
// resolution error code. A code not listed gets
// SkillResolutionDefaultPhrase.
var skillResolutionPhrases = map[string]string{
	SkillErrCodeNotFound:            "skill not found",
	SkillErrCodeForbidden:           "access denied",
	SkillErrCodeRateLimited:         "rate limited by the skill source",
	SkillErrCodeTimeout:             "timed out reaching the skill source",
	SkillErrCodeUpstreamUnavailable: "skill source unavailable",
	SkillErrCodeUnreachable:         "skill source unreachable",
	SkillErrCodeInvalidURI:          "invalid skill URI",
	SkillErrCodeUnsupportedContent:  "unsupported skill content",
	SkillErrCodeUnknownRegistry:     "skill registry not configured",
	SkillErrCodeRegistryDisabled:    "skill registry disabled",
	SkillErrCodeWrongRegistryType:   "unsupported skill registry type",
	SkillErrCodeFederationError:     "skill registry request failed",
	SkillErrCodeTrustViolation:      "skill content hash not trusted",
	SkillErrCodeStorageError:        "skill files unavailable",
}

// SkillResolutionDefaultPhrase is the reason phrase for a skill resolution
// error code with no phrase of its own.
const SkillResolutionDefaultPhrase = "resolver error"

// SkillResolutionPhrase returns the fixed reason phrase for code.
func SkillResolutionPhrase(code string) string {
	if p, ok := skillResolutionPhrases[code]; ok {
		return p
	}
	return SkillResolutionDefaultPhrase
}
