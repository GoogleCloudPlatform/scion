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
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
)

// Trigger rule evaluation metric names and attribute keys (mixed-rule-arrays §5).
// With unit "s", OpenTelemetry's Prometheus exporter exports MetricTriggerRuleEval
// as trigger_rule_eval_seconds{dialect, class}.
const (
	triggerInstrumentationScope = "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger"
	MetricTriggerRuleEval       = "trigger_rule_eval"
	MetricTriggerRuleEvalTotal  = "trigger_rule_eval_total"
	AttrRuleDialect             = "dialect"
	AttrRuleClass               = "class"
)

// MatchRules normalizes a stored Trigger's match_rules on read (mixed-rule-arrays
// §2.1, SEC-3b1rw-01). Normalization fails closed with triggerrule.ErrCorruptMatchRules
// whenever MatchRulesCorrupt is set or an element expression is empty or
// whitespace-only, so a corrupt stored row never degrades to an unconditional
// Types-only match. Element-count and per-element byte caps are enforced by
// writeAndValidateTriggerMatch at authoring/resume and by MatchAllWithTrace at
// evaluation time so multi-element authoring diagnostics remain per-element.
func MatchRules(tr store.Trigger) ([]triggerrule.Rule, error) {
	if tr.MatchRulesCorrupt {
		return nil, triggerrule.ErrCorruptMatchRules
	}
	if len(tr.MatchRules) > 0 {
		out := make([]triggerrule.Rule, len(tr.MatchRules))
		for i, r := range tr.MatchRules {
			if strings.TrimSpace(r.Expr) == "" {
				return nil, triggerrule.ErrCorruptMatchRules
			}
			d := strings.TrimSpace(r.Dialect)
			if d == "" {
				d = ruleengine.DialectCEL
			}
			out[i] = triggerrule.Rule{
				Dialect: d,
				Expr:    r.Expr,
			}
		}
		return out, nil
	}
	if tr.MatchRule != "" {
		if strings.TrimSpace(tr.MatchRule) == "" {
			return nil, triggerrule.ErrCorruptMatchRules
		}
		d := strings.TrimSpace(tr.MatchDialect)
		if d == "" {
			d = ruleengine.DialectCEL
		}
		return []triggerrule.Rule{{
			Dialect: d,
			Expr:    tr.MatchRule,
		}}, nil
	}
	return nil, nil
}

// RuleEvalKey identifies one (dialect, class) evaluation counter/duration bucket.
type RuleEvalKey struct {
	Dialect string
	Class   string
}

// RuleEvalBucket summarizes accumulated evaluations for a RuleEvalKey.
type RuleEvalBucket struct {
	Count         int64
	TotalDuration time.Duration
}

// RuleEvalMetrics observes per-dialect rule evaluation counts and durations (§5).
type RuleEvalMetrics interface {
	ObserveRuleEval(ctx context.Context, dialect, class string, elapsed time.Duration)
}

// OTelRuleEvalMetrics records per-dialect rule evaluation counts and durations
// using OpenTelemetry instruments (exported to Prometheus as
// trigger_rule_eval_seconds{dialect, class}).
type OTelRuleEvalMetrics struct {
	evalSeconds metric.Float64Histogram
	evalTotal   metric.Int64Counter
}

// NewOTelRuleEvalMetrics constructs OTel instruments for trigger rule evaluation.
func NewOTelRuleEvalMetrics(mp metric.MeterProvider) (*OTelRuleEvalMetrics, error) {
	if mp == nil {
		return nil, fmt.Errorf("trigger: nil MeterProvider")
	}
	meter := mp.Meter(triggerInstrumentationScope)
	hist, err := meter.Float64Histogram(
		MetricTriggerRuleEval,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of trigger match rule element evaluations in seconds."),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s histogram: %w", MetricTriggerRuleEval, err)
	}
	total, err := meter.Int64Counter(
		MetricTriggerRuleEvalTotal,
		metric.WithUnit("{evaluation}"),
		metric.WithDescription("Total trigger match rule element evaluations by dialect and outcome class."),
	)
	if err != nil {
		return nil, fmt.Errorf("creating %s counter: %w", MetricTriggerRuleEvalTotal, err)
	}
	return &OTelRuleEvalMetrics{
		evalSeconds: hist,
		evalTotal:   total,
	}, nil
}

// ObserveRuleEval records one element evaluation duration (in seconds) and count.
func (m *OTelRuleEvalMetrics) ObserveRuleEval(ctx context.Context, dialect, class string, elapsed time.Duration) {
	if m == nil {
		return
	}
	sec := max(elapsed.Seconds(), float64(0))
	opt := metric.WithAttributes(
		attribute.String(AttrRuleDialect, dialect),
		attribute.String(AttrRuleClass, class),
	)
	m.evalSeconds.Record(ctx, sec, opt)
	m.evalTotal.Add(ctx, 1, opt)
}

func normalizeMetricClass(cls string) string {
	switch cls {
	case triggerrule.MatchClassMatched,
		triggerrule.MatchClassFalse,
		triggerrule.MatchClassUncertain,
		triggerrule.MatchClassFailed,
		triggerrule.MatchClassBudget:
		return cls
	default:
		return triggerrule.MatchClassFailed
	}
}

func normalizeMetricDialect(dialect string, rules *triggerrule.Rules) string {
	safe := triggerrule.SafeDialectName(dialect)
	if safe == "unknown" {
		return "unknown"
	}
	if safe == ruleengine.DialectCEL || safe == "system_one" {
		return safe
	}
	if rules != nil && rules.IsRegisteredDialect(safe) {
		return safe
	}
	return "unknown"
}
