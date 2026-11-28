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

package githubfetch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// CooldownDefault is how long a credential scope is held back after a
	// rate-limit response that carries neither Retry-After nor
	// X-RateLimit-Reset.
	CooldownDefault = 60 * time.Second

	// CooldownMax caps how long a single rate-limit response can hold a
	// credential scope back, however far in the future the response says the
	// limit resets.
	CooldownMax = 5 * time.Minute

	// AnonScope is the cooldown and cache scope for unauthenticated requests.
	AnonScope = "anon"

	secondaryLimitPeek = 4096
)

// RateLimitError reports that a GitHub request was not completed because the
// credential scope in use is rate limited, either because this request got a
// rate-limit response or because an earlier one did and the cooldown has not
// ended yet (Sent is false in that case: no request went out). It carries no
// credential-derived material.
type RateLimitError struct {
	Ref             string
	RetryAt         time.Time
	Sent            bool
	Unauthenticated bool
}

func (e *RateLimitError) Error() string {
	subject := "GitHub API"
	if e.Ref != "" {
		subject = e.Ref
	}
	verb := "is in a rate-limit cooldown"
	if e.Sent {
		verb = "was rate limited by GitHub"
	}
	msg := fmt.Sprintf("%s %s; retry after %s", subject, verb, e.RetryAt.UTC().Format(time.RFC3339))
	if e.Unauthenticated {
		msg += "; set GITHUB_TOKEN for higher limits"
	}
	return msg
}

// WithRef returns a copy of e naming ref.
func (e *RateLimitError) WithRef(ref string) *RateLimitError {
	cp := *e
	cp.Ref = ref
	return &cp
}

// Cooldown records, per credential scope, a "not before" time set by a GitHub
// rate-limit response, and holds requests for that scope back until then.
//
// Scopes are map keys only and are never logged or put into errors.
type Cooldown struct {
	now func() time.Time

	mu    sync.Mutex
	until map[string]time.Time
}

// NewCooldown returns an empty cooldown tracker. now may be nil, in which case
// time.Now is used.
func NewCooldown(now func() time.Time) *Cooldown {
	if now == nil {
		now = time.Now
	}
	return &Cooldown{now: now, until: make(map[string]time.Time)}
}

var sharedCooldown = NewCooldown(nil)

// SharedCooldown returns the process-wide cooldown tracker.
func SharedCooldown() *Cooldown { return sharedCooldown }

// CooldownIdentity returns the legacy cooldown identity for token: the
// credential fingerprint, or AnonScope when token is empty.
func CooldownIdentity(token string) string {
	if token == "" {
		return AnonScope
	}
	return TokenFingerprint(token)
}

// CooldownIdentityForInstallation returns the cooldown identity for a GitHub
// App installation ID. Empty or "public" maps to AnonScope.
func CooldownIdentityForInstallation(installID string) string {
	if installID == "" || installID == "public" {
		return AnonScope
	}
	return "installation:" + installID
}

// CooldownIdentityIsAnonymous reports whether identity is AnonScope.
func CooldownIdentityIsAnonymous(identity string) bool {
	return identity == AnonScope
}

// Active reports whether scope is in a cooldown, and when it ends.
func (c *Cooldown) Active(scope string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.until[scope]
	if !ok || !c.now().Before(t) {
		return time.Time{}, false
	}
	return t, true
}

// Now returns the cooldown tracker's current clock time.
func (c *Cooldown) Now() time.Time { return c.now() }

// Record starts or extends the cooldown for scope by d, returning the end time.
// A later response never shortens a cooldown already in place.
func (c *Cooldown) Record(scope string, d time.Duration) time.Time {
	return c.RecordUntil(scope, c.now().Add(capCooldown(d)))
}

// RecordUntil starts or extends the cooldown for scope until t, returning the
// effective end time. A later call never shortens a cooldown already in place.
func (c *Cooldown) RecordUntil(scope string, t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for id, u := range c.until {
		if !now.Before(u) && id != scope {
			delete(c.until, id)
		}
	}
	if cur, ok := c.until[scope]; ok && cur.After(t) {
		return cur
	}
	c.until[scope] = t
	return t
}

// HasEntry reports whether scope currently has an entry in the cooldown map
// (even if already elapsed). Intended for tests verifying cleanup.
func (c *Cooldown) HasEntry(scope string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.until[scope]
	return ok
}

// ClearIfElapsed drops scope's entry once its cooldown has ended.
func (c *Cooldown) ClearIfElapsed(scope string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.until[scope]; ok && !c.now().Before(t) {
		delete(c.until, scope)
	}
}

// Do sends req with client on behalf of identity, unless identity is in a
// cooldown, in which case it returns a *RateLimitError without sending
// anything. Retained for existing pkg/agent and pkg/hub callers until Phases
// 4–5 migrate them onto Client.
func (c *Cooldown) Do(client *http.Client, req *http.Request, identity string) (*http.Response, error) {
	if t, ok := c.Active(identity); ok {
		return nil, &RateLimitError{RetryAt: t, Unauthenticated: CooldownIdentityIsAnonymous(identity)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	limited, err := IsHTTPResponseRateLimited(resp)
	if err != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	if limited {
		t := c.RecordUntil(identity, c.now().Add(CooldownDuration(resp.Header, c.now())))
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
		}
		return nil, &RateLimitError{RetryAt: t, Sent: true, Unauthenticated: CooldownIdentityIsAnonymous(identity)}
	}
	c.ClearIfElapsed(identity)
	return resp, nil
}

// IsRateLimitStatus reports whether a response status, headers, and optional
// body snippet represent a GitHub rate-limit response (429, 403 with
// X-RateLimit-Remaining: 0, or 403 with "secondary rate limit" in body).
func IsRateLimitStatus(status int, header http.Header, body []byte) bool {
	switch status {
	case http.StatusTooManyRequests:
		return true
	case http.StatusForbidden:
		if header != nil && header.Get("X-RateLimit-Remaining") == "0" {
			return true
		}
		if len(body) > 0 && strings.Contains(strings.ToLower(string(body)), "secondary rate limit") {
			return true
		}
	}
	return false
}

// IsHTTPResponseRateLimited reports whether resp is a GitHub rate-limit
// response (429, 403 with X-RateLimit-Remaining: 0, or 403 with secondary rate
// limit wording in its body, peeking and restoring up to 4 KiB).
func IsHTTPResponseRateLimited(resp *http.Response) (bool, error) {
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return true, nil
	case http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return true, nil
		}
		if resp.Body == nil {
			return false, nil
		}
		head, err := io.ReadAll(io.LimitReader(resp.Body, secondaryLimitPeek))
		if err != nil {
			return false, fmt.Errorf("failed to read GitHub 403 response: %w", err)
		}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
		return strings.Contains(strings.ToLower(string(head)), "secondary rate limit"), nil
	}
	return false, nil
}

// CooldownDuration derives the cooldown length from a rate-limit response's
// headers: Retry-After (seconds or HTTP date) first, then X-RateLimit-Reset
// (Unix seconds), else CooldownDefault; capped at CooldownMax.
func CooldownDuration(header http.Header, now time.Time) time.Duration {
	if header == nil {
		return CooldownDefault
	}
	if ra := strings.TrimSpace(header.Get("Retry-After")); ra != "" {
		if secs, ok := ParseRetryAfterSeconds(ra); ok && secs > 0 {
			return SecondsUpTo(secs, CooldownMax)
		}
		if t, err := http.ParseTime(ra); err == nil && t.After(now) {
			return capCooldown(t.Sub(now))
		}
	}
	if rs := strings.TrimSpace(header.Get("X-RateLimit-Reset")); rs != "" {
		if unix, err := strconv.ParseInt(rs, 10, 64); err == nil {
			if t := time.Unix(unix, 0); t.After(now) {
				return capCooldown(t.Sub(now))
			}
		}
	}
	return CooldownDefault
}

// ParseRetryAfterSeconds parses a Retry-After value given in seconds.
func ParseRetryAfterSeconds(v string) (int64, bool) {
	secs, err := strconv.ParseInt(v, 10, 64)
	if err == nil {
		return secs, true
	}
	if errors.Is(err, strconv.ErrRange) && v != "" && strings.Trim(v, "0123456789") == "" {
		return math.MaxInt64, true
	}
	return 0, false
}

// SecondsUpTo converts a non-negative count of seconds to a duration capped at max.
func SecondsUpTo(secs int64, max time.Duration) time.Duration {
	if secs <= 0 {
		return 0
	}
	if secs >= int64(max/time.Second) {
		return max
	}
	return time.Duration(secs) * time.Second
}

func capCooldown(d time.Duration) time.Duration {
	if d <= 0 {
		return CooldownDefault
	}
	if d > CooldownMax {
		return CooldownMax
	}
	return d
}
