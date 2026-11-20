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

package githubapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// defaultPullRequestTimeout bounds outbound GET /pulls/{number} calls when
	// the caller has not already set a context deadline (Stage 3 §5.5, §11 G5).
	defaultPullRequestTimeout = 5 * time.Second

	// pullRequestResponseBodyLimit bounds a single pull request JSON response.
	pullRequestResponseBodyLimit = 1 << 20
)

// Error codes for pull request API failures.
const (
	ErrCodePullRequestNotFound     = "pull_request_not_found"
	ErrCodePullRequestUnauthorized = "pull_request_unauthorized"
	ErrCodePullRequestRefused      = "pull_request_refused"
	ErrCodePullRequestRateLimited  = "pull_request_rate_limited"
	ErrCodePullRequestFailed       = "pull_request_request_failed"
)

// PullRequestAPIError classifies GitHub API errors during pull request fetches.
//
// Message and Error() carry fixed, content-free text only (ptone/scion#7498):
// they never include tokens or text received from GitHub. GitHub's error
// message or truncated body is available only via UpstreamDetail for sanitized
// server-side diagnostic logging.
type PullRequestAPIError struct {
	ErrorCode  string
	StatusCode int
	Message    string
	Err        error

	upstream string
}

func (e *PullRequestAPIError) Error() string {
	return fmt.Sprintf("github app pull request error (%s): %s", e.ErrorCode, e.Message)
}

func (e *PullRequestAPIError) Unwrap() error {
	return e.Err
}

// UpstreamDetail returns the unrendered text received from GitHub or the HTTP
// transport. Callers must sanitize and cap it before server logging and must
// never return it to clients or persist it.
func (e *PullRequestAPIError) UpstreamDetail() string {
	return e.upstream
}

func withDefaultPullRequestTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultPullRequestTimeout)
}

// GetPullRequestJSON fetches the raw JSON object for a pull request
// (`GET /repos/{owner}/{repo}/pulls/{number}`) using an installation access
// token. Errors are content-free and bounded to defaultPullRequestTimeout (5 s)
// unless ctx already carries a deadline.
func (c *Client) GetPullRequestJSON(ctx context.Context, installationToken, owner, repo string, pullNumber int) ([]byte, error) {
	if strings.TrimSpace(installationToken) == "" {
		return nil, &PullRequestAPIError{
			ErrorCode: ErrCodePullRequestFailed,
			Message:   "installation token is required",
		}
	}
	if !isSafePathSegment(owner) || !isSafePathSegment(repo) {
		return nil, &PullRequestAPIError{
			ErrorCode: ErrCodePullRequestFailed,
			Message:   "invalid repository owner or name",
		}
	}
	if pullNumber <= 0 {
		return nil, &PullRequestAPIError{
			ErrorCode: ErrCodePullRequestFailed,
			Message:   "invalid pull request number",
		}
	}

	ctx, cancel := withDefaultPullRequestTimeout(ctx)
	defer cancel()

	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d",
		c.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), pullNumber)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &PullRequestAPIError{
			ErrorCode: ErrCodePullRequestFailed,
			Message:   "failed to build pull request request",
			Err:       err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &PullRequestAPIError{
			ErrorCode: ErrCodePullRequestFailed,
			Message:   "GitHub API request failed",
			Err:       err,
			upstream:  err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyReadLimit))
		return nil, classifyPullRequestError(resp, errBody)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, pullRequestResponseBodyLimit+1))
	if err != nil {
		return nil, &PullRequestAPIError{
			ErrorCode: ErrCodePullRequestFailed,
			Message:   "failed to read pull request response",
			Err:       err,
		}
	}
	if len(body) > pullRequestResponseBodyLimit {
		return nil, &PullRequestAPIError{
			ErrorCode: ErrCodePullRequestFailed,
			Message:   "pull request response exceeded size limit",
		}
	}
	return body, nil
}

func classifyPullRequestError(resp *http.Response, body []byte) *PullRequestAPIError {
	var ghErr githubErrorResponse
	_ = json.Unmarshal(body, &ghErr)

	detail := ghErr.Message
	if detail == "" {
		detail = string(body)
	}
	lower := strings.ToLower(ghErr.Message)

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return &PullRequestAPIError{
			ErrorCode:  ErrCodePullRequestRateLimited,
			StatusCode: resp.StatusCode,
			Message:    "GitHub rate limited the pull request request.",
			Err:        ErrRateLimited,
			upstream:   detail,
		}
	case http.StatusForbidden:
		if isRateLimitedForbidden(resp, lower) {
			return &PullRequestAPIError{
				ErrorCode:  ErrCodePullRequestRateLimited,
				StatusCode: resp.StatusCode,
				Message:    "GitHub rate limited the pull request request.",
				Err:        ErrRateLimited,
				upstream:   detail,
			}
		}
		return &PullRequestAPIError{
			ErrorCode:  ErrCodePullRequestRefused,
			StatusCode: resp.StatusCode,
			Message:    "GitHub refused the pull request request.",
			upstream:   detail,
		}
	case http.StatusUnauthorized:
		return &PullRequestAPIError{
			ErrorCode:  ErrCodePullRequestUnauthorized,
			StatusCode: resp.StatusCode,
			Message:    "GitHub rejected the installation token.",
			upstream:   detail,
		}
	case http.StatusNotFound:
		return &PullRequestAPIError{
			ErrorCode:  ErrCodePullRequestNotFound,
			StatusCode: resp.StatusCode,
			Message:    "GitHub pull request was not found.",
			upstream:   detail,
		}
	default:
		return &PullRequestAPIError{
			ErrorCode:  ErrCodePullRequestFailed,
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("GitHub API error (status %d).", resp.StatusCode),
			upstream:   detail,
		}
	}
}
