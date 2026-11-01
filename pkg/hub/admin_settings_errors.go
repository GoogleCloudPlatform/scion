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
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// The settings write APIs (server-config in every tier, the section reset,
// project defaults, federation, GCP IAM, github-app and integrations) answer
// every error with the writeError envelope,
//
//	{"error": {"code": "...", "message": "...", "details": {...}}}
//
// (ptone/scion#4450). A rejected value is 422 validation_error; its details
// carry "errors", one settingsFieldError per failure, each naming the koanf
// path of the field, and "field", the first entry's field. A stale revision
// is 409 revision_conflict with details.conflicted listing the expected and
// current revision of each conflicting section.

// settingsFieldError is one entry of a settings validation error.
type settingsFieldError struct {
	// Section is the settings section (opsettings registry name), when known.
	Section string `json:"section,omitempty"`
	// Field is the koanf path of the field (for example
	// server.hub.public_url). Never empty.
	Field string `json:"field"`
	// Path is the location inside the section document as the validator
	// reported it (a JSON pointer for schema errors), when it differs from
	// Field.
	Path string `json:"path,omitempty"`
	// Message describes the failure, without the field prefix.
	Message string `json:"message"`
}

// writeSettingsValidation writes a 422 validation_error for entries. The
// top-level message is message when set, otherwise the first entry's
// "<field>: <message>". extra adds members to details (for example keys).
// entries must not be empty.
func writeSettingsValidation(w http.ResponseWriter, message string, entries []settingsFieldError, extra map[string]interface{}) {
	if message == "" {
		message = entries[0].Message
		if entries[0].Field != "" {
			message = entries[0].Field + ": " + entries[0].Message
		}
	}
	details := map[string]interface{}{
		"field":  entries[0].Field,
		"errors": entries,
	}
	for k, v := range extra {
		details[k] = v
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, message, details)
}

// writeSettingsFieldError writes a 422 validation_error for one field.
// message is the full human-readable message.
func writeSettingsFieldError(w http.ResponseWriter, field, message string) {
	writeSettingsValidation(w, message, []settingsFieldError{{Field: field, Message: message}}, nil)
}

// writeSettingsKeysError writes a 422 validation_error with one entry per
// key (each a koanf path) and the keys list in details.keys.
func writeSettingsKeysError(w http.ResponseWriter, message string, keys []string) {
	writeSettingsCodedKeysError(w, ErrCodeValidationError, message, keys)
}

// writeSettingsCodedKeysError writes a 422 with code for a request whose
// keys cannot be saved (layer0_rejected, unclassified_keys_rejected, ...):
// details.keys lists them and details.errors has one entry per key.
func writeSettingsCodedKeysError(w http.ResponseWriter, code, message string, keys []string) {
	entries := make([]settingsFieldError, 0, len(keys))
	for _, k := range keys {
		entries = append(entries, settingsFieldError{Field: settingsKeyField(k), Message: message})
	}
	details := map[string]interface{}{"keys": keys, "errors": entries}
	if len(entries) > 0 {
		details["field"] = entries[0].Field
	}
	writeError(w, http.StatusUnprocessableEntity, code, message, details)
}

// settingsKeyField is the koanf path of a rejected request key: keys are
// koanf paths already, except the top-level request member "federation".
func settingsKeyField(key string) string {
	if key == "federation" {
		return "server.federation"
	}
	return key
}

// writeSettingsValidationErrors writes a 422 for config validation errors
// whose Path is already a koanf path (the config package validators).
func writeSettingsValidationErrors(w http.ResponseWriter, errs []config.ValidationError) {
	writeSettingsValidation(w, errs[0].Error(), configValidationEntries(errs, ""), nil)
}

// configValidationEntries converts config validation errors whose Path is
// a koanf path to entries; fallback is the field of an error with no path.
func configValidationEntries(errs []config.ValidationError, fallback string) []settingsFieldError {
	entries := make([]settingsFieldError, 0, len(errs))
	for _, e := range errs {
		entries = append(entries, settingsFieldError{Field: e.Path, Message: e.Message})
	}
	return fillEmptySettingsFields(entries, fallback)
}

// sectionValidationEntries converts the errors of opsettings.Validate for
// section (paths inside the section document) to entries with koanf
// fields.
func sectionValidationEntries(section string, errs []config.ValidationError) []settingsFieldError {
	out := make([]settingsFieldError, 0, len(errs))
	for _, e := range errs {
		field := settingsKoanfField(section, e.Path)
		entry := settingsFieldError{Section: section, Field: field, Message: e.Message}
		if e.Path != "" && e.Path != field {
			entry.Path = e.Path
		}
		out = append(out, entry)
	}
	return out
}

// sortedSectionValidationEntries flattens per-section validation errors,
// sections in name order.
func sortedSectionValidationEntries(bySection map[string][]config.ValidationError) []settingsFieldError {
	names := make([]string, 0, len(bySection))
	for name := range bySection {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []settingsFieldError
	for _, name := range names {
		out = append(out, sectionValidationEntries(name, bySection[name])...)
	}
	return out
}

// fillEmptySettingsFields sets fallback as the field of entries that have
// none; with no fallback the section, or "settings" as a last resort, is
// used so that every entry names a field.
func fillEmptySettingsFields(entries []settingsFieldError, fallback string) []settingsFieldError {
	for i := range entries {
		if entries[i].Field != "" {
			continue
		}
		switch {
		case fallback != "":
			entries[i].Field = fallback
		case entries[i].Section != "":
			entries[i].Field = settingsKoanfField(entries[i].Section, "")
		default:
			entries[i].Field = "settings"
		}
	}
	return entries
}

// settingsKoanfField maps a location inside a section document to its koanf
// path. pointer is a JSON pointer ("/a/b"), or a dotted path. An empty
// pointer names the section as a whole: its koanf prefix. A location the
// registry does not know falls back to "<section>.<dotted pointer>".
func settingsKoanfField(section, pointer string) string {
	segs := pointerSegments(pointer)
	sec := opsettings.SectionByName(section)
	if len(segs) == 0 {
		return sectionKoanfPrefix(section, sec)
	}
	// Sections with a JSON field -> koanf path mapping (access, federation,
	// ...): the first segment is a mapped section key.
	if kp := opsettings.KoanfPathFromSectionKey(section, segs[0]); kp != "" {
		return joinKoanf(kp, segs[1:])
	}
	dotted := strings.Join(segs, ".")
	if sec != nil {
		// Sections whose document keys are their koanf paths
		// (agent_defaults) or sit below one root (telemetry, runtimes,
		// profiles, ...).
		candidates := []string{dotted}
		if root := sectionKoanfRoot(sec); root != "" {
			candidates = append(candidates, root+"."+dotted)
		}
		for _, c := range candidates {
			for _, kp := range sec.KoanfPaths {
				if c == kp || strings.HasPrefix(c, kp+".") || strings.HasPrefix(kp, c+".") {
					return c
				}
			}
		}
	}
	return section + "." + dotted
}

// sectionKoanfPrefix is the koanf path of a section as a whole: its only
// koanf path, the longest dotted prefix all its paths share (when that has
// at least two segments), or the section name.
func sectionKoanfPrefix(section string, sec *opsettings.Section) string {
	if sec == nil || len(sec.KoanfPaths) == 0 {
		return section
	}
	if len(sec.KoanfPaths) == 1 {
		return sec.KoanfPaths[0]
	}
	common := strings.Split(sec.KoanfPaths[0], ".")
	for _, kp := range sec.KoanfPaths[1:] {
		segs := strings.Split(kp, ".")
		n := 0
		for n < len(common) && n < len(segs) && common[n] == segs[n] {
			n++
		}
		common = common[:n]
	}
	if len(common) >= 2 {
		return strings.Join(common, ".")
	}
	return section
}

// sectionKoanfRoot is the first koanf segment all the section's paths
// share, or "".
func sectionKoanfRoot(sec *opsettings.Section) string {
	root := ""
	for _, kp := range sec.KoanfPaths {
		first, _, _ := strings.Cut(kp, ".")
		if root == "" {
			root = first
		} else if root != first {
			return ""
		}
	}
	return root
}

// pointerSegments splits a JSON pointer or dotted path, decoding pointer
// escapes.
func pointerSegments(p string) []string {
	var out []string
	for _, s := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '.' }) {
		s = strings.ReplaceAll(s, "~1", "/")
		s = strings.ReplaceAll(s, "~0", "~")
		out = append(out, s)
	}
	return out
}

func joinKoanf(base string, rest []string) string {
	if len(rest) == 0 {
		return base
	}
	return base + "." + strings.Join(rest, ".")
}

// writeSettingsRevisionConflict writes a 409 revision_conflict. conflicted
// lists each conflicting section with its expected and current revision;
// extra adds members to details (the save report of a partial apply).
func writeSettingsRevisionConflict(w http.ResponseWriter, message string, conflicted []map[string]interface{}, extra map[string]interface{}) {
	details := map[string]interface{}{"conflicted": conflicted}
	for k, v := range extra {
		details[k] = v
	}
	writeError(w, http.StatusConflict, ErrCodeRevisionConflict, message, details)
}

// settingsConflictEntry is one details.conflicted entry.
func settingsConflictEntry(section string, expected, current int64) map[string]interface{} {
	return map[string]interface{}{
		"section":           section,
		"expected_revision": expected,
		"current_revision":  current,
	}
}

// federationSaveErrors checks a merged federation section for a save: the
// duration strings (conversion silently falls back to zero for an invalid
// value, which would fail at time.ParseDuration during ApplySnapshot; a
// value sent in the body was checked by validateDurationSettings, so this
// catches one carried over from the stored row by the merge) and the
// FederationConfig rules. Every failure is returned.
func federationSaveErrors(fed opsettings.FederationSettings) []settingsFieldError {
	var out []settingsFieldError
	for _, d := range []struct {
		key   string
		value *string
	}{
		{"refresh_interval", &fed.RefreshInterval},
		{"debounce_interval", &fed.DebounceInterval},
	} {
		if *d.value == "" {
			continue
		}
		if _, err := time.ParseDuration(*d.value); err != nil {
			out = append(out, settingsFieldError{
				Section: "federation",
				Field:   "server.federation." + d.key,
				Message: fmt.Sprintf("invalid %s %q: %v", d.key, *d.value, err),
			})
			// Already reported; the conversion below would only log it.
			*d.value = ""
		}
	}
	fedCfg := convertFederationSettingsToConfig(fed)
	for _, e := range fedCfg.ValidateFields() {
		out = append(out, settingsFieldError{
			Section: "federation",
			Field:   "server.federation." + e.Path,
			Message: e.Message,
		})
	}
	return out
}

// writeFederationRefused writes a 422 validation_error refusing the
// federation member as a whole: details.keys is ["federation"], the request
// key, and the entry's field its koanf path.
func writeFederationRefused(w http.ResponseWriter, message string) {
	writeSettingsValidation(w, message,
		[]settingsFieldError{{Section: "federation", Field: "server.federation", Message: message}},
		map[string]interface{}{"keys": []string{"federation"}})
}

// writeServerConfigFileError writes the response for an error of a
// server-config check that returns a *serverConfigFileValidationError for
// a rejected value (422) and any other error for an unreadable request
// (400).
func writeServerConfigFileError(w http.ResponseWriter, err error) {
	var valErr *serverConfigFileValidationError
	if errors.As(err, &valErr) && len(valErr.entries) > 0 {
		writeSettingsValidation(w, valErr.Error(), valErr.entries, nil)
		return
	}
	writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
}
