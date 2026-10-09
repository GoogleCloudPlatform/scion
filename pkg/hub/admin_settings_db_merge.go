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
// keys, restricted to the sent keys. For most sections a sent object value
// replaces the stored one (nested objects are not merged); a section in
// deepMergeSections merges its nested objects key by key instead (see
// below):
//
//   - A sent key with a value in requestDoc takes that value.
//   - A sent key that requestDoc leaves out is removed from the row;
//     because a section with a stored row owns all of its keys, the key
//     is then unset (bootstrap values from settings.yaml or env are not
//     re-applied). Which sent
//     values are left out follows each field's encoding in requestDoc: an
//     explicit null, and for omitempty fields their zero value ("", 0, []).
//     A *bool field carries an explicit false as a value.
//   - A key the body omits keeps its stored value.
//
// Deep merge (deepMergeSections, telemetry today; ptone/scion#3717): when
// a sent key's field is a struct (for example telemetry.cloud or
// telemetry.filter) and the body sends a JSON object for it, the rules
// above apply again inside that object, at every depth: a nested key the
// body omits keeps its stored value, a sent nested key replaces it, and
// an explicit null or a zero value left out by omitempty clears it. An
// empty object ({}) for a struct field therefore changes nothing; send
// null to clear the whole object. Maps with free-form keys (resource,
// cloud.headers, filter.sampling.rates) and arrays (filter.events.include
// and the like) are not merged: a sent value replaces the stored one as a
// whole, and {} or [] clears it. This is the convention of the file-mode
// server merge (mergeServerSettings).
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
// invalid, the write's validation rejects the save as before. In a
// deep-merge section the same holds inside a merged object: a carried
// nested key whose value fails the schema is dropped
// ("<section>.<key>.<nested key>"), a sent one is not.
//
// Only keys the section models (sectionKeyKoanfPath) are
// applied, so request-only members (for example the GitHub App secrets,
// which are never stored in the section) cannot remove a stored key. Sent
// keys are matched to the section's fields with the case-insensitive rule
// the typed request decode follows (structFieldByJSONName).
//
// The row is read fresh from the store (the ops cache can be stale in HA).
// With no row, the base is empty and the save creates the row from the
// sent keys only; from then on the row owns every key in the section. In
// practice the no-row path is rare: startup seeding (syncHubSettings in
// cmd/server_foreground.go) creates a seeded row from bootstrap material
// for every registered section on boot, so the base is normally that
// seeded row. For a non-managed (seeded) row, stored keys overridden by a
// node-local env var are dropped, so one node's env value is not pinned
// into the shared row (see buildAccessDocOnCurrent). In a deep-merge
// section only the overridden nested key is dropped (for example
// telemetry.cloud.enabled), never the object that holds it.
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

	tree := patchSection(section, base, next, fp)

	schema, _ := opsettings.SchemaInfo()[section].Schema.(map[string]interface{})
	if dropped := dropKeysForbiddenBySchema(section, schema, base); len(dropped) > 0 {
		slog.Warn("admin settings save: removing stored keys the section schema does not allow (takes effect only if the save is written)",
			"section", section, "keys", dropped)
	}
	validate := func(doc json.RawMessage) bool { return len(opsettings.Validate(section, doc)) == 0 }
	dropped := dropInvalidCarriedKeys(section, schema, base, tree.keys(), validate)
	if deepMergeSections[section] {
		dropped = append(dropped, dropInvalidCarriedNestedKeys(section, base, tree, validate)...)
		sort.Strings(dropped)
	}
	if len(dropped) > 0 {
		slog.Warn("admin settings save: removing stored keys whose value fails the section schema (takes effect only if the save is written)",
			"section", section, "keys", dropped)
	}

	doc, err := json.Marshal(base)
	if err != nil {
		return nil, 0, fmt.Errorf("marshalling %s doc: %w", section, err)
	}
	return doc, baseRev, nil
}

// deepMergeSections lists the sections whose nested objects
// mergeSectionOnCurrent merges key by key rather than replacing them
// whole (see its doc comment). A section is listed when clients send only
// part of its nested objects: the settings page sends telemetry.cloud
// without headers, tls or batch, and no telemetry.filter or resource at
// all (ptone/scion#3717). Every other section keeps the top-level rule;
// github_app and agent_defaults have no nested object the page edits in
// part.
//
// dropInvalidCarriedNestedKeys checks a carried nested key on its own,
// wrapped in its parent objects, so a listed section's nested object
// schemas must not have required keys (telemetry's have none).
var deepMergeSections = map[string]bool{"telemetry": true}

// sentTree records the keys a body sent at one object level, by the
// section-level (or nested) JSON key the value was applied to. A key maps
// to nil when the sent value was applied whole (a scalar, an array, a
// map, null, or any value in a section that is not deep-merged), and to
// the keys sent inside it when its object was merged key by key.
type sentTree map[string]sentTree

// keys returns the keys of t as a set.
func (t sentTree) keys() map[string]bool {
	out := make(map[string]bool, len(t))
	for k := range t {
		out[k] = true
	}
	return out
}

// applySectionPatch applies the keys of fp that the section models onto
// base, taking their values from next (see mergeSectionOnCurrent), and
// returns the set of section keys the body sent.
func applySectionPatch(section string, base, next map[string]json.RawMessage, fp *fieldPresence) map[string]bool {
	return patchSection(section, base, next, fp).keys()
}

// patchSection is applySectionPatch, returning the keys sent at every
// level that was merged key by key.
func patchSection(section string, base, next map[string]json.RawMessage, fp *fieldPresence) sentTree {
	sent := sentTree{}
	if fp == nil {
		return sent
	}
	deep := deepMergeSections[section]
	model := sectionModelType(section)
	for sentKey, raw := range fp.raw {
		key, ok := modelledSectionKey(section, sentKey)
		if !ok {
			continue
		}
		if deep && model != nil && isJSONObject(raw) {
			if f, ok := structFieldByJSONName(model, key); ok {
				if st, ok := structTypeOf(f.Type); ok {
					sent[key] = mergeObjectKey(base, next, key, st, raw)
					continue
				}
			}
		}
		sent[key] = nil
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

// mergeObjectKey merges the object raw, sent for obj[key], into the stored
// object at obj[key] key by key, and returns the keys sent inside it. t is
// the struct type of the field; the sent members are matched to its fields
// with sentStructFields, the rule the typed request decode follows, and
// their values are taken from next[key] (the request doc at this level).
// A nested struct field sent as an object is merged the same way; any
// other sent member replaces the stored value, or clears it when next
// leaves it out (an explicit null, or a zero value dropped by omitempty).
// A stored value that is not an object is replaced. When nothing is left
// in the merged object, obj[key] is removed.
func mergeObjectKey(obj, next map[string]json.RawMessage, key string, t reflect.Type, raw json.RawMessage) sentTree {
	cur := map[string]json.RawMessage{}
	if v, ok := obj[key]; ok && isJSONObject(v) {
		if err := json.Unmarshal(v, &cur); err != nil || cur == nil {
			cur = map[string]json.RawMessage{}
		}
	}
	var nx map[string]json.RawMessage
	if v, ok := next[key]; ok && isJSONObject(v) {
		_ = json.Unmarshal(v, &nx)
	}
	sent := sentTree{}
	fields, _ := sentStructFields(t, raw)
	for _, sf := range fields {
		k := jsonFieldName(sf.field)
		if st, ok := structTypeOf(sf.field.Type); ok && isJSONObject(sf.val) {
			sent[k] = mergeObjectKey(cur, nx, k, st, sf.val)
			continue
		}
		sent[k] = nil
		if v, ok := nx[k]; ok && !isJSONNull(v) {
			cur[k] = v
		} else {
			delete(cur, k)
		}
	}
	if len(cur) == 0 {
		delete(obj, key)
		return sent
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return sent
	}
	obj[key] = b
	return sent
}

// structTypeOf returns t, with pointers removed, when it is a struct type.
func structTypeOf(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t, t.Kind() == reflect.Struct
}

// jsonFieldName returns the JSON key encoding/json uses for f.
func jsonFieldName(f reflect.StructField) string {
	if jn := strings.Split(f.Tag.Get("json"), ",")[0]; jn != "" {
		return jn
	}
	return f.Name
}

// sectionModelType returns the struct type of a section's document
// (opsettings.Section.New), or nil when the section has none. A struct
// that only embeds another struct (TelemetrySettings embeds
// config.V1TelemetryConfig) resolves to the embedded type, whose fields
// are the section's top-level keys.
func sectionModelType(section string) reflect.Type {
	sec := opsettings.SectionByName(section)
	if sec == nil || sec.New == nil {
		return nil
	}
	t, ok := structTypeOf(reflect.TypeOf(sec.New()))
	if !ok {
		return nil
	}
	if t.NumField() == 1 && t.Field(0).Anonymous {
		if et, ok := structTypeOf(t.Field(0).Type); ok {
			return et
		}
	}
	return t
}

// dropInvalidCarriedNestedKeys is dropInvalidCarriedKeys inside the
// objects a deep-merge section merged key by key (tree): within each,
// nested keys the body did not send whose value fails the section schema
// are removed. It returns their paths ("<section>.<key>.<nested key>"),
// sorted. Each carried key is checked on its own, as a document holding
// only that key inside its parent objects.
func dropInvalidCarriedNestedKeys(section string, doc map[string]json.RawMessage, tree sentTree, validate func(json.RawMessage) bool) []string {
	if whole, err := json.Marshal(doc); err != nil || validate(whole) {
		return nil
	}
	var dropped []string
	for key, sub := range tree {
		if sub == nil {
			continue
		}
		dropped = append(dropped, dropInvalidCarriedIn(section, doc, []string{key}, sub, validate)...)
	}
	sort.Strings(dropped)
	return dropped
}

// dropInvalidCarriedIn checks the object at path (relative to the section
// document) inside parent, whose last element names it, for
// dropInvalidCarriedNestedKeys.
func dropInvalidCarriedIn(section string, parent map[string]json.RawMessage, path []string, sent sentTree, validate func(json.RawMessage) bool) []string {
	key := path[len(path)-1]
	v, ok := parent[key]
	if !ok || !isJSONObject(v) {
		return nil
	}
	if validate(wrapAtPath(path[:len(path)-1], map[string]json.RawMessage{key: v})) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(v, &obj); err != nil {
		return nil
	}
	var dropped []string
	for k, cv := range obj {
		sub, wasSent := sent[k]
		switch {
		case sub != nil:
			dropped = append(dropped, dropInvalidCarriedIn(section, obj, append(append([]string{}, path...), k), sub, validate)...)
		case wasSent:
		default:
			if validate(wrapAtPath(path, map[string]json.RawMessage{k: cv})) {
				continue
			}
			delete(obj, k)
			dropped = append(dropped, section+"."+strings.Join(path, ".")+"."+k)
		}
	}
	if b, err := json.Marshal(obj); err == nil {
		parent[key] = b
	}
	return dropped
}

// wrapAtPath returns the JSON document holding leaf nested inside the
// objects named by path, outermost first.
func wrapAtPath(path []string, leaf map[string]json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(leaf)
	for i := len(path) - 1; i >= 0; i-- {
		b, _ = json.Marshal(map[string]json.RawMessage{path[i]: b})
	}
	return b
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
	if sectionKeyKoanfPath(section, sentKey) != "" {
		return sentKey, true
	}
	model := sectionModelType(section)
	if model == nil {
		return "", false
	}
	f, ok := structFieldByJSONName(model, sentKey)
	if !ok {
		return "", false
	}
	key := jsonFieldName(f)
	if sectionKeyKoanfPath(section, key) == "" {
		return "", false
	}
	return key, true
}

// sectionKeyKoanfPath returns the koanf path of a section-level JSON key,
// or "" when the section does not model the key. Sections with a mapping
// in opsettings (KoanfPathFromSectionKey) use it. Sections without one
// store their keys at the koanf path of the same name, either bare
// (agent_defaults: default_template) or under the section name
// (telemetry: telemetry.cloud); for those, the key is modelled when that
// path is one of the section's registered koanf paths.
func sectionKeyKoanfPath(section, key string) string {
	if p := opsettings.KoanfPathFromSectionKey(section, key); p != "" {
		return p
	}
	sec := opsettings.SectionByName(section)
	if sec == nil {
		return ""
	}
	for _, p := range sec.KoanfPaths {
		if p == key || p == section+"."+key {
			return p
		}
	}
	return ""
}

// dropEnvOverriddenSectionKeys removes the stored keys of a section whose
// koanf key is overridden by a node-local env var. In a deep-merge section
// an env var can pin a nested key (SCION_SERVER_TELEMETRY_CLOUD_ENABLED
// pins telemetry.cloud.enabled); only that nested key is removed, and the
// object holding it keeps its other keys.
func dropEnvOverriddenSectionKeys(base map[string]json.RawMessage, section string, envKeys []string) {
	if len(envKeys) == 0 {
		return
	}
	env := make(map[string]bool, len(envKeys))
	for _, k := range envKeys {
		env[k] = true
	}
	for key := range base {
		if p := sectionKeyKoanfPath(section, key); p != "" && env[p] {
			delete(base, key)
		}
	}
	if !deepMergeSections[section] {
		return
	}
	prefix := section + "."
	for _, k := range envKeys {
		rest, ok := strings.CutPrefix(k, prefix)
		if !ok {
			continue
		}
		if path := strings.Split(rest, "."); len(path) > 1 {
			deleteNestedKey(base, path)
		}
	}
}

// deleteNestedKey removes the key at path (two or more elements) from the
// objects nested in obj, leaving its parent objects in place. Nothing
// changes when a parent along the path is missing or not an object.
func deleteNestedKey(obj map[string]json.RawMessage, path []string) {
	v, ok := obj[path[0]]
	if !ok || !isJSONObject(v) {
		return
	}
	var child map[string]json.RawMessage
	if err := json.Unmarshal(v, &child); err != nil || child == nil {
		return
	}
	if len(path) == 2 {
		if _, ok := child[path[1]]; !ok {
			return
		}
		delete(child, path[1])
	} else {
		deleteNestedKey(child, path[1:])
	}
	if b, err := json.Marshal(child); err == nil {
		obj[path[0]] = b
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

// telemetryPresence returns the presence of the top-level telemetry object
// in a PUT body, or nil when the body has none or it is not a JSON object.
// The member is resolved with sentStructFields, the rule the typed request
// decode follows.
func telemetryPresence(rawBody []byte) *fieldPresence {
	sent, ok := sentStructFields(reflect.TypeOf(ServerConfigUpdateRequest{}), rawBody)
	if !ok {
		return nil
	}
	for _, sf := range sent {
		if sf.field.Name != "Telemetry" || !isJSONObject(sf.val) {
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

// agentDefaultsRequestKeys are the agent_defaults keys a server-config PUT
// writes (buildSingleSectionDoc). The section's other keys are read-only
// here: default_max_agent_role and default_agent_role are accepted only as
// an unchanged echo of GET (dbUnwrittenLayer1Paths), and
// default_harness_auth has no request field. They are never in
// agentDefaultsPresence, so a save never changes or clears them.
var agentDefaultsRequestKeys = map[string]bool{
	"default_template":                        true,
	"default_harness_config":                  true,
	"default_max_turns":                       true,
	"default_max_model_calls":                 true,
	"default_max_duration":                    true,
	"default_resources":                       true,
	"default_model":                           true,
	"default_thinking_level":                  true,
	"default_runtime_broker":                  true,
	"default_timezone":                        true,
	"default_gcp_identity_mode":               true,
	"default_gcp_identity_service_account_id": true,
}

// agentDefaultsPresence returns the presence of the agent_defaults keys in
// a PUT body, whose keys are top-level members: the members that resolve
// (modelledSectionKey, the case-insensitive rule the typed decode follows)
// to one of agentDefaultsRequestKeys. It returns nil when fp is nil.
func agentDefaultsPresence(fp *fieldPresence) *fieldPresence {
	if fp == nil {
		return nil
	}
	out := &fieldPresence{raw: map[string]json.RawMessage{}}
	for k, v := range fp.raw {
		if key, ok := modelledSectionKey("agent_defaults", k); ok && agentDefaultsRequestKeys[key] {
			out.raw[k] = v
		}
	}
	return out
}

// sentKeys returns the member names of fp, or nil when fp is nil.
func (fp *fieldPresence) sentKeys() map[string]json.RawMessage {
	if fp == nil {
		return nil
	}
	return fp.raw
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
