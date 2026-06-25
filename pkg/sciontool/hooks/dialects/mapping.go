/*
Copyright 2026 The Scion Authors.
*/

package dialects

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"gopkg.in/yaml.v3"
)

// MappingDialectSpec defines a data-driven dialect loaded from a YAML file.
// It maps harness-specific event names to normalized Scion event names and
// optionally extracts fields from the event payload using dotted paths.
type MappingDialectSpec struct {
	Dialect         string                      `yaml:"dialect"`
	EventNameField  string                      `yaml:"event_name_field"`
	EventNameFields []string                    `yaml:"event_name_fields"`
	Mappings        map[string]MappingEntrySpec `yaml:"mappings"`
}

// MappingEntrySpec defines how a single harness event maps to a normalized event.
type MappingEntrySpec struct {
	Event  string            `yaml:"event"`
	Fields map[string]string `yaml:"fields,omitempty"`
}

// MappingDialect implements hooks.Dialect using a data-driven spec loaded from YAML.
type MappingDialect struct {
	spec MappingDialectSpec
}

// NewMappingDialect creates a MappingDialect from a parsed spec.
func NewMappingDialect(spec MappingDialectSpec) *MappingDialect {
	return &MappingDialect{spec: spec}
}

// Name returns the dialect name as declared in the spec.
func (d *MappingDialect) Name() string {
	return d.spec.Dialect
}

// Parse converts a harness event payload into a normalized Event using the
// mapping spec. Common fields (prompt, tool_name, message, etc.) are always
// extracted from top-level data. Custom field paths from the mapping entry
// override those defaults.
func (d *MappingDialect) Parse(data map[string]interface{}) (*hooks.Event, error) {
	rawName := d.eventName(data)
	if rawName == "" {
		return nil, fmt.Errorf("event name field(s) %v missing or empty in input data", d.eventNameFields())
	}

	normalizedName := rawName
	var entry *MappingEntrySpec
	if m, ok := d.spec.Mappings[rawName]; ok {
		normalizedName = m.Event
		entry = &m
	}

	event := &hooks.Event{
		Name:    normalizedName,
		RawName: rawName,
		Dialect: d.spec.Dialect,
		Data: hooks.EventData{
			Prompt:    getString(data, "prompt"),
			ToolName:  getString(data, "tool_name"),
			Message:   getString(data, "message"),
			Reason:    getString(data, "reason"),
			Source:    getString(data, "source"),
			SessionID: getString(data, "session_id"),
			Raw:       data,
		},
	}

	// Extract tool input/output if available as strings
	if val, ok := data["tool_input"]; ok {
		if str, ok := val.(string); ok {
			event.Data.ToolInput = str
		}
	}
	if val, ok := data["tool_output"]; ok {
		if str, ok := val.(string); ok {
			event.Data.ToolOutput = str
		}
	}

	// Extract status fields
	if val, ok := data["success"]; ok {
		if b, ok := val.(bool); ok {
			event.Data.Success = b
		}
	}
	if val, ok := data["error"]; ok {
		if str, ok := val.(string); ok {
			event.Data.Error = str
		}
	}

	// Apply custom field paths from the mapping entry, overriding defaults.
	if entry != nil {
		for field, path := range entry.Fields {
			applyFieldPath(data, field, path, &event.Data)
		}
	}

	// Run common token and file path extraction (shared with all dialects).
	extractTokens(data, &event.Data)
	extractFilePath(data, &event.Data)

	return event, nil
}

func (d *MappingDialect) eventName(data map[string]interface{}) string {
	fields := d.eventNameFields()
	for _, field := range fields {
		if rawName := getString(data, field); rawName != "" {
			return rawName
		}
	}
	return ""
}

func (d *MappingDialect) eventNameFields() []string {
	if len(d.spec.EventNameFields) > 0 {
		return d.spec.EventNameFields
	}
	if d.spec.EventNameField != "" {
		return []string{d.spec.EventNameField}
	}
	return nil
}

// applyFieldPath resolves a dotted path in the data and assigns the result
// to the named field on EventData.
func applyFieldPath(data map[string]interface{}, field, path string, ed *hooks.EventData) {
	switch field {
	case "tool_name":
		if v := resolveFieldPath(data, path); v != "" {
			ed.ToolName = v
		}
	case "tool_input":
		if v := resolveFieldPathRaw(data, path); v != "" {
			ed.ToolInput = v
		}
	case "tool_output":
		if v := resolveFieldPathRaw(data, path); v != "" {
			ed.ToolOutput = v
		}
	case "prompt":
		if v := resolveFieldPath(data, path); v != "" {
			ed.Prompt = v
		}
	case "message":
		if v := resolveFieldPath(data, path); v != "" {
			ed.Message = v
		}
	case "reason":
		if v := resolveFieldPath(data, path); v != "" {
			ed.Reason = v
		}
	case "source":
		if v := resolveFieldPath(data, path); v != "" {
			ed.Source = v
		}
	case "session_id":
		if v := resolveFieldPath(data, path); v != "" {
			ed.SessionID = v
		}
	case "error":
		if v := resolveFieldPath(data, path); v != "" {
			ed.Error = v
		}
	case "file_path":
		if v := resolveFieldPath(data, path); v != "" {
			ed.FilePath = v
		}
	case "assistant_text":
		if v := resolveFieldPath(data, path); v != "" {
			ed.AssistantText = v
		}
	case "input_tokens":
		if v := resolveFieldPathInt64(data, path); v > 0 {
			ed.InputTokens = v
		}
	case "output_tokens":
		if v := resolveFieldPathInt64(data, path); v > 0 {
			ed.OutputTokens = v
		}
	case "cached_tokens":
		if v := resolveFieldPathInt64(data, path); v > 0 {
			ed.CachedTokens = v
		}
	case "success":
		if val, found := resolveFieldPathBool(data, path); found {
			ed.Success = val
		}
	}
}

// resolveFieldPath walks a dotted path (e.g. ".toolCall.name") through nested
// maps and returns the string value at the terminal key. Returns "" on any
// miss or non-string terminal.
func resolveFieldPath(data map[string]interface{}, path string) string {
	val := walkPath(data, path)
	if val == nil {
		return ""
	}
	if str, ok := val.(string); ok {
		return str
	}
	return ""
}

// resolveFieldPathRaw walks a dotted path and returns the value as a string.
// Non-string terminal values are JSON-serialized (useful for tool_input which
// may be an object).
func resolveFieldPathRaw(data map[string]interface{}, path string) string {
	val := walkPath(data, path)
	if val == nil {
		return ""
	}
	if str, ok := val.(string); ok {
		return str
	}
	b, err := json.Marshal(val)
	if err != nil {
		return ""
	}
	return string(b)
}

// resolveFieldPathBool walks a dotted path and returns a boolean value.
func resolveFieldPathBool(data map[string]interface{}, path string) (bool, bool) {
	val := walkPath(data, path)
	if val == nil {
		return false, false
	}
	if b, ok := val.(bool); ok {
		return b, true
	}
	return false, false
}

// resolveFieldPathInt64 walks a dotted path and returns an int64 value.
// JSON numbers arrive as float64, so both float64 and integer types are handled.
func resolveFieldPathInt64(data map[string]interface{}, path string) int64 {
	val := walkPath(data, path)
	if val == nil {
		return 0
	}
	switch v := val.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// walkPath navigates nested maps following a dotted path like ".toolCall.name".
func walkPath(data map[string]interface{}, path string) interface{} {
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return nil
	}
	segments := strings.Split(path, ".")
	var current interface{} = data
	for _, seg := range segments {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil
		}
		current, ok = m[seg]
		if !ok {
			return nil
		}
	}
	return current
}

// LoadMappingDialect reads a dialect.yaml file and returns a MappingDialect.
func LoadMappingDialect(path string) (*MappingDialect, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading dialect spec: %w", err)
	}

	var spec MappingDialectSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parsing dialect spec: %w", err)
	}

	if spec.Dialect == "" {
		return nil, fmt.Errorf("dialect spec missing required 'dialect' field")
	}
	if spec.EventNameField == "" && len(spec.EventNameFields) == 0 {
		return nil, fmt.Errorf("dialect spec missing required 'event_name_field' or 'event_name_fields' field")
	}

	return NewMappingDialect(spec), nil
}

// DiscoverMappingDialect looks for a dialect.yaml in the well-known harness
// bundle path ($HOME/.scion/harness/dialect.yaml) and loads it if the declared
// dialect name matches the requested name.
func DiscoverMappingDialect(dialectName string) (*MappingDialect, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot determine home directory: %w", err)
	}

	path := filepath.Join(home, ".scion", "harness", "dialect.yaml")
	md, err := LoadMappingDialect(path)
	if err != nil {
		return nil, err
	}

	if md.Name() != dialectName {
		return nil, fmt.Errorf("dialect.yaml declares dialect %q but %q was requested", md.Name(), dialectName)
	}

	return md, nil
}
