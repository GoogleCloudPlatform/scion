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

package txdiag_test

import (
	"os"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

// hookCalls counts runs of a connection hook registered on modernc's
// registered driver instance. It is registered here, once, before any
// database is opened (modernc's registries are unlocked process globals that
// must be filled before the first Open), so TestRegisteredDriverHooksApply
// can prove the wrapped open path still applies such registrations.
var hookCalls atomic.Int64

func TestMain(m *testing.M) {
	sqlite.RegisterConnectionHook(func(sqlite.ExecQuerierContext, string) error {
		hookCalls.Add(1)
		return nil
	})
	os.Exit(m.Run())
}
