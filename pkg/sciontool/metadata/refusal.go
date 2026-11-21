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

package metadata

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/backoff"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
)

// Hub refusals of a GCP credential (ptone/scion#6436).
//
// Only successful tokens are cached. When the hub refuses to issue a GCP
// credential for the agent (HTTP 403), every tool request inside the agent
// would otherwise go back to the hub at once. Instead a refusal opens a
// back-off window (capped exponential delay with jitter, the shared
// sciontool backoff): during it, token requests get the cached refusal
// (HTTP 403) without a hub call, and one log line is written per back-off
// step. The next request after the window asks the hub again. A success
// resets the back-off, and so does a change of the agent's assignment (its
// service account or its hub credential), which is retried at once. Any
// other error keeps the earlier behaviour: 502 and no back-off.

// logRefusal writes the one line per back-off step (a variable so tests can
// count the lines).
var logRefusal = log.Error

const (
	refusalBackoffBase = 10 * time.Second
	refusalBackoffMax  = 5 * time.Minute

	rateLimitBackoffBase   = 2 * time.Second
	rateLimitBackoffMax    = 60 * time.Second
	rateLimitMaxRetryAfter = 5 * time.Minute
)

// rateLimitState tracks a shared HTTP 429 backoff window for GCP token
// fetches. Both access and identity token endpoints share the Hub's per-agent
// token bucket, so a 429 on either opens a backoff window with exponential
// backoff + jitter while honoring the Hub's Retry-After header when larger.
type rateLimitState struct {
	mu      sync.Mutex
	backoff backoff.Backoff
	until   time.Time
}

func newRateLimitState() *rateLimitState {
	return &rateLimitState{
		backoff: backoff.Backoff{
			Base:   rateLimitBackoffBase,
			Max:    rateLimitBackoffMax,
			Jitter: backoff.DefaultJitter,
		},
	}
}

func (rl *rateLimitState) active(now time.Time) (time.Duration, bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.until.IsZero() || !now.Before(rl.until) {
		return 0, false
	}
	return rl.until.Sub(now), true
}

func (rl *rateLimitState) limited(now time.Time, retryAfter time.Duration) (left time.Duration, stepped bool) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if !rl.until.IsZero() && now.Before(rl.until) {
		return rl.until.Sub(now), false
	}
	d := rl.backoff.Failure()
	if retryAfter > 0 {
		if retryAfter > rateLimitMaxRetryAfter {
			retryAfter = rateLimitMaxRetryAfter
		}
		if retryAfter > d {
			d = retryAfter
		}
	}
	rl.until = now.Add(d)
	return d, true
}

func (rl *rateLimitState) succeeded() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.backoff.Reset()
	rl.until = time.Time{}
}

// refusalState is the back-off state of one credential kind (the access
// token, or the identity tokens of every audience).
type refusalState struct {
	mu      sync.Mutex
	backoff backoff.Backoff
	until   time.Time
	key     string
}

func newRefusalState() *refusalState {
	return &refusalState{backoff: backoff.Backoff{Base: refusalBackoffBase, Max: refusalBackoffMax, Jitter: backoff.DefaultJitter}}
}

// active reports whether a refusal window is open at now for the current
// assignment, and the time left. key returns the current assignment key; it
// is called only when a window has been recorded. A different key (the
// assignment changed) clears the state, so the request goes to the hub at
// once.
func (r *refusalState) active(now time.Time, key func() string) (time.Duration, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.until.IsZero() {
		return 0, false
	}
	if key() != r.key {
		r.clearLocked()
		return 0, false
	}
	if now.Before(r.until) {
		return r.until.Sub(now), true
	}
	return 0, false
}

// refused records a hub refusal for assignment key at now. It returns the
// time until the hub is asked again, and stepped = true when this refusal
// took a new back-off step (the caller logs one line for it). A refusal
// recorded while a window for the same key is still open (a request that
// raced the window, the refresh loop, another identity-token audience) does
// not take another step and must not be logged: one step, one line.
func (r *refusalState) refused(now time.Time, key string) (left time.Duration, stepped bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if key == r.key && !r.until.IsZero() && now.Before(r.until) {
		return r.until.Sub(now), false
	}
	if key != r.key {
		r.backoff.Reset()
	}
	d := r.backoff.Failure()
	r.until = now.Add(d)
	r.key = key
	return d, true
}

// succeeded clears the state after the hub issued a credential.
func (r *refusalState) succeeded() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearLocked()
}

func (r *refusalState) clearLocked() {
	r.backoff.Reset()
	r.until = time.Time{}
	r.key = ""
}

// hubStatusError is the direct HTTP path's error for a non-200 hub answer.
type hubStatusError struct {
	code       int
	body       string
	retryAfter time.Duration
}

func (e *hubStatusError) Error() string { return fmt.Sprintf("hub returned %d: %s", e.code, e.body) }

// HTTPStatusCode returns the hub's HTTP status.
func (e *hubStatusError) HTTPStatusCode() int { return e.code }

// RetryAfterDuration returns the parsed Retry-After duration, if present.
func (e *hubStatusError) RetryAfterDuration() time.Duration { return e.retryAfter }

// parseRetryAfter parses an HTTP Retry-After header (delta-seconds or
// HTTP-date) relative to now.
func parseRetryAfter(val string, now time.Time) time.Duration {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0
	}
	if secs, err := strconv.Atoi(val); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(val); err == nil {
		d := t.Sub(now)
		if d > 0 {
			return d
		}
	}
	return 0
}

// isHubRefusal reports whether err carries an HTTP 403 from the hub: the
// direct path's hubStatusError, or the hub client's StatusError (both have
// HTTPStatusCode).
func isHubRefusal(err error) bool {
	var sc interface{ HTTPStatusCode() int }
	return errors.As(err, &sc) && sc.HTTPStatusCode() == http.StatusForbidden
}

// isHubRateLimited reports whether err carries an HTTP 429 from the hub.
func isHubRateLimited(err error) bool {
	var sc interface{ HTTPStatusCode() int }
	return errors.As(err, &sc) && sc.HTTPStatusCode() == http.StatusTooManyRequests
}

// hubRetryAfter extracts a Retry-After duration from err if the error type
// implements RetryAfterDuration().
func hubRetryAfter(err error) time.Duration {
	var ra interface{ RetryAfterDuration() time.Duration }
	if errors.As(err, &ra) {
		return ra.RetryAfterDuration()
	}
	return 0
}

// assignmentKey identifies the agent's current assignment: its service
// account and a hash of its current hub credential. The credential itself is
// never stored or logged.
func (s *Server) assignmentKey() string {
	sum := sha256.Sum256([]byte(s.authToken()))
	return s.config.SAEmail + "|" + hex.EncodeToString(sum[:8])
}

func (s *Server) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// noteFetchResult updates r and the shared rate-limit state after a hub fetch
// for what (a log label): a success clears both; a hub 403 opens or extends
// the refusal window; a hub 429 opens or extends the rate-limit backoff window
// (honoring Retry-After if present).
func (s *Server) noteFetchResult(r *refusalState, what string, err error) {
	if err == nil {
		r.succeeded()
		s.rateLimit.succeeded()
		return
	}
	if isHubRefusal(err) {
		if d, stepped := r.refused(s.clock(), s.assignmentKey()); stepped {
			logRefusal("GCP %s: credential refused by hub (403); next attempt in %s", what, d.Round(time.Second))
		}
		return
	}
	if isHubRateLimited(err) {
		if d, stepped := s.rateLimit.limited(s.clock(), hubRetryAfter(err)); stepped {
			logRefusal("GCP %s: rate limited by hub (429); next attempt in %s", what, d.Round(time.Second))
		}
	}
}

// refusalLeft reports whether r's window is open for the current assignment,
// and the time left.
func (s *Server) refusalLeft(r *refusalState) (time.Duration, bool) {
	return r.active(s.clock(), s.assignmentKey)
}

// rateLimitLeft reports whether the shared 429 backoff window is open, and
// the time left.
func (s *Server) rateLimitLeft() (time.Duration, bool) {
	return s.rateLimit.active(s.clock())
}

// writeRefusal answers with the cached refusal.
func writeRefusal(w http.ResponseWriter, left time.Duration) {
	http.Error(w, fmt.Sprintf("credential refused by hub; retry after %s", left.Round(time.Second)), http.StatusForbidden)
}

// writeRateLimit answers with 429 Too Many Requests and a Retry-After header.
func writeRateLimit(w http.ResponseWriter, left time.Duration) {
	secs := max(1, int(math.Ceil(left.Seconds())))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	http.Error(w, fmt.Sprintf("rate limited by hub; retry after %s", left.Round(time.Second)), http.StatusTooManyRequests)
}

// serveRefusal answers with the cached refusal while r's window is open,
// and reports whether it did.
func (s *Server) serveRefusal(w http.ResponseWriter, r *refusalState) bool {
	left, ok := s.refusalLeft(r)
	if ok {
		writeRefusal(w, left)
	}
	return ok
}

// serveRateLimit answers with 429 + Retry-After while the shared rate-limit
// backoff window is open, and reports whether it did.
func (s *Server) serveRateLimit(w http.ResponseWriter) bool {
	left, ok := s.rateLimitLeft()
	if ok {
		writeRateLimit(w, left)
	}
	return ok
}
