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

// Postgres parity of TransferBrokerReservation: the storetest conformance
// checks, which run on SQLite in pkg/store/storetest, run here against a
// fresh Postgres schema, where the agent and reservation rows are locked
// with SELECT ... FOR UPDATE.
package integrationtest

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
)

func TestTransferBrokerReservation_PostgresParity(t *testing.T) {
	requirePG(t)
	storetest.TransferBrokerReservationConformance(t, postgresStoreFactory)
}

// The hook-based checks set the process-global test hook, so they do not
// run in parallel.
func TestTransferBrokerReservationHook_PostgresParity(t *testing.T) {
	requirePG(t)
	storetest.TransferBrokerReservationHookConformance(t, postgresStoreFactory, func(fn storetest.TransferHook) func() {
		return entadapter.SetTransferBrokerReservationTestHook(entadapter.TransferHookFunc(fn))
	})
}
