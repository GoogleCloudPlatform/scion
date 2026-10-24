/*
Copyright 2026 The Scion Authors.
*/

package handlers

// telemetry_analytics.go is the hook-path producer of agent.usage analytics
// events (WP2 design note telemetry/design/wp2-design.md, sections 1-4). An
// event is appended to the analytics outbox from the same accepted
// model-end increment that records gen_ai.api.calls and scion.usage.tokens,
// before those counters, and only when SCION_USAGE_SOURCE=hooks.

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// hookSourceIdentity derives a vetted composite identity for dialects whose
// captured payloads carry one, else none. Antigravity (agy 1.2.12 fixture):
// conversationId plus initialNumSteps (advances across turns) plus
// invocationNum (advances within a turn) names one main-loop invocation.
func hookSourceIdentity(event *hooks.Event) (identity, sourceEventID string) {
	if event.Dialect == "antigravity" {
		conv := rawScalar(event.Data.Raw, "conversationId")
		steps := rawScalar(event.Data.Raw, "initialNumSteps")
		inv := rawScalar(event.Data.Raw, "invocationNum")
		if conv != "" && steps != "" && inv != "" {
			return telemetrycontract.SourceIdentityComposite,
				telemetry.CompositeSourceEventID("hook", event.Dialect, event.RawName, conv, steps, inv)
		}
	}
	return telemetrycontract.SourceIdentityNone, ""
}

// rawScalar renders one top-level raw payload scalar as a string ("" when
// absent or not a scalar). JSON numbers arrive as float64; integral values
// are printed without a fraction.
func rawScalar(raw map[string]interface{}, key string) string {
	switch v := raw[key].(type) {
	case string:
		return v
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	default:
		return ""
	}
}

// hookUsageObservation is the event form of a hook model-end increment.
// The dialect layer keeps only token values > 0, so a zero cannot be told
// from an absent field: only positive categories are written and the
// status is partial (some tokens) or not_reported (none), never an
// invented zero. Hook token semantics match the counters: input is
// reported as-is, and reasoning is already included in output.
func hookUsageObservation(event *hooks.Event, now time.Time) telemetry.UsageObservation {
	status := telemetrycontract.StatusSuccess
	if event.Data.Error != "" {
		status = telemetrycontract.StatusError
	}
	positive := func(n int64) *int64 {
		if n <= 0 {
			return nil
		}
		return &n
	}
	dialect := event.Dialect
	if dialect == "" {
		dialect = "unknown"
	}
	obs := telemetry.UsageObservation{
		SourceKind:              "hook." + dialect + ".model_end",
		EventTime:               now,
		Model:                   event.Data.Model,
		SessionID:               event.Data.SessionID,
		Status:                  status,
		Calls:                   1,
		Input:                   positive(event.Data.InputTokens),
		Output:                  positive(event.Data.OutputTokens),
		CacheRead:               positive(event.Data.CachedTokens),
		CacheWrite:              positive(event.Data.CacheWriteTokens),
		Reasoning:               positive(event.Data.ReasoningTokens),
		ReasoningOverlapsOutput: true,
		InputDerivation:         telemetrycontract.InputDerivationReported,
		TokenStatus:             telemetrycontract.TokenStatusNotReported,
	}
	if obs.Input != nil || obs.Output != nil || obs.CacheRead != nil || obs.CacheWrite != nil || obs.Reasoning != nil {
		obs.TokenStatus = telemetrycontract.TokenStatusPartial
	}
	obs.Identity, obs.SourceEventID = hookSourceIdentity(event)
	return obs
}

// recordUsageAnalytics appends the analytics event for a model-end
// increment, if hook analytics is wired. Failures are logged at debug and
// counted by the outbox; they never block the counters.
func (h *TelemetryHandler) recordUsageAnalytics(event *hooks.Event) {
	if h.Analytics == nil || !strings.EqualFold(event.Name, hooks.EventModelEnd) {
		return
	}
	if _, err := h.Analytics.Record(hookUsageObservation(event, time.Now())); err != nil {
		log.Debug("Hook usage analytics event not recorded: %v", err)
	}
}
