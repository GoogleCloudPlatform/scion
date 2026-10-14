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

package hub

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// durationFieldError is a duration-valued setting whose value does not
// parse as a Go duration. field is the key's koanf path.
type durationFieldError struct {
	field string
	msg   string
}

func (e *durationFieldError) Error() string { return e.msg }

// validateDurationSettings checks every duration-valued key the registry
// lists (opsettings.Section.Durations and NestedDurations) in the given
// section documents. A key that is absent, null or "" is skipped ("" and
// null clear the key); a string that does not parse as a duration fails.
// A non-string value is left to the section schema. Sections are checked
// in registry order and map entries in key order, so the first failure
// reported is stable.
func validateDurationSettings(sectionDocs map[string]json.RawMessage) *durationFieldError {
	for _, sec := range opsettings.Registry {
		doc, ok := sectionDocs[sec.Name]
		if !ok {
			continue
		}
		keys := opsettings.DurationKeys(sec.Name)
		if len(keys) == 0 && len(sec.NestedDurations) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(doc, &fields); err != nil {
			continue // the section schema reports a malformed document
		}
		for _, k := range keys {
			if fe := checkDurationValue(fields[k.SectionKey], k.KoanfPath, k.SectionKey); fe != nil {
				return fe
			}
		}
		for _, pattern := range sec.NestedDurations {
			segs := strings.Split(strings.TrimPrefix(pattern, sec.Name+"."), ".")
			if fe := walkNestedDuration(fields, segs, sec.Name); fe != nil {
				return fe
			}
		}
	}
	return nil
}

// walkNestedDuration follows segs (a NestedDurations pattern below the
// section name) through the object fields, whose koanf path is prefix. A
// "*" segment visits every key of the map at that level. A missing key or
// a value that is not an object ends the walk without an error.
func walkNestedDuration(fields map[string]json.RawMessage, segs []string, prefix string) *durationFieldError {
	names := []string{segs[0]}
	if segs[0] == "*" {
		names = make([]string, 0, len(fields))
		for name := range fields {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	for _, name := range names {
		path := prefix + "." + name
		if len(segs) == 1 {
			if fe := checkDurationValue(fields[name], path, path); fe != nil {
				return fe
			}
			continue
		}
		var next map[string]json.RawMessage
		if err := json.Unmarshal(fields[name], &next); err != nil || next == nil {
			continue
		}
		if fe := walkNestedDuration(next, segs[1:], path); fe != nil {
			return fe
		}
	}
	return nil
}

// checkDurationValue reports raw, the value of the duration-valued key at
// koanf path field, if it is a non-empty string that does not parse as a
// duration. The message names the key as name.
func checkDurationValue(raw json.RawMessage, field, name string) *durationFieldError {
	if raw == nil {
		return nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil || v == "" {
		return nil
	}
	if _, err := time.ParseDuration(v); err != nil {
		return &durationFieldError{
			field: field,
			msg:   fmt.Sprintf("invalid %s %q: %v", name, v, err),
		}
	}
	return nil
}
