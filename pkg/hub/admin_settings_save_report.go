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
	"encoding/json"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// savedKeysReport is the key-level result of a server-config save: the
// Layer-1 keys whose value changed, split into those applied to the running
// hub and those that take effect after a restart (ptone/scion#3904,
// ptone/scion#3722).
type savedKeysReport struct {
	Applied        []string
	PendingRestart []string
}

// reportSavedKeys returns the changed keys of the written sections. before
// holds each section's effective document before the save (the stored row,
// or the bootstrap value when there was none); after holds the documents
// written.
//
// The report is built from the documents, not from what ApplySnapshot did:
// a notification channel that fails validation, or a federation issuer
// that cannot be built, is skipped when applied but still listed here
// (ptone/scion#4466).
func reportSavedKeys(before, after map[string]json.RawMessage) savedKeysReport {
	r := savedKeysReport{Applied: []string{}, PendingRestart: []string{}}
	for section, doc := range after {
		for _, key := range changedSectionKeys(section, before[section], doc) {
			if !keyTakesEffect(section, key, doc) {
				continue
			}
			if opsettings.IsRestartRequired(key) {
				r.PendingRestart = append(r.PendingRestart, key)
			} else {
				r.Applied = append(r.Applied, key)
			}
		}
	}
	sort.Strings(r.Applied)
	sort.Strings(r.PendingRestart)
	return r
}

// keyTakesEffect reports whether a changed key of the written section
// document is used by the running hub, so it can be reported at all.
// ApplySnapshot ignores every github_app field other than app_id while the
// section has no app_id, so those fields are listed under neither heading.
func keyTakesEffect(section, key string, doc json.RawMessage) bool {
	if section != "github_app" || key == "server.github_app.app_id" {
		return true
	}
	var gh opsettings.GitHubAppSettings
	if err := json.Unmarshal(docOrEmpty(doc), &gh); err != nil {
		return true
	}
	return gh.AppID != 0
}

// changedSectionKeys returns the koanf paths of section whose value differs
// between the before and after documents. When a registered path and a path
// nested under it both changed, only the nested one is listed. A section
// with no koanf paths is reported by its name when its document changed.
func changedSectionKeys(section string, before, after json.RawMessage) []string {
	sec := opsettings.SectionByName(section)
	if sec == nil {
		return nil
	}
	if len(sec.KoanfPaths) == 0 {
		if !jsonDocsEqual(before, after) {
			return []string{section}
		}
		return nil
	}
	kb, errB := opsettings.LoadSectionsIntoKoanf(map[string]json.RawMessage{section: docOrEmpty(before)})
	ka, errA := opsettings.LoadSectionsIntoKoanf(map[string]json.RawMessage{section: docOrEmpty(after)})
	if errB != nil || errA != nil {
		slog.Warn("server-config save: cannot compare section keys; reporting the section as changed",
			"section", section, "before_error", errB, "after_error", errA)
		return append([]string(nil), sec.KoanfPaths...)
	}
	var changed []string
	for _, p := range sec.KoanfPaths {
		if !reflect.DeepEqual(kb.Get(p), ka.Get(p)) {
			changed = append(changed, p)
		}
	}
	// Drop a path when a more specific registered path under it changed
	// (telemetry.cloud when telemetry.cloud.endpoint changed).
	out := changed[:0]
	for _, p := range changed {
		nested := false
		for _, q := range changed {
			if strings.HasPrefix(q, p+".") {
				nested = true
				break
			}
		}
		if !nested {
			out = append(out, p)
		}
	}
	return out
}

func docOrEmpty(doc json.RawMessage) json.RawMessage {
	if len(doc) == 0 {
		return json.RawMessage(`{}`)
	}
	return doc
}

// jsonDocsEqual reports whether two JSON documents hold the same value. A
// missing document equals {}.
func jsonDocsEqual(a, b json.RawMessage) bool {
	var va, vb interface{}
	if json.Unmarshal(docOrEmpty(a), &va) != nil || json.Unmarshal(docOrEmpty(b), &vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
