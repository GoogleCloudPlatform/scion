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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
)

// Compile-time assertions that GitHub's adapters satisfy every app-shaped
// source seam interface in pkg/hub/trigger/source (app-shaped-source-seams.md §2).
var (
	_ source.WireVerifier         = (*WireVerifier)(nil)
	_ source.InstallationResolver = (*InstallationResolver)(nil)
	_ source.Normalizer           = (*EventNormalizer)(nil)
	_ source.ActorIdentity        = (*AppActorIdentity)(nil)
	_ source.IdentityMinter       = (*TokenMinter)(nil)
	_ source.ProviderAction       = (*CommentAction)(nil)
	_ source.Readiness            = (*ReadinessChecker)(nil)
	_ source.Presets              = (*PresetCatalog)(nil)
)

// Sentinel errors returned by GitHub source seam adapters.
var (
	ErrWebhookSecretNotConfigured = errors.New("github source: webhook secret is not configured")
	ErrInvalidWebhookSignature    = errors.New("github source: invalid webhook signature")
	ErrInvalidWebhookHeaders      = errors.New("github source: invalid webhook headers")
	ErrWebhookTargetMismatch      = errors.New("github source: webhook target mismatch")
	ErrInvalidDeliveryID          = errors.New("github source: invalid delivery id")
	ErrUnsupportedPayload         = errors.New("github source: unsupported payload type")
	ErrNotConfigured              = errors.New("github source: adapter callback not configured")
)

// GitHub lifecycle webhook event names handled by InstallationResolver.OnLifecycle.
const (
	EventInstallation             = "installation"
	EventInstallationRepositories = "installation_repositories"
)

// RequiredEvents lists the GitHub App webhook events Stage 3 requires for
// trigger readiness (§10.1), sorted for deterministic output.
var RequiredEvents = []string{
	EventIssueComment,
	EventIssues,
	EventPullRequest,
	EventPullRequestReview,
	EventPullRequestReviewComment,
}

// RequiredPermission names a minimum GitHub App permission required for
// trigger readiness (§10.1).
type RequiredPermission struct {
	Name  string
	Level string
}

// RequiredPermissions lists the GitHub App permissions Stage 3 requires for
// trigger readiness (§10.1), sorted by name for deterministic output.
var RequiredPermissions = []RequiredPermission{
	{Name: "contents", Level: "write"},
	{Name: "issues", Level: "write"},
	{Name: "metadata", Level: "read"},
	{Name: "pull_requests", Level: "write"},
}

// WireVerifier implements source.WireVerifier for GitHub App webhooks.
type WireVerifier struct {
	// ExpectedAppID, when > 0, enforces X-GitHub-Hook-Installation-Target-Type ==
	// "integration" and X-GitHub-Hook-Installation-Target-ID == ExpectedAppID
	// (Stage 3 §4.1 step 1).
	ExpectedAppID int64
	// Now returns the delivery receipt timestamp (defaults to time.Now).
	Now func() time.Time
}

// Verify authenticates a GitHub webhook request using X-Hub-Signature-256 and
// extracts the provider-neutral Delivery metadata.
func (v *WireVerifier) Verify(r *http.Request, body []byte, secrets source.SecretSource) (source.Delivery, error) {
	if r == nil {
		return source.Delivery{}, ErrInvalidWebhookHeaders
	}
	if secrets == nil {
		return source.Delivery{}, ErrWebhookSecretNotConfigured
	}
	secret, err := secrets.WebhookSecret(r.Context())
	if err != nil {
		return source.Delivery{}, err
	}
	if secret == "" {
		return source.Delivery{}, ErrWebhookSecretNotConfigured
	}

	sig := r.Header.Get("X-Hub-Signature-256")
	if !verifyHMACSHA256(body, sig, secret) {
		return source.Delivery{}, ErrInvalidWebhookSignature
	}

	eventValues := r.Header.Values("X-GitHub-Event")
	if len(eventValues) != 1 || strings.TrimSpace(eventValues[0]) == "" {
		return source.Delivery{}, ErrInvalidWebhookHeaders
	}
	eventName := eventValues[0]

	deliveryValues := r.Header.Values("X-GitHub-Delivery")
	deliveryID := ""
	if len(deliveryValues) > 1 {
		return source.Delivery{}, ErrInvalidDeliveryID
	}
	if len(deliveryValues) == 1 {
		deliveryID = deliveryValues[0]
	}
	if IsSupportedEvent(eventName) {
		if !isGUIDDeliveryID(deliveryID) {
			return source.Delivery{}, ErrInvalidDeliveryID
		}
		deliveryID = strings.ToLower(deliveryID)
	}

	targetType := r.Header.Get("X-GitHub-Hook-Installation-Target-Type")
	targetID := r.Header.Get("X-GitHub-Hook-Installation-Target-ID")
	if v != nil && v.ExpectedAppID > 0 && IsSupportedEvent(eventName) {
		targetTypes := r.Header.Values("X-GitHub-Hook-Installation-Target-Type")
		targetIDs := r.Header.Values("X-GitHub-Hook-Installation-Target-ID")
		if len(targetTypes) != 1 || targetTypes[0] != "integration" ||
			len(targetIDs) != 1 || targetIDs[0] != strconv.FormatInt(v.ExpectedAppID, 10) {
			return source.Delivery{}, ErrWebhookTargetMismatch
		}
	}

	nowFn := time.Now
	if v != nil && v.Now != nil {
		nowFn = v.Now
	}

	return source.Delivery{
		ID:         deliveryID,
		TargetID:   targetID,
		TargetType: targetType,
		Event:      eventName,
		Headers:    FilterHeaders(r.Header),
		ReceivedAt: nowFn().UTC(),
	}, nil
}

// DedupKey returns a deterministic provider-level deduplication key combining
// the delivery ID and the signed raw-body SHA-256 digest (Stage 3 rev 1.8 §4.3).
func (v *WireVerifier) DedupKey(d source.Delivery, body []byte) string {
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	id := strings.ToLower(strings.TrimSpace(d.ID))
	if id == "" {
		return bodyHash
	}
	return id + ":" + bodyHash
}

func verifyHMACSHA256(payload []byte, signature, secret string) bool {
	if secret == "" || signature == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	sigBytes, err := hex.DecodeString(signature[len(prefix):])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hmac.Equal(mac.Sum(nil), sigBytes)
}

func isGUIDDeliveryID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// InstallationResolver implements source.InstallationResolver for GitHub App
// installations, delegating store lookups, candidate/primary routing, and
// installation lifecycle persistence to injected hub callbacks.
type InstallationResolver struct {
	LookupInstallation func(ctx context.Context, installationID int64) (source.Installation, error)
	ResolveTargets     func(ctx context.Context, inst source.Installation, repoFullName string) ([]source.Target, error)
	HandleLifecycle    func(ctx context.Context, d source.Delivery, body []byte) (bool, error)
}

// Installation resolves the GitHub App installation referenced by payload.
// payload may be []byte (raw JSON webhook body), Result, or *Result.
func (r *InstallationResolver) Installation(ctx context.Context, _ source.Delivery, payload any) (source.Installation, error) {
	if r == nil || r.LookupInstallation == nil {
		return source.Installation{}, ErrNotConfigured
	}
	instID, _, err := extractInstallationAndRepo(payload)
	if err != nil {
		return source.Installation{}, err
	}
	if instID <= 0 {
		return source.Installation{}, errors.New("github source: missing installation.id")
	}
	return r.LookupInstallation(ctx, instID)
}

// Targets resolves the Scion projects targeted by payload under inst,
// including candidate filtering and primary-project routing.
func (r *InstallationResolver) Targets(ctx context.Context, inst source.Installation, payload any) ([]source.Target, error) {
	if r == nil || r.ResolveTargets == nil {
		return nil, ErrNotConfigured
	}
	_, repo, err := extractInstallationAndRepo(payload)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(repo) == "" {
		return nil, errors.New("github source: missing repository.full_name")
	}
	return r.ResolveTargets(ctx, inst, repo)
}

// OnLifecycle handles GitHub installation and installation_repositories
// lifecycle webhooks. Non-lifecycle events return (false, nil).
func (r *InstallationResolver) OnLifecycle(ctx context.Context, d source.Delivery, payload any) (bool, error) {
	if d.Event != EventInstallation && d.Event != EventInstallationRepositories {
		return false, nil
	}
	if r == nil || r.HandleLifecycle == nil {
		return false, ErrNotConfigured
	}
	body, ok := payload.([]byte)
	if !ok {
		return false, ErrUnsupportedPayload
	}
	return r.HandleLifecycle(ctx, d, body)
}

func extractInstallationAndRepo(payload any) (int64, string, error) {
	switch p := payload.(type) {
	case Result:
		return p.InstallationID, p.RepoFullName, nil
	case *Result:
		if p == nil {
			return 0, "", ErrUnsupportedPayload
		}
		return p.InstallationID, p.RepoFullName, nil
	case []byte:
		var env struct {
			Installation struct {
				ID int64 `json:"id"`
			} `json:"installation"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(p, &env); err != nil {
			return 0, "", fmt.Errorf("github source: decode routing envelope: %w", err)
		}
		return env.Installation.ID, env.Repository.FullName, nil
	default:
		return 0, "", ErrUnsupportedPayload
	}
}

// EventNormalizer implements source.Normalizer using GitHub's Normalize function.
type EventNormalizer struct {
	// OptionsFn resolves per-delivery normalisation options (such as AppSlug,
	// MentionAliases, and ReceivedAt). When nil, Options is used.
	OptionsFn func(ctx context.Context, d source.Delivery) Options
	// Options holds static normalisation options used when OptionsFn is nil.
	Options Options
}

// Normalize converts a verified GitHub delivery into a provider-neutral source.Event.
// payload may be []byte (raw JSON webhook body), Result, or *Result.
func (n *EventNormalizer) Normalize(ctx context.Context, d source.Delivery, payload any, t source.Target) (source.Event, error) {
	var res Result
	switch p := payload.(type) {
	case Result:
		res = p
	case *Result:
		if p == nil {
			return source.Event{}, ErrUnsupportedPayload
		}
		res = *p
	case []byte:
		var opts Options
		if n != nil {
			if n.OptionsFn != nil {
				opts = n.OptionsFn(ctx, d)
			} else {
				opts = n.Options
			}
		}
		if opts.ReceivedAt.IsZero() && !d.ReceivedAt.IsZero() {
			opts.ReceivedAt = d.ReceivedAt
		}
		var err error
		res, err = Normalize(d.Event, d.ID, p, opts)
		if err != nil {
			return source.Event{}, err
		}
	default:
		return source.Event{}, ErrUnsupportedPayload
	}

	return resultToSourceEvent(res, t), nil
}

func resultToSourceEvent(res Result, t source.Target) source.Event {
	var senderID string
	if res.SenderID > 0 {
		senderID = strconv.FormatInt(res.SenderID, 10)
	}
	var performedVia string
	if res.PerformedViaAppID > 0 {
		performedVia = strconv.FormatInt(res.PerformedViaAppID, 10)
	}
	return source.Event{
		Handled:      res.Handled,
		SourceKind:   res.SourceKind,
		SourceID:     res.SourceID,
		DeliveryID:   res.DeliveryID,
		DedupKey:     res.DedupKey(t.ProjectID),
		Type:         res.Type,
		Subject:      res.Subject,
		OccurredAt:   res.OccurredAt,
		Attributes:   res.Attributes,
		Lists:        res.Lists,
		Payload:      res.Payload,
		PayloadBytes: res.PayloadBytes,
		Truncated:    res.Truncated,
		Sender: source.Principal{
			Login:             res.SenderLogin,
			ID:                senderID,
			Type:              res.SenderType,
			PerformedViaAppID: performedVia,
		},
	}
}

// AppActorIdentity implements source.ActorIdentity for GitHub's own-App loop guard (§5.7).
type AppActorIdentity struct {
	// Resolve returns the App's slug (or "<slug>[bot]") and numeric App ID.
	// When nil, AppSlug and AppID are used.
	Resolve func(ctx context.Context) (appSlug string, appID int64)
	AppSlug string
	AppID   int64
}

// IsSelf reports whether sender matches this GitHub App's own bot login or
// performed_via_github_app.id.
func (a *AppActorIdentity) IsSelf(ctx context.Context, sender source.Principal) bool {
	if a == nil {
		return false
	}
	slug, appID := a.AppSlug, a.AppID
	if a.Resolve != nil {
		slug, appID = a.Resolve(ctx)
	}
	var performedVia int64
	if s := strings.TrimSpace(sender.PerformedViaAppID); s != "" {
		if parsed, err := strconv.ParseInt(s, 10, 64); err == nil && parsed > 0 {
			performedVia = parsed
		}
	}
	return IsOwnAppActor(sender.Type, sender.Login, slug, performedVia, appID)
}

// TokenMinter implements source.IdentityMinter by delegating to an injected
// project-scoped GitHub installation-token minting function.
type TokenMinter struct {
	MintFunc func(ctx context.Context, t source.Target, scope source.CredentialScope) (source.Credential, error)
}

// Mint mints a short-lived GitHub App installation token for target t and scope.
func (m *TokenMinter) Mint(ctx context.Context, t source.Target, scope source.CredentialScope) (source.Credential, error) {
	if m == nil || m.MintFunc == nil {
		return source.Credential{}, ErrNotConfigured
	}
	return m.MintFunc(ctx, t, scope)
}

// CommentAction implements source.ProviderAction for the github.comment action
// (marker idempotency, per-project comment bucket, and short-lived credentials
// via source.IdentityMinter).
type CommentAction struct {
	// ValidateFunc validates and normalises a github.comment action payload.
	ValidateFunc func(ctx context.Context, req source.ActionValidateRequest) (string, error)
	// AllowRate enforces the per-project outbound comment bucket. Optional; when
	// set, Fire calls it before minting or posting.
	AllowRate func(projectID string) error
	// FireFunc executes the idempotent comment post using the supplied minter.
	FireFunc func(ctx context.Context, req source.ActionFireRequest) (source.ActionFireResult, error)
	// DefaultMinter is used when req.Minter is nil.
	DefaultMinter source.IdentityMinter
}

// Kind returns the canonical action kind ("github.comment").
func (a *CommentAction) Kind() string {
	return ActionKindGitHubComment
}

// Validate validates and normalises a github.comment action payload.
func (a *CommentAction) Validate(ctx context.Context, req source.ActionValidateRequest) (string, error) {
	if a == nil || a.ValidateFunc == nil {
		return "", ErrNotConfigured
	}
	return a.ValidateFunc(ctx, req)
}

// Fire enforces the optional per-project rate bucket, ensures a minter is
// attached, and delegates to FireFunc.
func (a *CommentAction) Fire(ctx context.Context, req source.ActionFireRequest) (source.ActionFireResult, error) {
	if a == nil || a.FireFunc == nil {
		return source.ActionFireResult{}, ErrNotConfigured
	}
	if a.AllowRate != nil {
		if err := a.AllowRate(req.ProjectID); err != nil {
			return source.ActionFireResult{
				Status: "failed",
				Error:  err.Error(),
			}, nil
		}
	}
	if req.Minter == nil {
		req.Minter = a.DefaultMinter
	}
	return a.FireFunc(ctx, req)
}

// ReadinessSnapshot carries the raw GitHub App configuration and fetched GET /app
// metadata evaluated by ReadinessChecker.
type ReadinessSnapshot struct {
	Configured       bool
	WebhooksEnabled  bool
	HasWebhookSecret bool
	AppSlug          string
	Events           []string
	Permissions      map[string]string
	CheckedAt        *time.Time
	Error            string
}

// ReadinessChecker implements source.Readiness for the GitHub App trigger integration.
type ReadinessChecker struct {
	Snapshot func(ctx context.Context) (ReadinessSnapshot, error)
}

// Check evaluates Stage 3 GitHub trigger readiness from the current App snapshot.
func (c *ReadinessChecker) Check(ctx context.Context) (source.Report, error) {
	if c == nil || c.Snapshot == nil {
		return source.Report{}, ErrNotConfigured
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return source.Report{}, err
	}
	return EvaluateReadiness(snap), nil
}

// EvaluateReadiness computes a provider-neutral source.Report from a GitHub App
// ReadinessSnapshot using Stage 3's required events and permissions tables (§10.1).
func EvaluateReadiness(snap ReadinessSnapshot) source.Report {
	lastErr := snap.Error
	if !snap.Configured && lastErr == "" {
		lastErr = "GitHub App is not configured."
	}

	missingEvents := []string{}
	missingPerms := []string{}
	if snap.Configured && (len(snap.Events) > 0 || len(snap.Permissions) > 0) {
		eventSet := make(map[string]bool, len(snap.Events))
		for _, ev := range snap.Events {
			eventSet[ev] = true
		}
		for _, reqEv := range RequiredEvents {
			if !eventSet[reqEv] {
				missingEvents = append(missingEvents, reqEv)
			}
		}
		for _, reqPerm := range RequiredPermissions {
			if !permissionSatisfies(snap.Permissions[reqPerm.Name], reqPerm.Level) {
				missingPerms = append(missingPerms, reqPerm.Name+":"+reqPerm.Level)
			}
		}
	}

	hasMetadata := snap.Configured && (len(snap.Events) > 0 || len(snap.Permissions) > 0)
	ready := snap.WebhooksEnabled &&
		snap.HasWebhookSecret &&
		hasMetadata &&
		lastErr == "" &&
		len(missingEvents) == 0 &&
		len(missingPerms) == 0

	return source.Report{
		Ready:              ready,
		WebhooksEnabled:    snap.WebhooksEnabled,
		HasWebhookSecret:   snap.HasWebhookSecret,
		MissingEvents:      missingEvents,
		MissingPermissions: missingPerms,
		AppSlug:            snap.AppSlug,
		CheckedAt:          snap.CheckedAt,
		Error:              lastErr,
	}
}

func permissionSatisfies(appLevel, requiredLevel string) bool {
	switch requiredLevel {
	case "read":
		return appLevel == "read" || appLevel == "write" || appLevel == "admin"
	case "write":
		return appLevel == "write" || appLevel == "admin"
	default:
		return appLevel == requiredLevel
	}
}

// PresetCatalog implements source.Presets over GitHub's trigger preset registry.
type PresetCatalog struct{}

// List returns all GitHub trigger presets in provider-neutral source.Preset form.
func (c *PresetCatalog) List() []source.Preset {
	raw := Presets(SourceKind)
	out := make([]source.Preset, len(raw))
	for i, p := range raw {
		fields := make([]source.PresetField, len(p.Fields))
		for j, f := range p.Fields {
			fields[j] = source.PresetField{
				Name:     f.Name,
				Type:     f.Type,
				Enum:     f.Enum,
				Default:  f.Default,
				Required: f.Required,
				Help:     f.Help,
			}
		}
		out[i] = source.Preset{
			ID:          p.ID,
			Source:      p.Source,
			Version:     p.Version,
			Title:       p.Title,
			Description: p.Description,
			Fields:      fields,
			ActionKinds: p.ActionKinds,
		}
	}
	return out
}

// Compile compiles preset p with caller-supplied fields into a provider-neutral
// source.CompiledMatch and source.SuggestedAction.
func (c *PresetCatalog) Compile(p source.Preset, fields map[string]any) (source.CompiledMatch, source.SuggestedAction, error) {
	compiled, err := CompilePreset(CompilePresetRequest{
		Preset: p.ID,
		Fields: fields,
	})
	if err != nil {
		return source.CompiledMatch{}, source.SuggestedAction{}, err
	}

	rules := make([]source.CompiledRule, len(compiled.Match.Rules))
	for i, r := range compiled.Match.Rules {
		rules[i] = source.CompiledRule{
			Dialect: r.Dialect,
			Expr:    r.Expr,
		}
	}
	match := source.CompiledMatch{
		Types: append([]string(nil), compiled.Match.Types...),
		Rules: rules,
	}

	var suggested source.SuggestedAction
	if compiled.SuggestedAction != nil {
		suggested = source.SuggestedAction{
			Type:   compiled.SuggestedAction.Type,
			Fields: suggestedActionFields(compiled.SuggestedAction),
		}
	}
	return match, suggested, nil
}

func suggestedActionFields(sa *SuggestedAction) map[string]any {
	if sa == nil {
		return nil
	}
	fields := map[string]any{
		"template":      sa.Template,
		"task":          sa.Task,
		"ephemeral":     sa.Ephemeral,
		"githubToken":   sa.GitHubToken,
		"untrustedVars": append([]string(nil), sa.UntrustedVars...),
	}
	if sa.AgentName != "" {
		fields["agentName"] = sa.AgentName
	}
	if sa.Branch != "" {
		fields["branch"] = sa.Branch
	}
	if len(sa.Labels) > 0 {
		cp := make(map[string]string, len(sa.Labels))
		for k, v := range sa.Labels {
			cp[k] = v
		}
		fields["labels"] = cp
	}
	if sa.ReuseMessage != "" {
		fields["reuseMessage"] = sa.ReuseMessage
	}
	if sa.AckComment != nil {
		fields["ackComment"] = map[string]any{"body": sa.AckComment.Body}
	}
	return fields
}
