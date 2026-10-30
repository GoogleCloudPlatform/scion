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
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Runtime entries are merged key by key on save (ptone/scion#6580). The
// runtimes map still decides which entries exist: an entry the body leaves
// out is deleted, and an entry sent as null is deleted too. Within an entry
// the body sends:
//
//   - a key the body sends takes the value it decodes to (a runtime setting
//     V1RuntimeConfig models);
//   - a key sent as null, or as a zero value the typed encoding leaves out
//     ("", false, {}, []), is cleared;
//   - a stored key the body does not send is kept.
//
// A stored key V1RuntimeConfig does not model (for example
// runtimes.NAME.resources, which nothing reads) is kept exactly as stored,
// and named in a save warning. Before this, every save re-encoded each
// entry through V1RuntimeConfig and so dropped such keys silently. Such a
// stored key can only come from outside this API (a hand-edited
// settings.yaml, or a row written by another build): the PUT refuses a
// body that sends one, null included (rejectUnpersistedKeys,
// rejectUnknownFileConfigKeys), so the merge ignores a sent one.
//
// A stored key that matches a runtime setting only when case is ignored
// (for example Namespace for namespace, from a hand-edited settings.yaml)
// is kept as stored and named in a save warning; a sent value is written
// under the canonical name beside it.
//
// The removed per-runtime storage overrides (removedRuntimeStorageKeys)
// are the exception: a stored one the body does not send is dropped, with
// a warning, as the Server Config page always did by stripping them on
// load. A sent one is still rejected by validation.

// removedRuntimeStorageKeys are the per-runtime storage override keys
// config.RemovedStorageOverrideErrors rejects.
var removedRuntimeStorageKeys = map[string]bool{
	"shared_dir_storage_backend":  true,
	"shared_dir_storage_backends": true,
	"home_storage_backend":        true,
	"home_storage_leaf":           true,
}

// mergeRuntimeEntries merges the runtime entries of a save onto the stored
// entries (see the rules above). stored maps entry names to stored entry
// objects (a stored entry that is not an object counts as empty); sent is
// the typed runtimes map of the request; sentRaw is the raw JSON runtimes
// object of the body, whose members decide which keys were sent. encode
// turns a typed entry into the value form of stored (JSON values for a
// database row, YAML values for settings.yaml). It returns the merged
// entries and one warning per unmodeled key kept or removed-storage key
// dropped, sorted.
func mergeRuntimeEntries(stored map[string]map[string]any, sent map[string]config.V1RuntimeConfig, sentRaw map[string]json.RawMessage, encode func(config.V1RuntimeConfig) map[string]any) (map[string]map[string]any, []string) {
	if sentRaw == nil {
		// No body (internal callers): every key the typed entries encode
		// counts as sent.
		if b, err := json.Marshal(sent); err == nil {
			_ = json.Unmarshal(b, &sentRaw)
		}
	}
	rtType := reflect.TypeOf(config.V1RuntimeConfig{})
	out := make(map[string]map[string]any, len(sent))
	var warnings []string
	for name, entry := range sent {
		raw, ok := sentRaw[name]
		if ok && isJSONNull(raw) {
			continue // a null entry deletes it
		}
		var sentKeys map[string]json.RawMessage
		if ok {
			_ = json.Unmarshal(raw, &sentKeys)
		}
		merged := map[string]any{}
		for k, v := range stored[name] {
			merged[k] = v
		}
		encoded := encode(entry)
		for key := range sentKeys {
			f, modeled := structFieldByJSONName(rtType, key)
			if !modeled {
				continue
			}
			canonical := jsonFieldName(f)
			if v, has := encoded[canonical]; has {
				merged[canonical] = v
			} else {
				delete(merged, canonical)
			}
		}
		for key := range merged {
			if f, modeled := structFieldByJSONName(rtType, key); modeled {
				if canonical := jsonFieldName(f); canonical != key {
					// A stored spelling that only matches a setting when case
					// is ignored (a hand-edited file): kept as stored, since
					// renaming it could change which value applies.
					warnings = append(warnings, fmt.Sprintf("runtimes.%s.%s is not spelled as the runtime setting %s; it is kept as stored and may be ignored; rename it to %s", name, key, canonical, canonical))
					continue
				}
				if removedRuntimeStorageKeys[key] && !sentFoldHas(sentKeys, key) {
					delete(merged, key)
					warnings = append(warnings, fmt.Sprintf("runtimes.%s.%s is no longer supported and was removed from the stored entry", name, key))
				}
				continue
			}
			warnings = append(warnings, fmt.Sprintf("runtimes.%s.%s is not a runtime setting and has no effect; it is kept as stored", name, key))
		}
		out[name] = merged
	}
	sort.Strings(warnings)
	return out, warnings
}

// sentFoldHas reports whether keys has key, matched as the typed request
// decode matches members to fields (exact, else case-insensitive).
func sentFoldHas(keys map[string]json.RawMessage, key string) bool {
	_, ok := (&fieldPresence{raw: keys}).sentFold(key)
	return ok
}

// sentRuntimesObject returns the members of the runtimes object of a
// server-config PUT body, matched case-insensitively as the typed decode
// matches it, or nil when the body sends no runtimes object.
func sentRuntimesObject(rawBody []byte) map[string]json.RawMessage {
	fp, err := parseFieldPresence(rawBody)
	if err != nil {
		return nil
	}
	raw, ok := fp.sentFold("runtimes")
	if !ok {
		return nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// mergeRuntimesRow merges a save's runtimes onto the stored runtimes row
// value (database mode) and returns the JSON document to write.
func mergeRuntimesRow(row []byte, sent map[string]config.V1RuntimeConfig, sentRaw map[string]json.RawMessage) (json.RawMessage, []string, error) {
	stored := map[string]map[string]any{}
	if len(bytes.TrimSpace(row)) > 0 {
		var entries map[string]json.RawMessage
		if err := decodeJSONNumbers(row, &entries); err != nil {
			return nil, nil, fmt.Errorf("decoding current runtimes row: %w", err)
		}
		for name, e := range entries {
			var obj map[string]any
			if decodeJSONNumbers(e, &obj) == nil && obj != nil {
				stored[name] = obj
			}
		}
	}
	merged, warnings := mergeRuntimeEntries(stored, sent, sentRaw, func(rt config.V1RuntimeConfig) map[string]any {
		var m map[string]any
		b, err := json.Marshal(rt)
		if err == nil {
			_ = decodeJSONNumbers(b, &m)
		}
		return m
	})
	doc, err := json.Marshal(merged)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding runtimes doc: %w", err)
	}
	return doc, warnings, nil
}

// mergeRuntimesFile merges a save's runtimes onto the runtimes value of
// settings.yaml (file mode), as decoded by yaml.v3, and returns the value
// to store.
func mergeRuntimesFile(current any, sent map[string]config.V1RuntimeConfig, sentRaw map[string]json.RawMessage) (map[string]any, []string) {
	stored := map[string]map[string]any{}
	if m, ok := current.(map[string]any); ok {
		for name, e := range m {
			if obj, ok := e.(map[string]any); ok {
				stored[name] = obj
			}
		}
	}
	merged, warnings := mergeRuntimeEntries(stored, sent, sentRaw, func(rt config.V1RuntimeConfig) map[string]any {
		m, _ := marshalToMap(rt).(map[string]any)
		return m
	})
	out := make(map[string]any, len(merged))
	for name, e := range merged {
		out[name] = e
	}
	return out, warnings
}

// decodeJSONNumbers decodes data into v, keeping numbers as json.Number so
// stored values are written back exactly.
func decodeJSONNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	return dec.Decode(v)
}

// writeResourceQuantityError answers a save whose resource quantities
// fail config.ValidateResourceQuantities: the message is the first error,
// and details carry the field path of the first error and every error.
func writeResourceQuantityError(w http.ResponseWriter, status int, code string, errs []config.ValidationError) {
	all := make([]string, 0, len(errs))
	for _, e := range errs {
		all = append(all, e.Error())
	}
	writeError(w, status, code, errs[0].Error(), map[string]interface{}{
		"field":  errs[0].Path,
		"errors": all,
	})
}

// resourceQuantityWarnings returns the save warnings for the warnings of
// config.ValidateResourceQuantities.
func resourceQuantityWarnings(warnings []config.ValidationError) []string {
	out := make([]string, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, w.Error())
	}
	return out
}
