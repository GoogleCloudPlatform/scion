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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts/remotefetch"
)

// Kind classifies a GitHub fetch failure.
type Kind string

const (
	KindNotFound           Kind = "not_found"
	KindAuthRejected       Kind = "auth_rejected"
	KindForbidden          Kind = "forbidden"
	KindRateLimited        Kind = "rate_limited"
	KindTooLarge           Kind = "too_large"
	KindTimeout            Kind = "timeout"
	KindUnreachable        Kind = "unreachable"
	KindUnsupportedContent Kind = "unsupported_content"
	KindUpstream           Kind = "upstream"
	KindInvalidSource      Kind = "invalid_source"
)

// Error is a classified GitHub fetch failure. Error() never includes a token,
// URL query string, or redirect URL.
type Error struct {
	Kind       Kind
	Status     int
	RetryAfter time.Duration
	Repo       string // "owner/repo" when known
	CredKind   CredKind
	Hint       string
	transient  bool // true for transient network/timeout errors eligible for retry
	err        error
}

func (e *Error) Error() string {
	var msg string
	switch e.Kind {
	case KindNotFound:
		if e.CredKind == CredAnonymous {
			msg = "GitHub repository, ref, or path not found, or private and no credential is configured"
		} else {
			msg = "GitHub repository, ref, or path not found, or the configured credential cannot read it"
		}
	case KindAuthRejected:
		msg = "GitHub rejected the configured credential (invalid or expired)"
	case KindForbidden:
		msg = "GitHub denied access to the repository"
	case KindRateLimited:
		if e.RetryAfter > 0 {
			msg = fmt.Sprintf("GitHub rate limit exceeded; retry after %s", e.RetryAfter.Round(time.Second))
		} else {
			msg = "GitHub rate limit exceeded"
		}
	case KindTooLarge:
		msg = "GitHub source exceeds the size or file count limit"
	case KindTimeout:
		msg = "timed out fetching from GitHub"
	case KindUnreachable:
		msg = "could not reach GitHub"
	case KindUnsupportedContent:
		msg = "GitHub source contains unsupported content"
	case KindInvalidSource:
		msg = "invalid GitHub source URL"
	case KindUpstream:
		if e.Status > 0 {
			msg = fmt.Sprintf("GitHub returned HTTP %d", e.Status)
		} else {
			msg = "GitHub returned an invalid or inconsistent response"
		}
	default:
		msg = "GitHub fetch failed"
	}
	if e.Repo != "" {
		msg = e.Repo + ": " + msg
	}
	if e.Hint != "" {
		msg += " (" + e.Hint + ")"
	}
	return msg
}

func (e *Error) Unwrap() error {
	return e.err
}

// ClassifyStatus maps a non-2xx GitHub HTTP status and its allowlisted headers
// and body snippet into a typed *Error.
func ClassifyStatus(status int, header http.Header, body []byte, repo string, cred Credential, now time.Time) *Error {
	base := &Error{
		Status:   status,
		Repo:     repo,
		CredKind: cred.Kind,
		Hint:     cred.Hint,
	}
	if cred.IsAnonymous() {
		base.CredKind = CredAnonymous
	}

	switch status {
	case http.StatusNotFound:
		base.Kind = KindNotFound
		return base
	case http.StatusUnauthorized:
		base.Kind = KindAuthRejected
		return base
	case http.StatusTooManyRequests:
		base.Kind = KindRateLimited
		base.RetryAfter = CooldownDuration(header, now)
		return base
	case http.StatusForbidden:
		if IsRateLimitStatus(status, header, body) {
			base.Kind = KindRateLimited
			base.RetryAfter = CooldownDuration(header, now)
			return base
		}
		base.Kind = KindForbidden
		if header != nil && header.Get("X-GitHub-SSO") != "" {
			if base.Hint != "" {
				base.Hint += "; SAML SSO authorization required for this credential"
			} else {
				base.Hint = "SAML SSO authorization required for this credential"
			}
		}
		return base
	default:
		base.Kind = KindUpstream
		return base
	}
}

// ClassifyRemoteFetchError maps a remotefetch error (or generic transport
// error) into a typed *Error. It never copies raw URLs or tokens into the
// returned Error.
func ClassifyRemoteFetchError(err error, repo string, cred Credential, now time.Time) *Error {
	if err == nil {
		return nil
	}
	var ghe *Error
	if errors.As(err, &ghe) {
		return ghe
	}
	var fe *remotefetch.Error
	if errors.As(err, &fe) {
		switch fe.Reason {
		case remotefetch.ReasonStatus:
			return ClassifyStatus(fe.Status, fe.Header, fe.Body, repo, cred, now)
		case remotefetch.ReasonTooLarge:
			return &Error{Kind: KindTooLarge, Repo: repo, CredKind: cred.Kind}
		case remotefetch.ReasonTimeout:
			return &Error{Kind: KindTimeout, Repo: repo, CredKind: cred.Kind, transient: true}
		case remotefetch.ReasonConnect:
			return &Error{Kind: KindUnreachable, Repo: repo, CredKind: cred.Kind, transient: true}
		case remotefetch.ReasonResolve,
			remotefetch.ReasonDeniedAddress,
			remotefetch.ReasonHostNotAllowed,
			remotefetch.ReasonRedirects,
			remotefetch.ReasonScheme,
			remotefetch.ReasonPort,
			remotefetch.ReasonHost,
			remotefetch.ReasonBadURL,
			remotefetch.ReasonUserinfo,
			remotefetch.ReasonAuthorization:
			return &Error{Kind: KindUnreachable, Repo: repo, CredKind: cred.Kind}
		default:
			return &Error{Kind: KindUpstream, Repo: repo, CredKind: cred.Kind}
		}
	}
	if strings.Contains(strings.ToLower(err.Error()), "deadline exceeded") {
		return &Error{Kind: KindTimeout, Repo: repo, CredKind: cred.Kind}
	}
	return &Error{Kind: KindUnreachable, Repo: repo, CredKind: cred.Kind}
}
