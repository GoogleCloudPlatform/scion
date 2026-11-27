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
// Pi (pi 1.3.0): (message_id, response_id, timestamp) from assistant
// message_end names one completed model request.
// Muse Code (muse 1.4.25 fixture): (request_id, response_id) from PostLLMCall
// names one completed or terminally failed model request (`request_id` is
// "<turn_id>:<step>:<attempt>", present on both success and failure).
func hookSourceIdentity(event *hooks.Event) (identity, sourceEventID string) {
	switch event.Dialect {
	case "antigravity":
		conv := rawScalar(event.Data.Raw, "conversationId")
		steps := rawScalar(event.Data.Raw, "initialNumSteps")
		inv := rawScalar(event.Data.Raw, "invocationNum")
		if conv != "" && steps != "" && inv != "" {
			return telemetrycontract.SourceIdentityComposite,
				telemetry.CompositeSourceEventID("hook", event.Dialect, event.RawName, conv, steps, inv)
		}
	case "pi":
		msgID := rawScalar(event.Data.Raw, "message_id")
		respID := rawScalar(event.Data.Raw, "response_id")
		ts := rawScalar(event.Data.Raw, "timestamp")
		if msgID != "" || respID != "" || ts != "" {
			return telemetrycontract.SourceIdentityComposite,
				telemetry.CompositeSourceEventID("hook", event.Dialect, event.RawName, msgID, respID, ts)
		}
	case "muse-code":
		reqID := rawScalar(event.Data.Raw, "request_id")
		respID := rawScalar(event.Data.Raw, "response_id")
		if reqID != "" || respID != "" {
			return telemetrycontract.SourceIdentityComposite,
				telemetry.CompositeSourceEventID("hook", event.Dialect, event.RawName, reqID, respID)
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
// Token presence from the dialect layer distinguishes measured zeros from
// absent fields: present categories (including 0) are written as non-nil
// pointers, absent categories remain nil, and TokenStatus is reported when
// all five canonical categories are present, partial when 1-4 are present,
// and not_reported when none are present. Hook token semantics match the
// counters: input is reported as-is, and reasoning is already included in
// output.
func hookUsageObservation(event *hooks.Event, now time.Time) telemetry.UsageObservation {
	status := telemetrycontract.StatusSuccess
	if event.Data.Error != "" {
		status = telemetrycontract.StatusError
	}
	present := func(ok bool, n int64) *int64 {
		if !ok {
			return nil
		}
		v := n
		return &v
	}
	dialect := event.Dialect
	if dialect == "" {
		dialect = "unknown"
	}
	inputDerivation := telemetrycontract.InputDerivationReported
	if event.Data.InputTokensDerived {
		inputDerivation = telemetrycontract.InputDerivationDerived
	}
	obs := telemetry.UsageObservation{
		SourceKind:              "hook." + dialect + ".model_end",
		EventTime:               now,
		Model:                   event.Data.Model,
		SessionID:               event.Data.SessionID,
		Status:                  status,
		Calls:                   1,
		Input:                   present(event.Data.HasInputTokens(), event.Data.InputTokens),
		Output:                  present(event.Data.HasOutputTokens(), event.Data.OutputTokens),
		CacheRead:               present(event.Data.HasCachedTokens(), event.Data.CachedTokens),
		CacheWrite:              present(event.Data.HasCacheWriteTokens(), event.Data.CacheWriteTokens),
		Reasoning:               present(event.Data.HasReasoningTokens(), event.Data.ReasoningTokens),
		ReasoningOverlapsOutput: true,
		InputDerivation:         inputDerivation,
		TokenStatus:             telemetrycontract.TokenStatusNotReported,
	}
	presentCount := 0
	for _, p := range []*int64{obs.Input, obs.Output, obs.CacheRead, obs.CacheWrite, obs.Reasoning} {
		if p != nil {
			presentCount++
		}
	}
	switch presentCount {
	case 0:
		obs.TokenStatus = telemetrycontract.TokenStatusNotReported
	case 5:
		obs.TokenStatus = telemetrycontract.TokenStatusReported
	default:
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
