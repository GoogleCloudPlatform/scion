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

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"gopkg.in/yaml.v3"
)

// Output helpers shared by "scion hub settings", "scion hub
// project-defaults" and "scion hub experiments". Each command writes
// text by default, or JSON / YAML with the global --format flag.

// settingsOutputFormat returns "json", "yaml" or "" (text).
func settingsOutputFormat() (string, error) {
	switch outputFormat {
	case "", "text":
		return "", nil
	case "json", "yaml":
		return outputFormat, nil
	default:
		return "", fmt.Errorf("unsupported --format %q (want text, json or yaml)", outputFormat)
	}
}

// writeStructured writes v as indented JSON or as YAML.
func writeStructured(w io.Writer, format string, v interface{}) error {
	if format == "yaml" {
		return writeYAMLValue(w, v)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeYAMLValue writes v as YAML, going through JSON so that json tags
// and raw JSON values decide the field names.
func writeYAMLValue(w io.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var generic interface{}
	if err := json.Unmarshal(b, &generic); err != nil {
		return err
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(generic); err != nil {
		return err
	}
	return enc.Close()
}

// formatSettingsValue renders a JSON value for text output: a string or
// other scalar on its own, anything else as YAML.
func formatSettingsValue(raw json.RawMessage) (string, error) {
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch t := v.(type) {
	case nil:
		return "null", nil
	case string:
		return t, nil
	case bool, float64:
		return strings.TrimSpace(string(raw)), nil
	}
	var sb strings.Builder
	if err := writeYAMLValue(&sb, v); err != nil {
		return "", err
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// settingsScopeHint names what a refused call needs.
type settingsScopeHint struct {
	// Scope is the access-token scope (on a hub-boundary token) that admits
	// the call.
	Scope string
	// Retry is the guidance printed for a revision conflict; empty means
	// the server configuration's (re-run, or pin with --revision
	// SECTION=N).
	Retry string
}

// srvcfgRetryHint is the revision-conflict guidance of the server
// configuration commands.
const srvcfgRetryHint = "The settings changed since they were read. Re-run the command to apply the change to the current values, or pin a revision with --revision SECTION=N."

// srvcfgResetRetryHint is the revision-conflict guidance of a section
// reset, which takes no revision: the Hub refuses a reset of runtimes or
// profiles when the other section changed meanwhile.
const srvcfgResetRetryHint = "The section changed while it was reset. Re-run the command."

// reportSettingsError writes a settings refusal to w in full (field
// errors, keys, revision conflicts and what was written before a
// conflict) and returns the error for the command to exit with. With a
// structured --format it writes the refusal as one object to out instead.
func reportSettingsError(out, w io.Writer, format string, err error, hint settingsScopeHint) error {
	var se *hubclient.SettingsError
	if !errors.As(err, &se) {
		var api *apiclient.APIError
		if errors.As(err, &api) {
			se = &hubclient.SettingsError{API: api}
		} else {
			return err
		}
	}
	if format != "" {
		_ = writeStructured(out, format, settingsErrorView(se))
		return fmt.Errorf("%s", se.API.Message)
	}

	for _, fe := range se.Errors {
		loc := fe.Location()
		if fe.Section != "" && fe.Field != "" {
			loc = fe.Field + " (" + fe.Section + ")"
		}
		if loc == "" {
			_, _ = fmt.Fprintf(w, "  %s\n", fe.Message)
		} else {
			_, _ = fmt.Fprintf(w, "  %s: %s\n", loc, fe.Message)
		}
	}
	if len(se.Keys) > 0 && len(se.Errors) == 0 {
		_, _ = fmt.Fprintf(w, "  keys: %s\n", strings.Join(se.Keys, ", "))
	}
	if se.IsRevisionConflict() {
		for _, c := range se.Conflicts {
			_, _ = fmt.Fprintf(w, "  section %s: expected revision %d, current revision %d\n", c.Section, c.ExpectedRevision, c.CurrentRevision)
		}
		if len(se.Applied) > 0 {
			_, _ = fmt.Fprintf(w, "  already saved before the conflict: %s\n", formatRevisionMap(se.Applied))
		}
		if len(se.PendingRestart) > 0 {
			_, _ = fmt.Fprintf(w, "  saved keys pending a hub restart: %s\n", strings.Join(se.PendingRestart, ", "))
		}
		retry := hint.Retry
		if retry == "" {
			retry = srvcfgRetryHint
		}
		_, _ = fmt.Fprintln(w, "  "+retry)
	}
	switch se.API.StatusCode {
	case http.StatusForbidden:
		if hint.Scope != "" {
			_, _ = fmt.Fprintf(w, "  This needs a hub admin login, or an access token with the %s scope on a hub boundary.\n", hint.Scope)
		}
	case http.StatusUnauthorized:
		_, _ = fmt.Fprintln(w, "  Log in with 'scion hub auth login', or set an access token.")
	}

	msg := se.API.Message
	if msg == "" {
		msg = http.StatusText(se.API.StatusCode)
	}
	return fmt.Errorf("%s (%s, HTTP %d)", msg, se.API.Code, se.API.StatusCode)
}

// settingsErrorView is the structured form of a refusal.
func settingsErrorView(se *hubclient.SettingsError) map[string]interface{} {
	v := map[string]interface{}{
		"status":  se.API.StatusCode,
		"code":    se.API.Code,
		"message": se.API.Message,
	}
	if len(se.Errors) > 0 {
		v["errors"] = se.Errors
	}
	if len(se.Keys) > 0 {
		v["keys"] = se.Keys
	}
	if len(se.Conflicts) > 0 {
		v["conflicted"] = se.Conflicts
	}
	if len(se.Applied) > 0 {
		v["applied"] = se.Applied
	}
	if len(se.UnchangedRevisions) > 0 {
		v["unchanged_revisions"] = se.UnchangedRevisions
	}
	if len(se.AppliedKeys) > 0 {
		v["applied_keys"] = se.AppliedKeys
	}
	if len(se.PendingRestart) > 0 {
		v["pending_restart"] = se.PendingRestart
	}
	if len(se.Skipped) > 0 {
		v["skipped"] = se.Skipped
	}
	if se.API.RequestID != "" {
		v["request_id"] = se.API.RequestID
	}
	return map[string]interface{}{"error": v}
}

func formatRevisionMap(m map[string]int64) string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s (revision %d)", n, m[n]))
	}
	return strings.Join(parts, ", ")
}
