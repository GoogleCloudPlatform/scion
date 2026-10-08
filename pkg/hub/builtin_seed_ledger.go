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
	"sort"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// builtinSeedLedgerSection is the hub_settings section that records which
// built-in ("bundled") resource names this hub has ever seeded
// (ptone/scion#3544). Startup bootstrap creates a missing built-in only if its
// name is not in the ledger, so a built-in the user deleted stays deleted
// across restarts and upgrades. Deletes need no bookkeeping: the row goes away
// and the name stays here.
//
// Like the migration_* marker rows, the section has no opsettings definition,
// so it is never surfaced as an operational setting.
const builtinSeedLedgerSection = "builtin_resources_seeded"

// builtinSeedLedgerSchemaVersion is the schema_version written to the row.
const builtinSeedLedgerSchemaVersion = 1

// builtinSeedLedgerUpdatedBy is the updated_by value bootstrap writes.
const builtinSeedLedgerUpdatedBy = "bootstrap"

// builtinSeedLedgerMaxAttempts bounds the CAS retry loop in save.
const builtinSeedLedgerMaxAttempts = 5

// builtinSeedLedgerDoc is the JSON value stored in the hub_settings row.
type builtinSeedLedgerDoc struct {
	SchemaVersion  int      `json:"schema_version"`
	HarnessConfigs []string `json:"harness_configs"`
	Templates      []string `json:"templates"`
}

// builtinSeedLedger is an in-memory view of the seeded-built-ins ledger.
// Changes are tracked as pending additions and removals so that save can
// merge them onto the latest stored value (CAS), which keeps concurrent
// writers from dropping each other's names.
type builtinSeedLedger struct {
	rev     int64 // revision of the loaded row; 0 when no row existed
	seen    map[storage.ResourceKind]map[string]bool
	added   map[storage.ResourceKind]map[string]bool
	removed map[storage.ResourceKind]map[string]bool
	dirty   bool
	// failClosed is set when the stored ledger could not be loaded. Every
	// name then reports as Seen, so no missing built-in is (re)created, and
	// save is skipped so the unreadable row is never overwritten.
	failClosed bool
}

func newBuiltinSeedLedger() *builtinSeedLedger {
	return &builtinSeedLedger{
		seen:    map[storage.ResourceKind]map[string]bool{},
		added:   map[storage.ResourceKind]map[string]bool{},
		removed: map[storage.ResourceKind]map[string]bool{},
	}
}

// newFailClosedBuiltinSeedLedger returns a ledger for use when the stored
// ledger could not be loaded: every name is treated as seen and save is a
// no-op.
func newFailClosedBuiltinSeedLedger() *builtinSeedLedger {
	l := newBuiltinSeedLedger()
	l.failClosed = true
	return l
}

func setAdd(m map[storage.ResourceKind]map[string]bool, kind storage.ResourceKind, slug string) {
	if m[kind] == nil {
		m[kind] = map[string]bool{}
	}
	m[kind][slug] = true
}

// Seen reports whether slug of the given kind has ever been seeded.
func (l *builtinSeedLedger) Seen(kind storage.ResourceKind, slug string) bool {
	return l.failClosed || l.seen[kind][slug]
}

// Mark records slug of the given kind as seeded. It marks the ledger dirty
// only if the name is new.
func (l *builtinSeedLedger) Mark(kind storage.ResourceKind, slug string) {
	if l.Seen(kind, slug) {
		return
	}
	setAdd(l.seen, kind, slug)
	setAdd(l.added, kind, slug)
	delete(l.removed[kind], slug)
	l.dirty = true
}

// Forget removes slug of the given kind from the ledger. Bootstrap never
// calls it; it exists for tests.
func (l *builtinSeedLedger) Forget(kind storage.ResourceKind, slug string) {
	if !l.Seen(kind, slug) {
		return
	}
	delete(l.seen[kind], slug)
	delete(l.added[kind], slug)
	setAdd(l.removed, kind, slug)
	l.dirty = true
}

// names returns the sorted, de-duplicated slugs recorded for kind.
func (l *builtinSeedLedger) names(kind storage.ResourceKind) []string {
	out := make([]string, 0, len(l.seen[kind]))
	for slug := range l.seen[kind] {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// decodeBuiltinSeedLedger fills a fresh ledger from a stored row.
func decodeBuiltinSeedLedger(row *store.HubSetting) (*builtinSeedLedger, error) {
	l := newBuiltinSeedLedger()
	if row == nil {
		return l, nil
	}
	l.rev = row.Revision
	var doc builtinSeedLedgerDoc
	if len(row.Value) > 0 {
		if err := json.Unmarshal(row.Value, &doc); err != nil {
			return nil, fmt.Errorf("decode %s: %w", builtinSeedLedgerSection, err)
		}
	}
	for _, slug := range doc.HarnessConfigs {
		setAdd(l.seen, storage.ResourceKindHarnessConfig, slug)
	}
	for _, slug := range doc.Templates {
		setAdd(l.seen, storage.ResourceKindTemplate, slug)
	}
	return l, nil
}

// encode renders the ledger's current names as the stored JSON value.
func (l *builtinSeedLedger) encode() (json.RawMessage, error) {
	return json.Marshal(builtinSeedLedgerDoc{
		SchemaVersion:  builtinSeedLedgerSchemaVersion,
		HarnessConfigs: l.names(storage.ResourceKindHarnessConfig),
		Templates:      l.names(storage.ResourceKindTemplate),
	})
}

// loadBuiltinSeedLedger reads the ledger row. A missing row yields an empty
// ledger (fresh install, or first boot after upgrading to this code).
func (s *Server) loadBuiltinSeedLedger(ctx context.Context) (*builtinSeedLedger, error) {
	row, err := s.store.GetHubSetting(ctx, builtinSeedLedgerSection)
	if errors.Is(err, store.ErrNotFound) {
		return newBuiltinSeedLedger(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", builtinSeedLedgerSection, err)
	}
	return decodeBuiltinSeedLedger(row)
}

// saveBuiltinSeedLedger persists the ledger's pending changes. It is a no-op
// when nothing changed. Changes are merged onto the latest stored value with a
// CAS write, retried on revision conflicts, so concurrent bootstraps (for
// example HA replicas, normally serialized by LockBundledResources) converge
// on the union of their names.
func (s *Server) saveBuiltinSeedLedger(ctx context.Context, l *builtinSeedLedger) error {
	if l == nil || !l.dirty || l.failClosed {
		return nil
	}
	rev := l.rev
	current := l
	for attempt := 0; attempt < builtinSeedLedgerMaxAttempts; attempt++ {
		if attempt > 0 {
			// Reload and re-apply our pending changes on top.
			fresh, err := s.loadBuiltinSeedLedger(ctx)
			if err != nil {
				return err
			}
			for kind, slugs := range l.added {
				for slug := range slugs {
					setAdd(fresh.seen, kind, slug)
				}
			}
			for kind, slugs := range l.removed {
				for slug := range slugs {
					delete(fresh.seen[kind], slug)
				}
			}
			current = fresh
			rev = fresh.rev
		}

		value, err := current.encode()
		if err != nil {
			return fmt.Errorf("encode %s: %w", builtinSeedLedgerSection, err)
		}
		// rev == 0 means no row was loaded: create-only, which conflicts if
		// another writer created it in the meantime.
		row, err := s.store.UpsertHubSetting(ctx, builtinSeedLedgerSection, value,
			builtinSeedLedgerUpdatedBy, rev, "seeded")
		if errors.Is(err, store.ErrRevisionConflict) {
			continue
		}
		if err != nil {
			return fmt.Errorf("save %s: %w", builtinSeedLedgerSection, err)
		}
		if current != l {
			l.seen = current.seen
		}
		l.rev = row.Revision
		l.added = map[storage.ResourceKind]map[string]bool{}
		l.removed = map[storage.ResourceKind]map[string]bool{}
		l.dirty = false
		return nil
	}
	return fmt.Errorf("save %s: too many revision conflicts", builtinSeedLedgerSection)
}

// isBuiltinName reports whether slug names a bundled resource of the given
// kind in this binary's catalog.
//
// It covers the resources catalog only. config.UpdateDefaultTemplates also
// seeds harness.EmbedOnlyHarnesses() onto disk; that list is empty today.
// If it ever becomes non-empty, those names must be added here or a deleted
// embed-only harness config will be re-imported on workstation hubs.
// TestIsBuiltinName_CoversEmbedOnlyHarnesses guards this.
func isBuiltinName(kind storage.ResourceKind, slug string) bool {
	switch kind {
	case storage.ResourceKindHarnessConfig:
		for _, name := range resources.BuiltinHarnessConfigNames() {
			if api.Slugify(name) == slug {
				return true
			}
		}
	case storage.ResourceKindTemplate:
		for _, r := range resources.BuiltinTemplates() {
			if api.Slugify(r.Name) == slug {
				return true
			}
		}
	}
	return false
}
