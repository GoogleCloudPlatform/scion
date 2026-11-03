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

package txdiag

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"sync"
	"sync/atomic"
)

// fakeDriver is a database/sql driver with no database. A fullConn
// implements every optional interface the wrapper forwards; a minimalConn
// implements only driver.Conn.
type fakeDriver struct {
	minimal bool
	// onExec runs inside ExecContext (for example to advance a fake clock).
	onExec func()
	// onOpen runs when a new connection is opened.
	onOpen func()
	// onBegin runs inside BeginTx (a slow BEGIN).
	onBegin func()
	// onCommit runs inside a transaction's Commit (a slow fsync).
	onCommit func()
	// execErr, when set, is returned by every Exec.
	execErr error
	valid   atomic.Bool // IsValid result; true by default via newFakeDriver
	calls   *fakeCalls
}

type fakeCalls struct {
	mu    sync.Mutex
	names []string
}

func (c *fakeCalls) add(n string) {
	c.mu.Lock()
	c.names = append(c.names, n)
	c.mu.Unlock()
}

func (c *fakeCalls) count(n string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := 0
	for _, x := range c.names {
		if x == n {
			k++
		}
	}
	return k
}

func newFakeDriver() *fakeDriver {
	d := &fakeDriver{calls: &fakeCalls{}}
	d.valid.Store(true)
	return d
}

func (d *fakeDriver) Open(string) (driver.Conn, error) {
	if d.onOpen != nil {
		d.onOpen()
	}
	if d.minimal {
		return &minimalConn{d: d}, nil
	}
	return &fullConn{minimalConn{d: d}}, nil
}

// fakeConnector is a driver.Connector that is also an io.Closer.
type fakeConnector struct {
	d      *fakeDriver
	closed atomic.Bool
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) { return c.d.Open("") }
func (c *fakeConnector) Driver() driver.Driver                        { return c.d }
func (c *fakeConnector) Close() error                                 { c.closed.Store(true); return nil }

var _ io.Closer = (*fakeConnector)(nil)

type minimalConn struct{ d *fakeDriver }

func (c *minimalConn) Prepare(string) (driver.Stmt, error) {
	c.d.calls.add("Prepare")
	return &fakeStmt{d: c.d}, nil
}
func (c *minimalConn) Close() error { c.d.calls.add("Close"); return nil }
func (c *minimalConn) Begin() (driver.Tx, error) {
	c.d.calls.add("Begin")
	return fakeTx{d: c.d}, nil
}

type fullConn struct{ minimalConn }

func (c *fullConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.d.calls.add("BeginTx")
	if c.d.onBegin != nil {
		c.d.onBegin()
	}
	return fakeTx{d: c.d}, nil
}

func (c *fullConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	c.d.calls.add("ExecContext")
	if c.d.onExec != nil {
		c.d.onExec()
	}
	if c.d.execErr != nil {
		return nil, c.d.execErr
	}
	return driver.RowsAffected(1), nil
}

func (c *fullConn) Ping(context.Context) error { c.d.calls.add("Ping"); return nil }
func (c *fullConn) ResetSession(context.Context) error {
	c.d.calls.add("ResetSession")
	return nil
}
func (c *fullConn) IsValid() bool { c.d.calls.add("IsValid"); return c.d.valid.Load() }

// CheckNamedValue accepts customArg, which the default converter rejects.
func (c *fullConn) CheckNamedValue(nv *driver.NamedValue) error {
	c.d.calls.add("CheckNamedValue")
	if v, ok := nv.Value.(customArg); ok {
		nv.Value = v.s
		return nil
	}
	return driver.ErrSkip
}

// customArg is a type only fullConn's CheckNamedValue accepts; the default
// converter rejects structs.
type customArg struct{ s string }

type fakeTx struct{ d *fakeDriver }

func (t fakeTx) Commit() error {
	if t.d != nil && t.d.onCommit != nil {
		t.d.onCommit()
	}
	return nil
}

func (fakeTx) Rollback() error { return nil }

type fakeStmt struct{ d *fakeDriver }

func (s *fakeStmt) Close() error  { return nil }
func (s *fakeStmt) NumInput() int { return -1 }
func (s *fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	s.d.calls.add("StmtExec")
	return driver.RowsAffected(0), nil
}
func (s *fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	return nil, errors.New("fakeStmt: query not supported")
}

// errFake is a driver error type tests match with errors.As.
type errFake struct{ code int }

func (e *errFake) Error() string { return "fake driver error" }
func (e *errFake) Code() int     { return e.code }
