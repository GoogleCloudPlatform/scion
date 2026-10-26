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
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
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
	// Skipped lists the changed keys the running hub could not use, which
	// are therefore not in Applied (ptone/scion#4466).
	Skipped []skippedKey
}

// skippedKey is a changed key that was saved but is not in use, with a
// reason. The reason names no configured value.
type skippedKey struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

const notificationChannelsKey = "server.notification_channels"

// reportSavedKeys returns the changed keys of the written sections. before
// holds each section's effective document before the save (the stored row,
// or the bootstrap value when there was none); after holds the documents
// written.
//
// The report is built from the documents; withSkipped then moves the keys
// that ApplySnapshot skipped out of Applied.
func reportSavedKeys(before, after map[string]json.RawMessage) savedKeysReport {
	r := savedKeysReport{Applied: []string{}, PendingRestart: []string{}, Skipped: []skippedKey{}}
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

// withSkipped moves the applied keys that the last ApplySnapshot skipped
// into Skipped: server.notification_channels when a channel failed
// validation, and the federation keys when the federation configuration
// was not installed (ptone/scion#4466). PendingRestart is unchanged.
func (r savedKeysReport) withSkipped(skips layer1ApplySkips) savedKeysReport {
	applied := make([]string, 0, len(r.Applied))
	skipped := append([]skippedKey{}, r.Skipped...)
	for _, key := range r.Applied {
		switch {
		case key == notificationChannelsKey && len(skips.SkippedChannels) > 0:
			skipped = append(skipped, skippedKey{Key: key, Reason: skippedChannelsReason(skips)})
		case strings.HasPrefix(key, "server.federation.") && skips.FederationReason != "":
			skipped = append(skipped, skippedKey{Key: key, Reason: skips.FederationReason})
		default:
			applied = append(applied, key)
		}
	}
	r.Applied = applied
	r.Skipped = skipped
	return r
}

// notYetAppliedReason is the skipped reason of a key whose section was
// saved but not included in the saving hub's self-apply, because the
// re-read of the store after the write failed or kept losing to newer
// writes (ptone/scion#5691).
const notYetAppliedReason = "Saved; this Hub applies it on its next settings refresh."

// withNotYetApplied merges the report of the sections that were saved but
// not included in the self-apply: their changed keys are listed in
// Skipped with notYetAppliedReason rather than in Applied, and their
// restart-required keys stay in PendingRestart (ptone/scion#5691).
func (r savedKeysReport) withNotYetApplied(pending savedKeysReport) savedKeysReport {
	skipped := append([]skippedKey{}, r.Skipped...)
	for _, key := range pending.Applied {
		skipped = append(skipped, skippedKey{Key: key, Reason: notYetAppliedReason})
	}
	r.Skipped = skipped
	r.PendingRestart = append(append([]string{}, r.PendingRestart...), pending.PendingRestart...)
	sort.Strings(r.PendingRestart)
	return r
}

// skippedChannelsReason names the skipped channels by their 1-based
// position in the list, never by a configured value.
func skippedChannelsReason(skips layer1ApplySkips) string {
	pos := make([]string, len(skips.SkippedChannels))
	for i, idx := range skips.SkippedChannels {
		pos[i] = strconv.Itoa(idx + 1)
	}
	others := " The other channels are in use."
	if len(pos) >= skips.ChannelCount {
		others = " No notification channel is in use."
	}
	if len(pos) == 1 {
		return fmt.Sprintf("Notification channel %s of %d failed validation and is not in use.%s",
			pos[0], skips.ChannelCount, others)
	}
	return fmt.Sprintf("Notification channels %s of %d failed validation and are not in use.%s",
		strings.Join(pos, ", "), skips.ChannelCount, others)
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
