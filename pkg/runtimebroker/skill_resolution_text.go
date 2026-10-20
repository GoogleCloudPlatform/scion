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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
)

// Store a fixed reason phrase for skill resolver errors; full detail goes to
// server-side logs (ptone/scion#5061). The text the agent runtime returns
// to the hub (an API error body, a stored phase message, launch.error) is
// built here from the error code, the upstream HTTP status and a
// correlation id. The full resolver error is logged by
// logSkillResolutionFailure with the same correlation id.

// skillResolutionPhrase returns agent.SkillResolutionPhrase(code). The
// phrase map lives in pkg/agent, so that the Hub's skill resolve errors
// share it.
func skillResolutionPhrase(code string) string { return agent.SkillResolutionPhrase(code) }

// maxPublicSkillRefLen bounds the skill reference shown to the user, as
// quoted in the text (without the quotes) and as the skill detail, so the
// correlation id at the end of the text survives the hub's cut of a stored
// failure reason to 512 bytes.
const maxPublicSkillRefLen = 200

// redactSkillRef returns agent.RedactSkillRef(uri). The skill reference
// and URL redaction live in pkg/agent, so that every log record of a
// declared skill reference shares them (see agent.LoggedSkillRef).
func redactSkillRef(uri string) string { return agent.RedactSkillRef(uri) }

// publicSkillRef returns redactSkillRef(uri) cut to maxPublicSkillRefLen
// bytes (see capText), for the skill detail of an error response.
func publicSkillRef(uri string) string {
	return capText(redactSkillRef(uri), maxPublicSkillRefLen)
}

// quotedSkillRef returns redactSkillRef(uri) quoted as by %q. When the
// quoted text between the quotes is longer than maxPublicSkillRefLen bytes,
// it is cut at an escape boundary and ends in an ellipsis: the cap applies
// after quoting, because %q writes a control or invalid byte as four bytes.
func quotedSkillRef(uri string) string {
	ref := redactSkillRef(uri)
	q := strconv.Quote(ref)
	if len(q)-2 <= maxPublicSkillRefLen {
		return q
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(ref); {
		_, w := utf8.DecodeRuneInString(ref[i:])
		esc := strconv.Quote(ref[i : i+w])
		esc = esc[1 : len(esc)-1]
		if b.Len()-1+len(esc) > maxPublicSkillRefLen {
			b.WriteString("…")
			break
		}
		b.WriteString(esc)
		i += w
	}
	b.WriteByte('"')
	return b.String()
}

// capText returns s cut to at most limit bytes at a rune boundary, with an
// ellipsis appended when it was cut. limit should be at least
// utf8.UTFMax, so that a cut text keeps at least one rune.
func capText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// skillResolutionPublicText returns the user-visible text for e. For an
// install failure it is the existing install text, whose reason is already
// public (see agent.skillInstallError). For a resolver error it is a fixed
// reason phrase, the upstream HTTP status when one is known, and ref, the
// correlation id that also appears in the server-side log record. An empty
// ref is left out.
func skillResolutionPublicText(e *agent.SkillResolutionError, ref string) string {
	uri := quotedSkillRef(e.URI)
	if e.Code == agent.SkillErrCodeInstallFailed {
		return fmt.Sprintf("required skill %s could not be installed: %s", uri, e.Message)
	}
	text := fmt.Sprintf("required skill %s could not be resolved: %s", uri, skillResolutionPhrase(e.Code))
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

// skillLogText returns the full text of e for the server-side log, built
// from its parts: the reference as redactSkillRef gives it, and the
// message with its URLs redacted (see agent.RedactURLsInText). When
// redactSkillRef changed the reference, the message is withheld (see
// agent.ResolverMessageWithheld): a resolver message can quote the
// reference, or parts of it, in any form. The code, status and
// correlation id stay in their own log attributes.
func skillLogText(e *agent.SkillResolutionError) string {
	redacted := *e
	redacted.URI = redactSkillRef(e.URI)
	redacted.Message = agent.ResolverReasonForLog(e.URI, e.Message)
	return redacted.Error()
}

// skillFailure is a skill resolution failure in a create or start error,
// with the correlation id that its user-visible text and its log record
// carry.
type skillFailure struct {
	err *agent.SkillResolutionError
	ref string
}

// skillFailureFrom returns the skill resolution failure in err's chain, with
// the correlation id for runID (see skillResolutionCorrelationID), or nil
// when there is none.
func skillFailureFrom(err error, runID string) *skillFailure {
	var skillErr *agent.SkillResolutionError
	if !errors.As(err, &skillErr) {
		return nil
	}
	return &skillFailure{err: skillErr, ref: skillResolutionCorrelationID(runID)}
}

// publicText returns the user-visible text of f (see
// skillResolutionPublicText).
func (f *skillFailure) publicText() string {
	return skillResolutionPublicText(f.err, f.ref)
}

// failureLogAttrs returns the error attributes of a general create or start
// failure log record. For a skill resolution failure f they are its
// user-visible text and correlation id: the full text is in the one record
// logSkillResolutionFailure writes. For any other error (f nil) they are
// err's text, through sanitizeFailureCause.
func failureLogAttrs(err error, f *skillFailure) []any {
	if f == nil {
		return []any{"error", sanitizeFailureCause(err.Error())}
	}
	return []any{"error", f.publicText(), "correlation_id", f.ref}
}

// runtimeCauseLogAttrs returns runtimeFailureLogAttrs(err) for a general
// failure record when there is no skill failure f, and nothing when there
// is one: the runtime cause of a skill failure is the skill failure itself,
// whose full text belongs only in the record logSkillResolutionFailure
// writes.
func runtimeCauseLogAttrs(err error, f *skillFailure) []any {
	if f != nil {
		return nil
	}
	return runtimeFailureLogAttrs(err)
}

// spanErrorText returns err's text for a trace span status: for an error
// holding a skill resolution failure, the failure's redacted log text (see
// skillLogText), with a fixed suffix when err also holds
// context.DeadlineExceeded or context.Canceled; for any other error, err's
// text unchanged.
func spanErrorText(err error) string {
	var skillErr *agent.SkillResolutionError
	if !errors.As(err, &skillErr) {
		return err.Error()
	}
	text := skillLogText(skillErr)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		text += "; context deadline exceeded"
	case errors.Is(err, context.Canceled):
		text += "; context canceled"
	}
	return text
}

// skillFailureLogMsg is the message of the log record that carries the full
// text of a skill resolution or install failure.
const skillFailureLogMsg = "Required skill failed"

// logSkillResolutionFailure logs the full text of the skill failure f, with
// its URLs redacted, under f's correlation id. attrs are extra log
// attributes (agent_id, project_id, run_id).
func (s *Server) logSkillResolutionFailure(f *skillFailure, attrs ...any) {
	s.agentLifecycleLog.Warn(skillFailureLogMsg, append(attrs,
		"correlation_id", f.ref,
		"skill_uri", agent.LoggedSkillRef(f.err.URI),
		"code", f.err.Code,
		"status", f.err.Status,
		"error", skillLogText(f.err))...)
}
