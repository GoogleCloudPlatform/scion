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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The runtime-profile timezone (profiles.<name>.timezone) was removed. Agent
// container TZ comes from the agent's pinned timezone, a storage-scope TZ
// environment variable, then agent_defaults.default_timezone, then UTC. This
// file holds the two places that still know the old key exists: the admin
// PUT rejection, and the one-time startup step that retires stored values.

// profileTimezoneRetireUpdatedBy is the hub_settings updated_by value for
// writes made by the retirement step.
const profileTimezoneRetireUpdatedBy = "system:profile-timezone-retire"

// profileTimezoneRetireAttempts bounds reruns after a revision conflict
// (another hub replica writing the same section at the same time).
const profileTimezoneRetireAttempts = 3

// removedProfileTimezoneMessage is the 422 text for a request that still
// sends profiles.<name>.timezone.
func removedProfileTimezoneMessage(profile string) string {
	return fmt.Sprintf("profiles.%s.timezone was removed; set agent_defaults.default_timezone, or a hub/broker-scope TZ environment variable", profile)
}

// removedProfileTimezoneInBody returns the first profile name (in sorted
// order) whose object in the request body's "profiles" map has a "timezone"
// key, whatever its value (including "" and null).
func removedProfileTimezoneInBody(rawBody []byte) (string, bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &top); err != nil {
		return "", false
	}
	profilesRaw, ok := top["profiles"]
	if !ok {
		return "", false
	}
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(profilesRaw, &profiles); err != nil {
		return "", false
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(profiles[name], &fields); err != nil {
			continue
		}
		if _, ok := fields[config.LegacyProfileTimezoneKey]; ok {
			return name, true
		}
	}
	return "", false
}

// rejectRemovedProfileTimezone writes a 422 and returns true when the raw
// admin server-config body sets profiles.<name>.timezone. Both PUT handlers
// call it: their typed decode would otherwise drop the key silently and an
// old client would believe it had saved a zone.
func rejectRemovedProfileTimezone(w http.ResponseWriter, rawBody []byte) bool {
	name, ok := removedProfileTimezoneInBody(rawBody)
	if !ok {
		return false
	}
	writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError, removedProfileTimezoneMessage(name), nil)
	return true
}

// ProfileTimezoneRetireInput describes where the retirement step looks.
type ProfileTimezoneRetireInput struct {
	// DBTier is true when hub_settings is authoritative (postgres). The
	// step then migrates the stored rows. Otherwise settings.yaml is
	// authoritative and the step only logs.
	DBTier bool
	// File is the raw scan of the global settings file. On the DB tier its
	// profile values count only while the hub_settings profiles row is
	// seeded from that file (the seed extraction no longer carries them).
	File config.SettingsFileProfileTimezoneScan
}

// profileTZDecision is the outcome of the design's five-case table for one
// set of legacy values.
type profileTZDecision struct {
	// copyZone is the value to copy into default_timezone, or "".
	copyZone string
	// action describes what happens to one legacy value.
	action func(value string) string
}

// decideProfileTimezones applies the table. def is the current
// default_timezone; zones is every legacy value (empty ones included).
func decideProfileTimezones(def string, zones []string) profileTZDecision {
	distinct := map[string]bool{}
	for _, z := range zones {
		if z != "" {
			distinct[z] = true
		}
	}
	var only string
	for z := range distinct {
		only = z
	}

	switch {
	case def == "" && len(distinct) == 1 && validateDefaultTimezone(only) == nil:
		return profileTZDecision{copyZone: only, action: func(v string) string {
			if v == "" {
				return "empty value removed"
			}
			return "copied into agent_defaults.default_timezone"
		}}
	case def == "" && len(distinct) == 1:
		return profileTZDecision{action: func(v string) string {
			if v == "" {
				return "empty value removed"
			}
			return "dropped: not a valid IANA time zone name, so it was not copied into agent_defaults.default_timezone; agents on this profile move to UTC"
		}}
	case def == "" && len(distinct) > 1:
		return profileTZDecision{action: func(v string) string {
			if v == "" {
				return "empty value removed"
			}
			return "dropped: profiles set different zones and agent_defaults.default_timezone is empty, so nothing was copied; agents on this profile move to UTC (use broker- or project-scope TZ environment variables instead)"
		}}
	default:
		return profileTZDecision{action: func(v string) string {
			switch v {
			case "":
				return "empty value removed"
			case def:
				return "matches agent_defaults.default_timezone; nothing to copy"
			default:
				return fmt.Sprintf("dropped: agent_defaults.default_timezone %q is kept; agents on this profile move to it", def)
			}
		}}
	}
}

// RetireProfileTimezones is the one-time step that retires stored
// runtime-profile timezone values. The hub runs it at every start, after
// operational settings are loaded and seeded and before the agent
// dispatcher is wired; once the values are gone it finds nothing to do.
//
// DB tier: the five-case table decides whether the single unambiguous zone
// is copied into agent_defaults.default_timezone, then every
// profiles.<name>.timezone key is stripped from the hub_settings profiles
// row. Writes go through OperationalSettings.Update with each section's
// current revision and updatedBy profileTimezoneRetireUpdatedBy.
// agent_defaults is written before profiles, so a crash between the two
// writes reruns as "zone equals default": the rerun only strips.
//
// File tier: settings.yaml is operator-owned and is not rewritten. The step
// logs each value, and the line to add when the table would have copied it.
func RetireProfileTimezones(ctx context.Context, ops *OperationalSettings, in ProfileTimezoneRetireInput, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if !in.DBTier || ops == nil {
		logFileTierProfileTimezones(in.File, log)
		return nil
	}
	var err error
	for attempt := 0; attempt < profileTimezoneRetireAttempts; attempt++ {
		err = retireDBTierProfileTimezones(ctx, ops, in.File, log)
		if !errors.Is(err, store.ErrRevisionConflict) {
			return err
		}
		log.Info("profile timezone retirement: settings changed concurrently; rereading", "attempt", attempt+1)
	}
	return err
}

// logFileTierProfileTimezones logs one warning per profiles.<name>.timezone
// key in the settings file. The value has no effect.
func logFileTierProfileTimezones(scan config.SettingsFileProfileTimezoneScan, log *slog.Logger) {
	if len(scan.ProfileTimezones) == 0 {
		return
	}
	zones := make([]string, 0, len(scan.ProfileTimezones))
	for _, f := range scan.ProfileTimezones {
		zones = append(zones, f.Timezone)
	}
	decision := decideProfileTimezones(scan.DefaultTimezone, zones)
	for _, f := range scan.ProfileTimezones {
		action := decision.action(f.Timezone)
		if f.Timezone == "" {
			action = "empty value ignored; remove the key from the file"
		} else if decision.copyZone != "" {
			action = fmt.Sprintf("ignored; to keep it, add the top-level line `default_timezone: %s` to %s "+
				"(the settings file form of agent_defaults.default_timezone) before saving server config from the admin UI, "+
				"because that save rewrites the profiles section without this key", decision.copyZone, scan.Path)
		}
		log.Warn("runtime-profile timezone was removed and has no effect; settings file not modified",
			"file", scan.Path, "profile", f.Profile, "timezone", f.Timezone, "action", action)
	}
}

// hubSettingDoc reads a hub_settings row as a raw JSON object. A missing row
// returns (nil, nil).
func hubSettingDoc(ctx context.Context, st store.HubSettingStore, section string) (*store.HubSetting, map[string]any, error) {
	row, err := st.GetHubSetting(ctx, section)
	if errors.Is(err, store.ErrNotFound) {
		return nil, map[string]any{}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s settings: %w", section, err)
	}
	doc := map[string]any{}
	if len(row.Value) > 0 {
		if err := json.Unmarshal(row.Value, &doc); err != nil {
			return nil, nil, fmt.Errorf("parsing %s settings: %w", section, err)
		}
	}
	return row, doc, nil
}

func retireDBTierProfileTimezones(ctx context.Context, ops *OperationalSettings, file config.SettingsFileProfileTimezoneScan, log *slog.Logger) error {
	profRow, profDoc, err := hubSettingDoc(ctx, ops.store, "profiles")
	if err != nil {
		return err
	}
	rowValues := config.LegacyProfileTimezonesFromProfiles(profDoc)

	// Values only in the settings file count while the profiles row is
	// seeded from that file: the seed extraction strips them, so the file
	// is the only place they still exist.
	var fileValues []config.LegacyProfileTimezone
	if profRow == nil || profRow.Origin == "seeded" {
		fileValues = file.ProfileTimezones
	}
	if len(rowValues) == 0 && len(fileValues) == 0 {
		return nil
	}

	adRow, adDoc, err := hubSettingDoc(ctx, ops.store, "agent_defaults")
	if err != nil {
		return err
	}
	def, _ := adDoc["default_timezone"].(string)

	zones := make([]string, 0, len(rowValues)+len(fileValues))
	for _, v := range rowValues {
		zones = append(zones, v.Timezone)
	}
	for _, v := range fileValues {
		zones = append(zones, v.Timezone)
	}
	decision := decideProfileTimezones(def, zones)

	// 1. agent_defaults first (copy case only). Written as "managed" so the
	// every-boot seed sync, which never seeds default_timezone, does not
	// overwrite the copied value.
	if decision.copyZone != "" {
		adDoc["default_timezone"] = decision.copyZone
		doc, err := json.Marshal(adDoc)
		if err != nil {
			return fmt.Errorf("encoding agent_defaults: %w", err)
		}
		var rev int64 // 0 = create-only when there is no row yet
		if adRow != nil {
			rev = adRow.Revision
		}
		if _, err := ops.Update(ctx, "agent_defaults", doc, profileTimezoneRetireUpdatedBy, rev, "managed"); err != nil {
			return fmt.Errorf("writing agent_defaults.default_timezone: %w", err)
		}
		log.Warn("runtime-profile timezone copied into agent_defaults.default_timezone; it now applies to every agent "+
			"with no pinned timezone and no TZ environment variable. Clear Default timezone in admin server config to undo. "+
			"agent_defaults is now admin-managed: settings.yaml no longer seeds it on this hub",
			"default_timezone", decision.copyZone)
	}

	// 2. Then strip the profiles row, keeping its origin.
	if len(rowValues) > 0 {
		for _, v := range rowValues {
			if p, ok := profDoc[v.Profile].(map[string]any); ok {
				delete(p, config.LegacyProfileTimezoneKey)
			}
		}
		doc, err := json.Marshal(profDoc)
		if err != nil {
			return fmt.Errorf("encoding profiles: %w", err)
		}
		if _, err := ops.Update(ctx, "profiles", doc, profileTimezoneRetireUpdatedBy, profRow.Revision, profRow.Origin); err != nil {
			return fmt.Errorf("stripping profiles.*.timezone: %w", err)
		}
		for _, v := range rowValues {
			log.Warn("runtime-profile timezone was removed; stripped from the hub profiles settings",
				"profile", v.Profile, "timezone", v.Timezone, "action", decision.action(v.Timezone))
		}
	}

	for _, v := range fileValues {
		log.Warn("runtime-profile timezone in the settings file is ignored by this hub; remove it from the file",
			"file", file.Path, "profile", v.Profile, "timezone", v.Timezone, "action", decision.action(v.Timezone))
	}
	return nil
}
