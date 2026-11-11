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

//go:build integration

// Postgres parity of the conditional reservation releases: the storetest
// conformance checks of ReleaseReservationIfAgent and
// ReleaseReservationIfScopeBrokerAbsent, which run on SQLite in
// pkg/store/storetest, run here against a fresh Postgres schema. On Postgres
// the agent row is locked with SELECT ... FOR UPDATE, and the scope ID is
// compared with the uuid column through the parsed UUID.
package integrationtest

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
)

func postgresStoreFactory(t *testing.T) store.Store {
	t.Helper()
	return newStore(t)
}

func TestReservationRelease_PostgresParity(t *testing.T) {
	requirePG(t)
	storetest.ReservationReleaseConformance(t, postgresStoreFactory)
}

// A scope ID that cannot be a broker ID is released with no error on
// Postgres too: the write never compares a non-UUID literal with the uuid
// column (a raw-string comparison would fail there with an invalid input
// error).
func TestReleaseIfScopeBrokerAbsent_UnparseableScopeID_Released_Postgres(t *testing.T) {
	requirePG(t)
	storetest.ReleaseIfScopeBrokerAbsentUnparseableScopeConformance(t, postgresStoreFactory)
}

func TestReleaseIfScopeBrokerAbsent_NonCanonicalScopeID_Postgres(t *testing.T) {
	requirePG(t)
	storetest.ReleaseIfScopeBrokerAbsentNonCanonicalScopeConformance(t, postgresStoreFactory)
}
