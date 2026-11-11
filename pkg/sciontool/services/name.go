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

package services

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// maxServiceNameLen bounds a service Name. A legitimate service Name is a
// short identifier ("chrome", "vnc-server"); 100 is generous for that while
// keeping the resulting log filename (Name + the longest suffix,
// ".lifecycle.log") comfortably under typical filesystem NAME_MAX (255).
const maxServiceNameLen = 100

// ErrInvalidServiceName is wrapped by every error ValidateServiceName
// returns, so a caller can distinguish "this Name was refused" from any
// other error via errors.Is.
var ErrInvalidServiceName = errors.New("invalid service name")

// ValidateServiceName reports whether name is safe to use as a single path
// component when building a service's log file paths (openLogs appends a
// fixed suffix — ".stdout.log", ".stderr.log", ".lifecycle.log" — directly
// onto name and passes the result to an openat(2) relative to the log
// directory's own fd; openat does not split its name argument on "/", so
// name can smuggle extra path components, including ".." segments, straight
// through the no-follow/regular-file protections that guard the log
// directory itself).
//
// A service's Name comes from scion-services.yaml, which the workload can
// write directly (see readServicesYAML's doc comment in
// cmd/sciontool/commands/init.go for why root reading that file's *content*
// at all is accepted, not defended against, for the run-as-uid dimension —
// this is the analogous defense for the path dimension). Rejecting anything
// that could reach outside the log directory or otherwise misbehave as a
// path component closes that gap without needing any awareness of
// RequirePrivilegeDrop: a legitimate service Name is a plain identifier, so
// no valid caller is ever affected, on any runtime.
//
// Rejected: an empty name, ".", "..", any name containing a path separator
// ('/' or the platform's os.PathSeparator), any name containing a NUL byte
// or any other control character, and any name longer than
// maxServiceNameLen.
//
// The control-character rule (beyond NUL) exists independently of the
// path-escape defense above: a VALID name — one with no path separator,
// so it passes every other rule — can still contain a newline or other
// control character, and unlike an invalid name (which always goes through
// SafeNameForLog before it's logged), a valid name is logged raw at every
// log.TaggedInfo("service:"+name, ...) call site and in openLogs' own error
// messages. That would let a workload-chosen but otherwise "valid" name
// forge additional log lines. Note this is deliberately a narrow denylist
// (reject control characters) rather than a broader allowlist restricting
// the character set outright — the latter risks breaking existing
// legitimate service names and is tracked separately.
func ValidateServiceName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty", ErrInvalidServiceName)
	case len(name) > maxServiceNameLen:
		return fmt.Errorf("%w: longer than %d characters", ErrInvalidServiceName, maxServiceNameLen)
	case name == ".":
		return fmt.Errorf("%w: \".\"", ErrInvalidServiceName)
	case name == "..":
		return fmt.Errorf("%w: \"..\"", ErrInvalidServiceName)
	case strings.ContainsRune(name, '/'):
		return fmt.Errorf("%w: contains '/'", ErrInvalidServiceName)
	case os.PathSeparator != '/' && strings.ContainsRune(name, os.PathSeparator):
		return fmt.Errorf("%w: contains a path separator", ErrInvalidServiceName)
	case strings.ContainsRune(name, 0):
		return fmt.Errorf("%w: contains a NUL byte", ErrInvalidServiceName)
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return fmt.Errorf("%w: contains a control character", ErrInvalidServiceName)
	default:
		return nil
	}
}

// SafeNameForLog returns a version of an untrusted (possibly invalid,
// possibly adversarial) service Name that is safe to embed directly in a
// log line: truncated to a bounded length before quoting (so an
// attacker-supplied name cannot blow up log volume), and quoted via
// strconv.Quote, which both delimits it unambiguously and escapes any
// control character (including newlines) that could otherwise be used to
// forge additional log lines. Never log an untrusted service Name any other
// way.
func SafeNameForLog(name string) string {
	const maxLogLen = 80
	if len(name) > maxLogLen {
		name = name[:maxLogLen] + "...(truncated)"
	}
	return strconv.Quote(name)
}
