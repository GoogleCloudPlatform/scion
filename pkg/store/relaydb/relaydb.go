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

// Package relaydb is a separate conduit relay's database access (scion
// server --role relay). It opens the database from the relay's own
// connection input and returns only:
//
//   - the conduit registry store;
//   - an allowlisted settings reader, which today reads hub.conduit and
//     nothing else;
//   - on PostgreSQL, a LISTEN source on the hub's conduit channel only,
//     delivering only the hub event subjects it is given;
//   - a close function.
//
// It reads no token, credential or secret table and never migrates the
// schema (the hubs do). Only cmd/server_relay.go imports it; a test in
// pkg/conduit/relayrole/relayserver checks both.
package relaydb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/hubsetting"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// Options is the relay's database connection input, separate from the
// hub's.
type Options struct {
	// Driver is "postgres" or "sqlite".
	Driver string
	// DSN is the relay's connection string.
	DSN string
	// MaxOpenConns and MaxIdleConns size the pool (0: driver defaults).
	MaxOpenConns int
	MaxIdleConns int
	// EventSubjects are the hub event subjects the LISTEN source delivers
	// (PostgreSQL only; none: no LISTEN source).
	EventSubjects []string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// DB is an open relay database.
type DB struct {
	// Registry is the conduit registry store.
	Registry registry.Store
	// Settings reads the allowlisted settings.
	Settings *SettingsReader

	events *Listener
	close  func()
}

// Events returns the LISTEN source, or nil when there is none (SQLite, or
// no subjects).
func (db *DB) Events() relayrole.Events {
	if db.events == nil {
		return nil
	}
	return db.events
}

// StartEvents starts the LISTEN source, if any. Call it after every
// subscription and listen hook is registered.
func (db *DB) StartEvents() {
	if db.events != nil {
		db.events.Start()
	}
}

// Close releases the database and stops the LISTEN source.
func (db *DB) Close() { db.close() }

// Open opens the relay's database access.
func Open(ctx context.Context, o Options) (*DB, error) {
	if o.DSN == "" {
		return nil, errors.New("conduit relay database: no connection string")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	pool := entc.PoolConfig{MaxOpenConns: o.MaxOpenConns, MaxIdleConns: o.MaxIdleConns}
	var client *ent.Client
	var err error
	switch strings.ToLower(o.Driver) {
	case "postgres":
		client, err = entc.OpenPostgres(o.DSN, pool)
	case "sqlite":
		dsn := o.DSN
		if !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn
		}
		client, err = entc.OpenSQLite(dsn, pool)
	default:
		return nil, fmt.Errorf("conduit relay database: unsupported driver %q", o.Driver)
	}
	if err != nil {
		return nil, fmt.Errorf("conduit relay database: %w", err)
	}
	db := &DB{
		Registry: entadapter.NewConduitRegistryStore(client),
		Settings: &SettingsReader{client: client},
		close:    func() { _ = client.Close() },
	}
	if strings.EqualFold(o.Driver, "postgres") && len(o.EventSubjects) > 0 {
		l, err := NewListener(o.DSN, o.EventSubjects, o.Logger.With("subsystem", "relay.events"))
		if err != nil {
			db.close()
			return nil, err
		}
		db.events = l
		closeClient := db.close
		db.close = func() { l.Close(); closeClient() }
	}
	return db, nil
}

// experimentsSection is the hub settings section holding the experiment
// overrides: the only row the relay reads.
const experimentsSection = "experiments"

// conduitExperiment is the only experiment the relay reads.
const conduitExperiment = "hub.conduit"

// SettingsReader reads the settings on the relay's allowlist: today only
// hub.conduit. It has no general settings access.
type SettingsReader struct {
	client *ent.Client
	reg    *experiments.Registry // nil: experiments.Default()
}

// ConduitEnabled reports whether hub.conduit is on, resolved as the hub
// resolves it: the experiments section's overrides document (parsed with
// opsettings.ParseExperimentsDoc) through experiments.Registry.Resolve. No
// row means no overrides.
func (r *SettingsReader) ConduitEnabled(ctx context.Context) (bool, error) {
	row, err := r.client.HubSetting.Query().Where(hubsetting.SectionEQ(experimentsSection)).Only(ctx)
	var overrides map[string]bool
	malformed := false
	switch {
	case ent.IsNotFound(err):
	case err != nil:
		return false, fmt.Errorf("reading hub.conduit: %w", err)
	default:
		doc, bad := opsettings.ParseExperimentsDoc(row.Value)
		overrides, malformed = doc.Overrides, bad
	}
	reg := r.reg
	if reg == nil {
		reg = experiments.Default()
	}
	return reg.Resolve(conduitExperiment, overrides, malformed), nil
}
