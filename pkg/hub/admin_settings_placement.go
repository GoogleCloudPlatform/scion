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
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Server-config saves of the runtimes and profiles sections: their
// Kubernetes placement is checked on save (config.RuntimeSaveChecks and
// config.PlacementPairSaveChecks: a problem the save introduces is
// refused, one already stored is a warning), and both sections are
// written in one store transaction that also compares the revision of a
// section the save leaves out, the one the checks used.

// placementSections are the sections a placement save checks and writes
// together.
var placementSections = []string{"profiles", "runtimes"}

// rejectRepeatedPlacementSections writes a 400 validation_failed when the
// JSON object body sends runtimes or profiles more than once, or under a
// name that matches only without regard to case: the typed decode matches
// member names without regard to case and merges a repeated member, so
// such a body would save something other than what any one member says.
// done is true when the response has been written.
func rejectRepeatedPlacementSections(w http.ResponseWriter, rawBody []byte) (done bool) {
	bad := map[string]bool{}
	seen := map[string]bool{}
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false // the typed decode reports a malformed body
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		name, _ := tok.(string)
		for _, section := range placementSections {
			if strings.EqualFold(name, section) {
				if name != section || seen[section] {
					bad[section] = true
				}
				seen[section] = true
			}
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return false
		}
	}
	if len(bad) == 0 {
		return false
	}
	errs := map[string][]config.ValidationError{}
	for _, section := range placementSections {
		if bad[section] {
			errs[section] = []config.ValidationError{{Path: section, Message: section + ": send the section once, spelt in lower case"}}
		}
	}
	writeJSON(w, http.StatusBadRequest, map[string]interface{}{
		"error":  "validation_failed",
		"errors": errs,
	})
	return true
}

// placementPair is the plan of a save that sends runtimes or profiles:
// whether it does, and for a section it leaves out, the revision of the
// value the save checks used (the cached one, ops.Snapshot; 0 when there
// is no row), which the write compares.
type placementPair struct {
	active bool
	reads  map[string]int64
}

// placementPairForSave plans the guarded runtimes and profiles write of a
// save (see placementPair).
func placementPairForSave(ops *OperationalSettings, sectionDocs map[string]json.RawMessage) placementPair {
	var pair placementPair
	for _, section := range placementSections {
		if _, sent := sectionDocs[section]; sent {
			pair.active = true
		}
	}
	if !pair.active {
		return pair
	}
	pair.reads = map[string]int64{}
	for _, section := range placementSections {
		if _, sent := sectionDocs[section]; !sent {
			pair.reads[section] = ops.SectionRevision(section)
		}
	}
	return pair
}

// placementReadConflicts lists, for a 409, the sections a guarded save
// read but did not write whose stored revision is no longer the one read
// (expected_revision is the revision read; read is true).
func placementReadConflicts(ctx context.Context, ops *OperationalSettings, reads map[string]int64, written map[string]bool) []map[string]interface{} {
	var out []map[string]interface{}
	for _, section := range placementSections {
		expected, ok := reads[section]
		if !ok || written[section] {
			continue
		}
		var current int64
		row, err := ops.store.GetHubSetting(ctx, section)
		switch {
		case err == nil:
			current = row.Revision
		case !errors.Is(err, store.ErrNotFound):
			continue // unknown: the written sections are still listed
		}
		if current != expected {
			out = append(out, map[string]interface{}{
				"section":           section,
				"expected_revision": expected,
				"current_revision":  current,
				"read":              true,
			})
		}
	}
	return out
}

// resetPlacementSection resets runtimes or profiles to the bootstrap value
// (DELETE /api/v1/admin/server-config/sections/{name}). The placement save
// checks run on the pair the reset leaves (the bootstrap value of the
// section and the current other section): a problem the reset introduces
// answers 422 naming the section. The row is then deleted in a guarded
// transaction that also compares the other section's revision, answering
// 409 on a change. ok is false when an error response has been written.
func (s *Server) resetPlacementSection(ctx context.Context, w http.ResponseWriter, ops *OperationalSettings, section string) bool {
	row, err := ops.store.GetHubSetting(ctx, section)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "Section not found in database: "+section, nil)
		return false
	}
	if err != nil {
		slog.Error("Failed to read section for reset", "section", section, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to reset section", nil)
		return false
	}
	snap := ops.Snapshot()
	boot, _ := config.OverlayMapSections(nil, ops.bootstrapKoanf)
	runtimes, profiles := snap.Runtimes, snap.Profiles
	other := "profiles"
	if section == "profiles" {
		profiles, other = boot.Profiles, "runtimes"
	} else {
		runtimes = boot.Runtimes
	}
	errs, _ := config.RuntimeSaveChecks(snap.Runtimes, runtimes, snap.Profiles, profiles)
	pairErrs, _ := config.PlacementPairSaveChecks(snap.Runtimes, runtimes, snap.Profiles, profiles)
	if errs = append(errs, pairErrs...); len(errs) > 0 {
		writeError(w, http.StatusUnprocessableEntity, ErrCodeValidationError,
			fmt.Sprintf("Resetting %s would leave Kubernetes placement in runtimes or profiles not valid (%d findings); fix the named settings first", section, len(errs)),
			map[string]interface{}{"section": section, "errors": errs})
		return false
	}
	reads := map[string]int64{other: ops.SectionRevision(other)}
	err = ops.deleteGuarded(ctx, section, row.Revision, reads)
	switch {
	case errors.Is(err, store.ErrRevisionConflict):
		conflicted := []map[string]interface{}{{
			"section":           section,
			"expected_revision": row.Revision,
			"current_revision":  s.getCurrentRevision(ops, section),
		}}
		conflicted = append(conflicted, placementReadConflicts(ctx, ops, reads, map[string]bool{section: true})...)
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":      "revision_conflict",
			"message":    "The section, or the section checked with it, changed during the reset.",
			"conflicted": conflicted,
		})
		return false
	case err != nil:
		slog.Error("Failed to reset section", "section", section, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to reset section", nil)
		return false
	}
	return true
}

// errGuardedBatchUnsupported reports a settings store without
// store.HubSettingGuardedBatchWriter: a runtimes or profiles save then
// fails closed rather than writing the sections one at a time.
var errGuardedBatchUnsupported = errors.New("the settings store cannot save runtimes and profiles in one transaction")

// guardedSectionWrite is one section of OperationalSettings.updateGuarded.
type guardedSectionWrite struct {
	section          string
	doc              json.RawMessage
	expectedRevision int64
}

// updateGuarded is update for several sections in one store transaction
// guarded by the revisions of the sections in reads
// (store.HubSettingGuardedBatchWriter): every section is validated, then
// written together or not at all, cached, published and self-applied once.
// It returns each written section's new revision and the sections the
// self-apply did not include (see update).
func (o *OperationalSettings) updateGuarded(ctx context.Context, writes []guardedSectionWrite, reads map[string]int64, updatedBy, origin string) (map[string]int64, map[string]bool, error) {
	bw, ok := o.store.(store.HubSettingGuardedBatchWriter)
	if !ok {
		return nil, nil, errGuardedBatchUnsupported
	}
	batch := make([]store.HubSettingWrite, 0, len(writes))
	for _, wr := range writes {
		if err := o.checkWriteGuard(ctx, wr.section); err != nil {
			return nil, nil, err
		}
		if errs := opsettings.Validate(wr.section, wr.doc); len(errs) > 0 {
			return nil, nil, fmt.Errorf("%w for section %q: %v", ErrSectionValidation, wr.section, errs)
		}
		batch = append(batch, store.HubSettingWrite{Section: wr.section, Value: wr.doc, ExpectedRevision: wr.expectedRevision})
	}
	rows, err := bw.UpsertHubSettingsGuarded(ctx, batch, reads, updatedBy, origin)
	if err != nil {
		return nil, nil, err
	}
	revs := make(map[string]int64, len(rows))
	notInEffect := map[string]bool{}
	for i := range rows {
		row := &rows[i]
		revs[row.Section] = row.Revision
		if o.cacheWrite(row.Section, row) && !o.refreshThrough(ctx, row.SettingsRevision, row.Section) {
			notInEffect[row.Section] = true
		}
	}
	for i := range rows {
		o.publish(rows[i].Section, rows[i].Revision)
	}
	o.selfApply()
	return revs, notInEffect, nil
}
