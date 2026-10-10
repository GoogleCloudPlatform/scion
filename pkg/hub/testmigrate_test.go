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
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)

// testMigrateMu serialises schema migration across this package's tests.
// entc.AutoMigrate, which CompositeStore.Migrate calls, mutates the
// package-level pkg/ent/migrate Tables, so two parallel tests that migrate a
// store at the same time crash the whole test process ("fatal error:
// concurrent map writes"). Production migrates once per process, so only
// tests need this. Every Migrate or AutoMigrate call in pkg/hub tests goes
// through migrateTestStore or autoMigrateTestClient.
var testMigrateMu sync.Mutex

// testMigrator is the part of a store that migrateTestStore needs.
type testMigrator interface {
	Migrate(ctx context.Context) error
}

// migrateTestStore runs s.Migrate while holding testMigrateMu.
func migrateTestStore(ctx context.Context, s testMigrator) error {
	testMigrateMu.Lock()
	defer testMigrateMu.Unlock()
	return s.Migrate(ctx)
}

// autoMigrateTestClient runs entc.AutoMigrate on client while holding
// testMigrateMu, for tests that migrate a raw ent client.
func autoMigrateTestClient(ctx context.Context, client *ent.Client) error {
	testMigrateMu.Lock()
	defer testMigrateMu.Unlock()
	return entc.AutoMigrate(ctx, client)
}
