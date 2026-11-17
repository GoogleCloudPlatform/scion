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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Ingress payload, header, attribute, and queue bounds (§4.3, §6.1, §8.1, §8.2).
const (
	MaxPayloadBytes            = 64 * 1024
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
	// trigger_events.headers. Secret headers such as X-Scion-Signature and
	// Authorization are never stored.
	allowedEventHeaders = []string{
		"Content-Type",
		"User-Agent",
		HeaderEvent,
		HeaderDelivery,
		HeaderSubject,
	}

	errQueueFull = errors.New("trigger event queue full for project")
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

// IngressCounts snapshots running request outcome counters for tests and metrics.
type IngressCounts struct {
	Accepted          int64
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
}

// ClientIPFunc resolves the client IP address of r using the hub's trusted
// proxy configuration (geExchangeClientIP in package hub).
type ClientIPFunc func(r *http.Request) string

// IngressTxStore is the minimal transactional TriggerStore interface required
// by Ingress (satisfied by store.Store / *entadapter.CompositeStore).
type IngressTxStore interface {
	store.TriggerStore
	WithTx(ctx context.Context, fn func(tx store.Store) error) error
}

// IngressConfig configures the generic webhook ingress handler.
type IngressConfig struct {
	Store      IngressTxStore
	QueueStore store.DurableQueueStore
	Queue      durablequeue.Queue
	LoadSecret SecretLoader
	ClientIP   ClientIPFunc
	Enabled    func() bool
	Limiter    *IngressLimiter
	Cache      *EndpointCache
	Counters   *CounterAccumulator
	Audit      AuditSink
	Logger     *slog.Logger
	Now        func() time.Time
	NewEventID func() string
}

// Ingress handles unauthenticated POST /api/v1/hooks/{id} requests per Stage 1
// §8.1, §8.1a, §8.2, §6.1, and §6.2.
type Ingress struct {
	store      IngressTxStore
	queueStore store.DurableQueueStore
	queue      durablequeue.Queue
	loadSecret SecretLoader
	clientIP   ClientIPFunc
	enabled    func() bool
	limiter    *IngressLimiter
	cache      *EndpointCache
	counters   *CounterAccumulator
	audit      AuditSink
	logger     *slog.Logger
	now        func() time.Time
	newEventID func() string

	accepted          atomic.Int64
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
	return &Ingress{
		store:      cfg.Store,
		queueStore: cfg.QueueStore,
		queue:      cfg.Queue,
		loadSecret: cfg.LoadSecret,
		clientIP:   clientIP,
		enabled:    cfg.Enabled,
		limiter:    limiter,
		cache:      cache,
		counters:   counters,
		audit:      audit,
		logger:     logger,
		now:        nowFn,
		newEventID: newEventID,
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

// StartBackground starts the limiter's stale-bucket cleanup goroutine and the
// 10-second endpoint counter flush loop until ctx is cancelled.
func (h *Ingress) StartBackground(ctx context.Context) {
	if h == nil {
		return
	}
	if h.limiter != nil {
		h.limiter.StartCleanup(ctx)
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
		apierr.WriteError(w, http.StatusNotFound, apierr.ErrCodeNotFound, "not found", nil)
		return
	}

	// Step 2: per-client-IP rate limit (IPv6 /64, 20/s burst 100) before lookup or body read.
	clientIP := h.clientIP(r)
	if !h.limiter.AllowClientIP(clientIP) {
		h.rejectedRateLimit.Add(1)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}

	ctx := r.Context()
	now := h.now().UTC()

	// Step 3: per-replica endpoint lookup + split unverified buckets (R2).
	ep, err := h.cache.LookupEndpoint(ctx, h.store, endpointID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.failedStore.Add(1)
		h.logger.Error("Webhook endpoint lookup failed", "endpoint_id", endpointID, "error", err)
		apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "internal server error", nil)
		return
	}
	if errors.Is(err, store.ErrNotFound) || ep == nil || ep.Status != store.WebhookEndpointStatusActive {
		// Unknown or disabled endpoint ID charges ONLY the hub-wide rejection bucket.
		if !h.limiter.AllowRejection() {
			h.rejectedRateLimit.Add(1)
			writeRateLimited(w, "1", "rate limit exceeded")
			return
		}
		h.rejectedNotFound.Add(1)
		// Identical body for unknown and disabled endpoints (no enumeration oracle).
		apierr.WriteError(w, http.StatusNotFound, apierr.ErrCodeNotFound, "not found", nil)
		return
	}
	// Known active endpoint charges ONLY its per-endpoint bucket (10/s burst 60).
	if !h.limiter.AllowEndpoint(ep.ID) {
		h.rejectedRateLimit.Add(1)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}

	// Step 4: read body bounded to 64 KiB (413 above 64 KiB).
	limitedReader := http.MaxBytesReader(w, r.Body, MaxPayloadBytes)
	body, err := io.ReadAll(limitedReader)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			h.rejectedTooLarge.Add(1)
			h.counters.RecordRejected(ep.ID)
			apierr.WriteError(w, http.StatusRequestEntityTooLarge, apierr.ErrCodeInvalidRequest, "payload too large", nil)
			return
		}
		h.rejectedBadJSON.Add(1)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "failed to read request body", nil)
		return
	}

	// Step 5: Content-Type is application/json (parameters ignored); body parses as JSON.
	if !hasJSONContentType(r.Header) {
		h.rejectedMediaType.Add(1)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json", nil)
		return
	}
	if len(body) == 0 || !utf8.Valid(body) || bytes.ContainsRune(body, '\x00') || !json.Valid(body) {
		h.rejectedBadJSON.Add(1)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "body must be JSON", nil)
		return
	}

	// Step 6: scion_v1 HMAC verification (§8.1a). Fail closed (503) if the
	// endpoint's secret cannot be loaded or unsealed. Header bounds are checked
	// for status 400 ONLY after signature verification succeeds so an unsigned
	// caller always receives 401 (§8.1 step 7).
	if ep.SignatureMode != store.WebhookSignatureModeScionV1 {
		h.failedSecret.Add(1)
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
		h.logger.Error("Failed to load or unseal webhook endpoint secret",
			"endpoint_id", ep.ID,
			"project_id", ep.ProjectID,
			"secret_version", ep.SecretVersion,
			"error", err,
		)
		apierr.WriteError(w, http.StatusServiceUnavailable, apierr.ErrCodeUnavailable, "webhook verification unavailable", nil)
		return
	}

	sigVals := r.Header.Values(HeaderSignature)
	delivery := r.Header.Get(HeaderDelivery)
	eventHdr := r.Header.Get(HeaderEvent)
	subjectHdr := r.Header.Get(HeaderSubject)
	if len(sigVals) != 1 || !VerifyScionV1Signature(secret, now, sigVals[0], delivery, eventHdr, subjectHdr, body) {
		h.rejectedBadSig.Add(1)
		h.counters.RecordRejected(ep.ID)
		h.audit.RecordSignatureFailure(ctx, SignatureFailureAuditRecord{
			ProjectID:  ep.ProjectID,
			EndpointID: ep.ID,
			RemoteIP:   clientIP,
		})
		apierr.WriteError(w, http.StatusUnauthorized, apierr.ErrCodeInvalidSignature, "invalid signature", nil)
		return
	}

	// Step 7: routing header bounds and charset checks (400 only after HMAC verified).
	if hdrErr := validateSignedRoutingHeaders(r.Header, delivery, eventHdr, subjectHdr); hdrErr != nil {
		h.rejectedBadHeader.Add(1)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, hdrErr.Error(), nil)
		return
	}

	// Step 8: per-project verified bucket (50/s burst 300) and hub-wide verified
	// bucket (200/s burst 1000). The per-project pending queue cap (5000) is
	// checked once authoritatively inside WithTx under LockTriggerQuota below.
	if !h.limiter.AllowProjectVerified(ep.ProjectID) {
		h.rejectedRateLimit.Add(1)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}
	if !h.limiter.AllowHubVerified() {
		h.rejectedRateLimit.Add(1)
		writeRateLimited(w, "1", "rate limit exceeded")
		return
	}

	// Step 9: construct TriggerEvent (§8.2) and check pending cap + insert + enqueue in one transaction (§6.2).
	ev, eventAttr, attrOK := buildWebhookTriggerEvent(ep, r.Header, delivery, eventHdr, subjectHdr, clientIP, body, now, h.newEventID)
	if !attrOK {
		h.rejectedBadHeader.Add(1)
		h.counters.RecordRejected(ep.ID)
		apierr.WriteError(w, http.StatusBadRequest, apierr.ErrCodeInvalidRequest, "event attributes exceed maximum size", nil)
		return
	}

	txErr := h.store.WithTx(ctx, func(tx store.Store) error {
		if lErr := tx.LockTriggerQuota(ctx, ep.ProjectID); lErr != nil {
			return lErr
		}
		statsReader := h.queueStore
		if txQS, ok := tx.(store.DurableQueueStore); ok {
			statsReader = txQS
		}
		curStats, sErr := statsReader.DurableQueueStats(ctx, EventQueueKind, ep.ProjectID, now)
		if sErr != nil {
			return sErr
		}
		if curStats.Pending >= MaxPendingEventsPerProject {
			return errQueueFull
		}
		_, enqErr := EnqueueEvent(ctx, tx, tx, h.queue, &ev)
		return enqErr
	})

	if txErr != nil {
		if errors.Is(txErr, errQueueFull) {
			h.rejectedQueueFull.Add(1)
			writeRateLimited(w, "30", "queue full")
			return
		}
		// Map both trigger_events (source_kind, source_id, dedup_key) conflicts
		// (store.ErrAlreadyExists) and durable_queue_items (kind, dedup_key)
		// conflicts (durablequeue.ErrDuplicate, which wraps store.ErrAlreadyExists)
		// to 202 "duplicate".
		if errors.Is(txErr, store.ErrAlreadyExists) {
			dupEventID := ev.ID
			if !errors.Is(txErr, durablequeue.ErrDuplicate) && delivery != "" {
				existing, gErr := h.store.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindWebhook, ep.ID, ev.DedupKey)
				if gErr != nil {
					h.failedStore.Add(1)
					h.logger.Error("Failed to load duplicate webhook trigger event",
						"endpoint_id", ep.ID,
						"project_id", ep.ProjectID,
						"error", gErr,
					)
					apierr.WriteError(w, http.StatusInternalServerError, apierr.ErrCodeInternalError, "failed to persist webhook event", nil)
					return
				}
				dupEventID = existing.ID
			}
			h.duplicate.Add(1)
			h.audit.RecordReceipt(ctx, ReceiptAuditRecord{
				ProjectID:    ep.ProjectID,
				EndpointID:   ep.ID,
				EventID:      dupEventID,
				WebhookEvent: eventAttr,
				PayloadBytes: len(body),
				Result:       IngressStatusDuplicate,
				RemoteIP:     clientIP,
			})
			apierr.WriteJSON(w, http.StatusAccepted, IngressAcceptedResponse{
				EventID: dupEventID,
				Status:  IngressStatusDuplicate,
			})
			return
		}

		h.failedStore.Add(1)
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
	h.accepted.Add(1)
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

func buildWebhookTriggerEvent(
	ep *store.WebhookEndpoint,
	hdr http.Header,
	delivery, eventHdr, subjectHdr, clientIP string,
	body []byte,
	now time.Time,
	newEventID func() string,
) (store.TriggerEvent, string, bool) {
	eventType := "webhook.received"
	eventAttr := "received"
	if eventHdr != "" {
		eventType = "webhook." + eventHdr
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

	contentType := sanitizeHeaderForStorage(hdr.Get("Content-Type"), maxStoredContentTypeBytes)
	attrs := map[string]string{
		"endpoint_name":  ep.Name,
		"event":          eventAttr,
		"delivery":       delivery,
		"subject":        subjectHdr,
		"signature_mode": ep.SignatureMode,
		"content_type":   contentType,
		"payload_bytes":  strconv.Itoa(len(body)),
		"remote_ip":      clientIP,
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

	eventID := ""
	if newEventID != nil {
		eventID = newEventID()
	}
	if eventID == "" {
		eventID = uuid.NewString()
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
		Payload:      string(body),
		PayloadBytes: len(body),
		Status:       store.TriggerEventStatusPending,
	}, eventAttr, true
}

func writeRateLimited(w http.ResponseWriter, retryAfter, message string) {
	w.Header().Set("Retry-After", retryAfter)
	apierr.WriteError(w, http.StatusTooManyRequests, apierr.ErrCodeRateLimited, message, nil)
}
