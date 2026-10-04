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
	"net/http"
	"unicode/utf8"
)

// ghStatusError is a non-OK GitHub API response from ghResolveCommitSHA or
// ghListContents. Error() is the message those functions have always
// returned; status lets callers tell a 404 apart without matching text.
type ghStatusError struct {
	status int
	msg    string
}

func (e *ghStatusError) Error() string { return e.msg }

// isGHNotFound reports whether err comes from GitHub answering 404 for the
// ref or the skill path. Only these are remembered: they do not change
// between attempts made close together. Rate limits, 5xx, network errors
// and other statuses are never remembered.
func isGHNotFound(err error) bool {
	var se *ghStatusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// rememberGHNotFound remembers err for cacheKey for agent.FailureMemoTTL
// (see Server.ghFailures) when it is GitHub reporting the ref or skill path
// as not found. The key is the resolution cache key (ref plus token scope,
// see computeCacheKey).
func (s *Server) rememberGHNotFound(cacheKey string, err error) {
	if isGHNotFound(err) {
		s.ghFailures.Record(cacheKey, err)
	}
}

// maxGHErrorBody is how many bytes of a GitHub error response body a
// ghStatusError message keeps. It bounds the size of each remembered
// failure (and of the per-URI message returned to clients) when a server
// answers with a large error page.
const maxGHErrorBody = 512

// ghErrorBody returns body for use in a ghStatusError message, cut to
// maxGHErrorBody bytes with "..." appended when it is longer. The cut backs
// off to the start of a UTF-8 sequence so a multi-byte character is never
// split. The back-off is at most utf8.UTFMax-1 bytes, the longest a valid
// sequence can reach past the cut; a body that is not valid UTF-8 there is
// cut at that bound instead of shrinking further.
func ghErrorBody(body []byte) string {
	if len(body) <= maxGHErrorBody {
		return string(body)
	}
	n := maxGHErrorBody
	for n > maxGHErrorBody-(utf8.UTFMax-1) && !utf8.RuneStart(body[n]) {
		n--
	}
	return string(body[:n]) + "..."
}
