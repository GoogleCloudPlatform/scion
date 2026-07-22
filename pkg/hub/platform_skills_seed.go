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
	"io/fs"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// platformSkillURIPrefix is the URI scheme used for platform (binary-embedded) skills
// in the hub_settings["injected_skills"].system list.  These URIs are for display
// only — the actual skill injection still happens via injectPlatformSkills() at
// provision time (Step 3a2).  Using Optional: true ensures that any attempt to
// resolve them via the skill bank is silently skipped rather than fatal.
const platformSkillURIPrefix = "scion-platform://"

// seedPlatformSkillInsertions reads the embedded platform skills from
// resources.PlatformSkillsFS() and upserts them as system entries in
// hub_settings["injected_skills"].  It runs on every hub startup so that the
// system list always reflects the current binary — restarting the hub after
// an upgrade automatically refreshes the entries.
//
// Idempotent: calling more than once produces the same result.
// Preserves user_defined entries: only the system sub-list is replaced.
func (s *Server) seedPlatformSkillInsertions(ctx context.Context) error {
	skillsFS := resources.PlatformSkillsFS()

	entries, err := fs.ReadDir(skillsFS, ".")
	if err != nil {
		return fmt.Errorf("seedPlatformSkillInsertions: read platform skills FS: %w", err)
	}

	var systemRefs []api.SkillReference
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Only seed skills that have a SKILL.md, mirroring the injectPlatformSkills
		// behaviour that skips directories without a SKILL.md.
		if _, statErr := fs.Stat(skillsFS, name+"/SKILL.md"); statErr != nil {
			slog.Debug("seedPlatformSkillInsertions: skipping directory without SKILL.md",
				"name", name)
			continue
		}
		systemRefs = append(systemRefs, api.SkillReference{
			URI:      platformSkillURIPrefix + name,
			Optional: true,
		})
	}

	// Preserve any existing user_defined entries so a hub restart never wipes
	// admin-managed skills.
	var userDefined []api.SkillReference
	if existing, getErr := s.store.GetHubSetting(ctx, "injected_skills"); getErr == nil {
		var prev api.HubSkillInjectionSetting
		if jsonErr := json.Unmarshal(existing.Value, &prev); jsonErr == nil {
			userDefined = prev.UserDefined
		} else {
			slog.Warn("seedPlatformSkillInsertions: could not parse existing hub skill setting; user_defined will be reset",
				"error", jsonErr)
		}
	} else if !errors.Is(getErr, store.ErrNotFound) {
		slog.Warn("seedPlatformSkillInsertions: could not read existing hub skill setting; user_defined will be reset",
			"error", getErr)
	}

	setting := api.HubSkillInjectionSetting{
		System:      systemRefs,
		UserDefined: userDefined,
	}

	value, marshalErr := json.Marshal(setting)
	if marshalErr != nil {
		return fmt.Errorf("seedPlatformSkillInsertions: marshal: %w", marshalErr)
	}

	// expectedRevision=-1 is the unconditional-upsert / idempotent-seed pattern.
	if _, upsertErr := s.store.UpsertHubSetting(ctx, "injected_skills", value, "seeded", -1, "seeded"); upsertErr != nil {
		return fmt.Errorf("seedPlatformSkillInsertions: upsert: %w", upsertErr)
	}

	slog.Info("Seeded platform skills into hub injected_skills", "count", len(systemRefs))
	return nil
}
