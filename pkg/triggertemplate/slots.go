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

package triggertemplate

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/gitref"
	"github.com/GoogleCloudPlatform/scion/pkg/labels"
)

// Built-in Stage 2 slot names and size bounds (§5.2).
const (
	SlotTask    = "task"
	SlotMessage = "message"
	SlotName    = "name"
	SlotBranch  = "branch"
	SlotLabel   = "label"

	MaxTaskBytes    = 16384
	MaxMessageBytes = 16384
	MaxNameBytes    = 63
	MaxBranchBytes  = 100
	MaxLabelBytes   = 63

	maxNameUntrustedValueLen   = 32
	maxBranchUntrustedValueLen = 64
)

// Phase indicates when a slot is rendered relative to action execution.
type Phase int

const (
	// PhasePre slots render before Fire and forbid result.* placeholders.
	PhasePre Phase = iota
	// PhasePost slots render after Fire and admit result.* placeholders.
	PhasePost
)

func (p Phase) String() string {
	switch p {
	case PhasePre:
		return "pre"
	case PhasePost:
		return "post"
	default:
		return fmt.Sprintf("Phase(%d)", int(p))
	}
}

// Slot defines bounds, per-value untrusted validation/transforms, whole-string
// acceptance rules, and framing behavior for one template target slot (§5.2).
// All callback errors must be content-free (never echoing the input value).
type Slot struct {
	Name      string
	Phase     Phase
	MaxBytes  int
	Accept    func(rendered string) error
	Untrusted func(value string) (string, error)
	Frame     bool
}

var (
	slotMu       sync.RWMutex
	slotRegistry = make(map[string]Slot)
)

// Register adds s to the global slot registry. Duplicate names, empty names,
// unknown phases, and non-positive MaxBytes return an error. Safe for
// concurrent use (for example from package init functions in Stage 3).
func Register(s Slot) error {
	name := strings.TrimSpace(s.Name)
	if name == "" || name != s.Name {
		return errors.New("triggertemplate: slot name must be non-empty without surrounding whitespace")
	}
	if s.Phase != PhasePre && s.Phase != PhasePost {
		return fmt.Errorf("triggertemplate: slot %q has invalid phase %d", name, s.Phase)
	}
	if s.MaxBytes <= 0 {
		return fmt.Errorf("triggertemplate: slot %q must declare positive MaxBytes", name)
	}

	slotMu.Lock()
	defer slotMu.Unlock()
	if _, exists := slotRegistry[name]; exists {
		return fmt.Errorf("%w: %q", ErrSlotExists, name)
	}
	slotRegistry[name] = s
	return nil
}

// LookupSlot returns the registered Slot for name, if present.
func LookupSlot(name string) (Slot, bool) {
	slotMu.RLock()
	defer slotMu.RUnlock()
	s, ok := slotRegistry[name]
	return s, ok
}

func init() {
	builtins := []Slot{
		{
			Name:     SlotTask,
			Phase:    PhasePre,
			MaxBytes: MaxTaskBytes,
			Accept:   acceptUTF8NoNUL,
			Frame:    true,
		},
		{
			Name:     SlotMessage,
			Phase:    PhasePre,
			MaxBytes: MaxMessageBytes,
			Accept:   acceptUTF8NoNUL,
			Frame:    false,
		},
		{
			Name:      SlotName,
			Phase:     PhasePre,
			MaxBytes:  MaxNameBytes,
			Untrusted: untrustedNameValue,
			Accept:    acceptCanonicalAgentName,
			Frame:     false,
		},
		{
			Name:      SlotBranch,
			Phase:     PhasePre,
			MaxBytes:  MaxBranchBytes,
			Untrusted: untrustedBranchValue,
			Accept:    acceptBranchName,
			Frame:     false,
		},
		{
			Name:      SlotLabel,
			Phase:     PhasePre,
			MaxBytes:  MaxLabelBytes,
			Untrusted: untrustedLabelValue,
			Accept:    acceptLabelValue,
			Frame:     false,
		},
	}
	for _, s := range builtins {
		if err := Register(s); err != nil {
			panic(err)
		}
	}
}

func acceptUTF8NoNUL(rendered string) error {
	if !utf8.ValidString(rendered) {
		return errors.New("rendered value is not valid UTF-8")
	}
	if strings.IndexByte(rendered, 0) >= 0 {
		return errors.New("rendered value must not contain NUL bytes")
	}
	return nil
}

// untrustedNameValue enforces ^[a-z0-9-]{1,32}$ without echoing value.
func untrustedNameValue(value string) (string, error) {
	if len(value) < 1 || len(value) > maxNameUntrustedValueLen {
		return "", errors.New("name value must be 1..32 bytes")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return "", errors.New("name value must match ^[a-z0-9-]{1,32}$")
		}
	}
	return value, nil
}

// acceptCanonicalAgentName requires rendered to pass api.ValidateAgentName and
// equal its own slug (no silent case folding, separator normalization, or
// truncation). Never echoes rendered.
func acceptCanonicalAgentName(rendered string) error {
	slug, err := api.ValidateAgentName(rendered)
	if err != nil {
		return errors.New("agent name is invalid")
	}
	if slug != rendered {
		return errors.New("agent name must already be in canonical slug form")
	}
	return nil
}

// untrustedBranchValue enforces ^[A-Za-z0-9._/-]{1,64}$, no "..", and no
// leading '-' or '.' without echoing value.
func untrustedBranchValue(value string) (string, error) {
	if len(value) < 1 || len(value) > maxBranchUntrustedValueLen {
		return "", errors.New("branch value must be 1..64 bytes")
	}
	if value[0] == '-' || value[0] == '.' {
		return "", errors.New("branch value must not start with '-' or '.'")
	}
	if strings.Contains(value, "..") {
		return "", errors.New("branch value must not contain '..'")
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		isLower := c >= 'a' && c <= 'z'
		isUpper := c >= 'A' && c <= 'Z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isUpper && !isDigit && c != '.' && c != '_' && c != '/' && c != '-' {
			return "", errors.New("branch value must match ^[A-Za-z0-9._/-]{1,64}$")
		}
	}
	return value, nil
}

func acceptBranchName(rendered string) error {
	// gitref.ValidBranch is pure Go and guaranteed never to quote the branch name.
	return gitref.ValidBranch(rendered)
}

// untrustedLabelValue enforces printable ASCII 0x20..0x7E and <= 63 bytes.
func untrustedLabelValue(value string) (string, error) {
	if err := validateLabelASCII(value); err != nil {
		return "", err
	}
	return value, nil
}

func acceptLabelValue(rendered string) error {
	if err := validateLabelASCII(rendered); err != nil {
		return err
	}
	if err := labels.Validate(map[string]string{"k": rendered}); err != nil {
		return errors.New("label value failed labels.Validate")
	}
	return nil
}

func validateLabelASCII(s string) error {
	if len(s) > labels.MaxValueLen {
		return errors.New("label value exceeds 63 bytes")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7E {
			return errors.New("label value must contain only printable ASCII (0x20-0x7E)")
		}
	}
	return nil
}
