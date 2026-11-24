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

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
)

var (
	envNameRE       = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	fieldNameRE     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
	choiceSetNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

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

// ChoiceSetDecl declares a dynamic closed-choice set supplied at evaluation
// time inside a ContentEnvironment (runtime-design §4.4).
type ChoiceSetDecl struct {
	Name          string `json:"name"`
	MaxItems      int    `json:"max_items"`
	MaxLabelBytes int    `json:"max_label_bytes"`
}

// ContentEnvironment defines the closed list of bounded input fields and
// dynamic choice sets a rule in a consumer domain may reference
// (runtime-design §4.4).
type ContentEnvironment struct {
	Name          string          `json:"name"`
	Version       int             `json:"version"`
	Fields        []FieldDecl     `json:"fields"`
	ChoiceSets    []ChoiceSetDecl `json:"choice_sets,omitempty"`
	MaxTotalBytes int             `json:"max_total_bytes"`
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

// ChoiceSet looks up a declared dynamic choice set by name.
func (e ContentEnvironment) ChoiceSet(name string) (ChoiceSetDecl, bool) {
	for _, cs := range e.ChoiceSets {
		if cs.Name == name {
			return cs, true
		}
	}
	return ChoiceSetDecl{}, false
}

// Clone returns a defensive deep copy of e.
func (e ContentEnvironment) Clone() ContentEnvironment {
	cp := e
	if len(e.Fields) > 0 {
		cp.Fields = slices.Clone(e.Fields)
	}
	if len(e.ChoiceSets) > 0 {
		cp.ChoiceSets = slices.Clone(e.ChoiceSets)
	}
	return cp
}

// Validate checks that e is a well-formed ContentEnvironment declaration:
// valid name, positive version and MaxTotalBytes, at least one field, unique
// field and choice-set names, valid field kinds and trust labels, positive
// per-field and choice-set bounds, and per-entry bounds that do not exceed
// MaxTotalBytes (runtime-design §4.4).
func (e ContentEnvironment) Validate() error {
	if e.Name == "" || strings.TrimSpace(e.Name) != e.Name || !envNameRE.MatchString(e.Name) {
		return &CompileError{
			Path:     "environment.name",
			Category: CategoryInput,
			Message:  fmt.Sprintf("invalid content environment name %q (must match ^[a-z][a-z0-9_.-]{0,63}$)", e.Name),
		}
	}
	if e.Version < 1 {
		return &CompileError{
			Path:     "environment.version",
			Category: CategoryInput,
			Message:  fmt.Sprintf("content environment %q version must be >= 1, got %d", e.Name, e.Version),
		}
	}
	if e.MaxTotalBytes <= 0 {
		return &CompileError{
			Path:     "environment.max_total_bytes",
			Category: CategoryLimit,
			Message:  fmt.Sprintf("content environment %q max_total_bytes must be > 0, got %d", e.Name, e.MaxTotalBytes),
		}
	}
	if len(e.Fields) == 0 {
		return &CompileError{
			Path:     "environment.fields",
			Category: CategoryInput,
			Message:  fmt.Sprintf("content environment %q must declare at least one field", e.Name),
		}
	}
	seenFields := make(map[string]struct{}, len(e.Fields))
	for i, f := range e.Fields {
		fPath := fmt.Sprintf("environment.fields[%d]", i)
		if f.Name == "" || strings.TrimSpace(f.Name) != f.Name || !fieldNameRE.MatchString(f.Name) {
			return &CompileError{
				Path:     fPath + ".name",
				Category: CategoryInput,
				Message:  fmt.Sprintf("invalid field name %q (must match ^[a-z][a-z0-9_.-]{0,127}$)", f.Name),
			}
		}
		if _, dup := seenFields[f.Name]; dup {
			return &CompileError{
				Path:     fPath + ".name",
				Category: CategoryInput,
				Message:  fmt.Sprintf("duplicate field name %q in content environment %q", f.Name, e.Name),
			}
		}
		seenFields[f.Name] = struct{}{}
		switch f.Kind {
		case FieldKindText, FieldKindJSON:
		default:
			return &CompileError{
				Path:     fPath + ".kind",
				Category: CategoryInput,
				Message:  fmt.Sprintf("field %q has unsupported kind %q (expected text or json)", f.Name, f.Kind),
			}
		}
		if f.MaxBytes <= 0 {
			return &CompileError{
				Path:     fPath + ".max_bytes",
				Category: CategoryLimit,
				Message:  fmt.Sprintf("field %q max_bytes must be > 0, got %d", f.Name, f.MaxBytes),
			}
		}
		if f.MaxBytes > e.MaxTotalBytes {
			return &CompileError{
				Path:     fPath + ".max_bytes",
				Category: CategoryLimit,
				Message:  fmt.Sprintf("field %q max_bytes (%d) exceeds environment max_total_bytes (%d)", f.Name, f.MaxBytes, e.MaxTotalBytes),
			}
		}
		switch f.Trust {
		case TrustTrusted, TrustUntrusted:
		default:
			return &CompileError{
				Path:     fPath + ".trust",
				Category: CategoryInput,
				Message:  fmt.Sprintf("field %q has invalid trust label %q (expected trusted or untrusted)", f.Name, f.Trust),
			}
		}
	}
	seenChoiceSets := make(map[string]struct{}, len(e.ChoiceSets))
	for i, cs := range e.ChoiceSets {
		csPath := fmt.Sprintf("environment.choice_sets[%d]", i)
		if cs.Name == "" || strings.TrimSpace(cs.Name) != cs.Name || !choiceSetNameRE.MatchString(cs.Name) {
			return &CompileError{
				Path:     csPath + ".name",
				Category: CategoryInput,
				Message:  fmt.Sprintf("invalid choice set name %q (must match ^[a-z][a-z0-9_-]{0,63}$)", cs.Name),
			}
		}
		if _, dup := seenChoiceSets[cs.Name]; dup {
			return &CompileError{
				Path:     csPath + ".name",
				Category: CategoryInput,
				Message:  fmt.Sprintf("duplicate choice set name %q in content environment %q", cs.Name, e.Name),
			}
		}
		seenChoiceSets[cs.Name] = struct{}{}
		if cs.MaxItems <= 0 {
			return &CompileError{
				Path:     csPath + ".max_items",
				Category: CategoryLimit,
				Message:  fmt.Sprintf("choice set %q max_items must be > 0, got %d", cs.Name, cs.MaxItems),
			}
		}
		if cs.MaxLabelBytes <= 0 {
			return &CompileError{
				Path:     csPath + ".max_label_bytes",
				Category: CategoryLimit,
				Message:  fmt.Sprintf("choice set %q max_label_bytes must be > 0, got %d", cs.Name, cs.MaxLabelBytes),
			}
		}
		if cs.MaxLabelBytes > e.MaxTotalBytes {
			return &CompileError{
				Path:     csPath + ".max_label_bytes",
				Category: CategoryLimit,
				Message:  fmt.Sprintf("choice set %q max_label_bytes (%d) exceeds environment max_total_bytes (%d)", cs.Name, cs.MaxLabelBytes, e.MaxTotalBytes),
			}
		}
	}
	return nil
}

// EnvironmentRegistry is a concurrency-safe registry of named, versioned
// ContentEnvironments. The built-in "probe" environment is always pre-registered
// and owned by the "probe" consumer.
type EnvironmentRegistry struct {
	mu     sync.RWMutex
	envs   map[string]ContentEnvironment
	owners map[string]string
}

// NewEnvironmentRegistry constructs an EnvironmentRegistry pre-populated with
// the built-in "probe" environment (owned by the "probe" consumer).
func NewEnvironmentRegistry() *EnvironmentRegistry {
	probe := ProbeEnvironment()
	return &EnvironmentRegistry{
		envs: map[string]ContentEnvironment{
			probe.Name: probe,
		},
		owners: map[string]string{
			probe.Name: ProbeEnvironmentName,
		},
	}
}

// RegisterForConsumer validates and registers env owned by consumer.
// Cross-consumer compiles against env are refused by Dialect.Compile.
func (r *EnvironmentRegistry) RegisterForConsumer(consumer string, env ContentEnvironment) error {
	consumer = strings.TrimSpace(consumer)
	if consumer == "" {
		return &CompileError{
			Path:     "environment.consumer",
			Category: CategoryInput,
			Message:  "consumer name is required when registering a consumer content environment",
		}
	}
	return r.registerWithOwner(consumer, env)
}

func (r *EnvironmentRegistry) registerWithOwner(owner string, env ContentEnvironment) error {
	if err := env.Validate(); err != nil {
		return err
	}
	if len(env.ChoiceSets) > 0 {
		return &CompileError{
			Path:     "environment.choice_sets",
			Category: CategoryInput,
			Message:  "dynamic choice sets are not yet supported for consumer environments",
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.envs[env.Name]; exists {
		return &CompileError{
			Path:     "environment.name",
			Category: CategoryInput,
			Message:  fmt.Sprintf("content environment %q is already registered", env.Name),
		}
	}
	r.envs[env.Name] = env.Clone()
	if owner != "" {
		if r.owners == nil {
			r.owners = make(map[string]string)
		}
		r.owners[env.Name] = owner
	}
	return nil
}

// Owner returns the consumer name that owns the registered environment named
// name, or ("", false) if the environment has no recorded owner.
func (r *EnvironmentRegistry) Owner(name string) (string, bool) {
	if r == nil {
		return "", false
	}
	r.mu.RLock()
	owner, ok := r.owners[name]
	r.mu.RUnlock()
	if !ok || owner == "" {
		return "", false
	}
	return owner, true
}

// Get returns a defensive copy of the registered ContentEnvironment named name.
func (r *EnvironmentRegistry) Get(name string) (ContentEnvironment, bool) {
	if r == nil {
		return ContentEnvironment{}, false
	}
	r.mu.RLock()
	env, ok := r.envs[name]
	r.mu.RUnlock()
	if !ok {
		return ContentEnvironment{}, false
	}
	return env.Clone(), true
}

// Names returns the sorted names of all registered content environments.
func (r *EnvironmentRegistry) Names() []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]string, 0, len(r.envs))
	for name := range r.envs {
		out = append(out, name)
	}
	r.mu.RUnlock()
	slices.Sort(out)
	return out
}

// ProbeEnvironment returns the built-in synthetic probe ContentEnvironment
// (one text field "probe.text", max 16 KiB, untrusted).
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

// ValidateInputs checks that env is valid, inputs is non-empty, duplicate-free,
// and every referenced field is declared in env (AC-3 compile half).
func ValidateInputs(inputs []string, env ContentEnvironment) error {
	if err := env.Validate(); err != nil {
		return err
	}
	if len(inputs) == 0 {
		return &CompileError{
			Path:     "inputs",
			Category: CategoryInput,
			Message:  fmt.Sprintf("inputs must reference at least one field in content environment %q", env.Name),
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

const (
	// BytesPerTokenPrecheck is the conservative upper-bound bytes-per-token
	// factor for non-silent adapters (Truncation never / reported).
	BytesPerTokenPrecheck = 16
	// SilentBytesPerTokenPrecheck is the strict bytes-per-token lower bound
	// (1 byte per token) for adapters with silent truncation.
	SilentBytesPerTokenPrecheck = 1
	// TruncationSilent matches provider.TruncationSilent without importing provider.
	TruncationSilent = "silent"
)

// MaxDeclaredInputBytes returns the worst-case total byte size of inputs under
// env: the sum of each referenced field's MaxBytes, capped by env.MaxTotalBytes.
func MaxDeclaredInputBytes(inputs []string, env ContentEnvironment) int {
	sum := 0
	for _, name := range inputs {
		if f, ok := env.Field(name); ok && f.MaxBytes > 0 {
			sum += f.MaxBytes
		}
	}
	if env.MaxTotalBytes > 0 && sum > env.MaxTotalBytes {
		return env.MaxTotalBytes
	}
	return sum
}

// ValidateInputTokenBudget checks whether the declared worst-case byte size of
// inputs under env fits within caps.MaxInputTokens using the conservative
// bytes-to-tokens pre-check rules (runtime-design §§4.4, 4.5):
//   - Truncation == "silent": reject if MaxInputTokens <= 0 or bytes > tokens.
//   - Otherwise (never / reported / unset): reject if MaxInputTokens > 0 and bytes > tokens * 16.
func ValidateInputTokenBudget(inputs []string, env ContentEnvironment, caps *CapabilityLimits) error {
	if caps == nil {
		return nil
	}
	declaredBytes := MaxDeclaredInputBytes(inputs, env)
	if caps.Truncation == TruncationSilent {
		if caps.MaxInputTokens <= 0 || declaredBytes > caps.MaxInputTokens*SilentBytesPerTokenPrecheck {
			return &CompileError{
				Path:     "inputs",
				Category: CategoryLimit,
				Message:  fmt.Sprintf("declared input size %d bytes exceeds silent-truncation token limit (%d tokens)", declaredBytes, caps.MaxInputTokens),
			}
		}
		return nil
	}
	if caps.MaxInputTokens > 0 && declaredBytes > caps.MaxInputTokens*BytesPerTokenPrecheck {
		return &CompileError{
			Path:     "inputs",
			Category: CategoryLimit,
			Message:  fmt.Sprintf("declared input size %d bytes exceeds input token limit (%d tokens x %d bytes/token)", declaredBytes, caps.MaxInputTokens, BytesPerTokenPrecheck),
		}
	}
	return nil
}
