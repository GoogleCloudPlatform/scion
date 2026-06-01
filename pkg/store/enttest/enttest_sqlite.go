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

//go:build !integration

package enttest

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
)

// newClient returns an in-memory SQLite-backed client. This is the default
// build; the Postgres backend lives in enttest_postgres.go behind the
// `integration` tag.
func newClient(t *testing.T) *ent.Client { return newSQLiteClient(t) }

// setup/teardown have nothing to do for the SQLite backend.
func setup()    {}
func teardown() {}
