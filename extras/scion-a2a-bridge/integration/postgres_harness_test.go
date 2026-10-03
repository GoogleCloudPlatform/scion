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

package integration_test

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

var runIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type databaseRunAllocator struct {
	mu   sync.Mutex
	used map[string]struct{}
}

func newDatabaseRunAllocator() *databaseRunAllocator {
	return &databaseRunAllocator{used: make(map[string]struct{})}
}

func (a *databaseRunAllocator) newRun(runID string) (*databaseRun, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.used[runID]; exists {
		return nil, fmt.Errorf("run ID %q has already been allocated", runID)
	}
	run, err := newDatabaseRun(runID)
	if err != nil {
		return nil, err
	}
	a.used[runID] = struct{}{}
	return run, nil
}

type databaseRun struct {
	RunID        string
	DatabaseName string
	SchemaName   string
	connection   *pgx.Conn
	teardowns    []func(context.Context) error
}

func newDatabaseRun(runID string) (*databaseRun, error) {
	if !runIDPattern.MatchString(runID) {
		return nil, fmt.Errorf("run ID %q must contain lowercase alphanumerics separated by single hyphens", runID)
	}
	identifier := strings.ReplaceAll(runID, "-", "_")
	if len(identifier) > 32 {
		return nil, fmt.Errorf("run ID %q is too long", runID)
	}
	return &databaseRun{
		RunID:        runID,
		DatabaseName: "ge_a2a_" + identifier,
		SchemaName:   "harness_" + identifier,
	}, nil
}

func (r *databaseRun) addTeardown(teardown func(context.Context) error) {
	r.teardowns = append(r.teardowns, teardown)
}

func (r *databaseRun) teardown(ctx context.Context) error {
	var firstErr error
	for index := len(r.teardowns) - 1; index >= 0; index-- {
		if err := r.teardowns[index](ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.teardowns = nil
	return firstErr
}

func (r *databaseRun) allocateSchema(ctx context.Context, databaseURL string) error {
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	r.connection = connection
	r.addTeardown(connection.Close)
	if _, err := connection.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{r.SchemaName}.Sanitize()); err != nil {
		_ = r.teardown(ctx)
		return fmt.Errorf("create schema %s: %w", r.SchemaName, err)
	}
	r.addTeardown(func(ctx context.Context) error {
		_, err := connection.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{r.SchemaName}.Sanitize()+" CASCADE")
		return err
	})
	return nil
}

func (r *databaseRun) schemaExists(ctx context.Context) error {
	if r.connection == nil {
		return fmt.Errorf("schema %s has not been allocated", r.SchemaName)
	}
	var exists bool
	if err := r.connection.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", r.SchemaName).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("schema %s does not exist", r.SchemaName)
	}
	return nil
}
