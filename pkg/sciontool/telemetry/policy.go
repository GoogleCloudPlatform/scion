/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"fmt"
	"os"
	"regexp"

	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

const normalizedEventNameAttribute = "event.name"

const claudeNativeLogScope = "com.anthropic.claude_code.events"

const policyAdmissionReason = "telemetry rejected by receiver admission policy"

var safeSignalName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]{0,127}$`)

var nativeEventNameAliases = map[string]string{
	"codex.user_prompt":      "agent.user.prompt",
	"codex.tool_result":      "agent.tool.result",
	"gemini_cli.user_prompt": "agent.user.prompt",
}

var reservedIdentityAttributes = map[string]struct{}{
	"scion.agent":       {},
	"scion.agent.id":    {},
	"scion.agent.slug":  {},
	"scion.project":     {},
	"scion.project.id":  {},
	"scion.project_id":  {},
	"scion.harness":     {},
	"scion.model":       {},
	"scion.broker":      {},
	"scion.broker.id":   {},
	"scion.broker.name": {},
}

type policyResult[T any] struct {
	Data     T
	Filtered int64
	Rejected int64
	Reason   string
}

type receiverPolicy struct {
	filter   *Filter
	redactor *Redactor
	identity []*commonpb.KeyValue
}

func newReceiverPolicy(config *Config) *receiverPolicy {
	if config == nil {
		return nil
	}
	return &receiverPolicy{
		filter:   NewFilter(config.Filter),
		redactor: NewRedactor(config.Redaction),
		identity: authoritativeIdentity(),
	}
}

func authoritativeIdentity() []*commonpb.KeyValue {
	var attrs []*commonpb.KeyValue
	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, &commonpb.KeyValue{Key: key, Value: stringProtoValue(value)})
		}
	}
	add("scion.agent.id", os.Getenv("SCION_AGENT_ID"))
	add("scion.agent.slug", os.Getenv("SCION_AGENT_SLUG"))
	add("scion.project.id", projectcompat.ProjectIDFromEnv(os.Getenv))
	add("scion.harness", os.Getenv("SCION_HARNESS"))
	add("scion.model", os.Getenv("SCION_MODEL"))
	add("scion.broker.id", os.Getenv("SCION_BROKER_ID"))
	add("scion.broker.name", os.Getenv("SCION_BROKER_NAME"))
	return attrs
}

func (p *receiverPolicy) processSpans(input []*tracepb.ResourceSpans) policyResult[[]*tracepb.ResourceSpans] {
	if p == nil {
		return policyResult[[]*tracepb.ResourceSpans]{Data: input}
	}
	if err := validateSpans(input); err != nil {
		return policyResult[[]*tracepb.ResourceSpans]{Rejected: countSpans(input), Reason: policyAdmissionReason}
	}
	var filtered int64
	result := make([]*tracepb.ResourceSpans, 0, len(input))
	for _, original := range input {
		if original == nil {
			continue
		}
		rs := proto.Clone(original).(*tracepb.ResourceSpans)
		if rs.Resource == nil {
			rs.Resource = &resourcepb.Resource{}
		}
		p.processResource(rs.Resource)
		keptScopes := rs.ScopeSpans[:0]
		for _, scopeSpans := range rs.ScopeSpans {
			if scopeSpans == nil {
				continue
			}
			p.processScope(scopeSpans.Scope)
			keptSpans := scopeSpans.Spans[:0]
			for _, span := range scopeSpans.Spans {
				if span == nil {
					continue
				}
				if !p.filter.ShouldProcessSpan(span.Name) {
					filtered++
					continue
				}
				span.Attributes = p.redactor.RedactProtoAttributes(span.Attributes)
				for _, event := range span.Events {
					if event != nil {
						event.Attributes = p.redactor.RedactProtoAttributes(event.Attributes)
					}
				}
				for _, link := range span.Links {
					if link != nil {
						link.Attributes = p.redactor.RedactProtoAttributes(link.Attributes)
					}
				}
				if span.Status != nil && span.Status.Message != "" {
					span.Status.Message = p.redactor.RedactProtoValue("span.status.message", stringProtoValue(span.Status.Message)).GetStringValue()
				}
				keptSpans = append(keptSpans, span)
			}
			scopeSpans.Spans = keptSpans
			if len(keptSpans) > 0 {
				keptScopes = append(keptScopes, scopeSpans)
			}
		}
		rs.ScopeSpans = keptScopes
		if len(keptScopes) > 0 {
			result = append(result, rs)
		}
	}
	return policyResult[[]*tracepb.ResourceSpans]{Data: result, Filtered: filtered}
}

func (p *receiverPolicy) processLogs(input []*logspb.ResourceLogs) policyResult[[]*logspb.ResourceLogs] {
	if p == nil {
		return policyResult[[]*logspb.ResourceLogs]{Data: input}
	}
	if err := validateLogs(input); err != nil {
		return policyResult[[]*logspb.ResourceLogs]{Rejected: countLogs(input), Reason: policyAdmissionReason}
	}
	var filtered int64
	result := make([]*logspb.ResourceLogs, 0, len(input))
	for _, original := range input {
		if original == nil {
			continue
		}
		rl := proto.Clone(original).(*logspb.ResourceLogs)
		if rl.Resource == nil {
			rl.Resource = &resourcepb.Resource{}
		}
		p.processResource(rl.Resource)
		keptScopes := rl.ScopeLogs[:0]
		for _, scopeLogs := range rl.ScopeLogs {
			if scopeLogs == nil {
				continue
			}
			p.processScope(scopeLogs.Scope)
			keptRecords := scopeLogs.LogRecords[:0]
			for _, record := range scopeLogs.LogRecords {
				if record == nil {
					continue
				}
				eventName, _ := normalizedLogEventName(record)
				if eventName == "" {
					if p.filter.HasIncludes() {
						filtered++
						continue
					}
				} else if !p.filter.ShouldProcess(eventName) {
					filtered++
					continue
				}
				record.Attributes = removeEventNameAttributes(record.Attributes)
				record.Attributes = p.redactor.RedactProtoAttributes(record.Attributes)
				if eventName != "" {
					record.EventName = eventName
					record.Attributes = append(record.Attributes, &commonpb.KeyValue{Key: normalizedEventNameAttribute, Value: stringProtoValue(eventName)})
				}
				if record.Body != nil {
					if scopeLogs.GetScope().GetName() == claudeNativeLogScope {
						// The installed Claude native body is free-form text. Keep it
						// private even if a custom policy omits log.body.
						record.Body = stringProtoValue("[REDACTED]")
					} else if isStructuredValue(record.Body) {
						record.Body, _ = p.redactor.RedactStructuredProtoValue("log.body", record.Body)
					} else {
						record.Body = p.redactor.RedactProtoValue("log.body", record.Body)
					}
				}
				keptRecords = append(keptRecords, record)
			}
			scopeLogs.LogRecords = keptRecords
			if len(keptRecords) > 0 {
				keptScopes = append(keptScopes, scopeLogs)
			}
		}
		rl.ScopeLogs = keptScopes
		if len(keptScopes) > 0 {
			result = append(result, rl)
		}
	}
	return policyResult[[]*logspb.ResourceLogs]{Data: result, Filtered: filtered}
}

func (p *receiverPolicy) processMetrics(input []*metricpb.ResourceMetrics) policyResult[[]*metricpb.ResourceMetrics] {
	if p == nil {
		return policyResult[[]*metricpb.ResourceMetrics]{Data: input}
	}
	if err := validateMetrics(input); err != nil {
		return policyResult[[]*metricpb.ResourceMetrics]{Rejected: countMetricDataPoints(input), Reason: policyAdmissionReason}
	}
	result := make([]*metricpb.ResourceMetrics, 0, len(input))
	for _, original := range input {
		if original == nil {
			continue
		}
		rm := proto.Clone(original).(*metricpb.ResourceMetrics)
		if rm.Resource == nil {
			rm.Resource = &resourcepb.Resource{}
		}
		p.processResource(rm.Resource)
		keptScopes := rm.ScopeMetrics[:0]
		for _, scopeMetrics := range rm.ScopeMetrics {
			if scopeMetrics == nil {
				continue
			}
			p.processScope(scopeMetrics.Scope)
			keptMetrics := scopeMetrics.Metrics[:0]
			for _, metric := range scopeMetrics.Metrics {
				if metric == nil {
					continue
				}
				p.processMetric(metric)
				keptMetrics = append(keptMetrics, metric)
			}
			scopeMetrics.Metrics = keptMetrics
			if len(keptMetrics) > 0 {
				keptScopes = append(keptScopes, scopeMetrics)
			}
		}
		rm.ScopeMetrics = keptScopes
		if len(keptScopes) > 0 {
			result = append(result, rm)
		}
	}
	return policyResult[[]*metricpb.ResourceMetrics]{Data: result}
}

func (p *receiverPolicy) processResource(resource *resourcepb.Resource) {
	if resource == nil {
		return
	}
	resource.Attributes = p.redactor.RedactProtoAttributes(resource.Attributes)
	resource.Attributes = removeReservedIdentityAttributes(resource.Attributes)
	resource.Attributes = append(resource.Attributes, cloneKeyValues(p.identity)...)
}

func (p *receiverPolicy) processScope(scope *commonpb.InstrumentationScope) {
	if scope != nil {
		scope.Attributes = p.redactor.RedactProtoAttributes(scope.Attributes)
	}
}

func (p *receiverPolicy) processMetric(metric *metricpb.Metric) {
	processExemplars := func(exemplars []*metricpb.Exemplar) {
		for _, exemplar := range exemplars {
			if exemplar != nil {
				exemplar.FilteredAttributes = p.redactor.RedactProtoAttributes(exemplar.FilteredAttributes)
			}
		}
	}
	switch data := metric.Data.(type) {
	case *metricpb.Metric_Gauge:
		if data.Gauge == nil {
			return
		}
		for _, point := range data.Gauge.DataPoints {
			if point == nil {
				continue
			}
			point.Attributes = p.redactor.RedactProtoAttributes(point.Attributes)
			processExemplars(point.Exemplars)
		}
	case *metricpb.Metric_Sum:
		if data.Sum == nil {
			return
		}
		for _, point := range data.Sum.DataPoints {
			if point == nil {
				continue
			}
			point.Attributes = p.redactor.RedactProtoAttributes(point.Attributes)
			processExemplars(point.Exemplars)
		}
	case *metricpb.Metric_Histogram:
		if data.Histogram == nil {
			return
		}
		for _, point := range data.Histogram.DataPoints {
			if point == nil {
				continue
			}
			point.Attributes = p.redactor.RedactProtoAttributes(point.Attributes)
			processExemplars(point.Exemplars)
		}
	case *metricpb.Metric_ExponentialHistogram:
		if data.ExponentialHistogram == nil {
			return
		}
		for _, point := range data.ExponentialHistogram.DataPoints {
			if point == nil {
				continue
			}
			point.Attributes = p.redactor.RedactProtoAttributes(point.Attributes)
			processExemplars(point.Exemplars)
		}
	case *metricpb.Metric_Summary:
		if data.Summary == nil {
			return
		}
		for _, point := range data.Summary.DataPoints {
			if point == nil {
				continue
			}
			point.Attributes = p.redactor.RedactProtoAttributes(point.Attributes)
		}
	}
}

func normalizedLogEventName(record *logspb.LogRecord) (string, error) {
	if record == nil {
		return "", nil
	}
	values := make([]string, 0, 4)
	if record.EventName != "" {
		values = append(values, normalizeNativeEventName(record.EventName))
	}
	for _, attr := range record.Attributes {
		if attr == nil || !isEventNameAttribute(attr.Key) {
			continue
		}
		if attr.Value == nil {
			return "", fmt.Errorf("event name attribute must be a string")
		}
		text, ok := attr.Value.Value.(*commonpb.AnyValue_StringValue)
		if !ok {
			return "", fmt.Errorf("event name attribute must be a string")
		}
		if text.StringValue != "" {
			values = append(values, normalizeNativeEventName(text.StringValue))
		}
	}
	if len(values) == 0 {
		return "", nil
	}
	canonical := values[0]
	for _, value := range values[1:] {
		if value != canonical {
			return "", fmt.Errorf("conflicting event name representations")
		}
	}
	return canonical, nil
}

func normalizeNativeEventName(name string) string {
	if normalized, ok := nativeEventNameAliases[name]; ok {
		return normalized
	}
	return name
}

func isEventNameAttribute(key string) bool {
	return key == normalizedEventNameAttribute || key == "event_name" || key == "event.type"
}

func removeEventNameAttributes(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	result := make([]*commonpb.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		if attr == nil || !isEventNameAttribute(attr.Key) {
			result = append(result, attr)
		}
	}
	return result
}

func removeReservedIdentityAttributes(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	result := make([]*commonpb.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		if attr == nil {
			result = append(result, attr)
			continue
		}
		if _, reserved := reservedIdentityAttributes[attr.Key]; !reserved {
			if projectcompat.IsLegacyTelemetryIdentityKey(attr.Key) {
				continue
			}
			result = append(result, attr)
		}
	}
	return result
}

func cloneKeyValues(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	result := make([]*commonpb.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		if attr != nil {
			result = append(result, proto.Clone(attr).(*commonpb.KeyValue))
		}
	}
	return result
}

func upsertAttribute(attrs []*commonpb.KeyValue, key, value string) []*commonpb.KeyValue {
	found := false
	for _, attr := range attrs {
		if attr != nil && attr.Key == key {
			attr.Value = stringProtoValue(value)
			found = true
		}
	}
	if found {
		return attrs
	}
	return append(attrs, &commonpb.KeyValue{Key: key, Value: stringProtoValue(value)})
}

func isStructuredValue(value *commonpb.AnyValue) bool {
	if value == nil {
		return false
	}
	switch value.Value.(type) {
	case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
		return true
	default:
		return false
	}
}
