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
// deep-merge section the check reaches nested keys, in merged and carried
// objects alike: only the carried nested key whose value fails is dropped
// ("<section>.<key>.<nested key>"), not the object holding it; a sent
// nested key is never dropped.
//
// Only keys the section models (sectionKeyKoanfPath) are
// applied, so request-only members (for example the GitHub App secrets,
// which are never stored in the section) cannot remove a stored key. Sent
// keys are matched to the section's fields with the case-insensitive rule
// the typed request decode follows (structFieldByJSONName).
//
// The row is read fresh from the store (the ops cache can be stale in HA).
// With no row, the base is empty (access and endpoints start from the
// effective snapshot values instead; see sectionMergeOptions) and the save
// creates the row from the sent keys only; from then on the row owns every key in the section. In
// practice the no-row path is rare: startup seeding (syncHubSettings in
// cmd/server_foreground.go) creates a seeded row from bootstrap material
// for every registered section on boot, so the base is normally that
// seeded row. For a non-managed (seeded) row, stored keys overridden by a
// node-local env var are dropped, so one node's env value is not pinned
// into the shared row as managed (ptone/scion#2068; only this node's env
// keys are known, so a row seeded by another node may still carry that
// node's env values). In a deep-merge
// section only the overridden nested key is dropped (for example
// telemetry.cloud.enabled), never the object that holds it.
//
// It returns the revision the base was read at (0 when no row exists), for
// use as the CAS expected revision, so a concurrent write to the section
// between this read and the write turns into a 409 rather than a lost
// update.
func mergeSectionOnCurrent(ctx context.Context, ops *OperationalSettings, section string, requestDoc json.RawMessage, fp *fieldPresence) (json.RawMessage, int64, error) {
	return mergeSectionOnCurrentWith(ctx, ops, section, requestDoc, fp, sectionMergeOptions{})
}

// sectionMergeOptions adjusts the base mergeSectionOnCurrentWith merges
// onto, for sections whose base has a rule of its own. The zero value is
// the plain mergeSectionOnCurrent behaviour.
type sectionMergeOptions struct {
	// noRowBase, when set, returns the base used when the section has no
	// row, keyed by section-level JSON key (access and endpoints start
	// from the effective snapshot values). Keys overridden by a node-local
	// env var are dropped from it, as for a non-managed row. When nil, the
	// base is empty.
	noRowBase func(ops *OperationalSettings) map[string]json.RawMessage
	// seededBase, when set, edits the base read from a non-managed
	// (seeded) row after env-overridden keys are dropped (endpoints drops
	// the bootstrap hub_name, which applies without being written).
	seededBase func(base map[string]json.RawMessage)
}

// mergeSectionOnCurrentWith is mergeSectionOnCurrent with the base rules
// in opts.
func mergeSectionOnCurrentWith(ctx context.Context, ops *OperationalSettings, section string, requestDoc json.RawMessage, fp *fieldPresence, opts sectionMergeOptions) (json.RawMessage, int64, error) {
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
			if opts.seededBase != nil {
				opts.seededBase(base)
			}
		}
	case errors.Is(err, store.ErrNotFound):
		if opts.noRowBase != nil {
			if nb := opts.noRowBase(ops); nb != nil {
				base = nb
			}
			dropEnvOverriddenSectionKeys(base, section, ops.EnvOverriddenKeys())
		}
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
	var dropped []string
	if deepMergeSections[section] {
		dropped = dropInvalidCarriedNestedKeys(section, base, tree, validate)
	}
	dropped = append(dropped, dropInvalidCarriedKeys(section, schema, base, tree.keys(), validate)...)
	sort.Strings(dropped)
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
// all (ptone/scion#3717). Every other section keeps the top-level rule,
// because no client sends part of a nested object in it:
//   - github_app, agent_defaults, access, endpoints, lifecycle, quotas,
//     auto_expose_ports and agent_secrets have no nested object fields
//     (agent_defaults default_resources is a whole value the page builds
//     in full).
//   - notifications (notification_channels) and federation
//     (trusted_issuers, algorithms) hold arrays, which are replaced whole
//     in a deep-merged section as well; the pages send them in full.
//   - The map sections (runtimes, profiles, harness_configs) are not
//     merged at all (bodyMergedSections).
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

// add records key as sent with the keys sent inside it (nil when its value
// was applied whole). When key was already recorded (the body sent it under
// two spellings that resolve to the same field, for example "cloud" and
// "Cloud"), the two records are combined (unionSentTrees), so a key sent
// under either spelling counts as sent.
func (t sentTree) add(key string, sub sentTree) {
	if prev, dup := t[key]; dup {
		t[key] = unionSentTrees(prev, sub)
		return
	}
	t[key] = sub
}

// unionSentTrees combines two records of the keys sent inside one key. A
// value applied whole (nil) in either record makes the combined value
// whole: nothing inside it counts as carried.
func unionSentTrees(a, b sentTree) sentTree {
	if a == nil || b == nil {
		return nil
	}
	out := make(sentTree, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out.add(k, v)
	}
	return out
}

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
					sent.add(key, mergeObjectKey(base, next, key, st, raw))
					continue
				}
			}
		}
		sent.add(key, nil)
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
			sent.add(k, mergeObjectKey(cur, nx, k, st, sf.val))
			continue
		}
		sent.add(k, nil)
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
// (opsettings.Section.New), or nil when the section has none. Fields
// promoted from an embedded struct (TelemetrySettings embeds
// config.V1TelemetryConfig) are found through structFieldByJSONName, which
// follows encoding/json's embedding rule.
func sectionModelType(section string) reflect.Type {
	sec := opsettings.SectionByName(section)
	if sec == nil || sec.New == nil {
		return nil
	}
	t, ok := structTypeOf(reflect.TypeOf(sec.New()))
	if !ok {
		return nil
	}
	return t
}

// dropInvalidCarriedNestedKeys is dropInvalidCarriedKeys inside the
// objects of a deep-merge section: nested keys the body did not send whose
// value fails the section schema are removed, so one bad stored leaf (for
// example telemetry.cloud.batch.max_size) costs only that leaf, not the
// object holding it. It walks both the objects the body merged key by key
// (tree) and the stored objects the body left out. It returns the removed
// paths ("<section>.<key>.<nested key>"), sorted. Each carried key is
// checked on its own, as a document holding only that key inside its
// parent objects. Top-level keys are left to dropInvalidCarriedKeys, which
// runs after this.
func dropInvalidCarriedNestedKeys(section string, doc map[string]json.RawMessage, tree sentTree, validate func(json.RawMessage) bool) []string {
	if whole, err := json.Marshal(doc); err != nil || validate(whole) {
		return nil
	}
	var dropped []string
	for key := range doc {
		sub, sent := tree[key]
		if sent && sub == nil {
			continue // applied whole as sent; the write's validation decides
		}
		if sub == nil {
			sub = sentTree{}
		}
		dropped = append(dropped, dropInvalidCarriedIn(section, doc, []string{key}, sub, validate)...)
	}
	sort.Strings(dropped)
	return dropped
}

// dropInvalidCarriedIn checks the object at path (relative to the section
// document) inside parent, whose last element names it, for
// dropInvalidCarriedNestedKeys. sent holds the keys the body sent inside
// it. A carried key whose value fails is first checked key by key when it
// is an object itself; it is removed whole only if it still fails.
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
	for k := range obj {
		sub, wasSent := sent[k]
		childPath := append(append([]string{}, path...), k)
		if sub != nil {
			dropped = append(dropped, dropInvalidCarriedIn(section, obj, childPath, sub, validate)...)
			continue
		}
		if wasSent {
			continue
		}
		if validate(wrapAtPath(path, map[string]json.RawMessage{k: obj[k]})) {
			continue
		}
		inner := dropInvalidCarriedIn(section, obj, childPath, sentTree{}, validate)
		if _, ok := obj[k]; ok && validate(wrapAtPath(path, map[string]json.RawMessage{k: obj[k]})) {
			dropped = append(dropped, inner...)
			continue
		}
		delete(obj, k)
		dropped = append(dropped, section+"."+strings.Join(childPath, "."))
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
// path is one of the section's registered koanf paths. The few keys that
// follow neither rule are listed in sectionKeyKoanfPathExtra.
func sectionKeyKoanfPath(section, key string) string {
	if p := opsettings.KoanfPathFromSectionKey(section, key); p != "" {
		return p
	}
	if p := sectionKeyKoanfPathExtra[section][key]; p != "" {
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

// sectionKeyKoanfPathExtra maps the section keys whose koanf path follows
// neither rule of sectionKeyKoanfPath and that opsettings does not map
// (KoanfPathFromSectionKey): notifications stores notification_channels
// at server.notification_channels.
var sectionKeyKoanfPathExtra = map[string]map[string]string{
	"notifications": {"notification_channels": "server.notification_channels"},
}

// dropEnvOverriddenSectionKeys removes the stored keys of a section whose
// koanf key is overridden by a node-local env var. In a deep-merge section
// an env var can pin a nested key (SCION_SERVER_TELEMETRY_CLOUD_ENABLED
// pins telemetry.cloud.enabled); only that nested key is removed, and the
// object holding it keeps its other keys. This holds for an entry of a
// free-form map too: a pin on telemetry.cloud.headers.<name> removes only
// that header, although a sent map replaces the stored one whole. A map
// key that contains a dot cannot be named by a koanf path, so it is never
// removed this way.
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

// bodyMergedSections are the server-config sections whose save is merged
// on the current row by mergeSectionOnCurrent with the presence of their
// keys in the PUT body taken from sectionBodyPresence (ptone/scion#3720).
// github_app, telemetry and agent_defaults are merged too, with presence
// functions of their own (githubAppPresence, telemetryPresence,
// agentDefaultsPresence). gcp_iam keeps its own builder (buildGCPIAMDoc).
//
// The map sections (runtimes, profiles, harness_configs) are not merged:
// each is one free-form map that the Server Config editor always sends in
// full, so a sent map replaces the stored one whole, and an explicit null
// or {} clears it. An omitted map section is not written.
var bodyMergedSections = []string{
	"access", "endpoints", "lifecycle", "notifications", "federation",
	"auto_expose_ports", "quotas", "agent_secrets",
}

// bodyPathForKoanf returns the JSON path in a server-config PUT body
// (ServerConfigUpdateRequest) of a Layer-1 koanf key. The body follows the
// koanf layout, except that federation is a top-level member of the
// request (ServerConfigUpdateRequest.Federation), not server.federation.
func bodyPathForKoanf(koanfKey string) []string {
	if rest, ok := strings.CutPrefix(koanfKey, "server.federation."); ok {
		return []string{"federation", rest}
	}
	return strings.Split(koanfKey, ".")
}

// sentAtPath returns the raw value a JSON object sends at path, matching
// each member to a field of t (and then of the field's struct type) with
// sentStructFields, the rule the typed request decode follows. ok is false
// when a member along the path is not sent, or a parent is not an object.
func sentAtPath(t reflect.Type, raw json.RawMessage, path []string) (json.RawMessage, bool) {
	for i, name := range path {
		fields, ok := sentStructFields(t, raw)
		if !ok {
			return nil, false
		}
		var found *sentField
		for j := range fields {
			if jsonFieldName(fields[j].field) == name {
				found = &fields[j]
				break
			}
		}
		if found == nil {
			return nil, false
		}
		if i == len(path)-1 {
			return found.val, true
		}
		st, ok := structTypeOf(found.field.Type)
		if !ok {
			return nil, false
		}
		t, raw = st, found.val
	}
	return nil, false
}

// sectionJSONKeys returns the section-level JSON keys a section models
// (the fields of its document type with a koanf path), sorted.
func sectionJSONKeys(section string) []string {
	model := sectionModelType(section)
	if model == nil {
		return nil
	}
	var keys []string
	for _, f := range reflect.VisibleFields(model) {
		if !f.IsExported() || f.Anonymous {
			continue
		}
		k := jsonFieldName(f)
		if k == "-" || sectionKeyKoanfPath(section, k) == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sectionBodyPresence returns the presence of a section's keys in a
// server-config PUT body, keyed by section-level JSON key: each key the
// section models is looked up at the body path of its koanf key
// (bodyPathForKoanf), for example access admin_emails at
// server.hub.admin_emails. A key sent as null or as an empty value is
// present. It returns an empty presence (never nil) when the body sends
// none of the keys.
func sectionBodyPresence(section string, rawBody []byte) *fieldPresence {
	out := &fieldPresence{raw: map[string]json.RawMessage{}}
	reqType := reflect.TypeOf(ServerConfigUpdateRequest{})
	for _, key := range sectionJSONKeys(section) {
		p := sectionKeyKoanfPath(section, key)
		if v, ok := sentAtPath(reqType, rawBody, bodyPathForKoanf(p)); ok {
			out.raw[key] = v
		}
	}
	return out
}

// structToRawMap returns the JSON object v encodes to, member by member
// (nil when v does not encode to an object).
func structToRawMap(v any) map[string]json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// accessNoRowBase is the access base when no access row exists: the
// effective access values of the snapshot (bootstrap or file).
func accessNoRowBase(ops *OperationalSettings) map[string]json.RawMessage {
	snap := ops.Snapshot()
	return structToRawMap(opsettings.AccessSettings{
		AdminEmails:       snap.AdminEmails,
		UserAccessMode:    snap.UserAccessMode,
		DefaultUserRole:   snap.DefaultUserRole,
		AuthorizedDomains: snap.AuthorizedDomains,
	})
}

// endpointsNoRowBase is the endpoints base when no endpoints row exists:
// the effective public_url and image_registry. hub_name is left out: the
// bootstrap value applies without being written (bootstrapAppliesWhenAbsent).
func endpointsNoRowBase(ops *OperationalSettings) map[string]json.RawMessage {
	snap := ops.Snapshot()
	return structToRawMap(opsettings.EndpointsSettings{
		PublicURL:     snap.PublicURL,
		ImageRegistry: snap.ImageRegistry,
	})
}

// endpointsSeededBase drops hub_name from a non-managed endpoints base: a
// seeded row holds the bootstrap hub_name, which applies without being
// written (Snapshot falls back to it) and may not match the schema
// pattern, so it is not carried into the managed row.
func endpointsSeededBase(base map[string]json.RawMessage) {
	delete(base, "hub_name")
}

// bodyMergeOptions returns the base rules of a section in
// bodyMergedSections.
func bodyMergeOptions(section string) sectionMergeOptions {
	switch section {
	case "access":
		return sectionMergeOptions{noRowBase: accessNoRowBase}
	case "endpoints":
		return sectionMergeOptions{noRowBase: endpointsNoRowBase, seededBase: endpointsSeededBase}
	}
	return sectionMergeOptions{}
}

// unchangedManagedRow reports whether writing doc to section would leave
// its row as it is: the row exists, is managed, holds the same JSON value
// (compared as decoded JSON, so key order and spacing do not matter) and,
// when expectedRev is not -1, is at that revision. The server-config PUT
// skips such a write, so a save leaves the rows of the sections it does
// not change byte-identical, with their revision and audit fields. A
// seeded row is still written, which adopts it as managed, as before.
func unchangedManagedRow(ctx context.Context, ops *OperationalSettings, section string, doc json.RawMessage, expectedRev int64) (int64, bool) {
	row, err := ops.store.GetHubSetting(ctx, section)
	if err != nil || row.Origin != "managed" {
		return 0, false
	}
	if expectedRev >= 0 && row.Revision != expectedRev {
		return 0, false
	}
	var cur, next any
	if json.Unmarshal(row.Value, &cur) != nil || json.Unmarshal(doc, &next) != nil {
		return 0, false
	}
	if !reflect.DeepEqual(cur, next) {
		return 0, false
	}
	return row.Revision, true
}
