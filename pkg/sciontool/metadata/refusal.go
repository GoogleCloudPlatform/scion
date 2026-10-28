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
	"net/http"
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
)

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
	code int
	body string
}

func (e *hubStatusError) Error() string { return fmt.Sprintf("hub returned %d: %s", e.code, e.body) }

// HTTPStatusCode returns the hub's HTTP status.
func (e *hubStatusError) HTTPStatusCode() int { return e.code }

// isHubRefusal reports whether err carries an HTTP 403 from the hub: the
// direct path's hubStatusError, or the hub client's StatusError (both have
// HTTPStatusCode).
func isHubRefusal(err error) bool {
	var sc interface{ HTTPStatusCode() int }
	return errors.As(err, &sc) && sc.HTTPStatusCode() == http.StatusForbidden
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

// noteFetchResult updates r after a hub fetch for what (a log label): a
// success clears it; a hub 403 opens or extends the window and logs one line
// with the next retry delay.
func (s *Server) noteFetchResult(r *refusalState, what string, err error) {
	if err == nil {
		r.succeeded()
		return
	}
	if isHubRefusal(err) {
		if d, stepped := r.refused(s.clock(), s.assignmentKey()); stepped {
			logRefusal("GCP %s: credential refused by hub (403); next attempt in %s", what, d.Round(time.Second))
		}
	}
}

// refusalLeft reports whether r's window is open for the current assignment,
// and the time left.
func (s *Server) refusalLeft(r *refusalState) (time.Duration, bool) {
	return r.active(s.clock(), s.assignmentKey)
}

// writeRefusal answers with the cached refusal.
func writeRefusal(w http.ResponseWriter, left time.Duration) {
	http.Error(w, fmt.Sprintf("credential refused by hub; retry after %s", left.Round(time.Second)), http.StatusForbidden)
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
