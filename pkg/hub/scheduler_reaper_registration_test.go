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

package hub

import (
	"log/slog"
	"testing"
)

// TestRegisterSchedulerHandlers_RegistersLaunchReaper asserts that
// registerSchedulerHandlers must register the launch reaper on the
// scheduler: without this test, dropping the s.registerLaunchReaper() call
// would go unnoticed, silently turning off the deadline/staleness safety net
// in production. This exercises the production registration path
// (Server.registerSchedulerHandlers, called from StartBackgroundServices)
// without starting any ticker (registerSchedulerHandlers only builds the
// registration table; see TestBrokerProviderSelfHeal_RegisteredNonSingleton
// for the same pattern). It needs testServer (a real store), hence its own
// file with the !no_sqlite build tag, unlike scheduler_test.go's other
// Scheduler-only tests.
func TestRegisterSchedulerHandlers_RegistersLaunchReaper(t *testing.T) {
	srv, _ := testServer(t)
	srv.scheduler = NewScheduler(srv.store, slog.Default())
	srv.registerSchedulerHandlers()

	if srv.scheduler.launchReaper == nil {
		t.Fatal("expected registerSchedulerHandlers to register the launch reaper on the scheduler, but launchReaper is nil")
	}
}
