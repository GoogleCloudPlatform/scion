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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxActionBytes       = 64
	maxDeliveryIDBytes   = 128
	maxLoginBytes        = 128
	maxRepoFullNameBytes = 200
)

// Normalize converts a verified GitHub App webhook delivery into a normalised
// Stage 3 trigger Result (§4.3, §4.4, §5.1–§5.8, §10.2).
//
// Event families outside the Stage 3 table return Result{Handled: false}, nil.
// Malformed JSON or missing/invalid required fields return a descriptive error.
// If protected attributes alone exceed the 8 KiB serialized ceiling after prose
// truncation and unprotected-key removal, Normalize returns an error wrapping
// ErrAttributesCeilingExceeded.
func Normalize(eventName, deliveryID string, payload []byte, opts Options) (Result, error) {
	if !IsSupportedEvent(eventName) {
		return Result{Handled: false}, nil
	}
	if deliveryID != "" {
		if len(deliveryID) > maxDeliveryIDBytes || !utf8.ValidString(deliveryID) || !allPrintable(deliveryID) {
			return Result{}, errors.New("github trigger: invalid delivery ID")
		}
	}

	root, err := decodeJSONObject(payload)
	if err != nil {
		return Result{}, err
	}

	var action string
	if eventName != EventPush {
		actionRaw, err := reqString(root, "action", "action")
		if err != nil {
			return Result{}, err
		}
		if !isActionIdent(actionRaw) {
			return Result{}, errors.New("github trigger: invalid action identifier")
		}
		action = strings.ToLower(actionRaw)
	}

	instMap, err := reqMap(root, "installation", "installation")
	if err != nil {
		return Result{}, err
	}
	installationID, err := reqPositiveInt64(instMap, "id", "installation.id")
	if err != nil {
		return Result{}, err
	}

	repoMap, err := reqMap(root, "repository", "repository")
	if err != nil {
		return Result{}, err
	}
	repoFullName, err := reqString(repoMap, "full_name", "repository.full_name")
	if err != nil {
		return Result{}, err
	}
	splitOwner, splitRepo, ok := parseOwnerRepo(repoFullName)
	if !ok {
		return Result{}, errors.New("github trigger: invalid repository.full_name")
	}
	repoOwner := splitOwner
	if ownerMap, ok := optMap(repoMap, "owner"); ok {
		if login := optCleanString(ownerMap, "login"); login != "" {
			repoOwner = login
		}
	}
	repoName := splitRepo
	if name := optCleanString(repoMap, "name"); name != "" {
		repoName = name
	}
	repoPrivate, err := optBool(repoMap, "private", "repository.private")
	if err != nil {
		return Result{}, err
	}
	defaultBranch := optCleanString(repoMap, "default_branch")

	senderMap, err := reqMap(root, "sender", "sender")
	if err != nil {
		return Result{}, err
	}
	senderLogin, err := reqString(senderMap, "login", "sender.login")
	if err != nil {
		return Result{}, err
	}
	if len(senderLogin) > maxLoginBytes || !utf8.ValidString(senderLogin) || !allPrintableNonSpace(senderLogin) {
		return Result{}, errors.New("github trigger: invalid sender.login")
	}
	senderID, err := optNonNegativeInt64(senderMap, "id", "sender.id")
	if err != nil {
		return Result{}, err
	}
	senderType, err := optString(senderMap, "type", "sender.type")
	if err != nil {
		return Result{}, err
	}

	performedViaAppID, err := extractEventPerformedViaAppID(eventName, action, root)
	if err != nil {
		return Result{}, err
	}

	base := Result{
		Action:            action,
		InstallationID:    installationID,
		RepoFullName:      repoFullName,
		SenderLogin:       senderLogin,
		SenderID:          senderID,
		SenderType:        senderType,
		PerformedViaAppID: performedViaAppID,
	}

	attrs := map[string]string{
		"provider":        SourceKind,
		"repo":            repoFullName,
		"repo_owner":      repoOwner,
		"repo_name":       repoName,
		"repo_private":    strconv.FormatBool(repoPrivate),
		"installation_id": strconv.FormatInt(installationID, 10),
		"event":           eventName,
		"sender":          senderLogin,
	}
	if action != "" {
		attrs["action"] = action
	}
	if defaultBranch != "" {
		attrs["default_branch"] = defaultBranch
	}
	if deliveryID != "" {
		attrs["delivery"] = deliveryID
	}
	if senderID > 0 {
		attrs["sender_id"] = strconv.FormatInt(senderID, 10)
	}
	if senderType != "" {
		attrs["sender_type"] = senderType
	}

	lists := make(map[string][]string)

	var (
		number               int64
		pushRef              string
		pushCommitsTruncated bool
		occurredAt           time.Time
		rawCommandBody       string
	)

	switch eventName {
	case EventPullRequest:
		number, occurredAt, rawCommandBody, err = normalizePullRequest(root, action, repoFullName, opts, attrs, lists)
	case EventIssues:
		number, occurredAt, rawCommandBody, err = normalizeIssues(root, action, opts, attrs, lists)
	case EventIssueComment:
		number, occurredAt, rawCommandBody, err = normalizeIssueComment(root, action, opts, attrs, lists)
	case EventPullRequestReview:
		number, occurredAt, rawCommandBody, err = normalizePullRequestReview(root, action, repoFullName, opts, attrs, lists)
	case EventPullRequestReviewComment:
		number, occurredAt, rawCommandBody, err = normalizePullRequestReviewComment(root, action, repoFullName, opts, attrs, lists)
	case EventPush:
		pushRef, occurredAt, pushCommitsTruncated, err = normalizePush(root, repoMap, defaultBranch, opts, attrs, lists)
	}
	if err != nil {
		return base, err
	}

	var subject string
	if eventName == EventPush {
		subject = formatPushSubject(repoFullName, pushRef)
	} else {
		subject = fmt.Sprintf("github:%s#%d", repoFullName, number)
	}
	if len(subject) > MaxSubjectBytes || !allPrintable(subject) {
		return base, errors.New("github trigger: derived subject exceeds bounds or contains non-printable characters")
	}

	if occurredAt.IsZero() && !opts.ReceivedAt.IsZero() {
		occurredAt = opts.ReceivedAt.UTC()
	}
	if !opts.ReceivedAt.IsZero() && !occurredAt.IsZero() {
		if opts.ReceivedAt.UTC().Sub(occurredAt) > StaleThreshold {
			attrs["stale"] = "true"
		}
	}

	commandEligible := isCommandCreationAction(eventName, action)
	maxCmdArgs := opts.maxCommandArgsBytes()
	maxCmdLine := opts.maxCommandLineBytes()
	applyCommandAttributes(attrs, commandEligible, rawCommandBody, opts.AppSlug, opts.MentionAliases, maxCmdArgs, maxCmdLine)

	projected, truncated, err := projectDecodedPayload(root, opts.maxPayloadBytes())
	if err != nil {
		return base, err
	}
	if truncated {
		attrs["payload_truncated"] = "true"
	}

	listsTruncated, err := enforceListsCeiling(lists, DefaultMaxListsBytes)
	if err != nil {
		return base, err
	}
	if pushCommitsTruncated || listsTruncated {
		attrs["lists_truncated"] = "true"
	}
	if err := enforceAttributesCeiling(attrs, DefaultMaxAttributesBytes); err != nil {
		return base, err
	}

	eventType := SourceKind + "." + eventName
	if action != "" {
		eventType += "." + action
	}

	return Result{
		Handled:           true,
		SourceKind:        SourceKind,
		SourceID:          strconv.FormatInt(installationID, 10),
		DeliveryID:        deliveryID,
		Type:              eventType,
		Subject:           subject,
		OccurredAt:        occurredAt,
		Attributes:        attrs,
		Lists:             lists,
		Payload:           projected,
		PayloadBytes:      len(projected),
		Truncated:         truncated,
		Action:            action,
		InstallationID:    installationID,
		RepoFullName:      repoFullName,
		SenderLogin:       senderLogin,
		SenderID:          senderID,
		SenderType:        senderType,
		PerformedViaAppID: performedViaAppID,
		commandEligible:   commandEligible,
		rawCommandBody:    rawCommandBody,
		maxCommandArgs:    maxCmdArgs,
		maxCommandLine:    maxCmdLine,
	}, nil
}

// ApplyAppSlug updates command/mention attributes on a normalised Result when
// the App slug was lazily discovered after Normalize ran with an empty AppSlug,
// and re-enforces the attributes byte ceiling without re-decoding the payload.
func (r *Result) ApplyAppSlug(appSlug string, mentionAliases []string) error {
	if r == nil || !r.Handled || !r.commandEligible {
		return nil
	}
	delete(r.Attributes, "mention_detection")
	delete(r.Attributes, "mention")
	delete(r.Attributes, "command")
	delete(r.Attributes, "command_args")
	delete(r.Attributes, "command_line")
	applyCommandAttributes(r.Attributes, true, r.rawCommandBody, appSlug, mentionAliases, r.maxCommandArgs, r.maxCommandLine)
	return enforceAttributesCeiling(r.Attributes, DefaultMaxAttributesBytes)
}

func applyCommandAttributes(
	attrs map[string]string,
	commandEligible bool,
	rawCommandBody, appSlug string,
	mentionAliases []string,
	maxArgsBytes, maxLineBytes int,
) {
	if !commandEligible {
		return
	}
	if normalizeHandle(appSlug) == "" {
		attrs["mention_detection"] = "unavailable"
		return
	}
	match := ParseCommand(
		rawCommandBody,
		appSlug,
		mentionAliases,
		maxArgsBytes,
		maxLineBytes,
	)
	if match.Mentioned {
		attrs["mention"] = "true"
	}
	if match.HasCommand {
		attrs["command"] = match.Command
		attrs["command_args"] = match.CommandArgs
		attrs["command_line"] = match.CommandLine
	}
}

// extractEventPerformedViaAppID extracts performed_via_github_app.id ONLY for
// deliveries where this event's actor created that object in this event (§5.7):
//   - issue_comment.created and pull_request_review_comment.created -> comment
//   - pull_request_review.submitted -> review
//   - issues.opened -> issue
//   - pull_request.opened -> pull_request
//
// On all other actions (for example labeling, closing, assigning, or editing an
// App-created issue or PR), object.performed_via_github_app records the object's
// original creator rather than this event's actor and MUST NOT be consulted.
func extractEventPerformedViaAppID(eventName, action string, root map[string]any) (int64, error) {
	primaryKey := ""
	switch eventName {
	case EventIssueComment, EventPullRequestReviewComment:
		if action == "created" {
			primaryKey = "comment"
		}
	case EventPullRequestReview:
		if action == "submitted" {
			primaryKey = "review"
		}
	case EventIssues:
		if action == "opened" {
			primaryKey = "issue"
		}
	case EventPullRequest:
		if action == "opened" {
			primaryKey = "pull_request"
		}
	}
	if primaryKey == "" {
		return 0, nil
	}
	primaryMap, ok := optMap(root, primaryKey)
	if !ok {
		return 0, nil
	}
	return optPerformedViaAppID(primaryMap, primaryKey+".performed_via_github_app")
}

func optPerformedViaAppID(parent map[string]any, label string) (int64, error) {
	raw, exists := parent["performed_via_github_app"]
	if !exists || raw == nil {
		return 0, nil
	}
	appMap, ok := raw.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("github trigger: %s must be a JSON object", label)
	}
	return optNonNegativeInt64(appMap, "id", label+".id")
}

func normalizePullRequest(
	root map[string]any,
	action, repoFullName string,
	opts Options,
	attrs map[string]string,
	lists map[string][]string,
) (int64, time.Time, string, error) {
	prMap, err := reqMap(root, "pull_request", "pull_request")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	number, err := extractPRNumber(root, prMap)
	if err != nil {
		return 0, time.Time{}, "", err
	}
	if err := populatePRCommon(prMap, number, repoFullName, opts, attrs, lists); err != nil {
		return 0, time.Time{}, "", err
	}

	attrs["author_association"] = authorAssociationOrNone(prMap)
	if htmlURL := optCleanString(prMap, "html_url"); htmlURL != "" {
		attrs["html_url"] = htmlURL
	}

	rawBody := optCleanString(prMap, "body")
	if action == "opened" || action == "edited" {
		attrs["body"] = truncateUTF8(rawBody, opts.maxBodyBytes())
	}

	createdAt, createdStr, err := optTimestamp(prMap, "created_at", "pull_request.created_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	updatedAt, updatedStr, err := optTimestamp(prMap, "updated_at", "pull_request.updated_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	if createdStr != "" {
		attrs["created_at"] = createdStr
	}
	if updatedStr != "" {
		attrs["updated_at"] = updatedStr
	}

	for _, metricKey := range []string{"changed_files", "additions", "deletions", "commits"} {
		if val, ok, err := optInt64(prMap, metricKey, "pull_request."+metricKey); err != nil {
			return 0, time.Time{}, "", err
		} else if ok && val >= 0 {
			attrs[metricKey] = strconv.FormatInt(val, 10)
		}
	}

	populateActionDeltas(root, action, attrs)
	if msMap, ok := optMap(prMap, "milestone"); ok {
		if title := optCleanString(msMap, "title"); title != "" {
			attrs["milestone"] = title
		}
	}

	occurredAt := firstNonZeroTime(updatedAt, createdAt)
	return number, occurredAt, rawBody, nil
}

func normalizeIssues(
	root map[string]any,
	action string,
	opts Options,
	attrs map[string]string,
	lists map[string][]string,
) (int64, time.Time, string, error) {
	issueMap, err := reqMap(root, "issue", "issue")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	number, err := reqPositiveInt64(issueMap, "number", "issue.number")
	if err != nil {
		return 0, time.Time{}, "", err
	}

	attrs["number"] = strconv.FormatInt(number, 10)
	attrs["is_pull_request"] = "false"
	attrs["title"] = truncateUTF8(optCleanString(issueMap, "title"), opts.maxTitleBytes())
	attrs["author_association"] = authorAssociationOrNone(issueMap)
	if htmlURL := optCleanString(issueMap, "html_url"); htmlURL != "" {
		attrs["html_url"] = htmlURL
	}
	if userMap, ok := optMap(issueMap, "user"); ok {
		if login := optCleanString(userMap, "login"); login != "" {
			attrs["author"] = login
		}
	}
	if state := optCleanString(issueMap, "state"); state != "" {
		attrs["state"] = state
	}

	rawBody := optCleanString(issueMap, "body")
	if action == "opened" || action == "edited" {
		attrs["body"] = truncateUTF8(rawBody, opts.maxBodyBytes())
	}

	createdAt, createdStr, err := optTimestamp(issueMap, "created_at", "issue.created_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	updatedAt, updatedStr, err := optTimestamp(issueMap, "updated_at", "issue.updated_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	if createdStr != "" {
		attrs["created_at"] = createdStr
	}
	if updatedStr != "" {
		attrs["updated_at"] = updatedStr
	}

	populateActionDeltas(root, action, attrs)
	if msMap, ok := optMap(issueMap, "milestone"); ok {
		if title := optCleanString(msMap, "title"); title != "" {
			attrs["milestone"] = title
		}
	}

	lists["labels"] = extractObjectStrings(issueMap, "labels", "name")
	lists["assignees"] = extractObjectStrings(issueMap, "assignees", "login")

	occurredAt := firstNonZeroTime(updatedAt, createdAt)
	return number, occurredAt, rawBody, nil
}

func normalizeIssueComment(
	root map[string]any,
	action string,
	opts Options,
	attrs map[string]string,
	lists map[string][]string,
) (int64, time.Time, string, error) {
	issueMap, err := reqMap(root, "issue", "issue")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	number, err := reqPositiveInt64(issueMap, "number", "issue.number")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	commentMap, err := reqMap(root, "comment", "comment")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	commentID, err := reqPositiveInt64(commentMap, "id", "comment.id")
	if err != nil {
		return 0, time.Time{}, "", err
	}

	isPR := issueMap["pull_request"] != nil
	title := truncateUTF8(optCleanString(issueMap, "title"), opts.maxTitleBytes())
	state := optCleanString(issueMap, "state")

	attrs["number"] = strconv.FormatInt(number, 10)
	attrs["is_pull_request"] = strconv.FormatBool(isPR)
	attrs["title"] = title
	attrs["issue_title"] = title
	attrs["author_association"] = authorAssociationOrNone(commentMap)
	attrs["comment_id"] = strconv.FormatInt(commentID, 10)

	if commentURL := optCleanString(commentMap, "html_url"); commentURL != "" {
		attrs["html_url"] = commentURL
		attrs["comment_url"] = commentURL
	}
	if issueURL := optCleanString(issueMap, "html_url"); issueURL != "" {
		attrs["issue_url"] = issueURL
	}
	if prSubMap, ok := optMap(issueMap, "pull_request"); ok {
		if prURL := optCleanString(prSubMap, "html_url"); prURL != "" {
			attrs["pull_request_url"] = prURL
		}
	}
	if userMap, ok := optMap(issueMap, "user"); ok {
		if login := optCleanString(userMap, "login"); login != "" {
			attrs["author"] = login
		}
	}
	if state != "" {
		attrs["state"] = state
		attrs["issue_state"] = state
	}
	if msMap, ok := optMap(issueMap, "milestone"); ok {
		if msTitle := optCleanString(msMap, "title"); msTitle != "" {
			attrs["milestone"] = msTitle
		}
	}

	rawBody := optCleanString(commentMap, "body")
	attrs["body"] = truncateUTF8(rawBody, opts.maxBodyBytes())

	createdAt, createdStr, err := optTimestamp(commentMap, "created_at", "comment.created_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	updatedAt, updatedStr, err := optTimestamp(commentMap, "updated_at", "comment.updated_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	if createdStr != "" {
		attrs["created_at"] = createdStr
	}
	if updatedStr != "" {
		attrs["updated_at"] = updatedStr
	}

	lists["labels"] = extractObjectStrings(issueMap, "labels", "name")
	lists["assignees"] = extractObjectStrings(issueMap, "assignees", "login")

	var occurredAt time.Time
	if action == "created" {
		occurredAt = firstNonZeroTime(createdAt, updatedAt)
	} else {
		occurredAt = firstNonZeroTime(updatedAt, createdAt)
	}
	return number, occurredAt, rawBody, nil
}

func normalizePullRequestReview(
	root map[string]any,
	action, repoFullName string,
	opts Options,
	attrs map[string]string,
	lists map[string][]string,
) (int64, time.Time, string, error) {
	prMap, err := reqMap(root, "pull_request", "pull_request")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	number, err := extractPRNumber(root, prMap)
	if err != nil {
		return 0, time.Time{}, "", err
	}
	reviewMap, err := reqMap(root, "review", "review")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	reviewID, err := reqPositiveInt64(reviewMap, "id", "review.id")
	if err != nil {
		return 0, time.Time{}, "", err
	}

	if err := populatePRCommon(prMap, number, repoFullName, opts, attrs, lists); err != nil {
		return 0, time.Time{}, "", err
	}

	attrs["author_association"] = authorAssociationOrNone(reviewMap)
	attrs["review_id"] = strconv.FormatInt(reviewID, 10)
	if revState := optCleanString(reviewMap, "state"); revState != "" {
		attrs["review_state"] = strings.ToLower(revState)
	}
	if revURL := optCleanString(reviewMap, "html_url"); revURL != "" {
		attrs["html_url"] = revURL
	}
	if prURL := optCleanString(prMap, "html_url"); prURL != "" {
		attrs["pull_request_url"] = prURL
	}

	rawBody := optCleanString(reviewMap, "body")
	attrs["body"] = truncateUTF8(rawBody, opts.maxBodyBytes())

	submittedAt, submittedStr, err := optTimestamp(reviewMap, "submitted_at", "review.submitted_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	updatedAt, updatedStr, err := optTimestamp(reviewMap, "updated_at", "review.updated_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	prUpdatedAt, _, err := optTimestamp(prMap, "updated_at", "pull_request.updated_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	if submittedStr != "" {
		attrs["created_at"] = submittedStr
		attrs["updated_at"] = submittedStr
	}
	if updatedStr != "" {
		attrs["updated_at"] = updatedStr
	}

	var occurredAt time.Time
	if action == "submitted" {
		occurredAt = firstNonZeroTime(submittedAt, updatedAt, prUpdatedAt)
	} else {
		occurredAt = firstNonZeroTime(updatedAt, submittedAt, prUpdatedAt)
	}
	return number, occurredAt, rawBody, nil
}

func normalizePullRequestReviewComment(
	root map[string]any,
	action, repoFullName string,
	opts Options,
	attrs map[string]string,
	lists map[string][]string,
) (int64, time.Time, string, error) {
	prMap, err := reqMap(root, "pull_request", "pull_request")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	number, err := extractPRNumber(root, prMap)
	if err != nil {
		return 0, time.Time{}, "", err
	}
	commentMap, err := reqMap(root, "comment", "comment")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	commentID, err := reqPositiveInt64(commentMap, "id", "comment.id")
	if err != nil {
		return 0, time.Time{}, "", err
	}

	if err := populatePRCommon(prMap, number, repoFullName, opts, attrs, lists); err != nil {
		return 0, time.Time{}, "", err
	}

	attrs["author_association"] = authorAssociationOrNone(commentMap)
	attrs["comment_id"] = strconv.FormatInt(commentID, 10)
	if commentURL := optCleanString(commentMap, "html_url"); commentURL != "" {
		attrs["html_url"] = commentURL
		attrs["comment_url"] = commentURL
	}
	if prURL := optCleanString(prMap, "html_url"); prURL != "" {
		attrs["pull_request_url"] = prURL
	}

	if replyTo, ok, err := optInt64(commentMap, "in_reply_to_id", "comment.in_reply_to_id"); err != nil {
		return 0, time.Time{}, "", err
	} else if ok && replyTo > 0 {
		attrs["in_reply_to"] = strconv.FormatInt(replyTo, 10)
	}
	if path := optCleanString(commentMap, "path"); path != "" {
		attrs["path"] = path
	}
	line, hasLine, err := optInt64(commentMap, "line", "comment.line")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	if !hasLine || line <= 0 {
		line, hasLine, err = optInt64(commentMap, "original_line", "comment.original_line")
		if err != nil {
			return 0, time.Time{}, "", err
		}
	}
	if hasLine && line > 0 {
		attrs["line"] = strconv.FormatInt(line, 10)
	}
	if side := optCleanString(commentMap, "side"); side != "" {
		attrs["side"] = side
	}
	if hunk := optCleanString(commentMap, "diff_hunk"); hunk != "" {
		attrs["diff_hunk"] = truncateUTF8Tail(hunk, opts.maxDiffHunkBytes())
	}

	rawBody := optCleanString(commentMap, "body")
	attrs["body"] = truncateUTF8(rawBody, opts.maxBodyBytes())

	createdAt, createdStr, err := optTimestamp(commentMap, "created_at", "comment.created_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	updatedAt, updatedStr, err := optTimestamp(commentMap, "updated_at", "comment.updated_at")
	if err != nil {
		return 0, time.Time{}, "", err
	}
	if createdStr != "" {
		attrs["created_at"] = createdStr
	}
	if updatedStr != "" {
		attrs["updated_at"] = updatedStr
	}

	var occurredAt time.Time
	if action == "created" {
		occurredAt = firstNonZeroTime(createdAt, updatedAt)
	} else {
		occurredAt = firstNonZeroTime(updatedAt, createdAt)
	}
	return number, occurredAt, rawBody, nil
}

func normalizePush(
	root map[string]any,
	repoMap map[string]any,
	defaultBranch string,
	opts Options,
	attrs map[string]string,
	lists map[string][]string,
) (string, time.Time, bool, error) {
	ref, err := reqString(root, "ref", "ref")
	if err != nil {
		return "", time.Time{}, false, err
	}
	if !allPrintableNonSpace(ref) {
		return "", time.Time{}, false, errors.New("github trigger: invalid ref")
	}
	before, err := reqString(root, "before", "before")
	if err != nil {
		return "", time.Time{}, false, err
	}
	if !isGitSHAHex(before) {
		return "", time.Time{}, false, errors.New("github trigger: before must be a 40- or 64-character lowercase hex SHA")
	}
	after, err := reqString(root, "after", "after")
	if err != nil {
		return "", time.Time{}, false, err
	}
	if !isGitSHAHex(after) {
		return "", time.Time{}, false, errors.New("github trigger: after must be a 40- or 64-character lowercase hex SHA")
	}
	forced, err := optBool(root, "forced", "forced")
	if err != nil {
		return "", time.Time{}, false, err
	}
	created, err := optBool(root, "created", "created")
	if err != nil {
		return "", time.Time{}, false, err
	}
	deleted, err := optBool(root, "deleted", "deleted")
	if err != nil {
		return "", time.Time{}, false, err
	}

	isDefaultBranch := defaultBranch != "" && ref == "refs/heads/"+defaultBranch
	attrs["ref"] = cleanUTF8(ref)
	attrs["before"] = before
	attrs["after"] = after
	attrs["is_default_branch"] = strconv.FormatBool(isDefaultBranch)
	attrs["forced"] = strconv.FormatBool(forced)
	attrs["created"] = strconv.FormatBool(created)
	attrs["deleted"] = strconv.FormatBool(deleted)

	if rawPusher, exists := root["pusher"]; exists && rawPusher != nil {
		pusherMap, ok := rawPusher.(map[string]any)
		if !ok {
			return "", time.Time{}, false, errors.New("github trigger: pusher must be a JSON object")
		}
		if name := optCleanString(pusherMap, "name"); name != "" {
			attrs["pusher"] = name
		}
	}

	commitSHAs := []string{}
	commitCount := 0
	commitsTruncated := false
	if rawCommits, exists := root["commits"]; exists && rawCommits != nil {
		arr, ok := rawCommits.([]any)
		if !ok {
			return "", time.Time{}, false, errors.New("github trigger: commits must be a JSON array")
		}
		commitCount = len(arr)
		if commitCount > MaxPushCommits {
			commitsTruncated = true
		}
		limit := commitCount
		if limit > MaxPushCommits {
			limit = MaxPushCommits
		}
		commitSHAs = make([]string, 0, limit)
		for i := 0; i < limit; i++ {
			if m, ok := arr[i].(map[string]any); ok {
				if sha := optCleanString(m, "id"); sha != "" {
					commitSHAs = append(commitSHAs, sha)
				}
			}
		}
	}
	attrs["commit_count"] = strconv.Itoa(commitCount)
	lists["commits"] = commitSHAs

	pushedAt, err := optUnixOrRFC3339Timestamp(repoMap, "pushed_at", "repository.pushed_at")
	if err != nil {
		return "", time.Time{}, false, err
	}

	var headCommitTS time.Time
	if rawHead, exists := root["head_commit"]; exists && rawHead != nil {
		headCommitMap, ok := rawHead.(map[string]any)
		if !ok {
			return "", time.Time{}, false, errors.New("github trigger: head_commit must be a JSON object")
		}
		if msg := optCleanString(headCommitMap, "message"); msg != "" {
			attrs["head_message"] = truncateUTF8(msg, opts.maxHeadMessageBytes())
		}
		ts, _, err := optTimestamp(headCommitMap, "timestamp", "head_commit.timestamp")
		if err != nil {
			return "", time.Time{}, false, err
		}
		headCommitTS = ts
	}

	occurredAt := firstNonZeroTime(pushedAt, headCommitTS)
	return ref, occurredAt, commitsTruncated, nil
}

// formatPushSubject formats github:<owner>/<repo>@<ref> bounded to MaxSubjectBytes.
// When the full subject exceeds 256 bytes, it deterministically truncates the ref
// on a UTF-8 rune boundary and appends "~<first 12 hex of sha256(ref)>" (§4.3).
func formatPushSubject(repoFullName, ref string) string {
	full := "github:" + repoFullName + "@" + ref
	if len(full) <= MaxSubjectBytes {
		return full
	}
	sum := sha256.Sum256([]byte(ref))
	suffix := "~" + hex.EncodeToString(sum[:])[:12]
	prefixBase := "github:" + repoFullName + "@"
	maxRefPrefix := MaxSubjectBytes - len(prefixBase) - len(suffix)
	if maxRefPrefix < 0 {
		maxRefPrefix = 0
	}
	return prefixBase + truncateUTF8(ref, maxRefPrefix) + suffix
}

func isGitSHAHex(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func optUnixOrRFC3339Timestamp(m map[string]any, key, label string) (time.Time, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return time.Time{}, nil
	}
	switch x := v.(type) {
	case json.Number:
		sec, err := x.Int64()
		if err != nil || sec <= 0 {
			return time.Time{}, fmt.Errorf("github trigger: %s must be a positive Unix epoch integer", label)
		}
		return time.Unix(sec, 0).UTC(), nil
	case string:
		if x == "" {
			return time.Time{}, nil
		}
		t, err := time.Parse(time.RFC3339Nano, x)
		if err != nil {
			return time.Time{}, fmt.Errorf("github trigger: invalid RFC3339 timestamp in %s: %w", label, err)
		}
		return t.UTC(), nil
	default:
		return time.Time{}, fmt.Errorf("github trigger: %s must be a Unix epoch integer or RFC3339 string", label)
	}
}

func populatePRCommon(
	prMap map[string]any,
	number int64,
	repoFullName string,
	opts Options,
	attrs map[string]string,
	lists map[string][]string,
) error {
	baseMap, err := reqMap(prMap, "base", "pull_request.base")
	if err != nil {
		return err
	}
	headMap, err := reqMap(prMap, "head", "pull_request.head")
	if err != nil {
		return err
	}
	baseRef, err := reqString(baseMap, "ref", "pull_request.base.ref")
	if err != nil {
		return err
	}
	baseSHA, err := reqString(baseMap, "sha", "pull_request.base.sha")
	if err != nil {
		return err
	}
	headRef, err := reqString(headMap, "ref", "pull_request.head.ref")
	if err != nil {
		return err
	}
	headSHA, err := reqString(headMap, "sha", "pull_request.head.sha")
	if err != nil {
		return err
	}

	var headRepo string
	if headRepoMap, ok := optMap(headMap, "repo"); ok {
		headRepo = optCleanString(headRepoMap, "full_name")
	}
	isFork := headRepo == "" || !strings.EqualFold(headRepo, repoFullName)

	draft, err := optBool(prMap, "draft", "pull_request.draft")
	if err != nil {
		return err
	}
	merged, err := extractPRMerged(prMap)
	if err != nil {
		return err
	}

	attrs["number"] = strconv.FormatInt(number, 10)
	attrs["is_pull_request"] = "true"
	attrs["title"] = truncateUTF8(optCleanString(prMap, "title"), opts.maxTitleBytes())
	if userMap, ok := optMap(prMap, "user"); ok {
		if login := optCleanString(userMap, "login"); login != "" {
			attrs["author"] = login
		}
	}
	if state := optCleanString(prMap, "state"); state != "" {
		attrs["state"] = state
	}
	attrs["base_ref"] = cleanUTF8(baseRef)
	attrs["base_sha"] = cleanUTF8(baseSHA)
	attrs["head_ref"] = cleanUTF8(headRef)
	attrs["head_sha"] = cleanUTF8(headSHA)
	attrs["head_repo"] = headRepo
	attrs["is_fork"] = strconv.FormatBool(isFork)
	attrs["draft"] = strconv.FormatBool(draft)
	attrs["merged"] = strconv.FormatBool(merged)
	if mergeableState := optCleanString(prMap, "mergeable_state"); mergeableState != "" {
		attrs["mergeable_state"] = mergeableState
	}

	lists["labels"] = extractObjectStrings(prMap, "labels", "name")
	lists["assignees"] = extractObjectStrings(prMap, "assignees", "login")
	lists["requested_reviewers"] = extractObjectStrings(prMap, "requested_reviewers", "login")
	lists["requested_teams"] = extractObjectStrings(prMap, "requested_teams", "slug")
	return nil
}

func populateActionDeltas(root map[string]any, action string, attrs map[string]string) {
	switch action {
	case "labeled", "unlabeled":
		if labelMap, ok := optMap(root, "label"); ok {
			if name := optCleanString(labelMap, "name"); name != "" {
				attrs["label"] = name
			}
		}
	case "assigned", "unassigned":
		if assigneeMap, ok := optMap(root, "assignee"); ok {
			if login := optCleanString(assigneeMap, "login"); login != "" {
				attrs["assignee"] = login
			}
		}
	case "review_requested", "review_request_removed":
		if revMap, ok := optMap(root, "requested_reviewer"); ok {
			if login := optCleanString(revMap, "login"); login != "" {
				attrs["requested_reviewer"] = login
			}
		}
		if teamMap, ok := optMap(root, "requested_team"); ok {
			if slug := optCleanString(teamMap, "slug"); slug != "" {
				attrs["requested_team"] = slug
			}
		}
	}
}

func extractPRNumber(root, prMap map[string]any) (int64, error) {
	if n, ok, err := optInt64(prMap, "number", "pull_request.number"); err != nil {
		return 0, err
	} else if ok && n > 0 {
		return n, nil
	}
	return reqPositiveInt64(root, "number", "number")
}

func extractPRMerged(prMap map[string]any) (bool, error) {
	if raw, exists := prMap["merged"]; exists && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return false, errors.New("github trigger: pull_request.merged must be a boolean")
		}
		return b, nil
	}
	mergedAt := optCleanString(prMap, "merged_at")
	return mergedAt != "", nil
}

func authorAssociationOrNone(obj map[string]any) string {
	if assoc := optCleanString(obj, "author_association"); assoc != "" {
		return assoc
	}
	return "NONE"
}

func extractObjectStrings(parent map[string]any, arrayKey, fieldKey string) []string {
	raw, exists := parent[arrayKey]
	if !exists || raw == nil {
		return []string{}
	}
	arr, ok := raw.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(arr))
	for _, elem := range arr {
		if m, ok := elem.(map[string]any); ok {
			if val := optCleanString(m, fieldKey); val != "" {
				out = append(out, val)
			}
		}
	}
	return out
}

// enforceAttributesCeiling bounds serialized JSON of attrs to maxBytes (§4.3, §5.5).
// Body is shortened first on UTF-8 rune boundaries, followed by secondary prose/text fields.
func enforceAttributesCeiling(attrs map[string]string, maxBytes int) error {
	encoded, err := encodeCompactJSON(attrs)
	if err != nil {
		return err
	}
	if len(encoded) <= maxBytes {
		return nil
	}

	priorityKeys := []string{
		"body",
		"diff_hunk",
		"command_line",
		"command_args",
		"issue_title",
		"title",
		"head_message",
	}
	for _, key := range priorityKeys {
		for len(encoded) > maxBytes {
			cur := attrs[key]
			if cur == "" {
				break
			}
			excess := len(encoded) - maxBytes
			target := len(cur) - excess
			if target < 0 {
				target = 0
			}
			cutFn := truncateUTF8
			if key == "diff_hunk" {
				cutFn = truncateUTF8Tail
			}
			next := cutFn(cur, target)
			if len(next) == len(cur) && target > 0 {
				next = cutFn(cur, target-1)
			}
			attrs[key] = next
			if key == "issue_title" {
				if _, hasTitle := attrs["title"]; hasTitle {
					attrs["title"] = next
				}
			}
			encoded, err = encodeCompactJSON(attrs)
			if err != nil {
				return err
			}
		}
		if len(encoded) <= maxBytes {
			return nil
		}
	}

	protected := map[string]bool{
		"provider": true, "repo": true, "repo_owner": true, "repo_name": true,
		"repo_private": true, "installation_id": true, "delivery": true,
		"event": true, "action": true, "sender": true, "sender_id": true,
		"sender_type": true, "author_association": true, "number": true,
		"is_pull_request": true, "is_fork": true, "draft": true, "merged": true,
		"state": true, "mergeable_state": true, "head_repo": true, "enriched": true,
		"stale": true, "payload_truncated": true, "lists_truncated": true,
		"mention_detection": true, "mention": true, "command": true,
		"head_ref": true, "head_sha": true, "base_ref": true, "base_sha": true,
		"ref": true, "before": true, "after": true, "is_default_branch": true,
		"forced": true, "created": true, "deleted": true, "commit_count": true,
		"comment_id": true, "review_id": true, "line": true, "path": true,
		"html_url": true, "comment_url": true, "issue_url": true, "pull_request_url": true,
	}
	// Last-resort fallback: delete the largest unprotected key (such as labels,
	// milestones, assignees, or timestamps) rather than truncating identifier-like
	// values into plausible-looking wrong prefixes. Critical git refs/SHAs,
	// tracking IDs/locations, and primary URLs are protected above; if protected
	// keys alone still exceed maxBytes, fail closed with ErrAttributesCeilingExceeded.
	for len(encoded) > maxBytes {
		longestKey := ""
		longestLen := -1
		for k, v := range attrs {
			if protected[k] {
				continue
			}
			weight := len(k) + len(v)
			if weight > longestLen || (weight == longestLen && k > longestKey) {
				longestLen = weight
				longestKey = k
			}
		}
		if longestKey == "" {
			break
		}
		delete(attrs, longestKey)
		encoded, err = encodeCompactJSON(attrs)
		if err != nil {
			return err
		}
	}
	if len(encoded) > maxBytes {
		return fmt.Errorf("%w (%d bytes > %d-byte bound)", ErrAttributesCeilingExceeded, len(encoded), maxBytes)
	}
	return nil
}

// enforceListsCeiling bounds serialized JSON of lists to maxBytes (§4.3, §5.5),
// reporting whether any entries were dropped.
func enforceListsCeiling(lists map[string][]string, maxBytes int) (bool, error) {
	encoded, err := encodeCompactJSON(lists)
	if err != nil {
		return false, err
	}
	truncated := false
	for len(encoded) > maxBytes {
		longestKey := ""
		longestCount := 0
		for k, items := range lists {
			if k == ListEnriched {
				continue
			}
			if len(items) > longestCount || (len(items) == longestCount && len(items) > 0 && k > longestKey) {
				longestCount = len(items)
				longestKey = k
			}
		}
		if longestKey == "" || longestCount == 0 {
			break
		}
		items := lists[longestKey]
		drop := 1
		if longestCount > 8 {
			drop = longestCount / 4
		}
		lists[longestKey] = items[:longestCount-drop]
		truncated = true
		encoded, err = encodeCompactJSON(lists)
		if err != nil {
			return false, err
		}
	}
	if len(encoded) > maxBytes {
		return false, fmt.Errorf("github trigger: normalised lists (%d bytes) exceed %d-byte bound", len(encoded), maxBytes)
	}
	return truncated, nil
}

func reqMap(m map[string]any, key, label string) (map[string]any, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, fmt.Errorf("github trigger: missing %s", label)
	}
	sub, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("github trigger: %s must be a JSON object", label)
	}
	return sub, nil
}

func optMap(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, false
	}
	sub, ok := v.(map[string]any)
	return sub, ok
}

func reqString(m map[string]any, key, label string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", fmt.Errorf("github trigger: missing %s", label)
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", fmt.Errorf("github trigger: %s must be a non-empty string", label)
	}
	return s, nil
}

func optCleanString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return cleanUTF8(s)
}

func optString(m map[string]any, key, label string) (string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", nil
	}
	s, ok := v.(string)
	if !ok || !utf8.ValidString(s) {
		return "", fmt.Errorf("github trigger: %s must be a valid UTF-8 string", label)
	}
	return strings.TrimSpace(s), nil
}

func reqPositiveInt64(m map[string]any, key, label string) (int64, error) {
	n, ok, err := optInt64(m, key, label)
	if err != nil {
		return 0, err
	}
	if !ok || n <= 0 {
		return 0, fmt.Errorf("github trigger: %s must be a positive integer", label)
	}
	return n, nil
}

func optNonNegativeInt64(m map[string]any, key, label string) (int64, error) {
	n, ok, err := optInt64(m, key, label)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, nil
	}
	if n < 0 {
		return 0, fmt.Errorf("github trigger: %s must not be negative", label)
	}
	return n, nil
}

func optInt64(m map[string]any, key, label string) (int64, bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, false, fmt.Errorf("github trigger: %s must be an integer", label)
	}
	parsed, err := num.Int64()
	if err != nil {
		return 0, false, fmt.Errorf("github trigger: invalid integer %s: %w", label, err)
	}
	return parsed, true, nil
}

func optBool(m map[string]any, key, label string) (bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, fmt.Errorf("github trigger: %s must be a boolean", label)
	}
	return b, nil
}

func optTimestamp(m map[string]any, key, label string) (time.Time, string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return time.Time{}, "", nil
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, "", fmt.Errorf("github trigger: %s must be an RFC3339 string", label)
	}
	if s == "" {
		return time.Time{}, "", nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("github trigger: invalid RFC3339 timestamp in %s: %w", label, err)
	}
	utc := t.UTC()
	return utc, utc.Format(time.RFC3339), nil
}

func firstNonZeroTime(times ...time.Time) time.Time {
	for _, t := range times {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func cleanUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

func isActionIdent(s string) bool {
	if len(s) == 0 || len(s) > maxActionBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func parseOwnerRepo(fullName string) (owner, repo string, ok bool) {
	if len(fullName) > maxRepoFullNameBytes || !utf8.ValidString(fullName) || !allPrintableNonSpace(fullName) {
		return "", "", false
	}
	idx := strings.IndexByte(fullName, '/')
	if idx <= 0 || idx >= len(fullName)-1 {
		return "", "", false
	}
	owner = fullName[:idx]
	repo = fullName[idx+1:]
	if strings.ContainsRune(repo, '/') || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", "", false
	}
	return owner, repo, true
}

func allPrintable(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func allPrintableNonSpace(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
