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
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
)

// WithTx needs a real store, so this test is left out of no_sqlite builds
// (enttest opens SQLite unless built with -tags integration).
// WithTx marks a transient error returned from the transaction body.
func TestWithTx_MarksTransient(t *testing.T) {
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })
	err := cs.WithTx(context.Background(), func(store.Store) error {
		return fmt.Errorf("insert: %w", &pgconn.PgError{Code: pgDeadlockDetected})
	})
	assert.ErrorIs(t, err, store.ErrTransient)
	err = cs.WithTx(context.Background(), func(store.Store) error { return errors.New("boom") })
	assert.NotErrorIs(t, err, store.ErrTransient)
}
