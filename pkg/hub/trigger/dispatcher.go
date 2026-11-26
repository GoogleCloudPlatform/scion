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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/source"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

// Trigger drainer, fire budget, and maintenance defaults (Stage 1 §4.3, §4.4,
// §6.1, §6.3, §6.4, rev 7.2, rev 7.4).
const (
	DefaultPollInterval   = 2 * time.Second
	DefaultHandlerTimeout = 150 * time.Second
	DefaultLeaseDuration  = 180 * time.Second
	DefaultMaxRows        = 500_000
	DefaultBatchSize      = 4
	DefaultMaxAge         = 72 * time.Hour
	DefaultMaxPermanent   = 5

	DefaultPerFireTimeout     = 30 * time.Second
	DefaultEventRuleBudget    = time.Second
	DefaultEventCompileBudget = triggerrule.EventCompileBudget
	FireLeaseHorizon          = 10 * time.Minute
	HistoryRetention          = 7 * 24 * time.Hour
	MaxFireSubjectBytes       = 256

	DefaultReapBatchLimit      = 500
	DefaultReconcileBatchLimit = 500

	ExecutorKindTriggerDrainer   = "trigger_drainer"
	ExecutorIDPrefixTriggerEvent = "trigger_event:"

	// Content-free generic error texts persisted on trigger_events / trigger_fires.
	ErrEventDroppedByQueueLimits         = "dropped by queue limits"
	ErrEventProcessingFailed             = "processing failed"
	ErrFireRuleBudgetExhausted           = "rule budget exhausted"
	ErrFireRuleCompileBudgetExhausted    = triggerrule.SkipReasonCompileBudgetExhausted
	ErrFireRuleEvaluationFailed          = "rule evaluation failed"
	ErrFireRuleUncertain                 = "rule uncertain"
	ErrFireRuleNoLongerCompiles          = "rule no longer compiles"
	ErrFireGitHubAssociationUnauthorized = "github association not authorized"
	ErrFireSubjectBusy                   = "subject busy"
	ErrFireSubjectDispatchInProgress     = "subject dispatch in progress"
	ErrFireCoalesced                     = "coalesced"
	ErrFireCoalesceCancelled             = "coalesce cancelled"
	ErrFireDeferCancelled                = "defer cancelled"
	ErrFireStaleEvent                    = "stale event"
	ErrFireReapedDuringCoalesce          = "hub restarted during coalesce"
	ErrFireReapedDuringDefer             = "hub restarted during defer"
	ErrFireTargetAgentStillStarting      = "target agent still starting"
	ErrFireRateLimit                     = "rate limit"
	ErrFireConcurrencyLimit              = "concurrency limit"
	ErrFireProjectTriggerAgentCap        = "project trigger-agent cap"

	// ReuseDeferBackoff is the bounded retry delay when a reuse target agent's
	// create launch is still in flight (Stage 3 §6.2 rev 1.16).
	ReuseDeferBackoff = 10 * time.Second
	// ReuseDeferTimeout bounds total deferral since the fire's first handling
	// before failing with ErrFireTargetAgentStillStarting.
	ReuseDeferTimeout = 10 * time.Minute

	// Closed-set skipped-fire metric reasons (§6.5, Stage 2 §7.4). Free-text
	// errors are never emitted as metric labels.
	FireSkipReasonRuleBudgetExhausted        = "rule_budget_exhausted"
	FireSkipReasonRuleCompileBudgetExhausted = "rule_compile_budget_exhausted"
	FireSkipReasonRuleEvaluationFailed       = "rule_evaluation_failed"
	FireSkipReasonRuleUncertain              = "rule_uncertain"
	FireSkipReasonRuleNoLongerCompiles       = "rule_no_longer_compiles"
	FireSkipReasonSubjectBusy                = "subject_busy"
	FireSkipReasonSubjectDispatchInProgress  = "subject_dispatch_in_progress"
	FireSkipReasonCoalesced                  = "coalesced"
	FireSkipReasonCoalesceCancelled          = "coalesce_cancelled"
	FireSkipReasonDeferCancelled             = "defer_cancelled"
	FireSkipReasonAgentNameInUse             = "agent_name_in_use"
	FireSkipReasonCommentRateLimited         = "comment_rate_limited"
	FireSkipReasonOfflineQueueFull           = "offline_queue_full"
	FireSkipReasonTriggerChanged             = "trigger_changed"
	FireSkipReasonEventDeleted               = "event_deleted"
	FireSkipReasonTargetDeleted              = "target_deleted"
	FireSkipReasonTargetNotRunning           = "target_not_running"
	FireSkipReasonRateLimit                  = "rate_limit"
	FireSkipReasonConcurrency                = "concurrency"
	FireSkipReasonProjectCap                 = "project_cap"
	FireSkipReasonOther                      = "other"
)

// FireSkipReasons returns the closed set of skipped-fire metric reason labels
// in canonical order. Metrics recorders use this as the single source of truth
// so newly added FireSkipReason* constants are never silently bucketed as "other".
func FireSkipReasons() []string {
	return []string{
		FireSkipReasonRuleBudgetExhausted,
		FireSkipReasonRuleCompileBudgetExhausted,
		FireSkipReasonRuleEvaluationFailed,
		FireSkipReasonRuleUncertain,
		FireSkipReasonRuleNoLongerCompiles,
		FireSkipReasonSubjectBusy,
		FireSkipReasonSubjectDispatchInProgress,
		FireSkipReasonCoalesced,
		FireSkipReasonCoalesceCancelled,
		FireSkipReasonDeferCancelled,
		FireSkipReasonAgentNameInUse,
		FireSkipReasonCommentRateLimited,
		FireSkipReasonOfflineQueueFull,
		FireSkipReasonTriggerChanged,
		FireSkipReasonEventDeleted,
		FireSkipReasonTargetDeleted,
		FireSkipReasonTargetNotRunning,
		FireSkipReasonRateLimit,
		FireSkipReasonConcurrency,
		FireSkipReasonProjectCap,
		FireSkipReasonOther,
	}
}

// NormalizeFireSkipReason maps a skipped trigger fire's persisted error text
// onto a closed-set metric label value so label cardinality stays bounded.
func NormalizeFireSkipReason(errMsg string) string {
	switch strings.TrimSpace(errMsg) {
	case ErrFireRuleBudgetExhausted:
		return FireSkipReasonRuleBudgetExhausted
	case ErrFireRuleCompileBudgetExhausted:
		return FireSkipReasonRuleCompileBudgetExhausted
	case ErrFireRuleEvaluationFailed:
		return FireSkipReasonRuleEvaluationFailed
	case ErrFireRuleUncertain:
		return FireSkipReasonRuleUncertain
	case ErrFireRuleNoLongerCompiles:
		return FireSkipReasonRuleNoLongerCompiles
	case ErrFireSubjectBusy:
		return FireSkipReasonSubjectBusy
	case ErrFireSubjectDispatchInProgress:
		return FireSkipReasonSubjectDispatchInProgress
	case ErrFireCoalesced:
		return FireSkipReasonCoalesced
	case ErrFireCoalesceCancelled:
		return FireSkipReasonCoalesceCancelled
	case ErrFireDeferCancelled:
		return FireSkipReasonDeferCancelled
	case "agent name in use":
		return FireSkipReasonAgentNameInUse
	case "comment rate limited":
		return FireSkipReasonCommentRateLimited
	case ErrFireOfflineQueueFull:
		return FireSkipReasonOfflineQueueFull
	case ErrFireTriggerChanged:
		return FireSkipReasonTriggerChanged
	case ErrFireEventDeleted:
		return FireSkipReasonEventDeleted
	case ErrFireTargetDeleted:
		return FireSkipReasonTargetDeleted
	case ErrFireTargetNotRunning:
		return FireSkipReasonTargetNotRunning
	case ErrFireRateLimit:
		return FireSkipReasonRateLimit
	case ErrFireConcurrencyLimit:
		return FireSkipReasonConcurrency
	case ErrFireProjectTriggerAgentCap:
		return FireSkipReasonProjectCap
	default:
		return FireSkipReasonOther
	}
}

// DispatcherMetrics observes terminal trigger fire outcomes with bounded labels.
// skipReason is empty for non-skipped statuses and a FireSkipReason* constant
// when status == store.TriggerFireStatusSkipped.
type DispatcherMetrics interface {
	RecordTriggerFire(actionType, status, skipReason string)
}

// Core is the narrow capability interface package hub/trigger consumes from
// package hub without importing package hub (Stage 1 §11 D-S1-1(c)).
type Core interface {
	Fire(ctx context.Context, req hubfire.FireRequest) hubfire.FireOutcome
	Experiment(name string) bool
}

// FireAdmitter is an optional interface Core implementations may satisfy so
// long-running trigger fires (such as message --wake) register with hub
// shutdown admission and receive hub shutdown-abort signals while remaining
// bounded by itemCtx (the queue item's lifetime, Stage 1 ledger L3).
//
// admittedCtx preserves the caller's per-fire deadline alongside any abort
// values; abortCtx carries ONLY the real abort signal (hub shutdown + queue
// item lifetime, never the per-fire deadline) so Detach can outlive a wake
// resume longer than PerFireTimeout without outliving shutdown or itemCtx.
type FireAdmitter interface {
	AdmitFire(ctx, itemCtx context.Context) (admittedCtx, abortCtx context.Context, cleanup func(), ok bool)
}

// SourceAuthorizer is an optional interface Core implementations may satisfy
// to verify source-level authorization (such as GitHub App installation
// association on the project, Stage 3 §4.2 step 4) once per event before any
// matched rule fires. Returning authorized == false with err == nil records
// every matched candidate fire on the event as refused and marks the event
// processed; returning a non-nil err treats the check as interrupted (for
// example drainer context cancellation) so the queue item retries. For github
// events, a Core that does not implement SourceAuthorizer fails closed with a
// retryable internal error rather than firing.
type SourceAuthorizer interface {
	AuthorizeEventSource(ctx context.Context, ev *store.TriggerEvent) (authorized bool, err error)
}

// EventEvaluationOutcome summarizes the candidate-loop outcome for an event
// before it is marked processed.
type EventEvaluationOutcome struct {
	MatchCount      int
	FireCount       int
	AnyFireRecorded bool
}

// PostEvaluationHook is an optional interface Core implementations may satisfy
// to observe an event once after all candidate rules have been evaluated (and
// before the event is marked processed).
type PostEvaluationHook interface {
	OnEventEvaluated(ctx context.Context, ev *store.TriggerEvent, outcome EventEvaluationOutcome, attempts int)
}

// SourceEnricher is an optional interface Core implementations may satisfy to
// enrich a trigger event with source-fetched facts once per event before rule
// evaluation (Stage 3 §5.5). It aliases source.Enricher so the drainer and
// provider-free source seam share the exact same interface type.
type SourceEnricher = source.Enricher

// ReuseMessageRenderer is an optional interface Core implementations may
// satisfy to render a dispatch_agent trigger's reuseMessage template slot
// (triggertemplate.SlotMessage) with full project/source template variables
// for subject_mode="reuse" delivery (Stage 3 §6.2).
type ReuseMessageRenderer interface {
	RenderReuseMessage(ctx context.Context, req hubfire.FireRequest) (text string, refusal *hubfire.FireOutcome, err error)
}

// PreFireRefuser is an optional interface Core implementations may satisfy to
// refuse a matched trigger fire before fire-row claiming, coalescing, deferral,
// or subject reuse (for example, source-specific bot/own-App identity guards).
// Returning refused == true records the fire as refused with the sanitized
// reason (and optional auditReason) and skips claiming, coalescing, and reuse.
type PreFireRefuser interface {
	PreFireRefusal(ctx context.Context, tr *store.Trigger, actionType string, ev *hubfire.FireEvent) (reason, auditReason string, refused bool)
}

type triggerActionKey struct {
	triggerID   string
	actionIndex int
}

func firstTriggerActionType(tr *store.Trigger) string {
	if tr == nil {
		return "unknown"
	}
	if actions, err := Actions(*tr); err == nil && len(actions) > 0 {
		return actions[0].Type
	}
	if t := strings.TrimSpace(tr.ActionType); t != "" {
		return t
	}
	return "unknown"
}

// DispatcherConfig configures a trigger event Dispatcher.
type DispatcherConfig struct {
	Core           Core
	Store          store.TriggerStore
	Storage        func() storage.Storage
	SubjectClaims  SubjectClaims
	Queue          durablequeue.Queue
	TxRunner       func(ctx context.Context, fn func(ctx context.Context, tx store.Tx, ts store.TriggerStore) error) error
	Rules          *triggerrule.Rules
	RuleMetrics    RuleEvalMetrics
	FireAudit      FireAuditSink
	PerFireTimeout time.Duration
	RuleBudget     time.Duration
	CompileBudget  time.Duration
	Metrics        DispatcherMetrics
	Logger         *slog.Logger
	Now            func() time.Time
}

// DrainerOptions configures a durablequeue.Drainer driving a Dispatcher.
type DrainerOptions struct {
	QueueStore          store.DurableQueueStore
	Queue               durablequeue.Queue
	Owner               string
	PollInterval        time.Duration
	LeaseDuration       time.Duration
	HandlerTimeout      time.Duration
	BatchSize           int
	MaxRows             int
	MaxPermanent        int
	MaxAge              time.Duration
	MaintenanceInterval time.Duration
	ShutdownBudget      time.Duration
	Metrics             durablequeue.Metrics
}

// Dispatcher consumes trigger.event items from durablequeue, matches active
// triggers, claims idempotent trigger_fires rows, invokes Core.Fire, and marks
// trigger_events rows processed (§6.3).
//
// Security L1 seam note (ptone/scion#8030; mixed-rule-arrays §7): consumer
// "trigger" remains admitted exclusively to pure, no-I/O Grant("cel"), where
// celengine.Program.Eval enforces a post-return wall-clock deadline backstop.
// Admitting any second dialect to consumer "trigger" requires a dialect-level
// or engine-level post-return deadline check so evaluation never trusts a
// dialect to honor context cancellation.
type Dispatcher struct {
	core           Core
	store          store.TriggerStore
	storageFn      func() storage.Storage
	claims         SubjectClaims
	queue          durablequeue.Queue
	txRunner       func(ctx context.Context, fn func(ctx context.Context, tx store.Tx, ts store.TriggerStore) error) error
	rules          *triggerrule.Rules
	ruleMetrics    RuleEvalMetrics
	fireAudit      FireAuditSink
	perFireTimeout time.Duration
	ruleBudget     time.Duration
	compileBudget  time.Duration
	metrics        DispatcherMetrics
	logger         *slog.Logger
	now            func() time.Time

	statsMu       sync.Mutex
	evalStats     map[RuleEvalKey]RuleEvalBucket
	compileMisses map[string]int64
}

// NewDispatcher validates cfg and constructs a Dispatcher.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
	if cfg.Core == nil {
		return nil, errors.New("trigger: dispatcher requires Core")
	}
	if cfg.Store == nil {
		return nil, errors.New("trigger: dispatcher requires Store")
	}
	perFire := cfg.PerFireTimeout
	if perFire <= 0 {
		perFire = DefaultPerFireTimeout
	}
	ruleBudget := cfg.RuleBudget
	if ruleBudget <= 0 {
		ruleBudget = DefaultEventRuleBudget
	}
	compileBudget := cfg.CompileBudget
	if compileBudget <= 0 {
		compileBudget = DefaultEventCompileBudget
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	fireAudit := cfg.FireAudit
	if fireAudit == nil {
		fireAudit = NewSlogAuditSink(SlogAuditConfig{Now: nowFn})
	}
	return &Dispatcher{
		core:           cfg.Core,
		store:          cfg.Store,
		storageFn:      cfg.Storage,
		claims:         cfg.SubjectClaims,
		queue:          cfg.Queue,
		txRunner:       cfg.TxRunner,
		rules:          cfg.Rules,
		ruleMetrics:    cfg.RuleMetrics,
		fireAudit:      fireAudit,
		perFireTimeout: perFire,
		ruleBudget:     ruleBudget,
		compileBudget:  compileBudget,
		metrics:        cfg.Metrics,
		logger:         logger,
		now:            nowFn,
		evalStats:      make(map[RuleEvalKey]RuleEvalBucket),
		compileMisses:  make(map[string]int64),
	}, nil
}

// RuleEvalStats returns a point-in-time snapshot of per-(dialect, class) rule
// evaluation counts and durations observed by this Dispatcher.
func (d *Dispatcher) RuleEvalStats() map[RuleEvalKey]RuleEvalBucket {
	if d == nil {
		return nil
	}
	d.statsMu.Lock()
	defer d.statsMu.Unlock()
	out := make(map[RuleEvalKey]RuleEvalBucket, len(d.evalStats))
	maps.Copy(out, d.evalStats)
	return out
}

func (d *Dispatcher) recordRuleEval(ctx context.Context, dialect, class string, elapsed time.Duration) {
	if d == nil {
		return
	}
	dName := normalizeMetricDialect(dialect, d.rules)
	cName := normalizeMetricClass(class)
	dur := max(elapsed, time.Duration(0))

	d.statsMu.Lock()
	key := RuleEvalKey{Dialect: dName, Class: cName}
	b := d.evalStats[key]
	b.Count++
	b.TotalDuration += dur
	d.evalStats[key] = b
	d.statsMu.Unlock()

	if d.ruleMetrics != nil {
		d.ruleMetrics.ObserveRuleEval(ctx, dName, cName, dur)
	}
}

func (d *Dispatcher) recordFireMetric(actionType, status, errMsg string) {
	if d == nil || d.metrics == nil {
		return
	}
	skipReason := ""
	if status == store.TriggerFireStatusSkipped {
		skipReason = NormalizeFireSkipReason(errMsg)
	}
	d.metrics.RecordTriggerFire(actionType, status, skipReason)
}

// RuleCompileMissStats returns a point-in-time snapshot of per-dialect rule
// cache compile misses observed by this Dispatcher.
func (d *Dispatcher) RuleCompileMissStats() map[string]int64 {
	if d == nil {
		return nil
	}
	d.statsMu.Lock()
	defer d.statsMu.Unlock()
	out := make(map[string]int64, len(d.compileMisses))
	maps.Copy(out, d.compileMisses)
	return out
}

func (d *Dispatcher) recordRuleCompileMiss(ctx context.Context, dialect string) {
	if d == nil {
		return
	}
	dName := normalizeMetricDialect(dialect, d.rules)

	d.statsMu.Lock()
	d.compileMisses[dName]++
	d.statsMu.Unlock()

	if cm, ok := d.ruleMetrics.(RuleCompileMissMetrics); ok {
		cm.ObserveRuleCompileMiss(ctx, dName)
	}
}

// NewDrainer constructs the trigger.event durablequeue.Drainer with Stage 1
// §6.1 / §6.3 / rev 7.2 / rev 7.4 bounds: HandlerTimeout 150s, LeaseDuration
// 180s, MaxRows 500_000 (explicit), BatchSize 4, MaxAge 72h, MaxPermanent 5,
// Enabled gated on Core.Experiment(experiments.Triggers), and OnDrop marking
// dropped trigger_events rows failed.
func (d *Dispatcher) NewDrainer(opts DrainerOptions) (*durablequeue.Drainer, error) {
	if d == nil {
		return nil, errors.New("trigger: dispatcher is nil")
	}
	if opts.QueueStore == nil {
		return nil, errors.New("trigger: drainer requires QueueStore")
	}
	poll := opts.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	handlerTimeout := opts.HandlerTimeout
	if handlerTimeout <= 0 {
		handlerTimeout = DefaultHandlerTimeout
	}
	leaseDur := opts.LeaseDuration
	if leaseDur <= 0 {
		leaseDur = DefaultLeaseDuration
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultBatchSize
	}
	maxRows := opts.MaxRows
	if maxRows <= 0 {
		maxRows = DefaultMaxRows
	}
	maxPerm := opts.MaxPermanent
	if maxPerm <= 0 {
		maxPerm = DefaultMaxPermanent
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}

	return durablequeue.NewDrainer(durablequeue.DrainerConfig{
		Store:   opts.QueueStore,
		Queue:   opts.Queue,
		Kind:    EventQueueKind,
		Handler: d.HandleItem,
		Owner:   opts.Owner,
		Enabled: func() bool {
			return d.core.Experiment(experiments.Triggers)
		},
		PollInterval:        poll,
		LeaseDuration:       leaseDur,
		HandlerTimeout:      handlerTimeout,
		BatchSize:           batchSize,
		FairByProject:       true,
		MaxRows:             maxRows,
		MaxPermanent:        maxPerm,
		MaxAge:              maxAge,
		MaintenanceInterval: opts.MaintenanceInterval,
		ShutdownBudget:      opts.ShutdownBudget,
		OnDrop:              d.onQueueDrop,
		Metrics:             opts.Metrics,
		Logger:              d.logger,
		Now:                 d.now,
	})
}

func (d *Dispatcher) onQueueDrop(ctx context.Context, dropped []durablequeue.Item, reason string) {
	errMsg := ErrEventDroppedByQueueLimits
	if reason == durablequeue.DropReasonPoison {
		errMsg = ErrEventProcessingFailed
	}
	now := d.now().UTC()
	for _, it := range dropped {
		if it.DedupKey == "" {
			continue
		}
		if err := d.store.MarkTriggerEventFailed(ctx, it.DedupKey, now, errMsg); err != nil &&
			!errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
			d.logger.Warn("trigger: failed to mark dropped event failed",
				"eventID", it.DedupKey, "reason", reason, "error", err)
		}
	}
}

// HandleItem processes one leased trigger.event queue item (§6.3).
func (d *Dispatcher) HandleItem(ctx context.Context, item durablequeue.Item, lease durablequeue.Lease) durablequeue.Result {
	var qp EventQueuePayload
	if err := json.Unmarshal(item.Payload, &qp); err != nil || qp.Version != EventQueuePayloadVersion || qp.EventID == "" {
		return durablequeue.Result{
			Permanent: true,
			Err:       errors.New("trigger: invalid event queue payload"),
		}
	}

	ev, err := d.store.GetTriggerEvent(ctx, qp.EventID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalidInput) {
			return durablequeue.Result{Done: true}
		}
		d.logger.Warn("trigger: failed to load trigger event", "eventID", qp.EventID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: load event failed")}
	}
	if ev.Status != store.TriggerEventStatusPending {
		return durablequeue.Result{Done: true}
	}
	if ev.SourceKind == store.TriggerSourceKindGitHub && !d.core.Experiment(experiments.GitHubTriggers) {
		// Stage 3 §8.2: while hub.github_triggers is off, existing github triggers
		// and queued events wait without counting toward poison drops.
		return durablequeue.Result{
			RetryAt: d.now().UTC().Add(time.Minute),
			Err:     errors.New("trigger: github triggers experiment disabled"),
		}
	}

	candidates, err := d.loadActiveTriggers(ctx, ev.ProjectID, ev.SourceKind, ev.SourceID)
	if err != nil {
		d.logger.Warn("trigger: failed to list active triggers",
			"eventID", ev.ID, "projectID", ev.ProjectID, "sourceKind", ev.SourceKind, "sourceID", ev.SourceID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: list candidates failed")}
	}

	var rawPayload []byte
	if ev.Payload != "" {
		rawPayload = []byte(ev.Payload)
	}
	baseEvent := hubfire.FireEvent{
		ID:         ev.ID,
		ProjectID:  ev.ProjectID,
		SourceKind: ev.SourceKind,
		SourceID:   ev.SourceID,
		Type:       ev.Type,
		Subject:    ev.Subject,
		OccurredAt: ev.OccurredAt,
		ReceivedAt: ev.ReceivedAt,
		Attributes: ev.Attributes,
		Lists:      ev.Lists,
		Payload:    rawPayload,
	}
	budget := ruleengine.NewBudgetTracker(d.ruleBudget)
	// SEC-3b2-01: separate per-event compile budget charged only on cache misses
	// (never charged to eval budget, preserving F2).
	compileBudget := ruleengine.NewBudgetTracker(d.compileBudget)
	onCompileMiss := func(dialect string) {
		d.recordRuleCompileMiss(ctx, dialect)
	}

	var (
		matchCount            int
		fireCount             int
		hadAnyFire            bool
		sourceAuthChecked     bool
		sourceAuthorized      bool
		enrichmentChecked     bool
		webhookEPChecked      bool
		webhookEP             *store.WebhookEndpoint
		earliestRetryAt       time.Time
		stillCoalescingFires  = make(map[triggerActionKey]bool)
		baseActBuilt          bool
		overflowPreviewLoaded bool
		overflowPreview       []byte
		baseAct               triggerrule.Activation
		actErr                error
	)

	for i := range candidates {
		if ctx.Err() != nil {
			return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
		}
		tr := &candidates[i]

		typeStart := d.now()
		matched := triggerrule.MatchTypes(tr.MatchTypes, ev.Type)
		budget.Charge(max(d.now().Sub(typeStart), time.Duration(0)))
		if !matched {
			continue
		}

		// Stage 3 §4.2 step 4: before any enrichment, rule evaluation, or fire on a
		// github event, verify once per event that the project's GitHub App
		// installation association is authorized now. Running this ahead of
		// enrichment and rule evaluation ensures an unauthorized event never mints
		// a token, never calls out to GitHub, and never records rule-skip outcomes
		// that mask the refusal.
		if ev.SourceKind == store.TriggerSourceKindGitHub {
			if !sourceAuthChecked {
				if lease != nil {
					held, lErr := lease.Renew(ctx)
					if lErr != nil {
						return durablequeue.Result{Err: errors.New("trigger: renew queue lease failed")}
					}
					if !held {
						return durablequeue.Result{Err: errors.New("trigger: queue lease lost")}
					}
				}
				sourceAuthChecked = true
				sa, ok := d.core.(SourceAuthorizer)
				if !ok {
					d.logger.Error("trigger: core does not implement SourceAuthorizer for github event",
						"eventID", ev.ID, "projectID", ev.ProjectID, "sourceID", ev.SourceID)
					return durablequeue.Result{Err: errors.New("trigger: source authorizer unavailable")}
				}
				var saErr error
				sourceAuthorized, saErr = sa.AuthorizeEventSource(ctx, ev)
				if saErr != nil {
					d.logger.Warn("trigger: failed to authorize event source",
						"eventID", ev.ID, "projectID", ev.ProjectID, "sourceID", ev.SourceID, "error", saErr)
					return durablequeue.Result{Err: errors.New("trigger: authorize event source failed")}
				}
			}
			if !sourceAuthorized {
				matchCount++
				hadAnyFire = true
				if rErr := d.recordDirectFire(
					ctx, tr, ev, deriveFireSubject(ev.Subject),
					store.TriggerFireStatusRefused, ErrFireGitHubAssociationUnauthorized,
					"0/0", "", "", "github_association_unauthorized",
				); rErr != nil {
					return durablequeue.Result{Err: errors.New("trigger: record refused fire failed")}
				}
				continue
			}
		}

		// Stage 3 §5.5: consult optional SourceEnricher once per event after
		// MatchTypes prefiltering and AuthorizeEventSource succeed, and persist
		// updated attributes/lists on the event row BEFORE any rule is evaluated.
		if !enrichmentChecked {
			enrichmentChecked = true
			if enricher, ok := d.core.(SourceEnricher); ok && enricher.NeedsEnrichment(&baseEvent) {
				if lease != nil {
					held, lErr := lease.Renew(ctx)
					if lErr != nil {
						return durablequeue.Result{Err: errors.New("trigger: renew queue lease failed")}
					}
					if !held {
						return durablequeue.Result{Err: errors.New("trigger: queue lease lost")}
					}
				}
				enrichedAttrs, enrichedLists, enrichErr := enricher.Enrich(ctx, &baseEvent)
				if ctx.Err() != nil {
					return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
				}
				if enrichErr != nil {
					d.logger.Warn("trigger: source enrichment failed",
						"eventID", ev.ID, "projectID", ev.ProjectID, "sourceKind", ev.SourceKind, "error", enrichErr)
					if enrichedAttrs != nil {
						// Use the enricher's ceiling-checked failure attributes (normally
						// carrying enriched="failed", or preserving existing attributes if
						// adding enriched="failed" would exceed the 8 KiB bound).
						ev.Attributes = enrichedAttrs
						if enrichedLists != nil {
							ev.Lists = enrichedLists
						}
					} else {
						// Generic enricher error with no returned attribute map: mark
						// enriched="failed" so lost-lease re-handles short-circuit.
						if ev.Attributes == nil {
							ev.Attributes = map[string]string{"enriched": "failed"}
						} else if _, already := ev.Attributes["enriched"]; !already {
							updated := maps.Clone(ev.Attributes)
							updated["enriched"] = "failed"
							ev.Attributes = updated
						}
					}
				} else {
					if enrichedAttrs != nil {
						ev.Attributes = enrichedAttrs
					}
					if enrichedLists != nil {
						ev.Lists = enrichedLists
					}
				}
				if uErr := d.store.UpdateTriggerEventAttributes(ctx, ev.ID, ev.Attributes, ev.Lists); uErr != nil &&
					!errors.Is(uErr, store.ErrNotFound) {
					d.logger.Warn("trigger: failed to persist enriched event attributes",
						"eventID", ev.ID, "projectID", ev.ProjectID, "error", uErr)
					return durablequeue.Result{Err: errors.New("trigger: persist enriched attributes failed")}
				}
				baseEvent.Attributes = ev.Attributes
				baseEvent.Lists = ev.Lists
			}
		}

		// Parse payload JSON once per event via NewEventActivation (after any
		// enrichment has persisted) so every candidate trigger rule shares one
		// parsed activation and one aggregate per-event BudgetTracker (§6.3, F5).
		if !baseActBuilt {
			parseStart := d.now()
			baseAct, actErr = triggerrule.NewEventActivation(baseEvent)
			budget.Charge(max(d.now().Sub(parseStart), time.Duration(0)))
			baseActBuilt = true
		}

		// SEC-3b1rw-01: fail closed on corrupt stored match_rules before any fire.
		matchRules, normErr := MatchRules(*tr)
		if normErr != nil {
			remMs := budget.Remaining().Milliseconds()
			traceStr := triggerrule.EncodeRuleTrace(triggerrule.RuleTrace{
				Elements: []triggerrule.RuleTraceElement{{
					Index:   1,
					Dialect: "unknown",
					Class:   triggerrule.MatchClassFailed,
					Reason:  triggerrule.MatchReasonCorruptRules,
				}},
				BudgetRemainingMs: &remMs,
			})
			d.recordRuleEval(ctx, "unknown", triggerrule.MatchClassFailed, 0)
			hadAnyFire = true
			if rErr := d.recordDirectFire(
				ctx, tr, ev, deriveFireSubject(ev.Subject),
				store.TriggerFireStatusSkipped, ErrFireRuleEvaluationFailed,
				"0/0", "", traceStr, triggerrule.MatchReasonCorruptRules,
			); rErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
			}
			continue
		}

		scope := celengine.CacheScope{
			Consumer:  triggerrule.ConsumerName,
			ProjectID: tr.ProjectID,
			ID:        tr.ID,
			Revision:  int64(tr.AuthorizationRevision),
		}
		trigRef := hubfire.FireTriggerRef{
			ID:                      tr.ID,
			Name:                    tr.Name,
			SubjectMode:             tr.SubjectMode,
			MaxFiresPerMinute:       tr.MaxFiresPerMinute,
			MaxConcurrentDispatches: tr.MaxConcurrentDispatches,
		}
		trigAct := baseAct.WithTrigger(trigRef)

		matchedThrough := "0/0"
		decidedDialect := ""
		ruleTraceStr := ""

		if len(matchRules) == 0 {
			if budget.Exhausted() {
				hadAnyFire = true
				if rErr := d.recordDirectFire(
					ctx, tr, ev, deriveFireSubject(ev.Subject),
					store.TriggerFireStatusSkipped, ErrFireRuleBudgetExhausted,
					"0/0", "", "", triggerrule.MatchReasonBudgetExhausted,
				); rErr != nil {
					return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
				}
				continue
			}
		} else {
			if d.rules == nil || actErr != nil {
				firstDialect := triggerrule.SafeDialectName(matchRules[0].Dialect)
				remMs := budget.Remaining().Milliseconds()
				traceStr := triggerrule.EncodeRuleTrace(triggerrule.RuleTrace{
					Elements: []triggerrule.RuleTraceElement{{
						Index:   1,
						Dialect: firstDialect,
						Class:   triggerrule.MatchClassFailed,
						Reason:  triggerrule.MatchReasonEvalFailed,
					}},
					BudgetRemainingMs: &remMs,
				})
				d.recordRuleEval(ctx, firstDialect, triggerrule.MatchClassFailed, 0)
				hadAnyFire = true
				if rErr := d.recordDirectFire(
					ctx, tr, ev, deriveFireSubject(ev.Subject),
					store.TriggerFireStatusSkipped, ErrFireRuleEvaluationFailed,
					fmt.Sprintf("1/%d", len(matchRules)), firstDialect, traceStr, triggerrule.MatchReasonEvalFailed,
				); rErr != nil {
					return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
				}
				continue
			}

			// SEC-3b1rw-01 & SEC-3b2-01: ALWAYS pass WithBudget(budget) and
			// WithCompileBudget(compileBudget) so evaluation and miss-compiles
			// are each bounded by their separate per-event trackers.
			res, trace, evalErr := d.rules.MatchAllWithTrace(
				ctx,
				scope,
				matchRules,
				trigAct,
				triggerrule.WithBudget(budget),
				triggerrule.WithCompileBudget(compileBudget),
				triggerrule.WithCompileMissObserver(onCompileMiss),
			)
			for _, el := range trace.Elements {
				d.recordRuleEval(ctx, el.Dialect, el.Class, el.Elapsed)
			}
			matchedThrough = fmt.Sprintf("%d/%d", res.Through, len(matchRules))
			decidedDialect = res.Dialect
			ruleTraceStr = triggerrule.EncodeRuleTrace(trace)

			if evalErr != nil {
				skipReason, isSkip := triggerrule.SkipReason(evalErr)
				if !isSkip {
					if ctx.Err() != nil || res.Reason == celengine.ReasonCanceled {
						return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
					}
					skipReason = ErrFireRuleEvaluationFailed
				}
				var ce *celengine.CompileError
				if errors.As(evalErr, &ce) {
					d.logger.Warn("trigger: stored match rule no longer compiles",
						"triggerID", tr.ID,
						"eventID", ev.ID,
						"element", res.Through,
						"dialect", res.Dialect,
						"category", ce.Category,
					)
				}
				hadAnyFire = true
				if rErr := d.recordDirectFire(
					ctx, tr, ev, deriveFireSubject(ev.Subject),
					store.TriggerFireStatusSkipped, skipReason,
					matchedThrough, decidedDialect, ruleTraceStr, res.Reason,
				); rErr != nil {
					return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
				}
				continue
			}
			if !res.Matched {
				continue
			}
		}

		matchCount++
		hadAnyFire = true

		// Derived subject_rule output applies to trigger_fires.subject and coalescing
		// only; FireEvent.Subject for the agent envelope keeps the hub-shaped source
		// event subject so sender-derived text stays inside external_payload framing
		// (stage1-design rev 8.34, ledger L23).
		fireSubject := deriveFireSubject(ev.Subject)
		if strings.TrimSpace(tr.SubjectRule) != "" {
			if d.rules == nil || actErr != nil {
				d.logger.Warn("trigger subject_rule skipped; falling back to event subject",
					"trigger_id", tr.ID,
					"event_id", ev.ID,
					"reason", "rule_engine_unavailable",
				)
			} else {
				derived := d.rules.DeriveSubject(
					ctx,
					scope,
					triggerrule.Rule{Dialect: tr.SubjectDialect, Expr: tr.SubjectRule},
					trigAct,
					"",
					d.logger,
					triggerrule.WithBudget(budget),
					triggerrule.WithCompileBudget(compileBudget),
					triggerrule.WithCompileMissObserver(onCompileMiss),
				)
				if ctx.Err() != nil {
					return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
				}
				if derived != "" {
					fireSubject = derived
				}
			}
		}
		trigRef.FireSubject = fireSubject

		actions, actNormErr := Actions(*tr)
		if actNormErr != nil {
			if rErr := d.recordDirectFire(
				ctx, tr, ev, fireSubject,
				store.TriggerFireStatusSkipped, ErrFireRuleEvaluationFailed,
				matchedThrough, decidedDialect, ruleTraceStr, "corrupt_actions",
			); rErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
			}
			continue
		}

		isErrStatus := func(s string) bool {
			return s == store.TriggerFireStatusFailed ||
				s == store.TriggerFireStatusRefused ||
				s == store.TriggerFireStatusExpired
		}
		var (
			trigRecordedCount    int
			trigAggStatus        string
			trigAggErr           string
			trigAggTime          time.Time
			priorTerminalCounted bool
			priorHadError        bool
		)
		notePriorTerminal := func(existing *store.TriggerFire) {
			if existing == nil {
				return
			}
			if isErrStatus(existing.Status) {
				priorHadError = true
			}
			// Distinguish an already-flushed prior pass from a crash between
			// FinishTriggerFire and flushTrigCounters: if FireCount is still 0
			// or LastFireAt predates existing.FinishedAt, the prior pass crashed
			// before flushing, so this retry should still flush fire_count.
			if tr.FireCount > 0 && tr.LastFireAt != nil {
				if existing.FinishedAt == nil || !tr.LastFireAt.Before(existing.FinishedAt.UTC()) {
					priorTerminalCounted = true
				}
			}
		}
		recordTrigOutcome := func(status, errMsg string, at time.Time) {
			trigRecordedCount++
			if at.After(trigAggTime) {
				trigAggTime = at
			}
			switch {
			case trigAggStatus == "":
				trigAggStatus = status
				trigAggErr = errMsg
			case isErrStatus(status):
				if !isErrStatus(trigAggStatus) {
					trigAggStatus = status
					trigAggErr = errMsg
				}
			case status == store.TriggerFireStatusFired && !isErrStatus(trigAggStatus):
				trigAggStatus = status
				trigAggErr = ""
			}
		}
		flushTrigCounters := func() {
			if trigRecordedCount == 0 {
				return
			}
			writeCtx, cancelWrite := storeWriteContext(ctx)
			defer cancelWrite()
			if !priorTerminalCounted {
				if rErr := d.store.RecordTriggerFire(writeCtx, tr.ID, trigAggTime, trigAggStatus, trigAggErr); rErr != nil {
					d.logger.Warn("trigger: failed to update trigger fire counters",
						"triggerID", tr.ID, "eventID", ev.ID, "error", rErr)
				}
				return
			}
			// O-b: later pass for an already-counted matched event flushes only
			// an error delta when a deferred/retried sibling records the event's
			// first error outcome (never double-counting fire_count).
			if isErrStatus(trigAggStatus) && !priorHadError {
				if rErr := d.store.RecordTriggerFireErrorDelta(writeCtx, tr.ID, trigAggTime, trigAggStatus, trigAggErr); rErr != nil {
					d.logger.Warn("trigger: failed to update trigger fire error delta",
						"triggerID", tr.ID, "eventID", ev.ID, "error", rErr)
				}
			}
		}

		// Shared per-(trigger, event) rate-limit decision so all sibling actions
		// of one matched event agree on admission (action-arrays §3.5).
		var eventRateDecision hubfire.EventRateDecision
		trigRef.RateDecision = &eventRateDecision

		for actionIdx, act := range actions {
			if ctx.Err() != nil {
				flushTrigCounters()
				return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
			}

			actionTrigRef := trigRef
			actionTrigRef.ActionIndex = actionIdx

			// Stage 3 §4.4: refuse dispatch_agent and github.comment fires for stale
			// events (attributes.stale == "true") from a replay-protected source.
			// message actions still receive stale events. Checked per action before
			// coalescing or subject reuse so stale deliveries never coalesce, wake,
			// or dispatch, while passive sibling actions still run.
			if ActionSubjectToPreFireGuards(act.Type) && ev.Attributes["stale"] == "true" {
				replayProtected, rpErr := d.isEventSourceReplayProtected(ctx, ev, &webhookEPChecked, &webhookEP)
				if rpErr != nil {
					flushTrigCounters()
					d.logger.Warn("trigger: failed to check source replay protection for stale event",
						"eventID", ev.ID, "sourceKind", ev.SourceKind, "sourceID", ev.SourceID, "error", rpErr)
					return durablequeue.Result{Err: errors.New("trigger: stale check failed")}
				}
				if replayProtected {
					claimedDirect, rErr := d.recordActionDirectFire(
						ctx, tr, ev, actionIdx, act.Type, fireSubject,
						store.TriggerFireStatusRefused, ErrFireStaleEvent,
						matchedThrough, decidedDialect, ruleTraceStr, "stale_event", false,
					)
					if rErr != nil {
						flushTrigCounters()
						return durablequeue.Result{Err: errors.New("trigger: record refused fire failed")}
					}
					if claimedDirect {
						recordTrigOutcome(store.TriggerFireStatusRefused, ErrFireStaleEvent, d.now().UTC())
					} else if tr.FireCount > 0 {
						priorTerminalCounted = true
						priorHadError = true
					}
					continue
				}
			}

			// Source-specific pre-fire refusals (e.g. GitHub own-App / unknown-identity
			// bot guard) run per action after stale refusal so refusal reasons stay
			// deterministic, and before claiming, coalescing, deferral, or subject reuse.
			if refuser, ok := d.core.(PreFireRefuser); ok {
				if reason, auditReason, refused := refuser.PreFireRefusal(ctx, tr, act.Type, &baseEvent); refused {
					claimedDirect, rErr := d.recordActionDirectFire(
						ctx, tr, ev, actionIdx, act.Type, fireSubject,
						store.TriggerFireStatusRefused, reason,
						matchedThrough, decidedDialect, ruleTraceStr, auditReason, false,
					)
					if rErr != nil {
						flushTrigCounters()
						return durablequeue.Result{Err: errors.New("trigger: record refused fire failed")}
					}
					if claimedDirect {
						recordTrigOutcome(store.TriggerFireStatusRefused, reason, d.now().UTC())
					} else if tr.FireCount > 0 {
						priorTerminalCounted = true
						priorHadError = true
					}
					continue
				}
			}

			if lease != nil {
				held, lErr := lease.Renew(ctx)
				if lErr != nil {
					flushTrigCounters()
					return durablequeue.Result{Err: errors.New("trigger: renew queue lease failed")}
				}
				if !held {
					flushTrigCounters()
					return durablequeue.Result{Err: errors.New("trigger: queue lease lost")}
				}
			}
			claimNow := d.now().UTC()
			fire := store.TriggerFire{
				ID:                   uuid.NewString(),
				ProjectID:            tr.ProjectID,
				TriggerID:            tr.ID,
				EventID:              ev.ID,
				Subject:              fireSubject,
				ActionType:           act.Type,
				ActionIndex:          actionIdx,
				Status:               store.TriggerFireStatusFiring,
				MatchedThrough:       matchedThrough,
				DecidedDialect:       decidedDialect,
				RuleTrace:            ruleTraceStr,
				ExecutorKind:         ExecutorKindTriggerDrainer,
				ExecutorID:           ExecutorIDPrefixTriggerEvent + ev.ID,
				LeaseUntil:           claimNow.Add(FireLeaseHorizon),
				StartedAt:            claimNow,
				CreatedAt:            claimNow,
				InitiatorAttribution: tr.InitiatorAttribution,
			}

			var (
				claimed     bool
				promoted    bool
				skippedBusy bool
				existing    *store.TriggerFire
				cErr        error
			)
			if tr.CoalesceWindowSeconds > 0 {
				notBefore := claimNow.Add(time.Duration(tr.CoalesceWindowSeconds) * time.Second)
				var superseded []store.TriggerFire
				claimed, promoted, superseded, existing, cErr = d.store.ClaimTriggerFireCoalescing(ctx, &fire, ErrFireCoalesced, claimNow, notBefore)
				if len(superseded) > 0 {
					writeCtx, cancelWrite := storeWriteContext(ctx)
					for _, sf := range superseded {
						d.recordFireMetric(sf.ActionType, store.TriggerFireStatusSkipped, ErrFireCoalesced)
						if rErr := d.store.RecordTriggerFire(writeCtx, tr.ID, claimNow, store.TriggerFireStatusSkipped, ErrFireCoalesced); rErr != nil {
							d.logger.Warn("trigger: failed to update superseded coalesced fire counters",
								"triggerID", tr.ID, "fireID", sf.ID, "error", rErr)
						}
						if d.fireAudit != nil {
							d.fireAudit.RecordFire(writeCtx, FireAuditRecord{
								ProjectID:      tr.ProjectID,
								TriggerID:      tr.ID,
								EventID:        sf.EventID,
								FireID:         sf.ID,
								ActionIndex:    sf.ActionIndex,
								ActionType:     sf.ActionType,
								Status:         store.TriggerFireStatusSkipped,
								MatchedThrough: sf.MatchedThrough,
								DecidedDialect: sf.DecidedDialect,
								Reason:         FireSkipReasonCoalesced,
							})
						}
					}
					cancelWrite()
				}
				if cErr != nil {
					flushTrigCounters()
					d.logger.Warn("trigger: failed to claim coalescing trigger fire",
						"eventID", ev.ID, "triggerID", tr.ID, "actionIndex", actionIdx, "error", cErr)
					return durablequeue.Result{Err: errors.New("trigger: claim fire failed")}
				}
				if !claimed {
					if existing != nil && (existing.Status == store.TriggerFireStatusCoalescing || existing.Status == store.TriggerFireStatusDeferred) {
						stillCoalescingFires[triggerActionKey{tr.ID, actionIdx}] = true
						retryAt := notBefore
						if existing.NotBefore != nil && !existing.NotBefore.IsZero() {
							retryAt = existing.NotBefore.UTC()
						}
						if earliestRetryAt.IsZero() || retryAt.Before(earliestRetryAt) {
							earliestRetryAt = retryAt
						}
					} else if existing != nil && existing.Status == store.TriggerFireStatusFired {
						fireCount++
					}
					continue
				}
				if !promoted {
					stillCoalescingFires[triggerActionKey{tr.ID, actionIdx}] = true
					if earliestRetryAt.IsZero() || notBefore.Before(earliestRetryAt) {
						earliestRetryAt = notBefore
					}
					continue
				}
			} else {
				// Stage 1 PR 4 fire-level skip applies to message/github.comment actions;
				// dispatch_agent and exec actions with subject mode "skip" use
				// persistent subject claims (§5.6, P8) inside their action Fire so the
				// claim outlives the firing row for the lifetime of the agent or exec run.
				if tr.SubjectMode == store.TriggerSubjectModeSkip &&
					act.Type != store.TriggerActionDispatchAgent &&
					act.Type != store.TriggerActionExec {
					claimed, skippedBusy, existing, cErr = d.store.ClaimTriggerFireSubjectSkip(ctx, &fire, ErrFireSubjectBusy, claimNow)
				} else {
					claimed, existing, cErr = d.store.ClaimTriggerFire(ctx, &fire)
				}
				if cErr != nil {
					flushTrigCounters()
					d.logger.Warn("trigger: failed to claim trigger fire",
						"eventID", ev.ID, "triggerID", tr.ID, "actionIndex", actionIdx, "error", cErr)
					return durablequeue.Result{Err: errors.New("trigger: claim fire failed")}
				}
				if !claimed && isCoalescingOrReapedFire(existing) {
					// Existing coalescing, deferred, or reaped-pending row while
					// coalesce_window is 0 (e.g. edited to 0 mid-window or reuse
					// deferral re-handle): route through the promote/revive path.
					claimed, promoted, _, existing, cErr = d.store.ClaimTriggerFireCoalescing(ctx, &fire, ErrFireCoalesced, claimNow, claimNow)
					if cErr != nil {
						flushTrigCounters()
						d.logger.Warn("trigger: failed to promote pending trigger fire",
							"eventID", ev.ID, "triggerID", tr.ID, "actionIndex", actionIdx, "error", cErr)
						return durablequeue.Result{Err: errors.New("trigger: claim fire failed")}
					}
					if !claimed {
						if existing != nil && (existing.Status == store.TriggerFireStatusCoalescing || existing.Status == store.TriggerFireStatusDeferred) {
							stillCoalescingFires[triggerActionKey{tr.ID, actionIdx}] = true
							retryAt := claimNow
							if existing.NotBefore != nil && !existing.NotBefore.IsZero() {
								retryAt = existing.NotBefore.UTC()
							}
							if earliestRetryAt.IsZero() || retryAt.Before(earliestRetryAt) {
								earliestRetryAt = retryAt
							}
						} else if existing != nil && existing.Status == store.TriggerFireStatusFired {
							fireCount++
						}
						continue
					}
					if !promoted {
						stillCoalescingFires[triggerActionKey{tr.ID, actionIdx}] = true
						if earliestRetryAt.IsZero() || claimNow.Before(earliestRetryAt) {
							earliestRetryAt = claimNow
						}
						continue
					}
				} else if !claimed {
					// Display-only counter: on a lost-lease re-handle, if an earlier
					// attempt's claimed row is still in status "firing" (or transitions to
					// "fired" after this check), fire_count on trigger_events can
					// undercount; trigger_fires rows remain the authoritative fire log.
					if existing != nil {
						if existing.Status == store.TriggerFireStatusFired {
							fireCount++
						}
						if existing.Status != store.TriggerFireStatusCoalescing &&
							existing.Status != store.TriggerFireStatusDeferred &&
							existing.Status != store.TriggerFireStatusFiring {
							notePriorTerminal(existing)
						}
					}
					continue
				}
				if skippedBusy {
					recordTrigOutcome(store.TriggerFireStatusSkipped, ErrFireSubjectBusy, claimNow)
					if d.fireAudit != nil {
						writeCtx, cancelWrite := storeWriteContext(ctx)
						d.fireAudit.RecordFire(writeCtx, FireAuditRecord{
							ProjectID:      tr.ProjectID,
							TriggerID:      tr.ID,
							EventID:        ev.ID,
							FireID:         fire.ID,
							ActionIndex:    actionIdx,
							ActionType:     act.Type,
							Status:         store.TriggerFireStatusSkipped,
							MatchedThrough: fire.MatchedThrough,
							DecidedDialect: fire.DecidedDialect,
						})
						cancelWrite()
					}
					continue
				}
			}

			if ev.PayloadRef != "" && !overflowPreviewLoaded {
				overflowPreviewLoaded = true
				if d.storageFn != nil {
					if stor := d.storageFn(); stor != nil {
						prev, pErr := ReadOverflowPayloadPreview(ctx, stor, ev.ProjectID, ev.PayloadRef, DefaultOverflowDownloadTimeout)
						if pErr != nil {
							d.logger.Warn("trigger: failed to read overflow payload preview for delivery",
								"eventID", ev.ID, "payloadRef", ev.PayloadRef, "error", pErr)
						} else {
							overflowPreview = prev
						}
					}
				}
			}

			tracker := newTriggerFireTracker(ctx, d.core)
			req := hubfire.FireRequest{
				ProjectID: tr.ProjectID,
				Authority: hubfire.RecordedAuthority{
					Attribution:    tr.InitiatorAttribution,
					Ceiling:        tr.AuthorityCeiling,
					DefinitionKind: hubfire.DefinitionKindTrigger,
					DefinitionID:   tr.ID,
					FireID:         fire.ID,
				},
				ActionType: act.Type,
				Payload:    act.Payload,
				Event: &hubfire.FireEvent{
					ID:             ev.ID,
					ProjectID:      ev.ProjectID,
					SourceKind:     ev.SourceKind,
					SourceID:       ev.SourceID,
					Type:           ev.Type,
					Subject:        ev.Subject,
					OccurredAt:     ev.OccurredAt,
					ReceivedAt:     ev.ReceivedAt,
					Attributes:     maps.Clone(ev.Attributes),
					Lists:          cloneStringListMap(ev.Lists),
					Payload:        rawPayload,
					PayloadPreview: overflowPreview,
					PayloadBytes:   ev.PayloadBytes,
					PayloadRef:     ev.PayloadRef,
					Trigger:        &actionTrigRef,
				},
				Executor: hubfire.ExecutorContext{
					Kind: ExecutorKindTriggerDrainer,
					ID:   ExecutorIDPrefixTriggerEvent + ev.ID,
				},
				FireAt:   ev.ReceivedAt,
				Attempts: item.Attempts,
				Tracked:  tracker,
			}

			fireCtx, cancelFire := context.WithTimeout(ctx, d.perFireTimeout)
			var out hubfire.FireOutcome
			switch tr.SubjectMode {
			case store.TriggerSubjectModeSkip:
				// Stage 3 §6.5 & action-arrays §3.2: single liveness check across
				// firing|coalescing rows and live subject claims per (trigger, actionIdx, subject);
				// dispatch_agent's atomic Claim inside Fire remains the backstop
				// for concurrent replicas.
				if act.Type == store.TriggerActionDispatchAgent || promoted {
					includeFiring := act.Type != store.TriggerActionDispatchAgent
					live, _, lErr := d.subjectLive(fireCtx, tr.ID, actionIdx, fireSubject, fire.ID, includeFiring)
					if lErr != nil {
						d.logger.Warn("trigger: subjectLive check failed",
							"triggerID", tr.ID, "actionIndex", actionIdx, "fireID", fire.ID, "error", lErr)
						out = hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "fire failed"}
					} else if live {
						out = hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: ErrFireSubjectBusy}
					} else {
						out = d.core.Fire(fireCtx, req)
					}
				} else {
					out = d.core.Fire(fireCtx, req)
				}

			case store.TriggerSubjectModeReuse:
				// Stage 3 §6.2 (rev 1.16), §6.5 & action-arrays §3.2: reuse (claim
				// lookup, deferral, wake delivery) applies only to dispatch_agent
				// at its own action_index; a reuse on a dispatch action never blocks
				// a message or other sibling action.
				if act.Type != store.TriggerActionDispatchAgent {
					out = d.core.Fire(fireCtx, req)
					break
				}
				_, claim, lErr := d.subjectLive(fireCtx, tr.ID, actionIdx, fireSubject, fire.ID, false)
				switch {
				case lErr != nil:
					d.logger.Warn("trigger: subjectLive check failed for reuse",
						"triggerID", tr.ID, "actionIndex", actionIdx, "fireID", fire.ID, "error", lErr)
					out = hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "fire failed"}
				case claim != nil && claim.AgentID != "":
					if reuseTargetLaunchInFlight(claim.Agent, claimNow) {
						firstDeferred := claimNow
						if fire.DeferredAt != nil && !fire.DeferredAt.IsZero() {
							firstDeferred = fire.DeferredAt.UTC()
							if claimNow.Sub(firstDeferred) >= ReuseDeferTimeout {
								out = hubfire.FireOutcome{
									Status: hubfire.FireStatusFailed,
									Error:  ErrFireTargetAgentStillStarting,
								}
								break
							}
						}
						retryAt := claimNow.Add(ReuseDeferBackoff)
						if deadline := firstDeferred.Add(ReuseDeferTimeout); retryAt.After(deadline) {
							retryAt = deadline
						}
						writeCtx, cancelWrite := storeWriteContext(ctx)
						deferred, dErr := d.store.DeferTriggerFire(writeCtx, fire.ID, firstDeferred, retryAt, retryAt.Add(FireLeaseHorizon))
						cancelWrite()
						if dErr != nil {
							cancelFire()
							flushTrigCounters()
							d.logger.Warn("trigger: failed to defer reuse fire for launching agent",
								"triggerID", tr.ID, "actionIndex", actionIdx, "fireID", fire.ID, "agentID", claim.AgentID, "error", dErr)
							return durablequeue.Result{Err: errors.New("trigger: defer fire failed")}
						}
						if deferred {
							stillCoalescingFires[triggerActionKey{tr.ID, actionIdx}] = true
							if earliestRetryAt.IsZero() || retryAt.Before(earliestRetryAt) {
								earliestRetryAt = retryAt
							}
						}
						cancelFire()
						continue
					}
					out = d.fireReuseMessage(fireCtx, req, claim.AgentID)
					if out.TargetAgentID == "" {
						out.TargetAgentID = claim.AgentID
					}
				case claim != nil && claim.AgentID == "":
					out = hubfire.FireOutcome{
						Status: hubfire.FireStatusSkipped,
						Error:  ErrFireSubjectDispatchInProgress,
					}
				default:
					out = d.core.Fire(fireCtx, req)
				}

			default:
				out = d.core.Fire(fireCtx, req)
			}
			cancelFire()

			statusStr := normalizeFireStatus(out.Status, out.Error)
			finishedAt := d.now().UTC()

			if out.Status == hubfire.FireStatusWaiting {
				writeCtx, cancelWrite := storeWriteContext(ctx)
				parkErr := d.parkWaitingFire(writeCtx, tracker, tr, ev, &fire, act, out, finishedAt)
				cancelWrite()
				if parkErr != nil {
					flushTrigCounters()
					d.logger.Warn("trigger: failed to park waiting trigger fire",
						"fireID", fire.ID, "eventID", ev.ID, "triggerID", tr.ID, "actionIndex", actionIdx, "error", parkErr)
					return durablequeue.Result{Err: errors.New("trigger: park waiting fire failed")}
				}
				continue
			}

			writeCtx, cancelWrite := storeWriteContext(ctx)
			if out.Detached {
				// Detached actions (such as admitted exec runs) own their
				// trigger_fires outcome CAS and trigger counters in the same
				// transaction as the run record (em-2 invariant (b)); never
				// overwrite trigger_fires here, only start the tracker handoff.
				d.recordFireMetric(act.Type, statusStr, out.Error)
				if statusStr == store.TriggerFireStatusFired {
					fireCount++
				}
				if out.AfterRecord != nil {
					out.AfterRecord(writeCtx)
				}
				if d.fireAudit != nil {
					d.fireAudit.RecordFire(writeCtx, FireAuditRecord{
						ProjectID:      tr.ProjectID,
						TriggerID:      tr.ID,
						EventID:        ev.ID,
						FireID:         fire.ID,
						ActionIndex:    actionIdx,
						ActionType:     act.Type,
						Status:         statusStr,
						MatchedThrough: fire.MatchedThrough,
						DecidedDialect: fire.DecidedDialect,
					})
				}
				cancelWrite()
				continue
			}
			var (
				finished  bool
				finishErr error
			)
			tracker.record(func() {
				finished, finishErr = d.store.FinishTriggerFire(writeCtx, fire.ID, statusStr, out.ResultRef, out.TargetAgentID, out.Error, finishedAt)
			})
			if finishErr != nil {
				cancelWrite()
				flushTrigCounters()
				d.logger.Warn("trigger: failed to finish trigger fire",
					"fireID", fire.ID, "eventID", ev.ID, "triggerID", tr.ID, "actionIndex", actionIdx, "error", finishErr)
				return durablequeue.Result{Err: errors.New("trigger: finish fire failed")}
			}
			if finished {
				d.recordFireMetric(act.Type, statusStr, out.Error)
				if statusStr == store.TriggerFireStatusFired {
					fireCount++
				}
				if out.AfterRecord != nil {
					out.AfterRecord(writeCtx)
				}
				recordTrigOutcome(statusStr, out.Error, finishedAt)
				if d.fireAudit != nil {
					d.fireAudit.RecordFire(writeCtx, FireAuditRecord{
						ProjectID:      tr.ProjectID,
						TriggerID:      tr.ID,
						EventID:        ev.ID,
						FireID:         fire.ID,
						ActionIndex:    actionIdx,
						ActionType:     act.Type,
						Status:         statusStr,
						MatchedThrough: fire.MatchedThrough,
						DecidedDialect: fire.DecidedDialect,
					})
				}
			}
			cancelWrite()
		}
		flushTrigCounters()
	}

	if ctx.Err() != nil {
		return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
	}
	if err := d.cancelStrandedCoalescingFires(ctx, ev, stillCoalescingFires); err != nil {
		d.logger.Warn("trigger: failed to cancel stranded coalescing fires",
			"eventID", ev.ID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: cancel stranded coalescing fires failed")}
	}
	if !earliestRetryAt.IsZero() {
		return durablequeue.Result{RetryAt: earliestRetryAt}
	}
	if hook, ok := d.core.(PostEvaluationHook); ok {
		hook.OnEventEvaluated(ctx, ev, EventEvaluationOutcome{
			MatchCount:      matchCount,
			FireCount:       fireCount,
			AnyFireRecorded: hadAnyFire,
		}, item.Attempts)
	}
	if err := d.store.MarkTriggerEventProcessed(ctx, ev.ID, d.now().UTC(), matchCount, fireCount); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		d.logger.Warn("trigger: failed to mark event processed", "eventID", ev.ID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: mark event processed failed")}
	}
	return durablequeue.Result{Done: true}
}

// ActionSubjectToPreFireGuards reports whether actionType is subject to
// pre-fire replay/loop guards (such as stale-event refusal and source
// PreFireRefuser checks). Passive notification actions (message) return false.
func ActionSubjectToPreFireGuards(actionType string) bool {
	return actionType == store.TriggerActionDispatchAgent || actionType == store.TriggerActionGitHubComment
}

// reuseTargetLaunchInFlight reports whether agent a is in a launch state that
// wakeAgentForDM refuses (aligned with launchStartRefusal and wake_dm.go's
// pre-running phases): incomplete create, active in-flight launch before its
// deadline, or any pre-running phase (created..starting).
func reuseTargetLaunchInFlight(a *store.Agent, now time.Time) bool {
	if a == nil || !a.DeletedAt.IsZero() {
		return false
	}
	if a.IsIncompleteCreate() {
		return true
	}
	if a.IsInFlight() && (a.LaunchDeadline.IsZero() || now.Before(a.LaunchDeadline)) {
		return true
	}
	return store.InFlightPhases[a.Phase]
}

func isCoalescingOrReapedFire(f *store.TriggerFire) bool {
	if f == nil {
		return false
	}
	if f.Status == store.TriggerFireStatusCoalescing || f.Status == store.TriggerFireStatusDeferred {
		return true
	}
	return f.Status == store.TriggerFireStatusFailed &&
		(f.Error == ErrFireReapedDuringCoalesce || f.Error == ErrFireReapedDuringDefer)
}

// cancelStrandedCoalescingFires explicitly skips any coalescing, deferred, or
// reaped-pending fire rows for ev whose trigger is no longer actively pending
// in this pass (e.g., the trigger was paused/deleted or its match rules/types
// were edited mid-window so it no longer matches). Without this, those rows
// would stay live for skip/reuse until reaped as hub-restart failures.
func (d *Dispatcher) cancelStrandedCoalescingFires(ctx context.Context, ev *store.TriggerEvent, stillCoalescing map[triggerActionKey]bool) error {
	hasPending, err := d.store.HasPendingTriggerFiresForEvent(ctx, ev.ID)
	if err != nil {
		return err
	}
	if !hasPending {
		return nil
	}
	now := d.now().UTC()
	writeCtx, cancelWrite := storeWriteContext(ctx)
	defer cancelWrite()
	cursor := ""
	for {
		list, lErr := d.store.ListTriggerFires(ctx, store.TriggerFireFilter{EventID: ev.ID}, store.ListOptions{
			Limit:          100,
			Cursor:         cursor,
			SkipTotalCount: true,
		})
		if lErr != nil {
			return lErr
		}
		if list == nil || len(list.Items) == 0 {
			return nil
		}
		for i := range list.Items {
			ef := &list.Items[i]
			if !isCoalescingOrReapedFire(ef) || stillCoalescing[triggerActionKey{ef.TriggerID, ef.ActionIndex}] {
				continue
			}
			cancelErr := ErrFireCoalesceCancelled
			cancelReason := FireSkipReasonCoalesceCancelled
			if ef.Status == store.TriggerFireStatusDeferred ||
				(ef.Status == store.TriggerFireStatusFailed && ef.Error == ErrFireReapedDuringDefer) {
				cancelErr = ErrFireDeferCancelled
				cancelReason = FireSkipReasonDeferCancelled
			}
			cancelled, cErr := d.store.FinishPendingTriggerFire(writeCtx, ef.ID, store.TriggerFireStatusSkipped, cancelErr, "", "", "", now)
			if cErr != nil {
				return cErr
			}
			if !cancelled {
				continue
			}
			d.recordFireMetric(ef.ActionType, store.TriggerFireStatusSkipped, cancelErr)
			if rErr := d.store.RecordTriggerFire(writeCtx, ef.TriggerID, now, store.TriggerFireStatusSkipped, cancelErr); rErr != nil &&
				!errors.Is(rErr, store.ErrNotFound) {
				d.logger.Warn("trigger: failed to update cancelled pending fire counters",
					"triggerID", ef.TriggerID, "fireID", ef.ID, "error", rErr)
			}
			if d.fireAudit != nil {
				d.fireAudit.RecordFire(writeCtx, FireAuditRecord{
					ProjectID:      ef.ProjectID,
					TriggerID:      ef.TriggerID,
					EventID:        ef.EventID,
					FireID:         ef.ID,
					ActionIndex:    ef.ActionIndex,
					ActionType:     ef.ActionType,
					Status:         store.TriggerFireStatusSkipped,
					MatchedThrough: ef.MatchedThrough,
					DecidedDialect: ef.DecidedDialect,
					Reason:         cancelReason,
				})
			}
		}
		if list.NextCursor == "" {
			return nil
		}
		cursor = list.NextCursor
	}
}

func (d *Dispatcher) isEventSourceReplayProtected(
	ctx context.Context,
	ev *store.TriggerEvent,
	checked *bool,
	cachedEP **store.WebhookEndpoint,
) (bool, error) {
	if ev.SourceKind == store.TriggerSourceKindWebhook {
		if !*checked {
			*checked = true
			if ev.SourceID != "" {
				ep, err := d.store.GetWebhookEndpoint(ctx, ev.SourceID)
				if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrInvalidInput) {
					return false, err
				}
				*cachedEP = ep
			}
		}
		return SourceReplayProtected(ev.SourceKind, *cachedEP), nil
	}
	return SourceReplayProtected(ev.SourceKind, nil), nil
}

// subjectLive computes whether (triggerID, subject) is currently live across
// all three live states (Stage 3 §6.5):
//  1. A bound live subject claim (SubjectClaims.Live, lazily releasing
//     terminal/soft-deleted/missing agents),
//  2. A fresh unbound active subject claim (agent_id == "" and < 5 min old,
//     another replica mid-create, §6.2), or
//  3. A fire row in firing|coalescing for (triggerID, subject) excluding
//     excludeFireID.
//
// Returns the active claim (if any) so reuse needs no second lookup.
func (d *Dispatcher) subjectLive(ctx context.Context, triggerID string, actionIndex int, subject, excludeFireID string, includeFiring bool) (bool, *Claim, error) {
	now := d.now().UTC()
	if d.claims != nil {
		liveClaim, err := d.claims.Live(ctx, triggerID, actionIndex, subject)
		if err != nil {
			return false, nil, err
		}
		if liveClaim != nil {
			return true, liveClaim, nil
		}
		activeClaim, err := d.store.GetActiveTriggerSubjectClaim(ctx, triggerID, actionIndex, subject)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return false, nil, err
		}
		if activeClaim != nil && activeClaim.AgentID == "" && !activeClaim.ClaimedAt.Before(now.Add(-AbandonedClaimTTL)) {
			return true, activeClaim, nil
		}
	}
	liveFire, err := d.store.HasLiveTriggerFireForSubjectExcluding(ctx, triggerID, actionIndex, subject, excludeFireID, includeFiring, now)
	if err != nil {
		return false, nil, err
	}
	return liveFire, nil, nil
}

type reuseMessagePayload struct {
	AgentID string `json:"agentId"`
	Message string `json:"message"`
	Wake    bool   `json:"wake"`
}

func (d *Dispatcher) fireReuseMessage(ctx context.Context, req hubfire.FireRequest, agentID string) hubfire.FireOutcome {
	var text string
	if renderer, ok := d.core.(ReuseMessageRenderer); ok {
		rendered, refusal, err := renderer.RenderReuseMessage(ctx, req)
		if err != nil {
			d.logger.Warn("trigger: failed to render reuseMessage",
				"fireID", req.Authority.FireID, "triggerID", req.Authority.DefinitionID, "error", err)
			return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "fire failed"}
		}
		if refusal != nil {
			return *refusal
		}
		text = rendered
	} else {
		var p struct {
			ReuseMessage string `json:"reuseMessage"`
		}
		_ = json.Unmarshal([]byte(req.Payload), &p)
		text = p.ReuseMessage
	}
	if strings.TrimSpace(text) == "" {
		if req.Event != nil && req.Event.Subject != "" {
			text = fmt.Sprintf("New %s event on %s", req.Event.Type, req.Event.Subject)
		} else if req.Event != nil && req.Event.Type != "" {
			text = fmt.Sprintf("New %s event", req.Event.Type)
		} else {
			text = "New trigger event"
		}
	}

	rawMsg, err := json.Marshal(reuseMessagePayload{
		AgentID: agentID,
		Message: text,
		Wake:    true,
	})
	if err != nil {
		return hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "fire failed"}
	}

	msgReq := req
	msgReq.ActionType = store.TriggerActionMessage
	msgReq.Payload = string(rawMsg)
	return d.core.Fire(ctx, msgReq)
}

func (d *Dispatcher) loadActiveTriggers(ctx context.Context, projectID, sourceKind, sourceID string) ([]store.Trigger, error) {
	filter := store.TriggerFilter{
		ProjectID:  projectID,
		SourceKind: sourceKind,
		Status:     store.TriggerStatusActive,
	}
	if sourceKind == store.TriggerSourceKindGitHub {
		// Stage 3 §4.3 / §12.3 item 1: github rules store source_id = "" ("any
		// associated repository in the project"), while events carry the decimal
		// installation ID in source_id. Match source_id IN ('', ev.SourceID) for
		// github only; generic webhook lookups remain exact equality.
		if sourceID == "" {
			filter.SourceIDs = []string{""}
		} else {
			filter.SourceIDs = []string{"", sourceID}
		}
	} else {
		filter.SourceID = sourceID
	}

	var (
		all    []store.Trigger
		cursor string
	)
	for {
		res, err := d.store.ListTriggers(ctx, filter, store.ListOptions{
			Limit:  store.MaxTriggersPerProject,
			Cursor: cursor,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, res.Items...)
		if res.NextCursor == "" || len(res.Items) == 0 {
			// SEC-3b2-02: evaluate candidates oldest-first (CreatedAt ASC, ID ASC)
			// so a newly authored slow trigger cannot exhaust the shared per-event
			// budget ahead of pre-existing triggers on the same source.
			slices.SortFunc(all, func(a, b store.Trigger) int {
				if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
					return c
				}
				return cmp.Compare(a.ID, b.ID)
			})
			return all, nil
		}
		cursor = res.NextCursor
	}
}

func (d *Dispatcher) recordDirectFire(
	ctx context.Context,
	tr *store.Trigger,
	ev *store.TriggerEvent,
	subject, status, errMsg, matchedThrough, decidedDialect, ruleTrace, auditReason string,
) error {
	_, err := d.recordActionDirectFire(
		ctx, tr, ev, 0, firstTriggerActionType(tr),
		subject, status, errMsg, matchedThrough, decidedDialect, ruleTrace, auditReason, true,
	)
	return err
}

func (d *Dispatcher) recordActionDirectFire(
	ctx context.Context,
	tr *store.Trigger,
	ev *store.TriggerEvent,
	actionIndex int,
	actionType string,
	subject, status, errMsg, matchedThrough, decidedDialect, ruleTrace, auditReason string,
	updateCounter bool,
) (bool, error) {
	now := d.now().UTC()
	fire := store.TriggerFire{
		ID:                   uuid.NewString(),
		ProjectID:            tr.ProjectID,
		TriggerID:            tr.ID,
		EventID:              ev.ID,
		Subject:              subject,
		ActionType:           actionType,
		ActionIndex:          actionIndex,
		Status:               status,
		Error:                errMsg,
		MatchedThrough:       matchedThrough,
		DecidedDialect:       decidedDialect,
		RuleTrace:            ruleTrace,
		ExecutorKind:         ExecutorKindTriggerDrainer,
		ExecutorID:           ExecutorIDPrefixTriggerEvent + ev.ID,
		LeaseUntil:           now.Add(FireLeaseHorizon),
		StartedAt:            now,
		FinishedAt:           &now,
		CreatedAt:            now,
		InitiatorAttribution: tr.InitiatorAttribution,
	}
	claimed, existing, err := d.store.ClaimTriggerFire(ctx, &fire)
	if err != nil {
		return false, err
	}
	if !claimed && isCoalescingOrReapedFire(existing) {
		// On a coalescing/deferred re-handle, preserve the real skip/refusal
		// reason (eval/compile budget, corrupt rule, stale event, unauthorized
		// source) instead of letting cancelStrandedCoalescingFires relabel it
		// as "coalesce cancelled".
		finished, fErr := d.store.FinishPendingTriggerFire(ctx, existing.ID, status, errMsg, matchedThrough, decidedDialect, ruleTrace, now)
		if fErr != nil {
			return false, fErr
		}
		if finished {
			claimed = true
			fire.ID = existing.ID
		}
	}
	if claimed {
		d.recordFireMetric(actionType, status, errMsg)
		if updateCounter {
			if rErr := d.store.RecordTriggerFire(ctx, tr.ID, now, status, errMsg); rErr != nil {
				d.logger.Warn("trigger: failed to record direct fire counter",
					"triggerID", tr.ID, "fireID", fire.ID, "error", rErr)
			}
		}
		if d.fireAudit != nil {
			d.fireAudit.RecordFire(ctx, FireAuditRecord{
				ProjectID:      tr.ProjectID,
				TriggerID:      tr.ID,
				EventID:        ev.ID,
				FireID:         fire.ID,
				ActionIndex:    actionIndex,
				ActionType:     actionType,
				Status:         status,
				MatchedThrough: matchedThrough,
				DecidedDialect: decidedDialect,
				Reason:         auditReason,
			})
		}
	}
	return claimed, nil
}

func normalizeFireStatus(st hubfire.FireStatus, errMsg string) string {
	switch st {
	case hubfire.FireStatusFired:
		return store.TriggerFireStatusFired
	case hubfire.FireStatusWaiting:
		return store.TriggerFireStatusWaiting
	case hubfire.FireStatusFailed:
		return store.TriggerFireStatusFailed
	case hubfire.FireStatusRefused:
		return store.TriggerFireStatusRefused
	case hubfire.FireStatusSkipped:
		return store.TriggerFireStatusSkipped
	case hubfire.FireStatusExpired:
		return store.TriggerFireStatusExpired
	default:
		if errMsg != "" {
			return store.TriggerFireStatusFailed
		}
		return store.TriggerFireStatusFired
	}
}

func deriveFireSubject(raw string) string {
	if len(raw) <= MaxFireSubjectBytes {
		return raw
	}
	cut := MaxFireSubjectBytes
	for cut > 0 && !utf8.RuneStart(raw[cut]) {
		cut--
	}
	return raw[:cut]
}

func cloneStringListMap(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = nil
			continue
		}
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func storeWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// triggerFireTracker implements hubfire.FireTracker for a single trigger fire
// inside an active trigger.event queue item (§5.4, §6.3, rev 7.2).
//
// Detach strips the per-fire 30s deadline while keeping the queue item's 150s
// deadline, lease-loss cancellation, and any hub shutdown-abort cancellation so
// a wake fire never outlives its queue item.
type triggerFireTracker struct {
	itemCtx context.Context
	core    Core

	mu       sync.Mutex
	abortCtx context.Context

	once sync.Once
}

func newTriggerFireTracker(itemCtx context.Context, core Core) *triggerFireTracker {
	return &triggerFireTracker{
		itemCtx: itemCtx,
		core:    core,
	}
}

func (t *triggerFireTracker) Detach(ctx context.Context) (context.Context, context.CancelFunc) {
	dctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopItem := context.AfterFunc(t.itemCtx, cancel)

	t.mu.Lock()
	abortCtx := t.abortCtx
	t.mu.Unlock()
	var stopAbort func() bool
	if abortCtx != nil {
		stopAbort = context.AfterFunc(abortCtx, cancel)
	}

	cleanup := func() {
		stopItem()
		if stopAbort != nil {
			stopAbort()
		}
		cancel()
	}
	if deadline, ok := t.itemCtx.Deadline(); ok {
		tctx, tcancel := context.WithDeadline(dctx, deadline)
		return tctx, func() {
			tcancel()
			cleanup()
		}
	}
	return dctx, cleanup
}

func (t *triggerFireTracker) Admit(ctx context.Context) (context.Context, func(), bool) {
	if t.itemCtx.Err() != nil {
		return ctx, func() {}, false
	}
	if adm, ok := t.core.(FireAdmitter); ok {
		admittedCtx, abortCtx, done, admitted := adm.AdmitFire(ctx, t.itemCtx)
		if !admitted {
			return ctx, func() {}, false
		}
		t.mu.Lock()
		t.abortCtx = abortCtx
		t.mu.Unlock()
		return admittedCtx, done, true
	}
	return ctx, func() {}, true
}

func (t *triggerFireTracker) record(write func()) bool {
	ran := false
	t.once.Do(func() {
		ran = true
		if write != nil {
			write()
		}
	})
	return ran
}

// MaintenanceResult reports counts from one hourly trigger maintenance pass.
type MaintenanceResult struct {
	FiresReaped           int
	EventsReconciled      int
	EventsPurged          int
	FiresPurged           int
	OverflowObjectsPurged int
	OverflowOrphansSwept  int
	OverflowDeleteErrors  int
	ClaimsReleased        int
	ClaimsPurged          int
	RotatedSecretsPurged  int
}

// MaintenanceOptions configures optional overflow-object maintenance behaviour.
type MaintenanceOptions struct {
	// OrphanMinAge overrides DefaultOverflowOrphanMinAge when positive.
	// Set to a negative duration in tests to sweep immediately without a grace window.
	OrphanMinAge time.Duration
	Logger       *slog.Logger
}

// RunMaintenance executes the hourly trigger maintenance pass without object
// storage (legacy entry point for callers/tests without overflow storage).
func RunMaintenance(ctx context.Context, ts store.TriggerStore, now time.Time) (MaintenanceResult, error) {
	return RunMaintenanceWithStorage(ctx, ts, nil, now, MaintenanceOptions{})
}

// RunMaintenanceWithStorage executes the hourly trigger maintenance pass (§4.3,
// §6.4, rev 7.4; Stage 2 §4.4, §5.6, §8):
//  1. Reaps expired firing rows (lease_until <= now) to failed "hub restarted during fire".
//  2. Reconciles pending trigger_events older than DefaultMaxAge (72h) with no
//     trigger.event queue item to failed "dropped by queue limits".
//  3. Purges terminal trigger_fires and trigger_events older than
//     HistoryRetention (7 days), deleting any associated overflow payload objects
//     from stor (failures logged and retried on the next hourly sweep).
//  4. Sweeps orphan overflow payload objects under trigger-events/ that have no
//     matching trigger_events row and are older than the orphan grace window.
//  5. Releases active trigger_subject_claims whose agent is terminal
//     (stopped|error|halted) or deleted.
//  6. Deletes released trigger_subject_claims older than HistoryRetention (7 days).
//  7. Purges retired webhook endpoint signing secret rows past the 24h rotation grace window.
func RunMaintenanceWithStorage(ctx context.Context, ts store.TriggerStore, stor storage.Storage, now time.Time, opts MaintenanceOptions) (MaintenanceResult, error) {
	var out MaintenanceResult
	if ts == nil {
		return out, fmt.Errorf("%w: trigger store is nil", store.ErrInvalidInput)
	}
	now = now.UTC()
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	var errs []error
	reaped, err := ts.ReapExpiredTriggerFires(ctx, now, DefaultReapBatchLimit)
	out.FiresReaped = reaped
	if err != nil {
		errs = append(errs, fmt.Errorf("reap expired trigger fires: %w", err))
	}

	reconciled, err := ts.ReconcileOrphanedPendingTriggerEvents(
		ctx,
		EventQueueKind,
		now.Add(-DefaultMaxAge),
		now,
		ErrEventDroppedByQueueLimits,
		DefaultReconcileBatchLimit,
	)
	out.EventsReconciled = reconciled
	if err != nil {
		errs = append(errs, fmt.Errorf("reconcile orphaned pending trigger events: %w", err))
	}

	eventsPurged, firesPurged, purgedRefs, err := ts.PurgeOldTriggerHistoryWithPayloadRefs(ctx, now.Add(-HistoryRetention))
	out.EventsPurged = eventsPurged
	out.FiresPurged = firesPurged
	if err != nil {
		errs = append(errs, fmt.Errorf("purge old trigger history: %w", err))
	} else if stor != nil && len(purgedRefs) > 0 {
		delCount, delErrs := PurgeOverflowObjects(ctx, stor, purgedRefs, logger)
		out.OverflowObjectsPurged = delCount
		out.OverflowDeleteErrors += delErrs
	}

	if stor != nil {
		minAge := opts.OrphanMinAge
		if minAge == 0 {
			minAge = DefaultOverflowOrphanMinAge
		} else if minAge < 0 {
			minAge = 0
		}
		swept, sweepErrs, sErr := SweepOrphanOverflowObjects(ctx, ts, stor, now, minAge, logger)
		out.OverflowOrphansSwept = swept
		out.OverflowDeleteErrors += sweepErrs
		if sErr != nil {
			errs = append(errs, fmt.Errorf("sweep orphan overflow payload objects: %w", sErr))
		}
	}

	claimsReleased, err := ts.ReleaseTerminalTriggerSubjectClaims(ctx, now)
	out.ClaimsReleased = claimsReleased
	if err != nil {
		errs = append(errs, fmt.Errorf("release terminal trigger subject claims: %w", err))
	}

	claimsPurged, err := ts.PurgeOldTriggerSubjectClaims(ctx, now.Add(-HistoryRetention))
	out.ClaimsPurged = claimsPurged
	if err != nil {
		errs = append(errs, fmt.Errorf("purge old trigger subject claims: %w", err))
	}

	secretsPurged, err := ts.PurgeExpiredRotatedWebhookSecrets(ctx, now, store.WebhookSecretRotationGraceWindow)
	out.RotatedSecretsPurged = secretsPurged
	if err != nil {
		errs = append(errs, fmt.Errorf("purge expired rotated webhook secrets: %w", err))
	}

	return out, errors.Join(errs...)
}
