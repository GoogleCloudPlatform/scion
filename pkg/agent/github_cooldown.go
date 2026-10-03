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

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// GitHubCooldownDefault is how long a credential identity is held back
	// after a rate-limit response that carries neither Retry-After nor
	// X-RateLimit-Reset.
	GitHubCooldownDefault = 60 * time.Second

	// GitHubCooldownMax caps how long a single rate-limit response can hold
	// a credential identity back, however far in the future the response
	// says the limit resets (a primary limit can report a reset up to an
	// hour out).
	GitHubCooldownMax = 5 * time.Minute

	// GitHubRateLimitedCode is the ResolveError / ResolveSkillError code
	// reported for a ref that failed because its credential identity is
	// rate limited by GitHub.
	GitHubRateLimitedCode = SkillErrCodeRateLimited

	// githubAnonIdentity is the cooldown identity for unauthenticated calls.
	// GitHub limits those per source address, so every unauthenticated
	// caller in the process shares one identity.
	githubAnonIdentity = "anon"

	// githubSecondaryLimitPeek bounds how much of a 403 body is read to look
	// for the secondary-limit wording.
	githubSecondaryLimitPeek = 4096
)

// GitHubRateLimitError reports that a GitHub request was not completed
// because the credential identity in use is rate limited, either because
// this request got a rate-limit response or because an earlier one did and
// the cooldown has not ended yet (Sent is false in that case: no request
// went out). It carries no credential-derived material.
type GitHubRateLimitError struct {
	// Ref names the skill ref being resolved, when known.
	Ref string
	// RetryAt is when the cooldown for the credential identity ends.
	RetryAt time.Time
	// Sent reports whether a request was sent and answered with a
	// rate-limit response, as opposed to being held back by the cooldown.
	Sent bool
	// Unauthenticated reports that the request had no credential, so the
	// shared per-address limit applies.
	Unauthenticated bool
}

func (e *GitHubRateLimitError) Error() string {
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

// WithRef returns a copy of e naming ref. A copy, not a mutation: the same
// error value can be shared between coalesced callers.
func (e *GitHubRateLimitError) WithRef(ref string) *GitHubRateLimitError {
	cp := *e
	cp.Ref = ref
	return &cp
}

// GitHubCooldown records, per credential identity, a "not before" time set
// by a GitHub rate-limit response, and holds requests for that identity back
// until then. It is the one implementation used by both the broker-side
// GitHubSkillResolver and the hub's gh:// resolver, so both behave the same
// way under a rate limit.
//
// Identities come from GitHubCooldownIdentity; they are map keys only and
// are never logged or put into errors.
type GitHubCooldown struct {
	now func() time.Time

	mu    sync.Mutex
	until map[string]time.Time
}

// NewGitHubCooldown returns an empty cooldown tracker. now may be nil, in
// which case time.Now is used; tests pass a fake clock.
func NewGitHubCooldown(now func() time.Time) *GitHubCooldown {
	if now == nil {
		now = time.Now
	}
	return &GitHubCooldown{now: now, until: make(map[string]time.Time)}
}

var sharedGitHubCooldown = NewGitHubCooldown(nil)

// SharedGitHubCooldown returns the process-wide tracker. GitHub applies its
// limits per credential, not per resolver instance, so resolvers built per
// request (and, in a combined hub and broker process, both sides) share it.
func SharedGitHubCooldown() *GitHubCooldown { return sharedGitHubCooldown }

// GitHubCooldownIdentity returns the cooldown identity for token: the
// credential fingerprint, or a fixed anonymous identity when token is empty.
func GitHubCooldownIdentity(token string) string {
	if token == "" {
		return githubAnonIdentity
	}
	return credentialFingerprint(token)
}

// GitHubCooldownIdentityForInstallation returns the cooldown identity for
// requests made with a GitHub App installation token. GitHub limits those
// per installation, and a fresh installation token is minted per request,
// so a fingerprint of each token would never match the next one. installID
// "public" or "" means unauthenticated and maps to the anonymous identity.
func GitHubCooldownIdentityForInstallation(installID string) string {
	if installID == "" || installID == "public" {
		return githubAnonIdentity
	}
	return "installation:" + installID
}

// GitHubCooldownIdentityIsAnonymous reports whether identity is the one used
// for unauthenticated requests. Use it, not an empty credential, to decide
// whether a request was unauthenticated: an installation-backed request is
// authenticated by its installation even if no credential value is at hand.
func GitHubCooldownIdentityIsAnonymous(identity string) bool {
	return identity == githubAnonIdentity
}

// Active reports whether identity is in a cooldown, and when it ends.
func (c *GitHubCooldown) Active(identity string) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.until[identity]
	if !ok || !c.now().Before(t) {
		return time.Time{}, false
	}
	return t, true
}

// record starts (or extends) the cooldown for identity until t. A later
// response never shortens a cooldown already in place.
func (c *GitHubCooldown) record(identity string, t time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for id, u := range c.until {
		if !now.Before(u) && id != identity {
			delete(c.until, id)
		}
	}
	if cur, ok := c.until[identity]; ok && cur.After(t) {
		return cur
	}
	c.until[identity] = t
	return t
}

// clearIfElapsed drops identity's entry once its cooldown has ended. Called
// on the first non-rate-limited response after the cooldown.
func (c *GitHubCooldown) clearIfElapsed(identity string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.until[identity]; ok && !c.now().Before(t) {
		delete(c.until, identity)
	}
}

// Do sends req with client on behalf of identity, unless identity is in a
// cooldown, in which case it returns a *GitHubRateLimitError without sending
// anything. A rate-limit response starts a cooldown and is returned as a
// *GitHubRateLimitError (its body closed); any other response is returned
// as-is and ends an elapsed cooldown. Transport errors are returned
// unchanged and do not affect the cooldown.
func (c *GitHubCooldown) Do(client *http.Client, req *http.Request, identity string) (*http.Response, error) {
	if t, ok := c.Active(identity); ok {
		return nil, &GitHubRateLimitError{RetryAt: t, Unauthenticated: GitHubCooldownIdentityIsAnonymous(identity)}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	limited, err := isGitHubRateLimitResponse(resp)
	if err != nil {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, err
	}
	if limited {
		t := c.record(identity, c.now().Add(githubCooldownFor(resp, c.now())))
		if resp.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64*1024))
			_ = resp.Body.Close()
		}
		return nil, &GitHubRateLimitError{RetryAt: t, Sent: true, Unauthenticated: GitHubCooldownIdentityIsAnonymous(identity)}
	}
	c.clearIfElapsed(identity)
	return resp, nil
}

// isGitHubRateLimitResponse reports whether resp is a GitHub rate-limit
// response: a 429, or a 403 that either reports X-RateLimit-Remaining: 0 or
// carries the secondary-limit wording in its body. For a 403 the start of
// the body is read to check, then put back so the caller still sees it in
// full.
func isGitHubRateLimitResponse(resp *http.Response) (bool, error) {
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
		head, err := io.ReadAll(io.LimitReader(resp.Body, githubSecondaryLimitPeek))
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

// githubCooldownFor derives the cooldown length from a rate-limit response:
// Retry-After (seconds or an HTTP date) first, then X-RateLimit-Reset (Unix
// seconds), else GitHubCooldownDefault; capped at GitHubCooldownMax. A value
// that is not in the future is ignored in favour of the next source.
func githubCooldownFor(resp *http.Response, now time.Time) time.Duration {
	d := GitHubCooldownDefault
	if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
		if secs, err := strconv.ParseInt(ra, 10, 64); err == nil && secs > 0 {
			return capCooldown(time.Duration(secs) * time.Second)
		}
		if t, err := http.ParseTime(ra); err == nil && t.After(now) {
			return capCooldown(t.Sub(now))
		}
	}
	if rs := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); rs != "" {
		if unix, err := strconv.ParseInt(rs, 10, 64); err == nil {
			if t := time.Unix(unix, 0); t.After(now) {
				return capCooldown(t.Sub(now))
			}
		}
	}
	return d
}

func capCooldown(d time.Duration) time.Duration {
	if d > GitHubCooldownMax {
		return GitHubCooldownMax
	}
	return d
}
