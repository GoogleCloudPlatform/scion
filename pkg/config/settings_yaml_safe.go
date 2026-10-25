/*
Copyright 2026 The Scion Authors.
*/

package config

import (
	"fmt"

	yamlv3 "gopkg.in/yaml.v3"
)

// The yaml.v3 decoder panics, instead of returning an error, on some
// malformed mapping shapes (for example a "<<" merge key next to a
// non-scalar mapping key). Settings files are edited by hand, so the
// yaml.v3 decodes of settings file content go through the helpers below,
// which turn such a panic into an ordinary parse error. (The koanf YAML
// parser already returns this case as an error.)

// recoverYAMLPanic converts a recovered decoder panic into *err. It reports
// only the panic value, never a stack.
func recoverYAMLPanic(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("invalid YAML: %v", r)
	}
}

// unmarshalSettingsYAML is yamlv3.Unmarshal for settings file content: a
// decoder panic is returned as an error.
func unmarshalSettingsYAML(data []byte, out interface{}) (err error) {
	defer recoverYAMLPanic(&err)
	return yamlv3.Unmarshal(data, out)
}

// decodeSettingsYAMLNode is node.Decode for a node parsed from settings
// file content: a decoder panic is returned as an error.
func decodeSettingsYAMLNode(node *yamlv3.Node, out interface{}) (err error) {
	defer recoverYAMLPanic(&err)
	return node.Decode(out)
}
