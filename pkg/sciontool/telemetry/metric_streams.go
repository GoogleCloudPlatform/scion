package telemetry

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// The receiver keeps one current point per stream. Refusing a new stream at
// capacity preserves already accepted data; idle streams are expired only
// after their last output has been handed to the exporter.
const (
	maxActiveMetricStreams = 2048
	maxDuplicateIntervals  = 64
	metricStreamIdleTTL    = 30 * time.Minute
)

const hookMetricScope = "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks/handlers"

// LifecycleMetricScope separates init lifecycle counters from hook subprocess
// counters that may share a name and point labels but use a different writer.
const LifecycleMetricScope = hookMetricScope + "/lifecycle"

type metricStreamKey struct {
	resource, scope, attrs      string
	name, unit, kind, valueType string
	temporality                 metricpb.AggregationTemporality
	monotonic                   bool
}

type metricStream struct {
	resource       *metricpb.ResourceMetrics
	scope          *metricpb.ScopeMetrics
	metric         *metricpb.Metric
	start          uint64
	end            uint64
	lastSeen       time.Time
	dirty          bool
	pending        bool
	hook           bool
	collectorEpoch uint64 // GCP hook observation epoch; source intervals stay in start/end.
	cloudKey       cloudMetricIdentity
	seen           map[string]struct{}
	order          []metricFingerprint
	floorEnd       uint64
}

type metricFingerprint struct {
	key string
	end uint64
}

type metricStreams struct {
	streams         map[metricStreamKey]*metricStream
	rejected        map[string]uint64
	now             func() time.Time
	gcp             bool
	descriptors     map[string]metricDescriptorShape
	cloudIdentities map[cloudMetricIdentity]metricIdentityFull
}

type metricDescriptorShape struct{ kind, unit, valueType, labels string }
type cloudMetricIdentity struct{ resource, scope, point, name, unit, kind string }
type metricIdentityFull struct {
	resource, scope, point string
	temporality            metricpb.AggregationTemporality
	monotonic              bool
}

func (s *metricStreams) clone() *metricStreams {
	copyState := &metricStreams{streams: make(map[metricStreamKey]*metricStream, len(s.streams)), rejected: make(map[string]uint64, len(s.rejected)), descriptors: make(map[string]metricDescriptorShape, len(s.descriptors)), cloudIdentities: make(map[cloudMetricIdentity]metricIdentityFull, len(s.cloudIdentities)), now: s.now, gcp: s.gcp}
	for k, v := range s.rejected {
		copyState.rejected[k] = v
	}
	for k, v := range s.descriptors {
		copyState.descriptors[k] = v
	}
	for k, v := range s.cloudIdentities {
		copyState.cloudIdentities[k] = v
	}
	for k, v := range s.streams {
		entry := *v
		entry.resource = proto.Clone(v.resource).(*metricpb.ResourceMetrics)
		entry.scope = proto.Clone(v.scope).(*metricpb.ScopeMetrics)
		entry.metric = proto.Clone(v.metric).(*metricpb.Metric)
		entry.seen = make(map[string]struct{}, len(v.seen))
		for fingerprint := range v.seen {
			entry.seen[fingerprint] = struct{}{}
		}
		entry.order = append([]metricFingerprint(nil), v.order...)
		copyState.streams[k] = &entry
	}
	return copyState
}

func newMetricStreams() *metricStreams {
	return &metricStreams{streams: make(map[metricStreamKey]*metricStream), rejected: make(map[string]uint64), descriptors: make(map[string]metricDescriptorShape), cloudIdentities: make(map[cloudMetricIdentity]metricIdentityFull), now: time.Now}
}

func (s *metricStreams) reject(reason string) error {
	s.rejected[reason]++
	return errors.New(reason)
}

// canonicalAttrs retains OTLP value types and array order, while sorting all
// maps (including nested kvlists). Length-delimited protobuf wire encoding
// avoids delimiter and scalar-type collisions.
func canonicalAttrs(attrs []*commonpb.KeyValue) (string, error) {
	copyAttrs := make([]*commonpb.KeyValue, len(attrs))
	seen := make(map[string]struct{}, len(attrs))
	for i, kv := range attrs {
		if kv == nil {
			return "", errors.New("nil metric attribute")
		}
		if _, exists := seen[kv.Key]; exists {
			return "", errors.New("duplicate metric attribute")
		}
		seen[kv.Key] = struct{}{}
		copyAttrs[i] = proto.Clone(kv).(*commonpb.KeyValue)
		if err := canonicalValue(copyAttrs[i].Value); err != nil {
			return "", err
		}
	}
	sort.Slice(copyAttrs, func(i, j int) bool { return copyAttrs[i].Key < copyAttrs[j].Key })
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(&commonpb.KeyValueList{Values: copyAttrs})
	return string(b), err
}

func canonicalValue(value *commonpb.AnyValue) error {
	if value == nil {
		return nil
	}
	switch v := value.Value.(type) {
	case *commonpb.AnyValue_ArrayValue:
		if v.ArrayValue == nil {
			return errors.New("nil metric array")
		}
		for _, child := range v.ArrayValue.Values {
			if err := canonicalValue(child); err != nil {
				return err
			}
		}
	case *commonpb.AnyValue_KvlistValue:
		if v.KvlistValue == nil {
			return errors.New("nil metric map")
		}
		key, err := canonicalAttrs(v.KvlistValue.Values)
		if err != nil {
			return err
		}
		var sorted commonpb.KeyValueList
		if err := proto.Unmarshal([]byte(key), &sorted); err != nil {
			return err
		}
		v.KvlistValue.Values = sorted.Values
	}
	return nil
}

func resourceKey(rm *metricpb.ResourceMetrics) (string, error) {
	copyRM := proto.Clone(rm).(*metricpb.ResourceMetrics)
	copyRM.ScopeMetrics = nil
	if copyRM.Resource != nil {
		key, err := canonicalAttrs(copyRM.Resource.Attributes)
		if err != nil {
			return "", err
		}
		var attrs commonpb.KeyValueList
		if err := proto.Unmarshal([]byte(key), &attrs); err != nil {
			return "", err
		}
		copyRM.Resource.Attributes = attrs.Values
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(copyRM)
	return string(b), err
}

func scopeKey(sm *metricpb.ScopeMetrics) (string, error) {
	copySM := proto.Clone(sm).(*metricpb.ScopeMetrics)
	copySM.Metrics = nil
	if copySM.Scope != nil {
		key, err := canonicalAttrs(copySM.Scope.Attributes)
		if err != nil {
			return "", err
		}
		var attrs commonpb.KeyValueList
		if err := proto.Unmarshal([]byte(key), &attrs); err != nil {
			return "", err
		}
		copySM.Scope.Attributes = attrs.Values
	}
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(copySM)
	return string(b), err
}

func metricKind(m *metricpb.Metric) (string, metricpb.AggregationTemporality, bool, error) {
	switch data := m.Data.(type) {
	case *metricpb.Metric_Sum:
		if data.Sum == nil {
			return "", 0, false, errors.New("nil sum")
		}
		return "sum", data.Sum.AggregationTemporality, data.Sum.IsMonotonic, nil
	case *metricpb.Metric_Gauge:
		if data.Gauge == nil {
			return "", 0, false, errors.New("nil gauge")
		}
		return "gauge", 0, false, nil
	case *metricpb.Metric_Histogram:
		if data.Histogram == nil {
			return "", 0, false, errors.New("nil histogram")
		}
		return "histogram", data.Histogram.AggregationTemporality, false, nil
	default:
		return "", 0, false, errors.New("unsupported metric kind")
	}
}

func (s *metricStreams) add(rms []*metricpb.ResourceMetrics) error {
	if s.streams == nil {
		*s = *newMetricStreams()
	}
	s.expireDeliveredIdle()
	for _, rm := range rms {
		if rm == nil {
			return s.reject("nil resource metrics")
		}
		rk, err := resourceKey(rm)
		if err != nil {
			return s.reject(err.Error())
		}
		for _, sm := range rm.ScopeMetrics {
			if sm == nil {
				return s.reject("nil scope metrics")
			}
			sk, err := scopeKey(sm)
			if err != nil {
				return s.reject(err.Error())
			}
			for _, m := range sm.Metrics {
				if m == nil {
					return s.reject("nil metric")
				}
				kind, temporal, monotonic, err := metricKind(m)
				if err != nil {
					return s.reject(err.Error())
				}
				if sm.GetScope().GetName() == hookMetricScope && strings.HasPrefix(m.Name, "gen_ai.tokens.") {
					return s.reject("unsupported normalized hook token name")
				}
				if kind != "gauge" && temporal != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE && temporal != metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
					return s.reject("unsupported metric temporality")
				}
				if kind == "sum" && temporal == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA && !monotonic {
					return s.reject("nonmonotonic delta sum")
				}
				base := metricStreamKey{resource: rk, scope: sk, name: m.Name, unit: m.Unit, kind: kind, temporality: temporal, monotonic: monotonic}
				reservedHook := sm.GetScope().GetName() == hookMetricScope && isHookCounter(m.Name)
				hook := reservedHook && kind == "sum" && monotonic && temporal == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA
				if s.gcp && reservedHook && (!hook || m.Unit != hookCounterUnit(m.Name)) {
					return s.reject("unsupported normalized hook counter shape")
				}
				switch kind {
				case "sum", "gauge":
					var points []*metricpb.NumberDataPoint
					if kind == "sum" {
						points = m.GetSum().DataPoints
					} else {
						points = m.GetGauge().DataPoints
					}
					for _, point := range points {
						if point == nil || point.Value == nil {
							return s.reject("invalid number point")
						}
						if point.Flags != 0 {
							return s.reject("unsupported number point flags")
						}
						if temporal == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA && len(point.Exemplars) != 0 {
							return s.reject("delta exemplars unsupported")
						}
						if v, ok := point.Value.(*metricpb.NumberDataPoint_AsDouble); ok && (math.IsNaN(v.AsDouble) || math.IsInf(v.AsDouble, 0)) {
							return s.reject("nonfinite metric value")
						}
						if monotonic && (point.GetAsInt() < 0 || point.GetAsDouble() < 0) {
							return s.reject("negative monotonic sum")
						}
						if s.gcp {
							if err := s.validateDescriptor(rm, sm, m, kind, point.Attributes, point.Value); err != nil {
								return err
							}
							if err := s.validateCloudIdentity(rm, sm, m, kind, temporal, monotonic, point.Attributes); err != nil {
								return err
							}
						}
						key, err := canonicalAttrs(point.Attributes)
						if err != nil {
							return s.reject(err.Error())
						}
						k := base
						k.attrs = key
						switch point.Value.(type) {
						case *metricpb.NumberDataPoint_AsInt:
							k.valueType = "int64"
						case *metricpb.NumberDataPoint_AsDouble:
							k.valueType = "double"
						default:
							return s.reject("unsupported number point type")
						}
						if s.gcp && reservedHook && k.valueType != "int64" {
							return s.reject("unsupported normalized hook counter type")
						}
						if err := s.addNumber(k, rm, sm, m, point, hook); err != nil {
							return err
						}
					}
				case "histogram":
					for _, point := range m.GetHistogram().DataPoints {
						if point == nil {
							return s.reject("invalid histogram point")
						}
						if point.Flags != 0 {
							return s.reject("unsupported histogram point flags")
						}
						if temporal == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA && len(point.Exemplars) != 0 {
							return s.reject("delta exemplars unsupported")
						}
						if point.Sum != nil && (math.IsNaN(point.GetSum()) || math.IsInf(point.GetSum(), 0)) {
							return s.reject("nonfinite histogram sum")
						}
						if s.gcp {
							if err := s.validateDescriptor(rm, sm, m, kind, point.Attributes, nil); err != nil {
								return err
							}
							if err := s.validateCloudIdentity(rm, sm, m, kind, temporal, monotonic, point.Attributes); err != nil {
								return err
							}
						}
						key, err := canonicalAttrs(point.Attributes)
						if err != nil {
							return s.reject(err.Error())
						}
						k := base
						k.attrs = key
						k.valueType = "distribution"
						if err := s.addHistogram(k, rm, sm, m, point); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	return nil
}

func (s *metricStreams) validateCloudIdentity(rm *metricpb.ResourceMetrics, sm *metricpb.ScopeMetrics, metric *metricpb.Metric, kind string, temporality metricpb.AggregationTemporality, monotonic bool, attrs []*commonpb.KeyValue) error {
	for _, source := range []struct {
		attrs   []*commonpb.KeyValue
		allowed map[string]bool
		layer   string
	}{
		{rm.GetResource().GetAttributes(), cloudResourceFields, "resource"},
		{sm.GetScope().GetAttributes(), cloudScopeFields, "scope"},
		{attrs, cloudPointFieldsFor(sm.GetScope().GetName(), metric.Name), "point"},
	} {
		if err := checkCloudMetricFields(source.attrs, source.allowed, source.layer); err != nil {
			return s.reject(err.Error())
		}
	}
	if err := checkCloudSelfMetricFields(sm.GetScope().GetName(), metric.Name, attrs); err != nil {
		return s.reject(err.Error())
	}
	for _, values := range [][]*commonpb.KeyValue{allowedMetricAttrs(rm.GetResource().GetAttributes(), cloudResourceFields), allowedMetricAttrs(sm.GetScope().GetAttributes(), cloudScopeFields), allowedMetricAttrs(attrs, cloudPointFieldsFor(sm.GetScope().GetName(), metric.Name))} {
		for _, kv := range values {
			if proto.Size(kv.Value) > 256 {
				return s.reject("Cloud Monitoring identity value too long")
			}
		}
	}
	r, err := cloudResourceKey(rm)
	if err != nil {
		return s.reject(err.Error())
	}
	scope, err := cloudScopeKey(sm)
	if err != nil {
		return s.reject(err.Error())
	}
	point, err := canonicalAttrs(allowedMetricAttrs(attrs, cloudPointFieldsFor(sm.GetScope().GetName(), metric.Name)))
	if err != nil {
		return s.reject(err.Error())
	}
	fullR, err := resourceKey(rm)
	if err != nil {
		return s.reject(err.Error())
	}
	fullS, err := scopeKey(sm)
	if err != nil {
		return s.reject(err.Error())
	}
	fullP, err := canonicalAttrs(attrs)
	if err != nil {
		return s.reject(err.Error())
	}
	key := cloudMetricIdentity{resource: r, scope: scope, point: point, name: metric.Name, unit: metric.Unit, kind: kind}
	full := metricIdentityFull{resource: fullR, scope: fullS, point: fullP, temporality: temporality, monotonic: monotonic}
	if previous, exists := s.cloudIdentities[key]; exists {
		if previous.resource != full.resource || previous.scope != full.scope || previous.point != full.point {
			return s.reject("unsupported Cloud Monitoring identity dimension")
		}
		if previous.temporality != full.temporality || previous.monotonic != full.monotonic {
			return s.reject("incompatible Cloud Monitoring series writers")
		}
		return nil
	}
	if len(s.cloudIdentities) >= maxActiveMetricStreams {
		return s.reject("Cloud Monitoring identity limit")
	}
	s.cloudIdentities[key] = full
	return nil
}

func cloudIdentityFor(rm *metricpb.ResourceMetrics, sm *metricpb.ScopeMetrics, metric *metricpb.Metric, attrs []*commonpb.KeyValue, kind string) (cloudMetricIdentity, error) {
	r, err := cloudResourceKey(rm)
	if err != nil {
		return cloudMetricIdentity{}, err
	}
	scope, err := cloudScopeKey(sm)
	if err != nil {
		return cloudMetricIdentity{}, err
	}
	point, err := canonicalAttrs(allowedMetricAttrs(attrs, cloudPointFieldsFor(sm.GetScope().GetName(), metric.Name)))
	if err != nil {
		return cloudMetricIdentity{}, err
	}
	return cloudMetricIdentity{resource: r, scope: scope, point: point, name: metric.Name, unit: metric.Unit, kind: kind}, nil
}

func (s *metricStreams) validateDescriptor(rm *metricpb.ResourceMetrics, sm *metricpb.ScopeMetrics, m *metricpb.Metric, kind string, attrs []*commonpb.KeyValue, number any) error {
	labels := map[string]struct{}{}
	add := func(key string) error {
		name := cloudLabelKey(key)
		if name == "" || len(name) > 100 {
			return s.reject("invalid Cloud Monitoring label key")
		}
		if _, exists := labels[name]; exists {
			return s.reject("colliding Cloud Monitoring label keys")
		}
		labels[name] = struct{}{}
		return nil
	}
	for _, kv := range rm.GetResource().GetAttributes() {
		if kv.Key == "service.name" || kv.Key == "service.namespace" || kv.Key == "service.instance.id" {
			if kv.Value.GetStringValue() != "" {
				if len(kv.Value.GetStringValue()) > 256 {
					return s.reject("Cloud Monitoring identity label too long")
				}
				if err := add(kv.Key); err != nil {
					return err
				}
			}
		}
		if kv.Key == "scion.agent.id" && kv.Value.GetStringValue() != "" {
			if len(kv.Value.GetStringValue()) > 256 {
				return s.reject("Cloud Monitoring identity label too long")
			}
			if err := add(gcpAgentLabel); err != nil {
				return err
			}
		}
		if kv.Key == "scion.project.id" && kv.Value.GetStringValue() != "" {
			if len(kv.Value.GetStringValue()) > 256 {
				return s.reject("Cloud Monitoring identity label too long")
			}
			if err := add(gcpProjectLabel); err != nil {
				return err
			}
		}
	}
	for _, kv := range attrs {
		normalized := cloudLabelKey(kv.Key)
		for _, reserved := range []string{gcpResourceIDLabel, gcpScopeIDLabel, gcpPointIDLabel, gcpAgentLabel, gcpProjectLabel, "service_name", "service_namespace", "service_instance_id"} {
			if normalized == reserved {
				return s.reject("reserved Cloud Monitoring metric label")
			}
		}
		switch kv.Key {
		case gcpResourceIDLabel, gcpScopeIDLabel, gcpPointIDLabel, gcpAgentLabel, gcpProjectLabel:
			return s.reject("reserved Cloud Monitoring metric label")
		}
		if cloudPointFieldsFor(sm.GetScope().GetName(), m.Name)[kv.Key] {
			if proto.Size(kv.Value) > 256 {
				return s.reject("Cloud Monitoring point label too long")
			}
			if err := add(kv.Key); err != nil {
				return err
			}
		}
	}
	for _, key := range []string{gcpResourceIDLabel, gcpScopeIDLabel, gcpPointIDLabel} {
		if err := add(key); err != nil {
			return err
		}
	}
	if len(labels) > 30 {
		return s.reject("Cloud Monitoring descriptor label limit")
	}
	names := make([]string, 0, len(labels))
	for key := range labels {
		names = append(names, key)
	}
	sort.Strings(names)
	shape := metricDescriptorShape{kind: kind, unit: m.Unit, labels: strings.Join(names, "\x00")}
	switch kind {
	case "histogram":
		shape.valueType = "distribution"
	case "sum", "gauge":
		switch number.(type) {
		case *metricpb.NumberDataPoint_AsInt:
			shape.valueType = "int64"
		case *metricpb.NumberDataPoint_AsDouble:
			shape.valueType = "double"
		default:
			return s.reject("unsupported Cloud Monitoring number type")
		}
	}
	if kind == "sum" && !m.GetSum().IsMonotonic {
		shape.kind = "gauge"
	}
	if previous, exists := s.descriptors[m.Name]; exists {
		if previous != shape {
			return s.reject("incompatible Cloud Monitoring descriptor")
		}
		return nil
	}
	if len(s.descriptors) >= maxActiveMetricStreams {
		return s.reject("Cloud Monitoring descriptor limit")
	}
	s.descriptors[m.Name] = shape
	return nil
}

func cloudLabelKey(key string) string {
	if key == "" {
		return ""
	}
	key = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return '_'
	}, key)
	if key != "" && key[0] >= '0' && key[0] <= '9' {
		return "key_" + key
	}
	return key
}

func isHookCounter(name string) bool {
	switch name {
	case "agent.tool.calls", "agent.session.count", "gen_ai.api.calls", "scion.hook.tokens.input", "scion.hook.tokens.output", "scion.hook.tokens.cached":
		return true
	default:
		return false
	}
}

func hookCounterUnit(name string) string {
	switch name {
	case "agent.tool.calls", "gen_ai.api.calls":
		return "{call}"
	case "agent.session.count":
		return "{session}"
	default:
		return "{token}"
	}
}

func (s *metricStreams) entry(k metricStreamKey, rm *metricpb.ResourceMetrics, sm *metricpb.ScopeMetrics, m *metricpb.Metric, start, end uint64, hook bool) (*metricStream, error) {
	now := s.now()
	if entry := s.streams[k]; entry != nil {
		return entry, nil
	}
	if len(s.streams) >= maxActiveMetricStreams {
		return nil, s.reject("active metric stream limit")
	}
	entry := &metricStream{resource: proto.Clone(rm).(*metricpb.ResourceMetrics), scope: proto.Clone(sm).(*metricpb.ScopeMetrics), metric: proto.Clone(m).(*metricpb.Metric), start: start, end: end, hook: hook, seen: make(map[string]struct{}), lastSeen: now, dirty: true}
	if hook && s.gcp {
		entry.collectorEpoch = uint64(now.UnixNano())
	}
	entry.resource.ScopeMetrics = nil
	entry.scope.Metrics = nil
	switch k.kind {
	case "sum":
		entry.metric.GetSum().DataPoints = nil
	case "gauge":
		entry.metric.GetGauge().DataPoints = nil
	case "histogram":
		entry.metric.GetHistogram().DataPoints = nil
	}
	s.streams[k] = entry
	return entry, nil
}

// Registry entries live while at least one related stream can still accept a
// point. Retiring them only after delivered idle streams expire bounds churn
// without forgetting an active Cloud collision or descriptor shape.
func (s *metricStreams) expireDeliveredIdle() {
	now := s.now()
	removed := false
	for key, entry := range s.streams {
		if !entry.dirty && !entry.pending && now.Sub(entry.lastSeen) >= metricStreamIdleTTL {
			delete(s.streams, key)
			removed = true
		}
	}
	if !removed || !s.gcp {
		return
	}
	activeNames := make(map[string]struct{}, len(s.streams))
	type activeIdentity struct {
		full             metricIdentityFull
		name, unit, kind string
	}
	active := make(map[activeIdentity]struct{}, len(s.streams))
	for key := range s.streams {
		activeNames[key.name] = struct{}{}
		active[activeIdentity{full: metricIdentityFull{resource: key.resource, scope: key.scope, point: key.attrs, temporality: key.temporality, monotonic: key.monotonic}, name: key.name, unit: key.unit, kind: key.kind}] = struct{}{}
	}
	for name := range s.descriptors {
		if _, exists := activeNames[name]; !exists {
			delete(s.descriptors, name)
		}
	}
	for key, full := range s.cloudIdentities {
		identity := activeIdentity{full: full, name: key.name, unit: key.unit, kind: key.kind}
		if _, exists := active[identity]; !exists {
			delete(s.cloudIdentities, key)
		}
	}
}

func intervalKey(start, end uint64, value proto.Message) string {
	value = proto.Clone(value)
	switch point := value.(type) {
	case *metricpb.NumberDataPoint:
		key, _ := canonicalAttrs(point.Attributes)
		var attrs commonpb.KeyValueList
		_ = proto.Unmarshal([]byte(key), &attrs)
		point.Attributes = attrs.Values
	case *metricpb.HistogramDataPoint:
		key, _ := canonicalAttrs(point.Attributes)
		var attrs commonpb.KeyValueList
		_ = proto.Unmarshal([]byte(key), &attrs)
		point.Attributes = attrs.Values
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(value)
	return fmt.Sprintf("%d/%d/%x", start, end, b)
}

func samePoint(a, b proto.Message) bool { return intervalKey(0, 0, a) == intervalKey(0, 0, b) }

func (s *metricStreams) remember(entry *metricStream, fingerprint string, end uint64) {
	entry.seen[fingerprint] = struct{}{}
	entry.order = append(entry.order, metricFingerprint{fingerprint, end})
	if len(entry.order) > maxDuplicateIntervals {
		oldest := entry.order[0]
		delete(entry.seen, oldest.key)
		if oldest.end > entry.floorEnd {
			entry.floorEnd = oldest.end
		}
		entry.order = entry.order[1:]
	}
}

func (s *metricStreams) addNumber(k metricStreamKey, rm *metricpb.ResourceMetrics, sm *metricpb.ScopeMetrics, m *metricpb.Metric, point *metricpb.NumberDataPoint, hook bool) error {
	start, end := point.StartTimeUnixNano, point.TimeUnixNano
	if end == 0 || (k.kind == "sum" && (start == 0 || start >= end)) {
		return s.reject("invalid metric interval")
	}
	entry, err := s.entry(k, rm, sm, m, start, end, hook)
	if err != nil {
		return err
	}
	if s.gcp {
		entry.cloudKey, err = cloudIdentityFor(rm, sm, m, point.Attributes, k.kind)
		if err != nil {
			return s.reject(err.Error())
		}
	}
	copyPoint := proto.Clone(point).(*metricpb.NumberDataPoint)
	var current *metricpb.NumberDataPoint
	if k.kind == "sum" && len(entry.metric.GetSum().DataPoints) > 0 {
		current = entry.metric.GetSum().DataPoints[0]
	}
	if k.kind == "gauge" && len(entry.metric.GetGauge().DataPoints) > 0 {
		current = entry.metric.GetGauge().DataPoints[0]
	}
	if current != nil {
		if k.kind == "gauge" {
			if end < entry.end {
				return nil
			}
			if end == entry.end && !samePoint(current, copyPoint) {
				return s.reject("conflicting gauge timestamp")
			}
			if end == entry.end {
				return nil
			}
		} else if hook {
			fingerprint := intervalKey(start, end, copyPoint)
			if _, exists := entry.seen[fingerprint]; exists {
				return nil
			}
			if end <= entry.floorEnd {
				return s.reject("hook duplicate window expired")
			}
			if err := addNumberValue(current, copyPoint); err != nil {
				return s.reject(err.Error())
			}
			if start < entry.start {
				entry.start = start
			}
			if end > entry.end {
				entry.end = end
			}
			current.StartTimeUnixNano, current.TimeUnixNano = entry.start, entry.end
			s.remember(entry, fingerprint, end)
			entry.dirty = true
			entry.lastSeen = s.now()
			return nil
		} else if k.temporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
			if start != entry.start {
				if start < entry.start {
					return nil
				}
				if start < entry.end {
					return s.reject("ambiguous cumulative writer overlap")
				}
				if entry.dirty || entry.pending {
					return s.reject("cumulative reset before export")
				}
				entry.start = start
			} else {
				if end < entry.end {
					return nil
				}
				if end == entry.end && !samePoint(current, copyPoint) {
					return s.reject("conflicting cumulative writers")
				}
				if end == entry.end {
					return nil
				}
				if k.monotonic && numberDecreased(current, copyPoint) {
					return s.reject("decreasing cumulative sum")
				}
			}
		} else {
			fingerprint := intervalKey(start, end, copyPoint)
			if _, exists := entry.seen[fingerprint]; exists {
				return nil
			}
			if start < entry.end {
				return s.reject("overlapping delta intervals")
			}
			if err := addNumberValue(current, copyPoint); err != nil {
				return s.reject(err.Error())
			}
			entry.end = end
			current.TimeUnixNano = end
			current.StartTimeUnixNano = entry.start
			s.remember(entry, fingerprint, end)
			entry.dirty = true
			entry.lastSeen = s.now()
			return nil
		}
	}
	if k.kind == "sum" {
		entry.metric.GetSum().DataPoints = []*metricpb.NumberDataPoint{copyPoint}
		if k.temporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
			entry.metric.GetSum().AggregationTemporality = metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
			s.remember(entry, intervalKey(start, end, copyPoint), end)
		}
	} else {
		entry.metric.GetGauge().DataPoints = []*metricpb.NumberDataPoint{copyPoint}
	}
	entry.start, entry.end, entry.lastSeen, entry.dirty = start, end, s.now(), true
	return nil
}

func numberDecreased(current, next *metricpb.NumberDataPoint) bool {
	switch value := current.Value.(type) {
	case *metricpb.NumberDataPoint_AsInt:
		return next.GetAsInt() < value.AsInt
	case *metricpb.NumberDataPoint_AsDouble:
		return next.GetAsDouble() < value.AsDouble
	default:
		return false
	}
}

func addNumberValue(dst, src *metricpb.NumberDataPoint) error {
	switch v := dst.Value.(type) {
	case *metricpb.NumberDataPoint_AsInt:
		s, ok := src.Value.(*metricpb.NumberDataPoint_AsInt)
		if !ok {
			return errors.New("mixed number point types")
		}
		const maxInt = math.MaxInt64
		const minInt = math.MinInt64
		if (s.AsInt > 0 && v.AsInt > maxInt-s.AsInt) || (s.AsInt < 0 && v.AsInt < minInt-s.AsInt) {
			return errors.New("metric sum overflow")
		}
		v.AsInt += s.AsInt
	case *metricpb.NumberDataPoint_AsDouble:
		s, ok := src.Value.(*metricpb.NumberDataPoint_AsDouble)
		if !ok {
			return errors.New("mixed number point types")
		}
		value := v.AsDouble + s.AsDouble
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("metric sum overflow")
		}
		v.AsDouble = value
	default:
		return errors.New("unsupported number point type")
	}
	return nil
}

func (s *metricStreams) addHistogram(k metricStreamKey, rm *metricpb.ResourceMetrics, sm *metricpb.ScopeMetrics, m *metricpb.Metric, point *metricpb.HistogramDataPoint) error {
	start, end := point.StartTimeUnixNano, point.TimeUnixNano
	if start == 0 || end == 0 || start >= end || len(point.BucketCounts) != len(point.ExplicitBounds)+1 {
		return s.reject("invalid histogram interval or buckets")
	}
	lastBound := math.Inf(-1)
	for _, bound := range point.ExplicitBounds {
		if math.IsNaN(bound) || math.IsInf(bound, 0) || bound <= lastBound {
			return s.reject("invalid histogram bounds")
		}
		lastBound = bound
	}
	var bucketTotal uint64
	for _, count := range point.BucketCounts {
		if bucketTotal > ^uint64(0)-count {
			return s.reject("histogram bucket overflow")
		}
		bucketTotal += count
	}
	if bucketTotal != point.Count {
		return s.reject("histogram bucket count mismatch")
	}
	entry, err := s.entry(k, rm, sm, m, start, end, false)
	if err != nil {
		return err
	}
	if s.gcp {
		entry.cloudKey, err = cloudIdentityFor(rm, sm, m, point.Attributes, k.kind)
		if err != nil {
			return s.reject(err.Error())
		}
	}
	copyPoint := proto.Clone(point).(*metricpb.HistogramDataPoint)
	points := entry.metric.GetHistogram().DataPoints
	if len(points) > 0 {
		current := points[0]
		if !equalBounds(current.ExplicitBounds, copyPoint.ExplicitBounds) || (current.Sum == nil) != (copyPoint.Sum == nil) {
			return s.reject("incompatible histogram")
		}
		if k.temporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
			if start < entry.start {
				return nil
			}
			if start != entry.start && start < entry.end {
				return s.reject("ambiguous histogram writer overlap")
			}
			if start != entry.start && (entry.dirty || entry.pending) {
				return s.reject("histogram reset before export")
			}
			if start == entry.start && end < entry.end {
				return nil
			}
			if start == entry.start && end == entry.end {
				if !samePoint(current, copyPoint) {
					return s.reject("conflicting histogram writers")
				}
				return nil
			}
		} else {
			fingerprint := intervalKey(start, end, copyPoint)
			if _, exists := entry.seen[fingerprint]; exists {
				return nil
			}
			if start < entry.end {
				return s.reject("overlapping histogram intervals")
			}
			if current.Count > ^uint64(0)-copyPoint.Count {
				return s.reject("histogram count overflow")
			}
			for i := range current.BucketCounts {
				if current.BucketCounts[i] > ^uint64(0)-copyPoint.BucketCounts[i] {
					return s.reject("histogram bucket overflow")
				}
			}
			if current.Sum != nil && (math.IsNaN(current.GetSum()+copyPoint.GetSum()) || math.IsInf(current.GetSum()+copyPoint.GetSum(), 0)) {
				return s.reject("histogram sum overflow")
			}
			current.Count += copyPoint.Count
			if current.Sum != nil {
				value := current.GetSum() + copyPoint.GetSum()
				current.Sum = &value
			}
			for i := range current.BucketCounts {
				current.BucketCounts[i] += copyPoint.BucketCounts[i]
			}
			if copyPoint.Min != nil && (current.Min == nil || copyPoint.GetMin() < current.GetMin()) {
				value := copyPoint.GetMin()
				current.Min = &value
			}
			if copyPoint.Max != nil && (current.Max == nil || copyPoint.GetMax() > current.GetMax()) {
				value := copyPoint.GetMax()
				current.Max = &value
			}
			current.TimeUnixNano = end
			entry.end = end
			s.remember(entry, fingerprint, end)
			entry.dirty = true
			entry.lastSeen = s.now()
			return nil
		}
	}
	entry.metric.GetHistogram().DataPoints = []*metricpb.HistogramDataPoint{copyPoint}
	if k.temporality == metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_DELTA {
		entry.metric.GetHistogram().AggregationTemporality = metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
		s.remember(entry, intervalKey(start, end, copyPoint), end)
	}
	entry.start, entry.end, entry.lastSeen, entry.dirty = start, end, s.now(), true
	return nil
}

func equalBounds(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *metricStreams) snapshot() []*metricpb.ResourceMetrics {
	var output []*metricpb.ResourceMetrics
	for _, entry := range s.streams {
		if !entry.dirty {
			continue
		}
		rm := proto.Clone(entry.resource).(*metricpb.ResourceMetrics)
		sm := proto.Clone(entry.scope).(*metricpb.ScopeMetrics)
		sm.Metrics = []*metricpb.Metric{proto.Clone(entry.metric).(*metricpb.Metric)}
		rm.ScopeMetrics = []*metricpb.ScopeMetrics{sm}
		output = append(output, rm)
		entry.dirty = false
		entry.pending = true
	}
	return output
}

// snapshotGCP freezes a whole eligible batch. Its hook timestamps describe an
// actual observation of the accumulated value, leaving source intervals intact.
func (s *metricStreams) snapshotGCP(observed time.Time, possible map[cloudMetricIdentity]uint64) ([]*metricpb.ResourceMetrics, map[cloudMetricIdentity]uint64, bool) {
	end := observed.UnixNano()
	ends := make(map[cloudMetricIdentity]uint64)
	for _, entry := range s.streams {
		if !entry.dirty {
			continue
		}
		var pointEnd uint64
		if entry.hook {
			// The pinned Monitoring SDK changes intervals shorter than 2ms to
			// epoch+1ms. Wait for a real observation that needs no rewrite.
			if end <= 0 || uint64(end) <= entry.collectorEpoch || uint64(end)-entry.collectorEpoch < uint64(2*time.Millisecond) {
				return nil, nil, false
			}
			pointEnd = uint64(end)
		} else {
			pointEnd = mappedMonitoringEnd(entry)
		}
		if previous := possible[entry.cloudKey]; previous != 0 && (pointEnd < previous || pointEnd-previous < uint64(5*time.Second)) {
			return nil, nil, false
		}
		ends[entry.cloudKey] = pointEnd
	}
	var output []*metricpb.ResourceMetrics
	for _, entry := range s.streams {
		if !entry.dirty {
			continue
		}
		rm := proto.Clone(entry.resource).(*metricpb.ResourceMetrics)
		sm := proto.Clone(entry.scope).(*metricpb.ScopeMetrics)
		metric := proto.Clone(entry.metric).(*metricpb.Metric)
		if entry.hook {
			point := metric.GetSum().DataPoints[0]
			point.StartTimeUnixNano = entry.collectorEpoch
			point.TimeUnixNano = ends[entry.cloudKey]
		}
		sm.Metrics = []*metricpb.Metric{metric}
		rm.ScopeMetrics = []*metricpb.ScopeMetrics{sm}
		output = append(output, rm)
		entry.dirty = false
		entry.pending = true
	}
	return output, ends, true
}

// The pinned Monitoring exporter maps non-gauge intervals shorter than 2ms
// to start+1ms. Admission and freeze must use the end actually sent by it.
func mappedMonitoringEnd(entry *metricStream) uint64 {
	if entry.cloudKey.kind == "gauge" {
		return entry.end
	}
	if entry.end >= entry.start && entry.end-entry.start < uint64(2*time.Millisecond) {
		return entry.start + uint64(time.Millisecond)
	}
	return entry.end
}

// clearPendingMarker only releases snapshot markers. Confirmed delivery is
// recorded separately by the pipeline after an exporter success.
func (s *metricStreams) clearPendingMarker() {
	for _, entry := range s.streams {
		entry.pending = false
	}
}

func (s *metricStreams) hasDirty() bool {
	for _, entry := range s.streams {
		if entry.dirty {
			return true
		}
	}
	return false
}
