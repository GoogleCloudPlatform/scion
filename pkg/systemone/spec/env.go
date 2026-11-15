// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package spec

import "fmt"

// FieldKind identifies the content serialization kind of an input field.
type FieldKind string

const (
	FieldKindText FieldKind = "text"
	FieldKindJSON FieldKind = "json"
)

// TrustLabel marks whether a consumer field originates from trusted hub state
// or untrusted user/agent/external content (runtime-design §4.4).
type TrustLabel string

const (
	TrustUntrusted TrustLabel = "untrusted"
	TrustTrusted   TrustLabel = "trusted"
)

const (
	// ProbeEnvironmentName is the built-in P1 content environment name.
	ProbeEnvironmentName = "probe"
	// ProbeEnvironmentVersion is the schema version of the built-in probe environment.
	ProbeEnvironmentVersion = 1
	// ProbeFieldText is the sole text field exposed in the P1 probe environment.
	ProbeFieldText = "probe.text"
	// ProbeMaxBytes is the 16 KiB size ceiling on probe.text (P1 brief §2, AC-3).
	ProbeMaxBytes = 16 * 1024
)

// FieldDecl declares a named, bounded field inside a ContentEnvironment.
type FieldDecl struct {
	Name     string     `json:"name"`
	Kind     FieldKind  `json:"kind"`
	MaxBytes int        `json:"max_bytes"`
	Trust    TrustLabel `json:"trust"`
}

// ContentEnvironment defines the closed list of bounded input fields a rule in
// a consumer domain may reference (runtime-design §4.4).
type ContentEnvironment struct {
	Name          string      `json:"name"`
	Version       int         `json:"version"`
	Fields        []FieldDecl `json:"fields"`
	MaxTotalBytes int         `json:"max_total_bytes"`
}

// Field looks up a declared field by name.
func (e ContentEnvironment) Field(name string) (FieldDecl, bool) {
	for _, f := range e.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return FieldDecl{}, false
}

// ProbeEnvironment returns the built-in synthetic probe ContentEnvironment
// (one text field "probe.text", max 16 KiB, untrusted). Consumer environments
// land in P2.
func ProbeEnvironment() ContentEnvironment {
	return ContentEnvironment{
		Name:    ProbeEnvironmentName,
		Version: ProbeEnvironmentVersion,
		Fields: []FieldDecl{
			{
				Name:     ProbeFieldText,
				Kind:     FieldKindText,
				MaxBytes: ProbeMaxBytes,
				Trust:    TrustUntrusted,
			},
		},
		MaxTotalBytes: ProbeMaxBytes,
	}
}

// ValidateInputs checks that inputs is non-empty, duplicate-free, and every
// referenced field is declared in env (AC-3 compile half).
func ValidateInputs(inputs []string, env ContentEnvironment) error {
	if env.Name != ProbeEnvironmentName {
		return &CompileError{
			Path:     "environment",
			Category: CategoryInput,
			Message:  fmt.Sprintf("content environment %q is not supported in P1 (only %q is built in)", env.Name, ProbeEnvironmentName),
		}
	}
	if len(inputs) == 0 {
		return &CompileError{
			Path:     "inputs",
			Category: CategoryInput,
			Message:  "inputs must reference at least one field in the probe environment",
		}
	}
	seen := make(map[string]struct{}, len(inputs))
	for i, name := range inputs {
		path := fmt.Sprintf("inputs[%d]", i)
		if name == "" {
			return &CompileError{
				Path:     path,
				Category: CategoryInput,
				Message:  "input field name must not be empty",
			}
		}
		if _, dup := seen[name]; dup {
			return &CompileError{
				Path:     path,
				Category: CategoryInput,
				Message:  fmt.Sprintf("duplicate input field %q", name),
			}
		}
		seen[name] = struct{}{}
		if _, ok := env.Field(name); !ok {
			return &CompileError{
				Path:     path,
				Category: CategoryInput,
				Message:  fmt.Sprintf("field %q is not declared in content environment %q", name, env.Name),
			}
		}
	}
	return nil
}
