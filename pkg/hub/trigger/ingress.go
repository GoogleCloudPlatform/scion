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

package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/apierr"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Ingress payload, header, attribute, and queue bounds (§4.3, §6.1, §8.1, §8.2; Stage 2 §7.4).
const (
	MaxPayloadBytes = 64 * 1024
	// MaxEndpointBodyBytes is the upper bound on per-endpoint max_body_bytes
	// (1 MiB, Stage 2 §8). Bodies <= MaxPayloadBytes (64 KiB) are stored inline;
	// bodies > MaxPayloadBytes are uploaded to hub-private object storage.
	MaxEndpointBodyBytes       = 1024 * 1024
	MaxAttributesBytes         = 8 * 1024
	MaxDeliveryBytes           = 128
	MaxEventBytes              = 64
	MaxSubjectBytes            = 256
	MaxPendingEventsPerProject = 5000

	maxStoredUserAgentBytes   = 512
	maxStoredContentTypeBytes = 256

	IngressStatusQueued    = "queued"
	IngressStatusDuplicate = "duplicate"

	hooksPathPrefix = "/api/v1/hooks/"
)

var (
	// HooksUnauthPathPattern is the exact unauthenticated route pattern of
	// §8.1 step 1: ^/api/v1/hooks/[0-9a-f-]{36}$. Trailing subpaths such as
	// /api/v1/hooks/x/y never match it and therefore require authentication.
	HooksUnauthPathPattern = regexp.MustCompile(`^/api/v1/hooks/[0-9a-f-]{36}$`)

	// eventNamePattern validates X-Scion-Event per §8.1 step 7.
	eventNamePattern = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)

	// allowedEventHeaders is the fixed §4.3 request-header allow-list stored on
	// trigger_events.headers. Secret headers such as X-Scion-Signature,
	// X-Hub-Signature-256, Stripe-Signature, and Authorization are never stored.
	allowedEventHeaders = []string{
		"Content-Type",
		"User-Agent",
		HeaderEvent,
		HeaderDelivery,
		HeaderSubject,
		HeaderGitHubEvent,
		HeaderGitHubDelivery,
	}

	errQueueFull                    = errors.New("trigger event queue full for project")
	errDuplicateDelivery            = errors.New("duplicate webhook delivery")
	errTxMissingQueuePendingCounter = errors.New("transaction store does not implement DurableQueuePendingCounter")
)

// IsHooksRequestPath reports whether path matches ^/api/v1/hooks/[0-9a-f-]{36}$
// exactly (§8.1 step 1).
func IsHooksRequestPath(path string) bool {
	return HooksUnauthPathPattern.MatchString(path)
}

// isCanonicalLowerUUID reports whether s is a valid 36-byte lowercase
// 8-4-4-4-12 UUID string.
func isCanonicalLowerUUID(s string) bool {
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
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return false
			}
		}
	}
	return uuid.Validate(s) == nil
}

// isPrintableString reports whether s is valid UTF-8 whose runes all satisfy
// unicode.IsPrint (rejecting NUL, CR, LF, TAB, DEL, and other control/format
// runes).
func isPrintableString(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// sanitizeHeaderForStorage strips NUL bytes and invalid UTF-8 and truncates on
// a UTF-8 rune boundary at maxBytes so stored header metadata can never fail
// PostgreSQL TEXT constraints.
func sanitizeHeaderForStorage(val string, maxBytes int) string {
	val = strings.ToValidUTF8(val, "")
	val = strings.ReplaceAll(val, "\x00", "")
	if len(val) <= maxBytes {
		return val
	}
	for maxBytes > 0 && !utf8.RuneStart(val[maxBytes]) {
		maxBytes--
	}
	return val[:maxBytes]
}

// IngressAcceptedResponse is the HTTP 202 response body of POST /api/v1/hooks/{id} (§8.1 step 10).
type IngressAcceptedResponse struct {
	EventID string `json:"eventId"`
	Status  string `json:"status"`
}

// Closed-set ingress metric result labels (§6.5). Every ingress outcome maps
// onto one of these constants so metric label cardinality stays bounded.
const (
	IngressResultAccepted                  = "accepted"
	IngressResultAcceptedOverflow          = "accepted_overflow"
	IngressResultDuplicate                 = "duplicate"
	IngressResultRejectedSignature         = "rejected_signature"
	IngressResultRejectedAuth              = "rejected_auth"
	IngressResultRejectedRateLimit         = "rejected_rate_limit"
	IngressResultRejectedSize              = "rejected_size"
	IngressResultRejectedUnknownEndpoint   = "rejected_unknown_endpoint"
	IngressResultRejectedSecretUnavailable = "rejected_secret_unavailable"
	IngressResultRejectedMediaType         = "rejected_media_type"
	IngressResultRejectedBadJSON           = "rejected_bad_json"
	IngressResultRejectedBadHeader         = "rejected_bad_header"
	IngressResultRejectedQueueFull         = "rejected_queue_full"
	IngressResultFailedStore               = "failed_store"
	IngressResultFailedStorage             = "failed_storage"
)

// IngressMetrics observes trigger ingress outcomes by bounded source_kind and
// result labels.
type IngressMetrics interface {
	RecordIngressEvent(sourceKind, result string)
}

// IngressPayloadMetrics is an optional extension of IngressMetrics that
// observes accepted webhook payload sizes in bytes (Stage 2 §8).
type IngressPayloadMetrics interface {
	ObserveIngressPayloadBytes(sourceKind string, bytes int)
}

// IngressCounts snapshots running request outcome counters for tests and metrics.
type IngressCounts struct {
	Accepted          int64
	AcceptedOverflow  int64
	Duplicate         int64
	RejectedRateLimit int64
	RejectedNotFound  int64
	RejectedTooLarge  int64
	RejectedMediaType int64
	RejectedBadJSON   int64
	RejectedBadSig    int64
	RejectedBadHeader int64
	RejectedQueueFull int64
	FailedSecret      int64
	FailedStore       int64
	FailedStorage     int64
}

// ClientIPFunc resolves the client IP address of r using the hub's trusted
// proxy configuration (trustedClientIP in package hub).
type ClientIPFunc func(r *http.Request) string

// HubTokenAuthResult describes the outcome of authorizing a hub_token webhook
// ingress request in package hub (Stage 2 §7.1).
type HubTokenAuthResult struct {
	Authenticated bool   // false -> 401 content-free (no identity on request context)
	Allowed       bool   // false (when Authenticated) -> 403 content-free
	Sender        string // "<principal kind>:<id>" recorded in attributes.sender
}

// HubTokenAuthorizer decides whether the request context's authenticated hub
// identity may send to ep (webhookEndpointAccessDecision(ctx, ep.ProjectID, ActionSend, ep)).
type HubTokenAuthorizer func(r *http.Request, ep *store.WebhookEndpoint) HubTokenAuthResult

// EffectiveMaxBodyBytes returns the bounded body-size limit for an endpoint's
// configured max_body_bytes (0 -> store.DefaultWebhookMaxBodyBytes (64 KiB);
// clamped to <= MaxEndpointBodyBytes (1 MiB)).
func EffectiveMaxBodyBytes(configured int) int {
	if configured <= 0 {
		return store.DefaultWebhookMaxBodyBytes
	}
	if configured > MaxEndpointBodyBytes {
		return MaxEndpointBodyBytes
	}
	return configured
}

// IngressTxStore is the minimal transactional TriggerStore interface required
// by Ingress (satisfied by store.Store / *entadapter.CompositeStore).
type IngressTxStore interface {
	store.TriggerStore
	WithTx(ctx context.Context, fn func(tx store.Store) error) error
}

// IngressConfig configures the generic webhook ingress handler.
type IngressConfig struct {
	Store                      IngressTxStore
	QueueStore                 store.DurableQueueStore
	Queue                      durablequeue.Queue
	Storage                    func() storage.Storage
	MaxOverflowBytesPerProject int64
	OverflowUploadTimeout      time.Duration
	OverflowTxTimeout          time.Duration
	LoadSecret                 SecretLoader
	VerifyGitHubSignature      GitHubSignatureVerifyFunc
	AuthorizeHubToken          HubTokenAuthorizer
	ClientIP                   ClientIPFunc
	Enabled                    func() bool
	Limiter                    *IngressLimiter
	Cache                      *EndpointCache
	Counters                   *CounterAccumulator
	Audit                      AuditSink
	Metrics                    IngressMetrics
	Logger                     *slog.Logger
	Now                        func() time.Time
	NewEventID                 func() string
}

// Ingress handles dual-posture POST /api/v1/hooks/{id} requests per Stage 1
// §8.1, §8.1a, §8.2, §6.1, §6.2 and Stage 2 §7.1, §7.3, §7.4, §8.
type Ingress struct {
	store                 IngressTxStore
	queueStore            store.DurableQueueStore
	queue                 durablequeue.Queue
	storageFn             func() storage.Storage
	maxOverflowBytes      int64
	overflowUploadTimeout time.Duration
	overflowTxTimeout     time.Duration
	loadSecret            SecretLoader
	verifyGitHubSignature GitHubSignatureVerifyFunc
	authorizeHubToken     HubTokenAuthorizer
	clientIP              ClientIPFunc
	enabled               func() bool
	limiter               *IngressLimiter
	cache                 *EndpointCache
	counters              *CounterAccumulator
	audit                 AuditSink
	metrics               IngressMetrics
	logger                *slog.Logger
	now                   func() time.Time
	newEventID            func() string

	accepted          atomic.Int64
	acceptedOverflow  atomic.Int64
	duplicate         atomic.Int64
	rejectedRateLimit atomic.Int64
	rejectedNotFound  atomic.Int64
	rejectedTooLarge  atomic.Int64
	rejectedMediaType atomic.Int64
	rejectedBadJSON   atomic.Int64
	rejectedBadSig    atomic.Int64
	rejectedBadHeader atomic.Int64
	rejectedQueueFull atomic.Int64
	failedSecret      atomic.Int64
	failedStore       atomic.Int64
	failedStorage     atomic.Int64
}

// NewIngress constructs an Ingress handler with Stage 1 defaults for any
// omitted helper components.
func NewIngress(cfg IngressConfig) *Ingress {
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	limiter := cfg.Limiter
	if limiter == nil {
		limiter = NewIngressLimiter(LimiterConfig{Now: nowFn})
	}
	cache := cfg.Cache
	if cache == nil {
		cache = NewEndpointCache(EndpointCacheConfig{Now: nowFn})
	}
	counters := cfg.Counters
	if counters == nil {
		counters = NewCounterAccumulator(CounterFlushInterval)
	}
	audit := cfg.Audit
	if audit == nil {
		audit = NewSlogAuditSink(SlogAuditConfig{Now: nowFn})
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clientIP := cfg.ClientIP
	if clientIP == nil {
		clientIP = defaultClientIP
	}
	newEventID := cfg.NewEventID
	if newEventID == nil {
		newEventID = uuid.NewString
	}
	maxOverflowBytes := cfg.MaxOverflowBytesPerProject
	if maxOverflowBytes <= 0 {
		maxOverflowBytes = MaxOverflowPayloadBytesPerProject
	}
	uploadTimeout := cfg.OverflowUploadTimeout
	if uploadTimeout <= 0 {
		uploadTimeout = DefaultOverflowUploadTimeout
	}
	txTimeout := cfg.OverflowTxTimeout
	if txTimeout <= 0 {
		txTimeout = DefaultOverflowTxTimeout
	}
	return &Ingress{
		store:                 cfg.Store,
		queueStore:            cfg.QueueStore,
		queue:                 cfg.Queue,
		storageFn:             cfg.Storage,
		maxOverflowBytes:      maxOverflowBytes,
		overflowUploadTimeout: uploadTimeout,
		overflowTxTimeout:     txTimeout,
		loadSecret:            cfg.LoadSecret,
		verifyGitHubSignature: cfg.VerifyGitHubSignature,
		authorizeHubToken:     cfg.AuthorizeHubToken,
		clientIP:              clientIP,
		enabled:               cfg.Enabled,
		limiter:               limiter,
		cache:                 cache,
		counters:              counters,
		audit:                 audit,
		metrics:               cfg.Metrics,
		logger:                logger,
		now:                   nowFn,
		newEventID:            newEventID,
	}
}

func (h *Ingress) getStorage() storage.Storage {
	if h == nil || h.storageFn == nil {
		return nil
	}
	return h.storageFn()
}

func (h *Ingress) recordMetric(result string) {
	if h != nil && h.metrics != nil {
		h.metrics.RecordIngressEvent(store.TriggerSourceKindWebhook, result)
	}
}

func (h *Ingress) recordPayloadBytes(n int) {
	if h == nil || h.metrics == nil {
		return
	}
	if pm, ok := h.metrics.(IngressPayloadMetrics); ok {
		pm.ObserveIngressPayloadBytes(store.TriggerSourceKindWebhook, n)
	}
}

func defaultClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			return v4.String()
		}
		return ip.String()
	}
	return host
}

// Counts returns a snapshot of the ingress handler's running request counters.
func (h *Ingress) Counts() IngressCounts {
	return IngressCounts{
		Accepted:          h.accepted.Load(),
		AcceptedOverflow:  h.acceptedOverflow.Load(),
		Duplicate:         h.duplicate.Load(),
		RejectedRateLimit: h.rejectedRateLimit.Load(),
		RejectedNotFound:  h.rejectedNotFound.Load(),
		RejectedTooLarge:  h.rejectedTooLarge.Load(),
		RejectedMediaType: h.rejectedMediaType.Load(),
		RejectedBadJSON:   h.rejectedBadJSON.Load(),
		RejectedBadSig:    h.rejectedBadSig.Load(),
		RejectedBadHeader: h.rejectedBadHeader.Load(),
		RejectedQueueFull: h.rejectedQueueFull.Load(),
		FailedSecret:      h.failedSecret.Load(),
		FailedStore:       h.failedStore.Load(),
		FailedStorage:     h.failedStorage.Load(),
	}
}

// InvalidateEndpoint evicts endpointID and its cached secret versions from the
// per-replica cache (e.g. upon local endpoint deletion).
func (h *Ingress) InvalidateEndpoint(endpointID string) {
	if h == nil || h.cache == nil {
		return
	}
	h.cache.Invalidate(endpointID)
}

// FlushCounters synchronously flushes accumulated receive/reject counters to storage.
func (h *Ingress) FlushCounters(ctx context.Context) error {
	if h == nil || h.counters == nil || h.store == nil {
		return nil
	}
	return h.counters.Flush(ctx, h.store)
}

// StartBackground starts the 10-second endpoint counter flush loop until ctx is
// cancelled. Keyed rate-limiter, cache, and audit tables enforce capacity on
// demand in O(1) without background sweep goroutines.
func (h *Ingress) StartBackground(ctx context.Context) {
	if h == nil {
		return
	}
	if h.counters != nil && h.store != nil {
		h.counters.StartLoop(ctx, h.store, h.logger)
	}
}

// Close flushes any accumulated endpoint counters on server shutdown (idempotent).
func (h *Ingress) Close(ctx context.Context) {
	if h == nil || h.counters == nil || h.store == nil {
		return
	}
	h.counters.Close(ctx, h.store)
}

// ServeHTTP executes the 10-step ingress pipeline of §8.1.
func (h *Ingress) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Step 0: experiment gate (also enforced by Server.handleWebhookIngress).
	if h == nil || (h.enabled != nil && !h.enabled()) {
		http.NotFound(w, r)
		return
	}
	if h.store == nil || h.queueStore == nil || h.queue == nil {
		apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook ingress is not available", nil)
		return
	}

	// Step 1: method POST + exact ^/api/v1/hooks/[0-9a-f-]{36}$ canonical UUID check.
	if r.Method != http.MethodPost {
		apierr.MethodNotAllowed(w, http.MethodPost)
		return
	}
	endpointID, ok := extractHookEndpointID(r)
	if !ok {
		h.rejectedNotFound.Add(1)
		h.recordMetric(IngressResultRejectedUnknownEndpoint)
		apierr.WriteError(w, http.StatusNotFound, apierr.ErrCodeNotFound, "not found", nil)
		return
	}

	// Step 2: per-client-IP rate limit (IPv6 /64, 20/s burst 100) before lookup or body read.
	clientIP := h.clientIP(r)
	if !h.limiter.AllowClientIP(clientIP) {
		h.rejectedRateLimit.Add(1)
		h.recordMetric(IngressResultRejectedRateLimit)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}

	ctx := r.Context()
	now := h.now().UTC()

	// Step 3: per-replica endpoint lookup + split unverified buckets (R2).
	ep, err := h.cache.LookupEndpoint(ctx, h.store, endpointID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.failedStore.Add(1)
		h.recordMetric(IngressResultFailedStore)
		h.logger.Error("Webhook endpoint lookup failed", "endpoint_id", endpointID, "error", err)
		apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "internal server error", nil)
		return
	}
	if errors.Is(err, store.ErrNotFound) || ep == nil || ep.Status != store.WebhookEndpointStatusActive {
		// Unknown or disabled endpoint ID charges ONLY the hub-wide rejection bucket.
		if !h.limiter.AllowRejection() {
			h.rejectedRateLimit.Add(1)
			h.recordMetric(IngressResultRejectedRateLimit)
			writeRateLimited(w, "1", "rate limit exceeded")
			return
		}
		h.rejectedNotFound.Add(1)
		h.recordMetric(IngressResultRejectedUnknownEndpoint)
		// Identical body for unknown and disabled endpoints (no enumeration oracle).
		apierr.WriteError(w, http.StatusNotFound, apierr.ErrCodeNotFound, "not found", nil)
		return
	}
	// Known active endpoint charges ONLY its per-endpoint bucket (default 10/s
	// burst 60, or the endpoint's lower configured rate/burst, §7.4). Running
	// this before step 6 ensures denied credentials on hub_token endpoints and
	// bad signatures on hmac endpoints both charge the endpoint's rate bucket (R2).
	if !h.limiter.AllowEndpointWithLimit(ep.ID, ep.RatePerSecond, ep.RateBurst) {
		h.rejectedRateLimit.Add(1)
		h.recordMetric(IngressResultRejectedRateLimit)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}

	// Step 4: read body bounded to the endpoint's effective max_body_bytes
	// (clamped to MaxEndpointBodyBytes; P5 raises MaxEndpointBodyBytes to 1 MiB
	// and branches bodies > MaxPayloadBytes to out-of-line object storage).
	maxBody := EffectiveMaxBodyBytes(ep.MaxBodyBytes)
	limitedReader := http.MaxBytesReader(w, r.Body, int64(maxBody))
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			h.rejectedTooLarge.Add(1)
			h.recordMetric(IngressResultRejectedSize)
			h.counters.RecordRejected(ep.ID)
			apierr.WriteError(w, http.StatusRequestEntityTooLarge, apierr.ErrCodeInvalidRequest, "payload too large", nil)
			return
		}
		h.rejectedBadJSON.Add(1)
		h.recordMetric(IngressResultRejectedBadJSON)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "failed to read request body", nil)
		return
	}

	// Step 5: Content-Type is application/json (parameters ignored); body parses as JSON.
	if !hasJSONContentType(r.Header) {
		h.rejectedMediaType.Add(1)
		h.recordMetric(IngressResultRejectedMediaType)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
		return
	}
	if len(body) == 0 || !utf8.Valid(body) || bytes.ContainsRune(body, '\x00') || !json.Valid(body) {
		h.rejectedBadJSON.Add(1)
		h.recordMetric(IngressResultRejectedBadJSON)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "body must be JSON", nil)
		return
	}

	// Step 6: authenticate the delivery according to ep.AuthMode (§7.1, §7.2).
	//   - hub_token: require an authenticated hub identity passing
	//     webhookEndpointAccessDecision(send); routing headers are honoured
	//     unsigned and bounded in step 7.
	//   - hmac (default): dispatch ep.SignatureMode through Verifier
	//     (scion_v1, github_v1, stripe_v1) with P3 rotation grace and P4a
	//     replay_window_seconds.
	// Header bounds are checked for status 400 ONLY after step 6 succeeds so an
	// unauthenticated caller always receives 401/403 (§8.1 step 7).
	var (
		verifiedHdrs VerifiedHeaders
		sigModeAttr  string
		senderAttr   string
	)
	authMode := ep.AuthMode
	if authMode == "" {
		authMode = store.WebhookAuthModeHMAC
	}
	switch authMode {
	case store.WebhookAuthModeHubToken:
		if h.authorizeHubToken == nil {
			h.failedSecret.Add(1)
			h.recordMetric(IngressResultRejectedSecretUnavailable)
			h.logger.Error("Webhook hub_token authorizer is not configured",
				"endpoint_id", ep.ID,
				"project_id", ep.ProjectID,
			)
			apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook verification unavailable", nil)
			return
		}
		authRes := h.authorizeHubToken(r, ep)
		if !authRes.Authenticated {
			h.rejectedBadSig.Add(1)
			h.recordMetric(IngressResultRejectedAuth)
			h.counters.RecordRejected(ep.ID)
			h.audit.RecordSignatureFailure(ctx, SignatureFailureAuditRecord{
				ProjectID:  ep.ProjectID,
				EndpointID: ep.ID,
				RemoteIP:   clientIP,
			})
			apierr.WriteError(w, http.StatusUnauthorized, apierr.ErrCodeUnauthorized, "authentication required", nil)
			return
		}
		if !authRes.Allowed {
			h.rejectedBadSig.Add(1)
			h.recordMetric(IngressResultRejectedAuth)
			h.counters.RecordRejected(ep.ID)
			h.audit.RecordSignatureFailure(ctx, SignatureFailureAuditRecord{
				ProjectID:  ep.ProjectID,
				EndpointID: ep.ID,
				RemoteIP:   clientIP,
			})
			apierr.WriteError(w, http.StatusForbidden, apierr.ErrCodeForbidden, "forbidden", nil)
			return
		}
		delivery := r.Header.Get(HeaderDelivery)
		eventHdr := r.Header.Get(HeaderEvent)
		subjectHdr := r.Header.Get(HeaderSubject)
		if hdrErr := validateSignedRoutingHeaders(r.Header, delivery, eventHdr, subjectHdr); hdrErr != nil {
			h.rejectedBadHeader.Add(1)
			h.recordMetric(IngressResultRejectedBadHeader)
			h.counters.RecordRejected(ep.ID)
			apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, hdrErr.Error(), nil)
			return
		}
		verifiedHdrs = VerifiedHeaders{
			Delivery: delivery,
			Event:    eventHdr,
			Subject:  subjectHdr,
		}
		sigModeAttr = store.WebhookAuthModeHubToken
		senderAttr = authRes.Sender

	case store.WebhookAuthModeHMAC:
		replayWindow := EffectiveReplayWindow(ep.ReplayWindowSeconds)
		verifier := NewVerifierForMode(ep.SignatureMode, now, replayWindow, h.verifyGitHubSignature)
		if verifier == nil {
			h.failedSecret.Add(1)
			h.recordMetric(IngressResultRejectedSecretUnavailable)
			h.logger.Error("Unsupported webhook endpoint signature mode",
				"endpoint_id", ep.ID,
				"project_id", ep.ProjectID,
				"signature_mode", ep.SignatureMode,
			)
			apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook verification unavailable", nil)
			return
		}
		secret, err := h.cache.LookupSecret(ctx, h.loadSecret, ep.ProjectID, ep.ID, ep.SecretVersion)
		if err != nil {
			h.failedSecret.Add(1)
			h.recordMetric(IngressResultRejectedSecretUnavailable)
			h.logger.Error("Failed to load or unseal webhook endpoint secret",
				"endpoint_id", ep.ID,
				"project_id", ep.ProjectID,
				"secret_version", ep.SecretVersion,
				"error", err,
			)
			apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook verification unavailable", nil)
			return
		}

		vh, vErr := verifier.Verify(r, body, []string{secret})
		// Stage 2 §4.4 / S22: during the 24h rotation grace window, fall back to
		// verifying against secret version n-1 if the current version did not match.
		if errors.Is(vErr, ErrInvalidSignature) && ep.SecretVersion > 1 && ep.SecretRotatedAt != nil &&
			!now.After(ep.SecretRotatedAt.UTC().Add(store.WebhookSecretRotationGraceWindow)) {
			prevSecret, prevErr := h.cache.LookupSecret(ctx, h.loadSecret, ep.ProjectID, ep.ID, ep.SecretVersion-1)
			if prevErr != nil && !errors.Is(prevErr, store.ErrNotFound) {
				h.failedSecret.Add(1)
				h.recordMetric(IngressResultRejectedSecretUnavailable)
				h.logger.Error("Failed to load or unseal previous webhook endpoint secret during rotation grace",
					"endpoint_id", ep.ID,
					"project_id", ep.ProjectID,
					"secret_version", ep.SecretVersion-1,
					"error", prevErr,
				)
				apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook verification unavailable", nil)
				return
			}
			if prevErr == nil {
				vh, vErr = verifier.Verify(r, body, []string{prevSecret})
			}
		}
		if vErr != nil {
			if errors.Is(vErr, ErrInvalidHeader) {
				h.rejectedBadHeader.Add(1)
				h.recordMetric(IngressResultRejectedBadHeader)
				h.counters.RecordRejected(ep.ID)
				apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, strings.TrimPrefix(vErr.Error(), ErrInvalidHeader.Error()+": "), nil)
				return
			}
			h.rejectedBadSig.Add(1)
			h.recordMetric(IngressResultRejectedSignature)
			h.counters.RecordRejected(ep.ID)
			h.audit.RecordSignatureFailure(ctx, SignatureFailureAuditRecord{
				ProjectID:  ep.ProjectID,
				EndpointID: ep.ID,
				RemoteIP:   clientIP,
			})
			apierr.WriteError(w, http.StatusUnauthorized, apierr.ErrCodeInvalidSignature, "invalid signature", nil)
			return
		}
		verifiedHdrs = vh
		sigModeAttr = ep.SignatureMode

	default:
		h.failedSecret.Add(1)
		h.recordMetric(IngressResultRejectedSecretUnavailable)
		h.logger.Error("Unsupported webhook endpoint auth mode",
			"endpoint_id", ep.ID,
			"project_id", ep.ProjectID,
			"auth_mode", ep.AuthMode,
		)
		apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook verification unavailable", nil)
		return
	}

	// Step 7: apply routing hints after verification (Stage 2 §7.2, §18 S20).
	// Signed scion_v1 headers (and already-populated provider headers) always
	// take precedence; for overflow bodies only the first 64 KiB is scanned.
	effHints := EffectiveRoutingHints(sigModeAttr, ep.RoutingHints)
	verifiedHdrs, hintErr := ResolveRoutingHints(body, effHints, verifiedHdrs)
	delivery := verifiedHdrs.Delivery
	eventHdr := verifiedHdrs.Event
	subjectHdr := verifiedHdrs.Subject

	// Step 8: per-project verified bucket (50/s burst 300) and hub-wide verified
	// bucket (200/s burst 1000). The per-project pending queue cap (5000) is
	// checked once authoritatively inside WithTx under LockTriggerQuota below.
	if !h.limiter.AllowProjectVerified(ep.ProjectID) {
		h.rejectedRateLimit.Add(1)
		h.recordMetric(IngressResultRejectedRateLimit)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}
	if !h.limiter.AllowHubVerified() {
		h.rejectedRateLimit.Add(1)
		h.recordMetric(IngressResultRejectedRateLimit)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}

	// Step 9: construct TriggerEvent (§8.2). For overflow bodies
	// (len(body) > MaxPayloadBytes, Stage 2 §8 deviation R1), run non-locking
	// dedup + advisory budget pre-checks and upload to hub-private pkg/storage
	// BEFORE opening WithTx so object-storage latency never holds a DB
	// connection or advisory lock. Then run the DB-only transaction with
	// LockTriggerQuota, authoritative dedup, pending cap, authoritative in-lock
	// byte budget (O1), and event + queue insert.
	isOverflow := len(body) > MaxPayloadBytes
	var overflowStor storage.Storage
	if isOverflow {
		overflowStor = h.getStorage()
		if overflowStor == nil {
			h.failedStorage.Add(1)
			h.recordMetric(IngressResultFailedStorage)
			h.logger.Error("Webhook overflow payload storage is not configured",
				"endpoint_id", ep.ID,
				"project_id", ep.ProjectID,
				"payload_bytes", len(body),
			)
			apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook payload storage unavailable", nil)
			return
		}
	}

	ev, eventAttr, attrOK := buildWebhookTriggerEvent(ep, r.Header, delivery, eventHdr, subjectHdr, clientIP, sigModeAttr, senderAttr, hintErr, body, isOverflow, now, h.newEventID)
	if !attrOK {
		h.rejectedBadHeader.Add(1)
		h.recordMetric(IngressResultRejectedBadHeader)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "event attributes exceed maximum size", nil)
		return
	}
	if isOverflow && ev.PayloadRef == "" {
		h.failedStore.Add(1)
		h.recordMetric(IngressResultFailedStore)
		apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "failed to build overflow payload path", nil)
		return
	}

	var uploadedOverflow bool
	if isOverflow {
		// Pre-tx non-locking dedup check: an early 202 duplicate avoids a
		// pointless upload on redelivery. Authoritative dedup still runs
		// inside LockTriggerQuota below.
		if delivery != "" {
			existing, gErr := h.store.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindWebhook, ep.ID, ev.DedupKey)
			if gErr == nil && existing != nil {
				h.writeDuplicateAccepted(w, ctx, ep, existing.ID, eventAttr, len(body), clientIP)
				return
			}
			if gErr != nil && !errors.Is(gErr, store.ErrNotFound) {
				h.failedStore.Add(1)
				h.recordMetric(IngressResultFailedStore)
				h.logger.Error("Pre-upload webhook dedup lookup failed",
					"endpoint_id", ep.ID,
					"project_id", ep.ProjectID,
					"error", gErr,
				)
				apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "failed to persist webhook event", nil)
				return
			}
		}

		// Pre-tx non-locking advisory pending-cap check (O-C): refuse early when
		// the project's durable queue is already at the cap to avoid wasted
		// storage write+delete churn. Authoritative check still runs inside
		// LockTriggerQuota below.
		var preCounter store.DurableQueuePendingCounter
		if c, ok := h.store.(store.DurableQueuePendingCounter); ok {
			preCounter = c
		} else if c, ok := h.queueStore.(store.DurableQueuePendingCounter); ok {
			preCounter = c
		}
		if preCounter != nil {
			pending, pErr := preCounter.CountDurableQueuePending(ctx, EventQueueKind, ep.ProjectID)
			if pErr != nil {
				h.failedStore.Add(1)
				h.recordMetric(IngressResultFailedStore)
				h.logger.Error("Pre-upload webhook pending-cap lookup failed",
					"endpoint_id", ep.ID,
					"project_id", ep.ProjectID,
					"error", pErr,
				)
				apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "failed to persist webhook event", nil)
				return
			}
			if pending >= MaxPendingEventsPerProject {
				h.rejectedQueueFull.Add(1)
				h.recordMetric(IngressResultRejectedQueueFull)
				writeRateLimited(w, "30", "queue full")
				return
			}
		}

		// Pre-tx non-locking advisory budget check: refuse early when the
		// project is already over its 256 MiB retention budget to avoid a
		// wasted upload. Authoritative budget still runs inside LockTriggerQuota.
		usedBytes, sumErr := h.store.SumTriggerEventOverflowBytes(ctx, ep.ProjectID, now.Add(-HistoryRetention))
		if sumErr != nil {
			h.failedStore.Add(1)
			h.recordMetric(IngressResultFailedStore)
			h.logger.Error("Pre-upload webhook overflow budget lookup failed",
				"endpoint_id", ep.ID,
				"project_id", ep.ProjectID,
				"error", sumErr,
			)
			apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "failed to persist webhook event", nil)
			return
		}
		if usedBytes+int64(len(body)) > h.maxOverflowBytes {
			h.rejectedQueueFull.Add(1)
			h.recordMetric(IngressResultRejectedQueueFull)
			writeRateLimited(w, "30", "queue full")
			return
		}

		// Stage 2 §8 / R1 / S21: upload to the event-unique hub-private path
		// BEFORE opening WithTx, with a bounded timeout; never issue signed URLs.
		uploadCtx, cancelUpload := context.WithTimeout(ctx, h.overflowUploadTimeout)
		_, upErr := overflowStor.Upload(uploadCtx, ev.PayloadRef, bytes.NewReader(body), storage.UploadOptions{
			ContentType: "application/json",
			Idempotent:  true,
		})
		cancelUpload()
		if upErr != nil {
			h.failedStorage.Add(1)
			h.recordMetric(IngressResultFailedStorage)
			h.logger.Error("Failed to upload webhook overflow payload to object storage",
				"endpoint_id", ep.ID,
				"project_id", ep.ProjectID,
				"event_id", ev.ID,
				"payload_bytes", len(body),
				"error", upErr,
			)
			apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook payload storage unavailable", nil)
			return
		}
		uploadedOverflow = true
	}

	var (
		dupEventID string
		closureErr error
	)
	// O-B: explicitly bound the upload-to-commit window (pool wait + lock wait +
	// tx body) well below DefaultOverflowOrphanMinAge so an in-flight overflow
	// upload can never be swept before its transaction commits.
	txCtx := ctx
	var cancelTx context.CancelFunc
	if uploadedOverflow {
		txCtx, cancelTx = context.WithTimeout(ctx, h.overflowTxTimeout)
	}
	txErr := h.store.WithTx(txCtx, func(tx store.Store) error {
		ret := func(err error) error {
			closureErr = err
			return err
		}
		if lErr := tx.LockTriggerQuota(txCtx, ep.ProjectID); lErr != nil {
			return ret(lErr)
		}
		// §6.2: duplicate redeliveries (when X-Scion-Delivery is present) are
		// acknowledged with 202 "duplicate" BEFORE the pending-cap and overflow
		// budget checks so a redelivery at the cap never returns 429.
		if delivery != "" {
			existing, gErr := tx.GetTriggerEventByDedupKey(txCtx, store.TriggerSourceKindWebhook, ep.ID, ev.DedupKey)
			if gErr == nil && existing != nil {
				dupEventID = existing.ID
				return ret(errDuplicateDelivery)
			}
			if gErr != nil && !errors.Is(gErr, store.ErrNotFound) {
				return ret(gErr)
			}
		}
		txCounter, ok := tx.(store.DurableQueuePendingCounter)
		if !ok {
			return ret(errTxMissingQueuePendingCounter)
		}
		pending, sErr := txCounter.CountDurableQueuePending(txCtx, EventQueueKind, ep.ProjectID)
		if sErr != nil {
			return ret(sErr)
		}
		if pending >= MaxPendingEventsPerProject {
			return ret(errQueueFull)
		}
		if isOverflow {
			// O1: compute the overflow byte sum authoritatively inside
			// LockTriggerQuota so concurrent/multi-replica bursts cannot
			// overshoot the 256 MiB per-project budget.
			usedBytes, sumErr := tx.SumTriggerEventOverflowBytes(txCtx, ep.ProjectID, now.Add(-HistoryRetention))
			if sumErr != nil {
				return ret(sumErr)
			}
			if usedBytes+int64(len(body)) > h.maxOverflowBytes {
				return ret(errQueueFull)
			}
		}
		if _, enqErr := EnqueueEvent(txCtx, tx, tx, h.queue, &ev); enqErr != nil {
			return ret(enqErr)
		}
		return nil
	})
	if cancelTx != nil {
		cancelTx()
	}

	if txErr != nil {
		// R1 cleanup rule: delete the just-uploaded object ONLY when the tx
		// closure itself returned an error (rollback is certain). Never delete
		// on a COMMIT error (closureErr == nil), because Postgres COMMIT errors
		// can be ambiguous after server-side commit; leave those to the hourly
		// orphan sweep.
		if uploadedOverflow && closureErr != nil {
			delCtx, cancelDel := context.WithTimeout(context.WithoutCancel(ctx), DefaultOverflowDownloadTimeout)
			if dErr := overflowStor.Delete(delCtx, ev.PayloadRef); dErr != nil && !errors.Is(dErr, storage.ErrNotFound) {
				h.logger.Warn("Failed to clean up uploaded overflow payload after transaction error; orphan sweep will retry",
					"endpoint_id", ep.ID,
					"project_id", ep.ProjectID,
					"event_id", ev.ID,
					"payload_ref", ev.PayloadRef,
					"error", dErr,
				)
			}
			cancelDel()
		}

		if errors.Is(txErr, errQueueFull) {
			h.rejectedQueueFull.Add(1)
			h.recordMetric(IngressResultRejectedQueueFull)
			writeRateLimited(w, "30", "queue full")
			return
		}
		// Answer 202 "duplicate" strictly for X-Scion-Delivery deduplication:
		// either found up-front in tx before the pending-cap check
		// (errDuplicateDelivery), or on a concurrent (source_kind, source_id,
		// dedup_key) conflict when X-Scion-Delivery was supplied. Event-ID
		// collisions (durablequeue.ErrDuplicate, or store.ErrAlreadyExists
		// without a matching delivery-dedup row) fall through to 500 so the
		// sender retries.
		if errors.Is(txErr, errDuplicateDelivery) ||
			(delivery != "" && !errors.Is(txErr, durablequeue.ErrDuplicate) && errors.Is(txErr, store.ErrAlreadyExists)) {
			if dupEventID == "" {
				existing, gErr := h.store.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindWebhook, ep.ID, ev.DedupKey)
				if gErr != nil || existing == nil {
					h.failedStore.Add(1)
					h.recordMetric(IngressResultFailedStore)
					h.logger.Error("Failed to resolve webhook trigger event after unique conflict",
						"endpoint_id", ep.ID,
						"project_id", ep.ProjectID,
						"error", txErr,
						"lookup_error", gErr,
					)
					apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "failed to persist webhook event", nil)
					return
				}
				dupEventID = existing.ID
			}
			h.writeDuplicateAccepted(w, ctx, ep, dupEventID, eventAttr, len(body), clientIP)
			return
		}

		h.failedStore.Add(1)
		h.recordMetric(IngressResultFailedStore)
		h.logger.Error("Failed to persist webhook trigger event",
			"endpoint_id", ep.ID,
			"project_id", ep.ProjectID,
			"error", txErr,
		)
		apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "failed to persist webhook event", nil)
		return
	}

	// Step 10: wake local drainer(s), update in-memory counters, emit receipt audit, answer 202.
	h.queue.Wake(EventQueueKind)
	if isOverflow {
		h.acceptedOverflow.Add(1)
		h.recordMetric(IngressResultAcceptedOverflow)
	} else {
		h.accepted.Add(1)
		h.recordMetric(IngressResultAccepted)
	}
	h.recordPayloadBytes(len(body))
	h.counters.RecordAccepted(ep.ID, now)
	h.audit.RecordReceipt(ctx, ReceiptAuditRecord{
		ProjectID:    ep.ProjectID,
		EndpointID:   ep.ID,
		EventID:      ev.ID,
		WebhookEvent: eventAttr,
		PayloadBytes: len(body),
		Result:       IngressStatusQueued,
		RemoteIP:     clientIP,
	})
	apierr.WriteJSON(w, http.StatusAccepted, IngressAcceptedResponse{
		EventID: ev.ID,
		Status:  IngressStatusQueued,
	})
}

func (h *Ingress) writeDuplicateAccepted(
	w http.ResponseWriter,
	ctx context.Context,
	ep *store.WebhookEndpoint,
	dupEventID, eventAttr string,
	payloadBytes int,
	clientIP string,
) {
	h.duplicate.Add(1)
	h.recordMetric(IngressResultDuplicate)
	h.audit.RecordReceipt(ctx, ReceiptAuditRecord{
		ProjectID:    ep.ProjectID,
		EndpointID:   ep.ID,
		EventID:      dupEventID,
		WebhookEvent: eventAttr,
		PayloadBytes: payloadBytes,
		Result:       IngressStatusDuplicate,
		RemoteIP:     clientIP,
	})
	apierr.WriteJSON(w, http.StatusAccepted, IngressAcceptedResponse{
		EventID: dupEventID,
		Status:  IngressStatusDuplicate,
	})
}

func extractHookEndpointID(r *http.Request) (string, bool) {
	path := r.URL.Path
	if !strings.HasPrefix(path, hooksPathPrefix) {
		return "", false
	}
	id := path[len(hooksPathPrefix):]
	if !isCanonicalLowerUUID(id) {
		return "", false
	}
	if pv := r.PathValue("id"); pv != "" && pv != id {
		return "", false
	}
	return id, true
}

func hasJSONContentType(hdr http.Header) bool {
	vals := hdr.Values("Content-Type")
	if len(vals) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(vals[0])
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func validateSignedRoutingHeaders(hdr http.Header, delivery, eventHdr, subjectHdr string) error {
	if len(hdr.Values(HeaderDelivery)) > 1 || len(hdr.Values(HeaderEvent)) > 1 || len(hdr.Values(HeaderSubject)) > 1 {
		return errors.New("duplicate X-Scion-* header is not allowed")
	}
	if len(delivery) > MaxDeliveryBytes || (delivery != "" && !isPrintableString(delivery)) {
		return errors.New("invalid X-Scion-Delivery header")
	}
	if eventHdr != "" {
		if len(eventHdr) > MaxEventBytes || !eventNamePattern.MatchString(eventHdr) {
			return errors.New("invalid X-Scion-Event header")
		}
	}
	if subjectHdr != "" {
		if len(subjectHdr) > MaxSubjectBytes || !isPrintableString(subjectHdr) {
			return errors.New("invalid X-Scion-Subject header")
		}
	}
	return nil
}

// WebhookEventType returns the canonical trigger event.type produced for a
// signed webhook ingress request with the given X-Scion-Event header value
// ("webhook.received" when empty, "webhook.<name>" otherwise).
func WebhookEventType(eventHeader string) string {
	if eventHeader != "" {
		return "webhook." + eventHeader
	}
	return "webhook.received"
}

func buildWebhookTriggerEvent(
	ep *store.WebhookEndpoint,
	hdr http.Header,
	delivery, eventHdr, subjectHdr, clientIP, sigModeAttr, senderAttr, routingHintErr string,
	body []byte,
	isOverflow bool,
	now time.Time,
	newEventID func() string,
) (store.TriggerEvent, string, bool) {
	eventType := WebhookEventType(eventHdr)
	eventAttr := "received"
	if eventHdr != "" {
		eventAttr = eventHdr
	}

	subject := "webhook:" + ep.ID
	if subjectHdr != "" {
		subject = subject + "/" + subjectHdr
	}

	dedupKey := delivery
	if dedupKey == "" {
		dedupKey = uuid.NewString()
	}

	eventID := ""
	if newEventID != nil {
		eventID = newEventID()
	}
	if eventID == "" {
		eventID = uuid.NewString()
	}

	var (
		payloadStr string
		payloadRef string
	)
	if isOverflow {
		ref, err := OverflowPayloadObjectPath(ep.ProjectID, eventID)
		if err != nil {
			return store.TriggerEvent{}, "", true
		}
		payloadRef = ref
	} else {
		payloadStr = string(body)
	}

	if sigModeAttr == "" {
		sigModeAttr = ep.SignatureMode
	}
	contentType := sanitizeHeaderForStorage(hdr.Get("Content-Type"), maxStoredContentTypeBytes)
	attrs := map[string]string{
		"endpoint_name":  ep.Name,
		"event":          eventAttr,
		"delivery":       delivery,
		"subject":        subjectHdr,
		"signature_mode": sigModeAttr,
		"content_type":   contentType,
		"payload_bytes":  strconv.Itoa(len(body)),
		"remote_ip":      clientIP,
	}
	if senderAttr != "" {
		attrs["sender"] = senderAttr
	}
	if routingHintErr != "" {
		attrs["routing_hint_error"] = routingHintErr
	}
	if payloadRef != "" {
		attrs["payload_ref"] = payloadRef
	}
	attrBytes, err := json.Marshal(attrs)
	if err != nil || len(attrBytes) > MaxAttributesBytes {
		return store.TriggerEvent{}, "", false
	}

	storedHeaders := make(map[string]string, len(allowedEventHeaders))
	for _, name := range allowedEventHeaders {
		v := hdr.Get(name)
		if v == "" {
			continue
		}
		maxLen := MaxSubjectBytes
		switch name {
		case "User-Agent":
			maxLen = maxStoredUserAgentBytes
		case "Content-Type":
			maxLen = maxStoredContentTypeBytes
		}
		storedHeaders[name] = sanitizeHeaderForStorage(v, maxLen)
	}

	return store.TriggerEvent{
		ID:           eventID,
		ProjectID:    ep.ProjectID,
		SourceKind:   store.TriggerSourceKindWebhook,
		SourceID:     ep.ID,
		Type:         eventType,
		Subject:      subject,
		DedupKey:     dedupKey,
		OccurredAt:   now,
		ReceivedAt:   now,
		Attributes:   attrs,
		Headers:      storedHeaders,
		Payload:      payloadStr,
		PayloadRef:   payloadRef,
		PayloadBytes: len(body),
		Status:       store.TriggerEventStatusPending,
	}, eventAttr, true
}

func writeRateLimited(w http.ResponseWriter, retryAfter, message string) {
	w.Header().Set("Retry-After", retryAfter)
	apierr.WriteError(w, http.StatusTooManyRequests, apierr.ErrCodeRateLimited, message, nil)
}
