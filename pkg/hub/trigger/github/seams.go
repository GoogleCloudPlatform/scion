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

// Compile-time assertions that GitHub's adapters satisfy the app-shaped
// source seam interfaces in pkg/hub/trigger/source (app-shaped-source-seams.md §2).
// source.ProviderAction is intentionally omitted here per §2.1: GitHub's
// github.comment remains implemented by the registered FireAction in
// pkg/hub/action_github_comment.go until G7 re-homes it behind the dispatcher.
var (
	_ source.WireVerifier         = (*WireVerifier)(nil)
	_ source.InstallationResolver = (*InstallationResolver)(nil)
	_ source.Normalizer           = (*EventNormalizer)(nil)
	_ source.ActorIdentity        = (*AppActorIdentity)(nil)
	_ source.IdentityMinter       = (*TokenMinter)(nil)
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

// GitHub lifecycle and ping webhook event names handled outside content-event routing.
const (
	EventPing                     = "ping"
	EventInstallation             = "installation"
	EventInstallationRepositories = "installation_repositories"
)

// VerifyWebhookSignature validates a GitHub webhook payload signature using
// X-Hub-Signature-256 ("sha256=<lowercase-hex>"). This is the single canonical
// GitHub webhook HMAC-SHA256 verifier shared with pkg/hub/githubapp.
func VerifyWebhookSignature(payload []byte, signature string, secret string) bool {
	if secret == "" || signature == "" {
		return false
	}

	const prefix = "sha256="
	if !strings.HasPrefix(signature, prefix) {
		return false
	}
	sigHex := signature[len(prefix):]

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expectedMAC := mac.Sum(nil)
	expectedHex := fmt.Sprintf("%x", expectedMAC)

	return hmac.Equal([]byte(sigHex), []byte(expectedHex))
}

// WebhookTargetMatches checks Stage 3 §4.1 step 1: configured appID > 0,
// exactly one X-GitHub-Hook-Installation-Target-Type header equal to
// "integration", and exactly one X-GitHub-Hook-Installation-Target-ID header
// equal to strconv.FormatInt(appID, 10). Shared by production webhook ingress
// and WireVerifier.
func WebhookTargetMatches(h http.Header, appID int64) bool {
	if appID <= 0 || h == nil {
		return false
	}
	targetTypes := h.Values("X-GitHub-Hook-Installation-Target-Type")
	targetIDs := h.Values("X-GitHub-Hook-Installation-Target-ID")
	return len(targetTypes) == 1 && targetTypes[0] == "integration" &&
		len(targetIDs) == 1 && targetIDs[0] == strconv.FormatInt(appID, 10)
}

// IsDeliveryGUID reports whether s is a 36-byte canonical UUID/GUID string
// (8-4-4-4-12 hex, case-insensitive) as required by Stage 3 §4.1 step 2.
// Shared by production webhook ingress and WireVerifier.
func IsDeliveryGUID(s string) bool {
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

// WireVerifier implements source.WireVerifier for GitHub App webhooks.
type WireVerifier struct {
	// ExpectedAppIDFn returns the hub's currently configured GitHub App ID on
	// each request. When non-nil it takes precedence over ExpectedAppID so a
	// long-lived WireVerifier observes live AppID updates.
	ExpectedAppIDFn func() int64
	// ExpectedAppID is the static GitHub App ID used when ExpectedAppIDFn is
	// nil. For content events, a resolved AppID <= 0 fails closed with
	// ErrWebhookTargetMismatch, matching production handleGitHubContentWebhook
	// step 1.
	ExpectedAppID int64
	// Now returns the delivery receipt timestamp (defaults to time.Now).
	Now func() time.Time
}

// Verify authenticates a GitHub webhook request using the canonical
// VerifyWebhookSignature and enforces production's target-id, event-header, and
// delivery-id checks (Stage 3 §4.1 steps 1–2) for content events.
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
	if !VerifyWebhookSignature(body, sig, secret) {
		return source.Delivery{}, ErrInvalidWebhookSignature
	}

	eventName := r.Header.Get("X-GitHub-Event")
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	targetType := r.Header.Get("X-GitHub-Hook-Installation-Target-Type")
	targetID := r.Header.Get("X-GitHub-Hook-Installation-Target-ID")

	isNonContent := eventName == EventPing ||
		eventName == EventInstallation ||
		eventName == EventInstallationRepositories

	if !isNonContent {
		// Step 1: target-id check fails closed when resolved AppID <= 0.
		var expectedAppID int64
		if v != nil {
			if v.ExpectedAppIDFn != nil {
				expectedAppID = v.ExpectedAppIDFn()
			} else {
				expectedAppID = v.ExpectedAppID
			}
		}
		if !WebhookTargetMatches(r.Header, expectedAppID) {
			return source.Delivery{}, ErrWebhookTargetMismatch
		}

		// Step 2: single X-GitHub-Event header, and for supported families a
		// single GUID-shaped X-GitHub-Delivery header. An empty single
		// X-GitHub-Event value passes here with IsSupportedEvent("") == false,
		// matching production's 200 ignored response.
		if len(r.Header.Values("X-GitHub-Event")) != 1 {
			return source.Delivery{}, ErrInvalidWebhookHeaders
		}
		if IsSupportedEvent(eventName) {
			if len(r.Header.Values("X-GitHub-Delivery")) != 1 || !IsDeliveryGUID(deliveryID) {
				return source.Delivery{}, ErrInvalidDeliveryID
			}
			deliveryID = strings.ToLower(deliveryID)
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

// DedupKey returns a deterministic provider-level deduplication key in the
// format "<lowercased-delivery-id>:<sha256-hex(body)>" (or "<sha256-hex(body)>"
// when d.ID is empty). This is a provider-level key combining both the delivery
// ID and signed body hash; per-project persistence continues to store
// Result.DedupKey(projectID) ("<delivery>/<project>") alongside the body SHA-256
// column (Stage 3 rev 1.8 §4.3).
func (v *WireVerifier) DedupKey(d source.Delivery, body []byte) string {
	sum := sha256.Sum256(body)
	bodyHash := hex.EncodeToString(sum[:])
	id := strings.ToLower(strings.TrimSpace(d.ID))
	if id == "" {
		return bodyHash
	}
	return id + ":" + bodyHash
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

	return resultToSourceEvent(res, d.ReceivedAt, t), nil
}

func resultToSourceEvent(res Result, receivedAt time.Time, t source.Target) source.Event {
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
		ReceivedAt:   receivedAt,
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

// ReadinessChecker implements source.Readiness by delegating to the hub's
// canonical GitHub App trigger readiness evaluator.
type ReadinessChecker struct {
	CheckFunc func(ctx context.Context) (source.Report, error)
}

// Check evaluates Stage 3 GitHub trigger readiness.
func (c *ReadinessChecker) Check(ctx context.Context) (source.Report, error) {
	if c == nil || c.CheckFunc == nil {
		return source.Report{}, ErrNotConfigured
	}
	return c.CheckFunc(ctx)
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
