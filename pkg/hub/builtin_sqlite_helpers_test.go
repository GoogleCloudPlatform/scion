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

// allBuiltinTemplateSlugs returns the sorted slugs of every bundled template.
func allBuiltinTemplateSlugs() []string {
	var out []string
	for _, r := range resources.BuiltinTemplates() {
		out = append(out, api.Slugify(r.Name))
	}
	sort.Strings(out)
	return out
}
