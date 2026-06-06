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

//go:build no_sqlite

package entc

import "context"

// AlphaTableResult records the outcome of migrating one legacy table.
type AlphaTableResult struct {
	EntTable    string
	LegacyTable string
	Source      int
	Dest        int
}

// AlphaReport is the aggregate outcome of a migration α run.
type AlphaReport struct {
	BackupPath string
	SourcePath string
	Tables     []AlphaTableResult
	ChildEdges int
	Skipped    bool
	SkipReason string
}

// TotalRows returns the total number of destination rows written.
func (r *AlphaReport) TotalRows() int {
	n := 0
	for _, t := range r.Tables {
		n += t.Dest
	}
	return n
}

// AlphaOptions tunes a migration α run.
type AlphaOptions struct {
	Logf         func(format string, args ...any)
	BackupSuffix string
}

// IsLegacyRawSQLSchema always reports false when built without SQLite support.
func IsLegacyRawSQLSchema(_ string) (bool, error) { return false, nil }

// MigrateAlphaSQLite is a no-op when built without SQLite support.
func MigrateAlphaSQLite(_ context.Context, path string, _ AlphaOptions) (*AlphaReport, error) {
	return &AlphaReport{SourcePath: path, Skipped: true, SkipReason: "built without sqlite support"}, nil
}
