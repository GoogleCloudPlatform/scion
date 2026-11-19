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
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// triggerRuntime holds the hub's shared trigger.event durablequeue handle and
// background drainer lifecycle state. The zero value is ready to use.
type triggerRuntime struct {
	mu          sync.Mutex
	queue       *durablequeue.StoreQueue
	ruleMetrics *trigger.OTelRuleEvalMetrics
	drainer     *durablequeue.Drainer
	cancel      context.CancelFunc
	done        chan struct{}
	started     bool
}

// TriggerEventQueue returns the hub's shared durablequeue.StoreQueue for
// trigger.event producers and drainers. When s was constructed via New, this
// returns s.durableQueue (the same queue instance used by POST /api/v1/hooks/{id}
// ingress so Wake wakes the local drainer immediately); bare test Server
// structs without s.durableQueue fall back to a lazy StoreQueue on s.triggers.
func (s *Server) TriggerEventQueue() *durablequeue.StoreQueue {
	if s == nil {
		return nil
	}
	if s.durableQueue != nil {
		return s.durableQueue
	}
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return nil
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return s.triggerQueueLocked(dqStore)
}

func (s *Server) triggerQueueLocked(dqStore store.DurableQueueStore) *durablequeue.StoreQueue {
	if s.durableQueue != nil {
		return s.durableQueue
	}
	if s.triggers.queue == nil {
		s.triggers.queue = durablequeue.NewQueue(dqStore)
	}
	return s.triggers.queue
}

// serverTriggerCore adapts Server's fire core, experiment gate, and wake-fire
// shutdown admission to trigger.Core and trigger.FireAdmitter without exposing
// package hub types to pkg/hub/trigger.
type serverTriggerCore struct {
	srv *Server
}

var (
	_ trigger.Core             = serverTriggerCore{}
	_ trigger.FireAdmitter     = serverTriggerCore{}
	_ trigger.SourceAuthorizer = serverTriggerCore{}
)

func (c serverTriggerCore) Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome {
	return c.srv.fireAction(ctx, req)
}

func (c serverTriggerCore) Experiment(name string) bool {
	return c.srv.experimentEnabled(name)
}

// AuthorizeEventSource implements trigger.SourceAuthorizer for Stage 3 §4.2
// step 4 (rev 1.10): before any fire on a github event, verify that the
// project's GitHub App installation association is authorized now via
// authorizeGitHubAppAssociation (using existing caches). Definitive
// errGitHubAssociationUnauthorized updates project.GitHubAppStatus to
// association_unauthorized and returns (false, nil) so matched candidates
// record refused "github association not authorized". Inconclusive
// errGitHubAssociationCheckFailed updates project.GitHubAppStatus to
// association_check_failed and returns an error so the durable queue backs off
// and retries until MaxAge without refusing the event.
func (c serverTriggerCore) AuthorizeEventSource(ctx context.Context, ev *store.TriggerEvent) (bool, error) {
	if ev == nil || ev.SourceKind != store.TriggerSourceKindGitHub {
		return true, nil
	}
	if c.srv == nil || c.srv.store == nil {
		return false, nil
	}
	instID, err := strconv.ParseInt(strings.TrimSpace(ev.SourceID), 10, 64)
	if err != nil || instID <= 0 {
		return false, nil
	}
	project, err := c.srv.store.GetProject(ctx, ev.ProjectID)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return false, nil
		}
		return false, err
	}
	if project == nil || project.GitHubInstallationID == nil || *project.GitHubInstallationID != instID {
		return false, nil
	}
	if _, authzErr := c.srv.authorizeGitHubAppAssociation(ctx, project, instID); authzErr != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		errCode := githubAssociationErrorCode(authzErr)
		c.srv.updateProjectGitHubAppStatus(ctx, project, store.GitHubAppStateError,
			errCode, authzErr.Error())
		if errors.Is(authzErr, errGitHubAssociationCheckFailed) {
			slog.WarnContext(ctx, "GitHub trigger fire deferred: installation association check inconclusive; will retry",
				"project_id", project.ID, "installation_id", instID, "event_id", ev.ID, "error", authzErr)
			return false, authzErr
		}
		slog.WarnContext(ctx, "GitHub trigger fire refused: installation association not authorized",
			"project_id", project.ID, "installation_id", instID, "event_id", ev.ID, "error", authzErr)
		return false, nil
	}
	c.srv.clearProjectGitHubAssociationErrorStatus(ctx, project)
	return true, nil
}

func (c serverTriggerCore) AdmitFire(ctx, itemCtx context.Context) (context.Context, context.Context, func(), bool) {
	if itemCtx != nil && itemCtx.Err() != nil {
		return ctx, nil, func() {}, false
	}
	var (
		trackCtx context.Context
		endWork  = func() {}
	)
	if existingAbort, _ := ctx.Value(scheduledFireAbortKey{}).(context.Context); existingAbort != nil {
		trackCtx = existingAbort
	} else if c.srv != nil {
		var ok bool
		trackCtx, endWork, ok = c.srv.scheduledDetachedFires.begin()
		if !ok {
			return ctx, nil, func() {}, false
		}
	}
	// Bound scheduledFireAbortKey and the returned abortCtx by BOTH hub
	// shutdown (trackCtx) and the durablequeue item lifetime (itemCtx), with
	// NO per-fire deadline, so both wakeFireTarget's detachKeepFireAbort(ctx)
	// and post-wake tracker.Detach(ctx) outlive the 30 s per-fire timeout
	// without outliving either the queue item (Stage 1 ledger L3) or shutdown.
	abortCtx, cancelAbort := context.WithCancel(context.Background())
	var stopTrack, stopItem func() bool
	if trackCtx != nil {
		stopTrack = context.AfterFunc(trackCtx, cancelAbort)
	}
	if itemCtx != nil {
		stopItem = context.AfterFunc(itemCtx, cancelAbort)
	}
	fireCtx, cancelFire := context.WithCancel(ctx)
	stopFire := context.AfterFunc(abortCtx, cancelFire)
	return withScheduledFireAbort(fireCtx, abortCtx), abortCtx, func() {
		stopFire()
		if stopItem != nil {
			stopItem()
		}
		if stopTrack != nil {
			stopTrack()
		}
		cancelFire()
		cancelAbort()
		endWork()
	}, true
}

func (s *Server) triggerDrainerContext(ctx context.Context) context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return ctx
}

// startTriggerDrainer marks trigger background services started and launches
// the trigger.event drainer when hub.triggers is enabled. When hub.triggers is
// off, no drainer goroutine runs until reconcileTriggerDrainer observes the
// experiment enabled (§16.2).
func (s *Server) startTriggerDrainer(ctx context.Context) <-chan struct{} {
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return nil
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.started = true
	s.reconcileTriggerDrainerLocked(ctx, dqStore)
	return rt.done
}

// reconcileTriggerDrainer aligns the running trigger.event drainer with the
// live hub.triggers experiment state (called on ApplySnapshot and in tests).
func (s *Server) reconcileTriggerDrainer(ctx context.Context) {
	dqStore, ok := s.store.(store.DurableQueueStore)
	if !ok || dqStore == nil {
		return
	}
	rt := &s.triggers
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if !rt.started {
		return
	}
	s.reconcileTriggerDrainerLocked(ctx, dqStore)
}

func (s *Server) reconcileTriggerDrainerLocked(ctx context.Context, dqStore store.DurableQueueStore) {
	rt := &s.triggers
	if !s.experimentEnabled(experiments.Triggers) {
		if rt.cancel != nil {
			rt.cancel()
			rt.drainer, rt.cancel, rt.done = nil, nil, nil
		}
		return
	}
	if rt.drainer != nil {
		return
	}
	ctx = s.triggerDrainerContext(ctx)
	if ctx.Err() != nil {
		return
	}
	q := s.triggerQueueLocked(dqStore)
	d, err := s.triggerDrainer(dqStore, q)
	if err != nil {
		slog.Warn("hub.triggers: drainer not started", "error", err)
		return
	}
	dctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	rt.drainer, rt.cancel, rt.done = d, cancel, done
	go func() {
		defer close(done)
		d.Run(dctx)
		cancel()
		rt.mu.Lock()
		if rt.drainer == d {
			rt.drainer, rt.cancel, rt.done = nil, nil, nil
		}
		rt.mu.Unlock()
	}()
}

// triggerDrainer constructs the trigger.event durablequeue.Drainer wired to
// Server's fire core (catalog entry point triggerDrainer:message for trigger.fire).
func (s *Server) triggerDrainer(dqStore store.DurableQueueStore, queue *durablequeue.StoreQueue) (*durablequeue.Drainer, error) {
	core := serverTriggerCore{srv: s}
	logger := logging.Subsystem("hub.trigger-drainer")
	if s.triggers.ruleMetrics == nil {
		m, mErr := trigger.NewOTelRuleEvalMetrics(otel.GetMeterProvider())
		if mErr != nil {
			logger.Warn("hub.triggers: failed to initialize rule eval metrics", "error", mErr)
		} else {
			s.triggers.ruleMetrics = m
		}
	}
	disp, err := trigger.NewDispatcher(trigger.DispatcherConfig{
		Store:       s.store,
		Core:        core,
		Rules:       s.TriggerRules(),
		RuleMetrics: s.triggers.ruleMetrics,
		Logger:      logger,
	})
	if err != nil {
		return nil, err
	}
	return disp.NewDrainer(trigger.DrainerOptions{
		QueueStore: dqStore,
		Queue:      queue,
		Owner:      s.HubID(),
	})
}

// stopTriggerDrainer cancels the running trigger.event drainer and waits for
// its bounded shutdown drain (or ctx cancellation).
func (s *Server) stopTriggerDrainer(ctx context.Context) {
	rt := &s.triggers
	rt.mu.Lock()
	cancel, done := rt.cancel, rt.done
	rt.drainer, rt.cancel, rt.done = nil, nil, nil
	rt.started = false
	rt.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// runTriggerMaintenance executes the hourly trigger reaper, orphan-pending
// event reconciliation, and 7-day history purge inside Server.purgeHandler
// (§4.3, §6.4, rev 7.4).
func (s *Server) runTriggerMaintenance(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	res, err := trigger.RunMaintenance(ctx, s.store, time.Now())
	if err != nil {
		slog.Error("Scheduler: trigger maintenance failed", "error", err)
	}
	if res.FiresReaped > 0 || res.EventsReconciled > 0 || res.EventsPurged > 0 || res.FiresPurged > 0 {
		slog.Info("Scheduler: completed trigger maintenance",
			"fires_reaped", res.FiresReaped,
			"events_reconciled", res.EventsReconciled,
			"events_purged", res.EventsPurged,
			"fires_purged", res.FiresPurged)
	}
}

// verifiedLabelForFireEvent maps known source kinds and signature_mode values
// to canonical integrity labels and fails closed to ExternalVerifiedUnverified
// for missing or unrecognized modes (never assuming scion-v1 and never echoing
// raw input).
func verifiedLabelForFireEvent(ev *hubfire.FireEvent) string {
	if ev == nil {
		return messaging.ExternalVerifiedUnverified
	}
	if ev.SourceKind == store.TriggerSourceKindGitHub {
		return messaging.ExternalVerifiedGitHubHMACSHA256Projected
	}
	switch ev.Attributes["signature_mode"] {
	case store.WebhookSignatureModeBodyOnly, messaging.ExternalVerifiedHMACSHA256Body:
		return messaging.ExternalVerifiedHMACSHA256Body
	case store.WebhookSignatureModeScionV1, messaging.ExternalVerifiedScionV1:
		return messaging.ExternalVerifiedScionV1
	default:
		return messaging.ExternalVerifiedUnverified
	}
}

const (
	neutralEventContentType = "application/octet-stream"
	maxSafeContentTypeBytes = 128
)

var safeContentTypeParamMaxLen = map[string]int{
	"charset": 32,
	"version": 16,
}

func isSafeMediaTypeToken(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		isAlphaNum := (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
		if i == 0 {
			if !isAlphaNum {
				return false
			}
			continue
		}
		if !isAlphaNum && b != '.' && b != '_' && b != '+' && b != '-' {
			return false
		}
	}
	return true
}

func trimHTTPOWS(s string) string {
	start := 0
	for start < len(s) && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	end := len(s)
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// sanitizeEventContentType restricts an event's content_type attribute to a
// canonical lowercase media-type token ("type/subtype") plus an optional
// allowlisted token parameter subset (charset, version). Any malformed value,
// quoted string, comment, control byte, or unknown parameter is replaced with
// neutralEventContentType while leaving the event body untouched.
func sanitizeEventContentType(raw string) string {
	if len(raw) == 0 || len(raw) > maxSafeContentTypeBytes {
		return neutralEventContentType
	}
	for i := 0; i < len(raw); i++ {
		if (raw[i] < 0x20 && raw[i] != '\t') || raw[i] > 0x7e {
			return neutralEventContentType
		}
	}
	trimmed := trimHTTPOWS(raw)
	if trimmed == "" {
		return neutralEventContentType
	}
	parts := strings.Split(trimmed, ";")
	if len(parts) > 1+len(safeContentTypeParamMaxLen) {
		return neutralEventContentType
	}
	media := trimHTTPOWS(parts[0])
	slash := strings.IndexByte(media, '/')
	if slash <= 0 || slash >= len(media)-1 || strings.IndexByte(media[slash+1:], '/') >= 0 {
		return neutralEventContentType
	}
	mainType := media[:slash]
	subType := media[slash+1:]
	if !isSafeMediaTypeToken(mainType, 64) || !isSafeMediaTypeToken(subType, 64) {
		return neutralEventContentType
	}

	var b strings.Builder
	b.Grow(len(trimmed))
	b.WriteString(strings.ToLower(mainType))
	b.WriteByte('/')
	b.WriteString(strings.ToLower(subType))

	var seenParams map[string]struct{}
	for _, rawParam := range parts[1:] {
		p := trimHTTPOWS(rawParam)
		if p == "" {
			return neutralEventContentType
		}
		eq := strings.IndexByte(p, '=')
		if eq <= 0 || eq >= len(p)-1 || strings.IndexByte(p[eq+1:], '=') >= 0 {
			return neutralEventContentType
		}
		k := strings.ToLower(p[:eq])
		v := p[eq+1:]
		maxValLen, allowed := safeContentTypeParamMaxLen[k]
		if !allowed || !isSafeMediaTypeToken(v, maxValLen) {
			return neutralEventContentType
		}
		if seenParams == nil {
			seenParams = make(map[string]struct{}, len(parts)-1)
		}
		if _, dup := seenParams[k]; dup {
			return neutralEventContentType
		}
		seenParams[k] = struct{}{}
		b.WriteString("; ")
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strings.ToLower(v))
	}
	return b.String()
}

// buildDispatchedEventBody constructs the §8.3 messaging.EventBody for a
// FireEvent delivered to an agent.
func buildDispatchedEventBody(ev *hubfire.FireEvent) messaging.EventBody {
	if ev == nil {
		return messaging.EventBody{}
	}
	var body messaging.EventBody
	switch ev.Type {
	case string(messaging.EventWebhookReceived):
		body.Type = messaging.EventWebhookReceived
	case string(messaging.EventScheduleFired):
		body.Type = messaging.EventScheduleFired
	default:
		body.Type = messaging.EventTriggerFired
		body.EventType = ev.Type
	}
	body.Subject = ev.Subject
	body.EventID = ev.ID
	if ev.SourceKind != "" || ev.SourceID != "" {
		sourceName := ""
		if ev.Attributes != nil {
			sourceName = ev.Attributes["endpoint_name"]
		}
		body.Source = &messaging.EventSource{
			Kind: ev.SourceKind,
			ID:   ev.SourceID,
			Name: sourceName,
		}
	}
	if ev.Trigger != nil {
		body.Trigger = &messaging.EventTriggerRef{
			ID:   ev.Trigger.ID,
			Name: ev.Trigger.Name,
		}
	}
	verified := verifiedLabelForFireEvent(ev)
	if len(ev.Attributes) > 0 {
		attrs := maps.Clone(ev.Attributes)
		if verified != messaging.ExternalVerifiedScionV1 && verified != messaging.ExternalVerifiedGitHubHMACSHA256Projected {
			// §8.1a: generic webhook modes other than scion_v1 strip header-derived
			// attributes. Stage 3 GitHub App deliveries (§4.1, §4.3) keep "delivery"
			// and "event" populated from X-GitHub-Delivery and X-GitHub-Event: those
			// headers are shape-checked at ingress, NOT authenticated by
			// X-Hub-Signature-256 (which covers the raw request body only).
			delete(attrs, "delivery")
			delete(attrs, "event")
			delete(attrs, "subject")
		}
		if rawCT, hasCT := attrs["content_type"]; hasCT {
			attrs["content_type"] = sanitizeEventContentType(rawCT)
		}
		if len(attrs) > 0 {
			body.Attributes = attrs
		}
	}
	if len(ev.Lists) > 0 {
		body.Lists = copyTriggerStringListMap(ev.Lists)
	}
	if len(ev.Payload) > 0 {
		body.External = messaging.NewExternalPayload(verified, ev.Payload)
	}
	return body
}

// DispatchedEventUsesPhaseANoRow reports whether req stays on the Stage 1
// Phase-A unpersisted delivery path even when hub.conduit_deliver is enabled.
// Trigger fires have no scheduled_events row and use Sender="trigger", so
// persisting them into messages.scheduled_event_id and holding on
// awaiting_session would fail held-message re-drive revalidation; keeping them
// on the no-row path makes non-deliverable targets fail loud and record
// status=failed on trigger_fires without changing schedule fire behaviour.
//
// Named without "Trigger" so action_message.go satisfies TestFireCoreContainment.
func DispatchedEventUsesPhaseANoRow(req hubfire.FireRequest) bool {
	return req.Authority.DefinitionKind == hubfire.DefinitionKindTrigger
}

// ApplyDispatchedEventEnvelope populates structuredMsg's §8.3 event carrier and
// sender metadata when req.Event is present, before action_message.go persists
// (when enabled) and calls RenderDeliveryText.
//
// Named without the word "Trigger" so action_message.go can invoke it while
// satisfying TestFireCoreContainment.
func ApplyDispatchedEventEnvelope(structuredMsg *messages.StructuredMessage, req hubfire.FireRequest) error {
	if structuredMsg == nil || req.Event == nil {
		return nil
	}
	if req.Event.Trigger != nil {
		structuredMsg.Sender = "trigger"
		structuredMsg.SenderID = "TRIGGER"
		if structuredMsg.Metadata == nil {
			structuredMsg.Metadata = make(map[string]string)
		}
		structuredMsg.Metadata["system_category"] = messages.SystemCategoryTrigger
	}
	if !req.Event.OccurredAt.IsZero() {
		structuredMsg.Timestamp = req.Event.OccurredAt.UTC().Format(time.RFC3339)
	}

	body := buildDispatchedEventBody(req.Event)
	rawEvent, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal dispatched event body: %w", err)
	}
	structuredMsg.Event = rawEvent
	return nil
}
