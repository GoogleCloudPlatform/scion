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
	"reflect"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	yamlv3 "gopkg.in/yaml.v3"
)

// rawServerObject returns the server member of a PUT body, or nil when
// the body has none, or it is not a JSON object. The top-level key is
// resolved by sentStructFields over ServerConfigUpdateRequest, the same
// rule the request decode and the server sections use, so "Server" or
// "SERVER" select the same value as "server".
func rawServerObject(rawBody []byte) json.RawMessage {
	sent, ok := sentStructFields(reflect.TypeOf(ServerConfigUpdateRequest{}), rawBody)
	if !ok {
		return nil
	}
	for _, sf := range sent {
		if sf.field.Name == "Server" && isJSONObject(sf.val) {
			return sf.val
		}
	}
	return nil
}

// mergeServerSettings deep-merges a server config update into the server
// section of the raw settings map (file-mode PUT).
//
// Every struct-typed server section (github_app, database, auth, broker,
// secrets, hub, and so on, at any depth) is merged field by field, so a
// field the request leaves out keeps its stored value. This matters
// because clients send only the fields they edit or show: dropping the
// rest would remove credentials such as the GitHub App private key or the
// database URL from settings.yaml.
//
// Clearing a field: send it explicitly as null or as its zero value ("",
// 0, false, [] or {} for a list or map). null also removes a whole
// section. This is the same convention as the top-level settings
// (setOrDeleteString) and the DB-mode PUT. Lists (notification_channels)
// and maps with free-form keys are not merged: a sent value replaces the
// stored one as a whole.
//
// rawServer is the request body's "server" object and decides which
// fields were sent; values come from incoming, which has already been
// validated, normalized and had masked placeholders restored. A nil
// rawServer is derived from incoming, so its zero-valued fields count as
// omitted.
func mergeServerSettings(raw map[string]interface{}, incoming *config.V1ServerConfig, rawServer json.RawMessage) {
	if incoming == nil {
		return
	}
	if len(rawServer) == 0 {
		b, err := json.Marshal(incoming)
		if err != nil {
			return
		}
		rawServer = b
	}
	sent, ok := sentStructFields(reflect.TypeOf(config.V1ServerConfig{}), rawServer)
	if !ok {
		return
	}
	typed, _ := marshalToMap(incoming).(map[string]interface{})
	existing, _ := raw["server"].(map[string]interface{})
	if existing == nil {
		existing = make(map[string]interface{})
	}
	mergeSettingsStruct(existing, reflect.TypeOf(config.V1ServerConfig{}), sent, typed)
	raw["server"] = existing
}

// sentField is a field of a struct type named by a JSON object key, with
// the value sent for it.
type sentField struct {
	field reflect.StructField
	val   json.RawMessage
}

// sentStructFields resolves the keys of the JSON object raw to the fields
// of struct type t, with the structFieldByJSONName rule (exact match,
// otherwise case-insensitive). Keys that match no field are dropped.
//
// Several keys can resolve to the same field (for example "github_app"
// and "GitHub_App"). They are combined in body order the way
// encoding/json decodes them into the request: when the earlier value and
// the later value are both JSON objects, the later object's members are
// appended to the earlier one's, so a struct field receives the members
// of both and a member sent in both takes the later value; otherwise
// (a scalar, an array or null) the later value replaces the earlier one.
// ok is false when raw is not a JSON object.
//
// rawServerObject, the merge and validateMergedServerSections all use
// this, so the top-level server key, the sections the merge writes and
// the sections the validator checks are matched by one rule.
func sentStructFields(t reflect.Type, raw json.RawMessage) (fields []sentField, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	index := make(map[int]int)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		f, found := structFieldByJSONName(t, key)
		if !found {
			continue
		}
		if i, dup := index[f.Index[0]]; dup {
			fields[i].val = combineJSONValues(fields[i].val, val)
			continue
		}
		index[f.Index[0]] = len(fields)
		fields = append(fields, sentField{field: f, val: val})
	}
	return fields, true
}

// combineJSONValues returns the value encoding/json leaves in a struct
// field after decoding prev and then next into it: the members of both
// objects in order when both are JSON objects, otherwise next.
func combineJSONValues(prev, next json.RawMessage) json.RawMessage {
	if !isJSONObject(prev) || !isJSONObject(next) {
		return next
	}
	a := bytes.TrimSpace(prev)
	b := bytes.TrimSpace(next)
	ai := bytes.TrimSpace(a[1 : len(a)-1])
	bi := bytes.TrimSpace(b[1 : len(b)-1])
	switch {
	case len(ai) == 0:
		return next
	case len(bi) == 0:
		return prev
	}
	out := make([]byte, 0, len(ai)+len(bi)+3)
	out = append(out, '{')
	out = append(out, ai...)
	out = append(out, ',')
	out = append(out, bi...)
	out = append(out, '}')
	return out
}

// isJSONObject reports whether v is a JSON object.
func isJSONObject(v json.RawMessage) bool {
	v = bytes.TrimSpace(v)
	return len(v) >= 2 && v[0] == '{' && v[len(v)-1] == '}'
}

// mergeSettingsStruct merges the sent fields of struct type t into
// existing (a YAML-decoded map keyed by yaml names). typed holds the YAML
// form of the decoded request at the same level; a sent field missing
// from it was a zero value dropped by omitempty, and is deleted.
// Unknown keys were rejected before the merge; anything left (a read-only
// echo) was dropped by sentStructFields and is not persisted.
func mergeSettingsStruct(existing map[string]interface{}, t reflect.Type, sent []sentField, typed map[string]interface{}) {
	for _, sf := range sent {
		f, val := sf.field, sf.val
		yk := yamlFieldName(f)
		val = bytes.TrimSpace(val)
		if bytes.Equal(val, []byte("null")) {
			delete(existing, yk)
			continue
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && len(val) > 0 && val[0] == '{' {
			if sub, ok := sentStructFields(ft, val); ok {
				subExisting, _ := existing[yk].(map[string]interface{})
				if subExisting == nil {
					subExisting = make(map[string]interface{})
				}
				subTyped, _ := typed[yk].(map[string]interface{})
				mergeSettingsStruct(subExisting, ft, sub, subTyped)
				if len(subExisting) > 0 {
					existing[yk] = subExisting
				} else if v, ok := typed[yk]; ok {
					existing[yk] = v
				} else {
					delete(existing, yk)
				}
				continue
			}
		}
		if v, ok := typed[yk]; ok {
			existing[yk] = v
		} else {
			delete(existing, yk)
		}
	}
}

// structFieldByJSONName returns the field of struct type t whose JSON name
// is name. Like encoding/json, it prefers an exact match and otherwise
// accepts a case-insensitive one, so a key that decoded into the request
// is also found here (the strict unknown-key check folds case the same
// way).
func structFieldByJSONName(t reflect.Type, name string) (reflect.StructField, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return reflect.StructField{}, false
	}
	var folded reflect.StructField
	foundFolded := false
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jn := strings.Split(f.Tag.Get("json"), ",")[0]
		if jn == "-" {
			continue
		}
		if jn == "" {
			jn = f.Name
		}
		if jn == name {
			return f, true
		}
		if !foundFolded && strings.EqualFold(jn, name) {
			folded, foundFolded = f, true
		}
	}
	return folded, foundFolded
}

// yamlFieldName returns the key yaml.v3 uses for f.
func yamlFieldName(f reflect.StructField) string {
	if yn := strings.Split(f.Tag.Get("yaml"), ",")[0]; yn != "" && yn != "-" {
		return yn
	}
	return strings.ToLower(f.Name)
}

// validateMergedServerSections re-runs the server checks whose result
// depends on more than one field, on the merged settings: a request that
// sends only part of home_storage or shared_dir_storage is combined with
// the stored fields by the deep merge, so the request alone does not show
// the result that will be written. Only sections the request sent are
// checked, so a save is not blocked by an unrelated stored value.
func validateMergedServerSections(raw map[string]interface{}, rawServer json.RawMessage) error {
	sent, ok := sentStructFields(reflect.TypeOf(config.V1ServerConfig{}), rawServer)
	if !ok {
		return nil
	}
	var sentHome, sentShared bool
	for _, sf := range sent {
		switch sf.field.Name {
		case "HomeStorage":
			sentHome = true
		case "SharedDirStorage":
			sentShared = true
		}
	}
	if !sentHome && !sentShared {
		return nil
	}
	merged, err := serverConfigFromRaw(raw)
	if err != nil || merged == nil {
		return err
	}
	if sentHome {
		if err := merged.HomeStorage.Validate(); err != nil {
			return err
		}
	}
	if sentShared {
		data, err := yamlv3.Marshal(map[string]interface{}{
			"runtimes": raw["runtimes"],
			"profiles": raw["profiles"],
		})
		if err != nil {
			return err
		}
		var rp struct {
			Runtimes map[string]config.V1RuntimeConfig `yaml:"runtimes"`
			Profiles map[string]config.V1ProfileConfig `yaml:"profiles"`
		}
		if err := yamlv3.Unmarshal(data, &rp); err != nil {
			return err
		}
		if errs := config.ValidateSharedDirStorageBackends(rp.Runtimes, rp.Profiles, merged.SharedDirStorage); len(errs) > 0 {
			return errs[0]
		}
	}
	return nil
}
