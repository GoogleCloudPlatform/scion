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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// mergeSectionOnCurrent builds the document for a DB-backed admin settings
// save of one section on top of the section's current row, so a save
// changes only the keys it sends (ptone/scion#3718).
//
// requestDoc is the section document built from the request
// (buildSingleSectionDoc). fp is the presence of the section's own object
// in the raw request body: its keys are section-level JSON keys, and it
// decides which keys the request sends. The result is the current row's
// raw JSON with an RFC 7386-style merge patch applied to its top-level
// keys only (a sent object value replaces the stored one; nested objects
// are not merged), restricted to the sent keys:
//
//   - A sent key with a value in requestDoc takes that value.
//   - A sent key that requestDoc leaves out is removed from the row, so
//     the bootstrap value applies again, if there is one. Which sent
//     values are left out follows each field's encoding in requestDoc: an
//     explicit null, and for omitempty fields their zero value ("", 0, []).
//     A *bool field carries an explicit false as a value.
//   - A key the body omits keeps its stored value.
//
// Carried-forward keys never block the save and are never dropped
// silently:
//
//   - A stored key the section does not model is kept as stored when the
//     section schema allows it. When the schema forbids it (the section
//     object has additionalProperties: false, as github_app does), it is
//     dropped before the write.
//   - A stored key the body does not send whose value fails the section
//     schema (for example a string where an integer is required) is
//     dropped before the write.
//
// Each dropped key is named by path ("<section>.<key>", never its value)
// in a warning. A key the body sends is never dropped: if its value is
// invalid, the write's validation rejects the save as before.
//
// Only keys the section models (opsettings.KoanfPathFromSectionKey) are
// applied, so request-only members (for example the GitHub App secrets,
// which are never stored in the section) cannot remove a stored key. Sent
// keys are matched to the section's fields with the case-insensitive rule
// the typed request decode follows (structFieldByJSONName).
//
// The row is read fresh from the store (the ops cache can be stale in HA).
// With no row the base is empty, so absent keys fall back to the bootstrap
// value. For a non-managed (seeded) row, stored keys overridden by a
// node-local env var are dropped, so one node's env value is not pinned
// into the shared row (see buildAccessDocOnCurrent).
//
// It returns the revision the base was read at (0 when no row exists), for
// use as the CAS expected revision, so a concurrent write to the section
// between this read and the write turns into a 409 rather than a lost
// update.
func mergeSectionOnCurrent(ctx context.Context, ops *OperationalSettings, section string, requestDoc json.RawMessage, fp *fieldPresence) (json.RawMessage, int64, error) {
	var next map[string]json.RawMessage
	if len(requestDoc) > 0 {
		if err := json.Unmarshal(requestDoc, &next); err != nil {
			return nil, 0, fmt.Errorf("decoding %s request doc: %w", section, err)
		}
	}

	base := map[string]json.RawMessage{}
	var baseRev int64
	row, err := ops.store.GetHubSetting(ctx, section)
	switch {
	case err == nil:
		if len(bytes.TrimSpace(row.Value)) > 0 {
			if err := json.Unmarshal(row.Value, &base); err != nil {
				return nil, 0, fmt.Errorf("decoding current %s row: %w", section, err)
			}
			if base == nil { // stored JSON null
				base = map[string]json.RawMessage{}
			}
		}
		baseRev = row.Revision
		if row.Origin != "managed" {
			dropEnvOverriddenSectionKeys(base, section, ops.EnvOverriddenKeys())
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		return nil, 0, fmt.Errorf("reading current %s row: %w", section, err)
	}

	sent := applySectionPatch(section, base, next, fp)

	schema, _ := opsettings.SchemaInfo()[section].Schema.(map[string]interface{})
	if dropped := dropKeysForbiddenBySchema(section, schema, base); len(dropped) > 0 {
		slog.Warn("admin settings save: removing stored keys the section schema does not allow (takes effect only if the save is written)",
			"section", section, "keys", dropped)
	}
	validate := func(doc json.RawMessage) bool { return len(opsettings.Validate(section, doc)) == 0 }
	if dropped := dropInvalidCarriedKeys(section, schema, base, sent, validate); len(dropped) > 0 {
		slog.Warn("admin settings save: removing stored keys whose value fails the section schema (takes effect only if the save is written)",
			"section", section, "keys", dropped)
	}

	doc, err := json.Marshal(base)
	if err != nil {
		return nil, 0, fmt.Errorf("marshalling %s doc: %w", section, err)
	}
	return doc, baseRev, nil
}

// applySectionPatch applies the keys of fp that the section models onto
// base, taking their values from next (see mergeSectionOnCurrent), and
// returns the set of section keys the body sent.
func applySectionPatch(section string, base, next map[string]json.RawMessage, fp *fieldPresence) map[string]bool {
	sent := map[string]bool{}
	if fp == nil {
		return sent
	}
	for sentKey := range fp.raw {
		key, ok := modelledSectionKey(section, sentKey)
		if !ok {
			continue
		}
		sent[key] = true
		// requestDoc is a marshalled section struct for the sections wired
		// today, so it holds no null; the check keeps a raw request doc
		// (a future section) from storing an explicit null as a value.
		if v, ok := next[key]; ok && !isJSONNull(v) {
			base[key] = v
		} else {
			delete(base, key)
		}
	}
	return sent
}

// dropInvalidCarriedKeys removes from doc the keys the body did not send
// (not in sent) whose value fails the section schema, and returns their
// paths ("<section>.<key>"), sorted. validate reports whether a section
// document is valid. When the whole doc is valid nothing is dropped;
// otherwise each carried key is checked on its own, as a one-key
// document. A schema with top-level required keys cannot be checked that
// way, so for it nothing is dropped and the write's validation decides.
func dropInvalidCarriedKeys(section string, schema map[string]interface{}, doc map[string]json.RawMessage, sent map[string]bool, validate func(json.RawMessage) bool) []string {
	if whole, err := json.Marshal(doc); err != nil || validate(whole) {
		return nil
	}
	if req, ok := schema["required"].([]interface{}); ok && len(req) > 0 {
		return nil
	}
	var dropped []string
	for key, v := range doc {
		if sent[key] {
			continue
		}
		one, err := json.Marshal(map[string]json.RawMessage{key: v})
		if err != nil || validate(one) {
			continue
		}
		delete(doc, key)
		dropped = append(dropped, section+"."+key)
	}
	sort.Strings(dropped)
	return dropped
}

// dropKeysForbiddenBySchema removes from doc the top-level keys that are
// not properties of the object schema when it sets additionalProperties to
// false, and returns their paths ("<section>.<key>"), sorted. With a nil
// schema, or one that allows extra keys, nothing is dropped.
func dropKeysForbiddenBySchema(section string, schema map[string]interface{}, doc map[string]json.RawMessage) []string {
	if schema == nil {
		return nil
	}
	if extra, ok := schema["additionalProperties"].(bool); !ok || extra {
		return nil
	}
	props, _ := schema["properties"].(map[string]interface{})
	var dropped []string
	for key := range doc {
		if _, ok := props[key]; ok {
			continue
		}
		delete(doc, key)
		dropped = append(dropped, section+"."+key)
	}
	sort.Strings(dropped)
	return dropped
}

// modelledSectionKey resolves a key sent in the request body to the
// section-level JSON key the section models. The key is matched to the
// section struct's fields with structFieldByJSONName (an exact match,
// otherwise a case-insensitive one, as the typed request decode matches
// them). ok is false for a key the section does not model.
func modelledSectionKey(section, sentKey string) (string, bool) {
	if opsettings.KoanfPathFromSectionKey(section, sentKey) != "" {
		return sentKey, true
	}
	sec := opsettings.SectionByName(section)
	if sec == nil || sec.New == nil {
		return "", false
	}
	f, ok := structFieldByJSONName(reflect.TypeOf(sec.New()), sentKey)
	if !ok {
		return "", false
	}
	key := strings.Split(f.Tag.Get("json"), ",")[0]
	if key == "" {
		key = f.Name
	}
	if opsettings.KoanfPathFromSectionKey(section, key) == "" {
		return "", false
	}
	return key, true
}

// dropEnvOverriddenSectionKeys removes the stored keys of a section whose
// koanf key is overridden by a node-local env var.
func dropEnvOverriddenSectionKeys(base map[string]json.RawMessage, section string, envKeys []string) {
	if len(envKeys) == 0 {
		return
	}
	env := make(map[string]bool, len(envKeys))
	for _, k := range envKeys {
		env[k] = true
	}
	for key := range base {
		if p := opsettings.KoanfPathFromSectionKey(section, key); p != "" && env[p] {
			delete(base, key)
		}
	}
}

// githubAppPresence returns the presence of the server.github_app object in
// a PUT body, or nil when the body has none or it is not a JSON object. The
// server and github_app members are resolved with sentStructFields, the
// rule the typed request decode follows, so their spelling matches the
// decode.
func githubAppPresence(rawBody []byte) *fieldPresence {
	rawServer := rawServerObject(rawBody)
	if rawServer == nil {
		return nil
	}
	sent, ok := sentStructFields(reflect.TypeOf(config.V1ServerConfig{}), rawServer)
	if !ok {
		return nil
	}
	for _, sf := range sent {
		if sf.field.Name != "GitHubApp" || !isJSONObject(sf.val) {
			continue
		}
		fp, err := parseFieldPresence(sf.val)
		if err != nil {
			return nil
		}
		return fp
	}
	return nil
}

// githubAppPresenceFromTop returns the presence of the server.github_app
// object from the top-level presence of a PUT body, resolved with the same
// rule as githubAppPresence. It returns nil when fp is nil. It re-derives
// the same presence handlePutServerConfigDB passes to mergeSectionOnCurrent
// (githubAppPresence(rawBody)); both must stay on githubAppPresence so the
// doc builder and the merge agree on which keys were sent.
func githubAppPresenceFromTop(fp *fieldPresence) *fieldPresence {
	if fp == nil {
		return nil
	}
	b, err := json.Marshal(fp.raw)
	if err != nil {
		return nil
	}
	return githubAppPresence(b)
}

// sentFold returns the raw value sent for name: the member named exactly
// name, otherwise one whose name matches it case-insensitively, as the
// typed request decode matches members to fields. ok is false when no
// such member was sent.
func (fp *fieldPresence) sentFold(name string) (json.RawMessage, bool) {
	if fp == nil {
		return nil, false
	}
	if v, ok := fp.raw[name]; ok {
		return v, true
	}
	for k, v := range fp.raw {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return nil, false
}
