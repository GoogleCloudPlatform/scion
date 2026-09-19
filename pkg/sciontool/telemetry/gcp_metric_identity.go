package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// Monitoring only maps point labels and selected service resource attributes.
// These fixed labels retain permitted post-policy stream identity. Fields not
// explicitly allowed here must never enter a Cloud label or identity digest.
const (
	gcpResourceIDLabel = "scion_metric_resource_id"
	gcpScopeIDLabel    = "scion_metric_scope_id"
	gcpPointIDLabel    = "scion_metric_point_id"
	gcpAgentLabel      = "scion_agent_id"
	gcpProjectLabel    = "scion_project_id"
)

var cloudResourceFields = map[string]bool{
	"service.name": true, "service.namespace": true, "service.instance.id": true,
	"scion.agent.id": true, "scion.project.id": true, "scion.agent.slug": true,
	"scion.harness": true, "scion.model": true, "scion.broker.id": true, "scion.broker.name": true,
	"gcp.project_id": true,
}
var cloudScopeFields = map[string]bool{"component": true, "scope.kind": true, "scope.variant": true}
var cloudPointFields = map[string]bool{
	"agent_id": true, "project_id": true, "harness": true, "model": true,
	"tool_name": true, "status": true, "operation": true, "sensor": true,
	"phase": true, "run": true,
}

const pipelineMetricScope = "github.com/GoogleCloudPlatform/scion/pkg/sciontool/telemetry"

var cloudPipelineStatusFields = map[string]bool{"scion.telemetry.provider": true, "scion.telemetry.project_id": true}
var cloudExportErrorFields = map[string]bool{"signal": true, "error_type": true}

func cloudPointFieldsFor(scopeName, metricName string) map[string]bool {
	if scopeName == pipelineMetricScope {
		switch metricName {
		case "scion.telemetry.pipeline.status":
			return cloudPipelineStatusFields
		case "scion.telemetry.export.errors":
			return cloudExportErrorFields
		}
	}
	return cloudPointFields
}

func checkCloudSelfMetricFields(scopeName, metricName string, attrs []*commonpb.KeyValue) error {
	if scopeName != pipelineMetricScope || (metricName != "scion.telemetry.pipeline.status" && metricName != "scion.telemetry.export.errors") {
		return nil
	}
	for _, kv := range attrs {
		if _, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); !ok || len(kv.GetValue().GetStringValue()) > 256 {
			return fmt.Errorf("invalid Cloud Monitoring self metric dimension")
		}
		if metricName == "scion.telemetry.export.errors" {
			value := kv.GetValue().GetStringValue()
			if kv.Key == "signal" && value != "metrics" && value != "logs" && value != "spans" {
				return fmt.Errorf("invalid Cloud Monitoring self metric signal")
			}
			if kv.Key == "error_type" && value != "none" && value != "auth" && value != "quota" && value != "timeout" && value != "other" {
				return fmt.Errorf("invalid Cloud Monitoring self metric error type")
			}
		}
	}
	return nil
}

func forbiddenCloudMetricField(key string) bool {
	key = strings.NewReplacer(".", "_", "-", "_").Replace(strings.ToLower(key))
	for _, forbidden := range []string{"prompt", "conversation", "session_id", "tool_input", "tool_output", "payload", "message", "content"} {
		if strings.Contains(key, forbidden) {
			return true
		}
	}
	return false
}

func checkCloudMetricFields(attrs []*commonpb.KeyValue, allowed map[string]bool, layer string) error {
	for _, kv := range attrs {
		if kv == nil || forbiddenCloudMetricField(kv.Key) || forbiddenCloudMetricValue(kv.Value) {
			return fmt.Errorf("forbidden Cloud Monitoring metric dimension")
		}
		if !allowed[kv.Key] {
			return fmt.Errorf("unsupported Cloud Monitoring %s dimension", layer)
		}
		if layer == "resource" && (kv.Key == "gcp.project_id" || kv.Key == "scion.broker.id") {
			if _, ok := kv.GetValue().GetValue().(*commonpb.AnyValue_StringValue); !ok || len(kv.GetValue().GetStringValue()) > 256 {
				return fmt.Errorf("invalid Cloud Monitoring resource identity")
			}
		}
	}
	return nil
}

func forbiddenCloudMetricValue(value *commonpb.AnyValue) bool {
	if value == nil {
		return false
	}
	switch nested := value.Value.(type) {
	case *commonpb.AnyValue_ArrayValue:
		for _, child := range nested.ArrayValue.GetValues() {
			if forbiddenCloudMetricValue(child) {
				return true
			}
		}
	case *commonpb.AnyValue_KvlistValue:
		for _, child := range nested.KvlistValue.GetValues() {
			if child == nil || forbiddenCloudMetricField(child.Key) || forbiddenCloudMetricValue(child.Value) {
				return true
			}
		}
	}
	return false
}

func allowedMetricAttrs(attrs []*commonpb.KeyValue, allowed map[string]bool) []*commonpb.KeyValue {
	var result []*commonpb.KeyValue
	for _, kv := range attrs {
		if allowed[kv.Key] {
			result = append(result, kv)
		}
	}
	return result
}

func cloudResourceKey(rm *metricpb.ResourceMetrics) (string, error) {
	copyRM := proto.Clone(rm).(*metricpb.ResourceMetrics)
	if copyRM.Resource != nil {
		copyRM.Resource.Attributes = allowedMetricAttrs(copyRM.Resource.Attributes, cloudResourceFields)
	}
	return resourceKey(copyRM)
}

func cloudScopeKey(sm *metricpb.ScopeMetrics) (string, error) {
	copySM := proto.Clone(sm).(*metricpb.ScopeMetrics)
	if copySM.Scope != nil {
		copySM.Scope.Attributes = allowedMetricAttrs(copySM.Scope.Attributes, cloudScopeFields)
	}
	return scopeKey(copySM)
}

func metricAttrString(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.Key == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func gcpIdentityMetrics(input []*metricpb.ResourceMetrics) ([]*metricpb.ResourceMetrics, error) {
	output := make([]*metricpb.ResourceMetrics, 0, len(input))
	for _, source := range input {
		if source == nil {
			return nil, fmt.Errorf("nil Cloud Monitoring resource")
		}
		if err := checkCloudMetricFields(source.GetResource().GetAttributes(), cloudResourceFields, "resource"); err != nil {
			return nil, err
		}
		rm := proto.Clone(source).(*metricpb.ResourceMetrics)
		rkey, err := cloudResourceKey(source)
		if err != nil {
			return nil, err
		}
		resourceID := identityDigest(rkey)
		agentID := metricAttrString(source.GetResource().GetAttributes(), "scion.agent.id")
		projectID := metricAttrString(source.GetResource().GetAttributes(), "scion.project.id")
		if rm.Resource != nil {
			rm.Resource.Attributes = allowedMetricAttrs(rm.Resource.Attributes, cloudResourceFields)
		}
		for i, sm := range rm.ScopeMetrics {
			if sm == nil {
				return nil, fmt.Errorf("nil Cloud Monitoring scope")
			}
			if err := checkCloudMetricFields(source.ScopeMetrics[i].GetScope().GetAttributes(), cloudScopeFields, "scope"); err != nil {
				return nil, err
			}
			skey, err := cloudScopeKey(source.ScopeMetrics[i])
			if err != nil {
				return nil, err
			}
			scopeID := identityDigest(skey)
			if sm.Scope != nil {
				sm.Scope.Attributes = allowedMetricAttrs(sm.Scope.Attributes, cloudScopeFields)
			}
			for _, metric := range sm.Metrics {
				if metric == nil {
					return nil, fmt.Errorf("nil Cloud Monitoring metric")
				}
				if sm.GetScope().GetName() == hookMetricScope && strings.HasPrefix(metric.Name, "gen_ai.tokens.") {
					return nil, fmt.Errorf("unsupported normalized hook token name")
				}
				if _, _, _, err := metricKind(metric); err != nil {
					return nil, err
				}
				allowed := cloudPointFieldsFor(sm.GetScope().GetName(), metric.Name)
				for _, point := range metric.GetSum().GetDataPoints() {
					if err := addGCPIdentityLabels(&point.Attributes, allowed, sm.GetScope().GetName(), metric.Name, resourceID, scopeID, agentID, projectID); err != nil {
						return nil, err
					}
				}
				for _, point := range metric.GetGauge().GetDataPoints() {
					if err := addGCPIdentityLabels(&point.Attributes, allowed, sm.GetScope().GetName(), metric.Name, resourceID, scopeID, agentID, projectID); err != nil {
						return nil, err
					}
				}
				for _, point := range metric.GetHistogram().GetDataPoints() {
					if err := addGCPIdentityLabels(&point.Attributes, allowed, sm.GetScope().GetName(), metric.Name, resourceID, scopeID, agentID, projectID); err != nil {
						return nil, err
					}
				}
			}
		}
		output = append(output, rm)
	}
	return output, nil
}

func addGCPIdentityLabels(attrs *[]*commonpb.KeyValue, allowed map[string]bool, scopeName, metricName, resourceID, scopeID, agentID, projectID string) error {
	if err := checkCloudMetricFields(*attrs, allowed, "point"); err != nil {
		return err
	}
	if err := checkCloudSelfMetricFields(scopeName, metricName, *attrs); err != nil {
		return err
	}
	for _, kv := range *attrs {
		for _, reserved := range []string{gcpResourceIDLabel, gcpScopeIDLabel, gcpPointIDLabel, gcpAgentLabel, gcpProjectLabel, "service_name", "service_namespace", "service_instance_id"} {
			if cloudLabelKey(kv.Key) == reserved {
				return fmt.Errorf("reserved Cloud Monitoring metric label")
			}
		}
	}
	*attrs = allowedMetricAttrs(*attrs, allowed)
	key, err := canonicalAttrs(*attrs)
	if err != nil {
		return err
	}
	*attrs = append(*attrs,
		metricStringLabel(gcpResourceIDLabel, resourceID),
		metricStringLabel(gcpScopeIDLabel, scopeID),
		metricStringLabel(gcpPointIDLabel, identityDigest(key)),
	)
	if agentID != "" {
		*attrs = append(*attrs, metricStringLabel(gcpAgentLabel, agentID))
	}
	if projectID != "" {
		*attrs = append(*attrs, metricStringLabel(gcpProjectLabel, projectID))
	}
	return nil
}

func metricStringLabel(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func identityDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
