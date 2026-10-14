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

package runtimebroker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// Store a fixed reason phrase for skill resolver errors; full detail goes to
// server-side logs (ptone/scion#5061). The text the agent runtime returns
// to the hub (an API error body, a stored phase message, launch.error) is
// built here from the error code, the upstream HTTP status and a
// correlation id. The full resolver error is logged by
// logSkillResolutionFailure with the same correlation id.

// skillResolutionPhrases is the fixed reason phrase for each resolver
// error code. A code not listed gets skillResolutionDefaultPhrase.
var skillResolutionPhrases = map[string]string{
	agent.SkillErrCodeNotFound:            "skill not found",
	agent.SkillErrCodeForbidden:           "access denied",
	agent.SkillErrCodeRateLimited:         "rate limited by the skill source",
	agent.SkillErrCodeTimeout:             "timed out reaching the skill source",
	agent.SkillErrCodeUpstreamUnavailable: "skill source unavailable",
	agent.SkillErrCodeUnreachable:         "skill source unreachable",
	"invalid_uri":                         "invalid skill URI",
}

// skillResolutionDefaultPhrase is the reason phrase for a resolver error
// code with no phrase of its own.
const skillResolutionDefaultPhrase = "resolver error"

// skillResolutionPhrase returns the fixed reason phrase for code.
func skillResolutionPhrase(code string) string {
	if p, ok := skillResolutionPhrases[code]; ok {
		return p
	}
	return skillResolutionDefaultPhrase
}

// unparseableSkillRef stands in for an http(s) skill reference that does
// not parse as a URL.
const unparseableSkillRef = "(unparseable skill URL)"

// publicSkillRef returns the declared skill reference uri for user-visible
// text. An http(s) reference loses its userinfo, query and fragment, and
// its path is redacted by logging.RedactURL where that applies; any other
// reference (gh://, a registry name) is returned as is.
func publicSkillRef(uri string) string {
	lower := strings.ToLower(uri)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return unparseableSkillRef
	}
	u.User = nil
	u.RawQuery = ""
	u.ForceQuery = false
	u.Fragment = ""
	u.RawFragment = ""
	return logging.RedactURL(u)
}

// skillResolutionPublicText returns the user-visible text for e. For an
// install failure it is the existing install text, whose reason is already
// public (see agent.skillInstallError). For a resolver error it is a fixed
// reason phrase, the upstream HTTP status when one is known, and ref, the
// correlation id that also appears in the server-side log record. An empty
// ref is left out.
func skillResolutionPublicText(e *agent.SkillResolutionError, ref string) string {
	uri := publicSkillRef(e.URI)
	if e.Code == agent.SkillErrCodeInstallFailed {
		return fmt.Sprintf("required skill %q could not be installed: %s", uri, e.Message)
	}
	text := fmt.Sprintf("required skill %q could not be resolved: %s", uri, skillResolutionPhrase(e.Code))
	if e.Status > 0 {
		text += fmt.Sprintf(" (status %d)", e.Status)
	}
	if ref != "" {
		text += " (ref " + ref + ")"
	}
	return text
}

// skillResolutionCorrelationID returns the id that ties the stored text of
// a skill resolution failure to its server-side log record: the run id when
// there is one, else a new random id.
func skillResolutionCorrelationID(runID string) string {
	if runID != "" {
		return runID
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}

// textURLPattern matches an http(s) URL inside free text.
var textURLPattern = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// redactedLogValue replaces a query value or userinfo in a logged URL.
const redactedLogValue = "REDACTED"

// redactURLsInText returns s with every http(s) URL in it redacted for the
// server-side log: userinfo replaced, every query value replaced (the keys
// are kept), and the path redacted by logging.RedactURL where that applies.
// The rest of s is unchanged.
func redactURLsInText(s string) string {
	return textURLPattern.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "(unparseable URL)"
		}
		if u.User != nil {
			u.User = url.User(redactedLogValue)
		}
		if u.RawQuery != "" {
			parts := strings.Split(u.RawQuery, "&")
			for i, p := range parts {
				if key, _, ok := strings.Cut(p, "="); ok {
					parts[i] = key + "=" + redactedLogValue
				}
			}
			u.RawQuery = strings.Join(parts, "&")
		}
		return logging.RedactURL(u)
	})
}

// skillResolutionLogError returns the text of err for the server-side log,
// with its URLs redacted (see redactURLsInText).
func skillResolutionLogError(err error) string {
	if err == nil {
		return ""
	}
	return redactURLsInText(err.Error())
}

// logSkillResolutionFailure logs the full skill resolution error e with
// correlationID, the id the user-visible text carries. attrs are extra log
// attributes (agent_id, project_id, run_id).
func (s *Server) logSkillResolutionFailure(e *agent.SkillResolutionError, correlationID string, attrs ...any) {
	s.agentLifecycleLog.Warn("Required skill could not be resolved", append(attrs,
		"correlation_id", correlationID,
		"skill_uri", redactURLsInText(e.URI),
		"code", e.Code,
		"status", e.Status,
		"error", skillResolutionLogError(e))...)
}

// startErrorLogValue returns err for the "error" attribute of a start or
// create failure log record: a skill resolution failure as its redacted text
// (see skillResolutionLogError), any other error unchanged.
func startErrorLogValue(err error) any {
	var skillErr *agent.SkillResolutionError
	if errors.As(err, &skillErr) {
		return skillResolutionLogError(err)
	}
	return err
}
