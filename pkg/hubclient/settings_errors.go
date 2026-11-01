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

package hubclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// maxSettingsErrorBodyBytes bounds how much of a settings error body is read.
const maxSettingsErrorBodyBytes = 1 << 20

// SettingsFieldError is one field-level refusal of a settings write.
type SettingsFieldError struct {
	// Section is the hub settings section the field belongs to, when known.
	Section string `json:"section,omitempty"`
	// Field is the dotted settings key, when known.
	Field string `json:"field,omitempty"`
	// Path is the location inside the section document, when known and
	// different from Field.
	Path string `json:"path,omitempty"`
	// Message describes the refusal.
	Message string `json:"message"`
}

// Location returns the most specific name for where the error applies:
// the field, else the section and path, else the section.
func (e SettingsFieldError) Location() string {
	switch {
	case e.Field != "":
		return e.Field
	case e.Section != "" && e.Path != "":
		return e.Section + ":" + strings.TrimPrefix(e.Path, "/")
	case e.Path != "":
		return strings.TrimPrefix(e.Path, "/")
	default:
		return e.Section
	}
}

// SettingsRevisionConflict reports a section whose stored revision differs
// from the revision the write expected.
type SettingsRevisionConflict struct {
	Section          string `json:"section"`
	ExpectedRevision int64  `json:"expected_revision"`
	CurrentRevision  int64  `json:"current_revision"`
}

// SettingsSkippedKey is a changed key that was saved but is not in use.
type SettingsSkippedKey struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// SettingsError is a refused settings write, decoded from the hub's
// settings error envelope. It accepts both the structured form
// ({"error": {"code", "message", "details": {...}}}) and the earlier flat
// form ({"error": "<code>", "errors": ..., "conflicted": ...}), so the CLI
// reports field errors and revision conflicts from older and newer hubs
// alike. It unwraps to an *apiclient.APIError, so the apiclient status
// helpers (IsConflictError, IsForbiddenError, ...) keep working.
type SettingsError struct {
	API *apiclient.APIError

	// Errors lists the field-level refusals, in the order the hub sent
	// them (section order for the flat form, which groups by section).
	Errors []SettingsFieldError
	// Keys lists the keys a key-class refusal names.
	Keys []string

	// Conflicts, Applied and the rest describe a revision conflict: the
	// sections that conflicted, the sections already written (name to new
	// revision), the current revisions of the sections left unchanged, and
	// the key report of the sections written before the conflict.
	Conflicts          []SettingsRevisionConflict
	Applied            map[string]int64
	UnchangedRevisions map[string]int64
	AppliedKeys        []string
	PendingRestart     []string
	Skipped            []SettingsSkippedKey
}

// Error implements error.
func (e *SettingsError) Error() string {
	return e.API.Error()
}

// Unwrap returns the underlying API error.
func (e *SettingsError) Unwrap() error {
	return e.API
}

// StatusCode returns the HTTP status of the refusal.
func (e *SettingsError) StatusCode() int {
	return e.API.StatusCode
}

// settingsRevisionConflictCode is the error code of a compare-and-set
// refusal.
const settingsRevisionConflictCode = "revision_conflict"

// IsRevisionConflict reports whether the refusal is a revision conflict: a
// 409 with the revision_conflict code or a list of conflicted sections.
// Other 409 refusals (a refused GCP permission-check change, a settings file
// that needs migrating) are not, so re-reading and retrying does not help
// them; apiclient.IsConflictError matches any 409.
func (e *SettingsError) IsRevisionConflict() bool {
	if e.API.StatusCode != http.StatusConflict {
		return false
	}
	return e.API.Code == settingsRevisionConflictCode || len(e.Conflicts) > 0
}

// decodeSettingsResponse decodes a settings endpoint response: a 2xx body
// into T, any other status into a *SettingsError.
func decodeSettingsResponse[T any](resp *http.Response) (*T, error) {
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		defer func() { _ = resp.Body.Close() }()
		return nil, parseSettingsError(resp)
	}
	return apiclient.DecodeRequired[T](resp)
}

// parseSettingsError reads a non-2xx settings response into a
// *SettingsError. A body in neither envelope form keeps the status, a
// status-derived code and, when short, the body text as the message.
func parseSettingsError(resp *http.Response) *SettingsError {
	api := &apiclient.APIError{
		StatusCode: resp.StatusCode,
		Code:       settingsStatusCode(resp.StatusCode),
		Message:    http.StatusText(resp.StatusCode),
	}
	if resp.Header != nil {
		api.RequestID = resp.Header.Get("X-Request-ID")
	}
	se := &SettingsError{API: api}
	if resp.Body == nil {
		return se
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSettingsErrorBodyBytes))
	if err != nil || len(body) == 0 {
		return se
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		if len(body) < 500 {
			api.Message = strings.TrimSpace(string(body))
		}
		return se
	}

	// details holds the members describing the refusal: error.details in
	// the structured form, the top-level object in the flat form.
	details := top
	if rawErr, ok := top["error"]; ok {
		var structured struct {
			Code      string                     `json:"code"`
			Message   string                     `json:"message"`
			Details   map[string]json.RawMessage `json:"details"`
			RequestID string                     `json:"requestId"`
		}
		var flatCode string
		switch {
		case json.Unmarshal(rawErr, &structured) == nil:
			if structured.Code != "" {
				api.Code = structured.Code
			}
			if structured.Message != "" {
				api.Message = structured.Message
			}
			if structured.RequestID != "" {
				api.RequestID = structured.RequestID
			}
			details = structured.Details
		case json.Unmarshal(rawErr, &flatCode) == nil:
			if flatCode != "" {
				api.Code = flatCode
			}
			var msg string
			if json.Unmarshal(top["message"], &msg) == nil && msg != "" {
				api.Message = msg
			}
		}
	}
	if details == nil {
		return se
	}

	api.Details = make(map[string]interface{}, len(details))
	for k, v := range details {
		var anyVal interface{}
		if json.Unmarshal(v, &anyVal) == nil {
			api.Details[k] = anyVal
		}
	}

	se.Errors = parseSettingsFieldErrors(details["errors"])
	if len(se.Errors) == 0 {
		// A lone field-level refusal: details.field names the key and the
		// message describes it.
		var field string
		if json.Unmarshal(details["field"], &field) == nil && field != "" {
			se.Errors = []SettingsFieldError{{Field: field, Message: api.Message}}
		}
	}
	_ = json.Unmarshal(details["keys"], &se.Keys)
	_ = json.Unmarshal(details["conflicted"], &se.Conflicts)
	_ = json.Unmarshal(details["applied"], &se.Applied)
	_ = json.Unmarshal(details["unchanged_revisions"], &se.UnchangedRevisions)
	_ = json.Unmarshal(details["applied_keys"], &se.AppliedKeys)
	_ = json.Unmarshal(details["pending_restart"], &se.PendingRestart)
	_ = json.Unmarshal(details["skipped"], &se.Skipped)
	return se
}

// parseSettingsFieldErrors decodes the errors member in either form: a list
// of entries, or (the flat form) an object of section name to a list of
// entries. Entry members are matched case-insensitively, since the flat
// form's entries carry the untagged Path and Message names.
func parseSettingsFieldErrors(raw json.RawMessage) []SettingsFieldError {
	if len(raw) == 0 {
		return nil
	}
	var list []map[string]interface{}
	if json.Unmarshal(raw, &list) == nil {
		out := make([]SettingsFieldError, 0, len(list))
		for _, m := range list {
			out = append(out, settingsFieldErrorFromMap("", m))
		}
		return out
	}
	var bySection map[string][]map[string]interface{}
	if json.Unmarshal(raw, &bySection) == nil {
		sections := make([]string, 0, len(bySection))
		for s := range bySection {
			sections = append(sections, s)
		}
		sort.Strings(sections)
		var out []SettingsFieldError
		for _, s := range sections {
			for _, m := range bySection[s] {
				out = append(out, settingsFieldErrorFromMap(s, m))
			}
		}
		return out
	}
	return nil
}

func settingsFieldErrorFromMap(section string, m map[string]interface{}) SettingsFieldError {
	get := func(name string) string {
		for k, v := range m {
			if strings.EqualFold(k, name) {
				if s, ok := v.(string); ok {
					return s
				}
				if v != nil {
					return fmt.Sprint(v)
				}
			}
		}
		return ""
	}
	e := SettingsFieldError{
		Section: get("section"),
		Field:   get("field"),
		Path:    get("path"),
		Message: get("message"),
	}
	if e.Section == "" {
		e.Section = section
	}
	return e
}

// settingsStatusCode maps a status to the apiclient error code used when
// the body names none.
func settingsStatusCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return apiclient.ErrCodeInvalidRequest
	case http.StatusUnauthorized:
		return apiclient.ErrCodeUnauthorized
	case http.StatusForbidden:
		return apiclient.ErrCodeForbidden
	case http.StatusNotFound:
		return apiclient.ErrCodeNotFound
	case http.StatusConflict:
		return apiclient.ErrCodeConflict
	case http.StatusUnprocessableEntity:
		return apiclient.ErrCodeValidationError
	default:
		if status >= 500 {
			return apiclient.ErrCodeInternalError
		}
		return apiclient.ErrCodeInvalidRequest
	}
}
