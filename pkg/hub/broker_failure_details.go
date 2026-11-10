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

import (
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// brokerCodeTemplateError is the runtime broker's code for a failure to
// prepare an agent's template (pkg/runtimebroker ErrCodeTemplateError).
const brokerCodeTemplateError = "template_error"

// brokerCauseFileNotFound is the template_error cause for a template or
// harness-config file missing on the broker.
const brokerCauseFileNotFound = "file_not_found"

// maxRelayedFileNameBytes bounds a relayed template file name.
const maxRelayedFileNameBytes = 255

// maxRelayedReasonRunes bounds a relayed local_worktree_refused reason; the
// CLI applies the same bound (cmd boundedBrokerText).
const maxRelayedReasonRunes = 1024

// urlWithUserinfoPattern matches a URL that carries userinfo
// (scheme://user[:password]@host). A reason holding one is not relayed.
var urlWithUserinfoPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^/[:space:]]*@`)

// runIDPattern is the form run ids are minted in (uuid.NewString(), at the
// hub's start dispatch and the runtime broker's start context): a canonical
// lower-case UUID. A broker-supplied run id of any other form is dropped,
// never cut and relayed; no path, URL or token can match it.
var runIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// tokenRunPattern matches a run of 20 or more characters, with no space,
// from the alphabets access tokens, keys and their encodings use: letters,
// digits and "_.+=~-". hasTokenShape adds the digit test.
var tokenRunPattern = regexp.MustCompile(`[A-Za-z0-9_.+=~-]{20,}`)

// brokerFailureDetails returns the details the hub relays with a broker
// failure on the generic 502 path, and the allowlisted details
// brokerRecordText adds to a recorded text: a fresh map of string values
// chosen by the broker's error code, never the broker's own map. A
// runtime_error gives its allowlisted cause (brokerRuntimeCauseDetails); a
// template_error gives cause "file_not_found" and, when it is a plain base
// name, the file; a local_worktree_refused gives its reason as one bounded
// printable line, unless it holds a URL with userinfo; a run_mismatch gives
// its currentRunId when that is a run id in the minted form (runIDPattern).
// Anything else gives nil.
func brokerFailureDetails(err error) map[string]interface{} {
	if d := brokerRuntimeCauseDetails(err); d != nil {
		return d
	}
	var se *brokerStatusError
	if !errors.As(err, &se) || se == nil {
		return nil
	}
	details := se.brokerErrorDetails()
	str := func(key string) (string, bool) {
		v, ok := details[key].(string)
		return v, ok
	}
	switch se.brokerErrorCode() {
	case brokerCodeTemplateError:
		if cause, _ := str("cause"); cause != brokerCauseFileNotFound {
			return nil
		}
		out := map[string]interface{}{"cause": brokerCauseFileNotFound}
		if f, ok := str("file"); ok && relayableFileName(f) {
			out["file"] = f
		}
		return out
	case api.BrokerErrCodeLocalWorktreeRefused:
		reason, ok := str(api.BrokerErrDetailReason)
		if !ok {
			return nil
		}
		// The userinfo check runs on the whole control-stripped text, before
		// the length cap, so a URL that straddles the cut is still caught.
		if urlWithUserinfoPattern.MatchString(printableText(reason, 0)) {
			return nil
		}
		reason = boundedRelayText(reason)
		if reason == "" {
			return nil
		}
		return map[string]interface{}{api.BrokerErrDetailReason: reason}
	case api.BrokerErrorCodeRunMismatch:
		// Only the run that holds the agent's name, by its exact key, and
		// only in the minted form; anything else is dropped.
		current, ok := str(api.BrokerErrorDetailCurrentRunID)
		if !ok || !runIDPattern.MatchString(current) {
			return nil
		}
		return map[string]interface{}{api.BrokerErrorDetailCurrentRunID: current}
	}
	return nil
}

// brokerRecordText is the text for a broker HTTP error answer se wherever a
// failed dispatch is reported as text (a recorded failure, a warning, or
// the message of a generic error response): se's own text (the status and,
// when well formed, the code), then the broker's JSON error message capped
// and rendered by renderBrokerText, then the broker's current run when
// brokerFailureDetails allows it. The message comes from one read
// (brokerErrorRawMessage), and relayableBrokerMessage judges both that raw
// text and the exact rendered text that is appended. A message that holds a
// URL with userinfo, a path separator or a token shape is replaced by
// brokerErrorGenericMessage, as is a missing one; a message that only
// repeats the code is left out. The raw body is never used.
func brokerRecordText(se *brokerStatusError) string {
	text := se.Error()
	raw, ok := se.brokerErrorRawMessage()
	rendered := renderBrokerText(raw, maxBrokerMessageBytes)
	switch {
	case ok && raw == se.brokerErrorCode() && brokerCodePattern.MatchString(raw):
		// se's text already names the code.
	case ok && relayableBrokerMessage(raw) && relayableBrokerMessage(rendered):
		text += ": " + rendered
	default:
		text += ": " + brokerErrorGenericMessage
	}
	if current, ok := brokerFailureDetails(se)[api.BrokerErrorDetailCurrentRunID].(string); ok {
		text += " (current run " + current + ")"
	}
	return text
}

// relayableBrokerMessage reports whether a broker's error message m may be
// relayed: it holds no URL with userinfo (urlWithUserinfoPattern), no path
// separator (the separator test of relayableFileName) and no token shape
// (hasTokenShape). The checks run on the whole message, before any cap, so
// a value that straddles the cap is still caught. A URL with userinfo always
// holds "://" and so already fails the separator test; the userinfo test is
// kept as defence in depth, should the separator test ever change.
func relayableBrokerMessage(m string) bool {
	return !urlWithUserinfoPattern.MatchString(m) && !strings.ContainsAny(m, `/\`) && !hasTokenShape(m)
}

// hasTokenShape reports whether s holds a run of tokenRunPattern with a
// digit in it: the shape of an access token, key or encoded secret, which
// words and snake_case codes lack.
func hasTokenShape(s string) bool {
	for _, run := range tokenRunPattern.FindAllString(s, -1) {
		if strings.ContainsAny(run, "0123456789") {
			return true
		}
	}
	return false
}

// relayableFileName reports whether f is a plain file base name the hub may
// relay: 1-255 bytes of valid UTF-8, no path separator, not "." or "..",
// its own filepath.Base, and unchanged by boundedRelayText.
func relayableFileName(f string) bool {
	if len(f) == 0 || len(f) > maxRelayedFileNameBytes || !utf8.ValidString(f) {
		return false
	}
	if strings.ContainsAny(f, `/\`) || f == "." || f == ".." {
		return false
	}
	return filepath.Base(f) == f && boundedRelayText(f) == f
}

// boundedRelayText returns s as a single printable line of at most
// maxRelayedReasonRunes runes: only graphic characters and ASCII space are
// kept (control, format, line and paragraph separator characters are
// dropped), longer text ends in "...", and surrounding space is trimmed.
// It is the same rule as cmd boundedBrokerText.
func boundedRelayText(s string) string {
	return printableText(s, maxRelayedReasonRunes)
}

// printableText keeps only graphic characters and ASCII space from s,
// stops after maxRunes runes with "..." when there is more (no cap when
// maxRunes <= 0), and trims surrounding space.
func printableText(s string, maxRunes int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if !unicode.IsGraphic(r) && r != ' ' {
			continue
		}
		if maxRunes > 0 && n == maxRunes {
			b.WriteString("...")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
