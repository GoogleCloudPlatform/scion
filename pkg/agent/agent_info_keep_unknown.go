// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// marshalAgentInfoKeepUnknown encodes info for a read-modify-write of
// agent-info.json whose previous content was orig, keeping every top-level
// key of orig that api.AgentInfo does not know (ptone/scion#4591).
//
// agent-info.json has more than one writer: sciontool's StatusHandler in the
// container writes keys such as toolName and activitySource that
// api.AgentInfo has no field for, and a newer writer may add more. A plain
// json.MarshalIndent(info) would drop them.
//
// The known keys are encoded exactly as json.MarshalIndent(info, "", "  ")
// encodes them, so a known key the update cleared is removed as before.
// When orig has no unknown keys, or is not a JSON object (missing, empty,
// null, corrupt), the result is byte-for-byte json.MarshalIndent(info, "",
// "  "). Otherwise the unknown keys follow the known ones in sorted order,
// with their values as read (re-indented, not re-escaped). A key that
// matches a known name case-insensitively counts as known, since
// json.Unmarshal already read it into the struct.
func marshalAgentInfoKeepUnknown(orig []byte, info *api.AgentInfo) ([]byte, error) {
	plain, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(orig, &raw); err != nil || len(raw) == 0 {
		return plain, nil
	}
	var unknown []string
	for k := range raw {
		if !isKnownAgentInfoKey(k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return plain, nil
	}
	sort.Strings(unknown)

	compact, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.Write(compact[:len(compact)-1]) // drop the closing '}'
	needComma := len(compact) > 2       // "{}" has no fields
	for _, k := range unknown {
		key, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		if needComma {
			buf.WriteByte(',')
		}
		needComma = true
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(raw[k])
	}
	buf.WriteByte('}')

	var out bytes.Buffer
	if err := json.Indent(&out, buf.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

var (
	agentInfoKnownKeysOnce sync.Once
	agentInfoKnownKeys     []string
)

// isKnownAgentInfoKey reports whether json.Unmarshal into api.AgentInfo
// would read key into a field (field names match case-insensitively).
func isKnownAgentInfoKey(key string) bool {
	agentInfoKnownKeysOnce.Do(func() {
		agentInfoKnownKeys = jsonFieldNames(reflect.TypeOf(api.AgentInfo{}))
	})
	for _, k := range agentInfoKnownKeys {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// jsonFieldNames returns the JSON object keys encoding/json uses for struct
// type t, following untagged embedded structs.
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				names = append(names, jsonFieldNames(ft)...)
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		names = append(names, name)
	}
	return names
}
