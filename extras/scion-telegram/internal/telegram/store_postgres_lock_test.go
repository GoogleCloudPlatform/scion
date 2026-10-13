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

package telegram

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lockFakeConnector hands out lockFakeConns that fail the advisory unlock
// statement when failUnlock is set, and counts closed connections.
type lockFakeConnector struct {
	failUnlock bool
	closed     atomic.Int32
}

func (c *lockFakeConnector) Connect(context.Context) (driver.Conn, error) {
	return &lockFakeConn{owner: c}, nil
}

func (c *lockFakeConnector) Driver() driver.Driver { return lockFakeDriver{} }

type lockFakeDriver struct{}

func (lockFakeDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use the connector")
}

type lockFakeConn struct {
	owner     *lockFakeConnector
	closeOnce sync.Once
}

func (c *lockFakeConn) ExecContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Result, error) {
	if c.owner.failUnlock && strings.Contains(query, "pg_advisory_unlock") {
		return nil, errors.New("unlock failed")
	}
	return driver.RowsAffected(0), nil
}

func (c *lockFakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}

func (c *lockFakeConn) Close() error {
	c.closeOnce.Do(func() { c.owner.closed.Add(1) })
	return nil
}

func (c *lockFakeConn) Begin() (driver.Tx, error) {
	return nil, errors.New("transactions not supported")
}

func TestWithSchemaLock_ConnectionRelease(t *testing.T) {
	fnErr := errors.New("ddl failed")
	tests := []struct {
		name       string
		failUnlock bool
		fnErr      error
		fnPanics   bool
		wantErr    string
		wantClosed int32
		wantIdle   int
	}{
		{name: "unlock ok returns connection to pool", wantIdle: 1},
		{name: "unlock failure discards connection", failUnlock: true,
			wantErr: "release schema lock", wantClosed: 1},
		{name: "unlock failure after fn error discards connection",
			failUnlock: true, fnErr: fnErr, wantErr: fnErr.Error(), wantClosed: 1},
		{name: "fn panic discards connection", fnPanics: true, wantClosed: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connector := &lockFakeConnector{failUnlock: tt.failUnlock}
			db := sql.OpenDB(connector)
			t.Cleanup(func() { _ = db.Close() })

			var err error
			var recovered any
			func() {
				defer func() { recovered = recover() }()
				err = withSchemaLock(context.Background(), db, func(*sql.Conn) error {
					if tt.fnPanics {
						panic("ddl panic")
					}
					return tt.fnErr
				})
			}()
			if tt.fnPanics {
				assert.Equal(t, "ddl panic", recovered, "panic propagates")
			} else {
				require.Nil(t, recovered)
			}
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
			assert.Equal(t, tt.wantClosed, connector.closed.Load(), "closed connections")
			assert.Equal(t, tt.wantIdle, db.Stats().Idle, "idle connections")
		})
	}
}
