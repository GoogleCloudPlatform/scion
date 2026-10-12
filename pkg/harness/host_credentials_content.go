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

package harness

import (
	"encoding/json"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// hostCredentialContentChecks holds, for shipped required files whose path
// can also hold a file that is not a credential, a check that the content
// read from the host holds sign-in data. Files without an entry need no
// content check: their existence is enough.
var hostCredentialContentChecks = map[shippedRequiredFile]func([]byte) bool{
	// copilot's ~/.copilot/config.json is both its settings file and its
	// file-based sign-in. A user who signs in through the OS keychain still
	// has one, holding only settings.
	{"COPILOT_CONFIG", "CopilotConfigFile", ".copilot/config.json"}: copilotConfigHoldsSignIn,
}

// hostCredentialHoldsSignIn reports whether data, read from the host for the
// required file f, holds sign-in data. It is true for every file without a
// content check.
func hostCredentialHoldsSignIn(f HostCredentialFile, data []byte) bool {
	rel, ok := homeRelativeSuffix(f.TargetSuffix)
	if !ok {
		return false
	}
	check, ok := hostCredentialContentChecks[shippedRequiredFile{f.Name, f.Field, rel}]
	if !ok {
		return true
	}
	return check(data)
}

// copilotSettingsKeys are copilot config.json top-level keys, lowercased,
// that hold settings or identity but never a credential. Sources: the
// copilot CLI's config schema as quoted from its bundled code in
// github/copilot-cli issue #207 (banner, beep, last_logged_in_user,
// logged_in_users, model, render_markdown, screen_reader, theme,
// trusted_folders, copilot_integration_id, copilot_url); and scion's own
// copilot provisioner (harnesses/copilot/provision.py), which writes
// trustedFolders.
// The copilot provisioner applies the same rule inside the agent container
// (_COPILOT_SETTINGS_KEYS and _config_holds_sign_in in
// harnesses/copilot/provision.py). Keep the two in sync: its tests compare
// these key lists with its own and run its check over the fixtures in
// testdata/copilot_config_signin.json.
// last_logged_in_user and logged_in_users name the signed-in user (host and
// login) while the token itself is in the OS keychain.
var copilotSettingsKeys = map[string]struct{}{
	"banner":                 {},
	"beep":                   {},
	"copilot_integration_id": {},
	"copilot_url":            {},
	"last_logged_in_user":    {},
	"logged_in_users":        {},
	"model":                  {},
	"render_markdown":        {},
	"screen_reader":          {},
	"theme":                  {},
	"trusted_folders":        {},
	"trustedfolders":         {},
}

// copilotConfigHoldsSignIn reports whether the content of a copilot
// config.json may hold sign-in data. Lines that start with "//" are dropped
// and the rest is parsed as JSON. Top-level keys are compared lowercased (the
// copilot CLI lowercases them when it loads the file).
//   - It holds sign-in if copilot_tokens has a string value that is not empty
//     or whitespace-only (where the CLI keeps tokens when no keychain is
//     available), or copilot_hmac_key or capi_hmac_key is a non-empty string.
//     Each check matches what the CLI itself accepts: it discards a config
//     token that is blank after trimming, but reads the HMAC keys untrimmed.
//   - It does not if every other key is a known settings key
//     (copilotSettingsKeys); an empty copilot_tokens, copilot_hmac_key or
//     capi_hmac_key also counts as no sign-in. This is the file of a user
//     who signs in through the OS keychain.
//   - A key in neither list counts as possible sign-in (fail open): a future
//     copilot version may keep a credential under a new key, and such a file
//     is handled as it was before this check.
//   - Content that does not parse as a JSON object is not sign-in (fail
//     closed): copilot cannot read sign-in from it either, so copying it
//     into the agent cannot help.
func copilotConfigHoldsSignIn(data []byte) bool {
	lines := strings.Split(string(data), "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if !strings.HasPrefix(strings.TrimSpace(ln), "//") {
			kept = append(kept, ln)
		}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(strings.Join(kept, "\n")), &top); err != nil || top == nil {
		util.Debugf("host credentials: copilot config.json is not a JSON object; not treating it as sign-in")
		return false
	}
	unknown := false
	for key, raw := range top {
		switch k := strings.ToLower(key); k {
		case "copilot_tokens":
			var tokens map[string]any
			_ = json.Unmarshal(raw, &tokens)
			for _, v := range tokens {
				// Trimmed on purpose: the copilot CLI ignores a config token
				// that is blank after trimming (github/copilot-cli issue
				// #207), so a whitespace-only value gives it no sign-in.
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
			}
		case "copilot_hmac_key", "capi_hmac_key":
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return true
			}
		default:
			if _, ok := copilotSettingsKeys[k]; !ok {
				unknown = true
			}
		}
	}
	return unknown
}
