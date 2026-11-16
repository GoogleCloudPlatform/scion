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

package github

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// ErrAttributesCeilingExceeded is returned by Normalize when protected
// identifier attributes alone exceed the 8 KiB serialized ceiling after all
// prose fields have been truncated and all unprotected keys have been deleted.
// Callers (such as webhook ingress, §4.1 step 8) can inspect it with errors.Is
// to map unshrinkable deliveries to a refused or ignored event.
var ErrAttributesCeilingExceeded = errors.New("github trigger: normalised attributes exceed ceiling")

// Source and provenance constants (§4.3, §5.8).
const (
	SourceKind      = "github"
	PayloadVerified = "github-hmac-sha256+projected"
)

// Supported GitHub webhook event families in Stage 3 (§5.1; push is G2b).
const (
	EventPullRequest              = "pull_request"
	EventIssues                   = "issues"
	EventIssueComment             = "issue_comment"
	EventPullRequestReview        = "pull_request_review"
	EventPullRequestReviewComment = "pull_request_review_comment"
)

// Stage 3 attribute, list, and projected payload size bounds (§4.3, §5.2–5.8).
const (
	DefaultMaxPayloadBytes    = 64 * 1024
	DefaultMaxAttributesBytes = 8 * 1024
	DefaultMaxListsBytes      = 8 * 1024
	MaxSubjectBytes           = 256

	MaxTitleBytes       = 512
	MaxBodyBytes        = 4 * 1024
	MaxDiffHunkBytes    = 1024
	MaxCommandNameRunes = 32
	MaxCommandArgsBytes = 1024
	MaxCommandLineBytes = 1024

	// StaleThreshold is the §4.4 replay-protection freshness window: when
	// Options.ReceivedAt - Result.OccurredAt exceeds 24h, Normalize sets
	// attributes["stale"] = "true".
	StaleThreshold = 24 * time.Hour
)

// Enrichment attribute key reserved for the §5.5 drainer-side hook (unset by Normalize).
const AttrEnriched = "enriched"

// allowedHeaderNames lists the webhook headers Stage 3 records on trigger_events (§4.3).
var allowedHeaderNames = []string{
	"X-GitHub-Event",
	"X-GitHub-Delivery",
	"X-GitHub-Hook-ID",
	"X-GitHub-Hook-Installation-Target-ID",
	"User-Agent",
}

// Options configures webhook normalisation and command detection.
type Options struct {
	// AppSlug is the GitHub App's slug (from GET /app or admin override),
	// without a leading '@' or trailing "[bot]". When empty, mention/command
	// detection on creation actions is skipped and
	// attributes["mention_detection"] = "unavailable" is recorded (§10.2).
	AppSlug string

	// MentionAliases are optional additional @-handles (for example "scion")
	// recognised alongside AppSlug when parsing commands and mentions (§5.6, §10.2).
	MentionAliases []string

	// ReceivedAt is the fallback timestamp for Result.OccurredAt when the
	// webhook payload's primary object carries no timestamp (§4.3).
	ReceivedAt time.Time

	// Size limits; zero or negative values fall back to the Stage 3 defaults.
	MaxPayloadBytes     int
	MaxBodyBytes        int
	MaxTitleBytes       int
	MaxDiffHunkBytes    int
	MaxCommandArgsBytes int
	MaxCommandLineBytes int
}

func (o Options) maxPayloadBytes() int {
	if o.MaxPayloadBytes > 0 {
		return o.MaxPayloadBytes
	}
	return DefaultMaxPayloadBytes
}

func (o Options) maxBodyBytes() int {
	if o.MaxBodyBytes > 0 {
		return o.MaxBodyBytes
	}
	return MaxBodyBytes
}

func (o Options) maxTitleBytes() int {
	if o.MaxTitleBytes > 0 {
		return o.MaxTitleBytes
	}
	return MaxTitleBytes
}

func (o Options) maxDiffHunkBytes() int {
	if o.MaxDiffHunkBytes > 0 {
		return o.MaxDiffHunkBytes
	}
	return MaxDiffHunkBytes
}

func (o Options) maxCommandArgsBytes() int {
	if o.MaxCommandArgsBytes > 0 {
		return o.MaxCommandArgsBytes
	}
	return MaxCommandArgsBytes
}

func (o Options) maxCommandLineBytes() int {
	if o.MaxCommandLineBytes > 0 {
		return o.MaxCommandLineBytes
	}
	return MaxCommandLineBytes
}

// Result holds the normalised event and ingress routing facts produced by Normalize.
type Result struct {
	// Handled is false when eventName is outside the Stage 3 event family table.
	Handled bool

	// Event identity (§4.3), ready to map into trigger_events / hubfire.FireEvent.
	SourceKind string
	SourceID   string
	DeliveryID string
	Type       string
	Subject    string
	OccurredAt time.Time

	// Normalised string attributes (§5.2–5.6) and list-valued attributes (§4.3, D-S3-8).
	Attributes map[string]string
	Lists      map[string][]string

	// Bounded projected JSON payload (§5.8).
	Payload      []byte
	PayloadBytes int
	Truncated    bool

	// Ingress routing and bot-guard facts (§4.1–4.2, §5.7).
	InstallationID int64
	RepoFullName   string
	SenderLogin    string
	SenderID       int64
	SenderType     string
}

// DedupKey returns the per-project deduplication key "<delivery>/<projectID>" (§4.3).
// It returns "" if either delivery ID or projectID is empty.
func (r Result) DedupKey(projectID string) string {
	return DedupKey(r.DeliveryID, projectID)
}

// IsOwnApp reports whether the webhook sender is the GitHub App's own bot
// identity (<slug>[bot] with sender.type == "Bot", case-insensitive; §4.1 step 6, §5.7).
func (r Result) IsOwnApp(appSlug string) bool {
	return IsOwnAppSender(r.SenderType, r.SenderLogin, appSlug)
}

// DedupKey formats the §4.3 per-project deduplication key "<deliveryID>/<projectID>".
func DedupKey(deliveryID, projectID string) string {
	if deliveryID == "" || projectID == "" {
		return ""
	}
	return deliveryID + "/" + projectID
}

// IsOwnAppSender reports whether (senderType, senderLogin) matches the App's
// own bot login (<appSlug>[bot] with type "Bot", case-insensitive).
func IsOwnAppSender(senderType, senderLogin, appSlug string) bool {
	slug := normalizeHandle(appSlug)
	if slug == "" {
		return false
	}
	return strings.EqualFold(senderType, "Bot") &&
		strings.EqualFold(senderLogin, slug+"[bot]")
}

// IsSupportedEvent reports whether eventName is in the Stage 3 normalisation table (§5.1).
func IsSupportedEvent(eventName string) bool {
	switch eventName {
	case EventPullRequest,
		EventIssues,
		EventIssueComment,
		EventPullRequestReview,
		EventPullRequestReviewComment:
		return true
	default:
		return false
	}
}

// FilterHeaders extracts the §4.3 allow-listed GitHub webhook headers.
func FilterHeaders(h http.Header) map[string]string {
	out := make(map[string]string)
	for _, k := range allowedHeaderNames {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}
