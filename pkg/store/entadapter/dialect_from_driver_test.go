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

package entadapter

import (
	"testing"

	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// TestStores_UsesRowLocks_ReadsDriverDialect pins that the access constraint,
// hub setting and lifecycle hook stores read the row-lock dialect from the
// driver with no query: a driver that fails every statement still gives the
// right answer, and nothing is sent to it (ptone/scion#3835). usesRowLocks
// takes no context, so a cancelled or expired context cannot affect it. A
// probe query that failed used to be able to leave Postgres without row locks
// for the life of the process.
func TestStores_UsesRowLocks_ReadsDriverDialect(t *testing.T) {
	stores := map[string]func(*ent.Client) func() bool{
		"access_constraint": func(c *ent.Client) func() bool { return NewAccessConstraintStore(c).usesRowLocks },
		"hub_setting":       func(c *ent.Client) func() bool { return NewHubSettingStore(c).usesRowLocks },
		"lifecycle_hook":    func(c *ent.Client) func() bool { return NewLifecycleHookStore(c).usesRowLocks },
	}
	for name, newStore := range stores {
		for _, tc := range []struct {
			dialect string
			want    bool
		}{
			{dialect.Postgres, true},
			{dialect.SQLite, false},
		} {
			t.Run(name+"/"+tc.dialect, func(t *testing.T) {
				drv := &captureDriver{dialectName: tc.dialect}
				usesRowLocks := newStore(ent.NewClient(ent.Driver(drv)))
				assert.Equal(t, tc.want, usesRowLocks())

				drv.mu.Lock()
				defer drv.mu.Unlock()
				assert.Empty(t, drv.stmts, "dialect detection must not query the database")
			})
		}
	}
}
