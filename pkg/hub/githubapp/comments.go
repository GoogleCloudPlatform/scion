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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxCommentBodyBytes is GitHub's maximum comment body length in UTF-8 bytes.
	MaxCommentBodyBytes = 65536

	// maxFireIDBytes bounds a trigger fire identifier embedded in a comment marker.
	maxFireIDBytes = 256

	// commentResponseBodyLimit bounds a single created-comment JSON response
	// (64 KiB comment body plus GitHub comment metadata).
	commentResponseBodyLimit = 256 << 10

	// commentListResponseBodyLimit bounds one 100-comment page from GitHub.
	commentListResponseBodyLimit = 8 << 20

	// defaultCommentRequestTimeout bounds outbound comment API calls when the
	// caller has not already set a context deadline.
	defaultCommentRequestTimeout = 5 * time.Second

	fireMarkerPrefix = "<!-- scion fire:"
	fireMarkerSuffix = " -->"
)

var (
	// ErrCommentRefused indicates GitHub refused the comment request due to
	// permissions, repository state, or validation (403 non-rate-limit, 422).
	ErrCommentRefused = errors.New("github comment refused")

	// ErrRateLimited indicates GitHub rate-limited the comment request
	// (429, or 403 primary/secondary rate limit).
	ErrRateLimited = errors.New("github rate limited")
)

// Error codes for comment API failures.
const (
	ErrCodeCommentRefused       = "comment_refused"
	ErrCodeRateLimited          = "rate_limited"
	ErrCodeCommentUnauthorized  = "comment_unauthorized"
	ErrCodeCommentNotFound      = "comment_not_found"
	ErrCodeCommentRequestFailed = "comment_request_failed"
)

// CommentAPIError classifies GitHub API errors during comment operations.
//
// Message and Error() carry fixed, content-free text only (ptone/scion#7498):
// they never include comment bodies, tokens, or text received from GitHub.
// GitHub's error message or truncated error body is available only via
// UpstreamDetail for sanitized server-side diagnostic logging.
type CommentAPIError struct {
	ErrorCode  string
	StatusCode int
	Message    string
	Err        error

	upstream string
}

func (e *CommentAPIError) Error() string {
	return fmt.Sprintf("github app comment error (%s): %s", e.ErrorCode, e.Message)
}

func (e *CommentAPIError) Unwrap() error {
	return e.Err
}

// UpstreamDetail returns the unrendered text received from GitHub or the HTTP
// transport. Callers must sanitize and cap it before server logging and must
// never return it to clients or persist it.
func (e *CommentAPIError) UpstreamDetail() string {
	return e.upstream
}

// CommentUser identifies the GitHub account that authored a comment.
type CommentUser struct {
	Login string `json:"login"`
	Type  string `json:"type,omitempty"`
}

// IssueComment represents a GitHub issue, PR conversation, or PR review comment.
type IssueComment struct {
	ID        int64       `json:"id"`
	Body      string      `json:"body"`
	HTMLURL   string      `json:"html_url"`
	User      CommentUser `json:"user"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

type createCommentRequest struct {
	Body string `json:"body"`
}

// FireMarker builds the invisible HTML marker `<!-- scion fire:<fire_id> -->`
// used to make GitHub comment creation idempotent across lost-lease retries.
func FireMarker(fireID string) (string, error) {
	if fireID == "" {
		return "", fmt.Errorf("github comment marker: fire_id is required")
	}
	if len(fireID) > maxFireIDBytes {
		return "", fmt.Errorf("github comment marker: fire_id exceeds %d bytes", maxFireIDBytes)
	}
	if !utf8.ValidString(fireID) {
		return "", fmt.Errorf("github comment marker: fire_id is not valid UTF-8")
	}
	if strings.Contains(fireID, "--") {
		return "", fmt.Errorf("github comment marker: fire_id must not contain '--'")
	}
	for _, r := range fireID {
		if r <= 0x20 || r == 0x7f || r == '<' || r == '>' {
			return "", fmt.Errorf("github comment marker: fire_id contains disallowed character")
		}
	}
	return fireMarkerPrefix + fireID + fireMarkerSuffix, nil
}

// HasFireMarker reports whether commentBody ends with the exact fire marker for fireID.
func HasFireMarker(commentBody, fireID string) bool {
	marker, err := FireMarker(fireID)
	if err != nil {
		return false
	}
	return strings.HasSuffix(commentBody, marker)
}

// AppendFireMarker appends `<!-- scion fire:<fire_id> -->` to body while
// keeping the total UTF-8 byte length within MaxCommentBodyBytes (65,536).
//
// If body already ends with the exact marker and fits within MaxCommentBodyBytes,
// it is returned unchanged. If body is too long to fit alongside the marker,
// body is truncated on a UTF-8 rune boundary before appending the marker.
func AppendFireMarker(body, fireID string) (string, error) {
	marker, err := FireMarker(fireID)
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(body, 0) || !utf8.ValidString(body) {
		return "", fmt.Errorf("github comment body must be valid UTF-8 without NUL bytes")
	}
	if strings.HasSuffix(body, marker) && len(body) <= MaxCommentBodyBytes {
		return body, nil
	}
	if body == "" {
		return marker, nil
	}

	sep := "\n"
	if strings.HasSuffix(body, "\n") {
		sep = ""
	}
	if len(body)+len(sep)+len(marker) <= MaxCommentBodyBytes {
		return body + sep + marker, nil
	}

	// Truncate body to fit "\n" + marker within MaxCommentBodyBytes.
	maxBody := MaxCommentBodyBytes - 1 - len(marker)
	truncated := truncateUTF8Bytes(body, maxBody)
	if truncated == "" {
		return marker, nil
	}
	if strings.HasSuffix(truncated, "\n") {
		return truncated + marker, nil
	}
	return truncated + "\n" + marker, nil
}

func truncateUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= 0 {
		return ""
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// PostCommentOptions configures an idempotent GitHub comment post for a trigger fire.
type PostCommentOptions struct {
	Owner string
	Repo  string
	// IssueNumber is the issue or pull request number.
	IssueNumber int
	// ReviewCommentID, when positive, posts a reply in the pull request review
	// thread for that review comment instead of a top-level issue/PR comment.
	ReviewCommentID int64
	Body            string
	FireID          string
	// ExpectedAuthorLogin, when non-empty, restricts retry deduplication
	// marker matches to comments authored by the App's bot login (matched
	// case-insensitively on user.login), preventing untrusted commenters from
	// suppressing a retry by quoting or planting a fire marker. When empty
	// (App identity unknown), deduplication falls back to suffix marker
	// matching only.
	ExpectedAuthorLogin string
	// Attempts is the 1-based fire delivery attempt count. When Attempts > 1,
	// existing comments since Since are checked for FireID's marker before posting.
	Attempts int
	Since    time.Time
}

// PostCommentIdempotent appends the fire marker to opts.Body and posts the
// comment to GitHub. When opts.Attempts > 1, it first lists up to 100 comments
// since opts.Since and returns the existing comment without posting if one
// already ends with `<!-- scion fire:<fire_id> -->` and (when
// opts.ExpectedAuthorLogin is non-empty) was authored by that GitHub login.
func (c *Client) PostCommentIdempotent(ctx context.Context, installationToken string, opts PostCommentOptions) (*IssueComment, error) {
	markedBody, err := AppendFireMarker(opts.Body, opts.FireID)
	if err != nil {
		return nil, err
	}

	if opts.Attempts > 1 {
		var existing []IssueComment
		if opts.ReviewCommentID > 0 {
			existing, err = c.ListPullReviewComments(ctx, installationToken, opts.Owner, opts.Repo, opts.IssueNumber, opts.Since)
		} else {
			existing, err = c.ListIssueComments(ctx, installationToken, opts.Owner, opts.Repo, opts.IssueNumber, opts.Since)
		}
		if err != nil {
			return nil, err
		}
		expectedAuthor := strings.TrimSpace(opts.ExpectedAuthorLogin)
		for i := range existing {
			if !HasFireMarker(existing[i].Body, opts.FireID) {
				continue
			}
			if expectedAuthor != "" && !strings.EqualFold(strings.TrimSpace(existing[i].User.Login), expectedAuthor) {
				continue
			}
			found := existing[i]
			return &found, nil
		}
	}

	if opts.ReviewCommentID > 0 {
		return c.ReplyToReviewComment(ctx, installationToken, opts.Owner, opts.Repo, opts.IssueNumber, opts.ReviewCommentID, markedBody)
	}
	return c.CreateIssueComment(ctx, installationToken, opts.Owner, opts.Repo, opts.IssueNumber, markedBody)
}

// CreateIssueComment creates a comment on an issue or pull request conversation
// (`POST /repos/{owner}/{repo}/issues/{number}/comments`).
func (c *Client) CreateIssueComment(ctx context.Context, installationToken, owner, repo string, issueNumber int, body string) (*IssueComment, error) {
	if err := validateRepoTarget(installationToken, owner, repo, issueNumber); err != nil {
		return nil, err
	}
	if err := validateRawCommentBody(body); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments",
		c.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), issueNumber)
	return c.postComment(ctx, installationToken, endpoint, body)
}

// ReplyToReviewComment replies to an existing pull request review comment thread
// (`POST /repos/{owner}/{repo}/pulls/{number}/comments/{comment_id}/replies`).
func (c *Client) ReplyToReviewComment(ctx context.Context, installationToken, owner, repo string, pullNumber int, commentID int64, body string) (*IssueComment, error) {
	if err := validateRepoTarget(installationToken, owner, repo, pullNumber); err != nil {
		return nil, err
	}
	if commentID <= 0 {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "invalid review comment_id",
		}
	}
	if err := validateRawCommentBody(body); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments/%d/replies",
		c.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), pullNumber, commentID)
	return c.postComment(ctx, installationToken, endpoint, body)
}

// ListIssueComments lists up to 100 comments on an issue or pull request
// conversation updated at or after since
// (`GET /repos/{owner}/{repo}/issues/{number}/comments?since=...&per_page=100`).
func (c *Client) ListIssueComments(ctx context.Context, installationToken, owner, repo string, issueNumber int, since time.Time) ([]IssueComment, error) {
	if err := validateRepoTarget(installationToken, owner, repo, issueNumber); err != nil {
		return nil, err
	}
	q := commentListQuery(since)
	endpoint := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments?%s",
		c.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), issueNumber, q)
	return c.listComments(ctx, installationToken, endpoint)
}

// ListPullReviewComments lists up to 100 pull request review comments updated
// at or after since (`GET /repos/{owner}/{repo}/pulls/{number}/comments?since=...&per_page=100`).
func (c *Client) ListPullReviewComments(ctx context.Context, installationToken, owner, repo string, pullNumber int, since time.Time) ([]IssueComment, error) {
	if err := validateRepoTarget(installationToken, owner, repo, pullNumber); err != nil {
		return nil, err
	}
	q := commentListQuery(since)
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/comments?%s",
		c.apiBaseURL, url.PathEscape(owner), url.PathEscape(repo), pullNumber, q)
	return c.listComments(ctx, installationToken, endpoint)
}

func commentListQuery(since time.Time) string {
	v := url.Values{}
	v.Set("per_page", "100")
	if !since.IsZero() {
		v.Set("since", since.UTC().Format(time.RFC3339))
	}
	return v.Encode()
}

func validateRepoTarget(installationToken, owner, repo string, number int) error {
	if strings.TrimSpace(installationToken) == "" {
		return &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "installation token is required",
		}
	}
	if !isSafePathSegment(owner) || !isSafePathSegment(repo) {
		return &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "invalid repository owner or name",
		}
	}
	if number <= 0 {
		return &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "invalid issue or pull request number",
		}
	}
	return nil
}

func isSafePathSegment(seg string) bool {
	if seg == "" || seg == "." || seg == ".." {
		return false
	}
	return !strings.ContainsAny(seg, "/\\\x00")
}

func validateRawCommentBody(body string) error {
	if body == "" {
		return &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "comment body is required",
		}
	}
	if len(body) > MaxCommentBodyBytes {
		return &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "comment body exceeds GitHub's 65536-byte limit",
		}
	}
	if strings.ContainsRune(body, 0) || !utf8.ValidString(body) {
		return &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "comment body must be valid UTF-8 without NUL bytes",
		}
	}
	return nil
}

func withDefaultCommentTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, defaultCommentRequestTimeout)
}

func (c *Client) postComment(ctx context.Context, installationToken, endpoint, body string) (*IssueComment, error) {
	ctx, cancel := withDefaultCommentTimeout(ctx)
	defer cancel()

	payload, err := json.Marshal(createCommentRequest{Body: body})
	if err != nil {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "failed to encode comment request",
			Err:       err,
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "failed to build comment request",
			Err:       err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "GitHub API request failed",
			Err:       err,
			upstream:  err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyReadLimit))
		return nil, classifyCommentError(resp, errBody)
	}

	respBody, err := readBoundedBody(resp.Body, commentResponseBodyLimit)
	if err != nil {
		msg := "failed to read comment response"
		if errors.Is(err, errResponseBodyTooLarge) {
			msg = "GitHub comment response body exceeded size limit"
		}
		return nil, &CommentAPIError{
			ErrorCode:  ErrCodeCommentRequestFailed,
			StatusCode: resp.StatusCode,
			Message:    msg,
			Err:        err,
			upstream:   err.Error(),
		}
	}

	var comment IssueComment
	if err := json.Unmarshal(respBody, &comment); err != nil {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "failed to parse comment response",
			Err:       err,
		}
	}
	return &comment, nil
}

func (c *Client) listComments(ctx context.Context, installationToken, endpoint string) ([]IssueComment, error) {
	ctx, cancel := withDefaultCommentTimeout(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "failed to build comment list request",
			Err:       err,
		}
	}
	req.Header.Set("Authorization", "Bearer "+installationToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "GitHub API request failed",
			Err:       err,
			upstream:  err.Error(),
		}
	}
	defer func() { _ = resp.Body.Close() }()

	c.trackRateLimit(resp)

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamBodyReadLimit))
		return nil, classifyCommentError(resp, errBody)
	}

	respBody, err := readBoundedBody(resp.Body, commentListResponseBodyLimit)
	if err != nil {
		msg := "failed to read comment list response"
		if errors.Is(err, errResponseBodyTooLarge) {
			msg = "GitHub comment list response body exceeded size limit"
		}
		return nil, &CommentAPIError{
			ErrorCode:  ErrCodeCommentRequestFailed,
			StatusCode: resp.StatusCode,
			Message:    msg,
			Err:        err,
			upstream:   err.Error(),
		}
	}

	var comments []IssueComment
	if err := json.Unmarshal(respBody, &comments); err != nil {
		return nil, &CommentAPIError{
			ErrorCode: ErrCodeCommentRequestFailed,
			Message:   "failed to parse comment list response",
			Err:       err,
		}
	}
	return comments, nil
}

func classifyCommentError(resp *http.Response, body []byte) *CommentAPIError {
	var ghErr githubErrorResponse
	_ = json.Unmarshal(body, &ghErr)

	detail := ghErr.Message
	if detail == "" {
		detail = string(body)
	}
	lower := strings.ToLower(ghErr.Message)

	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return &CommentAPIError{
			ErrorCode:  ErrCodeRateLimited,
			StatusCode: resp.StatusCode,
			Message:    "GitHub rate limited the comment request.",
			Err:        ErrRateLimited,
			upstream:   detail,
		}

	case http.StatusForbidden:
		if isRateLimitedForbidden(resp, lower) {
			return &CommentAPIError{
				ErrorCode:  ErrCodeRateLimited,
				StatusCode: resp.StatusCode,
				Message:    "GitHub rate limited the comment request.",
				Err:        ErrRateLimited,
				upstream:   detail,
			}
		}
		return &CommentAPIError{
			ErrorCode:  ErrCodeCommentRefused,
			StatusCode: resp.StatusCode,
			Message:    "GitHub refused the comment request.",
			Err:        ErrCommentRefused,
			upstream:   detail,
		}

	case http.StatusUnprocessableEntity:
		return &CommentAPIError{
			ErrorCode:  ErrCodeCommentRefused,
			StatusCode: resp.StatusCode,
			Message:    "GitHub rejected the comment request.",
			Err:        ErrCommentRefused,
			upstream:   detail,
		}

	case http.StatusUnauthorized:
		return &CommentAPIError{
			ErrorCode:  ErrCodeCommentUnauthorized,
			StatusCode: resp.StatusCode,
			Message:    "GitHub rejected the installation token.",
			upstream:   detail,
		}

	case http.StatusNotFound:
		return &CommentAPIError{
			ErrorCode:  ErrCodeCommentNotFound,
			StatusCode: resp.StatusCode,
			Message:    "GitHub comment target was not found.",
			upstream:   detail,
		}

	default:
		return &CommentAPIError{
			ErrorCode:  ErrCodeCommentRequestFailed,
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("GitHub API error (status %d).", resp.StatusCode),
			upstream:   detail,
		}
	}
}

func isRateLimitedForbidden(resp *http.Response, lowerMessage string) bool {
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	if resp.Header.Get("Retry-After") != "" {
		return true
	}
	return strings.Contains(lowerMessage, "secondary rate limit") ||
		strings.Contains(lowerMessage, "abuse detection")
}
