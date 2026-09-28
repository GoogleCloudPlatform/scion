/*
Copyright 2025 The Scion Authors.
*/

package telemetry

import (
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
)

func protoResourceMetricsToSDK(resourceMetrics []*metricpb.ResourceMetrics) []metricdata.ResourceMetrics {
	result := make([]metricdata.ResourceMetrics, 0, len(resourceMetrics))
	for _, rm := range resourceMetrics {
		if rm == nil {
			continue
		}

		scopeMetrics := make([]metricdata.ScopeMetrics, 0, len(rm.ScopeMetrics))
		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				continue
			}

			metrics := make([]metricdata.Metrics, 0, len(sm.Metrics))
			for _, metric := range sm.Metrics {
				sdkMetric, ok := protoMetricToSDK(metric)
				if ok {
					metrics = append(metrics, sdkMetric)
				}
			}
			if len(metrics) == 0 {
				continue
			}

			scopeMetrics = append(scopeMetrics, metricdata.ScopeMetrics{
				Scope:   protoScopeToSDK(sm.Scope, sm.SchemaUrl),
				Metrics: metrics,
			})
		}
		if len(scopeMetrics) == 0 {
			continue
		}

		result = append(result, metricdata.ResourceMetrics{
			Resource:     protoResourceToSDK(rm.Resource, rm.SchemaUrl),
			ScopeMetrics: scopeMetrics,
		})
	}

	return result
}

func protoMetricToSDK(metric *metricpb.Metric) (metricdata.Metrics, bool) {
	if metric == nil {
		return metricdata.Metrics{}, false
	}

	sdkMetric := metricdata.Metrics{
		Name:        metric.Name,
		Description: metric.Description,
		Unit:        metric.Unit,
	}

	switch data := metric.Data.(type) {
	case *metricpb.Metric_Gauge:
		sdkMetric.Data = protoGaugeToSDK(data.Gauge)
	case *metricpb.Metric_Sum:
		sdkMetric.Data = protoSumToSDK(data.Sum)
	case *metricpb.Metric_Histogram:
		sdkMetric.Data = protoHistogramToSDK(data.Histogram)
	case *metricpb.Metric_ExponentialHistogram:
		sdkMetric.Data = protoExponentialHistogramToSDK(data.ExponentialHistogram)
	case *metricpb.Metric_Summary:
		sdkMetric.Data = protoSummaryToSDK(data.Summary)
	default:
		return metricdata.Metrics{}, false
	}

	return sdkMetric, true
}

func protoGaugeToSDK(gauge *metricpb.Gauge) metricdata.Aggregation {
	if gauge == nil {
		return metricdata.Gauge[int64]{}
	}

	if protoNumberPointsUseFloat(gauge.DataPoints) {
		points := make([]metricdata.DataPoint[float64], 0, len(gauge.DataPoints))
		for _, point := range gauge.DataPoints {
			if point == nil || isNoRecordedFlags(point.Flags) {
				continue
			}
			points = append(points, metricdata.DataPoint[float64]{
				Attributes: protoAttrSet(point.Attributes),
				StartTime:  unixNanosToTime(point.StartTimeUnixNano),
				Time:       unixNanosToTime(point.TimeUnixNano),
				Value:      protoNumberPointToFloat(point),
			})
		}
		return metricdata.Gauge[float64]{DataPoints: points}
	}

	points := make([]metricdata.DataPoint[int64], 0, len(gauge.DataPoints))
	for _, point := range gauge.DataPoints {
		if point == nil || isNoRecordedFlags(point.Flags) {
			continue
		}
		points = append(points, metricdata.DataPoint[int64]{
			Attributes: protoAttrSet(point.Attributes),
			StartTime:  unixNanosToTime(point.StartTimeUnixNano),
			Time:       unixNanosToTime(point.TimeUnixNano),
			Value:      point.GetAsInt(),
		})
	}
	return metricdata.Gauge[int64]{DataPoints: points}
}

func protoSumToSDK(sum *metricpb.Sum) metricdata.Aggregation {
	if sum == nil {
		return metricdata.Sum[int64]{}
	}

	temporality := protoMetricTemporality(sum.AggregationTemporality)
	if protoNumberPointsUseFloat(sum.DataPoints) {
		points := make([]metricdata.DataPoint[float64], 0, len(sum.DataPoints))
		for _, point := range sum.DataPoints {
			if point == nil || isNoRecordedFlags(point.Flags) {
				continue
			}
			points = append(points, metricdata.DataPoint[float64]{
				Attributes: protoAttrSet(point.Attributes),
				StartTime:  unixNanosToTime(point.StartTimeUnixNano),
				Time:       unixNanosToTime(point.TimeUnixNano),
				Value:      protoNumberPointToFloat(point),
			})
		}
		return metricdata.Sum[float64]{
			DataPoints:  points,
			Temporality: temporality,
			IsMonotonic: sum.IsMonotonic,
		}
	}

	points := make([]metricdata.DataPoint[int64], 0, len(sum.DataPoints))
	for _, point := range sum.DataPoints {
		if point == nil || isNoRecordedFlags(point.Flags) {
			continue
		}
		points = append(points, metricdata.DataPoint[int64]{
			Attributes: protoAttrSet(point.Attributes),
			StartTime:  unixNanosToTime(point.StartTimeUnixNano),
			Time:       unixNanosToTime(point.TimeUnixNano),
			Value:      point.GetAsInt(),
		})
	}
	return metricdata.Sum[int64]{
		DataPoints:  points,
		Temporality: temporality,
		IsMonotonic: sum.IsMonotonic,
	}
}

func protoHistogramToSDK(histogram *metricpb.Histogram) metricdata.Aggregation {
	if histogram == nil {
		return metricdata.Histogram[float64]{}
	}

	points := make([]metricdata.HistogramDataPoint[float64], 0, len(histogram.DataPoints))
	for _, point := range histogram.DataPoints {
		if point == nil || isNoRecordedFlags(point.Flags) {
			continue
		}

		dp := metricdata.HistogramDataPoint[float64]{
			Attributes:   protoAttrSet(point.Attributes),
			StartTime:    unixNanosToTime(point.StartTimeUnixNano),
			Time:         unixNanosToTime(point.TimeUnixNano),
			Count:        point.Count,
			Bounds:       append([]float64(nil), point.ExplicitBounds...),
			BucketCounts: append([]uint64(nil), point.BucketCounts...),
			Sum:          point.GetSum(),
		}
		if point.Sum != nil {
			dp.Sum = point.GetSum()
		}
		if point.Min != nil {
			dp.Min = metricdata.NewExtrema(point.GetMin())
		}
		if point.Max != nil {
			dp.Max = metricdata.NewExtrema(point.GetMax())
		}
		points = append(points, dp)
	}

	return metricdata.Histogram[float64]{
		DataPoints:  points,
		Temporality: protoMetricTemporality(histogram.AggregationTemporality),
	}
}

func protoExponentialHistogramToSDK(histogram *metricpb.ExponentialHistogram) metricdata.Aggregation {
	if histogram == nil {
		return metricdata.ExponentialHistogram[float64]{}
	}

	points := make([]metricdata.ExponentialHistogramDataPoint[float64], 0, len(histogram.DataPoints))
	for _, point := range histogram.DataPoints {
		if point == nil || isNoRecordedFlags(point.Flags) {
			continue
		}

		dp := metricdata.ExponentialHistogramDataPoint[float64]{
			Attributes:    protoAttrSet(point.Attributes),
			StartTime:     unixNanosToTime(point.StartTimeUnixNano),
			Time:          unixNanosToTime(point.TimeUnixNano),
			Count:         point.Count,
			Sum:           point.GetSum(),
			Scale:         point.Scale,
			ZeroCount:     point.ZeroCount,
			ZeroThreshold: point.ZeroThreshold,
			PositiveBucket: metricdata.ExponentialBucket{
				Offset: point.GetPositive().GetOffset(),
				Counts: append([]uint64(nil), point.GetPositive().GetBucketCounts()...),
			},
			NegativeBucket: metricdata.ExponentialBucket{
				Offset: point.GetNegative().GetOffset(),
				Counts: append([]uint64(nil), point.GetNegative().GetBucketCounts()...),
			},
		}
		if point.Min != nil {
			dp.Min = metricdata.NewExtrema(point.GetMin())
		}
		if point.Max != nil {
			dp.Max = metricdata.NewExtrema(point.GetMax())
		}
		points = append(points, dp)
	}

	return metricdata.ExponentialHistogram[float64]{
		DataPoints:  points,
		Temporality: protoMetricTemporality(histogram.AggregationTemporality),
	}
}

func protoSummaryToSDK(summary *metricpb.Summary) metricdata.Aggregation {
	if summary == nil {
		return metricdata.Summary{}
	}

	points := make([]metricdata.SummaryDataPoint, 0, len(summary.DataPoints))
	for _, point := range summary.DataPoints {
		if point == nil || isNoRecordedFlags(point.Flags) {
			continue
		}

		quantiles := make([]metricdata.QuantileValue, 0, len(point.QuantileValues))
		for _, value := range point.QuantileValues {
			if value == nil {
				continue
			}
			quantiles = append(quantiles, metricdata.QuantileValue{
				Quantile: value.Quantile,
				Value:    value.Value,
			})
		}

		points = append(points, metricdata.SummaryDataPoint{
			Attributes:     protoAttrSet(point.Attributes),
			StartTime:      unixNanosToTime(point.StartTimeUnixNano),
			Time:           unixNanosToTime(point.TimeUnixNano),
			Count:          point.Count,
			Sum:            point.Sum,
			QuantileValues: quantiles,
		})
	}

	return metricdata.Summary{DataPoints: points}
}

func protoAttrSet(attrs []*commonpb.KeyValue) attribute.Set {
	return attribute.NewSet(protoAttrsToSDK(attrs)...)
}

func protoMetricTemporality(temporality metricpb.AggregationTemporality) metricdata.Temporality {
	switch temporality {
	case metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA:
		return metricdata.DeltaTemporality
	case metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE:
		return metricdata.CumulativeTemporality
	default:
		return metricdata.CumulativeTemporality
	}
}

func protoNumberPointsUseFloat(points []*metricpb.NumberDataPoint) bool {
	for _, point := range points {
		if point == nil {
			continue
		}
		if _, ok := point.Value.(*metricpb.NumberDataPoint_AsDouble); ok {
			return true
		}
	}
	return false
}

func protoNumberPointToFloat(point *metricpb.NumberDataPoint) float64 {
	if point == nil {
		return 0
	}
	if _, ok := point.Value.(*metricpb.NumberDataPoint_AsDouble); ok {
		return point.GetAsDouble()
	}
	return float64(point.GetAsInt())
}

func isNoRecordedFlags(flags uint32) bool {
	mask := uint32(metricpb.DataPointFlags_DATA_POINT_FLAGS_NO_RECORDED_VALUE_MASK)
	return flags&mask == mask
}

func unixNanosToTime(nanos uint64) time.Time {
	if nanos == 0 {
		return time.Time{}
	}
	return time.Unix(0, int64(nanos))
}
