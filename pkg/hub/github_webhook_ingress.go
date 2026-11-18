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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	githubtrigger "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

// GitHub App content-event ingress rate-limit, header, status, and audit
// constants (Stage 3 design §4.1–§4.4).
const (
	GitHubWebhookVerifiedRatePerSecond = 100.0
	GitHubWebhookVerifiedBurst         = 500
	GitHubWebhookMaxDeliveryBytes      = 64

	GitHubAuditEventRejected              = "trigger.github.rejected"
	GitHubAuditRejectReasonTargetMismatch = "target_mismatch"

	GitHubWebhookStatusQueued              = "queued"
	GitHubWebhookStatusDuplicate           = "duplicate"
	GitHubWebhookStatusSkipped             = "skipped"
	GitHubWebhookStatusIgnored             = "ignored"
	GitHubWebhookStatusUnknownInstallation = "unknown_installation"
	GitHubWebhookStatusNoProject           = "no_project"

	GitHubWebhookSkipReasonQueueFull         = "queue_full"
	GitHubWebhookSkipReasonAttributesCeiling = "attributes_ceiling"

	GitHubWebhookRoutingRoutedToPrimary = "routed_to_primary"
	GitHubWebhookRoutingStalePrimary    = "stale_primary"

	githubTargetMismatchAuditInterval = time.Minute
	githubStalePrimaryWarnInterval    = time.Hour
	maxGitHubStalePrimaryWarnEntries  = 4096
	githubCandidateProjectsPageSize   = 200
	maxGitHubStoredUserAgentBytes     = 512
	maxGitHubStoredHeaderBytes        = 256
	maxGitHubEnvelopeActionBytes      = 64
	maxGitHubEnvelopeLoginBytes       = 128
)

var (
	errGitHubDuplicateDelivery       = errors.New("duplicate github webhook delivery")
	errGitHubProjectQueueFull        = errors.New("github trigger event queue full for project")
	errGitHubMissingPendingCounterTx = errors.New("transaction store does not implement DurableQueuePendingCounter")
)

// GitHubWebhookProjectEvent reports the per-project outcome of an accepted
// GitHub content webhook delivery (§4.1 step 10).
type GitHubWebhookProjectEvent struct {
	ProjectID string `json:"project_id"`
	EventID   string `json:"event_id"`
	Status    string `json:"status"`
}

// GitHubWebhookSkippedProject reports a candidate project whose event was not
// persisted during fan-out (§4.1 steps 8–10).
type GitHubWebhookSkippedProject struct {
	ProjectID string `json:"project_id"`
	Reason    string `json:"reason"`
}

// GitHubWebhookAcceptedResponse is the HTTP 202 response payload returned for
// verified GitHub App content-event deliveries (§4.1 steps 5–10).
type GitHubWebhookAcceptedResponse struct {
	Status          string                        `json:"status"`
	Reason          string                        `json:"reason,omitempty"`
	Delivery        string                        `json:"delivery,omitempty"`
	Events          []GitHubWebhookProjectEvent   `json:"events,omitempty"`
	SkippedProjects []GitHubWebhookSkippedProject `json:"skipped_projects,omitempty"`
}

type githubStalePrimaryKey struct {
	installationID int64
	repo           string
}

type githubReceiptAuditRecord struct {
	Delivery           string
	Event              string
	Action             string
	InstallationID     int64
	Repo               string
	PayloadBytes       int
	Result             string
	Routing            string
	BypassedProjectIDs []string
	ProjectIDs         []string
	Events             []GitHubWebhookProjectEvent
	SkippedProjects    []GitHubWebhookSkippedProject
}

// githubWebhookIngressState holds the per-Server verified token bucket,
// rate-limited target-mismatch audit window, rate-limited stale-primary warning
// map, and outcome counters for GitHub App content-event ingress. Zero value is
// ready to use with Stage 3 defaults.
type githubWebhookIngressState struct {
	mu sync.Mutex

	bucketInit      bool
	bucketTokens    float64
	bucketLastCheck time.Time
	bucketRate      float64
	bucketBurst     int

	lastTargetMismatchAudit    time.Time
	suppressedTargetMismatches int

	stalePrimaryWarns map[githubStalePrimaryKey]time.Time

	nowFn       func() time.Time
	auditLogger *slog.Logger
	warnLogger  *slog.Logger

	selfIgnoredCount    atomic.Int64
	targetMismatchCount atomic.Int64
	rateLimitedCount    atomic.Int64
}

func (st *githubWebhookIngressState) now() time.Time {
	st.mu.Lock()
	fn := st.nowFn
	st.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return timeNow()
}

func (st *githubWebhookIngressState) auditLog() *slog.Logger {
	st.mu.Lock()
	l := st.auditLogger
	st.mu.Unlock()
	if l != nil {
		return l
	}
	return logging.Subsystem("hub.audit")
}

func (st *githubWebhookIngressState) allowVerified(now time.Time) bool {
	st.mu.Lock()
	defer st.mu.Unlock()

	rate := st.bucketRate
	if rate <= 0 {
		rate = GitHubWebhookVerifiedRatePerSecond
	}
	burst := st.bucketBurst
	if burst == 0 {
		burst = GitHubWebhookVerifiedBurst
	}
	if burst < 0 {
		return false
	}

	if !st.bucketInit {
		st.bucketInit = true
		st.bucketTokens = float64(burst) - 1
		st.bucketLastCheck = now
		return true
	}

	elapsed := now.Sub(st.bucketLastCheck).Seconds()
	if elapsed > 0 {
		st.bucketTokens += elapsed * rate
		if st.bucketTokens > float64(burst) {
			st.bucketTokens = float64(burst)
		}
		st.bucketLastCheck = now
	}
	if st.bucketTokens >= 1 {
		st.bucketTokens--
		return true
	}
	return false
}

func (st *githubWebhookIngressState) recordTargetMismatchAudit(ctx context.Context, now time.Time) {
	st.targetMismatchCount.Add(1)

	st.mu.Lock()
	if !st.lastTargetMismatchAudit.IsZero() && now.Sub(st.lastTargetMismatchAudit) < githubTargetMismatchAuditInterval {
		st.suppressedTargetMismatches++
		st.mu.Unlock()
		return
	}
	suppressed := st.suppressedTargetMismatches
	st.suppressedTargetMismatches = 0
	st.lastTargetMismatchAudit = now
	l := st.auditLogger
	st.mu.Unlock()

	if l == nil {
		l = logging.Subsystem("hub.audit")
	}
	attrs := []slog.Attr{
		slog.String("event_type", GitHubAuditEventRejected),
		slog.String("reason", GitHubAuditRejectReasonTargetMismatch),
	}
	if suppressed > 0 {
		attrs = append(attrs, slog.Int("suppressed_count", suppressed))
	}
	if reqID := logging.RequestIDFromContext(ctx); reqID != "" {
		attrs = append(attrs, slog.String(logging.AttrRequestID, reqID))
	}
	l.LogAttrs(ctx, slog.LevelWarn, GitHubAuditEventRejected, attrs...)
}

func (st *githubWebhookIngressState) maybeWarnStalePrimary(installationID int64, repo string, now time.Time) {
	key := githubStalePrimaryKey{
		installationID: installationID,
		repo:           strings.ToLower(strings.TrimSpace(repo)),
	}

	st.mu.Lock()
	if st.stalePrimaryWarns == nil {
		st.stalePrimaryWarns = make(map[githubStalePrimaryKey]time.Time)
	}
	if last, ok := st.stalePrimaryWarns[key]; ok && now.Sub(last) < githubStalePrimaryWarnInterval {
		st.mu.Unlock()
		return
	}
	if len(st.stalePrimaryWarns) >= maxGitHubStalePrimaryWarnEntries {
		var oldestKey githubStalePrimaryKey
		var oldestTime time.Time
		hasOldest := false
		for k, t := range st.stalePrimaryWarns {
			if now.Sub(t) >= githubStalePrimaryWarnInterval {
				delete(st.stalePrimaryWarns, k)
				continue
			}
			if !hasOldest || t.Before(oldestTime) {
				oldestKey = k
				oldestTime = t
				hasOldest = true
			}
		}
		if len(st.stalePrimaryWarns) >= maxGitHubStalePrimaryWarnEntries && hasOldest {
			delete(st.stalePrimaryWarns, oldestKey)
		}
	}
	st.stalePrimaryWarns[key] = now
	l := st.warnLogger
	st.mu.Unlock()

	if l == nil {
		l = slog.Default()
	}
	l.Warn("GitHub trigger primary project is stale; fanning out to all candidate projects",
		"installation_id", installationID,
		"repo", repo,
		"routing", GitHubWebhookRoutingStalePrimary,
	)
}

func (st *githubWebhookIngressState) recordReceiptAudit(ctx context.Context, rec githubReceiptAuditRecord) {
	l := st.auditLog()
	attrs := []slog.Attr{
		slog.String("event_type", trigger.AuditEventReceived),
		slog.String("source_kind", store.TriggerSourceKindGitHub),
		slog.String("delivery", rec.Delivery),
		slog.String("event", rec.Event),
		slog.Int("payload_bytes", rec.PayloadBytes),
		slog.String("result", rec.Result),
	}
	if rec.Action != "" {
		attrs = append(attrs, slog.String("action", rec.Action))
	}
	if rec.InstallationID > 0 {
		attrs = append(attrs, slog.Int64("installation_id", rec.InstallationID))
	}
	if rec.Repo != "" {
		attrs = append(attrs, slog.String("repo", rec.Repo))
	}
	if rec.Routing != "" {
		attrs = append(attrs, slog.String("routing", rec.Routing))
	}
	if len(rec.BypassedProjectIDs) > 0 {
		attrs = append(attrs, slog.Any("bypassed_project_ids", rec.BypassedProjectIDs))
	}
	if len(rec.ProjectIDs) > 0 {
		attrs = append(attrs, slog.Any("project_ids", rec.ProjectIDs))
	}
	if len(rec.Events) > 0 {
		attrs = append(attrs, slog.Any("events", rec.Events))
	}
	if len(rec.SkippedProjects) > 0 {
		attrs = append(attrs, slog.Any("skipped_projects", rec.SkippedProjects))
	}
	if reqID := logging.RequestIDFromContext(ctx); reqID != "" {
		attrs = append(attrs, slog.String(logging.AttrRequestID, reqID))
	}
	l.LogAttrs(ctx, slog.LevelInfo, trigger.AuditEventReceived, attrs...)
}

// handleGitHubContentWebhook executes Stage 3 design §4.1 steps 0–10 for
// signature-verified GitHub App content-event deliveries.
func (s *Server) handleGitHubContentWebhook(w http.ResponseWriter, r *http.Request, eventType, deliveryID string, body []byte) {
	// Step 0: both hub.triggers and hub.github_triggers must be enabled.
	if !s.githubTriggersEnabled() {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": eventType})
		return
	}

	ctx := r.Context()
	now := s.githubWebhookIngress.now().UTC()

	// Step 1: X-GitHub-Hook-Installation-Target-Type == "integration" and
	// X-GitHub-Hook-Installation-Target-ID == <configured AppID>.
	s.mu.RLock()
	appID := s.config.GitHubAppConfig.AppID
	s.mu.RUnlock()

	targetTypes := r.Header.Values("X-GitHub-Hook-Installation-Target-Type")
	targetIDs := r.Header.Values("X-GitHub-Hook-Installation-Target-ID")
	if appID <= 0 ||
		len(targetTypes) != 1 || targetTypes[0] != "integration" ||
		len(targetIDs) != 1 || targetIDs[0] != strconv.FormatInt(appID, 10) {
		s.githubWebhookIngress.recordTargetMismatchAudit(ctx, now)
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "wrong target", nil)
		return
	}

	// Step 2: event family in Stage 3 table else 200 ignored; X-GitHub-Delivery
	// present, <= 64 bytes, and GUID-shaped (8-4-4-4-12 hex).
	if len(r.Header.Values("X-GitHub-Event")) != 1 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid event header", nil)
		return
	}
	if !githubtrigger.IsSupportedEvent(eventType) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "event": eventType})
		return
	}
	if len(r.Header.Values("X-GitHub-Delivery")) != 1 || !isValidGitHubDeliveryGUID(deliveryID) {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid delivery id", nil)
		return
	}
	deliveryID = strings.ToLower(deliveryID)

	// Step 3: hub-wide verified bucket for GitHub (100/s burst 500, no IP keying).
	if !s.githubWebhookIngress.allowVerified(now) {
		s.githubWebhookIngress.rateLimitedCount.Add(1)
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "rate limit exceeded", nil)
		return
	}

	// Step 4: parse JSON object without NUL and extract installation.id (+ routing envelope).
	env, err := parseGitHubContentEnvelope(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid payload", nil)
		return
	}

	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "trigger store is not available", nil)
		return
	}

	// Step 5: installation lookup — must exist and have status == active.
	inst, err := s.store.GetGitHubInstallation(ctx, env.installationID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		slog.Error("GitHub trigger ingress installation lookup failed",
			"installation_id", env.installationID,
			"delivery_id", deliveryID,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "internal server error", nil)
		return
	}
	if errors.Is(err, store.ErrNotFound) || inst == nil || inst.Status != store.GitHubInstallationStatusActive {
		s.githubWebhookIngress.recordReceiptAudit(ctx, githubReceiptAuditRecord{
			Delivery:       deliveryID,
			Event:          eventType,
			Action:         env.action,
			InstallationID: env.installationID,
			Repo:           env.repoFullName,
			PayloadBytes:   len(body),
			Result:         GitHubWebhookStatusUnknownInstallation,
		})
		writeJSON(w, http.StatusAccepted, GitHubWebhookAcceptedResponse{
			Status:   GitHubWebhookStatusUnknownInstallation,
			Delivery: deliveryID,
		})
		return
	}

	// Step 6: own-App guard (§5.7) — sender.type == "Bot" && sender.login == <slug>+"[bot]".
	if !env.senderOK {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid payload", nil)
		return
	}
	if githubtrigger.IsOwnAppSender(env.senderType, env.senderLogin, s.GitHubAppBotLogin(ctx)) {
		s.githubWebhookIngress.selfIgnoredCount.Add(1)
		writeJSON(w, http.StatusAccepted, GitHubWebhookAcceptedResponse{
			Status:   GitHubWebhookStatusIgnored,
			Reason:   "self",
			Delivery: deliveryID,
		})
		return
	}

	// Step 7: fan-out (§4.2 steps 1–4).
	if !env.repoOK || !env.actionOK {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid payload", nil)
		return
	}
	candidates, err := s.listGitHubInstallationCandidateProjects(ctx, inst.InstallationID, env.repoFullName)
	if err != nil {
		slog.Error("GitHub trigger ingress project fan-out lookup failed",
			"installation_id", inst.InstallationID,
			"delivery_id", deliveryID,
			"error", err,
		)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "internal server error", nil)
		return
	}
	if len(candidates) == 0 {
		s.githubWebhookIngress.recordReceiptAudit(ctx, githubReceiptAuditRecord{
			Delivery:       deliveryID,
			Event:          eventType,
			Action:         env.action,
			InstallationID: inst.InstallationID,
			Repo:           env.repoFullName,
			PayloadBytes:   len(body),
			Result:         GitHubWebhookStatusNoProject,
		})
		writeJSON(w, http.StatusAccepted, GitHubWebhookAcceptedResponse{
			Status:   GitHubWebhookStatusNoProject,
			Delivery: deliveryID,
		})
		return
	}

	selected, bypassed, routingStatus := ApplyPrimaryRouting(candidates, inst.TriggerRouting, env.repoFullName)
	auditRouting := ""
	switch routingStatus {
	case GitHubTriggerRoutingStatusOK:
		auditRouting = GitHubWebhookRoutingRoutedToPrimary
	case GitHubTriggerRoutingStatusStalePrimary:
		auditRouting = GitHubWebhookRoutingStalePrimary
		s.githubWebhookIngress.maybeWarnStalePrimary(inst.InstallationID, env.repoFullName, now)
	}

	selectedIDs := make([]string, len(selected))
	for i := range selected {
		selectedIDs[i] = selected[i].ID
	}

	// Step 8: normalise once (§5, §10.2).
	appSlug, mentionAliases, _ := s.GitHubAppMentionIdentities(ctx)
	normRes, normErr := githubtrigger.Normalize(eventType, deliveryID, body, githubtrigger.Options{
		AppSlug:        appSlug,
		MentionAliases: mentionAliases,
		ReceivedAt:     now,
	})
	if normErr != nil {
		if errors.Is(normErr, githubtrigger.ErrAttributesCeilingExceeded) {
			skipped := make([]GitHubWebhookSkippedProject, len(selected))
			for i := range selected {
				skipped[i] = GitHubWebhookSkippedProject{
					ProjectID: selected[i].ID,
					Reason:    GitHubWebhookSkipReasonAttributesCeiling,
				}
			}
			s.githubWebhookIngress.recordReceiptAudit(ctx, githubReceiptAuditRecord{
				Delivery:           deliveryID,
				Event:              eventType,
				Action:             env.action,
				InstallationID:     inst.InstallationID,
				Repo:               env.repoFullName,
				PayloadBytes:       len(body),
				Result:             GitHubWebhookStatusSkipped,
				Routing:            auditRouting,
				BypassedProjectIDs: bypassed,
				ProjectIDs:         selectedIDs,
				SkippedProjects:    skipped,
			})
			writeJSON(w, http.StatusAccepted, GitHubWebhookAcceptedResponse{
				Status:          GitHubWebhookStatusSkipped,
				Delivery:        deliveryID,
				SkippedProjects: skipped,
			})
			return
		}
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid payload", nil)
		return
	}

	queue := s.TriggerEventQueue()
	if queue == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable, "trigger event queue is not available", nil)
		return
	}

	// Step 9: per-project tx (dedup before pending-cap check, pending cap < 5000,
	// insert trigger_events + durablequeue Enqueue) and Wake after commit.
	storedHeaders := sanitizeGitHubEventHeaders(githubtrigger.FilterHeaders(r.Header))
	projectedPayload := string(normRes.Payload)

	var (
		events    []GitHubWebhookProjectEvent
		skipped   []GitHubWebhookSkippedProject
		anyQueued bool
	)

	for i := range selected {
		proj := &selected[i]
		ev := store.TriggerEvent{
			ID:           uuid.NewString(),
			ProjectID:    proj.ID,
			SourceKind:   store.TriggerSourceKindGitHub,
			SourceID:     normRes.SourceID,
			Type:         normRes.Type,
			Subject:      normRes.Subject,
			DedupKey:     normRes.DedupKey(proj.ID),
			OccurredAt:   normRes.OccurredAt,
			ReceivedAt:   now,
			Attributes:   maps.Clone(normRes.Attributes),
			Lists:        copyTriggerStringListMap(normRes.Lists),
			Headers:      maps.Clone(storedHeaders),
			Payload:      projectedPayload,
			PayloadBytes: normRes.PayloadBytes,
			Status:       store.TriggerEventStatusPending,
			CreatedAt:    now,
		}

		var dupEventID string
		txErr := s.store.WithTx(ctx, func(tx store.Store) error {
			if lErr := tx.LockTriggerQuota(ctx, proj.ID); lErr != nil {
				return lErr
			}
			existing, gErr := tx.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindGitHub, normRes.SourceID, ev.DedupKey)
			if gErr == nil && existing != nil {
				dupEventID = existing.ID
				return errGitHubDuplicateDelivery
			}
			if gErr != nil && !errors.Is(gErr, store.ErrNotFound) {
				return gErr
			}

			txCounter, ok := tx.(store.DurableQueuePendingCounter)
			if !ok {
				return errGitHubMissingPendingCounterTx
			}
			pending, cErr := txCounter.CountDurableQueuePending(ctx, trigger.EventQueueKind, proj.ID)
			if cErr != nil {
				return cErr
			}
			if pending >= trigger.MaxPendingEventsPerProject {
				return errGitHubProjectQueueFull
			}

			_, enqErr := trigger.EnqueueEvent(ctx, tx, tx, queue, &ev)
			return enqErr
		})

		if txErr == nil {
			anyQueued = true
			events = append(events, GitHubWebhookProjectEvent{
				ProjectID: proj.ID,
				EventID:   ev.ID,
				Status:    GitHubWebhookStatusQueued,
			})
			continue
		}

		if errors.Is(txErr, errGitHubProjectQueueFull) {
			skipped = append(skipped, GitHubWebhookSkippedProject{
				ProjectID: proj.ID,
				Reason:    GitHubWebhookSkipReasonQueueFull,
			})
			continue
		}

		if errors.Is(txErr, errGitHubDuplicateDelivery) ||
			(!errors.Is(txErr, durablequeue.ErrDuplicate) && errors.Is(txErr, store.ErrAlreadyExists)) {
			if dupEventID == "" {
				existing, gErr := s.store.GetTriggerEventByDedupKey(ctx, store.TriggerSourceKindGitHub, normRes.SourceID, ev.DedupKey)
				if gErr != nil || existing == nil {
					if anyQueued {
						queue.Wake(trigger.EventQueueKind)
					}
					slog.Error("Failed to resolve GitHub trigger event after unique conflict",
						"project_id", proj.ID,
						"installation_id", inst.InstallationID,
						"delivery_id", deliveryID,
						"error", txErr,
						"lookup_error", gErr,
					)
					writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to persist github trigger event", nil)
					return
				}
				dupEventID = existing.ID
			}
			events = append(events, GitHubWebhookProjectEvent{
				ProjectID: proj.ID,
				EventID:   dupEventID,
				Status:    GitHubWebhookStatusDuplicate,
			})
			continue
		}

		if anyQueued {
			queue.Wake(trigger.EventQueueKind)
		}
		slog.Error("Failed to persist GitHub trigger event",
			"project_id", proj.ID,
			"installation_id", inst.InstallationID,
			"delivery_id", deliveryID,
			"error", txErr,
		)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "failed to persist github trigger event", nil)
		return
	}

	if anyQueued {
		queue.Wake(trigger.EventQueueKind)
	}

	// Step 10: 202 response body + content-free trigger.event.received audit.
	topStatus := GitHubWebhookStatusSkipped
	switch {
	case anyQueued:
		topStatus = GitHubWebhookStatusQueued
	case len(events) > 0:
		topStatus = GitHubWebhookStatusDuplicate
	}

	s.githubWebhookIngress.recordReceiptAudit(ctx, githubReceiptAuditRecord{
		Delivery:           deliveryID,
		Event:              eventType,
		Action:             normRes.Attributes["action"],
		InstallationID:     inst.InstallationID,
		Repo:               normRes.RepoFullName,
		PayloadBytes:       normRes.PayloadBytes,
		Result:             topStatus,
		Routing:            auditRouting,
		BypassedProjectIDs: bypassed,
		ProjectIDs:         selectedIDs,
		Events:             events,
		SkippedProjects:    skipped,
	})

	writeJSON(w, http.StatusAccepted, GitHubWebhookAcceptedResponse{
		Status:          topStatus,
		Delivery:        deliveryID,
		Events:          events,
		SkippedProjects: skipped,
	})
}

func (s *Server) listGitHubInstallationCandidateProjects(ctx context.Context, installationID int64, repoFullName string) ([]store.Project, error) {
	var (
		candidates []store.Project
		cursor     string
	)
	for {
		page, err := s.store.ListProjects(ctx, store.ProjectFilter{
			GitHubInstallationID: &installationID,
		}, store.ListOptions{
			Limit:          githubCandidateProjectsPageSize,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if err != nil {
			return nil, err
		}
		for i := range page.Items {
			proj := page.Items[i]
			if s.IsGitHubTriggerCandidate(installationID, &proj, repoFullName) {
				candidates = append(candidates, proj)
			}
		}
		if page.NextCursor == "" || len(page.Items) == 0 {
			return candidates, nil
		}
		cursor = page.NextCursor
	}
}

type githubContentEnvelope struct {
	installationID int64
	action         string
	actionOK       bool
	repoFullName   string
	repoOK         bool
	senderLogin    string
	senderType     string
	senderOK       bool
}

func parseGitHubContentEnvelope(payload []byte) (githubContentEnvelope, error) {
	if len(payload) == 0 || !utf8.Valid(payload) || bytes.ContainsRune(payload, '\x00') {
		return githubContentEnvelope{}, errors.New("invalid payload")
	}

	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return githubContentEnvelope{}, errors.New("invalid payload")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return githubContentEnvelope{}, errors.New("invalid payload")
	}

	instRaw, ok := root["installation"].(map[string]any)
	if !ok || instRaw == nil {
		return githubContentEnvelope{}, errors.New("missing installation")
	}
	instID, ok := parsePositiveJSONInt64(instRaw["id"])
	if !ok {
		return githubContentEnvelope{}, errors.New("invalid installation.id")
	}

	out := githubContentEnvelope{
		installationID: instID,
	}

	if actionRaw, ok := root["action"].(string); ok && isValidGitHubActionIdent(actionRaw) {
		out.action = strings.ToLower(actionRaw)
		out.actionOK = true
	}

	if repoMap, ok := root["repository"].(map[string]any); ok && repoMap != nil {
		if fullName, ok := repoMap["full_name"].(string); ok && isValidGitHubOwnerRepo(fullName) {
			out.repoFullName = fullName
			out.repoOK = true
		}
	}

	if senderMap, ok := root["sender"].(map[string]any); ok && senderMap != nil {
		login, hasLogin := senderMap["login"].(string)
		if hasLogin && len(login) > 0 && len(login) <= maxGitHubEnvelopeLoginBytes &&
			utf8.ValidString(login) && isPrintableNonSpaceString(login) {
			if rawType, exists := senderMap["type"]; exists && rawType != nil {
				typeStr, isStr := rawType.(string)
				if !isStr || !utf8.ValidString(typeStr) {
					return githubContentEnvelope{}, errors.New("invalid sender.type")
				}
				out.senderType = strings.TrimSpace(typeStr)
			}
			out.senderLogin = login
			out.senderOK = true
		}
	}

	return out, nil
}

func parsePositiveJSONInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		s := n.String()
		if strings.ContainsAny(s, ".eE") {
			return 0, false
		}
		parsed, err := strconv.ParseInt(s, 10, 64)
		return parsed, err == nil && parsed > 0
	case float64:
		if n <= 0 || float64(int64(n)) != n {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

func isValidGitHubDeliveryGUID(s string) bool {
	if s == "" || len(s) > GitHubWebhookMaxDeliveryBytes || len(s) != 36 {
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
	return uuid.Validate(s) == nil
}

func isValidGitHubActionIdent(s string) bool {
	if s == "" || len(s) > maxGitHubEnvelopeActionBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-'
		if !ok {
			return false
		}
	}
	return true
}

func isValidGitHubOwnerRepo(fullName string) bool {
	if fullName == "" || len(fullName) > 200 || !utf8.ValidString(fullName) || !isPrintableNonSpaceString(fullName) {
		return false
	}
	slash := strings.IndexByte(fullName, '/')
	if slash <= 0 || slash == len(fullName)-1 {
		return false
	}
	if strings.IndexByte(fullName[slash+1:], '/') >= 0 {
		return false
	}
	owner := fullName[:slash]
	repo := fullName[slash+1:]
	return owner != "." && owner != ".." && repo != "." && repo != ".."
}

func isPrintableNonSpaceString(s string) bool {
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func sanitizeGitHubEventHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		maxBytes := maxGitHubStoredHeaderBytes
		if strings.EqualFold(k, "User-Agent") {
			maxBytes = maxGitHubStoredUserAgentBytes
		}
		out[k] = sanitizeGitHubHeaderValue(v, maxBytes)
	}
	return out
}

func sanitizeGitHubHeaderValue(val string, maxBytes int) string {
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
