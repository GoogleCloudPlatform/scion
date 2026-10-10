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

// sqliteFakeErr stands in for the SQLite driver's error type, matched by
// its Code method and type name.
type sqliteFakeErr struct{ code int }

func (e *sqliteFakeErr) Error() string { return fmt.Sprintf("sqlite error %d", e.code) }
func (e *sqliteFakeErr) Code() int     { return e.code }

type otherCodeErr struct{}

func (otherCodeErr) Error() string { return "deadlock detected" }
func (otherCodeErr) Code() int     { return sqliteBusy }

func TestIsTransientDBError_Typed(t *testing.T) {
	transient := []error{
		&pgconn.PgError{Code: pgSerializationFailure},
		&pgconn.PgError{Code: pgDeadlockDetected},
		fmt.Errorf("create: %w", &pgconn.PgError{Code: pgSerializationFailure}),
		&sqliteFakeErr{code: sqliteBusy},
		&sqliteFakeErr{code: sqliteLocked},
		&sqliteFakeErr{code: 5 | 2<<8}, // an extended BUSY code
	}
	for _, err := range transient {
		assert.True(t, isTransientDBError(err), "%v", err)
		assert.ErrorIs(t, markTransient(err), store.ErrTransient)
		assert.ErrorIs(t, markTransient(err), err, "the original stays in the chain")
	}
	notTransient := []error{
		nil,
		&pgconn.PgError{Code: "23505"},
		&sqliteFakeErr{code: 19},
		otherCodeErr{}, // not a SQLite type, and message text is not used
		errors.New("could not serialize access (SQLSTATE 40001)"),
	}
	for _, err := range notTransient {
		assert.False(t, isTransientDBError(err), "%v", err)
		if err != nil {
			assert.NotErrorIs(t, markTransient(err), store.ErrTransient)
		}
	}
}

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
