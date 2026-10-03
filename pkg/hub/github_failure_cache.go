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
	"sync"
	"time"
)

const (
	// ghFailureCacheTTL is how long resolveGitHubSkill remembers that GitHub
	// reported a gh:// ref as not found for a cache key (ref plus the token
	// scope, see computeCacheKey). Within this window a resolution of the
	// same key returns the remembered error without calling GitHub. It is
	// kept short because the cause can be fixed at any time (the path is
	// pushed, the App is granted access to the repo). The same value as the
	// broker-side resolution cache uses.
	ghFailureCacheTTL = time.Minute

	// ghMaxRememberedFailures bounds how many failures are remembered at
	// once. When the limit is reached after dropping expired entries, a new
	// failure is not remembered; not remembering is always safe, it only
	// means the next resolution asks GitHub again.
	ghMaxRememberedFailures = 1024
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

// rememberGHNotFound remembers err for cacheKey (see ghFailureCache) when
// it is GitHub reporting the ref or skill path as not found.
func (s *Server) rememberGHNotFound(cacheKey string, err error) {
	if isGHNotFound(err) {
		s.ghFailures.record(cacheKey, err)
	}
}

// maxGHErrorBody is how many bytes of a GitHub error response body a
// ghStatusError message keeps. It bounds the size of each remembered
// failure (and of the per-URI message returned to clients) when a server
// answers with a large error page.
const maxGHErrorBody = 512

// ghErrorBody returns body for use in a ghStatusError message, cut to
// maxGHErrorBody bytes with "..." appended when it is longer.
func ghErrorBody(body []byte) string {
	if len(body) <= maxGHErrorBody {
		return string(body)
	}
	return string(body[:maxGHErrorBody]) + "..."
}

// ghFailureCache remembers recent not-found resolutions by cache key, in
// memory only (per hub process; never written to the store). The zero
// value is ready to use and safe for concurrent use.
type ghFailureCache struct {
	mu       sync.Mutex
	failures map[string]ghRememberedFailure
}

type ghRememberedFailure struct {
	err       error
	expiresAt time.Time
}

// record remembers err for key until ghFailureCacheTTL from now, after
// dropping expired entries. Once ghMaxRememberedFailures unexpired failures
// are held, a failure for a new key is not remembered.
func (c *ghFailureCache) record(key string, err error) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failures == nil {
		c.failures = make(map[string]ghRememberedFailure)
	}
	for k, f := range c.failures {
		if !now.Before(f.expiresAt) {
			delete(c.failures, k)
		}
	}
	if _, ok := c.failures[key]; !ok && len(c.failures) >= ghMaxRememberedFailures {
		return
	}
	c.failures[key] = ghRememberedFailure{err: err, expiresAt: now.Add(ghFailureCacheTTL)}
}

// recent returns the failure remembered for key, or nil if there is none or
// it has expired.
func (c *ghFailureCache) recent(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.failures[key]
	if !ok {
		return nil
	}
	if !time.Now().Before(f.expiresAt) {
		delete(c.failures, key)
		return nil
	}
	return f.err
}

func (c *ghFailureCache) clear(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.failures, key)
}
