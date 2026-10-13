package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClearableConfigPath(t *testing.T) {
	for path, want := range map[string]bool{
		"max_turns":         true,
		"thinking_level":    true,
		"resources":         true,
		"system_prompt":     true,
		"auth_selectedType": true,
		"env.FOO":           true,
		"env.":              false,
		"env":               false,
		"volumes":           false,
		"mcp_servers":       false,
		"skills":            false,
		"telemetry":         false,
		"harness":           false,
		"cleared_fields":    false,
		"":                  false,
	} {
		if got := ClearableConfigPath(path); got != want {
			t.Errorf("ClearableConfigPath(%q) = %v, want %v", path, got, want)
		}
	}
}

// Every clearable key must name a real ScionConfig json key, or a clear
// would silently do nothing.
func TestClearableConfigKeysAreScionConfigKeys(t *testing.T) {
	keys := map[string]bool{}
	typ := reflect.TypeOf(ScionConfig{})
	for i := 0; i < typ.NumField(); i++ {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		keys[name] = true
	}
	for k := range clearableConfigKeys {
		if !keys[k] {
			t.Errorf("clearable key %q is not an api.ScionConfig json key", k)
		}
	}
}

func TestScionConfigClearedFieldsWireOnly(t *testing.T) {
	var fromJSON ScionConfig
	if err := json.Unmarshal([]byte(`{"cleared_fields":["max_turns","env.FOO"]}`), &fromJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromJSON.ClearedFields, []string{"max_turns", "env.FOO"}) {
		t.Errorf("json ClearedFields = %v", fromJSON.ClearedFields)
	}
	out, err := json.Marshal(ScionConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "cleared_fields") {
		t.Errorf("empty ClearedFields marshalled: %s", out)
	}

	var fromYAML ScionConfig
	if err := yaml.Unmarshal([]byte("cleared_fields: [max_turns]\nmax_turns: 3\n"), &fromYAML); err != nil {
		t.Fatal(err)
	}
	if fromYAML.ClearedFields != nil {
		t.Errorf("a template set ClearedFields: %v", fromYAML.ClearedFields)
	}
}
