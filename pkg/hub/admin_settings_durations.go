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
// lists (opsettings.Section.Durations) in the given section documents. A
// key that is absent, null or "" is skipped ("" and null clear the key);
// a string that does not parse as a duration fails. A non-string value is
// left to the section schema. Sections are checked in registry order, so
// the first failure reported is stable.
func validateDurationSettings(sectionDocs map[string]json.RawMessage) *durationFieldError {
	for _, sec := range opsettings.Registry {
		doc, ok := sectionDocs[sec.Name]
		if !ok {
			continue
		}
		keys := opsettings.DurationKeys(sec.Name)
		if len(keys) == 0 {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(doc, &fields); err != nil {
			continue // the section schema reports a malformed document
		}
		for _, k := range keys {
			raw, ok := fields[k.SectionKey]
			if !ok {
				continue
			}
			var v string
			if err := json.Unmarshal(raw, &v); err != nil || v == "" {
				continue
			}
			if _, err := time.ParseDuration(v); err != nil {
				return &durationFieldError{
					field: k.KoanfPath,
					msg:   fmt.Sprintf("invalid %s %q: %v", k.SectionKey, v, err),
				}
			}
		}
	}
	return nil
}
