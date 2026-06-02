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

// Package integrationtest holds the Postgres stress / integration suite. Unlike
// the CRUD-parity suites in pkg/store/storetest and pkg/store/entadapter (which
// run unchanged against both SQLite and Postgres to prove behavioral parity),
// every test here targets behavior that ONLY manifests under a real, multi-writer
// Postgres server: row-level contention, transaction isolation, connection-pool
// saturation, LISTEN/NOTIFY delivery, large-dataset migration, schema/type edge
// cases, and multi-process coordination.
//
// All test files are gated with the `integration` build tag and additionally skip
// at runtime unless SCION_TEST_POSTGRES_URL points at a live Postgres (local or
// CloudSQL); see requirePG. Under the default build only this file compiles, so
// `go test ./...` reports the package as having no tests rather than failing.
//
//	go test -tags integration ./pkg/store/integrationtest/... \
//	  with SCION_TEST_POSTGRES_URL=postgres://user:pass@host:5432/db?sslmode=disable
//
// Concurrency levels default to small values so the suite finishes well under the
// 5-minute local-Postgres target; set SCION_TEST_CONCURRENCY=<N> to crank them up
// for a heavier stress run.
package integrationtest
