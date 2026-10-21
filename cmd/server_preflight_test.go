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

package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/writesonread"
	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/preflight"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// runPreflightForTest is the one way a cmd test runs the preflight: it
// isolates the process-wide no-write switch (internal/writesonread) first,
// so the switch the run sets is off again when the test ends. Tests that
// call it must not run in parallel.
func runPreflightForTest(t *testing.T, o preflightOptions) (stdout, stderr string, err error) {
	t.Helper()
	writesonread.IsolateForTest(t)
	var out, errOut bytes.Buffer
	o.stdout, o.stderr = &out, &errOut
	if o.format == "" {
		o.format = "json"
	}
	err = runServerPreflight(context.Background(), o)
	return out.String(), errOut.String(), err
}

// preflightHub is a hub fixture: an isolated HOME and working directory,
// the global settings (active_profile a, profile a on docker, which is also
// the bootstrap material), and a migrated SQLite hub database at
// ~/.scion/hub.db with a legacy co-located broker row.
type preflightHub struct {
	home, globalDir, dbPath, workDir string
	client                           *ent.Client // read-write, for seeding
	store                            *entadapter.CompositeStore
	brokerID                         string
}

func newPreflightHub(t *testing.T) *preflightHub {
	t.Helper()
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "SCION_SEED_") || strings.HasPrefix(k, "SCION_SERVER_") || k == preflightDBEnv {
			t.Setenv(k, v) // restored at cleanup
			require.NoError(t, os.Unsetenv(k))
		}
	}
	h := &preflightHub{home: t.TempDir(), workDir: t.TempDir()}
	t.Setenv("HOME", h.home)
	t.Chdir(h.workDir) // the executor runs the preflight from a neutral directory
	h.globalDir = filepath.Join(h.home, ".scion")
	require.NoError(t, os.MkdirAll(h.globalDir, 0o755))
	h.writeSettings(t, "")
	h.dbPath = filepath.Join(h.globalDir, "hub.db")
	client, err := entc.OpenSQLite("file:"+h.dbPath+"?cache=shared", entc.PoolConfig{})
	require.NoError(t, err)
	h.client = client
	h.store = entadapter.NewCompositeStore(client)
	t.Cleanup(func() { _ = h.store.Close() })
	require.NoError(t, migrateHubStore(context.Background(), h.store, false))
	h.brokerID = uuid.NewString()
	require.NoError(t, h.store.CreateRuntimeBroker(context.Background(), &store.RuntimeBroker{
		ID: h.brokerID, Name: "co-located", Slug: "co-located", Status: store.BrokerStatusOnline,
	}))
	return h
}

// writeSettings writes the global settings file (active_profile a,
// profile a on docker) plus extra YAML.
func (h *preflightHub) writeSettings(t *testing.T, extra string) {
	t.Helper()
	body := "schema_version: \"1\"\nactive_profile: a\nprofiles:\n  a:\n    runtime: docker\n" + extra
	require.NoError(t, os.WriteFile(filepath.Join(h.globalDir, "settings.yaml"), []byte(body), 0o644))
}

// profilesRow stores a hub_settings profiles row holding names (docker).
// updatedBy nil leaves the column NULL.
func (h *preflightHub) profilesRow(t *testing.T, origin hubsetting.Origin, updatedBy *string, names ...string) {
	t.Helper()
	doc := map[string]config.V1ProfileConfig{}
	for _, n := range names {
		doc[n] = config.V1ProfileConfig{Runtime: "docker"}
	}
	data, err := json.Marshal(doc)
	require.NoError(t, err)
	c := h.client.HubSetting.Create().SetSection("profiles").SetValue(data).SetOrigin(origin)
	if updatedBy != nil {
		c.SetUpdatedBy(*updatedBy)
	}
	require.NoError(t, c.Exec(context.Background()))
}

// project creates a project, returning its ID.
func (h *preflightHub) project(t *testing.T, p *store.Project) string {
	t.Helper()
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if p.Name == "" {
		p.Name = p.Slug
	}
	require.NoError(t, h.store.CreateProject(context.Background(), p))
	return p.ID
}

// agent creates an agent; raw, when set, overwrites applied_config past
// the store.
func (h *preflightHub) agent(t *testing.T, a *store.Agent, raw string) string {
	t.Helper()
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.Name == "" {
		a.Name = a.Slug
	}
	require.NoError(t, h.store.CreateAgent(context.Background(), a))
	if raw != "" {
		require.NoError(t, h.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetAppliedConfig(raw).Exec(context.Background()))
	}
	return a.ID
}

// blocking seeds the post-sync blocking case: a seeded profiles row {a, b}
// that the boot-time sync replaces with the bootstrap {a}, and an unplaced
// agent saved on b.
func (h *preflightHub) blocking(t *testing.T) {
	t.Helper()
	seed := "seed"
	h.profilesRow(t, hubsetting.OriginSeeded, &seed, "a", "b")
	pid := h.project(t, &store.Project{Slug: "one"})
	h.agent(t, &store.Agent{Slug: "w", ProjectID: pid, AppliedConfig: &store.AgentAppliedConfig{Profile: "b"}}, "")
}

// executor is the options of an executor-invoked run on h's database.
func (h *preflightHub) executor() preflightOptions {
	return preflightOptions{executor: true, brokerID: h.brokerID, envDSN: h.dbPath}
}

// decodeReport decodes stdout as exactly one preflight report.
func decodeReport(t *testing.T, stdout string) preflight.Report {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stdout))
	var rep preflight.Report
	require.NoError(t, dec.Decode(&rep), "stdout must be one JSON document: %q", stdout)
	require.False(t, dec.More(), "stdout must hold exactly one JSON document: %q", stdout)
	return rep
}

// reportCodes lists the report's finding codes.
func reportCodes(rep preflight.Report) []string {
	out := []string{}
	for _, f := range rep.Findings {
		out = append(out, f.Code)
	}
	return out
}

// openPreflightSnapshot opens path the way the preflight does and returns
// a reader bound to one snapshot.
func openPreflightSnapshot(t *testing.T, brokerID, path string) *preflightReader {
	t.Helper()
	client, err := openPreflightClient("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	tx, err := client.BeginTx(context.Background(), snapshotTxOptions(dialect.SQLite))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return newPreflightReader(tx.Client(), brokerID)
}

// rawDB returns the read-write fixture's *sql.DB.
func (h *preflightHub) rawDB(t *testing.T) *sql.DB {
	t.Helper()
	drv, ok := h.client.Driver().(*entsql.Driver)
	require.True(t, ok)
	return drv.DB()
}

// T1: one unread, non-indexed, non-FK column dropped from each table read;
// the reads succeed with the exact rows.
func TestServerPreflight_MissingColumnFixture(t *testing.T) {
	h := newPreflightHub(t)
	ctx := context.Background()
	admin := "admin"
	h.profilesRow(t, hubsetting.OriginManaged, &admin, "a")
	pid := h.project(t, &store.Project{Slug: "one"})
	require.NoError(t, h.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: pid, BrokerID: h.brokerID, BrokerName: "co-located", LocalPath: "/srv/one", Status: store.BrokerStatusOnline,
	}))
	h.agent(t, &store.Agent{Slug: "w", ProjectID: pid, RuntimeBrokerID: h.brokerID, AppliedConfig: &store.AgentAppliedConfig{Profile: "a"}}, "")
	db := h.rawDB(t)
	for _, stmt := range []string{
		"ALTER TABLE hub_settings DROP COLUMN revision",
		"ALTER TABLE agents DROP COLUMN message",
		"ALTER TABLE runtime_brokers DROP COLUMN version",
		"ALTER TABLE projects DROP COLUMN shared_dirs",
		"ALTER TABLE project_contributors DROP COLUMN broker_name",
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}

	r := openPreflightSnapshot(t, h.brokerID, h.dbPath)
	found, flat, err := r.Broker(ctx, h.brokerID)
	require.NoError(t, err)
	assert.True(t, found)
	assert.False(t, flat)
	settings, err := r.HubSettings(ctx, []string{"runtimes", "profiles"})
	require.NoError(t, err)
	require.Len(t, settings, 1)
	assert.Equal(t, "profiles", settings[0].Section)
	assert.Equal(t, "managed", settings[0].Origin)
	require.NotNil(t, settings[0].UpdatedBy)
	assert.Equal(t, "admin", *settings[0].UpdatedBy)
	assert.JSONEq(t, `{"a":{"runtime":"docker"}}`, string(settings[0].Value))
	agents, unparseable, err := r.Agents(ctx)
	require.NoError(t, err)
	assert.Equal(t, []agent.ProfileRefAgentRow{{Slug: "w", ProjectID: pid, RuntimeBrokerID: h.brokerID, AppliedProfile: "a"}}, agents)
	assert.Zero(t, unparseable)
	projects, err := r.Projects(ctx)
	require.NoError(t, err)
	assert.Equal(t, []agent.ProfileRefProject{{ID: pid, Slug: "one"}}, projects)
	providers, err := r.Providers(ctx, h.brokerID)
	require.NoError(t, err)
	assert.Equal(t, []agent.ProfileRefProvider{{ProjectID: pid, LocalPath: "/srv/one"}}, providers)
}

// countingDriver counts the queries made outside and inside transactions.
type countingDriver struct {
	dialect.Driver
	mu                               sync.Mutex
	begins, outside, inside, commits int
	rollbacks                        int
}

func (d *countingDriver) count(p *int) { d.mu.Lock(); *p++; d.mu.Unlock() }

func (d *countingDriver) Exec(ctx context.Context, q string, args, v any) error {
	d.count(&d.outside)
	return d.Driver.Exec(ctx, q, args, v)
}

func (d *countingDriver) Query(ctx context.Context, q string, args, v any) error {
	d.count(&d.outside)
	return d.Driver.Query(ctx, q, args, v)
}

func (d *countingDriver) Tx(ctx context.Context) (dialect.Tx, error) { return d.BeginTx(ctx, nil) }

func (d *countingDriver) BeginTx(ctx context.Context, opts *sql.TxOptions) (dialect.Tx, error) {
	d.count(&d.begins)
	tx, err := d.Driver.(interface {
		BeginTx(context.Context, *sql.TxOptions) (dialect.Tx, error)
	}).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &countingTx{Tx: tx, d: d}, nil
}

type countingTx struct {
	dialect.Tx
	d *countingDriver
}

func (t *countingTx) Exec(ctx context.Context, q string, args, v any) error {
	t.d.count(&t.d.inside)
	return t.Tx.Exec(ctx, q, args, v)
}

func (t *countingTx) Query(ctx context.Context, q string, args, v any) error {
	t.d.count(&t.d.inside)
	return t.Tx.Query(ctx, q, args, v)
}

func (t *countingTx) Commit() error   { t.d.count(&t.d.commits); return t.Tx.Commit() }
func (t *countingTx) Rollback() error { t.d.count(&t.d.rollbacks); return t.Tx.Rollback() }

// T2: every read of a run happens inside exactly one transaction, which is
// rolled back, never committed.
func TestServerPreflight_SingleSnapshot(t *testing.T) {
	h := newPreflightHub(t)
	h.blocking(t)
	counter := &countingDriver{}
	orig := openPreflightClient
	t.Cleanup(func() { openPreflightClient = orig })
	openPreflightClient = func(driver, dsn string) (*ent.Client, error) {
		c, err := orig(driver, dsn)
		if err != nil {
			return nil, err
		}
		counter.Driver = c.Driver()
		return ent.NewClient(ent.Driver(counter)), nil
	}

	_, _, err := runPreflightForTest(t, h.executor())
	require.Error(t, err, "the fixture blocks")

	assert.Equal(t, 1, counter.begins, "exactly one snapshot transaction")
	assert.Zero(t, counter.outside, "no query outside the snapshot")
	assert.GreaterOrEqual(t, counter.inside, 5, "the five reads run inside the snapshot")
	assert.Zero(t, counter.commits, "the snapshot is never committed")
	assert.Equal(t, 1, counter.rollbacks, "the snapshot is rolled back")
}

// T3: a write through the opened client, or through its transaction, fails.
func TestServerPreflight_WriteFails(t *testing.T) {
	h := newPreflightHub(t)
	before := fileState(t, h.dbPath)
	client, err := openPreflightClient("sqlite", h.dbPath)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	err = client.HubSetting.Create().SetSection("x").SetValue(json.RawMessage(`{}`)).Exec(ctx)
	require.Error(t, err, "a write through the read-only client must fail")
	assert.Contains(t, err.Error(), "readonly database")
	tx, err := client.BeginTx(ctx, snapshotTxOptions(dialect.SQLite))
	require.NoError(t, err)
	err = tx.HubSetting.Create().SetSection("y").SetValue(json.RawMessage(`{}`)).Exec(ctx)
	require.Error(t, err, "a write through the snapshot must fail")
	_ = tx.Rollback()
	require.NoError(t, h.store.Close())
	assert.Equal(t, before, fileState(t, h.dbPath), "the database file is unchanged")
}

// fileState is a file's sha256 and mtime.
func fileState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%s %d", hex.EncodeToString(sum[:]), info.ModTime().UnixNano())
}

// treeState lists every entry under root (symlinks not followed) with its
// mode and its link target or content hash, and its mtime (directories
// too, so a transient create-and-remove shows).
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		v := info.Mode().String()
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			v += " -> " + target
		case info.Mode().IsRegular():
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			v += fmt.Sprintf(" %s %d", hex.EncodeToString(sum[:]), info.ModTime().UnixNano())
		case info.IsDir():
			v += fmt.Sprintf(" %d", info.ModTime().UnixNano())
		}
		out[rel] = v
		return nil
	}))
	return out
}

// socketFDs counts this process's open socket descriptors (Linux).
func socketFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	require.NoError(t, err)
	n := 0
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name())); err == nil && strings.HasPrefix(target, "socket:") {
			n++
		}
	}
	return n
}

// T4: an executor-invoked run changes no file: not the database, not the
// legacy settings key, the legacy global layout, a provider's legacy
// marker or the missing context file. Only SQLite's own -wal and -shm
// sidecars may appear, when the stopped database had none; and no socket
// is left open.
func TestServerPreflight_NoFileChangeNoListener(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the socket check reads /proc/self/fd")
	}
	h := newPreflightHub(t)
	ctx := context.Background()
	// The legacy hub project ID key in the global settings.
	h.writeSettings(t, legacyHubProjectIDYAML("legacy-project"))
	// A leftover legacy layout: a real directory and a dangling back-link.
	require.NoError(t, os.MkdirAll(filepath.Join(h.globalDir, legacyLayoutDir, "acme"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(h.globalDir, legacyLayoutDir, "acme", "workspace.txt"), []byte("hello"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join("..", "projects", "gone"), filepath.Join(h.globalDir, legacyLayoutDir, "gone")))
	// A provider whose project marker has legacy keys, in a git work tree.
	repo := filepath.Join(h.home, "src", "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".git"), 0o755))
	marker := filepath.Join(repo, config.DotScion)
	require.NoError(t, os.WriteFile(marker, []byte(legacyMarkerYAML("legacy-marker", "repo")), 0o644))
	pid := h.project(t, &store.Project{Slug: "repo"})
	require.NoError(t, h.store.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: pid, BrokerID: h.brokerID, BrokerName: "co-located", LocalPath: marker, Status: store.BrokerStatusOnline,
	}))
	h.agent(t, &store.Agent{Slug: "w", ProjectID: pid, RuntimeBrokerID: h.brokerID, AppliedConfig: &store.AgentAppliedConfig{Profile: "a"}}, "")
	require.NoFileExists(t, filepath.Join(h.globalDir, "context.yaml"))
	require.NoError(t, h.store.Close())
	// The database lives outside HOME, in its own directory
	// (SCION_PREFLIGHT_DB names it), so SQLite's own read-only WAL sidecars
	// (a known limit) never touch the settings directory: the whole HOME
	// tree, ~/.scion's mtime included, must be identical after the run.
	dbDir := t.TempDir()
	dbOut := filepath.Join(dbDir, "hub.db")
	require.NoError(t, os.Rename(h.dbPath, dbOut))
	// A --config directory whose settings file has a legacy key (audit row
	// 7: the bootstrap material is read from it).
	cfgDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "settings.yaml"), []byte("schema_version: \"1\"\n"+legacyHubProjectIDYAML("legacy-config")), 0o644))
	o := h.executor()
	o.envDSN = dbOut
	o.configPath = filepath.Join(cfgDir, "settings.yaml")
	roots := []string{h.home, h.workDir, cfgDir}
	before := map[string]map[string]string{}
	for _, r := range roots {
		before[r] = treeState(t, r)
	}
	dbBefore := treeState(t, dbDir)
	sockets := socketFDs(t)

	stdout, _, err := runPreflightForTest(t, o)
	require.NoError(t, err)
	rep := decodeReport(t, stdout)
	// The co-located agent's agent-info.json was looked up on disk under the
	// provider's path (and is missing): the run took the disk path.
	assert.Contains(t, reportCodes(rep), "settings.profiles.agent_info_missing")

	assert.Contains(t, before[h.home], filepath.Join(".scion", legacyLayoutDir, "acme"), "the fixture keeps a leftover legacy layout")
	for _, r := range roots {
		after := treeState(t, r)
		if r == h.home {
			assert.NotContains(t, after, ".scion/projects", "under the switch the layout is not migrated, and the preflight runs without projects/")
		}
		assert.Equal(t, before[r], after, "the preflight must change no file under %s", r)
	}
	// The database's directory: the database file is unchanged, and the
	// only new entries are SQLite's own sidecars (which also move the
	// directory's mtime).
	dbAfter := treeState(t, dbDir)
	assert.Equal(t, dbBefore["hub.db"], dbAfter["hub.db"], "the database file is unchanged")
	for _, sidecar := range []string{"hub.db-wal", "hub.db-shm"} {
		assert.Contains(t, dbAfter, sidecar, "SQLite's read-only WAL sidecar %s", sidecar)
		delete(dbAfter, sidecar)
	}
	delete(dbAfter, ".")
	delete(dbBefore, ".")
	assert.Equal(t, dbBefore, dbAfter, "nothing but SQLite's sidecars appears beside the database")
	// A listener or a connection left open only adds sockets; another test's
	// socket closing meanwhile must not fail this check.
	assert.LessOrEqual(t, socketFDs(t), sockets, "the preflight must leave no socket open")

	// Positive control: each snapshot detects a write the audit did not
	// list, so a tenth write-on-read path fails this test rather than
	// passing unseen.
	for _, c := range []struct{ root, write string }{
		{h.home, filepath.Join(h.globalDir, "zz-unaudited-write")},
		{h.workDir, filepath.Join(h.workDir, "zz-unaudited-write")},
		{cfgDir, filepath.Join(cfgDir, "zz-unaudited-write")},
		{dbDir, filepath.Join(dbDir, "zz-unaudited-write")},
	} {
		prev := treeState(t, c.root)
		require.NoError(t, os.WriteFile(c.write, []byte("x"), 0o644))
		assert.NotEqual(t, prev, treeState(t, c.root), "control: the snapshot of %s must see a new file", c.root)
		require.NoError(t, os.Remove(c.write))
	}
}

// The known limit behind T4's separate database directory: a read-only
// open of a stopped WAL database (no -wal/-shm files) leaves SQLite's own
// sidecars, and nothing else; the database file itself is unchanged. The database sits in its
// own directory here, so the settings directory is not involved.
func TestServerPreflight_StoppedWALSidecars(t *testing.T) {
	h := newPreflightHub(t)
	require.NoError(t, h.store.Close())
	dir := t.TempDir()
	db := filepath.Join(dir, "hub.db")
	b, err := os.ReadFile(h.dbPath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(db, b, 0o644))
	before := treeState(t, dir)
	o := h.executor()
	o.envDSN = db
	_, _, err = runPreflightForTest(t, o)
	require.NoError(t, err)
	after := treeState(t, dir)
	assert.Equal(t, before["hub.db"], after["hub.db"], "the database file is unchanged")
	for _, sidecar := range []string{"hub.db-wal", "hub.db-shm"} {
		assert.NotContains(t, before, sidecar, "a stopped database has no %s", sidecar)
		assert.Contains(t, after, sidecar, "the read-only open leaves SQLite's %s (the known limit)", sidecar)
		delete(after, sidecar)
	}
	delete(after, ".")
	delete(before, ".")
	assert.Equal(t, before, after, "only SQLite's own sidecars may appear")
}

// T5: a missing database is an error and is never created, whatever form
// the DSN takes.
func TestServerPreflight_MissingDBNotCreated(t *testing.T) {
	newPreflightHub(t)
	missing := filepath.Join(t.TempDir(), "missing.db")
	for _, dsn := range []string{missing, "file:" + missing, "file:" + missing + "?mode=rwc"} {
		t.Run(dsn, func(t *testing.T) {
			_, _, err := runPreflightForTest(t, preflightOptions{dbFlag: dsn})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "opening the sqlite hub database read only")
			assert.NoFileExists(t, missing, "a missing database must not be created")
		})
	}
}

// T6: the opened connection has the busy timeout and query_only, and the
// DSN is read only on its own (mode=ro): with query_only off, a write
// still fails with SQLITE_READONLY.
func TestServerPreflight_DBIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.db")
	rw, err := sql.Open("sqlite", "file:"+path)
	require.NoError(t, err)
	_, err = rw.Exec("CREATE TABLE t(x)")
	require.NoError(t, err)
	_, err = rw.Exec("INSERT INTO t VALUES(1)") // control: the file is writable
	require.NoError(t, err)
	require.NoError(t, rw.Close())

	client, err := openPreflightClient("sqlite", path)
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	db := client.Driver().(*entsql.Driver).DB()
	var busy, queryOnly int
	require.NoError(t, db.QueryRow("PRAGMA busy_timeout").Scan(&busy))
	require.NoError(t, db.QueryRow("PRAGMA query_only").Scan(&queryOnly))
	assert.Equal(t, 5000, busy, "busy_timeout")
	assert.Equal(t, 1, queryOnly, "query_only")

	ro, err := sql.Open("sqlite", preflightSQLiteDSN(path)) // mode=ro, no query_only
	require.NoError(t, err)
	defer func() { _ = ro.Close() }()
	require.NoError(t, ro.QueryRow("PRAGMA query_only").Scan(&queryOnly))
	require.Equal(t, 0, queryOnly)
	_, err = ro.Exec("INSERT INTO t VALUES(2)")
	require.Error(t, err, "mode=ro alone must refuse a write")
	assert.Contains(t, err.Error(), "attempt to write a readonly database (8)")
}

// T7: executor-invoked without SCION_PREFLIGHT_DB is an internal error,
// never a fallback to --db or ~/.scion/hub.db (both hold a blocking
// fixture here).
func TestServerPreflight_ExecutorRequiresEnv(t *testing.T) {
	h := newPreflightHub(t)
	h.blocking(t)
	o := h.executor()
	o.envDSN, o.dbFlag = "", h.dbPath
	stdout, _, err := runPreflightForTest(t, o)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SCION_PREFLIGHT_DB is required when --colocated-broker-id is given")
	assert.Equal(t, 1, exitCodeFor(err), "an internal error, not the blocking exit 3")
	assert.Empty(t, stdout, "no report")
}

// T8: the DSN precedence, the executor driver, and the ignored variable.
func TestServerPreflight_DSNPrecedence(t *testing.T) {
	cfg := func(driver, url string) *config.GlobalConfig {
		c := &config.GlobalConfig{}
		c.Database.Driver, c.Database.URL = driver, url
		return c
	}
	for _, c := range []struct {
		name               string
		executor           bool
		env, flag          string
		cfg                *config.GlobalConfig
		wantDriver, wantDS string
	}{
		{"by hand: --db wins", false, "", "/x/flag.db", cfg("sqlite", "/x/config.db"), "sqlite", "/x/flag.db"},
		{"by hand: a postgres --db", false, "", "postgres://h/db", cfg("sqlite", "/x/config.db"), "postgres", "postgres://h/db"},
		{"by hand: the config", false, "", "", cfg("sqlite", "/x/config.db"), "sqlite", "/x/config.db"},
		{"by hand: the environment variable is ignored", false, "/x/env.db", "", cfg("sqlite", "/x/config.db"), "sqlite", "/x/config.db"},
		{"executor: the environment variable wins over --db", true, "/x/env.db", "/x/flag.db", cfg("sqlite", "/x/config.db"), "sqlite", "/x/env.db"},
		{"executor: the driver comes from the config", true, "host=db user=hub", "", cfg("postgres", ""), "postgres", "host=db user=hub"},
	} {
		t.Run(c.name, func(t *testing.T) {
			driver, dsn := resolvePreflightDSN(c.executor, c.env, c.flag, c.cfg)
			assert.Equal(t, c.wantDriver, driver)
			assert.Equal(t, c.wantDS, dsn)
		})
	}

	t.Run("the default is ~/.scion/hub.db", func(t *testing.T) {
		newPreflightHub(t)
		stdout, _, err := runPreflightForTest(t, preflightOptions{})
		require.NoError(t, err)
		assert.Equal(t, []string{"settings.profiles.not_applicable"}, reportCodes(decodeReport(t, stdout)))
	})
	t.Run("the ignored variable is named, never its value", func(t *testing.T) {
		h := newPreflightHub(t)
		_, stderr, err := runPreflightForTest(t, preflightOptions{dbFlag: h.dbPath, envDSN: "/secret/value.db"})
		require.NoError(t, err)
		assert.Contains(t, stderr, "SCION_PREFLIGHT_DB is ignored without --colocated-broker-id")
		assert.NotContains(t, stderr, "/secret/value.db")
	})
}

// T9: a Postgres DSN's password appears in no form in the error or on
// stderr, for the URL and the keyword forms.
func TestServerPreflight_DSNRedacted(t *testing.T) {
	const secret = "s3cr3t-pw-91"
	for _, c := range []struct {
		name string
		o    func(t *testing.T, h *preflightHub) preflightOptions
	}{
		{"URL form", func(t *testing.T, h *preflightHub) preflightOptions {
			return preflightOptions{dbFlag: "postgres://hub:" + secret + "@127.0.0.1:1/hub?connect_timeout=1&sslmode=disable"}
		}},
		{"Unix-socket URL form", func(t *testing.T, h *preflightHub) preflightOptions {
			return preflightOptions{dbFlag: "postgres://hub:" + secret + "@/hub?host=/nonexistent-socket-dir&connect_timeout=1&sslmode=disable"}
		}},
		{"keyword form", func(t *testing.T, h *preflightHub) preflightOptions {
			t.Setenv("SCION_SERVER_DATABASE_DRIVER", "postgres")
			return preflightOptions{executor: true, brokerID: h.brokerID,
				envDSN: "host=127.0.0.1 port=1 user=hub password='" + secret + "' dbname=hub connect_timeout=1 sslmode=disable"}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPreflightHub(t)
			o := c.o(t, h)
			_, stderr, err := runPreflightForTest(t, o)
			require.Error(t, err, "nothing listens on port 1")
			assert.NotContains(t, err.Error(), secret)
			assert.NotContains(t, stderr, secret)
			assert.Equal(t, 1, exitCodeFor(err))
		})
	}
	// An open error that quotes the DSN raw (pgx redacts its own errors, so
	// a driver stand-in quotes it): the command's redaction masks it.
	for _, dsn := range []string{
		"postgres://hub:" + secret + "@db.invalid/hub",
		"postgres://hub:" + secret + "@/hub?host=/cloudsql/proj:region:db",
		"host=db.invalid user=hub password='" + secret + "' dbname=hub",
	} {
		t.Run("an open error quoting "+strings.SplitN(dsn, ":", 2)[0], func(t *testing.T) {
			h := newPreflightHub(t)
			t.Setenv("SCION_SERVER_DATABASE_DRIVER", "postgres")
			orig := openPreflightClient
			t.Cleanup(func() { openPreflightClient = orig })
			openPreflightClient = func(driver, d string) (*ent.Client, error) {
				return nil, fmt.Errorf("dial %s: connection refused (dsn %q)", d, d)
			}
			_, stderr, err := runPreflightForTest(t, preflightOptions{executor: true, brokerID: h.brokerID, envDSN: dsn})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "opening the postgres hub database read only")
			assert.NotContains(t, err.Error(), secret, "the open error must not quote the password")
			assert.NotContains(t, stderr, secret)
		})
	}
}

// T10: the report and the exit codes.
func TestServerPreflight_ReportAndExitCodes(t *testing.T) {
	t.Run("blocking: exit 3, one JSON document, no banner", func(t *testing.T) {
		h := newPreflightHub(t)
		h.blocking(t)
		stdout, _, err := runPreflightForTest(t, h.executor())
		require.Error(t, err)
		assert.Equal(t, 3, exitCodeFor(err))
		assert.True(t, isReportedInJSON(err), "the report was printed: Execute prints no banner")
		rep := decodeReport(t, stdout)
		assert.Equal(t, preflight.ReportVersion, rep.Version)
		assert.Equal(t, []string{"settings.profiles"}, rep.Checks)
		blocks, warns := 0, 0
		for _, f := range rep.Findings {
			switch f.Severity {
			case preflight.SeverityBlock:
				blocks++
			case preflight.SeverityWarn:
				warns++
			}
		}
		assert.Equal(t, rep.Blocking, blocks)
		assert.Equal(t, rep.Warnings, warns)
		assert.Contains(t, reportCodes(rep), "settings.profiles.agent_saved_profile")
	})
	t.Run("clean: exit 0", func(t *testing.T) {
		h := newPreflightHub(t)
		admin := "admin"
		h.profilesRow(t, hubsetting.OriginManaged, &admin, "a")
		stdout, _, err := runPreflightForTest(t, h.executor())
		require.NoError(t, err)
		assert.Zero(t, decodeReport(t, stdout).Blocking)
	})
	t.Run("hub only: exactly the not_applicable information", func(t *testing.T) {
		h := newPreflightHub(t)
		o := h.executor()
		o.brokerID = ""
		stdout, _, err := runPreflightForTest(t, o)
		require.NoError(t, err)
		rep := decodeReport(t, stdout)
		require.Len(t, rep.Findings, 1)
		assert.Equal(t, "settings.profiles.not_applicable", rep.Findings[0].Code)
		assert.Equal(t, preflight.SeverityInfo, rep.Findings[0].Severity)
	})
}

// T11: the text rendering, for the default and for --format plain.
func TestServerPreflight_TextOutput(t *testing.T) {
	for _, format := range []string{"text", "plain"} {
		t.Run(format, func(t *testing.T) {
			h := newPreflightHub(t)
			h.blocking(t)
			o := h.executor()
			o.format = format
			stdout, _, err := runPreflightForTest(t, o)
			require.Error(t, err)
			assert.Equal(t, 3, exitCodeFor(err))
			assert.False(t, isReportedInJSON(err), "text output: Execute prints the one-line error")
			assert.Contains(t, err.Error(), "preflight found 1 blocking finding(s)")
			assert.Contains(t, stdout, "preflight: 1 blocking")
			assert.Contains(t, stdout, "[BLOCK] settings.profiles.agent_saved_profile: ")
			assert.Contains(t, stdout, "  subjects: one/w\n")
			assert.Contains(t, stdout, "  fix: ")
		})
	}
	t.Run("truncated subjects", func(t *testing.T) {
		var b bytes.Buffer
		require.NoError(t, writePreflightText(&b, preflight.Report{Checks: []string{"x.y"}, Findings: []preflight.Finding{
			{Code: "x.y.z", Severity: preflight.SeverityWarn, Message: "m", Subjects: []string{"a", "b"}, SubjectCount: 5},
		}, Warnings: 1}))
		assert.Contains(t, b.String(), "  subjects: a, b (+3 more)\n")
	})
}

// T12: the reader through real rows and columns.
func TestServerPreflight_RowsThroughRealColumns(t *testing.T) {
	h := newPreflightHub(t)
	ctx := context.Background()
	other := uuid.NewString()
	require.NoError(t, h.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: other, Name: "other", Slug: "other", Status: store.BrokerStatusOnline}))
	flatID := uuid.NewString()
	require.NoError(t, h.store.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: flatID, Name: "flat", Slug: "flat", Status: store.BrokerStatusOnline}))
	require.NoError(t, h.client.RuntimeBroker.UpdateOneID(uuid.MustParse(flatID)).SetRuntimeTargetID("target-1").Exec(ctx))
	// hub_settings: a NULL updated_by and a pre-origin one.
	require.NoError(t, h.client.HubSetting.Create().SetSection("profiles").SetValue(json.RawMessage(`{}`)).SetOrigin(hubsetting.OriginSeeded).Exec(ctx))
	require.NoError(t, h.client.HubSetting.Create().SetSection("runtimes").SetValue(json.RawMessage(`{}`)).SetOrigin(hubsetting.OriginSeeded).SetUpdatedBy("admin").Exec(ctx))
	require.NoError(t, h.client.HubSetting.Create().SetSection("other").SetValue(json.RawMessage(`{}`)).SetOrigin(hubsetting.OriginSeeded).Exec(ctx))
	shared := h.project(t, &store.Project{Slug: "shared", GitRemote: "https://example.com/s.git",
		Labels:      map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared},
		Annotations: map[string]string{store.AnnotationProjectActiveProfile: "act"}})
	plain := h.project(t, &store.Project{Slug: "plain", Labels: map[string]string{store.LabelWorkspaceMode: store.WorkspaceModeShared}})
	require.NoError(t, h.store.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: shared, BrokerID: h.brokerID, BrokerName: "co-located", LocalPath: "/srv/shared", Status: store.BrokerStatusOnline}))
	require.NoError(t, h.store.AddProjectProvider(ctx, &store.ProjectProvider{ProjectID: plain, BrokerID: other, BrokerName: "other", LocalPath: "/srv/other", Status: store.BrokerStatusOnline}))
	h.agent(t, &store.Agent{Slug: "colo", ProjectID: shared, RuntimeBrokerID: h.brokerID, AppliedConfig: &store.AgentAppliedConfig{Profile: "p1"}}, "")
	h.agent(t, &store.Agent{Slug: "unplaced", ProjectID: shared}, "")
	h.agent(t, &store.Agent{Slug: "remote", ProjectID: plain, RuntimeBrokerID: other, AppliedConfig: &store.AgentAppliedConfig{Profile: "p2"}}, "")
	deleted := h.agent(t, &store.Agent{Slug: "deleted", ProjectID: shared, RuntimeBrokerID: h.brokerID}, "")
	require.NoError(t, h.client.Agent.UpdateOneID(uuid.MustParse(deleted)).SetDeletedAt(time.Now()).Exec(ctx))
	h.agent(t, &store.Agent{Slug: "bad-json-colo", ProjectID: shared, RuntimeBrokerID: h.brokerID}, `{"profile":"x"`)
	h.agent(t, &store.Agent{Slug: "bad-type-unplaced", ProjectID: shared}, `{"profile":"x","thinkingLevel":"high"}`)
	h.agent(t, &store.Agent{Slug: "bad-json-remote", ProjectID: plain, RuntimeBrokerID: other}, `{"profile":"x"`)

	r := openPreflightSnapshot(t, h.brokerID, h.dbPath)
	for _, c := range []struct {
		id          string
		found, flat bool
	}{{h.brokerID, true, false}, {flatID, true, true}, {uuid.NewString(), false, false}, {"not-a-uuid", false, false}} {
		found, flat, err := r.Broker(ctx, c.id)
		require.NoError(t, err)
		assert.Equal(t, [2]bool{c.found, c.flat}, [2]bool{found, flat}, "broker %s: found, flat", c.id)
	}

	settings, err := r.HubSettings(ctx, []string{"runtimes", "profiles"})
	require.NoError(t, err)
	bySection := map[string]preflight.HubSettingRow{}
	for _, s := range settings {
		bySection[s.Section] = s
	}
	require.Len(t, bySection, 2, "only the requested sections")
	assert.Nil(t, bySection["profiles"].UpdatedBy, "a NULL updated_by reads as nil")
	require.NotNil(t, bySection["runtimes"].UpdatedBy)
	assert.Equal(t, "admin", *bySection["runtimes"].UpdatedBy, "a pre-origin updated_by is kept")

	agents, unparseable, err := r.Agents(ctx)
	require.NoError(t, err)
	got := map[string]agent.ProfileRefAgentRow{}
	for _, a := range agents {
		got[a.Slug] = a
	}
	assert.NotContains(t, got, "deleted", "a deleted agent is not read")
	assert.Len(t, got, 6)
	assert.Equal(t, agent.ProfileRefAgentRow{Slug: "colo", ProjectID: shared, RuntimeBrokerID: h.brokerID, AppliedProfile: "p1"}, got["colo"])
	assert.Equal(t, agent.ProfileRefAgentRow{Slug: "remote", ProjectID: plain, RuntimeBrokerID: other, AppliedProfile: "p2"}, got["remote"])
	assert.Equal(t, "", got["bad-json-colo"].AppliedProfile)
	assert.Equal(t, 2, unparseable, "the co-located and the unplaced undecodable configs are tallied; the remote one, and the NULL ones, are not")

	projects, err := r.Projects(ctx)
	require.NoError(t, err)
	sort.Slice(projects, func(i, j int) bool { return projects[i].Slug < projects[j].Slug })
	assert.Equal(t, []agent.ProfileRefProject{
		{ID: plain, Slug: "plain"}, // the shared label without a git remote is not a shared workspace
		{ID: shared, Slug: "shared", ActiveProfile: "act", SharedWorkspace: true},
	}, projects)

	providers, err := r.Providers(ctx, h.brokerID)
	require.NoError(t, err)
	assert.Equal(t, []agent.ProfileRefProvider{{ProjectID: shared, LocalPath: "/srv/shared"}}, providers, "only the co-located broker's providers")
}

// T13: hidden, and human mode only (D-1): agent mode removes the whole
// server subtree, the hidden preflight included.
func TestServerPreflightCmd_HiddenAndHumanOnly(t *testing.T) {
	real := resolveCommandPath(rootCmd, "server.preflight")
	require.NotNil(t, real, "server preflight must exist in the real command tree")
	assert.True(t, real.Hidden, "server preflight must be hidden")

	build := func() *cobra.Command {
		root := &cobra.Command{Use: "scion"}
		root.AddCommand(cloneCommandShape(resolveCommandPath(rootCmd, "server")))
		return root
	}
	t.Setenv("SCION_CLI_MODE", "agent")
	root := build()
	applyModeRestrictions(root, resolveMode())
	assert.Nil(t, resolveCommandPath(root, "server.preflight"), "server preflight must be removed in agent mode")

	t.Setenv("SCION_CLI_MODE", "human")
	root = build()
	applyModeRestrictions(root, resolveMode())
	assert.NotNil(t, resolveCommandPath(root, "server.preflight"), "human mode keeps server preflight")
}

// T14: the snapshot options and the Postgres pool.
func TestSnapshotTxOptions(t *testing.T) {
	assert.Equal(t, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}, snapshotTxOptions(dialect.Postgres))
	assert.Nil(t, snapshotTxOptions(dialect.SQLite))
	assert.Equal(t, entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 2}, preflightPostgresPool())
}

// T15 (S-1): an executor-invoked run withholds error text from stderr,
// which becomes the persisted run log; a run by hand shows it.
func TestServerPreflight_ExecutorStderrWithholdsErrorText(t *testing.T) {
	const secret = "s3cr3t-tok-63"
	t.Run("handler: any error value, any key, inside groups", func(t *testing.T) {
		var b bytes.Buffer
		l := slog.New(&withholdErrorsHandler{inner: slog.NewTextHandler(&b, nil), withhold: true})
		boom := errors.New("cannot decode value '" + secret + "'")
		l.Warn("m", "err", boom, slog.Group("g", "cause", boom), "plain", "kept")
		l.With("e", boom).Warn("with")
		out := b.String()
		assert.NotContains(t, out, secret)
		assert.Contains(t, out, "err=\"<*errors.errorString>; "+withheldNote+"\"")
		assert.Contains(t, out, "g.cause=\"<*errors.errorString>; "+withheldNote+"\"")
		assert.Contains(t, out, "e=\"<*errors.errorString>; "+withheldNote+"\"")
		assert.Contains(t, out, "plain=kept")

		b.Reset()
		slog.New(&withholdErrorsHandler{inner: slog.NewTextHandler(&b, nil)}).Warn("m", "err", boom)
		assert.Contains(t, b.String(), secret, "by hand, the text is shown")
	})

	t.Run("a run logs an error: withheld when executor-invoked, shown by hand", func(t *testing.T) {
		// A probe through the client seam logs an error during the run,
		// as the check does for a loader or read error.
		orig := openPreflightClient
		t.Cleanup(func() { openPreflightClient = orig })
		openPreflightClient = func(driver, dsn string) (*ent.Client, error) {
			slog.Warn("probe", "error", errors.New("cannot convert value '"+secret+"'"))
			return orig(driver, dsn)
		}
		h := newPreflightHub(t)
		_, stderr, err := runPreflightForTest(t, h.executor())
		require.NoError(t, err)
		assert.NotContains(t, stderr, secret, "the executor's stderr must not quote the error")
		assert.Contains(t, stderr, "error=\"<*errors.errorString>; "+withheldNote+"\"")
		_, handStderr, err := runPreflightForTest(t, preflightOptions{dbFlag: h.dbPath})
		require.NoError(t, err)
		assert.Contains(t, handStderr, secret, "by hand, the error text is shown")
	})

	// The real loaders. Each fixture makes the loader fail for certain, and
	// the run logs that error through slog: withheld when executor-invoked,
	// shown by hand. quoted is text only the loader's error carries, so the
	// by-hand run proves the path was reached and the executor run that the
	// text was withheld. None of these loaders quotes a settings value as
	// written (the config decoder and the settings loader name the key or
	// the line; the JSON encoder names the unencodable value), so the
	// secret-like value never appears either way; the seam probe above pins
	// a value-quoting error.
	for _, c := range []struct {
		name, logMsg, quoted, code string
		setup                      func(t *testing.T, h *preflightHub) preflightOptions
		wantErr                    string // the command's own error, when the run cannot report
	}{
		{
			// A legacy server.yaml whose hub.port holds a non-number: the
			// config decoder fails, naming the key.
			name: "a server config that does not load", logMsg: "preflight: the server config does not load", quoted: "'hub.port' cannot parse value as 'int'",
			wantErr: "the server config does not load",
			setup: func(t *testing.T, h *preflightHub) preflightOptions {
				cfgPath := filepath.Join(t.TempDir(), "server.yaml")
				require.NoError(t, os.WriteFile(cfgPath, []byte("hub:\n  port: "+secret+"\n"), 0o644))
				o := h.executor()
				o.configPath = cfgPath
				return o
			},
		},
		{
			// A settings file that is not YAML: the effective settings do not
			// load (a decode quoting a value is not reachable here: the
			// settings loader ignores a value it cannot convert, by key). The
			// error names the line, not the file's text.
			name: "a settings file that does not load", logMsg: "preflight: the effective settings do not load", quoted: "did not find expected",
			code: "settings.profiles.could_not_run",
			setup: func(t *testing.T, h *preflightHub) preflightOptions {
				require.NoError(t, os.WriteFile(filepath.Join(h.globalDir, "settings.yaml"),
					[]byte("schema_version: \"1\"\nactive_profile: a\nprofiles:\n  a: ["+secret+"\n"), 0o644))
				return h.executor()
			},
		},
		{
			// Bootstrap material holding a value JSON cannot encode: the
			// section does not extract, and the error quotes the value.
			name: "bootstrap material that does not extract", logMsg: "preflight: the bootstrap section does not extract", quoted: "unsupported value: +Inf",
			code: "settings.profiles.could_not_evaluate",
			setup: func(t *testing.T, h *preflightHub) preflightOptions {
				h.writeSettings(t, "    weight: .inf\n")
				return h.executor()
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPreflightHub(t)
			admin := "admin"
			h.profilesRow(t, hubsetting.OriginManaged, &admin, "a")
			o := c.setup(t, h)
			stdout, stderr, err := runPreflightForTest(t, o)
			if c.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, c.wantErr, err.Error())
				assert.Empty(t, stdout)
			} else {
				require.Error(t, err, "the check blocks")
				assert.Contains(t, reportCodes(decodeReport(t, stdout)), c.code)
				assert.NotContains(t, stdout, c.quoted, "the report must not quote the loader's error")
			}
			assert.Contains(t, stderr, c.logMsg, "the run logs the loader's error")
			assert.Contains(t, stderr, withheldNote, "the logged error is withheld")
			assert.NotContains(t, stderr, c.quoted, "the executor's stderr must not quote the loader's error")
			assert.NotContains(t, stderr, secret)
			assert.NotContains(t, stdout, secret)
			assert.NotContains(t, err.Error(), secret)

			// The same run with the handler's by-hand setting (withhold off).
			o.executor, o.dbFlag, o.envDSN = false, o.envDSN, ""
			_, handStderr, handErr := runPreflightForTest(t, o)
			require.Error(t, handErr)
			assert.Contains(t, handStderr, c.logMsg)
			assert.Contains(t, handStderr, c.quoted, "by hand, the loader's error text is shown")
			assert.NotContains(t, handStderr, withheldNote, "by hand, nothing is withheld")
		})
	}
}

// TestNoWriteCommandHook pins the root hook's no-write guard: Execute's
// pre-run path and PersistentPreRunE turn the switch on for server
// preflight only, before any settings load, and skip the global layout
// migration for it; every other command is unchanged.
func TestNoWriteCommandHook(t *testing.T) {
	legacyHome := func(t *testing.T) string {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Chdir(t.TempDir())
		require.NoError(t, os.MkdirAll(filepath.Join(home, ".scion", legacyLayoutDir, "acme"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(home, ".scion", legacyLayoutDir, "acme", "f"), []byte("x"), 0o644))
		warnRemovedLegacyEnvOnce = sync.Once{}
		t.Cleanup(func() { warnRemovedLegacyEnvOnce = sync.Once{} })
		t.Cleanup(func() { config.SetProjectMigrationReporter(config.NewSlogReporter()) })
		return home
	}
	migrated := func(home string) bool {
		info, err := os.Lstat(filepath.Join(home, ".scion", legacyLayoutDir, "acme"))
		return err == nil && info.Mode()&os.ModeSymlink != 0
	}

	for _, args := range [][]string{
		{"server", "preflight"},
		{"--format", "json", "server", "preflight"},
		{"--config", "X", "server", "preflight"},
		{"-c", "X", "server", "preflight"},
	} {
		t.Run("sets: "+strings.Join(args, " "), func(t *testing.T) {
			writesonread.IsolateForTest(t)
			home := legacyHome(t)
			target := prepareInvocation(args)
			require.True(t, isNoWriteCommand(target), "args resolve to server preflight")
			assert.True(t, writesonread.Disabled(), "the switch is not set before resolveMode")
			assert.False(t, migrated(home), "the legacy layout must not be migrated for server preflight")
		})
	}
	for _, args := range [][]string{{"list"}, {"server", "status"}} {
		t.Run("leaves off: "+strings.Join(args, " "), func(t *testing.T) {
			writesonread.IsolateForTest(t)
			home := legacyHome(t)
			prepareInvocation(args)
			assert.False(t, writesonread.Disabled(), "the switch is set for %s", strings.Join(args, " "))
			assert.True(t, migrated(home), "the legacy layout is not migrated for %s", strings.Join(args, " "))
		})
	}
	t.Run("the layout hook skips server preflight on its own", func(t *testing.T) {
		// With the switch off, so only the hook's own skip can keep the
		// legacy layout in place (the switch is defence in depth).
		writesonread.IsolateForTest(t)
		home := legacyHome(t)
		maybeWarnRemovedLegacyEnv(serverPreflightCmd)
		assert.False(t, migrated(home), "maybeWarnRemovedLegacyEnv must skip server preflight")
		maybeWarnRemovedLegacyEnv(serverStatusCmd)
		assert.True(t, migrated(home), "control: the hook migrates for server status")
	})
	t.Run("an unknown bare flag first: no preflight runs", func(t *testing.T) {
		writesonread.IsolateForTest(t)
		target, _, _ := rootCmd.Find([]string{"--bogus", "server", "preflight"})
		assert.False(t, isNoWriteCommand(target), "the bogus flag takes the next word")
		err := rootCmd.ParseFlags([]string{"--bogus"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown flag: --bogus", "cobra rejects the command line before any RunE")
	})
	t.Run("PersistentPreRunE", func(t *testing.T) {
		restoreAllSilenceUsage(t)
		for _, c := range []struct {
			cmd  *cobra.Command
			want bool
		}{{serverPreflightCmd, true}, {serverStatusCmd, false}} {
			writesonread.IsolateForTest(t)
			legacyHome(t)
			_ = rootCmd.PersistentPreRunE(c.cmd, nil)
			assert.Equal(t, c.want, writesonread.Disabled(), "PersistentPreRunE for %s", c.cmd.CommandPath())
			writesonread.IsolateForTest(t)
		}
	})
}
