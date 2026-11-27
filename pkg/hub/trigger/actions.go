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

package trigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ErrCorruptTriggerActions is returned when a stored trigger's actions column
// is unparseable or yields no valid action elements (fail-closed read
// normalization, action-arrays-design §1).
var ErrCorruptTriggerActions = errors.New("trigger: stored actions data is corrupt")

// Actions normalizes a stored Trigger's action list on read (action-arrays §1).
// Normalization fails closed with ErrCorruptTriggerActions whenever
// ActionsCorrupt is set, an element has an empty type or non-object payload,
// or neither actions nor legacy scalar action_type is populated. Otherwise
// non-empty tr.Actions wins; else legacy scalar (ActionType, ActionPayload)
// normalizes to a one-element list.
func Actions(tr store.Trigger) ([]store.TriggerAction, error) {
	if tr.ActionsCorrupt {
		return nil, ErrCorruptTriggerActions
	}
	if len(tr.Actions) > 0 {
		out := make([]store.TriggerAction, len(tr.Actions))
		for i, a := range tr.Actions {
			t := strings.TrimSpace(a.Type)
			if t == "" {
				return nil, ErrCorruptTriggerActions
			}
			payload := strings.TrimSpace(a.Payload)
			if payload == "" {
				payload = "{}"
			}
			if !isJSONObject(payload) {
				return nil, ErrCorruptTriggerActions
			}
			out[i] = store.TriggerAction{
				Type:    t,
				Payload: payload,
			}
		}
		return out, nil
	}
	if t := strings.TrimSpace(tr.ActionType); t != "" {
		payload := strings.TrimSpace(tr.ActionPayload)
		if payload == "" {
			payload = "{}"
		}
		if !isJSONObject(payload) {
			return nil, ErrCorruptTriggerActions
		}
		return []store.TriggerAction{{
			Type:    t,
			Payload: payload,
		}}, nil
	}
	return nil, ErrCorruptTriggerActions
}

func isJSONObject(raw string) bool {
	b := bytes.TrimSpace([]byte(raw))
	return len(b) > 0 && b[0] == '{' && json.Valid(b)
}
