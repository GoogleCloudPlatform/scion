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

package trigger_test

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/txdiag"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

var (
	sharedSQLiteOnce   sync.Once
	sharedSQLiteClient *ent.Client
	sharedSQLiteErr    error
)

// sharedRaceEntClient returns enttest.NewClient(t) on Postgres, and on SQLite
// reuses a single migrated in-memory client across -count=N iterations so
// -race -count=1000 does not run 79-table AutoMigrate 1000 times. Callers must
// isolate rows by unique project/trigger/event IDs.
func sharedRaceEntClient(t *testing.T) *ent.Client {
	t.Helper()
	if enttest.Active() {
		return enttest.NewClient(t)
	}
	sharedSQLiteOnce.Do(func() {
		c, err := entc.OpenSQLite(
			"file:trigger_shared_race?mode=memory&cache=shared",
			entc.PoolConfig{MaxOpenConns: 1, Diag: txdiag.QuietConfig()},
		)
		if err != nil {
			sharedSQLiteErr = err
			return
		}
		if err := entc.AutoMigrate(context.Background(), c); err != nil {
			_ = c.Close()
			sharedSQLiteErr = err
			return
		}
		sharedSQLiteClient = c
	})
	if sharedSQLiteErr != nil {
		t.Fatalf("open shared sqlite ent client: %v", sharedSQLiteErr)
	}
	return sharedSQLiteClient
}

func TestMain(m *testing.M) {
	enttest.MainSetup()
	code := m.Run()
	if sharedSQLiteClient != nil {
		_ = sharedSQLiteClient.Close()
	}
	enttest.MainTeardown()
	os.Exit(code)
}
