/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
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

var safeSignalName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.:/-]{0,127}$`)

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
	add("scion.broker.id", os.Getenv("SCION_BROKER_ID"))
	add("scion.broker.name", os.Getenv("SCION_BROKER_NAME"))
	return attrs
}

func (p *receiverPolicy) processSpans(input []*tracepb.ResourceSpans) []*tracepb.ResourceSpans {
	if p == nil {
		return input
	}
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
				if span == nil || !safeSignalName.MatchString(span.Name) || !p.filter.ShouldProcessSpan(span.Name) {
					continue
				}
				span.Attributes = p.redactor.RedactProtoAttributes(span.Attributes)
				keptEvents := span.Events[:0]
				for _, event := range span.Events {
					if event == nil || !safeSignalName.MatchString(event.Name) {
						continue
					}
					event.Attributes = p.redactor.RedactProtoAttributes(event.Attributes)
					keptEvents = append(keptEvents, event)
				}
				span.Events = keptEvents
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
	return result
}

func (p *receiverPolicy) processLogs(input []*logspb.ResourceLogs) []*logspb.ResourceLogs {
	if p == nil {
		return input
	}
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
				eventName := normalizedLogEventName(record)
				if eventName != "" && !safeSignalName.MatchString(eventName) {
					continue
				}
				if eventName == "" {
					if p.filter.HasIncludes() {
						continue
					}
				} else if !p.filter.ShouldProcess(eventName) {
					continue
				}
				if eventName != "" {
					record.EventName = eventName
					record.Attributes = upsertAttribute(record.Attributes, normalizedEventNameAttribute, eventName)
				}
				record.Attributes = p.redactor.RedactProtoAttributes(record.Attributes)
				if record.Body != nil {
					if isStructuredValue(record.Body) {
						record.Body = p.redactor.RedactProtoValue("", record.Body)
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
	return result
}

func (p *receiverPolicy) processMetrics(input []*metricpb.ResourceMetrics) []*metricpb.ResourceMetrics {
	if p == nil {
		return input
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
				if metric == nil || !safeSignalName.MatchString(metric.Name) {
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
	return result
}

func (p *receiverPolicy) processResource(resource *resourcepb.Resource) {
	if resource == nil {
		return
	}
	for _, identity := range p.identity {
		resource.Attributes = upsertAttribute(resource.Attributes, identity.Key, identity.Value.GetStringValue())
	}
	resource.Attributes = p.redactor.RedactProtoAttributes(resource.Attributes)
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

func normalizedLogEventName(record *logspb.LogRecord) string {
	if record.EventName != "" {
		return record.EventName
	}
	for _, key := range []string{normalizedEventNameAttribute, "event_name", "event.type"} {
		for _, attr := range record.Attributes {
			if attr != nil && attr.Key == key && attr.Value != nil {
				return attr.Value.GetStringValue()
			}
		}
	}
	return ""
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
