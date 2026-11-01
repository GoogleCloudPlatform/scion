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
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"gopkg.in/yaml.v3"
)

// This file turns "scion hub settings set/unset" arguments into a server
// configuration write: the request body and the expected revision of each
// section it touches. It has no I/O, so every rule is unit-tested.
//
// The unit of a write is a settings key as the hub's schema lists it (its
// koanf path, for example server.hub.public_url, default_max_turns or
// runtimes):
//   - A change AT a schema key is sent on its own: the hub merges it onto
//     the stored section, and an explicit null clears it.
//   - A change BELOW a schema key (runtimes.docker.image,
//     telemetry.resource.team) is applied to the current value of that key
//     from GET and the whole value is sent. The hub replaces map-valued
//     keys wholesale (an entry the body leaves out of profiles or runtimes
//     is removed), so sending only the changed entry would drop the others.

// projectDefaultsSection is the settings section with its own command.
const projectDefaultsSection = "project_defaults"

// settingsUnsetRefused lists schema keys that "unset" refuses, because the
// hub does not clear them on an explicit null: the gcp_iam keys need a
// recognised value, the agent role defaults are read-only through the
// server configuration, and a null github_app object is not a clear (each
// of its keys can be unset on its own).
var settingsUnsetRefused = map[string]string{
	"server.hub.gcp_iam_check_mode":          "it needs a recognised value; set one instead",
	"server.hub.gcp_iam_deny_unknown_policy": "it needs a recognised value; set one instead",
	"default_max_agent_role":                 "it is read-only through the server configuration",
	"default_agent_role":                     "it is read-only through the server configuration",
	"server.github_app":                      "a null object is not a clear; unset its keys one by one",
}

// settingsUnsetPairs lists keys that are only valid together, so clearing
// one needs clearing the other in the same command.
var settingsUnsetPairs = map[string]string{
	"telemetry.cloud.tls.cert_file": "telemetry.cloud.tls.key_file",
	"telemetry.cloud.tls.key_file":  "telemetry.cloud.tls.cert_file",
}

// settingsKeyIndex maps schema keys to their sections.
type settingsKeyIndex struct {
	section map[string]string // schema key -> section
	restart map[string]bool   // schema keys applied only after a restart
	keys    []string          // sorted schema keys
}

func newSettingsKeyIndex(schema map[string]hubclient.ServerConfigSectionSchema) *settingsKeyIndex {
	ix := &settingsKeyIndex{section: map[string]string{}, restart: map[string]bool{}}
	for name, sec := range schema {
		for _, p := range sec.KoanfPaths {
			ix.section[p] = name
		}
		for _, p := range sec.RestartRequired {
			ix.restart[p] = true
		}
	}
	for k := range ix.section {
		ix.keys = append(ix.keys, k)
	}
	sort.Strings(ix.keys)
	return ix
}

// normalizeSettingsKey accepts the body spelling of federation keys
// (federation.enabled) for the schema spelling (server.federation.enabled).
func normalizeSettingsKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "federation" || strings.HasPrefix(key, "federation.") {
		return "server." + key
	}
	return key
}

// settingsBodyPath returns where a schema key sits in the request body and
// the GET response. Federation is sent top-level; every other key nests by
// its dotted path.
func settingsBodyPath(key string) []string {
	if rest, ok := strings.CutPrefix(key, "server.federation."); ok {
		return []string{"federation", rest}
	}
	if key == "server.federation" {
		return []string{"federation"}
	}
	return strings.Split(key, ".")
}

// resolve returns the schema key that holds key (key itself, or its
// longest schema-key prefix) and that key's section.
func (ix *settingsKeyIndex) resolve(key string) (schemaKey, section string, ok bool) {
	key = normalizeSettingsKey(key)
	best := ""
	for k := range ix.section {
		if (key == k || strings.HasPrefix(key, k+".")) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return "", "", false
	}
	return best, ix.section[best], true
}

// suggest returns up to three schema keys close to key.
func (ix *settingsKeyIndex) suggest(key string) []string {
	key = normalizeSettingsKey(key)
	last := key[strings.LastIndex(key, ".")+1:]
	type cand struct {
		k string
		d int
	}
	var cands []cand
	for _, k := range ix.keys {
		d := levenshtein(key, k)
		kLast := k[strings.LastIndex(k, ".")+1:]
		if kLast == last {
			d = 0
		}
		if d <= 3 {
			cands = append(cands, cand{k, d})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].d < cands[j].d })
	var out []string
	for i := 0; i < len(cands) && i < 3; i++ {
		out = append(out, cands[i].k)
	}
	return out
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// settingsChange is one requested change: set Key to Value, or unset it.
type settingsChange struct {
	Key   string
	Value interface{}
	Unset bool
}

// settingsPlanOptions tunes buildSettingsWrite.
type settingsPlanOptions struct {
	// AllowUnknown sends keys the schema does not list, for the hub to
	// decide. Without it such keys are refused, unless the hub reports its
	// file-backed keys editable.
	AllowUnknown bool
	// Revisions pins a section's expected revision, instead of the
	// revision read with GET.
	Revisions map[string]int64
}

// settingsWrite is a planned server configuration write.
type settingsWrite struct {
	Body      map[string]interface{}
	Revisions map[string]int64
	// Sections lists the sections touched, sorted.
	Sections []string
	// Unlisted lists the keys sent that the schema does not list.
	Unlisted []string
}

// settingsUnit collects the changes to one schema key.
type settingsUnit struct {
	key     string // schema key (or the key itself when unlisted)
	section string
	changes []settingsChange
}

// buildSettingsWrite plans a server configuration write for changes,
// against the configuration cfg read with GET and the schema index ix.
func buildSettingsWrite(cfg *hubclient.ServerConfig, ix *settingsKeyIndex, changes []settingsChange, opts settingsPlanOptions) (*settingsWrite, error) {
	if len(changes) == 0 {
		return nil, fmt.Errorf("no settings to change")
	}
	units := map[string]*settingsUnit{}
	var order []string
	var unlisted []string
	for _, ch := range changes {
		ch.Key = normalizeSettingsKey(ch.Key)
		if ch.Key == "" {
			return nil, fmt.Errorf("empty settings key")
		}
		if ch.Key == "expected_revisions" {
			return nil, fmt.Errorf("expected_revisions is not a settings key; use --revision SECTION=N")
		}
		schemaKey, section, ok := ix.resolve(ch.Key)
		if !ok {
			if !opts.AllowUnknown && !cfg.Layer0Editable {
				msg := fmt.Sprintf("%q is not a hub-managed setting", ch.Key)
				if s := ix.suggest(ch.Key); len(s) > 0 {
					msg += "; did you mean " + strings.Join(s, ", ") + "?"
				}
				return nil, fmt.Errorf("%s (see 'scion hub settings schema'; --no-validate sends it anyway)", msg)
			}
			schemaKey = ch.Key
			unlisted = append(unlisted, ch.Key)
		}
		if section == projectDefaultsSection {
			return nil, fmt.Errorf("%q is a project default, which is not part of the server configuration (PUT /api/v1/admin/project-defaults)", ch.Key)
		}
		u, ok := units[schemaKey]
		if !ok {
			u = &settingsUnit{key: schemaKey, section: section}
			units[schemaKey] = u
			order = append(order, schemaKey)
		}
		u.changes = append(u.changes, ch)
	}

	// Units must not overlap: a.b and a.b.c in one write would put two
	// values at one place in the body.
	sort.Strings(order)
	for i := 1; i < len(order); i++ {
		if strings.HasPrefix(order[i], order[i-1]+".") {
			return nil, fmt.Errorf("%q and %q overlap; change them in separate commands", order[i-1], order[i])
		}
	}

	unsetAt := map[string]bool{}
	for _, k := range order {
		for _, ch := range units[k].changes {
			if ch.Unset && ch.Key == k {
				unsetAt[k] = true
			}
		}
	}

	w := &settingsWrite{Body: map[string]interface{}{}, Revisions: map[string]int64{}, Unlisted: unlisted}
	touched := map[string]bool{}
	readModifyWrite := map[string]bool{} // sections with a change below a schema key
	for _, k := range order {
		u := units[k]
		for _, ch := range u.changes {
			if ch.Key != u.key && u.section != "" {
				readModifyWrite[u.section] = true
			}
		}
		value, err := unitValue(cfg, u, unsetAt)
		if err != nil {
			return nil, err
		}
		if err := putAtPath(w.Body, settingsBodyPath(k), value); err != nil {
			return nil, err
		}
		if u.section != "" {
			touched[u.section] = true
		}
	}

	for s := range touched {
		w.Sections = append(w.Sections, s)
	}
	sort.Strings(w.Sections)
	for s := range opts.Revisions {
		if !touched[s] {
			return nil, fmt.Errorf("--revision names section %q, which this change does not touch (touched: %s)", s, strings.Join(w.Sections, ", "))
		}
	}
	for _, s := range w.Sections {
		if rev, ok := opts.Revisions[s]; ok {
			// A change below a schema key sends a value built from this
			// GET, so only the revision of this GET guards it: a different
			// pin could let the write land on newer data it was not built
			// from.
			if meta, known := cfg.SectionMetadata[s]; known && readModifyWrite[s] && rev != meta.Revision {
				return nil, fmt.Errorf("section %s is at revision %d, not %d; this change rewrites part of its current value, so it can only be guarded by the revision just read (drop --revision for %s, or re-read and retry)", s, meta.Revision, rev, s)
			}
			w.Revisions[s] = rev
			continue
		}
		// A section the hub reports no metadata for (a file-backed hub)
		// gets no expected revision. A section reported without a stored
		// row has revision 0, which the hub reads as "create only".
		if meta, ok := cfg.SectionMetadata[s]; ok {
			w.Revisions[s] = meta.Revision
		}
	}
	return w, nil
}

// unitValue returns the body value for one schema key.
func unitValue(cfg *hubclient.ServerConfig, u *settingsUnit, unsetAt map[string]bool) (interface{}, error) {
	atKey := false
	below := false
	for _, ch := range u.changes {
		if ch.Key == u.key {
			atKey = true
		} else {
			below = true
		}
	}
	if atKey && below {
		return nil, fmt.Errorf("%q is changed both as a whole and below it; change it in separate commands", u.key)
	}
	if atKey {
		if len(u.changes) > 1 {
			return nil, fmt.Errorf("%q is given more than once", u.key)
		}
		ch := u.changes[0]
		if !ch.Unset {
			return ch.Value, nil
		}
		if reason, refused := settingsUnsetRefused[u.key]; refused {
			return nil, fmt.Errorf("cannot unset %q: %s", u.key, reason)
		}
		if partner, paired := settingsUnsetPairs[u.key]; paired && !unsetAt[partner] {
			return nil, fmt.Errorf("%q and %q are only valid together; unset both in one command", u.key, partner)
		}
		return nil, nil // explicit null: the hub clears the key
	}

	// Below the schema key: apply the changes to the current value and
	// send the whole value.
	var current interface{}
	if raw, ok := hubclient.LookupDotted(cfg.Raw, strings.Join(settingsBodyPath(u.key), ".")); ok {
		if err := json.Unmarshal(raw, &current); err != nil {
			return nil, fmt.Errorf("read current %q: %w", u.key, err)
		}
	}
	if current == nil {
		current = map[string]interface{}{}
	}
	root, ok := current.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%q is not an object, so it has no keys below it", u.key)
	}
	for _, ch := range u.changes {
		rest := strings.Split(strings.TrimPrefix(ch.Key, u.key+"."), ".")
		if ch.Unset {
			if err := deleteAtPath(root, rest); err != nil {
				return nil, fmt.Errorf("cannot unset %q: %w", ch.Key, err)
			}
			continue
		}
		if err := putAtPath(root, rest, ch.Value); err != nil {
			return nil, fmt.Errorf("cannot set %q: %w", ch.Key, err)
		}
	}
	return root, nil
}

// putAtPath sets m[path...] = value, creating intermediate objects.
func putAtPath(m map[string]interface{}, path []string, value interface{}) error {
	cur := m
	for i, p := range path {
		if p == "" {
			return fmt.Errorf("empty key segment in %q", strings.Join(path, "."))
		}
		if i == len(path)-1 {
			cur[p] = value
			return nil
		}
		next, exists := cur[p]
		if !exists || next == nil {
			nm := map[string]interface{}{}
			cur[p] = nm
			cur = nm
			continue
		}
		nm, ok := next.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%q is not an object", strings.Join(path[:i+1], "."))
		}
		cur = nm
	}
	return nil
}

// deleteAtPath removes m[path...]; it is an error when the key is not set.
func deleteAtPath(m map[string]interface{}, path []string) error {
	cur := m
	for i, p := range path {
		v, ok := cur[p]
		if !ok {
			return fmt.Errorf("it is not set")
		}
		if i == len(path)-1 {
			delete(cur, p)
			return nil
		}
		nm, ok := v.(map[string]interface{})
		if !ok {
			return fmt.Errorf("%q is not an object", strings.Join(path[:i+1], "."))
		}
		cur = nm
	}
	return nil
}

// parseSettingsValue reads a command-line value as YAML (true, 42, 1.5,
// "text", [a, b], {k: v}); asString keeps it as a string. An empty value
// is the empty string. A YAML null is refused: unset clears a key.
func parseSettingsValue(s string, asString bool) (interface{}, error) {
	if asString || s == "" {
		return s, nil
	}
	var v interface{}
	if err := yaml.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("value is not valid YAML (use --string to send it as text): %w", err)
	}
	if v == nil {
		return nil, fmt.Errorf("value is null; use 'unset' to clear a key")
	}
	return normalizeYAMLValue(v)
}

// normalizeYAMLValue converts YAML-decoded maps to JSON-encodable ones.
func normalizeYAMLValue(v interface{}) (interface{}, error) {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			n, err := normalizeYAMLValue(e)
			if err != nil {
				return nil, err
			}
			out[k] = n
		}
		return out, nil
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("map key %v is not a string", k)
			}
			n, err := normalizeYAMLValue(e)
			if err != nil {
				return nil, err
			}
			out[ks] = n
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			n, err := normalizeYAMLValue(e)
			if err != nil {
				return nil, err
			}
			out[i] = n
		}
		return out, nil
	default:
		return v, nil
	}
}

// settingsChangesFromDocument flattens a YAML or JSON settings document
// into changes, one per schema key it reaches: a member at a schema key is
// that key's value; a member above one is descended into. A null value is
// an unset. A member that reaches no schema key is a change of that key
// (refused later unless unlisted keys are allowed).
func settingsChangesFromDocument(doc map[string]interface{}, ix *settingsKeyIndex) []settingsChange {
	var out []settingsChange
	var walk func(prefix string, m map[string]interface{})
	walk = func(prefix string, m map[string]interface{}) {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := m[k]
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			norm := normalizeSettingsKey(key)
			if _, isKey := ix.section[norm]; isKey {
				out = append(out, settingsChange{Key: key, Value: v, Unset: v == nil})
				continue
			}
			if sub, ok := v.(map[string]interface{}); ok && ix.hasKeyBelow(norm) {
				walk(key, sub)
				continue
			}
			out = append(out, settingsChange{Key: key, Value: v, Unset: v == nil})
		}
	}
	walk("", doc)
	return out
}

// hasKeyBelow reports whether a schema key starts with prefix + ".".
func (ix *settingsKeyIndex) hasKeyBelow(prefix string) bool {
	for _, k := range ix.keys {
		if strings.HasPrefix(k, prefix+".") {
			return true
		}
	}
	return false
}

// parseRevisionPins reads --revision SECTION=N flags.
func parseRevisionPins(pins []string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, p := range pins {
		name, num, ok := strings.Cut(p, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("--revision %q: want SECTION=N", p)
		}
		n, err := strconv.ParseInt(strings.TrimSpace(num), 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("--revision %q: N must be a non-negative integer", p)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("--revision names section %q more than once", name)
		}
		out[name] = n
	}
	return out, nil
}

// settingsMaskedValue replaces a sensitive value in output the CLI prints
// itself (--dry-run). It matches the placeholder the Hub uses on GET.
const settingsMaskedValue = "********"

// settingsSensitiveName reports whether a settings key name holds secret
// material: a name containing secret, token, password or passwd, or naming
// key or credential material (private_key, api_key, apikey, credential).
// The section schemas mark no keys as secret, so names decide.
func settingsSensitiveName(name string) bool {
	n := strings.ToLower(name)
	for _, s := range []string{"secret", "token", "password", "passwd", "private_key", "privatekey", "api_key", "apikey", "credential"} {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// maskSensitiveSettings returns a copy of a request body with every value
// under a sensitive key replaced by settingsMaskedValue, and also every value
// the Hub itself masks on GET whatever its name: server.database.url,
// telemetry.cloud.headers values and notification channel params. Strings
// (except empty ones) and numbers are masked, since a secret can be typed as
// a number; booleans, nulls and the structure are kept, so a dry run still
// shows which keys would be sent and on/off switches stay readable.
func maskSensitiveSettings(v interface{}) interface{} {
	return maskSettingsAt(v, nil, false)
}

func maskSettingsAt(v interface{}, path []string, masked bool) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			p := append(append([]string(nil), path...), k)
			out[k] = maskSettingsAt(e, p, masked || settingsSensitiveName(k) || settingsHubMaskedPath(p))
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = maskSettingsAt(e, path, masked)
		}
		return out
	case string:
		if masked && t != "" {
			return settingsMaskedValue
		}
		return t
	case bool, nil:
		return v
	default: // numbers
		if masked {
			return settingsMaskedValue
		}
		return v
	}
}

// settingsHubMaskedPaths are the body paths whose values the Hub masks on
// GET by position rather than name (pkg/hub maskSensitiveFields): the
// database URL (a DSN can carry a password), telemetry.cloud.headers, and the
// params of each notification channel (server.notification_channels[*]
// .params; list positions are not part of the path). Every other field the
// Hub masks has a secret-looking name.
var settingsHubMaskedPaths = map[string]bool{
	"server.database.url":                 true,
	"telemetry.cloud.headers":             true,
	"server.notification_channels.params": true,
}

func settingsHubMaskedPath(p []string) bool {
	return settingsHubMaskedPaths[strings.Join(p, ".")]
}
