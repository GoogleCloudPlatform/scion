/*
Copyright 2026 The Scion Authors.
*/

package telemetry

import (
	"fmt"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func validateName(name string) error {
	if !safeSignalName.MatchString(name) {
		return fmt.Errorf("unsafe signal name")
	}
	return nil
}

func validateSpans(input []*tracepb.ResourceSpans) error {
	v := newAnyValueValidator()
	for _, rs := range input {
		if rs == nil {
			continue
		}
		if err := v.ValidateAttributes(rs.GetResource().GetAttributes()); err != nil {
			return err
		}
		for _, ss := range rs.ScopeSpans {
			if ss == nil {
				continue
			}
			if err := v.ValidateAttributes(ss.GetScope().GetAttributes()); err != nil {
				return err
			}
			for _, span := range ss.Spans {
				if span == nil {
					continue
				}
				if err := validateName(span.Name); err != nil {
					return err
				}
				if err := v.ValidateAttributes(span.Attributes); err != nil {
					return err
				}
				for _, event := range span.Events {
					if event == nil {
						continue
					}
					if err := validateName(event.Name); err != nil {
						return err
					}
					if err := v.ValidateAttributes(event.Attributes); err != nil {
						return err
					}
				}
				for _, link := range span.Links {
					if link != nil {
						if err := v.ValidateAttributes(link.Attributes); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func validateLogs(input []*logspb.ResourceLogs) error {
	v := newAnyValueValidator()
	for _, rl := range input {
		if rl == nil {
			continue
		}
		if err := v.ValidateAttributes(rl.GetResource().GetAttributes()); err != nil {
			return err
		}
		for _, sl := range rl.ScopeLogs {
			if sl == nil {
				continue
			}
			if err := v.ValidateAttributes(sl.GetScope().GetAttributes()); err != nil {
				return err
			}
			for _, record := range sl.LogRecords {
				if record == nil {
					continue
				}
				name, err := normalizedLogEventName(record)
				if err != nil {
					return err
				}
				if name != "" {
					if err := validateName(name); err != nil {
						return err
					}
				}
				if err := v.ValidateAttributes(record.Attributes); err != nil {
					return err
				}
				if record.Body != nil {
					if err := v.Validate(record.Body); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func validateMetrics(input []*metricpb.ResourceMetrics) error {
	v := newAnyValueValidator()
	for _, rm := range input {
		if rm == nil {
			continue
		}
		if err := v.ValidateAttributes(rm.GetResource().GetAttributes()); err != nil {
			return err
		}
		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				continue
			}
			if err := v.ValidateAttributes(sm.GetScope().GetAttributes()); err != nil {
				return err
			}
			for _, metric := range sm.Metrics {
				if metric == nil {
					continue
				}
				if err := validateName(metric.Name); err != nil {
					return err
				}
				if err := validateMetricAttributes(v, metric); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateMetricAttributes(v *anyValueValidator, metric *metricpb.Metric) error {
	check := func(attrs []*commonpb.KeyValue, exemplars []*metricpb.Exemplar) error {
		if err := v.ValidateAttributes(attrs); err != nil {
			return err
		}
		for _, exemplar := range exemplars {
			if exemplar != nil {
				if err := v.ValidateAttributes(exemplar.FilteredAttributes); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, point := range metric.GetGauge().GetDataPoints() {
		if point != nil {
			if err := check(point.Attributes, point.Exemplars); err != nil {
				return err
			}
		}
	}
	for _, point := range metric.GetSum().GetDataPoints() {
		if point != nil {
			if err := check(point.Attributes, point.Exemplars); err != nil {
				return err
			}
		}
	}
	for _, point := range metric.GetHistogram().GetDataPoints() {
		if point != nil {
			if err := check(point.Attributes, point.Exemplars); err != nil {
				return err
			}
		}
	}
	for _, point := range metric.GetExponentialHistogram().GetDataPoints() {
		if point != nil {
			if err := check(point.Attributes, point.Exemplars); err != nil {
				return err
			}
		}
	}
	for _, point := range metric.GetSummary().GetDataPoints() {
		if point != nil {
			if err := check(point.Attributes, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

func countSpans(input []*tracepb.ResourceSpans) int64 {
	var count int64
	for _, rs := range input {
		for _, ss := range rs.GetScopeSpans() {
			count += int64(len(ss.GetSpans()))
		}
	}
	return count
}

func countLogs(input []*logspb.ResourceLogs) int64 {
	var count int64
	for _, rl := range input {
		for _, sl := range rl.GetScopeLogs() {
			count += int64(len(sl.GetLogRecords()))
		}
	}
	return count
}

func countMetricDataPoints(input []*metricpb.ResourceMetrics) int64 {
	var count int64
	for _, rm := range input {
		for _, sm := range rm.GetScopeMetrics() {
			for _, metric := range sm.GetMetrics() {
				count += int64(len(metric.GetGauge().GetDataPoints()) + len(metric.GetSum().GetDataPoints()) + len(metric.GetHistogram().GetDataPoints()) + len(metric.GetExponentialHistogram().GetDataPoints()) + len(metric.GetSummary().GetDataPoints()))
			}
		}
	}
	return count
}
