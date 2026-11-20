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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/durablequeue"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
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

	DefaultPerFireTimeout  = 30 * time.Second
	DefaultEventRuleBudget = time.Second
	FireLeaseHorizon       = 10 * time.Minute
	HistoryRetention       = 7 * 24 * time.Hour
	MaxFireSubjectBytes    = 256

	DefaultReapBatchLimit      = 500
	DefaultReconcileBatchLimit = 500

	ExecutorKindTriggerDrainer   = "trigger_drainer"
	ExecutorIDPrefixTriggerEvent = "trigger_event:"

	// Content-free generic error texts persisted on trigger_events / trigger_fires.
	ErrEventDroppedByQueueLimits         = "dropped by queue limits"
	ErrEventProcessingFailed             = "processing failed"
	ErrFireRuleBudgetExhausted           = "rule budget exhausted"
	ErrFireRuleEvaluationFailed          = "rule evaluation failed"
	ErrFireRuleUncertain                 = "rule uncertain"
	ErrFireRuleNoLongerCompiles          = "rule no longer compiles"
	ErrFireGitHubAssociationUnauthorized = "github association not authorized"
	ErrFireSubjectBusy                   = "subject busy"

	// Closed-set skipped-fire metric reasons (§6.5). Free-text errors are
	// never emitted as metric labels.
	FireSkipReasonRuleBudgetExhausted  = "rule_budget_exhausted"
	FireSkipReasonRuleEvaluationFailed = "rule_evaluation_failed"
	FireSkipReasonRuleUncertain        = "rule_uncertain"
	FireSkipReasonRuleNoLongerCompiles = "rule_no_longer_compiles"
	FireSkipReasonSubjectBusy          = "subject_busy"
	FireSkipReasonOther                = "other"
)

// NormalizeFireSkipReason maps a skipped trigger fire's persisted error text
// onto a closed-set metric label value so label cardinality stays bounded.
func NormalizeFireSkipReason(errMsg string) string {
	switch strings.TrimSpace(errMsg) {
	case ErrFireRuleBudgetExhausted:
		return FireSkipReasonRuleBudgetExhausted
	case ErrFireRuleEvaluationFailed:
		return FireSkipReasonRuleEvaluationFailed
	case ErrFireRuleUncertain:
		return FireSkipReasonRuleUncertain
	case ErrFireRuleNoLongerCompiles:
		return FireSkipReasonRuleNoLongerCompiles
	case "subject busy":
		return FireSkipReasonSubjectBusy
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

// DispatcherConfig configures a trigger event Dispatcher.
type DispatcherConfig struct {
	Core           Core
	Store          store.TriggerStore
	Rules          *triggerrule.Rules
	RuleMetrics    RuleEvalMetrics
	FireAudit      FireAuditSink
	PerFireTimeout time.Duration
	RuleBudget     time.Duration
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
	rules          *triggerrule.Rules
	ruleMetrics    RuleEvalMetrics
	fireAudit      FireAuditSink
	perFireTimeout time.Duration
	ruleBudget     time.Duration
	metrics        DispatcherMetrics
	logger         *slog.Logger
	now            func() time.Time

	statsMu   sync.Mutex
	evalStats map[RuleEvalKey]RuleEvalBucket
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
		rules:          cfg.Rules,
		ruleMetrics:    cfg.RuleMetrics,
		fireAudit:      fireAudit,
		perFireTimeout: perFire,
		ruleBudget:     ruleBudget,
		metrics:        cfg.Metrics,
		logger:         logger,
		now:            nowFn,
		evalStats:      make(map[RuleEvalKey]RuleEvalBucket),
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

	// Parse payload JSON once per event via NewEventActivation so every candidate
	// trigger rule shares one parsed activation and one aggregate per-event
	// BudgetTracker (§6.3, F5, N3; mixed-rule-arrays §3, §6).
	parseStart := d.now()
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
	baseAct, actErr := triggerrule.NewEventActivation(baseEvent)
	budget := ruleengine.NewBudgetTracker(d.ruleBudget)
	budget.Charge(max(d.now().Sub(parseStart), time.Duration(0)))

	var (
		matchCount        int
		fireCount         int
		sourceAuthChecked bool
		sourceAuthorized  bool
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

		// Stage 3 §4.2 step 4: before any rule evaluation or fire on a github
		// event, verify once per event that the project's GitHub App installation
		// association is authorized now. Running this ahead of rule evaluation
		// ensures a refused or retrying github event never evaluates rules or
		// records rule-skip outcomes that mask the refusal.
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
			ID:   tr.ID,
			Name: tr.Name,
		}
		trigAct := baseAct.WithTrigger(trigRef)

		matchedThrough := "0/0"
		decidedDialect := ""
		ruleTraceStr := ""

		if len(matchRules) == 0 {
			if budget.Exhausted() {
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
				if rErr := d.recordDirectFire(
					ctx, tr, ev, deriveFireSubject(ev.Subject),
					store.TriggerFireStatusSkipped, ErrFireRuleEvaluationFailed,
					fmt.Sprintf("1/%d", len(matchRules)), firstDialect, traceStr, triggerrule.MatchReasonEvalFailed,
				); rErr != nil {
					return durablequeue.Result{Err: errors.New("trigger: record skipped fire failed")}
				}
				continue
			}

			// SEC-3b1rw-01: ALWAYS pass WithBudget(budget) so rule evaluation is
			// bounded by the shared per-event aggregate tracker.
			res, trace, evalErr := d.rules.MatchAllWithTrace(
				ctx,
				scope,
				matchRules,
				trigAct,
				triggerrule.WithBudget(budget),
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
				)
				if ctx.Err() != nil {
					return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
				}
				if derived != "" {
					fireSubject = derived
				}
			}
		}

		if lease != nil {
			held, lErr := lease.Renew(ctx)
			if lErr != nil {
				return durablequeue.Result{Err: errors.New("trigger: renew queue lease failed")}
			}
			if !held {
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
			ActionType:           tr.ActionType,
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
			skippedBusy bool
			existing    *store.TriggerFire
			cErr        error
		)
		if tr.SubjectMode == store.TriggerSubjectModeSkip {
			claimed, skippedBusy, existing, cErr = d.store.ClaimTriggerFireSubjectSkip(ctx, &fire, ErrFireSubjectBusy, claimNow)
		} else {
			claimed, existing, cErr = d.store.ClaimTriggerFire(ctx, &fire)
		}
		if cErr != nil {
			d.logger.Warn("trigger: failed to claim trigger fire",
				"eventID", ev.ID, "triggerID", tr.ID, "error", cErr)
			return durablequeue.Result{Err: errors.New("trigger: claim fire failed")}
		}
		if !claimed {
			// Display-only counter: on a lost-lease re-handle, if an earlier
			// attempt's claimed row is still in status "firing" (or transitions to
			// "fired" after this check), fire_count on trigger_events can
			// undercount; trigger_fires rows remain the authoritative fire log.
			if existing != nil && existing.Status == store.TriggerFireStatusFired {
				fireCount++
			}
			continue
		}
		if skippedBusy {
			writeCtx, cancelWrite := storeWriteContext(ctx)
			if rErr := d.store.RecordTriggerFire(writeCtx, tr.ID, claimNow, store.TriggerFireStatusSkipped, ErrFireSubjectBusy); rErr != nil {
				d.logger.Warn("trigger: failed to update trigger fire counters",
					"triggerID", tr.ID, "fireID", fire.ID, "error", rErr)
			}
			if d.fireAudit != nil {
				d.fireAudit.RecordFire(writeCtx, FireAuditRecord{
					ProjectID:      tr.ProjectID,
					TriggerID:      tr.ID,
					EventID:        ev.ID,
					FireID:         fire.ID,
					ActionType:     tr.ActionType,
					Status:         store.TriggerFireStatusSkipped,
					MatchedThrough: fire.MatchedThrough,
					DecidedDialect: fire.DecidedDialect,
				})
			}
			cancelWrite()
			continue
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
			ActionType: tr.ActionType,
			Payload:    tr.ActionPayload,
			Event: &hubfire.FireEvent{
				ID:         ev.ID,
				ProjectID:  ev.ProjectID,
				SourceKind: ev.SourceKind,
				SourceID:   ev.SourceID,
				Type:       ev.Type,
				Subject:    ev.Subject,
				OccurredAt: ev.OccurredAt,
				ReceivedAt: ev.ReceivedAt,
				Attributes: maps.Clone(ev.Attributes),
				Lists:      cloneStringListMap(ev.Lists),
				Payload:    rawPayload,
				Trigger:    &trigRef,
			},
			Executor: hubfire.ExecutorContext{
				Kind: ExecutorKindTriggerDrainer,
				ID:   ExecutorIDPrefixTriggerEvent + ev.ID,
			},
			FireAt:  ev.ReceivedAt,
			Tracked: tracker,
		}

		fireCtx, cancelFire := context.WithTimeout(ctx, d.perFireTimeout)
		out := d.core.Fire(fireCtx, req)
		cancelFire()

		statusStr := normalizeFireStatus(out.Status, out.Error)
		finishedAt := d.now().UTC()

		writeCtx, cancelWrite := storeWriteContext(ctx)
		var (
			finished  bool
			finishErr error
		)
		tracker.record(func() {
			finished, finishErr = d.store.FinishTriggerFire(writeCtx, fire.ID, statusStr, out.ResultRef, out.Error, finishedAt)
		})
		if finishErr != nil {
			cancelWrite()
			d.logger.Warn("trigger: failed to finish trigger fire",
				"fireID", fire.ID, "eventID", ev.ID, "triggerID", tr.ID, "error", finishErr)
			return durablequeue.Result{Err: errors.New("trigger: finish fire failed")}
		}
		if finished {
			d.recordFireMetric(tr.ActionType, statusStr, out.Error)
			if statusStr == store.TriggerFireStatusFired {
				fireCount++
			}
			if out.AfterRecord != nil {
				out.AfterRecord(writeCtx)
			}
			if rErr := d.store.RecordTriggerFire(writeCtx, tr.ID, finishedAt, statusStr, out.Error); rErr != nil {
				d.logger.Warn("trigger: failed to update trigger fire counters",
					"triggerID", tr.ID, "fireID", fire.ID, "error", rErr)
			}
			if d.fireAudit != nil {
				d.fireAudit.RecordFire(writeCtx, FireAuditRecord{
					ProjectID:      tr.ProjectID,
					TriggerID:      tr.ID,
					EventID:        ev.ID,
					FireID:         fire.ID,
					ActionType:     tr.ActionType,
					Status:         statusStr,
					MatchedThrough: fire.MatchedThrough,
					DecidedDialect: fire.DecidedDialect,
				})
			}
		}
		cancelWrite()
	}

	if ctx.Err() != nil {
		return durablequeue.Result{Err: errors.New("trigger: event handler canceled")}
	}
	if err := d.store.MarkTriggerEventProcessed(ctx, ev.ID, d.now().UTC(), matchCount, fireCount); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		d.logger.Warn("trigger: failed to mark event processed", "eventID", ev.ID, "error", err)
		return durablequeue.Result{Err: errors.New("trigger: mark event processed failed")}
	}
	return durablequeue.Result{Done: true}
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
	now := d.now().UTC()
	fire := store.TriggerFire{
		ID:                   uuid.NewString(),
		ProjectID:            tr.ProjectID,
		TriggerID:            tr.ID,
		EventID:              ev.ID,
		Subject:              subject,
		ActionType:           tr.ActionType,
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
	claimed, _, err := d.store.ClaimTriggerFire(ctx, &fire)
	if err != nil {
		return err
	}
	if claimed {
		d.recordFireMetric(tr.ActionType, status, errMsg)
		if rErr := d.store.RecordTriggerFire(ctx, tr.ID, now, status, errMsg); rErr != nil {
			d.logger.Warn("trigger: failed to record direct fire counter",
				"triggerID", tr.ID, "fireID", fire.ID, "error", rErr)
		}
		if d.fireAudit != nil {
			d.fireAudit.RecordFire(ctx, FireAuditRecord{
				ProjectID:      tr.ProjectID,
				TriggerID:      tr.ID,
				EventID:        ev.ID,
				FireID:         fire.ID,
				ActionType:     tr.ActionType,
				Status:         status,
				MatchedThrough: matchedThrough,
				DecidedDialect: decidedDialect,
				Reason:         auditReason,
			})
		}
	}
	return nil
}

func normalizeFireStatus(st hubfire.FireStatus, errMsg string) string {
	switch st {
	case hubfire.FireStatusFired:
		return store.TriggerFireStatusFired
	case hubfire.FireStatusFailed:
		return store.TriggerFireStatusFailed
	case hubfire.FireStatusRefused:
		return store.TriggerFireStatusRefused
	case hubfire.FireStatusSkipped:
		return store.TriggerFireStatusSkipped
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
	FiresReaped      int
	EventsReconciled int
	EventsPurged     int
	FiresPurged      int
}

// RunMaintenance executes the hourly trigger maintenance pass (§4.3, §6.4,
// rev 7.4):
//  1. Reaps expired firing rows (lease_until <= now) to failed "hub restarted during fire".
//  2. Reconciles pending trigger_events older than DefaultMaxAge (72h) with no
//     trigger.event queue item to failed "dropped by queue limits".
//  3. Purges terminal trigger_fires and trigger_events older than
//     HistoryRetention (7 days).
func RunMaintenance(ctx context.Context, ts store.TriggerStore, now time.Time) (MaintenanceResult, error) {
	var out MaintenanceResult
	if ts == nil {
		return out, fmt.Errorf("%w: trigger store is nil", store.ErrInvalidInput)
	}
	now = now.UTC()

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

	eventsPurged, firesPurged, err := ts.PurgeOldTriggerHistory(ctx, now.Add(-HistoryRetention))
	out.EventsPurged = eventsPurged
	out.FiresPurged = firesPurged
	if err != nil {
		errs = append(errs, fmt.Errorf("purge old trigger history: %w", err))
	}

	return out, errors.Join(errs...)
}
