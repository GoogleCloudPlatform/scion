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

//go:build !no_sqlite

package hub

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// readBuiltinSeedLedgerDoc returns the stored ledger document, or nil if the
// row does not exist.
func readBuiltinSeedLedgerDoc(t *testing.T, s store.Store) *builtinSeedLedgerDoc {
	t.Helper()
	row, err := s.GetHubSetting(context.Background(), builtinSeedLedgerSection)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		t.Fatalf("GetHubSetting(%s): %v", builtinSeedLedgerSection, err)
	}
	var doc builtinSeedLedgerDoc
	if err := json.Unmarshal(row.Value, &doc); err != nil {
		t.Fatalf("decode ledger: %v", err)
	}
	return &doc
}

// allBuiltinHarnessConfigSlugs returns the sorted slugs of every bundled
// harness config.
func allBuiltinHarnessConfigSlugs() []string {
	var out []string
	for _, n := range resources.BuiltinHarnessConfigNames() {
		out = append(out, api.Slugify(n))
	}
	sort.Strings(out)
	return out
}

// allBuiltinTemplateSlugs returns the sorted slugs of every bundled template.
func allBuiltinTemplateSlugs() []string {
	var out []string
	for _, r := range resources.BuiltinTemplates() {
		out = append(out, api.Slugify(r.Name))
	}
	sort.Strings(out)
	return out
}

// assertLedgerHasAllBuiltins fails unless the stored ledger lists exactly
// every bundled harness config and template.
func assertLedgerHasAllBuiltins(t *testing.T, s store.Store) {
	t.Helper()
	doc := readBuiltinSeedLedgerDoc(t, s)
	if doc == nil {
		t.Fatal("expected builtin seed ledger row to exist")
	}
	if doc.SchemaVersion != builtinSeedLedgerSchemaVersion {
		t.Errorf("schema_version = %d, want %d", doc.SchemaVersion, builtinSeedLedgerSchemaVersion)
	}
	if want := allBuiltinHarnessConfigSlugs(); !reflect.DeepEqual(doc.HarnessConfigs, want) {
		t.Errorf("ledger harness_configs = %v, want %v", doc.HarnessConfigs, want)
	}
	if want := allBuiltinTemplateSlugs(); !reflect.DeepEqual(doc.Templates, want) {
		t.Errorf("ledger templates = %v, want %v", doc.Templates, want)
	}
}

func TestBuiltinSeedLedger_LoadMissingIsEmpty(t *testing.T) {
	srv, _, _ := testTemplateBootstrapServer(t)
	l, err := srv.loadBuiltinSeedLedger(context.Background())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if l.Seen(storage.ResourceKindHarnessConfig, "claude") || l.Seen(storage.ResourceKindTemplate, "default") {
		t.Error("empty ledger reports a name as seen")
	}
	if l.dirty {
		t.Error("freshly loaded ledger should not be dirty")
	}
}

func TestBuiltinSeedLedger_RoundTripSortedDeduped(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	l, err := srv.loadBuiltinSeedLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l.Mark(storage.ResourceKindHarnessConfig, "codex")
	l.Mark(storage.ResourceKindHarnessConfig, "claude")
	l.Mark(storage.ResourceKindHarnessConfig, "codex") // duplicate
	l.Mark(storage.ResourceKindTemplate, "default")
	if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
		t.Fatalf("save: %v", err)
	}

	doc := readBuiltinSeedLedgerDoc(t, s)
	if doc == nil {
		t.Fatal("ledger row not written")
	}
	if doc.SchemaVersion != 1 {
		t.Errorf("schema_version = %d, want 1", doc.SchemaVersion)
	}
	if want := []string{"claude", "codex"}; !reflect.DeepEqual(doc.HarnessConfigs, want) {
		t.Errorf("harness_configs = %v, want %v", doc.HarnessConfigs, want)
	}
	if want := []string{"default"}; !reflect.DeepEqual(doc.Templates, want) {
		t.Errorf("templates = %v, want %v", doc.Templates, want)
	}

	row, err := s.GetHubSetting(ctx, builtinSeedLedgerSection)
	if err != nil {
		t.Fatal(err)
	}
	if row.Origin != "seeded" {
		t.Errorf("origin = %q, want seeded", row.Origin)
	}
	if row.UpdatedBy != builtinSeedLedgerUpdatedBy {
		t.Errorf("updated_by = %q, want %q", row.UpdatedBy, builtinSeedLedgerUpdatedBy)
	}

	reloaded, err := srv.loadBuiltinSeedLedger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"claude", "codex"} {
		if !reloaded.Seen(storage.ResourceKindHarnessConfig, slug) {
			t.Errorf("reloaded ledger missing harness-config %q", slug)
		}
	}
	if !reloaded.Seen(storage.ResourceKindTemplate, "default") {
		t.Error("reloaded ledger missing template default")
	}
	// Kinds are independent namespaces.
	if reloaded.Seen(storage.ResourceKindTemplate, "claude") {
		t.Error("harness-config name leaked into template namespace")
	}
}

func TestBuiltinSeedLedger_SaveNoopWhenClean(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	l, _ := srv.loadBuiltinSeedLedger(ctx)
	if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
		t.Fatalf("save clean empty ledger: %v", err)
	}
	if doc := readBuiltinSeedLedgerDoc(t, s); doc != nil {
		t.Fatal("saving a clean ledger must not create the row")
	}

	l.Mark(storage.ResourceKindHarnessConfig, "claude")
	if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
		t.Fatal(err)
	}
	row1, _ := s.GetHubSetting(ctx, builtinSeedLedgerSection)

	// Re-marking a known name does not dirty the ledger, so no write happens.
	l.Mark(storage.ResourceKindHarnessConfig, "claude")
	if l.dirty {
		t.Error("re-marking a seen name should not dirty the ledger")
	}
	if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
		t.Fatal(err)
	}
	row2, _ := s.GetHubSetting(ctx, builtinSeedLedgerSection)
	if row2.Revision != row1.Revision {
		t.Errorf("revision changed on no-op save: %d -> %d", row1.Revision, row2.Revision)
	}
}

func TestBuiltinSeedLedger_Forget(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	l, _ := srv.loadBuiltinSeedLedger(ctx)
	l.Mark(storage.ResourceKindHarnessConfig, "claude")
	l.Mark(storage.ResourceKindHarnessConfig, "codex")
	if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
		t.Fatal(err)
	}

	l.Forget(storage.ResourceKindHarnessConfig, "claude")
	if l.Seen(storage.ResourceKindHarnessConfig, "claude") {
		t.Error("Forget did not remove the name in memory")
	}
	if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
		t.Fatal(err)
	}
	doc := readBuiltinSeedLedgerDoc(t, s)
	if want := []string{"codex"}; !reflect.DeepEqual(doc.HarnessConfigs, want) {
		t.Errorf("harness_configs after Forget = %v, want %v", doc.HarnessConfigs, want)
	}
}

// TestBuiltinSeedLedger_ConcurrentSavesMerge verifies that two writers that
// loaded the same revision both land their names (CAS merge on conflict).
func TestBuiltinSeedLedger_ConcurrentSavesMerge(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	for _, seed := range []bool{false, true} {
		if seed {
			l, _ := srv.loadBuiltinSeedLedger(ctx)
			l.Mark(storage.ResourceKindTemplate, "default")
			if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
				t.Fatal(err)
			}
		}
		a, _ := srv.loadBuiltinSeedLedger(ctx)
		b, _ := srv.loadBuiltinSeedLedger(ctx)
		a.Mark(storage.ResourceKindHarnessConfig, "claude")
		b.Mark(storage.ResourceKindHarnessConfig, "codex")
		if err := srv.saveBuiltinSeedLedger(ctx, a); err != nil {
			t.Fatalf("save a (seed=%v): %v", seed, err)
		}
		if err := srv.saveBuiltinSeedLedger(ctx, b); err != nil {
			t.Fatalf("save b (seed=%v): %v", seed, err)
		}
		doc := readBuiltinSeedLedgerDoc(t, s)
		if want := []string{"claude", "codex"}; !reflect.DeepEqual(doc.HarnessConfigs, want) {
			t.Errorf("seed=%v: harness_configs = %v, want %v", seed, doc.HarnessConfigs, want)
		}
		if seed && !reflect.DeepEqual(doc.Templates, []string{"default"}) {
			t.Errorf("seed=%v: templates = %v, want [default]", seed, doc.Templates)
		}
		if err := s.DeleteHubSetting(ctx, builtinSeedLedgerSection); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIsBuiltinName(t *testing.T) {
	for _, name := range resources.BuiltinHarnessConfigNames() {
		if !isBuiltinName(storage.ResourceKindHarnessConfig, api.Slugify(name)) {
			t.Errorf("isBuiltinName(harness-config, %q) = false", name)
		}
	}
	if !isBuiltinName(storage.ResourceKindTemplate, "default") {
		t.Error("isBuiltinName(template, default) = false")
	}
	for _, tc := range []struct {
		kind storage.ResourceKind
		slug string
	}{
		{storage.ResourceKindHarnessConfig, "my-custom-config"},
		{storage.ResourceKindTemplate, "my-template"},
		{storage.ResourceKindTemplate, "claude"},       // harness-config name, wrong kind
		{storage.ResourceKindHarnessConfig, "default"}, // template name, wrong kind
	} {
		if isBuiltinName(tc.kind, tc.slug) {
			t.Errorf("isBuiltinName(%s, %q) = true, want false", tc.kind, tc.slug)
		}
	}
}

// TestIsBuiltinName_CoversEmbedOnlyHarnesses guards that every harness config
// config.UpdateDefaultTemplates seeds onto disk is known to isBuiltinName.
// Embed-only harnesses are seeded alongside the resources catalog; if that
// list becomes non-empty, isBuiltinName must learn about it.
func TestIsBuiltinName_CoversEmbedOnlyHarnesses(t *testing.T) {
	for _, h := range harness.EmbedOnlyHarnesses() {
		if !isBuiltinName(storage.ResourceKindHarnessConfig, api.Slugify(h.Name())) {
			t.Errorf("embed-only harness %q is seeded on disk but isBuiltinName does not know it; deleting it would not stick on workstation hubs", h.Name())
		}
	}
}

// TestBuiltinSeedLedger_FailClosed verifies the fail-closed ledger used after
// a load error: every name is seen and save never writes.
func TestBuiltinSeedLedger_FailClosed(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	if _, err := s.UpsertHubSetting(ctx, builtinSeedLedgerSection, json.RawMessage(`"not-a-ledger"`), "test", -1, "seeded"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.loadBuiltinSeedLedger(ctx); err == nil {
		t.Fatal("expected a decode error for a non-object ledger value")
	}

	l := newFailClosedBuiltinSeedLedger()
	if !l.Seen(storage.ResourceKindHarnessConfig, "anything") || !l.Seen(storage.ResourceKindTemplate, "default") {
		t.Error("fail-closed ledger must report every name as seen")
	}
	l.Mark(storage.ResourceKindHarnessConfig, "claude")
	before, _ := s.GetHubSetting(ctx, builtinSeedLedgerSection)
	if err := srv.saveBuiltinSeedLedger(ctx, l); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetHubSetting(ctx, builtinSeedLedgerSection)
	if after.Revision != before.Revision {
		t.Error("fail-closed ledger save must not write")
	}
}
