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
	"fmt"
	"testing"
)

// TestNewTestStoreParallel opens several test stores from parallel subtests
// and migrates each again. newTestStore itself no longer migrates (it
// restores the migrate-once template), so the explicit second
// migrateTestStore is what runs concurrent migrations here: without
// testMigrateMu they would write the shared pkg/ent/migrate Tables at once
// and the test process would die with "fatal error: concurrent map writes".
// The parallel newTestStore calls also exercise concurrent template restores.
func TestNewTestStoreParallel(t *testing.T) {
	const stores = 8
	for i := 0; i < stores; i++ {
		t.Run(fmt.Sprintf("store%d", i), func(t *testing.T) {
			t.Parallel()
			s, err := newTestStore(t, ":memory:")
			if err != nil {
				t.Fatalf("newTestStore: %v", err)
			}
			// A second Migrate, as many test helpers do, must be safe too.
			if err := migrateTestStore(context.Background(), s); err != nil {
				t.Fatalf("migrate again: %v", err)
			}
		})
	}
}
