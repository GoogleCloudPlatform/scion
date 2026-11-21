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

package source

import (
	"context"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
)

// SecretSource resolves a hub-held webhook signing secret for a provider.
type SecretSource interface {
	WebhookSecret(ctx context.Context) (string, error)
}

// SecretFunc adapts a function to SecretSource.
type SecretFunc func(ctx context.Context) (string, error)

// WebhookSecret calls f(ctx).
func (f SecretFunc) WebhookSecret(ctx context.Context) (string, error) {
	return f(ctx)
}

// Delivery holds provider-neutral metadata extracted from a verified incoming
// webhook delivery.
type Delivery struct {
	// ID is the provider's delivery identifier (e.g. X-GitHub-Delivery).
	ID string
	// TargetID is the provider app or target identifier from request headers
	// (e.g. X-GitHub-Hook-Installation-Target-ID), or "" if the provider has none.
	TargetID string
	// TargetType is the optional provider target classification header
	// (e.g. "integration" for GitHub Apps).
	TargetType string
	// Event is the provider's event family name (e.g. "issue_comment", "installation").
	Event string
	// Headers holds allow-listed delivery headers.
	Headers map[string]string
	// ReceivedAt records when the hub received the delivery.
	ReceivedAt time.Time
}

// WireVerifier encapsulates the wire semantics of one provider's shared webhook route.
type WireVerifier interface {
	// Verify authenticates the raw delivery with the hub-held provider secret and
	// returns the provider delivery ID, app/target ID (or "" if none), and event name.
	Verify(r *http.Request, body []byte, secrets SecretSource) (Delivery, error)
	// DedupKey returns a deterministic provider-level deduplication key for a
	// verified delivery (e.g. GitHub: delivery ID + sha256(body); Jira: hash of
	// event, issue.id, timestamp, comment/changelog id).
	DedupKey(d Delivery, body []byte) string
}

// Installation is the provider-neutral view of an external app installation or tenant.
type Installation struct {
	// ID is the external installation or tenant identifier (e.g. GitHub
	// installation.id formatted as a decimal string, or a Jira site/cloud ID).
	ID string
	// Account is the external account, organization, or site name.
	Account string
	// Active reports whether the installation is currently active and eligible
	// to route events.
	Active bool
	// Routing holds optional primary-project routing overrides keyed by external
	// resource identifier (e.g. lowercased "owner/repo" -> primary project ID).
	Routing map[string]string
}

// Target identifies one Scion project targeted by a delivery during fan-out.
type Target struct {
	// ProjectID is the Scion project ID.
	ProjectID string
	// InstallationID is the external installation or tenant ID bound to this target.
	InstallationID string
	// Resource is the external resource identifier (e.g. "owner/repo" for GitHub,
	// project key for Jira).
	Resource string
	// RoutingStatus records primary-routing disposition ("none", "ok", "stale_primary").
	RoutingStatus string
	// BypassedProjectIDs lists candidate project IDs bypassed by primary routing, if any.
	BypassedProjectIDs []string
}

// InstallationResolver maps an external installation/tenant to Scion projects
// and handles installation lifecycle events.
type InstallationResolver interface {
	// Installation resolves the external installation/tenant referenced by the delivery.
	Installation(ctx context.Context, d Delivery, payload any) (Installation, error)
	// Targets resolves the Scion projects targeted by the delivery under inst,
	// applying any configured primary-project routing.
	Targets(ctx context.Context, inst Installation, payload any) ([]Target, error)
	// OnLifecycle handles installation lifecycle deliveries (created/deleted/suspended,
	// scopes added/removed). When handled is true, the delivery is a lifecycle event
	// and must never enqueue a trigger event.
	OnLifecycle(ctx context.Context, d Delivery, payload any) (handled bool, err error)
}

// Principal identifies an actor on the external provider.
type Principal struct {
	// Login is the provider username or bot handle (e.g. "scion[bot]").
	Login string
	// ID is the provider's stable actor identifier (e.g. decimal user/account ID).
	ID string
	// Type classifies the actor (e.g. "User", "Bot", "Organization").
	Type string
	// PerformedViaAppID is the external app ID recorded on an object-creating
	// delivery when the provider attributes creation to an app.
	PerformedViaAppID string
}

// Event is the provider-neutral normalised trigger event produced by a Normalizer.
type Event struct {
	// ID is the persisted trigger event row identifier when known.
	ID string
	// Handled is false when the delivery's event family is outside the source's
	// supported event table.
	Handled bool
	// SourceKind identifies the provider (e.g. "github", "jira").
	SourceKind string
	// SourceID identifies the installation/tenant (e.g. decimal installation ID).
	SourceID string
	// DeliveryID is the provider delivery identifier.
	DeliveryID string
	// DedupKey is the per-target deduplication key (e.g. "<delivery>/<project_id>").
	DedupKey string
	// Type is the canonical trigger event type (e.g. "github.issue_comment.created").
	Type string
	// Subject is the canonical subject key (e.g. "github:owner/repo#42").
	Subject string
	// OccurredAt is when the event occurred on the provider.
	OccurredAt time.Time
	// ReceivedAt is when the hub received the delivery.
	ReceivedAt time.Time
	// Attributes holds normalised string attributes.
	Attributes map[string]string
	// Lists holds normalised list-valued attributes.
	Lists map[string][]string
	// Payload holds the bounded projected JSON payload.
	Payload []byte
	// PayloadBytes is the byte length of Payload.
	PayloadBytes int
	// Truncated reports whether prose fields were truncated to fit size bounds.
	Truncated bool
	// Sender identifies the provider actor who triggered the event.
	Sender Principal
}

// Normalizer converts a verified provider delivery into a provider-neutral Event.
type Normalizer interface {
	Normalize(ctx context.Context, d Delivery, payload any, t Target) (Event, error)
}

// Enricher is the drainer-side source enrichment hook consulted once per event
// before rule evaluation (Stage 3 §5.5, app-shaped-source-seams.md §2).
// pkg/hub/trigger.SourceEnricher aliases this interface.
type Enricher interface {
	// NeedsEnrichment reports whether ev lacks facts the source can fetch.
	NeedsEnrichment(ev *hubfire.FireEvent) bool
	// Enrich fetches and returns the attribute/list updates; the dispatcher
	// persists them on the event row before evaluating any rule, so rules,
	// history, the envelope, and the agent file all see the same facts.
	Enrich(ctx context.Context, ev *hubfire.FireEvent) (attrs map[string]string, lists map[string][]string, err error)
}

// ActorIdentity identifies whether a provider actor is the hub's own app,
// powering the own-actor loop guard.
type ActorIdentity interface {
	IsSelf(ctx context.Context, sender Principal) bool
}

// CredentialScope describes the least-privilege permissions requested when
// minting a short-lived provider credential for a target.
type CredentialScope struct {
	// Permissions maps provider permission names to requested levels
	// (e.g. {"issues": "write", "metadata": "read"} for GitHub).
	Permissions map[string]string
}

// Credential is a short-lived, target-scoped provider credential returned by
// an IdentityMinter. Tokens must remain in memory only and never be logged.
type Credential struct {
	Token     string
	ExpiresAt time.Time
	// Scope records the granted permission scope, if known.
	Scope map[string]string
}

// IdentityMinter mints short-lived, target-scoped credentials for calling back
// into the external provider as the app.
type IdentityMinter interface {
	Mint(ctx context.Context, t Target, scope CredentialScope) (Credential, error)
}

// ActionValidateRequest carries the inputs to ProviderAction.Validate.
type ActionValidateRequest struct {
	ProjectID string
	Payload   string
}

// ActionFireRequest carries the inputs to ProviderAction.Fire.
type ActionFireRequest struct {
	ProjectID string
	TriggerID string
	FireID    string
	Payload   string
	Event     Event
	Target    Target
	Attempts  int
	Minter    IdentityMinter
}

// ActionFireResult summarizes the outcome of ProviderAction.Fire.
type ActionFireResult struct {
	// Status is the provider-neutral fire status ("fired", "refused", "failed").
	Status string
	// ResultRef is the provider's created resource reference (e.g. comment ID).
	ResultRef string
	// Error is a content-free failure or refusal reason, if any.
	Error string
}

// ProviderAction is the base interface for provider-backed outbound actions
// (such as github.comment or jira.comment) that enforce idempotency markers,
// per-project rate buckets, and short-lived credentials via IdentityMinter.
//
// Invariant (app-shaped-source-seams.md §2.1): ProviderAction.Fire is only
// ever invoked by the trigger dispatcher inside a leased fire row so marker
// idempotency, attempts, and once-write outcome bookkeeping are owned in one
// place with no second entry point. GitHub's github.comment remains
// implemented by the registered FireAction in pkg/hub/action_github_comment.go
// until G7 re-homes it onto this interface when the shared ingress skeleton
// moves and the dispatcher's action registry is the sole caller.
type ProviderAction interface {
	Kind() string
	Validate(ctx context.Context, req ActionValidateRequest) (string, error)
	Fire(ctx context.Context, req ActionFireRequest) (ActionFireResult, error)
}

// Report summarizes whether a provider integration is configured with the
// webhook settings, event subscriptions, and permissions required for triggers.
type Report struct {
	Ready              bool       `json:"ready"`
	WebhooksEnabled    bool       `json:"webhooks_enabled"`
	HasWebhookSecret   bool       `json:"has_webhook_secret"`
	MissingEvents      []string   `json:"missing_events"`
	MissingPermissions []string   `json:"missing_permissions"`
	AppSlug            string     `json:"app_slug"`
	CheckedAt          *time.Time `json:"checked_at,omitempty"`
	Error              string     `json:"error,omitempty"`
}

// Readiness reports whether a provider app integration is ready to deliver
// and act on trigger events.
type Readiness interface {
	Check(ctx context.Context) (Report, error)
}

// PresetField describes one configurable field in a Preset's form schema.
type PresetField struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Enum     []string `json:"enum,omitempty"`
	Default  any      `json:"default,omitempty"`
	Required bool     `json:"required"`
	Help     string   `json:"help"`
}

// Preset describes a server-side trigger rule preset schema.
type Preset struct {
	ID          string        `json:"id"`
	Source      string        `json:"source"`
	Version     int           `json:"version"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Fields      []PresetField `json:"fields"`
	ActionKinds []string      `json:"action_kinds"`
}

// CompiledRule is one dialect-tagged rule element emitted by Presets.Compile.
type CompiledRule struct {
	Dialect string `json:"dialect"`
	Expr    string `json:"expr"`
}

// CompiledMatch holds the event-type prefilter and ordered match rules emitted
// by Presets.Compile.
type CompiledMatch struct {
	Types []string       `json:"types"`
	Rules []CompiledRule `json:"rules"`
}

// SuggestedAction holds the provider-neutral suggested action payload emitted
// by Presets.Compile.
type SuggestedAction struct {
	Type   string         `json:"type"`
	Fields map[string]any `json:"fields,omitempty"`
}

// Presets exposes a provider's trigger rule preset catalog and compiler.
type Presets interface {
	List() []Preset
	Compile(p Preset, fields map[string]any) (CompiledMatch, SuggestedAction, error)
}
