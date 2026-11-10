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

package hub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guard that keeps in-memory SQLite opens pinned to one connection,
// including the rule that pkg/hub opens a private in-memory database only
// through hubtestutil.OpenTestMemorySQLite, is repository-wide and lives in
// hack/memsqliteguard (TestRepoMemorySQLiteOpensArePinned).

// TestGoModHasNoAlternateSQLiteDriver keeps the module on the one SQLite
// driver production uses, modernc.org/sqlite ("sqlite"). Tests used to open
// SQLite through mattn/go-sqlite3 ("sqlite3"), which differs in time
// handling, DSN options and error types, so a test could pass where
// production fails (ptone/scion#4694). With the module not requiring it, an
// import of that driver does not compile.
func TestGoModHasNoAlternateSQLiteDriver(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "github.com/mattn/go-sqlite3") {
		t.Fatal("go.mod requires github.com/mattn/go-sqlite3; open SQLite with the production driver \"sqlite\" (modernc.org/sqlite) instead")
	}
}
