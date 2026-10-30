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

package memsqliteguard

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoRoot is the repository root relative to this package's directory.
const repoRoot = "../.."

// skipDir reports whether the walk should not descend into a directory with
// this name: hidden directories (.git, agent state), Go-ignored directories
// (testdata, _-prefixed) and third-party trees.
func skipDir(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") ||
		name == "testdata" || name == "vendor" || name == "node_modules"
}

func readRepoGoFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(repoRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != repoRoot && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") {
			return nil
		}
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestRepoMemorySQLiteOpensArePinned is the repository-wide guard: every
// in-memory SQLite open in the tree is pinned to one connection, and pkg/hub
// opens private in-memory databases only through
// hubtestutil.OpenTestMemorySQLite. See the package doc for the rules.
func TestRepoMemorySQLiteOpensArePinned(t *testing.T) {
	files := readRepoGoFiles(t)
	// The sanctioned hub helper must be in the scan; if it is not, the walk
	// started in the wrong place and a clean result would mean nothing.
	const helper = "pkg/hub/internal/hubtestutil/memsqlite.go"
	if _, ok := files[helper]; !ok {
		t.Fatalf("%s not found under %s; the guard is not scanning the repository", helper, repoRoot)
	}
	findings, stats, err := Scan(files)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MemoryOpens == 0 {
		t.Fatalf("scanned %d Go files but saw no in-memory SQLite open; the scanner is broken", stats.Files)
	}
	if len(findings) == 0 {
		return
	}
	var b strings.Builder
	for _, f := range findings {
		b.WriteString("\n  " + f.String())
	}
	t.Fatalf("in-memory SQLite opened without pinning the pool to one connection (%s), "+
		"or a raw private in-memory open in pkg/hub (%s).\n"+
		"Fix: pass entc.PoolConfig{MaxOpenConns: 1} to entc.OpenSQLite, call SetMaxOpenConns(1) on a sql.Open handle, "+
		"or use enttest.NewClient (pkg/store/enttest) / hubtestutil.OpenTestMemorySQLite "+
		"(pkg/hub/internal/hubtestutil/memsqlite.go). Background: ptone/scion#2312, ptone/scion#5341.%s",
		RuleUnpinned, RuleHubRaw, b.String())
}

// TestScanDetector checks the rules against the spellings they must catch
// and the ones they must allow. A "// want <rule>" comment marks each line
// that must be reported.
func TestScanDetector(t *testing.T) {
	const common = `
import (
	"database/sql"
	"fmt"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
)
`
	files := map[string]string{
		"pkg/other/probe_test.go": `package other
` + common + `
const memDSN = ":memory:"

const (
	implicitBase = "file:x?mode=memory&cache=shared"
	implicitDSN
)

var pkgDSN = "file:pkgvar?mode=memory&cache=shared"

var pinnedPool = entc.PoolConfig{MaxOpenConns: 1}

func drv() string { return "sqlite" }

func bad(name string) {
	const localDSN = "file:x?mode=memory"
	_, _ = sql.Open("sqlite", ":memory:") // want unpinned
	_, _ = sql.Open("sqlite", memDSN) // want unpinned
	_, _ = sql.Open( // want unpinned
		"sqlite",
		":memory:",
	)
	_, _ = sql.Open(drv(), localDSN) // want unpinned
	_, _ = ent.Open("sqlite", "file::memory:?_fk=1") // want unpinned
	_, _ = entc.OpenSQLite(implicitDSN, entc.PoolConfig{}) // want unpinned
	_, _ = entc.OpenSQLite(pkgDSN, entc.PoolConfig{MaxOpenConns: 2}) // want unpinned
	_, _ = entc.OpenSQLite(fmt.Sprintf("file:%s?mode=memory&cache=shared", name), entc.PoolConfig{}) // want unpinned
	_, _ = entc.OpenSQLite("file:"+name+"?mode=memory&cache=shared", entc.PoolConfig{MaxIdleConns: 1}) // want unpinned
	dsn := "file:" + name
	if name == "" {
		dsn = "file::memory:?cache=shared"
	}
	_, _ = entc.OpenSQLite(dsn, entc.PoolConfig{}) // want unpinned
	side, _ := sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared") // want unpinned
	side.SetMaxOpenConns(2)
	other, _ := sql.Open("sqlite", ":memory:") // want unpinned
	unrelated.SetMaxOpenConns(1)
	_ = other
	drvr, _ := entsql.Open("sqlite", ":memory:") // want unpinned
	drvr.DB().SetMaxIdleConns(1)
}

func returned() (*sql.DB, error) {
	return sql.Open("sqlite", "file:r?mode=memory&cache=shared") // want unpinned
}

func good(name string, pool entc.PoolConfig) {
	// _, _ = sql.Open("sqlite", ":memory:")
	db, _ := sql.Open("sqlite", ":memory:")
	db.SetMaxOpenConns(1)
	var db2, _ = sql.Open("sqlite", "file:"+name+"?mode=memory&cache=shared")
	db2.SetMaxOpenConns(1)
	d, _ := entsql.Open("sqlite", ":memory:")
	d.DB().SetMaxOpenConns(1)
	_, _ = entc.OpenSQLite("file:"+name+"?mode=memory&cache=shared", entc.PoolConfig{MaxOpenConns: 1})
	_, _ = entc.OpenSQLite(fmt.Sprintf("file:%s?mode=memory&cache=shared", name), pinnedPool)
	local := entc.PoolConfig{MaxOpenConns: 1, MaxIdleConns: 1}
	_, _ = entc.OpenSQLite(implicitBase, local)
	_, _ = sql.Open("sqlite", "/tmp/db.sqlite")
	_, _ = entc.OpenSQLite("file:"+name, pool)
	_, _ = entc.OpenSQLite(name, entc.PoolConfig{})
}
`,
		// Rule hub-raw: pkg/hub subpackages too, and only the sanctioned
		// helper may open a private in-memory database, pinned or not.
		"pkg/hub/internal/hubtestutil/probe.go": `package hubtestutil
` + common + `
func OpenTestMemorySQLite() *sql.DB {
	db, _ := sql.Open("sqlite", ":memory:?_timezone=UTC")
	db.SetMaxOpenConns(1)
	return db
}

func other() {
	db, _ := sql.Open("sqlite", ":memory:") // want hub-raw
	db.SetMaxOpenConns(1)
	_, _ = entc.OpenSQLite("file:x?mode=memory&cache=shared", entc.PoolConfig{MaxOpenConns: 1})
}
`,
		"pkg/hub/probe_test.go": `package hub
` + common + `
func openTestMemorySQLite() { _, _ = sql.Open("sqlite", ":memory:") } // want unpinned hub-raw
`,
		// Inside package entc, OpenSQLite is called unqualified.
		"pkg/ent/entc/probe_test.go": `package entc

func probe() {
	_, _ = OpenSQLite("file:x?mode=memory&cache=shared", PoolConfig{}) // want unpinned
	_, _ = OpenSQLite("file:y?mode=memory&cache=shared", PoolConfig{MaxOpenConns: 1})
}
`,
		// A local package named sql that is not database/sql is ignored.
		"pkg/notsql/probe_test.go": `package notsql

import "example.com/sql"

func probe() { _, _ = sql.Open("sqlite", ":memory:") }
`,
	}
	src := map[string][]byte{}
	var want []string
	for name, body := range files {
		src[name] = []byte(body)
		for i, line := range strings.Split(body, "\n") {
			_, rules, ok := strings.Cut(line, "// want ")
			if !ok {
				continue
			}
			for _, r := range strings.Fields(rules) {
				want = append(want, name+":"+strconv.Itoa(i+1)+":"+r)
			}
		}
	}
	findings, stats, err := Scan(src)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.File+":"+strconv.Itoa(f.Line)+":"+f.Rule)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings differ\n got:\n  %s\nwant:\n  %s\nfull:\n  %v",
			strings.Join(got, "\n  "), strings.Join(want, "\n  "), findings)
	}
	if stats.Files != len(files) || stats.MemoryOpens == 0 {
		t.Fatalf("stats = %+v, want %d files and some memory opens", stats, len(files))
	}
}
