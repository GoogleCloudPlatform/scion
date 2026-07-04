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

// Package runtime provides a reusable runtime library for standalone
// integration services (Discord, Telegram, etc.). It handles:
//   - Config resolution with layering: DB > env vars > local YAML
//   - LISTEN loop on the admin signal channel for live config/update signals
//   - Update signal handling with status write-back to integration_updates
//   - Schema retry at startup (tolerates hub-owned tables not existing yet)
//
// This is the key Phase 5C extraction that Phase 5D (Telegram) imports directly.
package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// ReconfigureFunc is called when a config change signal arrives. The integration
// should re-read its config and apply any changes that can be applied without restart.
type ReconfigureFunc func(cfg map[string]string) error

// Options configures a Runtime instance.
type Options struct {
	// Integration name (e.g. "discord", "telegram").
	Integration string

	// DatabaseURL is the Postgres connection string shared with the hub.
	DatabaseURL string

	// ConfigFile is the optional local YAML config file path.
	ConfigFile string

	// EnvPrefix for environment variable overrides (e.g. "DISCORD" reads DISCORD_BOT_TOKEN).
	// If empty, env vars are not layered.
	EnvPrefix string

	// EnvKeys lists config keys that should be read from environment variables.
	// The env var name is EnvPrefix + "_" + UPPER(key). For example, with
	// EnvPrefix="DISCORD" and key "bot_token", reads DISCORD_BOT_TOKEN.
	EnvKeys []string

	// OnReconfigure is called when a config change signal arrives from the admin plane.
	OnReconfigure ReconfigureFunc

	// UpdateHook is an optional command to execute when an update signal arrives.
	// If empty, the default behavior is graceful exit(0) relying on platform restart.
	UpdateHook string

	Log *slog.Logger
}

// Runtime manages the integration lifecycle: config loading, signal listening,
// and update handling.
type Runtime struct {
	opts Options
	log  *slog.Logger

	db       *sql.DB
	listener *hub.AdminSignalListener

	mu        sync.Mutex
	config    map[string]string
	cancelCtx context.CancelFunc

	ready      chan struct{} // closed when initial config load succeeds
	shutdownCh chan string   // update-triggered graceful shutdown signal
}

// New creates a Runtime but does not start it. Call Start to begin.
func New(opts Options) *Runtime {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	return &Runtime{
		opts:       opts,
		log:        log,
		ready:      make(chan struct{}),
		shutdownCh: make(chan string, 1),
	}
}

// Start connects to Postgres, loads config with layering, and starts the
// admin signal listener. It retries the initial DB connection with backoff
// to tolerate hub-owned tables not existing yet (P5/R4).
//
// The returned context is cancelled on Runtime.Stop().
func (rt *Runtime) Start(ctx context.Context) (context.Context, error) {
	rctx, cancel := context.WithCancel(ctx)
	rt.cancelCtx = cancel

	if err := rt.connectWithRetry(rctx); err != nil {
		cancel()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	if err := rt.loadConfig(rctx, true); err != nil {
		cancel()
		return nil, fmt.Errorf("load initial config: %w", err)
	}
	close(rt.ready)

	rt.listener = hub.NewAdminSignalListener(rctx, rt.opts.DatabaseURL, rt.log)
	rt.listener.OnSignal(func(signal hub.AdminSignal) {
		if signal.Integration != rt.opts.Integration {
			return
		}
		switch signal.Kind {
		case "config":
			rt.handleConfigSignal(rctx)
		case "update":
			rt.handleUpdateSignal(rctx, signal.ID)
		}
	})

	// F7: Re-scan tables on (re)connect — NOTIFYs missed during LISTEN
	// gaps are recovered from the source-of-truth rows.
	rt.listener.SetOnConnect(func() {
		rt.scanPendingUpdates(rctx)
		rt.handleConfigSignal(rctx)
	})

	go rt.pollLoop(rctx)

	rt.log.Info("Integration runtime started",
		"integration", rt.opts.Integration,
	)
	return rctx, nil
}

// ShutdownRequested returns a channel that receives the update ID when an
// update signal requests graceful shutdown. The caller should cancel the
// runtime's context to trigger orderly shutdown.
func (rt *Runtime) ShutdownRequested() <-chan string {
	return rt.shutdownCh
}

// SetReconfigure sets the callback invoked when a config change signal arrives.
// It may be called after Start.
func (rt *Runtime) SetReconfigure(fn ReconfigureFunc) {
	rt.mu.Lock()
	rt.opts.OnReconfigure = fn
	rt.mu.Unlock()
}

// Config returns the current merged config map. Thread-safe.
func (rt *Runtime) Config() map[string]string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cp := make(map[string]string, len(rt.config))
	for k, v := range rt.config {
		cp[k] = v
	}
	return cp
}

// WaitReady blocks until the initial config load has completed.
func (rt *Runtime) WaitReady(ctx context.Context) error {
	select {
	case <-rt.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop shuts down the signal listener and closes the DB connection.
func (rt *Runtime) Stop() {
	if rt.cancelCtx != nil {
		rt.cancelCtx()
	}
	if rt.listener != nil {
		rt.listener.Close()
	}
	if rt.db != nil {
		rt.db.Close()
	}
}

func (rt *Runtime) connectWithRetry(ctx context.Context) error {
	backoff := 500 * time.Millisecond
	attempt := 0

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		attempt++

		db, err := sql.Open("pgx", rt.opts.DatabaseURL)
		if err != nil {
			rt.log.Warn("Failed to open database", "attempt", attempt, "error", err)
			if !rt.sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = db.PingContext(pingCtx)
		cancel()
		if err != nil {
			db.Close()
			rt.log.Warn("Database ping failed", "attempt", attempt, "error", err)
			if !rt.sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		err = rt.checkHubTables(ctx, db)
		if err != nil {
			db.Close()
			rt.log.Warn("Hub tables not ready, retrying", "attempt", attempt, "error", err)
			if !rt.sleep(ctx, backoff) {
				return ctx.Err()
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		rt.db = db
		return nil
	}
}

func (rt *Runtime) checkHubTables(ctx context.Context, db *sql.DB) error {
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(checkCtx, "SELECT 1 FROM integration_configs LIMIT 0"); err != nil {
		return err
	}
	_, err := db.ExecContext(checkCtx, "SELECT 1 FROM integration_updates LIMIT 0")
	return err
}

// loadConfig implements the layering: local YAML → env vars → DB config.
// DB config has highest priority. When initial is true, a DB config load
// failure is fatal (returning an error); on subsequent reloads it is
// warn-and-keep (preserving last-known-good config).
func (rt *Runtime) loadConfig(ctx context.Context, initial bool) error {
	merged := make(map[string]string)

	// Layer 1: local YAML file (lowest priority).
	if rt.opts.ConfigFile != "" {
		provider, err := config.NewYAMLConfigProvider(rt.opts.ConfigFile)
		if err != nil {
			rt.log.Warn("Config file provider error, skipping", "error", err)
		} else {
			fileConfig, err := provider.Load(ctx)
			if err != nil {
				rt.log.Warn("Failed to load config file, skipping", "error", err)
			} else {
				for k, v := range fileConfig {
					merged[k] = v
				}
			}
		}
	}

	// Layer 2: environment variables.
	if rt.opts.EnvPrefix != "" {
		for _, key := range rt.opts.EnvKeys {
			envName := rt.opts.EnvPrefix + "_" + strings.ToUpper(key)
			if val := os.Getenv(envName); val != "" {
				merged[key] = val
			}
		}
	}

	// Layer 3: DB config (highest priority).
	dbConfig, err := rt.loadDBConfig(ctx)
	if err != nil {
		if initial {
			return fmt.Errorf("load DB config: %w", err)
		}
		rt.log.Warn("Failed to load DB config, keeping last-known-good", "error", err)
	} else {
		for k, v := range dbConfig {
			merged[k] = v
		}
	}

	rt.mu.Lock()
	rt.config = merged
	rt.mu.Unlock()
	return nil
}

func (rt *Runtime) loadDBConfig(ctx context.Context) (map[string]string, error) {
	if rt.db == nil {
		return nil, nil
	}

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var configJSON string
	err := rt.db.QueryRowContext(queryCtx,
		"SELECT config FROM integration_configs WHERE integration = $1",
		rt.opts.Integration,
	).Scan(&configJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return make(map[string]string), nil
		}
		return nil, fmt.Errorf("query integration_configs: %w", err)
	}

	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return nil, fmt.Errorf("unmarshal integration config: %w", err)
	}
	result := make(map[string]string, len(raw))
	for k, v := range raw {
		switch val := v.(type) {
		case string:
			result[k] = val
		default:
			result[k] = fmt.Sprintf("%v", val)
		}
	}
	return result, nil
}

func (rt *Runtime) handleConfigSignal(ctx context.Context) {
	rt.log.Info("Config change signal received", "integration", rt.opts.Integration)

	if err := rt.loadConfig(ctx, false); err != nil {
		rt.log.Error("Failed to reload config on signal", "error", err)
		return
	}

	rt.mu.Lock()
	cb := rt.opts.OnReconfigure
	rt.mu.Unlock()

	if cb != nil {
		cfg := rt.Config()
		if err := cb(cfg); err != nil {
			rt.log.Error("Reconfigure callback failed", "error", err)
		}
	}
}

func (rt *Runtime) handleUpdateSignal(ctx context.Context, updateID string) {
	rt.log.Info("Update signal received",
		"integration", rt.opts.Integration,
		"update_id", updateID,
	)

	// F9: Guarded transition requested→acknowledged. If 0 rows affected,
	// this is a stale/duplicate signal — do not proceed.
	applied, err := rt.transitionUpdateState(ctx, updateID, "requested", "acknowledged", "")
	if err != nil {
		rt.log.Error("Failed to acknowledge update", "error", err, "update_id", updateID)
		return
	}
	if !applied {
		rt.log.Info("Update signal stale or already processed", "update_id", updateID)
		return
	}

	applied, err = rt.transitionUpdateState(ctx, updateID, "acknowledged", "updating", "")
	if err != nil {
		rt.log.Error("Failed to mark update as updating", "error", err, "update_id", updateID)
		return
	}
	if !applied {
		return
	}

	if rt.opts.UpdateHook != "" {
		rt.log.Info("Executing update hook", "hook", rt.opts.UpdateHook)
		cmd := exec.CommandContext(ctx, "sh", "-c", rt.opts.UpdateHook)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			detail := fmt.Sprintf("update hook failed: %v", err)
			rt.transitionUpdateState(ctx, updateID, "updating", "failed", detail)
			rt.log.Error("Update hook failed", "error", err)
			return
		}
	}

	// F8: Signal main to shut down gracefully instead of os.Exit(0).
	rt.log.Info("Update processed, requesting graceful shutdown")
	select {
	case rt.shutdownCh <- updateID:
	default:
	}
}

// transitionUpdateState performs a guarded state transition: the UPDATE only
// fires if the row is currently in fromState. Returns (true, nil) if the row
// was transitioned, (false, nil) if it was stale/already-advanced, or
// (false, err) on database error.
func (rt *Runtime) transitionUpdateState(ctx context.Context, updateID, fromState, toState, detail string) (bool, error) {
	if rt.db == nil || updateID == "" {
		return true, nil
	}

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	query := "UPDATE integration_updates SET state = $1, update_time = NOW()"
	args := []any{toState}
	argIdx := 2

	if detail != "" {
		query += fmt.Sprintf(", detail = $%d", argIdx)
		args = append(args, detail)
		argIdx++
	}

	query += fmt.Sprintf(" WHERE id = $%d::uuid AND state = $%d", argIdx, argIdx+1)
	args = append(args, updateID, fromState)

	result, err := rt.db.ExecContext(writeCtx, query, args...)
	if err != nil {
		return false, err
	}

	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// pollLoop periodically re-scans the source-of-truth tables for rows
// that were missed during LISTEN gaps (F7). NOTIFYs are not queued for
// disconnected listeners, so this catches updates requested while the
// integration was down or reconnecting.
func (rt *Runtime) pollLoop(ctx context.Context) {
	rt.scanPendingUpdates(ctx)

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.scanPendingUpdates(ctx)
			rt.handleConfigSignal(ctx)
		}
	}
}

func (rt *Runtime) scanPendingUpdates(ctx context.Context) {
	if rt.db == nil {
		return
	}

	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	rows, err := rt.db.QueryContext(queryCtx,
		"SELECT id FROM integration_updates WHERE integration = $1 AND state = 'requested'",
		rt.opts.Integration)
	if err != nil {
		if ctx.Err() == nil {
			rt.log.Warn("Failed to scan pending updates", "error", err)
		}
		return
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rt.log.Warn("Failed to scan update row", "error", err)
			continue
		}
		rt.handleUpdateSignal(ctx, id)
	}
}

func (rt *Runtime) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
