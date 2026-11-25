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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	githubtrigger "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

const (
	githubTriggerCommentPermission = "github_trigger.comment"

	// TriggerTemplateSlotGitHubCommentBody is the PhasePre template slot for
	// github.comment action bodies (§7.1).
	TriggerTemplateSlotGitHubCommentBody = "github.comment.body"

	// TriggerTemplateSlotGitHubAckCommentBody is the PhasePost template slot for
	// dispatch_agent ackComment bodies (§7.1, §7.2).
	TriggerTemplateSlotGitHubAckCommentBody = "github.ackComment.body"

	githubCommentTargetEvent = "event"

	githubCommentAuditEventType = "trigger.fire.github_comment"

	githubReviewCommentEventPrefix = "github.pull_request_review_comment."

	unmatchedCommandRateWindow    = time.Minute
	unmatchedCommandBucketIdleTTL = 10 * time.Minute
	unmatchedCommandMaxEntries    = 10000
)

var (
	errGitHubCommentNotAssociated = errors.New("project does not have an authorized GitHub App association")
	errGitHubCommentInvalid       = errors.New("invalid github.comment payload")
)

func init() {
	slots := []triggertemplate.Slot{
		{
			Name:     TriggerTemplateSlotGitHubCommentBody,
			Phase:    triggertemplate.PhasePre,
			MaxBytes: githubapp.MaxCommentBodyBytes,
			Accept:   acceptGitHubCommentUTF8NoNUL,
			Frame:    false,
		},
		{
			Name:     TriggerTemplateSlotGitHubAckCommentBody,
			Phase:    triggertemplate.PhasePost,
			MaxBytes: githubapp.MaxCommentBodyBytes,
			Accept:   acceptGitHubCommentUTF8NoNUL,
			Frame:    false,
		},
	}
	for _, slot := range slots {
		if err := triggertemplate.Register(slot); err != nil {
			panic(err)
		}
	}
}

func acceptGitHubCommentUTF8NoNUL(rendered string) error {
	if !utf8.ValidString(rendered) {
		return errors.New("rendered comment body is not valid UTF-8")
	}
	if strings.IndexByte(rendered, 0) >= 0 {
		return errors.New("rendered comment body must not contain NUL bytes")
	}
	return nil
}

// GitHubCommentActionPayload is the stored JSON payload for a github.comment
// trigger action (§7.1).
type GitHubCommentActionPayload struct {
	Body          string   `json:"body"`
	Target        string   `json:"target,omitempty"`
	ReplyInThread bool     `json:"replyInThread,omitempty"`
	UntrustedVars []string `json:"untrustedVars,omitempty"`
}

// githubCommentAction implements FireAction, FirePayloadValidator, and
// FirePreauthorizer for Stage 3 §7.1 github.comment trigger fires.
type githubCommentAction struct {
	srv *Server
}

type githubCommentAuthorized struct {
	payload GitHubCommentActionPayload
	tmpl    *triggertemplate.Template
	project *store.Project
	auth    FireAuthority
}

func (a *githubCommentAction) Type() string {
	return store.TriggerActionGitHubComment
}

func (a *githubCommentAction) Permission() string {
	return githubTriggerCommentPermission
}

func (a *githubCommentAction) Detaches(_ hubfire.FireRequest) bool {
	return false
}

// SanitizedReasons implements FireErrorSanitizer so github.comment's generic
// failure reasons stay with the action rather than in the generic fire core.
func (a *githubCommentAction) SanitizedReasons() ([]string, string) {
	return []string{
		"invalid github.comment payload",
		"github comment refused",
		"github rate limited",
		"github comment failed",
		"github write not permitted",
		"github permission not permitted",
		"github association not authorized",
		"github app identity unknown",
		"github own app event",
		"stale event",
		"comment rate limited",
	}, "github comment failed"
}

func parseAndValidateGitHubCommentPayload(raw string) (GitHubCommentActionPayload, *triggertemplate.Template, error) {
	var p GitHubCommentActionPayload
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return GitHubCommentActionPayload{}, nil, fmt.Errorf("%w: %v", errGitHubCommentInvalid, err)
	}
	if p.Body == "" {
		return GitHubCommentActionPayload{}, nil, fmt.Errorf("%w: body is required", errGitHubCommentInvalid)
	}
	if len(p.Body) > githubapp.MaxCommentBodyBytes {
		return GitHubCommentActionPayload{}, nil, fmt.Errorf("%w: body exceeds %d-byte limit", errGitHubCommentInvalid, githubapp.MaxCommentBodyBytes)
	}
	if err := acceptGitHubCommentUTF8NoNUL(p.Body); err != nil {
		return GitHubCommentActionPayload{}, nil, fmt.Errorf("%w: %v", errGitHubCommentInvalid, err)
	}
	target := strings.TrimSpace(p.Target)
	if target == "" {
		target = githubCommentTargetEvent
	}
	if target != githubCommentTargetEvent {
		return GitHubCommentActionPayload{}, nil, fmt.Errorf("%w: target must be %q", errGitHubCommentInvalid, githubCommentTargetEvent)
	}
	p.Target = target

	tmpl, err := triggertemplate.ValidateSlot(p.Body, TriggerTemplateSlotGitHubCommentBody, p.UntrustedVars)
	if err != nil {
		return GitHubCommentActionPayload{}, nil, fmt.Errorf("%w: %v", errGitHubCommentInvalid, err)
	}
	return p, tmpl, nil
}

// validateGitHubCommentReplyInThreadMatchTypes enforces §7.1: replyInThread is
// only valid when every match type is a pull_request_review_comment event.
func validateGitHubCommentReplyInThreadMatchTypes(replyInThread bool, matchTypes []string) error {
	if !replyInThread {
		return nil
	}
	if len(matchTypes) == 0 {
		return fmt.Errorf("%w: replyInThread requires match.types to contain only %s* event types", errGitHubCommentInvalid, githubReviewCommentEventPrefix)
	}
	for _, mt := range matchTypes {
		if !strings.HasPrefix(mt, githubReviewCommentEventPrefix) {
			return fmt.Errorf("%w: replyInThread is only supported with %s* match types (got %q)", errGitHubCommentInvalid, githubReviewCommentEventPrefix, mt)
		}
	}
	return nil
}

// Validate parses and bounds payload at authoring time and verifies the project
// has an authorized GitHub App association (§7.1).
func (a *githubCommentAction) Validate(ctx context.Context, _ Identity, projectID string, payload string) (string, error) {
	if a == nil || a.srv == nil {
		return "", errors.New("github.comment action: server not configured")
	}
	p, _, err := parseAndValidateGitHubCommentPayload(payload)
	if err != nil {
		return "", err
	}
	project, err := a.srv.store.GetProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	authorized, err := a.srv.hasAuthorizedGitHubAssociationCached(ctx, project)
	if err != nil {
		return "", err
	}
	if !authorized {
		return "", errGitHubCommentNotAssociated
	}
	normalized, err := json.Marshal(p)
	if err != nil {
		return "", fmt.Errorf("encode github.comment payload: %w", err)
	}
	return string(normalized), nil
}

func (a *githubCommentAction) ValidatePayload(req hubfire.FireRequest) error {
	if _, _, err := parseAndValidateGitHubCommentPayload(req.Payload); err != nil {
		return err
	}
	if req.Event == nil || req.Event.SourceKind != store.TriggerSourceKindGitHub {
		return fmt.Errorf("%w: github.comment requires a github trigger event", errGitHubCommentInvalid)
	}
	return nil
}

func (a *githubCommentAction) Preauthorize(ctx context.Context, req hubfire.FireRequest) (*FireRefusal, error) {
	if req.Authority.DefinitionKind != hubfire.DefinitionKindTrigger {
		return &FireRefusal{Reason: "github.comment refused"}, nil
	}
	if a == nil || a.srv == nil || !a.srv.githubTriggersEnabled() {
		return &FireRefusal{Reason: "github.comment refused"}, nil
	}
	// Stage 3 §4.4: refuse github.comment fires for stale events (>24h old).
	if req.Event != nil && req.Event.Attributes["stale"] == "true" {
		return &FireRefusal{Reason: "stale event"}, nil
	}
	if refusal, _ := a.srv.githubBotSenderRefusal(ctx, req.Event); refusal != nil {
		return refusal, nil
	}
	return nil, nil
}

// githubBotSenderRefusal enforces the G4 own-App guard revisit
// (G1a security S3, Option A) and returns both the sanitized refusal and its
// structured audit reason:
//   - while the App's bot login is unknown (GET /app failed and
//     server.github_app.app_slug is unset), non-object-creating GitHub events
//     from Bot senders cannot be checked against <slug>[bot] at ingress, so
//     dispatch_agent and github.comment fires fail closed;
//   - once the App's bot login is known (via GET /app recovery or app_slug),
//     re-check queued events against <slug>[bot] so an App-authored event
//     enqueued during the unknown window is still refused on drain.
func (s *Server) githubBotSenderRefusal(ctx context.Context, ev *hubfire.FireEvent) (*FireRefusal, string) {
	if s == nil || ev == nil || ev.SourceKind != store.TriggerSourceKindGitHub {
		return nil, ""
	}
	senderType := strings.TrimSpace(ev.Attributes["sender_type"])
	if !strings.EqualFold(senderType, "Bot") {
		return nil, ""
	}
	botLogin := s.GitHubAppBotLogin(ctx)
	if botLogin == "" {
		return &FireRefusal{Reason: "github app identity unknown"}, "github_app_identity_unknown"
	}
	slug := strings.TrimSuffix(botLogin, "[bot]")
	if githubtrigger.IsOwnAppSender(senderType, ev.Attributes["sender"], slug) {
		return &FireRefusal{Reason: "github own app event"}, "github_own_app_event"
	}
	return nil, ""
}

// authorizeGitHubCommentFire verifies the frozen effect ceiling and live
// github_trigger.comment authorization for the recorded revision principal.
func (s *Server) authorizeGitHubCommentFire(
	ctx context.Context,
	projectID string,
	auth FireAuthority,
	identity Identity,
) error {
	if !EffectCeilingAllows(auth.Ceiling, githubTriggerCommentPermission, false) {
		return fmt.Errorf("frozen ceiling denies %s", githubTriggerCommentPermission)
	}
	if s.authzService == nil {
		return errors.New("authorization service is uninitialized")
	}

	switch auth.PrincipalKind {
	case store.DelegationPrincipalUser:
		userIdent, ok := identity.(UserIdentity)
		if !ok {
			return errors.New("invalid user identity")
		}
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource: Resource{
				Type:       permissions.ResourceGitHubTrigger,
				ID:         projectID,
				ParentType: permissions.ResourceProject,
				ParentID:   projectID,
			},
			Action:     Action(permissions.ActionComment),
			Permission: githubTriggerCommentPermission,
		})
		if !decision.Allowed {
			return fmt.Errorf("policy denied %s: %s", githubTriggerCommentPermission, decision.Reason)
		}
		return nil

	case store.DelegationPrincipalAgent:
		// Design rev 1.12 §8.1: in Stage 3, github_trigger.comment has no agent
		// scope, so agent principals cannot execute github.comment triggers.
		return errors.New("agent principals cannot execute github.comment triggers")

	default:
		return fmt.Errorf("unsupported principal kind %q", auth.PrincipalKind)
	}
}

func (a *githubCommentAction) Authorize(
	ctx context.Context,
	req hubfire.FireRequest,
	auth FireAuthority,
	identity Identity,
) (FireAuthorized, *FireRefusal, error) {
	if a == nil || a.srv == nil {
		return nil, nil, errors.New("github.comment action: server not configured")
	}
	s := a.srv
	payload, tmpl, err := parseAndValidateGitHubCommentPayload(req.Payload)
	if err != nil {
		return nil, nil, err
	}

	if authErr := s.authorizeGitHubCommentFire(ctx, req.ProjectID, auth, identity); authErr != nil {
		slog.WarnContext(ctx, "github.comment fire refused at authorization",
			"fire_id", req.Authority.FireID,
			"trigger_id", req.Authority.DefinitionID,
			"project_id", req.ProjectID,
			"principal_kind", auth.PrincipalKind,
			"principal_id", auth.PrincipalID,
			"authorization_revision", auth.Revision,
			"error", authErr)
		return nil, &FireRefusal{Reason: "github.comment refused"}, nil
	}
	if err := s.fireStanding(ctx, auth, nil); err != nil {
		return nil, &FireRefusal{Reason: "github.comment refused"}, nil
	}

	project, err := s.store.GetProject(ctx, req.ProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return nil, &FireRefusal{Reason: "github.comment refused"}, nil
		}
		return nil, nil, fmt.Errorf("github.comment project lookup: %w", err)
	}
	return &githubCommentAuthorized{
		payload: payload,
		tmpl:    tmpl,
		project: project,
		auth:    auth,
	}, nil, nil
}

func (a *githubCommentAction) Fire(
	ctx context.Context,
	req hubfire.FireRequest,
	authorized FireAuthorized,
) (hubfire.FireOutcome, error) {
	if a == nil || a.srv == nil {
		return hubfire.FireOutcome{}, errors.New("github.comment action: server not configured")
	}
	ga, ok := authorized.(*githubCommentAuthorized)
	if !ok || ga == nil || ga.project == nil || ga.tmpl == nil {
		return hubfire.FireOutcome{}, errors.New("github.comment action: missing authorized state")
	}
	if req.Event == nil {
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  "invalid github.comment payload",
		}, nil
	}

	target, err := extractGitHubCommentTarget(req.Event.Type, req.Event.Attributes, ga.payload.ReplyInThread)
	if err != nil {
		slog.WarnContext(ctx, "github.comment fire missing event target",
			"fire_id", req.Authority.FireID,
			"trigger_id", req.Authority.DefinitionID,
			"project_id", req.ProjectID,
			"event_id", req.Event.ID,
			"error", err)
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  "github comment failed",
		}, nil
	}

	vars := buildGitHubCommentTemplateVars(req.Event, ga.project)
	renderedBody, _, renderErr := ga.tmpl.Render(vars, ga.payload.UntrustedVars)
	if renderErr != nil {
		slog.WarnContext(ctx, "github.comment template render failed",
			"fire_id", req.Authority.FireID,
			"trigger_id", req.Authority.DefinitionID,
			"project_id", req.ProjectID,
			"event_id", req.Event.ID,
			"error", renderErr)
		return hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  "github comment failed",
		}, nil
	}

	attempts := req.Attempts
	if attempts <= 0 {
		attempts = 1
	}

	comment, outcome := a.srv.postGitHubTriggerComment(ctx, ga.project, githubapp.PostCommentOptions{
		Owner:           target.owner,
		Repo:            target.repo,
		IssueNumber:     target.number,
		ReviewCommentID: target.reviewCommentID,
		Body:            renderedBody,
		FireID:          req.Authority.FireID,
		Attempts:        attempts,
		Since:           req.Event.ReceivedAt,
	})
	if outcome != nil {
		return *outcome, nil
	}

	emitGitHubCommentAudit(ctx, githubCommentAuditParams{
		ProjectID: req.ProjectID,
		TriggerID: req.Authority.DefinitionID,
		FireID:    req.Authority.FireID,
		EventID:   req.Event.ID,
		Repo:      target.repoFullName,
		Number:    target.number,
		CommentID: comment.ID,
		HTMLURL:   comment.HTMLURL,
	})

	return hubfire.FireOutcome{
		Status:    hubfire.FireStatusFired,
		ResultRef: strconv.FormatInt(comment.ID, 10),
	}, nil
}

type githubCommentTarget struct {
	owner           string
	repo            string
	repoFullName    string
	number          int
	reviewCommentID int64
}

func extractGitHubCommentTarget(eventType string, attrs map[string]string, replyInThread bool) (githubCommentTarget, error) {
	if attrs == nil {
		return githubCommentTarget{}, errors.New("event has no attributes")
	}
	repoFullName := strings.TrimSpace(attrs["repo"])
	owner := strings.TrimSpace(attrs["repo_owner"])
	repo := strings.TrimSpace(attrs["repo_name"])
	if (owner == "" || repo == "") && repoFullName != "" {
		if parts := strings.SplitN(repoFullName, "/", 2); len(parts) == 2 {
			owner = strings.TrimSpace(parts[0])
			repo = strings.TrimSpace(parts[1])
		}
	}
	if owner == "" || repo == "" {
		return githubCommentTarget{}, errors.New("event missing repository owner/name")
	}
	if repoFullName == "" {
		repoFullName = owner + "/" + repo
	}
	numberStr := strings.TrimSpace(attrs["number"])
	number, err := strconv.Atoi(numberStr)
	if err != nil || number <= 0 {
		return githubCommentTarget{}, errors.New("event missing valid issue/PR number")
	}

	var reviewCommentID int64
	if replyInThread {
		if !strings.HasPrefix(eventType, githubReviewCommentEventPrefix) {
			return githubCommentTarget{}, errors.New("replyInThread is only valid for pull_request_review_comment events")
		}
		cidStr := strings.TrimSpace(attrs["comment_id"])
		cid, cidErr := strconv.ParseInt(cidStr, 10, 64)
		if cidErr != nil || cid <= 0 {
			return githubCommentTarget{}, errors.New("review comment event missing valid comment_id")
		}
		reviewCommentID = cid
	}
	return githubCommentTarget{
		owner:           owner,
		repo:            repo,
		repoFullName:    repoFullName,
		number:          number,
		reviewCommentID: reviewCommentID,
	}, nil
}

func buildGitHubCommentTemplateVars(ev *hubfire.FireEvent, project *store.Project) triggertemplate.Vars {
	fixed := make(triggertemplate.MapVars, 16+len(ev.Attributes))
	fixed["event.id"] = ev.ID
	fixed["event.source.kind"] = ev.SourceKind
	fixed["event.source.id"] = ev.SourceID
	if name := ev.Attributes["endpoint_name"]; name != "" {
		fixed["event.source.name"] = name
	} else {
		fixed["event.source.name"] = ev.SourceKind
	}
	if !ev.ReceivedAt.IsZero() {
		fixed["event.received_at"] = ev.ReceivedAt.UTC().Format(time.RFC3339)
	}
	fixed["event.type"] = ev.Type
	fixed["event.subject"] = ev.Subject
	if !ev.OccurredAt.IsZero() {
		fixed["event.occurred_at"] = ev.OccurredAt.UTC().Format(time.RFC3339)
	}
	if ev.Trigger != nil {
		fixed["trigger.id"] = ev.Trigger.ID
		fixed["trigger.name"] = ev.Trigger.Name
	}
	if project != nil {
		fixed["project.id"] = project.ID
		fixed["project.name"] = project.Name
	}
	for k, v := range ev.Attributes {
		fixed["event.attributes."+k] = v
	}

	if ev.PayloadRef != "" {
		return triggertemplate.ChainVars(fixed, payloadErrorVars{err: triggertemplate.ErrPayloadOutOfLine})
	}
	if len(ev.Payload) > 0 {
		if pv, err := triggertemplate.NewPayloadVars(ev.Payload); err == nil {
			return triggertemplate.ChainVars(fixed, pv)
		}
	}
	return fixed
}

// postGitHubTriggerComment runs the shared §7.1 comment posting sequence:
// per-project rate bucket -> mint installation token ({issues: write, metadata: read})
// -> marker-idempotent post -> failure classification & project status update.
// Returns (comment, nil) on success or (nil, terminalOutcome) on refusal/skip/failure.
func (s *Server) postGitHubTriggerComment(
	ctx context.Context,
	project *store.Project,
	opts githubapp.PostCommentOptions,
) (*githubapp.IssueComment, *hubfire.FireOutcome) {
	if err := s.AllowGitHubComment(project.ID); err != nil {
		if errors.Is(err, ErrGitHubCommentRateLimited) {
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusSkipped,
				Error:  "comment rate limited",
			}
		}
		return nil, &hubfire.FireOutcome{
			Status: hubfire.FireStatusFailed,
			Error:  "github comment failed",
		}
	}

	commentPerms := githubapp.TokenPermissions{
		Issues:   "write",
		Metadata: "read",
	}
	token, _, err := s.MintGitHubAppTokenWithPermissions(ctx, project, commentPerms)
	if err != nil {
		switch {
		case errors.Is(err, ErrGitHubWriteNotPermitted):
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusRefused,
				Error:  "github write not permitted",
			}
		case errors.Is(err, ErrGitHubPermissionNotPermitted):
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusRefused,
				Error:  "github permission not permitted",
			}
		case errors.Is(err, errGitHubAssociationUnauthorized):
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusRefused,
				Error:  "github association not authorized",
			}
		default:
			var mintErr *githubapp.TokenMintError
			if errors.As(err, &mintErr) && mintErr.ErrorCode == githubapp.ErrCodePermissionDenied {
				return nil, &hubfire.FireOutcome{
					Status: hubfire.FireStatusFailed,
					Error:  "github comment refused",
				}
			}
			slog.WarnContext(ctx, "github.comment installation token mint failed",
				"project_id", project.ID,
				"fire_id", opts.FireID,
				"error", err)
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusFailed,
				Error:  "github comment failed",
			}
		}
	}

	comment, err := s.PostGitHubCommentIdempotent(ctx, token, opts)
	if err != nil {
		var commentErr *githubapp.CommentAPIError
		upstream := ""
		if errors.As(err, &commentErr) {
			upstream = commentErr.UpstreamDetail()
		}
		switch {
		case errors.Is(err, githubapp.ErrCommentRefused):
			slog.WarnContext(ctx, "github.comment refused by GitHub",
				"project_id", project.ID,
				"fire_id", opts.FireID,
				"repo", opts.Owner+"/"+opts.Repo,
				"number", opts.IssueNumber,
				"error", err,
				"upstream", upstreamText(upstream))
			s.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateDegraded,
				githubapp.ErrCodePermissionDenied, githubAppStatusMessage(githubapp.ErrCodePermissionDenied))
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusFailed,
				Error:  "github comment refused",
			}
		case errors.Is(err, githubapp.ErrRateLimited):
			slog.WarnContext(ctx, "github.comment rate limited by GitHub",
				"project_id", project.ID,
				"fire_id", opts.FireID,
				"repo", opts.Owner+"/"+opts.Repo,
				"number", opts.IssueNumber,
				"error", err)
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusFailed,
				Error:  "github rate limited",
			}
		default:
			slog.WarnContext(ctx, "github.comment post failed",
				"project_id", project.ID,
				"fire_id", opts.FireID,
				"repo", opts.Owner+"/"+opts.Repo,
				"number", opts.IssueNumber,
				"error", err,
				"upstream", upstreamText(upstream))
			return nil, &hubfire.FireOutcome{
				Status: hubfire.FireStatusFailed,
				Error:  "github comment failed",
			}
		}
	}
	return comment, nil
}

type githubCommentAuditParams struct {
	ProjectID        string
	TriggerID        string
	FireID           string
	EventID          string
	Repo             string
	Number           int
	CommentID        int64
	HTMLURL          string
	UnmatchedCommand bool
	ActorID          string
	ExecutorKind     string
	ExecutorID       string
}

// emitGitHubCommentAudit emits the content-free trigger.fire.github_comment
// audit record (§7.1, §8.3). Comment bodies are never logged or audited.
func emitGitHubCommentAudit(ctx context.Context, p githubCommentAuditParams) {
	attrs := []slog.Attr{
		slog.String("event_type", githubCommentAuditEventType),
		slog.String("project", p.ProjectID),
		slog.String("project_id", p.ProjectID),
		slog.String("repo", p.Repo),
		slog.Int("number", p.Number),
		slog.Int64("comment_id", p.CommentID),
	}
	if p.TriggerID != "" {
		attrs = append(attrs, slog.String("trigger", p.TriggerID), slog.String("trigger_id", p.TriggerID))
	}
	if p.FireID != "" {
		attrs = append(attrs, slog.String("fire", p.FireID), slog.String("fire_id", p.FireID))
	}
	if p.EventID != "" {
		attrs = append(attrs, slog.String("event_id", p.EventID))
	}
	if p.HTMLURL != "" {
		attrs = append(attrs, slog.String("html_url", p.HTMLURL))
	}
	if p.UnmatchedCommand {
		attrs = append(attrs, slog.Bool("unmatched_command", true))
	}
	if p.ActorID != "" {
		attrs = append(attrs, slog.String("actor_id", p.ActorID))
	}
	if p.ExecutorKind != "" {
		attrs = append(attrs, slog.String("executor_kind", p.ExecutorKind))
	}
	if p.ExecutorID != "" {
		attrs = append(attrs, slog.String("executor_id", p.ExecutorID))
	}
	logging.Subsystem("hub.audit").LogAttrs(ctx, slog.LevelInfo, githubCommentAuditEventType, attrs...)
}

type unmatchedCommandKey struct {
	projectID string
	subject   string
}

// unmatchedCommandRateLimiter enforces 1 unmatched-command reply per minute
// per (project, subject) (§8.3). Its zero value is ready to use.
type unmatchedCommandRateLimiter struct {
	mu         sync.Mutex
	lastSent   map[unmatchedCommandKey]time.Time
	lastSweep  time.Time
	now        func() time.Time
	maxEntries int
}

func (l *unmatchedCommandRateLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return timeNow()
}

func (l *unmatchedCommandRateLimiter) capLimit() int {
	if l.maxEntries > 0 {
		return l.maxEntries
	}
	return unmatchedCommandMaxEntries
}

func (l *unmatchedCommandRateLimiter) allow(projectID, subject string) bool {
	projectID = strings.TrimSpace(projectID)
	subject = strings.TrimSpace(subject)
	if projectID == "" || subject == "" {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.clock()
	if l.lastSent == nil {
		l.lastSent = make(map[unmatchedCommandKey]time.Time)
		l.lastSweep = now
	}
	l.sweepLocked(now)

	key := unmatchedCommandKey{projectID: projectID, subject: subject}
	if prev, ok := l.lastSent[key]; ok && now.Sub(prev) < unmatchedCommandRateWindow {
		return false
	}
	l.lastSent[key] = now
	return true
}

func (l *unmatchedCommandRateLimiter) sweepLocked(now time.Time) {
	capLimit := l.capLimit()
	atCap := len(l.lastSent) >= capLimit
	if !atCap && now.Sub(l.lastSweep) < unmatchedCommandBucketIdleTTL {
		return
	}
	l.lastSweep = now

	for k, ts := range l.lastSent {
		if now.Sub(ts) >= unmatchedCommandBucketIdleTTL {
			delete(l.lastSent, k)
		}
	}
	if len(l.lastSent) < capLimit {
		return
	}

	keys := make([]unmatchedCommandKey, 0, len(l.lastSent))
	for k := range l.lastSent {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b unmatchedCommandKey) int {
		if c := l.lastSent[a].Compare(l.lastSent[b]); c != 0 {
			return c
		}
		if a.projectID != b.projectID {
			return strings.Compare(a.projectID, b.projectID)
		}
		return strings.Compare(a.subject, b.subject)
	})
	drop := len(keys) / 2
	if drop == 0 && len(keys) > 0 {
		drop = 1
	}
	for _, k := range keys[:drop] {
		delete(l.lastSent, k)
	}
}

// handleUnmatchedGitHubCommand implements Stage 3 §8.3: when a GitHub event in
// a project has attributes.command set, all candidate rules were evaluated with
// fire_count == 0 (no rule matched and no skipped/refused fires), and the
// project setting unmatched_command_reply is enabled, post one guidance comment
// as the hub through the shared comment path.
func (s *Server) handleUnmatchedGitHubCommand(ctx context.Context, ev *store.TriggerEvent, attempts int) {
	if s == nil || s.store == nil || ev == nil {
		return
	}
	if ev.SourceKind != store.TriggerSourceKindGitHub || !s.githubTriggersEnabled() {
		return
	}
	command := strings.TrimSpace(ev.Attributes["command"])
	if command == "" {
		return
	}
	// Never reply to bots (§8.3) or stale events (§4.4).
	if strings.EqualFold(strings.TrimSpace(ev.Attributes["sender_type"]), "Bot") {
		return
	}
	if ev.Attributes["stale"] == "true" {
		return
	}

	project, err := s.store.GetProject(ctx, ev.ProjectID)
	if err != nil || project == nil {
		return
	}
	replyEnabled, customText := readProjectGitHubTriggerSettings(project.Annotations)
	if !replyEnabled {
		return
	}

	target, err := extractGitHubCommentTarget(ev.Type, ev.Attributes, false)
	if err != nil {
		return
	}

	subject := strings.TrimSpace(ev.Subject)
	if subject == "" {
		subject = fmt.Sprintf("%s#%d", target.repoFullName, target.number)
	}
	if attempts <= 1 {
		if !s.unmatchedCommandLimiter.allow(project.ID, subject) {
			return
		}
	}

	projectName := project.Name
	if projectName == "" {
		projectName = project.ID
	}
	suffix := strings.TrimSpace(customText)
	if suffix == "" {
		suffix = "Available commands are listed in the project's Triggers page."
	}
	body := fmt.Sprintf("No automation in project **%s** handles `/%s` here. %s", projectName, command, suffix)

	if attempts <= 0 {
		attempts = 1
	}
	fireID := "unmatched-" + ev.ID
	comment, outcome := s.postGitHubTriggerComment(ctx, project, githubapp.PostCommentOptions{
		Owner:       target.owner,
		Repo:        target.repo,
		IssueNumber: target.number,
		Body:        body,
		FireID:      fireID,
		Attempts:    attempts,
		Since:       ev.ReceivedAt,
	})
	if outcome != nil {
		slog.WarnContext(ctx, "unmatched GitHub command reply did not post",
			"project_id", project.ID,
			"event_id", ev.ID,
			"status", outcome.Status,
			"error", outcome.Error)
		return
	}

	actorID := ""
	if project.Annotations != nil {
		actorID = strings.TrimSpace(project.Annotations[AnnotationGitHubTriggersUpdatedBy])
	}
	emitGitHubCommentAudit(ctx, githubCommentAuditParams{
		ProjectID:        project.ID,
		FireID:           fireID,
		EventID:          ev.ID,
		Repo:             target.repoFullName,
		Number:           target.number,
		CommentID:        comment.ID,
		HTMLURL:          comment.HTMLURL,
		UnmatchedCommand: true,
		ActorID:          actorID,
		ExecutorKind:     trigger.ExecutorKindTriggerDrainer,
		ExecutorID:       trigger.ExecutorIDPrefixTriggerEvent + ev.ID,
	})
}

const githubAckCommentFailedAuditEventType = "trigger.fire.github_ack_comment_failed"

// validateGitHubAckCommentPayload validates the dispatch_agent ackComment
// payload against the PhasePost github.ackComment.body slot (§7.1, §7.2).
func validateGitHubAckCommentPayload(ack *DispatchAgentAckComment, untrustedVars []string) error {
	if ack == nil {
		return nil
	}
	if ack.Body == "" {
		return &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: "action.ackComment.body is required when ackComment is set",
		}
	}
	if len(ack.Body) > githubapp.MaxCommentBodyBytes {
		return &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: fmt.Sprintf("action.ackComment.body exceeds %d-byte limit", githubapp.MaxCommentBodyBytes),
		}
	}
	if err := acceptGitHubCommentUTF8NoNUL(ack.Body); err != nil {
		return &dispatchAgentValidationError{
			status:  http.StatusBadRequest,
			code:    ErrCodeValidationError,
			message: fmt.Sprintf("invalid action.ackComment.body: %v", err),
		}
	}
	if _, err := validateTemplateSlot(ack.Body, TriggerTemplateSlotGitHubAckCommentBody, untrustedVars); err != nil {
		return err
	}
	return nil
}

// postDispatchAgentAckComment renders and posts the post-fire GitHub
// acknowledgement comment for a fired dispatch_agent trigger through the shared
// github.comment code path (§7.1, §7.2, app-shaped-source-seams §2.1).
// Failures never alter the dispatch outcome and are recorded visibly via audit
// and slog without ever logging the comment body.
func (s *Server) postDispatchAgentAckComment(
	ctx context.Context,
	req hubfire.FireRequest,
	da *dispatchAgentAuthorized,
	agent *store.Agent,
) {
	if s == nil || req.Event == nil || da == nil || da.payload.AckComment == nil {
		return
	}
	var agentID, agentName string
	if agent != nil {
		agentID = agent.ID
		agentName = agent.Name
		if agentName == "" {
			agentName = agent.Slug
		}
	}

	recordAckFailure := func(status hubfire.FireStatus, reason string) {
		slog.WarnContext(ctx, "dispatch_agent ackComment did not post",
			"project_id", req.ProjectID,
			"trigger_id", req.Authority.DefinitionID,
			"fire_id", req.Authority.FireID,
			"event_id", req.Event.ID,
			"agent_id", agentID,
			"status", status,
			"reason", reason)
		logging.Subsystem("hub.audit").LogAttrs(ctx, slog.LevelWarn, githubAckCommentFailedAuditEventType,
			slog.String("event_type", githubAckCommentFailedAuditEventType),
			slog.String("project", req.ProjectID),
			slog.String("project_id", req.ProjectID),
			slog.String("trigger", req.Authority.DefinitionID),
			slog.String("trigger_id", req.Authority.DefinitionID),
			slog.String("fire", req.Authority.FireID),
			slog.String("fire_id", req.Authority.FireID),
			slog.String("event_id", req.Event.ID),
			slog.String("agent_id", agentID),
			slog.String("status", string(status)),
			slog.String("reason", reason),
		)
	}

	if da.project == nil {
		recordAckFailure(hubfire.FireStatusFailed, "github comment failed")
		return
	}
	if authErr := s.authorizeGitHubCommentFire(ctx, req.ProjectID, da.auth, da.identity); authErr != nil {
		slog.WarnContext(ctx, "dispatch_agent ackComment refused at authorization",
			"fire_id", req.Authority.FireID,
			"trigger_id", req.Authority.DefinitionID,
			"project_id", req.ProjectID,
			"principal_kind", da.auth.PrincipalKind,
			"principal_id", da.auth.PrincipalID,
			"authorization_revision", da.auth.Revision,
			"error", authErr)
		recordAckFailure(hubfire.FireStatusRefused, "github.comment refused")
		return
	}
	target, err := extractGitHubCommentTarget(req.Event.Type, req.Event.Attributes, false)
	if err != nil {
		recordAckFailure(hubfire.FireStatusFailed, "github comment failed")
		return
	}

	postVars := triggertemplate.MapVars{
		"result.agent_id":   agentID,
		"result.agent_name": agentName,
		"result.agent_url":  s.AgentWebURL(agentID),
		"result.fire_id":    req.Authority.FireID,
	}
	vars := triggertemplate.ChainVars(postVars, buildGitHubCommentTemplateVars(req.Event, da.project))

	tmpl, err := triggertemplate.ValidateSlot(da.payload.AckComment.Body, TriggerTemplateSlotGitHubAckCommentBody, da.payload.UntrustedVars)
	if err != nil {
		recordAckFailure(hubfire.FireStatusRefused, "invalid ackComment template")
		return
	}
	renderedBody, _, renderErr := tmpl.Render(vars, da.payload.UntrustedVars)
	if renderErr != nil {
		recordAckFailure(hubfire.FireStatusRefused, "ackComment render failed")
		return
	}

	attempts := req.Attempts
	if attempts <= 0 {
		attempts = 1
	}

	comment, outcome := s.postGitHubTriggerComment(ctx, da.project, githubapp.PostCommentOptions{
		Owner:       target.owner,
		Repo:        target.repo,
		IssueNumber: target.number,
		Body:        renderedBody,
		FireID:      req.Authority.FireID,
		Attempts:    attempts,
		Since:       req.Event.ReceivedAt,
	})
	if outcome != nil {
		recordAckFailure(outcome.Status, outcome.Error)
		return
	}

	emitGitHubCommentAudit(ctx, githubCommentAuditParams{
		ProjectID: req.ProjectID,
		TriggerID: req.Authority.DefinitionID,
		FireID:    req.Authority.FireID,
		EventID:   req.Event.ID,
		Repo:      target.repoFullName,
		Number:    target.number,
		CommentID: comment.ID,
		HTMLURL:   comment.HTMLURL,
	})
}
